package gvoice

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/attachmentpolicy"
	"go.kenn.io/msgvault/internal/export"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/textimport"
)

const defaultPageSize = 500

// Client reads from a Google Voice Takeout export and imports messages
// into the msgvault store.
type Client struct {
	notifyImports  bool
	takeoutDir     string
	owner          ownerPhones
	identifier     string // GV phone number used as source identifier
	afterDate      time.Time
	beforeDate     time.Time
	limit          int
	index          []indexEntry
	indexBuilt     bool
	logger         *slog.Logger
	pageSize       int
	attachmentsDir string

	// LRU cache for parsed HTML files (avoid re-parsing when
	// consecutive messages come from the same file)
	lastFilePath string
	lastMessages []textMessage
	lastGroupPar []string
}

// ClientOption configures a Client.
type ClientOption func(*Client)

// WithNotifyImports explicitly requests import notifications for this call.
// The daemon must separately enable import notifications.
func WithNotifyImports(notify bool) ClientOption {
	return func(c *Client) { c.notifyImports = notify }
}

// WithAfterDate filters to messages on or after this date.
func WithAfterDate(t time.Time) ClientOption {
	return func(c *Client) { c.afterDate = t }
}

// WithBeforeDate filters to messages before this date.
func WithBeforeDate(t time.Time) ClientOption {
	return func(c *Client) { c.beforeDate = t }
}

// WithLimit sets the maximum number of messages to import.
func WithLimit(n int) ClientOption {
	return func(c *Client) { c.limit = n }
}

// WithLogger sets the logger for the client.
func WithLogger(l *slog.Logger) ClientOption {
	return func(c *Client) { c.logger = l }
}

// WithAttachmentsDir sets the archive attachment root. When it is empty the
// client stores no voicemail audio and behaves as before.
func WithAttachmentsDir(dir string) ClientOption {
	return func(c *Client) { c.attachmentsDir = dir }
}

