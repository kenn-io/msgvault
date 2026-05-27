// Package sync provides Gmail synchronization workflows.
package sync

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/export"
	"go.kenn.io/msgvault/internal/gmail"
	"go.kenn.io/msgvault/internal/mime"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/textutil"
)

// ErrHistoryExpired indicates that the Gmail history ID is too old and a full sync is required.
var ErrHistoryExpired = errors.New("history expired - run full sync")

// EmbedEnqueuer is optionally supplied to a Syncer; nil means vector
// search is disabled. Set via SetEmbedEnqueuer.
type EmbedEnqueuer interface {
	EnqueueMessages(ctx context.Context, messageIDs []int64) error
}

// Options configures sync behavior.
type Options struct {
	// SourceType is the type of source being synced ("gmail" or "imap").
	// Defaults to "gmail" if empty.
	SourceType string

	// Query is an optional Gmail search query (e.g., "before:2020/01/01")
	Query string

	// NoResume forces a fresh sync even if a checkpoint exists
	NoResume bool

	// BatchSize is the number of messages to fetch in parallel (default: 10)
	BatchSize int

	// AttachmentsDir is where to store attachments
	AttachmentsDir string

	// Limit caps the number of messages scanned per sync (0 = unlimited).
	// Enforced by truncating the message ID list before downloading content.
	// The API listing call (which returns lightweight IDs, not bodies) may
	// return more IDs than the limit; only the truncated set is fetched.
	Limit int
}

// DefaultOptions returns sensible defaults.
func DefaultOptions() *Options {
	return &Options{
		BatchSize:  10,
		SourceType: "gmail",
	}
}

// Syncer performs Gmail synchronization.
type Syncer struct {
	client        gmail.API
	store         *store.Store
	logger        *slog.Logger
	progress      gmail.SyncProgress
	opts          *Options
	embedEnqueuer EmbedEnqueuer
}

// New creates a new Syncer.
func New(client gmail.API, store *store.Store, opts *Options) *Syncer {
	if opts == nil {
		opts = DefaultOptions()
	}

	return &Syncer{
		client:   client,
		store:    store,
		logger:   slog.Default(),
		progress: gmail.NullProgress{},
		opts:     opts,
	}
}

// WithLogger sets the logger.
func (s *Syncer) WithLogger(logger *slog.Logger) *Syncer {
	s.logger = logger
	return s
}

// WithProgress sets the progress reporter.
func (s *Syncer) WithProgress(p gmail.SyncProgress) *Syncer {
	s.progress = p
	return s
}

// SetEmbedEnqueuer wires up the optional vector-search enqueuer. Safe
// to call with nil to disable.
func (s *Syncer) SetEmbedEnqueuer(e EmbedEnqueuer) {
	s.embedEnqueuer = e
}

// syncState holds the state for a sync operation.
type syncState struct {
	syncID     int64
	checkpoint *store.Checkpoint
	pageToken  string
	wasResumed bool
}

// initSyncState initializes sync state, resuming from checkpoint if possible.
func (s *Syncer) initSyncState(sourceID int64) (*syncState, error) {
	state := &syncState{
		checkpoint: &store.Checkpoint{},
	}

	if !s.opts.NoResume {
		activeSync, err := s.store.GetActiveSync(sourceID)
		if err != nil && !errors.Is(err, store.ErrSyncRunNotFound) {
			return nil, fmt.Errorf("check active sync: %w", err)
		}
		if activeSync != nil {
			state.syncID = activeSync.ID
			if activeSync.CursorBefore.Valid {
				state.pageToken = activeSync.CursorBefore.String
			}
			state.checkpoint = &store.Checkpoint{
				PageToken:         state.pageToken,
				MessagesProcessed: activeSync.MessagesProcessed,
				MessagesAdded:     activeSync.MessagesAdded,
				MessagesUpdated:   activeSync.MessagesUpdated,
				ErrorsCount:       activeSync.ErrorsCount,
			}
			state.wasResumed = true
			s.logger.Info("resuming sync", "messages_processed", state.checkpoint.MessagesProcessed)
			return state, nil
		}
	}

	// Start new sync
	syncID, err := s.store.StartSync(sourceID, "full")
	if err != nil {
		return nil, fmt.Errorf("start sync: %w", err)
	}
	state.syncID = syncID
	return state, nil
}

