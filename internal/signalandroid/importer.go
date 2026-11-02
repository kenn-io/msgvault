package signalandroid

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"go.kenn.io/msgvault/internal/export"
	"go.kenn.io/msgvault/internal/store"
)

const reactionTypeEmoji = "emoji"

// Importer imports a decrypted Signal Android database into the store.
type Importer struct {
	store    *store.Store
	progress ImportProgress
}

// NewImporter creates a Signal importer.
func NewImporter(s *store.Store, progress ImportProgress) *Importer {
	if progress == nil {
		progress = NullProgress{}
	}
	return &Importer{store: s, progress: progress}
}

// run holds the state of one import.
type run struct {
	imp        *Importer
	db         *sql.DB
	schema     *schema
	opts       ImportOptions
	summary    *ImportSummary
	sourceID   int64
	syncID     int64
	recipients map[int64]sigRecipient
	groups     map[string]string
	files      map[int64]string

	selfRecipientID int64
	selfToken       string
	selfParticipant int64

	participants map[int64]int64
	// seenIDs maps each message ID written in this run to its Signal row, so
	// a second row claiming the same author and timestamp is reported
	// instead of overwriting the first.
	seenIDs map[string]int64
	written int64
}

// Import reads the Signal database at dbPath and writes its chats, messages,
// reactions and attachments under a "signal" source for opts.Phone.
// Re-running it with a newer backup updates existing messages in place and
// adds new ones.
func (imp *Importer) Import(ctx context.Context, dbPath string, opts ImportOptions) (*ImportSummary, error) {
	startTime := time.Now()
	phone := normalizeE164(opts.Phone)
	if phone == "" {
		return nil, errors.New("phone number must be in E.164 format (starting with +)")
	}
	opts.Phone = phone
	if opts.BatchSize <= 0 {
		opts.BatchSize = DefaultOptions().BatchSize
	}
	if opts.MaxAttachmentSize <= 0 {
		opts.MaxAttachmentSize = DefaultOptions().MaxAttachmentSize
	}

	db, err := openReadOnlyDatabase(ctx, dbPath)
	if err != nil {
		return nil, fmt.Errorf("open signal database: %w", err)
	}
	defer func() { _ = db.Close() }()

	sch, err := detectSchema(ctx, db)
	if err != nil {
		return nil, err
	}
	r := &run{
		imp: imp, db: db, schema: sch, opts: opts,
		summary:      &ImportSummary{},
		participants: make(map[int64]int64),
		seenIDs:      make(map[string]int64),
	}
	if r.recipients, err = fetchRecipients(ctx, db, sch); err != nil {
		return nil, err
	}
	if r.groups, err = fetchGroupTitles(ctx, db, sch); err != nil {
		return nil, err
	}
	if err := r.resolveSelf(ctx); err != nil {
		return nil, err
	}
	if opts.AttachmentFilesDir != "" && opts.AttachmentsDir != "" {
		if r.files, err = scanAttachmentFiles(opts.AttachmentFilesDir); err != nil {
			return nil, err
		}
	}

	source, err := imp.store.GetOrCreateSource(SourceType, phone)
	if err != nil {
		return nil, fmt.Errorf("get or create source: %w", err)
	}
	if opts.DisplayName != "" {
		_ = imp.store.UpdateSourceDisplayName(source.ID, opts.DisplayName)
	}
	r.sourceID = source.ID
	r.summary.SourceID = source.ID

	syncID, err := imp.store.StartSync(source.ID, "signal_import")
	if err != nil {
		return nil, fmt.Errorf("start sync: %w", err)
	}
	r.syncID = syncID
	scoped := *imp
	scoped.store = imp.store.ScopedToSync(source.ID, syncID)
	r.imp = &scoped

	var syncErr error
	defer func() {
		if syncErr != nil {
			_ = r.imp.store.FailSync(syncID, syncErr.Error())
		} else {
			_ = r.imp.store.CompleteSync(syncID, "")
		}
	}()

	imp.progress.OnStart()
	if syncErr = r.ensureSelf(ctx); syncErr != nil {
		return nil, syncErr
	}

	threads, err := fetchThreads(ctx, db, sch)
	if err != nil {
		syncErr = err
		return nil, err
	}
	for _, thread := range threads {
		if r.limitReached() {
			break
		}
		if err := ctx.Err(); err != nil {
			syncErr = err
			return nil, err
		}
		if err := r.importThread(ctx, thread); err != nil {
			syncErr = err
			return nil, err
		}
	}

	if err := r.imp.store.RecomputeConversationStats(source.ID); err != nil {
		imp.progress.OnError(fmt.Errorf("recompute conversation stats: %w", err))
	}
	r.summary.Duration = time.Since(startTime)
	imp.progress.OnComplete(r.summary)
	return r.summary, nil
}

