package synctechsms

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

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
	if strings.TrimSpace(i.opts.OwnerPhone) == "" {
		return ImportSummary{}, errors.New("owner phone is required for synctech-sms imports")
	}
	files, err := DiscoverBackupFiles(path)
	if err != nil {
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
	summary, importErr := i.importFiles(src.ID, files)
	if importErr != nil {
		_ = i.store.FailSync(syncID, importErr.Error())
		return summary, importErr
	}
	total := int64(summary.SMSImported + summary.MMSImported + summary.CallsImported)
	_ = i.store.UpdateSyncCheckpoint(syncID, &store.Checkpoint{
		MessagesProcessed: total,
		MessagesAdded:     total,
	})
	_ = i.store.CompleteSync(syncID, "")
	if err := i.store.RecomputeConversationStats(src.ID); err != nil {
		return summary, fmt.Errorf("recompute conversation stats: %w", err)
	}
	return summary, nil
}

func (i *Importer) importFiles(sourceID int64, files []BackupFile) (ImportSummary, error) {
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
			if err := i.importSMS(sourceID, *record.SMS); err != nil {
				return err
			}
			summary.SMSImported++
		}
	case RecordMMS:
		if i.opts.IncludeMMS && record.MMS != nil {
			attachments, err := i.importMMS(sourceID, *record.MMS)
			if err != nil {
				return err
			}
			summary.MMSImported++
			summary.AttachmentsImported += attachments
		}
	case RecordCall:
		if i.opts.IncludeCalls && record.Call != nil {
			if err := i.importCall(sourceID, *record.Call); err != nil {
				return err
			}
			summary.CallsImported++
		}
	}
	return nil
}

func (i *Importer) importSMS(sourceID int64, sms SMS) error {
	remoteID, err := i.participantID(sms.Address, sms.ContactName.String)
	if err != nil {
		return err
	}
	ownerID, err := i.participantID(i.opts.OwnerPhone, "Me")
	if err != nil {
		return err
	}
	// Drafts are owner-authored messages that never made it out, but
	// they still belong on the owner's side of the conversation. Without
	// SMSTypeDraft here a draft imports as if it came from the contact.
	fromMe := sms.Type == SMSTypeSent || sms.Type == SMSTypeOutbox || sms.Type == SMSTypeFailed || sms.Type == SMSTypeQueued || sms.Type == SMSTypeDraft
	senderID := remoteID
	recipientIDs := []int64{ownerID}
	if fromMe {
		senderID = ownerID
		recipientIDs = []int64{remoteID}
	}
	convID, err := i.ensureConversation(sourceID, textConversationKey([]int64{ownerID, remoteID}), sms.ContactName.String)
	if err != nil {
		return err
	}
	if err := i.store.EnsureConversationParticipant(convID, remoteID, "member"); err != nil {
		return err
	}
	if err := i.store.EnsureConversationParticipant(convID, ownerID, "member"); err != nil {
		return err
	}
	msgID := stableID("sms", sms.Address, sms.Timestamp.String(), fmt.Sprint(sms.Type), sms.Body)
	return i.upsertTextMessage(sourceID, convID, msgID, "sms", senderID, recipientIDs, fromMe, sms.Timestamp, sms.Body, sms.Body, 0, sms)
}

func (i *Importer) importMMS(sourceID int64, mms MMS) (int, error) {
	ownerID, err := i.participantID(i.opts.OwnerPhone, "Me")
	if err != nil {
		return 0, err
	}
	participantIDs, senderID, recipientIDs, err := i.mmsParticipants(mms, ownerID)
	if err != nil {
		return 0, err
	}
	// Drafts belong to the owner — see the matching note in importSMS.
	fromMe := mms.MessageBox == MMSBoxSent || mms.MessageBox == MMSBoxOutbox || mms.MessageBox == MMSBoxDraft
	convID, err := i.ensureConversation(sourceID, textConversationKey(participantIDs), mms.ContactName.String)
	if err != nil {
		return 0, err
	}
	for _, participantID := range participantIDs {
		if err := i.store.EnsureConversationParticipant(convID, participantID, "member"); err != nil {
			return 0, err
		}
	}
	body := mmsText(mms)
	srcIDPart := mms.MessageID.String
	if srcIDPart == "" {
		srcIDPart = body
	}
	msgID := stableID("mms", srcIDPart, mms.Timestamp.String(), sortedKey(participantIDs))
	attachmentCount := countImportableAttachments(mms, i.opts.IncludeAttachments)
	if err := i.upsertTextMessage(sourceID, convID, msgID, "mms", senderID, recipientIDs, fromMe, mms.Timestamp, body, mms.Subject.String, attachmentCount, mms); err != nil {
		return 0, err
	}
	return i.importMMSAttachments(sourceID, msgID, mms)
}

func (i *Importer) importCall(sourceID int64, call Call) error {
	remoteAddress := callParticipantAddress(call)
	remoteID, err := i.participantID(remoteAddress, call.ContactName.String)
	if err != nil {
		return err
	}
	ownerID, err := i.participantID(i.opts.OwnerPhone, "Me")
	if err != nil {
		return err
	}
	fromMe := call.Type == CallOutgoing
	senderID := remoteID
	recipientIDs := []int64{ownerID}
	if fromMe {
		senderID = ownerID
		recipientIDs = []int64{remoteID}
	}
	convID, err := i.ensureConversation(sourceID, "calls:"+canonicalAddress(remoteAddress), call.ContactName.String)
	if err != nil {
		return err
	}
	if err := i.store.EnsureConversationParticipant(convID, remoteID, "member"); err != nil {
		return err
	}
	if err := i.store.EnsureConversationParticipant(convID, ownerID, "member"); err != nil {
		return err
	}
	body := fmt.Sprintf("Call %s, %d seconds", callTypeLabel(call.Type), call.DurationSeconds)
	msgID := stableID("call", remoteAddress, call.Timestamp.String(), fmt.Sprint(call.Type), strconv.Itoa(call.DurationSeconds))
	return i.upsertTextMessage(sourceID, convID, msgID, "synctech_sms_call", senderID, recipientIDs, fromMe, call.Timestamp, body, body, 0, call)
}

