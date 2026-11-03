package signalandroid

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"go.kenn.io/msgvault/internal/export"
	"go.kenn.io/msgvault/internal/sqliteutil"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/textutil"
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

	participants    map[int64]int64
	participantErrs map[int64]error
	// seenIDs maps each message ID written in this run to its Signal row, so
	// a second row claiming the same chat, author, and timestamp is reported
	// instead of overwriting the first.
	seenIDs map[string]int64
	quotes  []pendingQuote
	written int64
}

// Import reads the Signal database at dbPath and writes its chats, messages,
// reactions and attachments under a "signal" source for opts.Phone.
// Re-running it with a newer backup updates existing messages in place and
// adds new ones.
func (imp *Importer) Import(ctx context.Context, dbPath string, opts ImportOptions) (*ImportSummary, error) {
	startTime := time.Now()
	phone := NormalizePhone(opts.Phone)
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

	db, err := sqliteutil.OpenReadOnlyDatabase(ctx, dbPath)
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
		summary:         &ImportSummary{},
		participants:    make(map[int64]int64),
		participantErrs: make(map[int64]error),
		seenIDs:         make(map[string]int64),
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
	if opts.AttachmentFilesDir != "" {
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
		// Refresh committed work even when the import was interrupted.
		if err := r.imp.store.RecomputeConversationStatsContext(context.WithoutCancel(ctx), source.ID); err != nil {
			imp.progress.OnError(fmt.Errorf("recompute conversation stats: %w", err))
		}
		r.checkpoint()
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

	if err := r.linkQuotes(ctx); err != nil {
		syncErr = err
		return nil, err
	}
	r.summary.Duration = time.Since(startTime)
	imp.progress.OnComplete(r.summary)
	return r.summary, nil
}

// resolveSelf finds the account owner's recipient row from the backup
// itself: the author of outgoing messages and the recipient of incoming
// direct messages. --phone must match that row's number. A backup whose
// owner cannot be determined, or whose owner has another number, is refused
// before anything is written.
func (r *run) resolveSelf(ctx context.Context) error {
	evidence, err := ownerEvidence(ctx, r.db)
	if err != nil {
		return err
	}
	r.selfRecipientID = -1
	var best int64
	for id, count := range evidence {
		recipient, found := r.recipients[id]
		if !found || recipient.GroupID != "" {
			continue
		}
		if count > best || count == best && id < r.selfRecipientID {
			r.selfRecipientID, best = id, count
		}
	}
	if r.selfRecipientID < 0 {
		return errors.New("cannot determine which account made this Signal backup: " +
			"it holds no sent messages and no received direct messages")
	}
	self := r.recipients[r.selfRecipientID]
	if self.E164 == "" {
		return errors.New("cannot verify --phone: the backup does not record its own account's number")
	}
	if self.E164 != r.opts.Phone {
		return errors.New("--phone does not match the number of the account that made this Signal backup")
	}
	r.selfToken = self.ACI
	if r.selfToken == "" {
		return fmt.Errorf("%w: the account owner has no Signal account ID", ErrUnsupportedDatabase)
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
			return err
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
// false for recipients with neither. err reports a store failure: the caller
// must then leave archived attribution unchanged rather than write a message
// or reaction set without this participant.
func (r *run) participant(ctx context.Context, recipientID int64) (pid int64, ok bool, err error) {
	if pid, cached := r.participants[recipientID]; cached {
		return pid, pid != 0, r.participantErrs[recipientID]
	}
	recipient, found := r.recipients[recipientID]
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
		err = fmt.Errorf("ensure participant for Signal recipient %d: %w", recipientID, err)
		r.imp.progress.OnError(err)
		pid = 0
		r.participantErrs[recipientID] = err
	} else if pid != 0 {
		r.summary.Participants++
	}
	r.participants[recipientID] = pid
	return pid, pid != 0, err
}

// token returns the identity token of a recipient for message IDs.
func (r *run) token(recipientID int64) string {
	if recipientID == r.selfRecipientID {
		return r.selfToken
	}
	return r.recipients[recipientID].ACI
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

// checkpoint records the run's counters on its sync run.
func (r *run) checkpoint() {
	_ = r.imp.store.UpdateSyncCheckpoint(r.syncID, &store.Checkpoint{
		MessagesProcessed: r.summary.MessagesProcessed,
		MessagesAdded:     r.summary.MessagesAdded,
		MessagesUpdated:   r.summary.MessagesUpdated,
		ErrorsCount:       r.summary.Errors,
	})
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
		if pid, ok, _ := r.participant(ctx, member); ok {
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
		added, err := r.importBatch(ctx, conversationID, sourceConvID, messages, senders)
		if err != nil {
			return err
		}
		chatAdded += added
		r.checkpoint()
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
	chatID   string
	id       string
	rootID   int64
	senderID int64
}

// pendingQuote is a reply whose quoted message is linked after every thread
// is written, so the quoted message's newest revision no longer has to
// precede the reply.
type pendingQuote struct {
	childID  string
	parentID string
}

func (r *run) importBatch(ctx context.Context, conversationID int64, sourceConvID string, messages []sigMessage, senders map[int64]struct{}) (int64, error) {
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
		id := messageID(sourceConvID, token, m.DateSent)
		if previous, seen := r.seenIDs[id]; seen && previous != rootID {
			r.summary.DuplicateIDs++
			r.summary.MessagesSkipped++
			continue
		}
		r.seenIDs[id] = rootID
		pending = append(pending, pendingMessage{row: m, chatID: sourceConvID, id: id, rootID: rootID, senderID: author})
		ids = append(ids, id)
		lookupRows = append(lookupRows, m.RowID)
		if m.OriginalRowID.Valid {
			lookupRows = append(lookupRows, m.OriginalRowID.Int64)
		}
	}
	if len(pending) == 0 {
		return 0, nil
	}

	existing, err := r.imp.store.MessageExistsBatchContext(ctx, r.sourceID, ids)
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
	reactionsRead := err == nil
	if err != nil {
		// Keep the archived reactions rather than replacing them with an
		// empty snapshot the backup did not actually contain.
		r.summary.Errors++
		r.imp.progress.OnError(err)
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
		archivedID, isExisting := existing[p.id]
		var rowReactions []sigReaction
		if reactionsRead {
			rowReactions = reactions[p.rootID]
		}
		if err := r.writeMessage(ctx, conversationID, archivedID, p, rowAttachments,
			reactionsRead, rowReactions, mentions[p.row.RowID]); err != nil {
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
	ctx context.Context, conversationID, archivedID int64, p pendingMessage,
	attachments []sigAttachment, reactionsRead bool, reactions []sigReaction, mentions []sigMention,
) error {
	m := p.row
	st := r.imp.store
	body := ""
	if m.Body.Valid {
		body = applyMentions(m.Body.String, mentions, r.displayName)
	}
	for _, a := range attachments {
		if a.ContentType == "text/x-signal-plain" {
			if name, ok := r.files[a.ID]; ok {
				full, err := os.ReadFile(filepath.Join(r.opts.AttachmentFilesDir, name))
				if err != nil {
					return fmt.Errorf("read long text: %w", err)
				}
				body = applyMentions(string(full), mentions, r.displayName)
			}
		}
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
	pid, ok, err := r.participant(ctx, p.senderID)
	if err != nil {
		// Writing now would clear the archived sender; leave the message as is.
		return err
	}
	if ok {
		senderID = sql.NullInt64{Int64: pid, Valid: true}
	}
	msg := store.Message{
		ConversationID:  conversationID,
		SourceID:        r.sourceID,
		SourceMessageID: p.id,
		MessageType:     MessageType,
		SentAt:          millisTime(m.DateSent),
		ReceivedAt:      millisTime(m.DateReceived),
		SenderID:        senderID,
		IsFromMe:        isOutgoing(m.Type),
		Snippet:         sql.NullString{String: textutil.PrefixRunes(body, snippetRunes), Valid: body != ""},
		// Signal marks a message sent only once the server accepted it;
		// outbox, sending and failed messages did not leave the phone.
		IsSent:                  sql.NullBool{Bool: isSent(m.Type), Valid: true},
		SizeEstimate:            int64(len(body)),
		PreserveAttachmentStats: true,
		ArchivedAt:              time.Now(),
	}
	raw, err := json.Marshal(m, json.Deterministic(true))
	if err != nil {
		return err
	}
	writes, err := r.prepareAttachments(ctx, archivedID, attachments)
	if err != nil {
		return err
	}
	senderAddr := r.recipients[p.senderID].E164
	if p.senderID == r.selfRecipientID {
		senderAddr = r.opts.Phone
	}
	data := store.MessagePersistData{
		Message: &msg, BodyText: sql.NullString{String: body, Valid: body != ""},
		RawMIME: raw, RawFormat: rawFormat, Edited: m.OriginalRowID.Valid,
		MIMEAttachmentReplacement: writes,
		FTS:                       &store.FTSDoc{Body: body, FromAddr: senderAddr},
	}
	if reactionsRead {
		// The backup holds each message's current reactions; replace the
		// archived set so removed and changed reactions disappear.
		refs := make([]store.ReactionRef, 0, len(reactions))
		complete := true
		for _, reaction := range reactions {
			pid, ok, err := r.participant(ctx, reaction.AuthorID)
			if err != nil {
				complete = false
				break
			}
			if !ok || reaction.Emoji == "" {
				continue
			}
			refs = append(refs, store.ReactionRef{
				ParticipantID: pid, Type: reactionTypeEmoji, Value: reaction.Emoji,
				CreatedAt: millisTime(reaction.DateSent).Time,
			})
		}
		if complete {
			data.Reactions = &refs
		}
	}
	_, err = st.PersistMessageContext(ctx, &data)
	if err != nil {
		return err
	}
	if data.Edited {
		r.summary.MessagesEdited++
	}
	if data.Reactions != nil {
		r.summary.ReactionsAdded += int64(len(*data.Reactions))
	}
	if writes != nil {
		r.summary.AttachmentsStored += int64(len(*writes))
	}
	r.queueQuote(ctx, p.chatID, p.id, m)
	return nil
}

// queueQuote records a reply's quoted message for linkQuotes. Signal records
// a quote as the quoted message's author and sent timestamp, which is that
// message's ID here.
func (r *run) queueQuote(ctx context.Context, chatID, childID string, m sigMessage) {
	if m.QuoteID <= 0 || m.QuoteAuthor <= 0 {
		return
	}
	token := r.token(m.QuoteAuthor)
	if token == "" {
		r.summary.RepliesUnresolved++
		return
	}
	// Quotes can name any revision. Resolve its original timestamp in this chat.
	timestamp := m.QuoteID
	var original int64
	err := r.db.QueryRowContext(ctx, `SELECT COALESCE(o.date_sent, m.date_sent)
  FROM message m LEFT JOIN message o ON o._id = m.original_message_id
  WHERE m.thread_id = ? AND m.from_recipient_id = ? AND m.date_sent = ? LIMIT 1`, m.ThreadID, m.QuoteAuthor, m.QuoteID).Scan(&original)
	if err == nil {
		timestamp = original
	} else if !errors.Is(err, sql.ErrNoRows) {
		r.summary.Errors++
		r.imp.progress.OnError(fmt.Errorf("resolve quoted revision: %w", err))
		return
	}
	q := pendingQuote{childID: childID, parentID: messageID(chatID, token, timestamp)}
	r.quotes = append(r.quotes, q)
}

// linkQuotes points each queued reply at the message it quotes, once all
// threads are written.
func (r *run) linkQuotes(ctx context.Context) error {
	for _, q := range r.quotes {
		if err := ctx.Err(); err != nil {
			return err
		}
		found, err := r.imp.store.ArchivedSourceMessageIDs(r.sourceID, []string{q.parentID})
		if err != nil {
			r.summary.Errors++
			r.imp.progress.OnError(fmt.Errorf("look up quoted message: %w", err))
			continue
		}
		if _, ok := found[q.parentID]; !ok {
			r.summary.RepliesUnresolved++
			continue
		}
		if err := r.imp.store.SetReplyToContext(ctx, r.sourceID, q.childID, q.parentID); err != nil {
			r.summary.Errors++
			r.imp.progress.OnError(fmt.Errorf("link reply: %w", err))
			continue
		}
		r.summary.RepliesLinked++
	}
	return nil
}

// prepareAttachments builds the snapshot before committing the message. Missing
// files retain the archived attachment at the same source part key.
func (r *run) prepareAttachments(ctx context.Context, archivedID int64, attachments []sigAttachment) (*[]store.AttachmentWrite, error) {
	var writes []store.AttachmentWrite
	if archivedID != 0 {
		refs, err := r.imp.store.MessageMIMEAttachmentsContext(ctx, archivedID)
		if err != nil {
			return nil, err
		}
		for _, ref := range refs {
			writes = append(writes, store.AttachmentWrite{Filename: ref.Filename, MIMEType: ref.MimeType,
				StoragePath: ref.StoragePath, ContentHash: ref.ContentHash, Size: int64(ref.Size),
				MediaType: ref.MediaType, Width: ref.Width, Height: ref.Height,
				Role: ref.Role, RoleSource: ref.RoleSource, SourcePartKey: ref.SourcePartKey})
		}
	}
	for i, a := range attachments {
		if a.ContentType == "text/x-signal-plain" {
			continue
		}
		r.summary.AttachmentsFound++
		name, ok := r.files[a.ID]
		if !ok || r.opts.AttachmentsDir == "" {
			r.summary.AttachmentsMissing++
			continue
		}
		storagePath, contentHash, size, err := export.StoreAttachmentFromPath(
			r.opts.AttachmentsDir, filepath.Join(r.opts.AttachmentFilesDir, name), r.opts.MaxAttachmentSize)
		if err != nil {
			return nil, fmt.Errorf("store attachment %d: %w", a.ID, err)
		}
		if storagePath == "" {
			continue
		}
		mediaType := attachmentMediaType(a)
		role, roleSource := store.AttachmentRoleStandalone, store.AttachmentRoleSourceImporterSemantics
		if mediaType == "sticker" {
			role, roleSource = store.AttachmentRoleSticker, store.AttachmentRoleSourceProviderExplicit
		}
		write := store.AttachmentWrite{Filename: a.FileName, MIMEType: a.ContentType, StoragePath: storagePath,
			ContentHash: contentHash, Size: size, MediaType: mediaType, Width: a.Width, Height: a.Height,
			Role: role, RoleSource: roleSource, SourcePartKey: fmt.Sprintf("signal:attachment:%d", i)}
		replaced := false
		for j := range writes {
			if writes[j].SourcePartKey == write.SourcePartKey {
				writes[j] = write
				replaced = true
				break
			}
		}
		if !replaced {
			writes = append(writes, write)
		}
	}
	if len(writes) == 0 {
		return nil, nil
	}
	return &writes, nil
}

func millisTime(ms int64) sql.NullTime {
	if ms <= 0 {
		return sql.NullTime{}
	}
	return sql.NullTime{Time: time.UnixMilli(ms).UTC(), Valid: true}
}