// resolveSelf finds the account owner's recipient row: the author of the
// outgoing messages, or the recipient with the owner's number when the
// database holds no outgoing message. A backup of another number is refused.
func (r *run) resolveSelf(ctx context.Context) error {
	authors, err := selfRecipientIDs(ctx, r.db)
	if err != nil {
		return err
	}
	r.selfRecipientID = -1
	for _, id := range authors {
		if r.recipients[id].E164 == r.opts.Phone {
			r.selfRecipientID = id
			break
		}
	}
	if r.selfRecipientID < 0 && len(authors) > 0 {
		r.selfRecipientID = authors[0]
	}
	if r.selfRecipientID < 0 {
		for id, recipient := range r.recipients {
			if recipient.E164 == r.opts.Phone && recipient.GroupID == "" {
				r.selfRecipientID = id
				break
			}
		}
	}
	self := r.recipients[r.selfRecipientID]
	if self.E164 != "" && self.E164 != r.opts.Phone {
		return errors.New("--phone does not match the number of the account that made this Signal backup")
	}
	r.selfToken = self.ACI
	if r.selfToken == "" {
		r.selfToken = r.opts.Phone
	}
	return nil
}

func (r *run) ensureSelf(ctx context.Context) error {
	pid, err := r.imp.store.EnsureParticipantByPhone(r.opts.Phone, r.opts.DisplayName, SourceType)
	if err != nil {
		return fmt.Errorf("ensure self participant: %w", err)
	}
	if aci := r.recipients[r.selfRecipientID].ACI; aci != "" {
		if err := r.imp.store.SetParticipantIdentifierContext(ctx, pid, ACIIdentifierType, aci); err != nil {
			return fmt.Errorf("record own Signal identity: %w", err)
		}
	}
	r.selfParticipant = pid
	r.participants[r.selfRecipientID] = pid
	r.summary.Participants++
	return nil
}

// participant returns the participant of a Signal recipient, creating it on
// first use. Contacts with a known number are matched by number across all
// sources; contacts Signal knows only by ACI get an ACI identifier. ok is
// false for recipients with neither.
func (r *run) participant(ctx context.Context, recipientID int64) (int64, bool) {
	if pid, ok := r.participants[recipientID]; ok {
		return pid, pid != 0
	}
	recipient, found := r.recipients[recipientID]
	var pid int64
	var err error
	switch {
	case !found || recipient.GroupID != "":
	case recipient.E164 != "":
		pid, err = r.imp.store.EnsureParticipantByPhone(recipient.E164, recipient.Name, SourceType)
		if err == nil && recipient.ACI != "" {
			err = r.imp.store.SetParticipantIdentifierContext(ctx, pid, ACIIdentifierType, recipient.ACI)
		}
	case recipient.ACI != "":
		pid, err = r.imp.store.EnsureParticipantByIdentifierContext(ctx, ACIIdentifierType, recipient.ACI, recipient.Name)
	}
	if err != nil {
		r.summary.Errors++
		r.imp.progress.OnError(fmt.Errorf("ensure participant for Signal recipient %d: %w", recipientID, err))
		pid = 0
	} else if pid != 0 {
		r.summary.Participants++
	}
	r.participants[recipientID] = pid
	return pid, pid != 0
}

// token returns the identity token of a recipient for message IDs.
func (r *run) token(recipientID int64) string {
	if recipientID == r.selfRecipientID {
		return r.selfToken
	}
	return identityToken(r.recipients[recipientID])
}

func (r *run) displayName(recipientID int64) string {
	recipient := r.recipients[recipientID]
	switch {
	case recipient.Name != "":
		return recipient.Name
	case recipient.E164 != "":
		return recipient.E164
	case recipientID == r.selfRecipientID && r.opts.DisplayName != "":
		return r.opts.DisplayName
	}
	return "Signal user"
}

func (r *run) limitReached() bool {
	return r.opts.Limit > 0 && r.written >= int64(r.opts.Limit)
}

