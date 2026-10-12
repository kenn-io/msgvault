package synctechsms

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/attachmentpolicy"
	"go.kenn.io/msgvault/internal/export"
	"go.kenn.io/msgvault/internal/mime"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/textimport"
)

type Importer struct {
	store *store.Store
	opts  ImportOptions
}

func NewImporter(st *store.Store, opts ImportOptions) *Importer {
	return &Importer{store: st, opts: opts}
}

func (i *Importer) ImportPath(path string) (ImportSummary, error) {
	// An explicit backup import never inherits live or another run's authority.
	copyImporter := *i
	copyImporter.store = i.store.WithIngestContext(store.IngestContext{Mode: store.IngestBackfill})
	i = &copyImporter
	ctx := context.Background()
	if strings.TrimSpace(i.opts.OwnerPhone) == "" {
		return ImportSummary{}, errors.New("owner phone is required for synctech-sms imports")
	}
	if err := i.store.ValidateNativeImportChoice(ctx, SourceType, i.opts.NotifyImports); err != nil {
		return ImportSummary{}, err
	}
	src, err := i.store.GetOrCreateSource(SourceType, i.opts.OwnerPhone)
	if err != nil {
		return ImportSummary{}, fmt.Errorf("get source: %w", err)
	}
	syncID, err := i.store.StartSync(src.ID, AdapterName)
	if err != nil {
		return ImportSummary{}, fmt.Errorf("start sync: %w", err)
	}
	scoped := *i
	scoped.store = i.store.ScopedToSync(src.ID, syncID)
	logicalRun, err := scoped.store.BeginNativeImportAttempt(ctx, src.ID, SourceType, i.opts.NotifyImports, syncID, 0)
	if err != nil {
		_ = i.store.FailSync(syncID, err.Error())
		return ImportSummary{}, fmt.Errorf("begin Synctech import: %w", err)
	}
	scoped.store, err = scoped.store.WithImportRun(ctx, logicalRun.ID)
	if err != nil {
		_ = i.store.FailSync(syncID, err.Error())
		return ImportSummary{ImportRunID: logicalRun.ID}, fmt.Errorf("validate Synctech import: %w", err)
	}
	i = &scoped
	summary, importErr := i.importPathIntoSource(src.ID, path)
	summary.ImportRunID = logicalRun.ID
	if importErr != nil {
		_ = i.store.FailSync(syncID, importErr.Error())
		return summary, importErr
	}
	total := int64(summary.SMSImported + summary.MMSImported + summary.CallsImported)
	if err := i.store.UpdateSyncCheckpoint(syncID, &store.Checkpoint{
		MessagesProcessed: total,
		MessagesAdded:     total,
	}); err != nil {
		_ = i.store.FailSync(syncID, err.Error())
		return summary, fmt.Errorf("update sync checkpoint: %w", err)
	}
	if err := i.store.RecomputeConversationStats(src.ID); err != nil {
		_ = i.store.FailSync(syncID, err.Error())
		return summary, fmt.Errorf("recompute conversation stats: %w", err)
	}
	if err := i.store.CompleteSync(syncID, ""); err != nil {
		return summary, fmt.Errorf("complete sync: %w", err)
	}
	return summary, nil
}

// ImportPathIntoSource imports Synctech SMS backup records into an existing
// source without starting, completing, or failing a sync run. Callers that own
// a broader lifecycle, such as Google Drive polling, use this to avoid nested
// sync_runs.
func (i *Importer) ImportPathIntoSource(sourceID int64, path string) (ImportSummary, error) {
	if i.opts.NotifyImports {
		return ImportSummary{}, errors.New("notifications require an explicit backup import")
	}
	polling := *i
	polling.store = i.store.WithIngestContext(store.IngestContext{Mode: store.IngestBackfill})
	return polling.importPathIntoSource(sourceID, path)
}

func (i *Importer) importPathIntoSource(sourceID int64, path string) (ImportSummary, error) {
	if strings.TrimSpace(i.opts.OwnerPhone) == "" {
		return ImportSummary{}, errors.New("owner phone is required for synctech-sms imports")
	}
	files, err := DiscoverBackupFiles(path)
	if err != nil {
		return ImportSummary{}, err
	}
	return i.importFilesIntoSource(sourceID, files)
}