// NewClient creates a Client from a Google Voice Takeout directory.
// The directory should be the "Voice" folder containing "Calls/"
// and "Phones.vcf".
func NewClient(
	takeoutDir string, opts ...ClientOption,
) (*Client, error) {
	info, err := os.Stat(takeoutDir)
	if err != nil {
		return nil, fmt.Errorf("takeout directory: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("not a directory: %s", takeoutDir)
	}

	callsDir := filepath.Join(takeoutDir, "Calls")
	if _, err := os.Stat(callsDir); err != nil {
		return nil, fmt.Errorf(
			"calls directory not found in %s: %w",
			takeoutDir, err,
		)
	}

	vcfPath := filepath.Join(takeoutDir, "Phones.vcf")
	vcfData, err := os.ReadFile(vcfPath)
	if err != nil {
		return nil, fmt.Errorf("read Phones.vcf: %w", err)
	}

	owner, err := parseVCF(vcfData)
	if err != nil {
		return nil, fmt.Errorf("parse Phones.vcf: %w", err)
	}

	c := &Client{
		takeoutDir: takeoutDir,
		owner:      owner,
		identifier: owner.GoogleVoice,
		logger:     slog.Default(),
		pageSize:   defaultPageSize,
	}

	for _, opt := range opts {
		opt(c)
	}

	return c, nil
}

// Identifier returns the Google Voice phone number.
func (c *Client) Identifier() string {
	return c.identifier
}

// Close is a no-op for the Takeout client.
func (c *Client) Close() error {
	return nil
}

// Import reads all matching messages from the Takeout export and
// writes them into the msgvault store.
func (c *Client) Import(ctx context.Context, s *store.Store, sourceID int64) (*ImportSummary, error) {
	// Offline calls own a fresh run and never inherit another ingestion mode.
	s = s.WithIngestContext(store.IngestContext{Mode: store.IngestBackfill})
	if err := s.ValidateNativeImportChoice(ctx, "google_voice", c.notifyImports); err != nil {
		return nil, err
	}
	syncID, err := s.StartSync(sourceID, "google_voice_import")
	if err != nil {
		return nil, fmt.Errorf("start Google Voice import: %w", err)
	}
	scoped := s.ScopedToSync(sourceID, syncID)
	run, err := scoped.BeginNativeImportAttempt(ctx, sourceID, "google_voice", c.notifyImports, syncID, 0)
	if err != nil {
		_ = s.FailSync(syncID, err.Error())
		return nil, fmt.Errorf("begin Google Voice import: %w", err)
	}
	scoped, err = scoped.WithImportRun(ctx, run.ID)
	if err != nil {
		_ = s.FailSync(syncID, err.Error())
		return &ImportSummary{ImportRunID: run.ID}, err
	}
	summary, err := c.importEntries(ctx, scoped, sourceID)
	if summary == nil {
		summary = &ImportSummary{}
	}
	summary.ImportRunID = run.ID
	if err != nil {
		_ = s.FailSync(syncID, err.Error())
		return summary, err
	}
	if err = scoped.UpdateSyncCheckpoint(syncID, &store.Checkpoint{MessagesProcessed: int64(summary.MessagesImported), MessagesAdded: int64(summary.MessagesImported)}); err != nil {
		_ = s.FailSync(syncID, err.Error())
		return summary, fmt.Errorf("update Google Voice checkpoint: %w", err)
	}
	if err = s.CompleteSync(syncID, ""); err != nil {
		return summary, fmt.Errorf("complete Google Voice import: %w", err)
	}
	return summary, nil
}

func (c *Client) importEntries(ctx context.Context, s *store.Store, sourceID int64) (*ImportSummary, error) {
	if err := c.buildIndex(); err != nil {
		return nil, fmt.Errorf("build index: %w", err)
	}

	summary := &ImportSummary{}

	var maxMessageID int64
	if err := s.DB().QueryRowContext(ctx, `SELECT COALESCE(MAX(id), 0) FROM messages`).Scan(&maxMessageID); err != nil {
		return nil, fmt.Errorf("read message boundary: %w", err)
	}

	// Ensure labels
	labelIDs, err := c.ensureLabels(s, sourceID)
	if err != nil {
		return nil, err
	}

	// Caches
	phoneCache := map[string]int64{} // phone → participantID
	convCache := map[string]int64{}  // threadID → conversationID
	imported := 0

	for _, entry := range c.index {
		if err := ctx.Err(); err != nil {
			return summary, err
		}
		if c.limit > 0 && imported >= c.limit {
			break
		}

		switch entry.FileType {
		case fileTypeText, fileTypeGroup:
			n, err := c.importTextEntry(
				ctx, s, sourceID, &entry,
				labelIDs, phoneCache, convCache, summary,
			)
			if err != nil {
				c.logger.Warn(
					"failed to import text entry",
					"id", entry.ID,
					"error", err,
				)
				summary.Skipped++
				continue
			}
			imported += n

		default:
			if err := c.importCallEntry(
				ctx, s, sourceID, &entry,
				labelIDs, phoneCache, convCache, summary,
				maxMessageID,
			); err != nil {
				c.logger.Warn(
					"failed to import call entry",
					"id", entry.ID,
					"error", err,
				)
				summary.Skipped++
				continue
			}
			imported++
		}
	}

	if err := s.RecomputeConversationStats(sourceID); err != nil {
		return summary, fmt.Errorf("recompute stats: %w", err)
	}

	return summary, nil
}

func (c *Client) ensureLabels(
	s *store.Store, sourceID int64,
) (map[string]int64, error) {
	labels := map[string]int64{}
	for _, name := range []string{
		"sms", "mms", "call_received",
		"call_placed", "call_missed", "voicemail",
	} {
		id, err := s.EnsureLabel(sourceID, name, name, "user")
		if err != nil {
			return nil, fmt.Errorf("ensure label %q: %w", name, err)
		}
		labels[name] = id
	}
	return labels, nil
}

// voiceParticipantPlan prepares native phone identities without database writes.
// Only a successful canonical transaction contributes IDs to the import caches.
type voiceParticipantPlan struct {
	participants []store.ParticipantPersistData
	phones       []string
	indices      map[string]int
}

func (p *voiceParticipantPlan) add(phone, name string) int {
	normalized, err := textimport.NormalizePhone(phone)
	if err != nil || normalized == "" {
		return -1
	}
	if p.indices == nil {
		p.indices = make(map[string]int)
	}
	if index, ok := p.indices[normalized]; ok {
		return index
	}
	index := len(p.participants)
	p.indices[normalized] = index
	p.phones = append(p.phones, normalized)
	p.participants = append(p.participants, store.ParticipantPersistData{PhoneNumber: normalized, IdentifierType: "google_voice", DisplayName: name})
	return index
}

func voiceParticipantID(ids []int64, index int) int64 {
	if index < 0 {
		return 0
	}
	return ids[index]
}

func voiceRecipients(from int64, to []int64, replaceEmptyTo bool) []store.RecipientSet {
	var result []store.RecipientSet
	if from > 0 {
		result = append(result, store.RecipientSet{Type: "from", ParticipantIDs: []int64{from}})
	}
	if len(to) > 0 || replaceEmptyTo {
		result = append(result, store.RecipientSet{Type: "to", ParticipantIDs: to})
	}
	return result
}

func (p *voiceParticipantPlan) committed(ids []int64, thread string, phoneCache, convCache map[string]int64, summary *ImportSummary) {
	for index, phone := range p.phones {
		if _, ok := phoneCache[phone]; !ok {
			phoneCache[phone] = ids[index]
			summary.ParticipantsResolved++
		}
	}
	if _, ok := convCache[thread]; !ok {
		convCache[thread] = 1
		summary.ConversationsImported++
	}
	summary.MessagesImported++
}

func voiceLabelIDs(entry *indexEntry, labels map[string]int64, mms bool) []int64 {
	var result []int64
	for _, name := range entry.Labels {
		if id, ok := labels[name]; ok {
			result = append(result, id)
		}
	}
	if mms {
		if id, ok := labels["mms"]; ok {
			result = append(result, id)
		}
	}
	return result
}

func (c *Client) importTextEntry(
	ctx context.Context, s *store.Store, sourceID int64, entry *indexEntry,
	labelIDs map[string]int64, phoneCache, convCache map[string]int64, summary *ImportSummary,
) (int, error) {
	raw, err := os.ReadFile(entry.FilePath)
	if err != nil {
		return 0, fmt.Errorf("read raw text HTML: %w", err)
	}
	messages, groupParticipants, err := c.getCachedMessages(entry.FilePath)
	if err != nil {
		return 0, err
	}
	if entry.MessageIndex >= len(messages) {
		return 0, fmt.Errorf("message index %d out of range (file has %d messages)", entry.MessageIndex, len(messages))
	}
	msg := messages[entry.MessageIndex]
	var plan voiceParticipantPlan
	owner := plan.add(c.owner.GoogleVoice, "")
	sender := plan.add(msg.SenderPhone, msg.SenderName)
	contact := sender
	if entry.FileType == fileTypeText && msg.IsMe {
		contact = -1
		for _, other := range messages {
			if !other.IsMe && other.SenderPhone != "" {
				if index := plan.add(other.SenderPhone, other.SenderName); index >= 0 {
					contact = index
					break
				}
			}
		}
	}
	var group []int
	for _, phone := range groupParticipants {
		if index := plan.add(phone, ""); index >= 0 {
			group = append(group, index)
		}
	}
	convType, title := "direct_chat", ""
	if entry.FileType == fileTypeGroup {
		convType, title = "group_chat", "Group"
	}
	var committedIDs []int64
	_, err = s.PersistMessageWithParticipantsContext(ctx, plan.participants, func(ids []int64) *store.MessagePersistData {
		committedIDs = append([]int64(nil), ids...)
		var members []store.ConversationParticipantRef
		for _, id := range ids {
			members = append(members, store.ConversationParticipantRef{ParticipantID: id, Role: "member"})
		}
		ownerID, senderID, contactID := voiceParticipantID(ids, owner), voiceParticipantID(ids, sender), voiceParticipantID(ids, contact)
		from := senderID
		var to []int64
		if msg.IsMe {
			from = ownerID
			if entry.FileType == fileTypeGroup && len(groupParticipants) > 0 {
				for _, index := range group {
					to = append(to, ids[index])
				}
			} else if contactID > 0 {
				to = []int64{contactID}
			}
		} else if ownerID > 0 {
			to = []int64{ownerID}
		}
		return &store.MessagePersistData{
			Message:      &store.Message{SourceID: sourceID, SourceMessageID: entry.ID, MessageType: MessageTypeForFileType(entry.FileType), SenderID: sql.NullInt64{Int64: senderID, Valid: senderID > 0}, IsFromMe: msg.IsMe, Snippet: nullStr(snippet(msg.Body)), SentAt: sql.NullTime{Time: msg.Timestamp, Valid: !msg.Timestamp.IsZero()}, HasAttachments: len(msg.Attachments) > 0, SizeEstimate: int64(len(msg.Body))},
			Conversation: &store.ConversationPersistData{SourceConversationID: entry.ThreadID, ConversationType: convType, Title: title, Participants: members, PreserveExistingParticipants: true},
			BodyText:     sql.NullString{String: msg.Body, Valid: msg.Body != ""}, RawMIME: raw, RawFormat: "gvoice_html",
			Recipients: voiceRecipients(from, to, true), PreserveLabels: true, AddLabelIDs: voiceLabelIDs(entry, labelIDs, len(msg.Attachments) > 0),
		}
	})
	if err != nil {
		return 0, fmt.Errorf("persist text snapshot: %w", err)
	}
	plan.committed(committedIDs, entry.ThreadID, phoneCache, convCache, summary)
	return 1, nil
}

func (c *Client) importCallEntry(
	ctx context.Context, s *store.Store, sourceID int64, entry *indexEntry,
	labelIDs map[string]int64, phoneCache, convCache map[string]int64, summary *ImportSummary, maxMessageID int64,
) error {
	raw, err := os.ReadFile(entry.FilePath)
	if err != nil {
		return fmt.Errorf("read raw call HTML: %w", err)
	}
	record, err := parseCallHTML(strings.NewReader(string(raw)))
	if err != nil {
		return err
	}
	var plan voiceParticipantPlan
	owner, contact := plan.add(c.owner.GoogleVoice, ""), plan.add(record.Phone, record.Name)
	var body strings.Builder
	switch record.CallType {
	case fileTypeReceived:
		fmt.Fprintf(&body, "Received call from %s", record.Name)
	case fileTypePlaced:
		fmt.Fprintf(&body, "Placed call to %s", record.Name)
	case fileTypeMissed:
		fmt.Fprintf(&body, "Missed call from %s", record.Name)
	case fileTypeVoicemail:
		fmt.Fprintf(&body, "Voicemail from %s", record.Name)
	case fileTypeText, fileTypeGroup:
	}
	if record.Duration != "" {
		fmt.Fprintf(&body, " (%s)", formatDuration(record.Duration))
	}
	bodyStr := body.String()
	var readyAudio *store.KeyedAttachmentSnapshot
	if c.notifyImports && entry.FileType == fileTypeVoicemail && c.attachmentsDir != "" {
		write := c.prepareVoicemailAudio(entry, record)
		// A failed reacquisition may reuse the stronger canonical stored occurrence.
		if write.State == attachmentpolicy.StateFailed {
			write.ContentHash = ""
		}
		readyAudio = &store.KeyedAttachmentSnapshot{Prefix: "gvoice:voicemail:", Writes: []store.AttachmentWrite{write}, PreserveStoredOnFailure: true, InvalidateCacheOnChange: true, PreserveUnmentioned: true}
	}
	var committedIDs []int64
	msgID, err := s.PersistMessageWithParticipantsContext(ctx, plan.participants, func(ids []int64) *store.MessagePersistData {
		committedIDs = append([]int64(nil), ids...)
		var members []store.ConversationParticipantRef
		for _, id := range ids {
			members = append(members, store.ConversationParticipantRef{ParticipantID: id, Role: "member"})
		}
		from, to := voiceParticipantID(ids, contact), voiceParticipantID(ids, owner)
		isMe := record.CallType == fileTypePlaced
		if isMe {
			from, to = to, from
		}
		var toIDs []int64
		if to > 0 {
			toIDs = []int64{to}
		}
		return &store.MessagePersistData{
			Message:                 &store.Message{SourceID: sourceID, SourceMessageID: entry.ID, MessageType: MessageTypeForFileType(entry.FileType), SenderID: sql.NullInt64{Int64: from, Valid: from > 0}, IsFromMe: isMe, Snippet: nullStr(snippet(bodyStr)), SentAt: sql.NullTime{Time: record.Timestamp, Valid: !record.Timestamp.IsZero()}, SizeEstimate: int64(len(bodyStr)), PreserveAttachmentStats: true},
			Conversation:            &store.ConversationPersistData{SourceConversationID: entry.ThreadID, ConversationType: "direct_chat", Participants: members, PreserveExistingParticipants: true},
			KeyedAttachmentSnapshot: readyAudio,
			BodyText:                sql.NullString{String: bodyStr, Valid: bodyStr != ""}, RawMIME: raw, RawFormat: "gvoice_html", Recipients: voiceRecipients(from, toIDs, false), PreserveLabels: true, AddLabelIDs: voiceLabelIDs(entry, labelIDs, false),
		}
	})
	if err != nil {
		return fmt.Errorf("persist call snapshot: %w", err)
	}
	if !c.notifyImports {
		if err := c.storeVoicemailAudio(ctx, s, msgID, entry, record, msgID <= maxMessageID); err != nil {
			c.logger.Warn("failed to associate voicemail audio", "id", entry.ID, "error", err)
		}
	}
	plan.committed(committedIDs, entry.ThreadID, phoneCache, convCache, summary)
	return nil
}

// prepareVoicemailAudio publishes only content-addressed files, without SQL
// or authority locks. Canonical references are committed separately.
func (c *Client) prepareVoicemailAudio(entry *indexEntry, record *callRecord) store.AttachmentWrite {
	name := record.AudioSrc
	write := store.AttachmentWrite{
		Filename:      name,
		MediaType:     "audio",
		Role:          store.AttachmentRoleStandalone,
		RoleSource:    store.AttachmentRoleSourceImporterSemantics,
		SourcePartKey: voicemailAudioPartKey,
		State:         attachmentpolicy.StateFailed,
		SkipReason:    attachmentpolicy.SkipFetchFailure,
	}

	if name != "" &&
		!strings.ContainsAny(name, "/\\:\x00") &&
		strings.EqualFold(filepath.Ext(name), ".mp3") {
		write.MIMEType = "audio/mpeg"
		srcPath := filepath.Join(filepath.Dir(entry.FilePath), name)
		relPath, contentHash, size, err := export.StoreAttachmentFromPath(
			c.attachmentsDir, srcPath, 0,
		)
		if err == nil {
			write.StoragePath = relPath
			write.ContentHash = contentHash
			write.Size = size
			write.State = attachmentpolicy.StateStored
			write.SkipReason = ""
		} else {
			write.ContentHash = contentHash
		}
	}

	return write
}

// storeVoicemailAudio associates the recording named by one voicemail HTML
// record with its message, or records a typed acquisition gap.
func (c *Client) storeVoicemailAudio(
	ctx context.Context,
	s *store.Store,
	msgID int64,
	entry *indexEntry,
	record *callRecord,
	invalidateCache bool,
) error {
	if entry.FileType != fileTypeVoicemail {
		return nil
	}
	if c.attachmentsDir == "" {
		return nil
	}

	var previous store.AttachmentWrite
	err := s.DB().QueryRowContext(ctx, s.Rebind(`
		SELECT COALESCE(filename, ''), COALESCE(mime_type, ''), COALESCE(storage_path, ''),
		       COALESCE(content_hash, ''), COALESCE(size, 0),
		       COALESCE(source_attachment_id, ''), COALESCE(media_type, ''),
		       COALESCE(width, 0), COALESCE(height, 0), COALESCE(duration_ms, 0),
		       COALESCE(CAST(attachment_metadata AS TEXT), ''),
		       COALESCE(attachment_role, ''), COALESCE(role_source, ''),
		       COALESCE(source_part_key, ''), COALESCE(content_id, ''),
		       COALESCE(attachment_state, ''), COALESCE(attachment_skip_reason, '')
		FROM attachments
		WHERE message_id = ? AND source_part_key = ?
	`), msgID, voicemailAudioPartKey).Scan(
		&previous.Filename, &previous.MIMEType, &previous.StoragePath, &previous.ContentHash, &previous.Size,
		&previous.SourceAttachmentID, &previous.MediaType, &previous.Width, &previous.Height, &previous.DurationMS,
		&previous.Metadata, &previous.Role, &previous.RoleSource, &previous.SourcePartKey, &previous.ContentID,
		&previous.State, &previous.SkipReason,
	)
	if err != nil && err != sql.ErrNoRows {
		return fmt.Errorf("check existing voicemail audio: %w", err)
	}
	existing := err == nil

	write := c.prepareVoicemailAudio(entry, record)

	if write.State == attachmentpolicy.StateFailed && existing &&
		previous.State == attachmentpolicy.StateStored &&
		previous.StoragePath != "" && previous.ContentHash != "" {
		return nil
	}
	if existing && write == previous {
		return nil
	}

	if err := s.UpsertAttachmentRecordWithStats(ctx, msgID, write, invalidateCache); err != nil {
		return fmt.Errorf("write voicemail audio record: %w", err)
	}
	return nil
}

// buildIndex walks the Calls directory, parses each HTML file, and
// builds a sorted index of all messages and call records.
func (c *Client) buildIndex() error {
	if c.indexBuilt {
		return nil
	}

	callsDir := filepath.Join(c.takeoutDir, "Calls")
	entries, err := os.ReadDir(callsDir)
	if err != nil {
		return fmt.Errorf("read Calls directory: %w", err)
	}

	c.logger.Info("building index", "files", len(entries))

	var index []indexEntry
	skipped := 0

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}

		_, ft, err := classifyFile(entry.Name())
		if err != nil {
			skipped++
			continue
		}

		filePath := filepath.Join(callsDir, entry.Name())

		switch ft {
		case fileTypeText, fileTypeGroup:
			ents, err := c.indexTextFile(filePath, ft)
			if err != nil {
				c.logger.Warn(
					"failed to index text file",
					"file", entry.Name(),
					"error", err,
				)
				continue
			}
			index = append(index, ents...)

		default:
			ent, err := c.indexCallFile(filePath, ft)
			if err != nil {
				c.logger.Warn(
					"failed to index call file",
					"file", entry.Name(),
					"error", err,
				)
				continue
			}
			index = append(index, *ent)
		}
	}

	// Apply date filters
	var filtered []indexEntry
	for _, e := range index {
		if !c.afterDate.IsZero() && e.Timestamp.Before(c.afterDate) {
			continue
		}
		if !c.beforeDate.IsZero() &&
			!e.Timestamp.Before(c.beforeDate) {
			continue
		}
		filtered = append(filtered, e)
	}

	sort.Slice(filtered, func(i, j int) bool {
		return filtered[i].Timestamp.Before(filtered[j].Timestamp)
	})

	c.index = filtered
	c.indexBuilt = true

	c.logger.Info("index built",
		"total_entries", len(index),
		"filtered_entries", len(filtered),
		"skipped_files", skipped,
	)

	return nil
}