func (r *run) importThread(ctx context.Context, thread sigThread) error {
	chat := r.recipients[thread.RecipientID]
	sourceConvID, convType, ok := conversationIdentity(chat)
	if !ok {
		r.summary.ChatsSkipped++
		return nil
	}
	title := ""
	if convType == "group_chat" {
		title = r.groups[chat.GroupID]
	}
	r.summary.ChatsProcessed++
	conversationID, err := r.imp.store.EnsureConversationWithTypeContext(ctx, r.sourceID, sourceConvID, convType, title)
	if err != nil {
		r.summary.Errors++
		r.imp.progress.OnError(fmt.Errorf("ensure conversation %s: %w", sourceConvID, err))
		return nil
	}
	progressTitle := title
	if progressTitle == "" {
		progressTitle = r.displayName(thread.RecipientID)
	}
	r.imp.progress.OnChatStart(sourceConvID, progressTitle, 0)

	members := []int64{r.selfRecipientID}
	if convType == "group_chat" {
		groupMembers, err := fetchGroupMembers(ctx, r.db, r.schema, chat.GroupID)
		if err != nil {
			r.summary.Errors++
			r.imp.progress.OnError(fmt.Errorf("group members of %s: %w", sourceConvID, err))
		}
		members = append(members, groupMembers...)
	} else {
		members = append(members, thread.RecipientID)
	}
	for _, member := range members {
		if pid, ok := r.participant(ctx, member); ok {
			_ = r.imp.store.EnsureConversationParticipantContext(ctx, conversationID, pid, "member")
		}
	}

	var chatAdded int64
	senders := make(map[int64]struct{})
	afterRowID := int64(0)
	for !r.limitReached() {
		if err := ctx.Err(); err != nil {
			return err
		}
		messages, err := fetchMessages(ctx, r.db, r.schema, thread.ID, afterRowID, r.opts.BatchSize)
		if err != nil {
			r.summary.Errors++
			r.imp.progress.OnError(fmt.Errorf("read messages of %s: %w", sourceConvID, err))
			break
		}
		if len(messages) == 0 {
			break
		}
		afterRowID = messages[len(messages)-1].RowID
		added, err := r.importBatch(ctx, conversationID, messages, senders)
		if err != nil {
			return err
		}
		chatAdded += added
		_ = r.imp.store.UpdateSyncCheckpoint(r.syncID, &store.Checkpoint{
			MessagesProcessed: r.summary.MessagesProcessed,
			MessagesAdded:     r.summary.MessagesAdded,
		})
		r.imp.progress.OnProgress(r.summary.MessagesProcessed, r.summary.MessagesAdded, r.summary.MessagesSkipped)
		if len(messages) < r.opts.BatchSize {
			break
		}
	}
	for pid := range senders {
		_ = r.imp.store.EnsureConversationParticipantContext(ctx, conversationID, pid, "member")
	}
	r.imp.progress.OnChatComplete(sourceConvID, chatAdded)
	return nil
}

// pendingMessage is a message row that passed the filters, with its ID.
type pendingMessage struct {
	row      sigMessage
	id       string
	rootID   int64
	senderID int64
}