// ImportFilesIntoSource imports already-discovered backup files into an
// existing source without owning sync lifecycle.
func (i *Importer) ImportFilesIntoSource(sourceID int64, files []BackupFile) (ImportSummary, error) {
	if i.opts.NotifyImports {
		return ImportSummary{}, errors.New("notifications require an explicit backup import")
	}
	polling := *i
	polling.store = i.store.WithIngestContext(store.IngestContext{Mode: store.IngestBackfill})
	return polling.importFilesIntoSource(sourceID, files)
}

func (i *Importer) importFilesIntoSource(sourceID int64, files []BackupFile) (ImportSummary, error) {
	var summary ImportSummary
	summary.FilesSeen = len(files)
	for _, file := range files {
		rc, err := file.Opener()
		if err != nil {
			return summary, fmt.Errorf("open backup file %s: %w", file.Name, err)
		}
		err = ParseEach(rc, func(record Record) error {
			return i.importRecord(sourceID, record, &summary)
		})
		_ = rc.Close()
		if err != nil {
			return summary, fmt.Errorf("import backup file %s: %w", file.Name, err)
		}
		summary.FilesImported++
	}
	return summary, nil
}

func (i *Importer) importRecord(sourceID int64, record Record, summary *ImportSummary) error {
	switch record.Kind {
	case RecordSMS:
		if i.opts.IncludeSMS && record.SMS != nil {
			msgID, err := i.importSMS(sourceID, *record.SMS)
			if err != nil {
				return err
			}
			summary.MessageIDs = append(summary.MessageIDs, msgID)
			summary.SMSImported++
		}
	case RecordMMS:
		if i.opts.IncludeMMS && record.MMS != nil {
			msgID, attachments, err := i.importMMS(sourceID, *record.MMS)
			if err != nil {
				if msgID != 0 {
					summary.MessageIDs = append(summary.MessageIDs, msgID)
				}
				return err
			}
			summary.MessageIDs = append(summary.MessageIDs, msgID)
			summary.MMSImported++
			summary.AttachmentsImported += attachments
		}
	case RecordCall:
		if i.opts.IncludeCalls && record.Call != nil {
			msgID, err := i.importCall(sourceID, *record.Call)
			if err != nil {
				return err
			}
			summary.MessageIDs = append(summary.MessageIDs, msgID)
			summary.CallsImported++
		}
	}
	return nil
}

func (i *Importer) importSMS(sourceID int64, sms SMS) (int64, error) {
	participants := []store.ParticipantPersistData{
		synctechParticipant(sms.Address, sms.ContactName.String),
		synctechParticipant(i.opts.OwnerPhone, "Me"),
	}
	// Drafts remain owner-authored, like other unsent outgoing messages.
	fromMe := sms.Type == SMSTypeSent || sms.Type == SMSTypeOutbox || sms.Type == SMSTypeFailed || sms.Type == SMSTypeQueued || sms.Type == SMSTypeDraft
	return i.persistTextMessage(participants, sms, func(ids []int64) *store.MessagePersistData {
		remoteID, ownerID := ids[0], ids[1]
		senderID, recipientIDs := remoteID, []int64{ownerID}
		if fromMe {
			senderID, recipientIDs = ownerID, []int64{remoteID}
		}
		sourceMessageID := stableID("sms", sms.Address, sms.Timestamp.String(), fmt.Sprint(sms.Type), sms.Body)
		return textMessageSnapshot(sourceID, textConversationKey([]int64{ownerID, remoteID}), sms.ContactName.String, []int64{remoteID, ownerID}, sourceMessageID, "sms", senderID, recipientIDs, fromMe, sms.Timestamp, sms.Body, sms.Body, 0)
	})
}