// batchResult holds the result of processing a batch.
type batchResult struct {
	processed  int64
	added      int64
	skipped    int64
	oldestDate time.Time
}

// processBatch processes a single batch of messages from a list response.
func (s *Syncer) processBatch(ctx context.Context, sourceID int64, listResp *gmail.MessageListResponse, labelMap map[string]int64, checkpoint *store.Checkpoint, summary *gmail.SyncSummary) (*batchResult, error) {
	result := &batchResult{}

	if len(listResp.Messages) == 0 {
		return result, nil
	}

	// Build message ID list and thread ID map
	messageIDs := make([]string, len(listResp.Messages))
	threadIDs := make(map[string]string) // messageID -> threadID
	for i, m := range listResp.Messages {
		messageIDs[i] = m.ID
		threadIDs[m.ID] = m.ThreadID
	}

	// Check which messages already exist
	existingMap, err := s.store.MessageExistsWithRawBatch(sourceID, messageIDs)
	if err != nil {
		return nil, fmt.Errorf("check existing: %w", err)
	}

	// Filter to new messages
	var newIDs []string
	for _, id := range messageIDs {
		if _, exists := existingMap[id]; !exists {
			newIDs = append(newIDs, id)
		}
	}

	result.processed = int64(len(messageIDs))
	result.skipped = int64(len(messageIDs) - len(newIDs))

	// Fetch and ingest new messages
	if len(newIDs) > 0 {
		rawMessages, err := s.client.GetMessagesRawBatch(ctx, newIDs)
		if err != nil {
			return nil, fmt.Errorf("fetch messages: %w", err)
		}

		var insertedIDs []int64
		for i, raw := range rawMessages {
			if raw == nil {
				s.logger.Warn("failed to fetch message (nil response)", "id", newIDs[i])
				checkpoint.ErrorsCount++
				continue
			}
			// Non-nil stub with nil Raw signals a cross-mailbox
			// dedup skip (e.g. same message in All Mail and Trash).
			// Distinct from []byte{} which is a genuine empty body.
			if raw.Raw == nil {
				result.skipped++
				continue
			}

			// Track oldest message date for progress display
			// Gmail returns messages newest-to-oldest, so oldest shows where we've reached
			if raw.InternalDate > 0 {
				msgDate := time.UnixMilli(raw.InternalDate)
				if result.oldestDate.IsZero() || msgDate.Before(result.oldestDate) {
					result.oldestDate = msgDate
				}
			}

			threadID := threadIDs[newIDs[i]]
			insertedID, err := s.ingestMessage(sourceID, raw, threadID, labelMap)
			if err != nil {
				if errors.Is(err, errDuplicateRFC822) {
					result.skipped++
					continue
				}
				s.logger.Warn("failed to ingest message", "id", raw.ID, "error", err)
				checkpoint.ErrorsCount++
				continue
			}

			if insertedID > 0 {
				insertedIDs = append(insertedIDs, insertedID)
			}
			result.added++
			summary.BytesDownloaded += int64(len(raw.Raw))
		}

		// Hook vector-search enqueue after the batch-insert point.
		// Non-fatal on failure: missed IDs get picked up by full-rebuild.
		if s.embedEnqueuer != nil && len(insertedIDs) > 0 {
			if err := s.embedEnqueuer.EnqueueMessages(ctx, insertedIDs); err != nil {
				s.logger.Warn("vector enqueue failed", "ids", len(insertedIDs), "error", err)
			}
		}
	}

	return result, nil
}