func (r *run) importBatch(ctx context.Context, conversationID int64, messages []sigMessage, senders map[int64]struct{}) (int64, error) {
	var pending []pendingMessage
	var ids []string
	var lookupRows []int64
	for _, m := range messages {
		r.summary.MessagesProcessed++
		if skipReason(m) != "" {
			r.summary.MessagesSkipped++
			continue
		}
		author := m.AuthorID
		if isOutgoing(m.Type) {
			author = r.selfRecipientID
		}
		token := r.token(author)
		if token == "" || m.DateSent <= 0 {
			r.summary.MessagesSkipped++
			continue
		}
		rootID := m.RowID
		if m.OriginalRowID.Valid {
			rootID = m.OriginalRowID.Int64
		}
		id := messageID(token, m.DateSent)
		if previous, seen := r.seenIDs[id]; seen && previous != rootID {
			r.summary.DuplicateIDs++
			r.summary.MessagesSkipped++
			continue
		}
		r.seenIDs[id] = rootID
		pending = append(pending, pendingMessage{row: m, id: id, rootID: rootID, senderID: author})
		ids = append(ids, id)
		lookupRows = append(lookupRows, m.RowID)
		if m.OriginalRowID.Valid {
			lookupRows = append(lookupRows, m.OriginalRowID.Int64)
		}
	}
	if len(pending) == 0 {
		return 0, nil
	}

	existing, err := r.imp.store.ArchivedSourceMessageIDs(r.sourceID, ids)
	if err != nil {
		return 0, fmt.Errorf("look up archived Signal messages: %w", err)
	}
	attachments, err := fetchAttachments(ctx, r.db, r.schema, lookupRows)
	if err != nil {
		r.summary.Errors++
		r.imp.progress.OnError(err)
		attachments = map[int64][]sigAttachment{}
	}
	roots := make([]int64, len(pending))
	visible := make([]int64, len(pending))
	for i, p := range pending {
		roots[i] = p.rootID
		visible[i] = p.row.RowID
	}
	reactions, err := fetchReactions(ctx, r.db, r.schema, roots)
	if err != nil {
		r.summary.Errors++
		r.imp.progress.OnError(err)
		reactions = map[int64][]sigReaction{}
	}
	mentions, err := fetchMentions(ctx, r.db, r.schema, visible)
	if err != nil {
		r.summary.Errors++
		r.imp.progress.OnError(err)
		mentions = map[int64][]sigMention{}
	}

	var added int64
	for _, p := range pending {
		if r.limitReached() {
			break
		}
		// Edits copy the attachments to the newest revision; fall back to the
		// original row for databases that kept them there.
		rowAttachments := attachments[p.row.RowID]
		if len(rowAttachments) == 0 && p.row.OriginalRowID.Valid {
			rowAttachments = attachments[p.row.OriginalRowID.Int64]
		}
		_, isExisting := existing[p.id]
		if err := r.writeMessage(ctx, conversationID, p, rowAttachments,
			reactions[p.rootID], mentions[p.row.RowID]); err != nil {
			if ctx.Err() != nil {
				return added, ctx.Err()
			}
			r.summary.Errors++
			r.imp.progress.OnError(fmt.Errorf("write Signal message %s: %w", p.id, err))
			continue
		}
		r.written++
		if isExisting {
			r.summary.MessagesUpdated++
		} else {
			r.summary.MessagesAdded++
			added++
		}
		if pid, ok := r.participants[p.senderID]; ok && pid != 0 {
			senders[pid] = struct{}{}
		}
	}
	return added, nil
}

func (r *run) writeMessage(
	ctx context.Context, conversationID int64, p pendingMessage,
	attachments []sigAttachment, reactions []sigReaction, mentions []sigMention,
) error {
	m := p.row
	st := r.imp.store
	body := ""
	if m.Body.Valid {
		body = applyMentions(m.Body.String, mentions, r.displayName)
	}
	for _, a := range attachments {
		if a.Caption != "" && a.Caption != body {
			if body != "" {
				body += "\n\n"
			}
			body += a.Caption
		}
	}

	var senderID sql.NullInt64
	if pid, ok := r.participant(ctx, p.senderID); ok {
		senderID = sql.NullInt64{Int64: pid, Valid: true}
	}
	msg := store.Message{
		ConversationID:          conversationID,
		SourceID:                r.sourceID,
		SourceMessageID:         p.id,
		MessageType:             MessageType,
		SentAt:                  millisTime(m.DateSent),
		ReceivedAt:              millisTime(m.DateReceived),
		SenderID:                senderID,
		IsFromMe:                isOutgoing(m.Type),
		Snippet:                 sql.NullString{String: snippet(body), Valid: body != ""},
		SizeEstimate:            int64(len(body)),
		PreserveAttachmentStats: true,
		ArchivedAt:              time.Now(),
	}
	messageID, err := st.UpsertMessageContext(ctx, &msg)
	if err != nil {
		return err
	}
	if body != "" {
		if err := st.UpsertMessageBodyContext(ctx, messageID, sql.NullString{String: body, Valid: true}, sql.NullString{}); err != nil {
			return fmt.Errorf("store body: %w", err)
		}
	}
	if raw, err := json.Marshal(m, json.Deterministic(true)); err == nil {
		_ = st.UpsertMessageRawWithFormatContext(ctx, messageID, raw, rawFormat)
	}
	if m.OriginalRowID.Valid {
		r.summary.MessagesEdited++
		if err := st.SetMessageEditedStateContext(ctx, messageID, true); err != nil {
			return fmt.Errorf("mark edited: %w", err)
		}
	}

	if len(attachments) > 0 {
		if err := r.writeAttachments(ctx, messageID, attachments); err != nil {
			return err
		}
	}
	r.linkQuote(ctx, p.id, m)
	for _, reaction := range reactions {
		pid, ok := r.participant(ctx, reaction.AuthorID)
		if !ok || reaction.Emoji == "" {
			continue
		}
		created := millisTime(reaction.DateSent).Time
		if err := st.UpsertReaction(messageID, pid, reactionTypeEmoji, reaction.Emoji, created); err != nil {
			r.summary.Errors++
			r.imp.progress.OnError(fmt.Errorf("store reaction: %w", err))
			continue
		}
		r.summary.ReactionsAdded++
	}
	if body != "" {
		senderAddr := r.recipients[p.senderID].E164
		if p.senderID == r.selfRecipientID {
			senderAddr = r.opts.Phone
		}
		if err := st.UpsertFTSContext(ctx, messageID, "", body, senderAddr, "", ""); err != nil {
			r.summary.Errors++
			r.imp.progress.OnError(fmt.Errorf("index message: %w", err))
		}
	}
	return nil
}