func (i *Importer) importMMS(sourceID int64, mms MMS) (int64, int, error) {
	var attachmentSnapshot *store.KeyedAttachmentSnapshot
	if i.opts.NotifyImports && i.opts.IncludeAttachments {
		writes, err := i.prepareMMSAttachmentWrites(mms)
		if err != nil {
			return 0, 0, err
		}
		attachmentSnapshot = &store.KeyedAttachmentSnapshot{Prefix: "synctech:mms:", Writes: writes, PreserveUnmentioned: true}
	}
	participants := []store.ParticipantPersistData{synctechParticipant(i.opts.OwnerPhone, "Me")}
	for _, addr := range mms.Addresses {
		if strings.TrimSpace(addr.Address) == "" || addr.Address == "insert-address-token" {
			continue
		}
		participants = append(participants, synctechParticipant(addr.Address, ""))
	}
	fromMe := mms.MessageBox == MMSBoxSent || mms.MessageBox == MMSBoxOutbox || mms.MessageBox == MMSBoxDraft
	body := mmsText(mms)
	sourcePart := mms.MessageID.String
	if sourcePart == "" {
		sourcePart = body
	}
	var sourceMessageID string
	messageID, err := i.persistTextMessage(participants, mms, func(ids []int64) *store.MessagePersistData {
		participantIDs, senderID, recipientIDs := mmsParticipantIDs(mms, ids)
		// Native MMS identity and text-thread keys depend on resolved participant IDs.
		sourceMessageID = stableID("mms", sourcePart, mms.Timestamp.String(), sortedKey(participantIDs))
		data := textMessageSnapshot(sourceID, textConversationKey(participantIDs), mms.ContactName.String, participantIDs, sourceMessageID, "mms", senderID, recipientIDs, fromMe, mms.Timestamp, body, mms.Subject.String, countImportableAttachments(mms, i.opts.IncludeAttachments))
		data.KeyedAttachmentSnapshot = attachmentSnapshot
		return data
	})
	if err != nil {
		return 0, 0, err
	}
	if i.opts.NotifyImports {
		if attachmentSnapshot == nil {
			return messageID, 0, nil
		}
		return messageID, len(attachmentSnapshot.Writes), nil
	}
	// Silent native imports retain their post-message attachment/error semantics.
	imported, err := i.importMMSAttachments(sourceID, sourceMessageID, mms)
	return messageID, imported, err
}

func (i *Importer) importCall(sourceID int64, call Call) (int64, error) {
	remoteAddress := callParticipantAddress(call)
	participants := []store.ParticipantPersistData{synctechParticipant(remoteAddress, call.ContactName.String), synctechParticipant(i.opts.OwnerPhone, "Me")}
	fromMe := call.Type == CallOutgoing
	body := fmt.Sprintf("Call %s, %d seconds", callTypeLabel(call.Type), call.DurationSeconds)
	return i.persistTextMessage(participants, call, func(ids []int64) *store.MessagePersistData {
		remoteID, ownerID := ids[0], ids[1]
		senderID, recipientIDs := remoteID, []int64{ownerID}
		if fromMe {
			senderID, recipientIDs = ownerID, []int64{remoteID}
		}
		sourceMessageID := stableID("call", remoteAddress, call.Timestamp.String(), fmt.Sprint(call.Type), strconv.Itoa(call.DurationSeconds))
		return textMessageSnapshot(sourceID, "calls:"+canonicalAddress(remoteAddress), call.ContactName.String, []int64{remoteID, ownerID}, sourceMessageID, "synctech_sms_call", senderID, recipientIDs, fromMe, call.Timestamp, body, body, 0)
	})
}

func (i *Importer) persistTextMessage(participants []store.ParticipantPersistData, raw any, build func([]int64) *store.MessagePersistData) (int64, error) {
	rawJSON, err := json.Marshal(raw, json.Deterministic(true))
	if err != nil {
		return 0, fmt.Errorf("marshal raw record: %w", err)
	}
	id, err := i.store.PersistMessageWithParticipantsContext(context.Background(), participants, func(ids []int64) *store.MessagePersistData {
		data := build(ids)
		data.RawMIME, data.RawFormat = rawJSON, RawFormat
		return data
	})
	if err != nil {
		return 0, fmt.Errorf("persist Synctech message: %w", err)
	}
	return id, nil
}