// Full performs a full synchronization.
func (s *Syncer) Full(ctx context.Context, email string) (summary *gmail.SyncSummary, err error) {
	startTime := time.Now()
	summary = &gmail.SyncSummary{StartTime: startTime}

	// Get or create source
	sourceType := s.opts.SourceType
	if sourceType == "" {
		sourceType = "gmail"
	}
	source, err := s.store.GetOrCreateSource(sourceType, email)
	if err != nil {
		return nil, fmt.Errorf("get/create source: %w", err)
	}

	// Initialize sync state (resume or start new)
	state, err := s.initSyncState(source.ID)
	if err != nil {
		return nil, err
	}
	summary.WasResumed = state.wasResumed
	summary.ResumedFromToken = state.pageToken

	// Defer failure handling — recover from panics and return as error
	defer func() {
		if r := recover(); r != nil {
			stack := debug.Stack()
			s.logger.Error("sync panic recovered", "panic", r, "stack", string(stack))
			if failErr := s.store.FailSync(state.syncID, fmt.Sprintf("panic: %v", r)); failErr != nil {
				s.logger.Error("failed to record sync failure", "error", failErr)
			}
			summary = nil
			err = fmt.Errorf("sync panicked: %v", r)
		}
	}()

	// Get profile to verify connection and get historyId
	profile, err := s.client.GetProfile(ctx)
	if err != nil {
		_ = s.store.FailSync(state.syncID, err.Error())
		return nil, fmt.Errorf("get profile: %w", err)
	}

	s.logger.Info("syncing account", "email", profile.EmailAddress, "messages", profile.MessagesTotal)

	// Sync labels
	labelMap, err := s.syncLabels(ctx, source.ID)
	if err != nil {
		_ = s.store.FailSync(state.syncID, err.Error())
		return nil, fmt.Errorf("sync labels: %w", err)
	}

	// List and sync messages
	var totalEstimate int64
	firstPage := true
	pageToken := state.pageToken

	for {
		// List messages
		listResp, err := s.client.ListMessages(ctx, s.opts.Query, pageToken)
		if err != nil {
			_ = s.store.FailSync(state.syncID, err.Error())
			return nil, fmt.Errorf("list messages: %w", err)
		}

		if firstPage {
			totalEstimate = listResp.ResultSizeEstimate
			s.progress.OnStart(totalEstimate)
			firstPage = false
		}

		if len(listResp.Messages) == 0 {
			break
		}

		// Enforce limit by truncating before the expensive content download
		if s.opts.Limit > 0 {
			remaining := int64(s.opts.Limit) - state.checkpoint.MessagesProcessed
			if remaining <= 0 {
				break
			}
			if int64(len(listResp.Messages)) > remaining {
				listResp.Messages = listResp.Messages[:int(remaining)]
			}
		}

		// Process batch
		result, err := s.processBatch(ctx, source.ID, listResp, labelMap, state.checkpoint, summary)
		if err != nil {
			_ = s.store.FailSync(state.syncID, err.Error())
			return nil, err
		}

		state.checkpoint.MessagesProcessed += result.processed
		state.checkpoint.MessagesAdded += result.added

		// Report current position date before progress (so UI shows consistent state)
		if !result.oldestDate.IsZero() {
			if p, ok := s.progress.(gmail.SyncProgressWithDate); ok {
				p.OnLatestDate(result.oldestDate)
			}
		}

		// Report progress
		s.progress.OnProgress(state.checkpoint.MessagesProcessed, state.checkpoint.MessagesAdded, result.skipped)

		// Save checkpoint
		pageToken = listResp.NextPageToken
		state.checkpoint.PageToken = pageToken
		if err := s.store.UpdateSyncCheckpoint(state.syncID, state.checkpoint); err != nil {
			s.logger.Warn("failed to save checkpoint", "error", err)
		}

		// Stop if we've hit the limit
		if s.opts.Limit > 0 && state.checkpoint.MessagesProcessed >= int64(s.opts.Limit) {
			break
		}

		// No more pages
		if pageToken == "" {
			break
		}
	}

	// Update source with final history ID.
	// Full sync always advances the cursor (it records the starting point
	// for future incremental syncs), but warn when errors occurred.
	historyIDStr := strconv.FormatUint(profile.HistoryID, 10)
	if state.checkpoint.ErrorsCount > 0 {
		s.logger.Warn("full sync completed with errors",
			"errors", state.checkpoint.ErrorsCount,
			"history_id", historyIDStr)
	}
	if err := s.store.UpdateSourceSyncCursor(source.ID, historyIDStr); err != nil {
		s.logger.Warn("failed to update sync cursor", "error", err)
	}

	// Mark sync complete
	if err := s.store.CompleteSync(state.syncID, historyIDStr); err != nil {
		s.logger.Warn("failed to complete sync", "error", err)
	}

	// Checkpoint WAL after sync to fold it back into the main database.
	// This prevents WAL accumulation across long sync sessions and ensures
	// readers (e.g. build-cache) see a consistent database state.
	if err := s.store.CheckpointWAL(); err != nil {
		s.logger.Warn("wal checkpoint after sync failed", "error", err)
	}

	// Build summary
	summary.EndTime = time.Now()
	summary.Duration = summary.EndTime.Sub(summary.StartTime)
	summary.MessagesFound = state.checkpoint.MessagesProcessed
	summary.MessagesAdded = state.checkpoint.MessagesAdded
	summary.MessagesUpdated = state.checkpoint.MessagesUpdated
	summary.MessagesSkipped = state.checkpoint.MessagesProcessed - state.checkpoint.MessagesAdded - state.checkpoint.MessagesUpdated
	summary.Errors = state.checkpoint.ErrorsCount
	summary.FinalHistoryID = profile.HistoryID

	s.progress.OnComplete(summary)
	return summary, nil
}