func (i *Importer) upsertTextMessage(sourceID, convID int64, sourceMessageID, messageType string, senderID int64, recipientIDs []int64, fromMe bool, sentAt time.Time, body, subject string, attachmentCount int, raw any) error {
	msgID, err := i.store.UpsertMessage(&store.Message{
		ConversationID:  convID,
		SourceID:        sourceID,
		SourceMessageID: sourceMessageID,
		MessageType:     messageType,
		SentAt:          sql.NullTime{Time: sentAt, Valid: !sentAt.IsZero()},
		SenderID:        sql.NullInt64{Int64: senderID, Valid: senderID != 0},
		IsFromMe:        fromMe,
		Subject:         sql.NullString{String: subject, Valid: subject != ""},
		Snippet:         sql.NullString{String: body, Valid: body != ""},
		SizeEstimate:    int64(len(body)),
		HasAttachments:  attachmentCount > 0,
		AttachmentCount: attachmentCount,
	})
	if err != nil {
		return fmt.Errorf("upsert message: %w", err)
	}
	if body != "" {
		if err := i.store.UpsertMessageBody(msgID, sql.NullString{String: body, Valid: true}, sql.NullString{}); err != nil {
			return fmt.Errorf("upsert body: %w", err)
		}
	}
	rawJSON, err := json.Marshal(raw)
	if err != nil {
		return fmt.Errorf("marshal raw record: %w", err)
	}
	if err := i.store.UpsertMessageRawWithFormat(msgID, rawJSON, RawFormat); err != nil {
		return fmt.Errorf("upsert raw record: %w", err)
	}
	if err := i.store.ReplaceMessageRecipients(msgID, "from", []int64{senderID}, []string{""}); err != nil {
		return fmt.Errorf("replace from recipient: %w", err)
	}
	if err := i.store.ReplaceMessageRecipients(msgID, "to", recipientIDs, blankNames(len(recipientIDs))); err != nil {
		return fmt.Errorf("replace to recipient: %w", err)
	}
	if err := i.store.UpsertFTS(msgID, subject, body, "", "", ""); err != nil {
		return fmt.Errorf("upsert fts: %w", err)
	}
	return nil
}

func (i *Importer) participantID(address, displayName string) (int64, error) {
	n := textimport.NormalizeAddress(address)
	if n.Kind == textimport.AddressPhone {
		return i.store.EnsureParticipantByPhone(n.Value, displayName, ParticipantIdentifierType)
	}
	return i.store.EnsureParticipantByIdentifier(ParticipantIdentifierType, n.Value, displayName)
}

func (i *Importer) ensureConversation(sourceID int64, sourceConversationID, title string) (int64, error) {
	return i.store.EnsureConversationWithType(sourceID, sourceConversationID, "direct_chat", title)
}

func (i *Importer) mmsParticipants(mms MMS, ownerID int64) ([]int64, int64, []int64, error) {
	ids := []int64{ownerID}
	senderID := ownerID
	// Drafts belong to the owner — see the matching note in importSMS.
	fromMe := mms.MessageBox == MMSBoxSent || mms.MessageBox == MMSBoxOutbox || mms.MessageBox == MMSBoxDraft
	var recipients []int64
	for _, addr := range mms.Addresses {
		if strings.TrimSpace(addr.Address) == "" || addr.Address == "insert-address-token" {
			continue
		}
		id, err := i.participantID(addr.Address, "")
		if err != nil {
			return nil, 0, nil, err
		}
		ids = append(ids, id)
		if !fromMe && addr.Type == MMSAddressFrom {
			senderID = id
			continue
		}
		if fromMe && addr.Type == MMSAddressFrom {
			continue
		}
		recipients = append(recipients, id)
	}
	if len(recipients) == 0 {
		recipients = []int64{ownerID}
	}
	return uniqueInt64s(ids), senderID, uniqueInt64s(recipients), nil
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
		sum := sha256.Sum256(part.Data)
		hash := hex.EncodeToString(sum[:])
		filename := part.Filename.String
		if filename == "" {
			filename = fmt.Sprintf("mms-part-%d", idx)
		}
		storagePath := filepath.Join("synctech-sms", hash[:2], hash)
		fullPath := filepath.Join(i.opts.AttachmentsDir, storagePath)
		if err := os.MkdirAll(filepath.Dir(fullPath), 0o700); err != nil {
			return count, fmt.Errorf("create attachment directory: %w", err)
		}
		if err := os.WriteFile(fullPath, part.Data, 0o600); err != nil {
			return count, fmt.Errorf("write attachment: %w", err)
		}
		if err := i.store.UpsertAttachment(messageID, filename, part.ContentType, storagePath, hash, len(part.Data)); err != nil {
			return count, fmt.Errorf("upsert attachment: %w", err)
		}
		count++
	}
	return count, nil
}