func (c *Client) indexTextFile(
	filePath string, ft fileType,
) ([]indexEntry, error) {
	f, err := os.Open(filePath)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	messages, groupParticipants, err := parseTextHTML(f)
	if err != nil {
		return nil, err
	}

	var result []indexEntry
	for i, msg := range messages {
		bodyPrefix := msg.Body
		if len(bodyPrefix) > 50 {
			bodyPrefix = bodyPrefix[:50]
		}
		id := computeMessageID(
			msg.SenderPhone,
			msg.Timestamp.Format(time.RFC3339Nano),
			bodyPrefix,
		)

		var threadID string
		if ft == fileTypeGroup {
			threadID = computeThreadID(
				fileTypeGroup,
				"", groupParticipants,
			)
		} else {
			otherPhone := msg.SenderPhone
			if msg.IsMe {
				for _, m := range messages {
					if !m.IsMe {
						otherPhone = m.SenderPhone
						break
					}
				}
			}
			threadID = computeThreadID(
				fileTypeText,
				otherPhone, nil,
			)
		}

		label := labelForFileType(ft)
		result = append(result, indexEntry{
			ID:           id,
			ThreadID:     threadID,
			FilePath:     filePath,
			MessageIndex: i,
			Timestamp:    msg.Timestamp,
			FileType:     ft,
			Labels:       []string{label},
		})
	}

	return result, nil
}