// linkQuote points a reply at the message it quotes. Signal records a quote
// as the quoted message's author and sent timestamp, which is that message's
// ID here.
func (r *run) linkQuote(ctx context.Context, childID string, m sigMessage) {
	if m.QuoteID <= 0 || m.QuoteAuthor <= 0 {
		return
	}
	token := r.token(m.QuoteAuthor)
	if token == "" {
		r.summary.RepliesUnresolved++
		return
	}
	parentID := messageID(token, m.QuoteID)
	found, err := r.imp.store.ArchivedSourceMessageIDs(r.sourceID, []string{parentID})
	if err != nil || len(found) == 0 {
		r.summary.RepliesUnresolved++
		return
	}
	if err := r.imp.store.SetReplyToContext(ctx, r.sourceID, childID, parentID); err != nil {
		r.summary.Errors++
		r.imp.progress.OnError(fmt.Errorf("link reply: %w", err))
		return
	}
	r.summary.RepliesLinked++
}

// writeAttachments stores the attachment files found next to the database.
// An attachment without a file (a backup unpacked without media, or a
// download that never finished) is counted as missing; an earlier import's
// stored copy stays in place.
func (r *run) writeAttachments(ctx context.Context, messageID int64, attachments []sigAttachment) error {
	stored := 0
	for i, a := range attachments {
		r.summary.AttachmentsFound++
		name, ok := r.files[a.ID]
		if !ok {
			r.summary.AttachmentsMissing++
			continue
		}
		storagePath, contentHash, size, err := export.StoreAttachmentFromPath(
			r.opts.AttachmentsDir, filepath.Join(r.opts.AttachmentFilesDir, name), r.opts.MaxAttachmentSize)
		if err != nil || storagePath == "" {
			r.summary.AttachmentsMissing++
			continue
		}
		mediaType := attachmentMediaType(a)
		role, roleSource := store.AttachmentRoleStandalone, store.AttachmentRoleSourceImporterSemantics
		if mediaType == "sticker" {
			role, roleSource = store.AttachmentRoleSticker, store.AttachmentRoleSourceProviderExplicit
		}
		if err := r.imp.store.UpsertAttachmentRecord(ctx, messageID, store.AttachmentWrite{
			Filename: a.FileName, MIMEType: a.ContentType, StoragePath: storagePath,
			ContentHash: contentHash, Size: size, MediaType: mediaType,
			Width: a.Width, Height: a.Height,
			Role: role, RoleSource: roleSource,
			SourcePartKey: fmt.Sprintf("signal:attachment:%d", i),
		}); err != nil {
			return fmt.Errorf("store attachment: %w", err)
		}
		stored++
		r.summary.AttachmentsStored++
	}
	if stored > 0 {
		if err := r.imp.store.RecomputeMessageAttachmentStatsContext(ctx, messageID); err != nil {
			return fmt.Errorf("recompute attachment stats: %w", err)
		}
	}
	return nil
}

func millisTime(ms int64) sql.NullTime {
	if ms <= 0 {
		return sql.NullTime{}
	}
	return sql.NullTime{Time: time.UnixMilli(ms).UTC(), Valid: true}
}