func textMessageSnapshot(sourceID int64, conversationKey, title string, participantIDs []int64, sourceMessageID, messageType string, senderID int64, recipientIDs []int64, fromMe bool, sentAt time.Time, body, subject string, attachmentCount int) *store.MessagePersistData {
	members := make([]store.ConversationParticipantRef, 0, len(participantIDs))
	for _, id := range participantIDs {
		members = append(members, store.ConversationParticipantRef{ParticipantID: id, Role: "member"})
	}
	return &store.MessagePersistData{
		Message:      &store.Message{SourceID: sourceID, SourceMessageID: sourceMessageID, MessageType: messageType, SentAt: sql.NullTime{Time: sentAt, Valid: !sentAt.IsZero()}, SenderID: sql.NullInt64{Int64: senderID, Valid: senderID != 0}, IsFromMe: fromMe, Subject: sql.NullString{String: subject, Valid: subject != ""}, Snippet: sql.NullString{String: body, Valid: body != ""}, SizeEstimate: int64(len(body)), HasAttachments: attachmentCount > 0, AttachmentCount: attachmentCount},
		Conversation: &store.ConversationPersistData{SourceConversationID: conversationKey, ConversationType: "direct_chat", Title: title, Participants: members, PreserveExistingParticipants: true},
		BodyText:     sql.NullString{String: body, Valid: body != ""}, PreserveBodyIfOmitted: true,
		Recipients:     []store.RecipientSet{{Type: "from", ParticipantIDs: []int64{senderID}, DisplayNames: []string{""}}, {Type: "to", ParticipantIDs: recipientIDs, DisplayNames: blankNames(len(recipientIDs))}},
		PreserveLabels: true,
		FTS:            &store.FTSDoc{Subject: subject, Body: body}, FTSBestEffort: true,
	}
}

func synctechParticipant(address, displayName string) store.ParticipantPersistData {
	normalized := textimport.NormalizeAddress(address)
	if normalized.Kind == textimport.AddressPhone {
		return store.ParticipantPersistData{PhoneNumber: normalized.Value, IdentifierType: ParticipantIdentifierType, DisplayName: displayName}
	}
	return store.ParticipantPersistData{IdentifierType: ParticipantIdentifierType, IdentifierValue: normalized.Value, DisplayName: displayName}
}

// ids follows owner then each non-placeholder address in native XML order.
func mmsParticipantIDs(mms MMS, ids []int64) ([]int64, int64, []int64) {
	ownerID := ids[0]
	senderID := ownerID
	fromMe := mms.MessageBox == MMSBoxSent || mms.MessageBox == MMSBoxOutbox || mms.MessageBox == MMSBoxDraft
	var recipients []int64
	next := 1
	for _, addr := range mms.Addresses {
		if strings.TrimSpace(addr.Address) == "" || addr.Address == "insert-address-token" {
			continue
		}
		id := ids[next]
		next++
		if addr.Type == MMSAddressFrom {
			if !fromMe {
				senderID = id
			}
			continue
		}
		recipients = append(recipients, id)
	}
	if len(recipients) == 0 {
		recipients = []int64{ownerID}
	}
	return uniqueInt64s(ids), senderID, uniqueInt64s(recipients)
}