// syncLabels syncs all labels and returns a map of Gmail label ID to internal ID.
func (s *Syncer) syncLabels(ctx context.Context, sourceID int64) (map[string]int64, error) {
	labels, err := s.client.ListLabels(ctx)
	if err != nil {
		return nil, err
	}

	labelInfos := make(map[string]store.LabelInfo)
	for _, l := range labels {
		labelType := l.Type
		if labelType == "" {
			labelType = "user"
			if store.IsSystemLabel(l.ID) {
				labelType = "system"
			}
		}
		labelInfos[l.ID] = store.LabelInfo{Name: l.Name, Type: labelType}
	}

	return s.store.EnsureLabelsBatch(sourceID, labelInfos)
}

// messageData holds all parsed data for a message before persistence.
type messageData struct {
	message        *store.Message
	bodyText       string
	bodyHTML       string
	rawMIME        []byte
	from           []mime.Address
	to             []mime.Address
	cc             []mime.Address
	bcc            []mime.Address
	gmailLabelIDs  []string
	attachments    []mime.Attachment
	participantMap map[string]int64
}

// parseToModel parses a raw Gmail message into a messageData struct.
func (s *Syncer) parseToModel(sourceID int64, raw *gmail.RawMessage, threadID string) (*messageData, error) {
	// Validate raw MIME data exists
	if len(raw.Raw) == 0 {
		return nil, fmt.Errorf("missing raw MIME data for message %s", raw.ID)
	}

	// Fall back to raw.ThreadID if list response threadID is missing,
	// then to message ID as last resort
	if threadID == "" {
		threadID = raw.ThreadID
		if threadID == "" {
			threadID = raw.ID
		}
	}

	// Parse MIME - on failure, store with placeholder body
	// (threading override for IMAP happens after parsing below)
	parsed, parseErr := mime.Parse(raw.Raw)
	if parseErr != nil {
		// Extract just the first line of error (enmime includes full stack traces)
		errMsg := textutil.FirstLine(parseErr.Error())

		// Create placeholder message for MIME parse failures
		// This preserves the raw data for potential future re-parsing
		parsed = &mime.Message{
			Subject:  extractSubjectFromSnippet(raw.Snippet),
			BodyText: fmt.Sprintf("[MIME parsing failed: %s]\n\nRaw MIME data is preserved in message_raw table.", errMsg),
		}
		// Set date from InternalDate if available
		if raw.InternalDate > 0 {
			parsed.Date = time.UnixMilli(raw.InternalDate).UTC()
		}
		s.logger.Warn("MIME parse failed, storing with placeholder",
			"id", raw.ID,
			"error", errMsg)
	}

	// For IMAP sources, the API provides no real thread info —
	// ThreadID is just the composite message ID. Derive a thread
	// key from MIME threading headers to group related messages
	// into conversations.
	if s.opts.SourceType == "imap" {
		if derived := deriveThreadKey(parsed); derived != "" {
			threadID = derived
		}
	}

	// Ensure all text fields are valid UTF-8
	subject := textutil.EnsureUTF8(parsed.Subject)
	bodyText := textutil.EnsureUTF8(parsed.GetBodyText())
	bodyHTML := textutil.EnsureUTF8(parsed.BodyHTML)
	snippet := textutil.EnsureUTF8(raw.Snippet)

	// Ensure participant names are valid UTF-8 before database insertion
	ensureAddressUTF8(parsed.From)
	ensureAddressUTF8(parsed.To)
	ensureAddressUTF8(parsed.Cc)
	ensureAddressUTF8(parsed.Bcc)

	// Ensure attachment filenames and content types are valid UTF-8
	for i := range parsed.Attachments {
		parsed.Attachments[i].Filename = textutil.EnsureUTF8(parsed.Attachments[i].Filename)
		parsed.Attachments[i].ContentType = textutil.EnsureUTF8(parsed.Attachments[i].ContentType)
	}

	// Ensure participants exist in database
	allAddresses := make([]mime.Address, 0, len(parsed.From)+len(parsed.To)+len(parsed.Cc)+len(parsed.Bcc))
	allAddresses = append(allAddresses, parsed.From...)
	allAddresses = append(allAddresses, parsed.To...)
	allAddresses = append(allAddresses, parsed.Cc...)
	allAddresses = append(allAddresses, parsed.Bcc...)
	participantMap, err := s.store.EnsureParticipantsBatch(allAddresses)
	if err != nil {
		return nil, fmt.Errorf("ensure participants: %w", err)
	}

	// Get sender ID
	var senderID sql.NullInt64
	if len(parsed.From) > 0 && parsed.From[0].Email != "" {
		if id, ok := participantMap[parsed.From[0].Email]; ok {
			senderID = sql.NullInt64{Int64: id, Valid: true}
		}
	}

	// Use placeholder for conversation matching only (subject can be empty for storage)
	convSubject := subject
	if convSubject == "" {
		convSubject = "(no subject)"
	}
	conversationID, err := s.store.EnsureConversation(sourceID, threadID, convSubject)
	if err != nil {
		return nil, fmt.Errorf("ensure conversation: %w", err)
	}

	// Build message record
	rfc822ID := sql.NullString{}
	if parsed.MessageID != "" {
		rfc822ID = sql.NullString{
			String: parsed.MessageID, Valid: true,
		}
	}
	msg := &store.Message{
		ConversationID:  conversationID,
		SourceID:        sourceID,
		SourceMessageID: raw.ID,
		RFC822MessageID: rfc822ID,
		MessageType:     "email",
		SenderID:        senderID,
		Subject:         sql.NullString{String: subject, Valid: subject != ""},
		Snippet:         sql.NullString{String: snippet, Valid: snippet != ""},
		SizeEstimate:    raw.SizeEstimate,
		HasAttachments:  len(parsed.Attachments) > 0,
		AttachmentCount: len(parsed.Attachments),
	}

	// Set dates - always store in UTC for consistent querying
	if raw.InternalDate > 0 {
		t := time.UnixMilli(raw.InternalDate).UTC()
		msg.InternalDate = sql.NullTime{Time: t, Valid: true}
	}
	if !parsed.Date.IsZero() {
		msg.SentAt = sql.NullTime{Time: parsed.Date, Valid: true}
	} else if msg.InternalDate.Valid {
		// Fall back to InternalDate if Date header couldn't be parsed
		msg.SentAt = msg.InternalDate
	}

	return &messageData{
		message:        msg,
		bodyText:       bodyText,
		bodyHTML:       bodyHTML,
		rawMIME:        raw.Raw,
		from:           parsed.From,
		to:             parsed.To,
		cc:             parsed.Cc,
		bcc:            parsed.Bcc,
		gmailLabelIDs:  raw.LabelIDs,
		attachments:    parsed.Attachments,
		participantMap: participantMap,
	}, nil
}