func (c *Client) indexCallFile(
	filePath string, ft fileType,
) (*indexEntry, error) {
	f, err := os.Open(filePath)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	record, err := parseCallHTML(f)
	if err != nil {
		return nil, err
	}

	if record.CallType != 0 {
		ft = record.CallType
	}

	id := computeMessageID(
		ft.String(), record.Phone,
		record.Timestamp.Format(time.RFC3339Nano),
	)
	threadID := computeThreadID(
		ft, record.Phone, nil,
	)
	label := labelForFileType(ft)

	return &indexEntry{
		ID:        id,
		ThreadID:  threadID,
		FilePath:  filePath,
		Timestamp: record.Timestamp,
		FileType:  ft,
		Labels:    []string{label},
	}, nil
}

// getCachedMessages returns parsed messages for a file, using a
// simple LRU cache.
func (c *Client) getCachedMessages(
	filePath string,
) ([]textMessage, []string, error) {
	if c.lastFilePath == filePath {
		return c.lastMessages, c.lastGroupPar, nil
	}

	f, err := os.Open(filePath)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = f.Close() }()

	messages, groupParticipants, err := parseTextHTML(f)
	if err != nil {
		return nil, nil, err
	}

	c.lastFilePath = filePath
	c.lastMessages = messages
	c.lastGroupPar = groupParticipants

	return messages, groupParticipants, nil
}

func nullStr(s string) sql.NullString {
	if s == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: s, Valid: true}
}

// formatDuration converts ISO 8601 duration to human-readable format.
func formatDuration(iso string) string {
	iso = strings.TrimPrefix(iso, "PT")
	var parts []string

	if i := strings.Index(iso, "H"); i >= 0 {
		parts = append(parts, iso[:i]+"h")
		iso = iso[i+1:]
	}
	if i := strings.Index(iso, "M"); i >= 0 {
		parts = append(parts, iso[:i]+"m")
		iso = iso[i+1:]
	}
	if before, _, ok := strings.Cut(iso, "S"); ok {
		parts = append(parts, before+"s")
	}

	if len(parts) == 0 {
		return "0s"
	}
	return strings.Join(parts, " ")
}