func stableID(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		_, _ = h.Write([]byte(p))
		_, _ = h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

func canonicalAddress(address string) string {
	n := textimport.NormalizeAddress(address)
	return n.Value
}

func callParticipantAddress(call Call) string {
	number := strings.TrimSpace(call.Number)
	if number != "" && !strings.EqualFold(number, "null") {
		return number
	}
	return fmt.Sprintf("unknown-call:%d", call.Presentation)
}

// textConversationKey returns the conversation key shared by SMS and MMS so
// one-on-one threads between the same participants stay unified across both
// message types. Call logs use a different prefix so they remain separate.
func textConversationKey(participantIDs []int64) string {
	return "text:" + sortedKey(participantIDs)
}

// countImportableAttachments returns the number of MMS parts that will be
// written to disk so the parent message records the right has_attachments
// and attachment_count up front.
func countImportableAttachments(mms MMS, include bool) int {
	if !include {
		return 0
	}
	count := 0
	for _, part := range mms.Parts {
		if len(part.Data) > 0 {
			count++
		}
	}
	return count
}

func sortedKey(ids []int64) string {
	cp := append([]int64(nil), ids...)
	slices.Sort(cp)
	parts := make([]string, len(cp))
	for idx, id := range cp {
		parts[idx] = strconv.FormatInt(id, 10)
	}
	return strings.Join(parts, ",")
}

func uniqueInt64s(ids []int64) []int64 {
	seen := make(map[int64]bool, len(ids))
	var out []int64
	for _, id := range ids {
		if id == 0 || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

func blankNames(n int) []string {
	names := make([]string, n)
	return names
}

func mmsText(m MMS) string {
	var parts []string
	for _, p := range m.Parts {
		if p.Text.Valid {
			parts = append(parts, p.Text.String)
		}
	}
	return strings.Join(parts, "\n")
}

func callTypeLabel(t CallType) string {
	switch t {
	case CallIncoming:
		return "incoming"
	case CallOutgoing:
		return "outgoing"
	case CallMissed:
		return "missed"
	case CallVoicemail:
		return "voicemail"
	case CallRejected:
		return "rejected"
	case CallRefused:
		return "refused"
	default:
		return "unknown"
	}
}

func (i *Importer) importMMSAttachments(sourceID int64, sourceMessageID string, mms MMS) (int, error) {
	if !i.opts.IncludeAttachments {
		return 0, nil
	}
	if strings.TrimSpace(i.opts.AttachmentsDir) == "" {
		return 0, errors.New("attachments directory is required when importing MMS attachments")
	}
	// Filter by source_id: source_message_id is unique only per source
	// (the messages table key is (source_id, source_message_id)), so two
	// SyncTech sources backing up the same conversation can produce
	// colliding hashes and would otherwise attach to the wrong row.
	var messageID int64
	if err := i.store.DB().QueryRow(i.store.Rebind(`SELECT id FROM messages WHERE source_id = ? AND source_message_id = ?`), sourceID, sourceMessageID).Scan(&messageID); err != nil {
		return 0, fmt.Errorf("lookup MMS message for attachments: %w", err)
	}
	maxBytes := i.opts.MaxAttachmentBytes
	if maxBytes <= 0 {
		maxBytes = 25 << 20
	}
	count := 0
	for idx, part := range mms.Parts {
		if len(part.Data) == 0 {
			continue
		}
		if int64(len(part.Data)) > maxBytes {
			return count, fmt.Errorf("MMS attachment %d exceeds maximum size", idx)
		}
		filename := part.Filename.String
		if filename == "" {
			filename = fmt.Sprintf("mms-part-%d", idx)
		}
		att := &mime.Attachment{
			Filename:    filename,
			ContentType: part.ContentType,
			Content:     part.Data,
		}
		storagePath, err := export.StoreAttachmentFile(i.opts.AttachmentsDir, att)
		if err != nil {
			return count, fmt.Errorf("store MMS attachment: %w", err)
		}
		if err := i.store.UpsertAttachmentRecord(context.Background(), messageID, store.AttachmentWrite{
			Filename:      filename,
			MIMEType:      part.ContentType,
			StoragePath:   storagePath,
			ContentHash:   att.ContentHash,
			Size:          int64(len(part.Data)),
			Role:          store.AttachmentRoleStandalone,
			RoleSource:    store.AttachmentRoleSourceImporterSemantics,
			SourcePartKey: fmt.Sprintf("synctech:mms:%d", idx),
		}); err != nil {
			return count, fmt.Errorf("upsert attachment: %w", err)
		}
		count++
	}
	return count, nil
}

// File publication stays outside the identity, sync-generation and Events
// writer fences. The prepared references commit with the ready message.
func (i *Importer) prepareMMSAttachmentWrites(mms MMS) ([]store.AttachmentWrite, error) {
	if strings.TrimSpace(i.opts.AttachmentsDir) == "" {
		return nil, errors.New("attachments directory is required when importing MMS attachments")
	}
	maxBytes := i.opts.MaxAttachmentBytes
	if maxBytes <= 0 {
		maxBytes = 25 << 20
	}
	writes := make([]store.AttachmentWrite, 0, countImportableAttachments(mms, true))
	for idx, part := range mms.Parts {
		if len(part.Data) == 0 {
			continue
		}
		if int64(len(part.Data)) > maxBytes {
			return nil, fmt.Errorf("MMS attachment %d exceeds maximum size", idx)
		}
		filename := part.Filename.String
		if filename == "" {
			filename = fmt.Sprintf("mms-part-%d", idx)
		}
		attachment := &mime.Attachment{Filename: filename, ContentType: part.ContentType, Content: part.Data}
		relativePath, err := export.StoreAttachmentFile(i.opts.AttachmentsDir, attachment)
		if err != nil {
			return nil, fmt.Errorf("store MMS attachment: %w", err)
		}
		writes = append(writes, store.AttachmentWrite{Filename: filename, MIMEType: part.ContentType, StoragePath: relativePath, ContentHash: attachment.ContentHash, Size: int64(len(part.Data)), State: attachmentpolicy.StateStored, Role: store.AttachmentRoleStandalone, RoleSource: store.AttachmentRoleSourceImporterSemantics, SourcePartKey: fmt.Sprintf("synctech:mms:%d", idx)})
	}
	return writes, nil
}