// persistMessage stores a parsed message and all related data. Returns
// the internal message ID for hooks (e.g. vector-search enqueue).
func (s *Syncer) persistMessage(data *messageData, labelMap map[string]int64) (int64, error) {
	// Map Gmail label IDs to internal IDs
	var labelIDs []int64
	for _, gmailLabelID := range data.gmailLabelIDs {
		if internalID, ok := labelMap[gmailLabelID]; ok {
			labelIDs = append(labelIDs, internalID)
		}
	}

	// Build recipient sets
	recipients := []struct {
		typ   string
		addrs []mime.Address
	}{
		{"from", data.from},
		{"to", data.to},
		{"cc", data.cc},
		{"bcc", data.bcc},
	}
	var recipientSets []store.RecipientSet
	for _, r := range recipients {
		rs := buildRecipientSet(r.typ, r.addrs, data.participantMap)
		recipientSets = append(recipientSets, rs)
	}

	// Persist atomically
	messageID, err := s.store.PersistMessage(&store.MessagePersistData{
		Message:    data.message,
		BodyText:   sql.NullString{String: data.bodyText, Valid: data.bodyText != ""},
		BodyHTML:   sql.NullString{String: data.bodyHTML, Valid: data.bodyHTML != ""},
		RawMIME:    data.rawMIME,
		Recipients: recipientSets,
		LabelIDs:   labelIDs,
	})
	if err != nil {
		return 0, err
	}

	// Store attachments (best-effort, file I/O outside transaction)
	if s.opts.AttachmentsDir != "" && len(data.attachments) > 0 {
		for _, att := range data.attachments {
			if err := s.storeAttachment(messageID, &att); err != nil {
				s.logger.Warn("failed to store attachment", "message", messageID, "filename", att.Filename, "error", err)
			}
		}

		// Correct metadata if any attachments failed to store
		var storedCount int
		if err := s.store.DB().QueryRow(
			s.store.Rebind(`SELECT COUNT(*) FROM attachments WHERE message_id = ?`),
			messageID,
		).Scan(&storedCount); err != nil {
			s.logger.Warn("failed to count stored attachments",
				"message", messageID, "error", err)
		} else if storedCount != len(data.attachments) {
			if _, err := s.store.DB().Exec(
				s.store.Rebind(`UPDATE messages SET has_attachments = ?, attachment_count = ? WHERE id = ?`),
				storedCount > 0, storedCount, messageID,
			); err != nil {
				s.logger.Warn("failed to update attachment metadata",
					"message", messageID, "error", err)
			}
		}
	}

	// Populate FTS index (best-effort outside transaction)
	if s.store.FTS5Available() {
		subject := ""
		if data.message.Subject.Valid {
			subject = data.message.Subject.String
		}
		fromAddr := joinEmails(data.from)
		toAddrs := joinEmails(data.to)
		ccAddrs := joinEmails(data.cc)
		if err := s.store.UpsertFTS(messageID, subject, data.bodyText, fromAddr, toAddrs, ccAddrs); err != nil {
			s.logger.Warn("failed to upsert FTS", "message", messageID, "error", err)
		}
	}

	return messageID, nil
}

// errDuplicateRFC822 signals that a message was skipped because
// another message with the same RFC822 Message-ID already exists
// for this source. Used for cross-sync dedup on IMAP where
// composite IDs change when messages move between mailboxes.
var errDuplicateRFC822 = errors.New("duplicate RFC822 Message-ID")

// ingestMessage parses and stores a single message, returning the
// internal message ID on success. Returns (0, errDuplicateRFC822) for
// IMAP deduplication skips.
func (s *Syncer) ingestMessage(sourceID int64, raw *gmail.RawMessage, threadID string, labelMap map[string]int64) (int64, error) {
	data, err := s.parseToModel(sourceID, raw, threadID)
	if err != nil {
		return 0, err
	}

	// For IMAP sources, check if a message with the same RFC822
	// Message-ID already exists under a different composite ID.
	// This handles messages that moved between mailboxes across
	// syncs (e.g. All Mail → Trash changes the mailbox|uid key).
	// When matched, update the existing row's composite ID and
	// labels so future syncs skip it at the ID-filtering stage
	// instead of re-downloading the MIME body each time.
	if s.opts.SourceType == "imap" &&
		data.message.RFC822MessageID.Valid {
		existingID, err := s.store.GetMessageIDByRFC822ID(
			sourceID, data.message.RFC822MessageID.String)
		if err != nil {
			return 0, fmt.Errorf("check rfc822 dedup: %w", err)
		}
		if existingID > 0 {
			var labelIDs []int64
			for _, lbl := range data.gmailLabelIDs {
				if id, ok := labelMap[lbl]; ok {
					labelIDs = append(labelIDs, id)
				}
			}
			if err := s.store.UpdateMessageOnDedup(
				existingID,
				data.message.SourceMessageID,
				labelIDs,
			); err != nil {
				return 0, fmt.Errorf("update dedup message: %w", err)
			}
			return 0, errDuplicateRFC822
		}
	}

	return s.persistMessage(data, labelMap)
}

// ensureAddressUTF8 validates and converts address names to valid UTF-8 in place.
func ensureAddressUTF8(addrs []mime.Address) {
	for i := range addrs {
		addrs[i].Name = textutil.EnsureUTF8(addrs[i].Name)
	}
}

// buildRecipientSet deduplicates addresses and returns a RecipientSet
// ready for store.PersistMessage.
func buildRecipientSet(recipientType string, addresses []mime.Address, participantMap map[string]int64) store.RecipientSet {
	rs := store.RecipientSet{Type: recipientType}
	if len(addresses) == 0 {
		return rs
	}

	// Track participant ID -> display name, preferring non-empty names.
	// Handles duplicates where the first occurrence might have an empty
	// name but a later occurrence has a better display name.
	idToName := make(map[int64]string)
	var orderedIDs []int64

	for _, addr := range addresses {
		if id, ok := participantMap[addr.Email]; ok {
			name := textutil.EnsureUTF8(addr.Name)
			if _, seen := idToName[id]; !seen {
				orderedIDs = append(orderedIDs, id)
				idToName[id] = name
			} else if idToName[id] == "" && name != "" {
				idToName[id] = name
			}
		}
	}

	rs.ParticipantIDs = orderedIDs
	rs.DisplayNames = make([]string, len(orderedIDs))
	for i, id := range orderedIDs {
		rs.DisplayNames[i] = idToName[id]
	}
	return rs
}

// storeAttachment stores an attachment to disk and records it in the database.
func (s *Syncer) storeAttachment(messageID int64, att *mime.Attachment) error {
	storagePath, err := export.StoreAttachmentFile(s.opts.AttachmentsDir, att)
	if err != nil || storagePath == "" {
		return err
	}

	// Record in database
	return s.store.UpsertAttachment(messageID, att.Filename, att.ContentType, storagePath, att.ContentHash, len(att.Content))
}

// joinEmails concatenates email addresses from a slice of mime.Address with spaces.
func joinEmails(addrs []mime.Address) string {
	if len(addrs) == 0 {
		return ""
	}
	emails := make([]string, 0, len(addrs))
	for _, a := range addrs {
		if a.Email != "" {
			emails = append(emails, a.Email)
		}
	}
	return strings.Join(emails, " ")
}

// deriveThreadKey extracts a thread identifier from parsed MIME
// headers. Returns the thread root Message-ID from References
// (first entry per RFC 2822), falls back to InReplyTo for simple
// replies, then to the message's own Message-ID. Returns "" when
// no threading info is available.
//
// InReplyTo is parsed as a msg-id list per RFC 2822 (it may
// contain multiple IDs and comments); only the first valid ID
// is used. Angle brackets are stripped for consistency with
// parseReferences.
func deriveThreadKey(parsed *mime.Message) string {
	if len(parsed.References) > 0 {
		return parsed.References[0]
	}
	if parsed.InReplyTo != "" {
		if ids := parseMsgIDList(parsed.InReplyTo); len(ids) > 0 {
			return ids[0]
		}
	}
	if parsed.MessageID != "" {
		if ids := parseMsgIDList(parsed.MessageID); len(ids) > 0 {
			return ids[0]
		}
	}
	return ""
}

// parseMsgIDList extracts angle-bracketed message-IDs from a
// header value (In-Reply-To, Message-ID, References). Only
// content inside < > pairs is returned, with brackets stripped.
// Comments, CFWS, and bare tokens are ignored per RFC 2822
// msg-id syntax.
func parseMsgIDList(s string) []string {
	var result []string
	for {
		open := strings.IndexByte(s, '<')
		if open < 0 {
			break
		}
		closeIdx := strings.IndexByte(s[open+1:], '>')
		if closeIdx < 0 {
			break
		}
		id := s[open+1 : open+1+closeIdx]
		if id != "" {
			result = append(result, id)
		}
		s = s[open+1+closeIdx+1:]
	}
	return result
}

// extractSubjectFromSnippet attempts to extract a subject from the message snippet.
// Used as fallback when MIME parsing fails.
func extractSubjectFromSnippet(snippet string) string {
	if snippet == "" {
		return "(MIME parse error)"
	}
	// Use first line of snippet, truncated
	line := textutil.FirstLine(snippet)
	return textutil.TruncateRunes(line, 80)
}
