package signalandroid

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/attachmentstore"
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

	participants    map[int64]int64
	participantErrs map[int64]error
	pendingReplies  map[string]bool
	written         int64
}

// Import reads the Signal database at dbPath and writes its chats, messages,
// reactions and attachments under a "signal" source for opts.Phone.
// Re-running it with a newer backup updates existing messages in place and
// adds new ones.
func (imp *Importer) Import(ctx context.Context, dbPath string, opts ImportOptions) (summary *ImportSummary, retErr error) {
	startTime := time.Now()
	if opts.Limit < 0 {
		return nil, errors.New("limit must be non-negative")
	}
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

	sch, err := detectSchema(ctx, db, dbPath)
	if err != nil {
		return nil, err
	}
	r := &run{
		imp: imp, db: db, schema: sch, opts: opts,
		summary:         &ImportSummary{},
		participants:    make(map[int64]int64),
		participantErrs: make(map[int64]error),
		pendingReplies:  make(map[string]bool),
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
		if r.files, err = r.scanAttachmentFiles(opts.AttachmentFilesDir); err != nil {
			return nil, err
		}
	}

	source, err := r.source(ctx)
	if err != nil {
		return nil, fmt.Errorf("get or create source: %w", err)
	}
	r.sourceID = source.ID
	r.summary.SourceID = source.ID
	conversationIDs, err := imp.store.ListSourceConversationIDsContext(ctx, source.ID)
	if err != nil {
		return r.summary, err
	}
	archived := make(map[string]bool, len(conversationIDs))
	for _, id := range conversationIDs {
		archived[id] = true
	}
	if opts.DisplayName != "" {
		_ = imp.store.UpdateSourceDisplayName(source.ID, opts.DisplayName)
	}
	syncID, err := imp.store.StartSync(source.ID, "signal_import")
	if err != nil {
		return r.summary, fmt.Errorf("start sync: %w", err)
	}
	r.syncID = syncID
	scoped := *imp
	scoped.store = imp.store.ScopedToSync(source.ID, syncID)
	r.imp = &scoped

	defer func() {
		finalCtx := context.WithoutCancel(ctx)
		if err := r.resolveReplies(finalCtx); err != nil {
			retErr = errors.Join(retErr, err)
		}
		if err := r.imp.store.RecomputeConversationStatsContext(finalCtx, source.ID); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("recompute conversation stats: %w", err))
		}
		r.checkpoint()
		if retErr != nil {
			_ = r.imp.store.FailSync(syncID, retErr.Error())
		} else {
			retErr = r.imp.store.CompleteSync(syncID, "")
		}
		if retErr == nil {
			r.summary.Duration = time.Since(startTime)
			imp.progress.OnComplete(r.summary)
		}
	}()

	imp.progress.OnStart()
	if err := r.ensureSelf(ctx); err != nil {
		return r.summary, err
	}

	threads, err := fetchThreads(ctx, db, sch)
	if err != nil {
		return r.summary, err
	}
	for _, thread := range threads {
		if r.limitReached() {
			break
		}
		if err := ctx.Err(); err != nil {
			return r.summary, err
		}
		if err := r.importThread(ctx, thread, archived); err != nil {
			return r.summary, err
		}
	}

	return r.summary, nil
}

// source keeps an owner's archive when Signal changes the account's number.
func (r *run) source(ctx context.Context) (*store.Source, error) {
	if aci := r.recipients[r.selfRecipientID].ACI; aci != "" {
		owner, _, err := r.imp.store.ParticipantByIdentifierContext(ctx, ACIIdentifierType, aci)
		if err != nil {
			return nil, err
		}
		if owner != 0 {
			sources, err := r.imp.store.ListSourcesContext(ctx, SourceType)
			if err != nil {
				return nil, err
			}
			for _, source := range sources {
				pid, err := r.imp.store.PhoneParticipantContext(ctx, source.Identifier)
				if err != nil {
					return nil, err
				}
				if pid == owner {
					if source.Identifier != r.opts.Phone {
						if err := r.imp.store.UpdateSourceIdentifier(source.ID, r.opts.Phone); err != nil {
							return nil, err
						}
					}
					return source, nil
				}
			}
		}
	}
	return r.imp.store.GetOrCreateSource(SourceType, r.opts.Phone)
}

// resolveSelf finds the account owner's recipient row from the backup
// itself: the author of outgoing messages and the recipient of incoming
// direct messages. --phone must match that row's number. A backup whose
// owner cannot be determined, or whose owner has another number, is refused
// before anything is written. Without message evidence, --phone identifies
// the owner's recipient record.
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
		for id, recipient := range r.recipients {
			if recipient.GroupID == "" && recipient.E164 == r.opts.Phone {
				r.selfRecipientID = id
				break
			}
		}
		if r.selfRecipientID < 0 {
			return errors.New("cannot find your Signal account for --phone")
		}
	}
	self := r.recipients[r.selfRecipientID]
	if self.E164 == "" {
		return errors.New("cannot verify --phone: the backup does not record its own account's number")
	}
	if self.E164 != r.opts.Phone {
		return errors.New("--phone does not match the number of the account that made this Signal backup")
	}
	return nil
}

func (r *run) ensureSelf(ctx context.Context) error {
	pid, err := r.imp.store.EnsureParticipantByPhone(r.opts.Phone, r.opts.DisplayName, SourceType)
	if err != nil {
		return fmt.Errorf("ensure self participant: %w", err)
	}
	if aci := r.recipients[r.selfRecipientID].ACI; aci != "" {
		if err := r.bindACI(ctx, pid, aci); err != nil {
			return err
		}
	}
	r.participants[r.selfRecipientID] = pid
	r.summary.Participants++
	return nil
}

// bindACI merges earlier ACI-only history into the phone participant.
func (r *run) bindACI(ctx context.Context, phoneParticipant int64, aci string) error {
	existing, _, err := r.imp.store.ParticipantByIdentifierContext(ctx, ACIIdentifierType, aci)
	if err != nil {
		return fmt.Errorf("look up Signal identity: %w", err)
	}
	if existing != 0 && existing != phoneParticipant {
		if err := r.imp.store.MergeParticipantsContext(ctx, existing, phoneParticipant); err != nil {
			return fmt.Errorf("merge Signal contacts: %w", err)
		}
		for recipient, pid := range r.participants {
			if pid == existing {
				r.participants[recipient] = phoneParticipant
			}
		}
	}
	return r.imp.store.SetParticipantIdentifierContext(ctx, phoneParticipant, ACIIdentifierType, aci)
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
			identity, lookupErr := r.imp.store.GetParticipantIdentityContext(ctx, []int64{pid})
			if lookupErr != nil {
				err = lookupErr
				break
			}
			conflictingACI := false
			for _, identifier := range identity.Identifiers {
				if identifier.Type == ACIIdentifierType && identifier.Value != recipient.ACI {
					conflictingACI = true
					break
				}
			}
			if conflictingACI {
				// A recycled number must keep the two accounts' histories separate.
				pid, err = r.imp.store.EnsureParticipantByIdentifierContext(ctx, ACIIdentifierType, recipient.ACI, recipient.Name)
			} else {
				err = r.bindACI(ctx, pid, recipient.ACI)
			}
		}
	case recipient.ACI != "":
		pid, err = r.imp.store.EnsureParticipantByIdentifierContext(ctx, ACIIdentifierType, recipient.ACI, recipient.Name)
	}
	if err != nil {
		r.summary.Errors++
		err = fmt.Errorf("save Signal contact: %w", err)
		r.imp.progress.OnError(err)
		pid = 0
		r.participantErrs[recipientID] = err
	} else if pid != 0 {
		r.summary.Participants++
	}
	r.participants[recipientID] = pid
	return pid, pid != 0, err
}

// tokens returns a recipient's identity tokens, preferred first, followed by
// the PNI and phone tokens a message may have been archived under before the
// recipient's ACI was known.
func (r *run) tokens(recipientID int64, direct bool) []string {
	if recipientID == r.selfRecipientID {
		return []string{"self"}
	}
	if direct {
		return []string{"peer"}
	}
	return recipientIDs(r.recipients[recipientID], "", "pni:", "tel:")
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

func (r *run) importThread(ctx context.Context, thread sigThread, archived map[string]bool) error {
	chat, err := r.renumberedChat(ctx, r.recipients[thread.RecipientID], archived)
	if err != nil {
		return err
	}
	sourceConvID, convType, ok := conversationIdentity(chat, archived)
	if !ok {
		return r.skipThread(ctx, thread)
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
	membersRead := true
	if convType == "group_chat" {
		groupMembers, err := fetchGroupMembers(ctx, r.db, r.schema, chat.GroupID)
		if err != nil {
			membersRead = false
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
	if !membersRead {
		members = nil
	}

	var chatAdded int64
	conversation := store.ConversationPersistData{
		SourceConversationID: sourceConvID, ConversationType: convType, Title: title,
		PreserveExistingParticipants: true,
	}
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
		added, err := r.importBatch(ctx, conversationID, sourceConvID, messages, conversation, members)
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
	r.imp.progress.OnChatComplete(sourceConvID, chatAdded)
	return nil
}

// renumberedChat retains an archived PNI or phone chat after its contact changes number.
func (r *run) renumberedChat(ctx context.Context, chat sigRecipient, archived map[string]bool) (sigRecipient, error) {
	if chat.GroupID != "" || chat.ACI == "" || archived[directACIPrefix+chat.ACI] || archived[directPNIPrefix+chat.PNI] {
		return chat, nil
	}
	pid, _, err := r.imp.store.ParticipantByIdentifierContext(ctx, ACIIdentifierType, chat.ACI)
	if err != nil {
		return chat, fmt.Errorf("look up Signal chat participant: %w", err)
	}
	shared, err := r.imp.store.SharedConversationsContext(ctx, r.participants[r.selfRecipientID], pid)
	for _, conversation := range shared {
		pni, isPNI := strings.CutPrefix(conversation.SourceConversationID, directPNIPrefix)
		phone, isPhone := strings.CutPrefix(conversation.SourceConversationID, directPhonePrefix)
		if (isPNI || isPhone) && conversation.SourceID == r.sourceID && archived[conversation.SourceConversationID] {
			if isPNI {
				chat.PNI = pni
			} else {
				chat.E164 = phone
			}
			break
		}
	}
	return chat, err
}

func (r *run) skipThread(ctx context.Context, thread sigThread) error {
	var count int64
	if err := r.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM message WHERE thread_id = ? AND latest_revision_id IS NULL`, thread.ID).Scan(&count); err != nil {
		return fmt.Errorf("count skipped chat messages: %w", err)
	}
	r.summary.ChatsSkipped++
	r.summary.MessagesProcessed += count
	r.summary.MessagesSkipped += count
	return nil
}

// pendingMessage is a message row that passed the filters, with its ID.
type pendingMessage struct {
	row      sigMessage
	chatID   string
	id       string
	aliases  []string // older IDs the archive may hold the message under
	rootID   int64
	senderID int64
}

func (r *run) importBatch(ctx context.Context, conversationID int64, sourceConvID string, messages []sigMessage, conversation store.ConversationPersistData, members []int64) (int64, error) {
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
		tokens := r.tokens(author, conversation.ConversationType == "direct_chat")
		if len(tokens) == 0 || m.DateSent <= 0 {
			r.summary.MessagesSkipped++
			continue
		}
		rootID := m.RowID
		if m.OriginalRowID.Valid {
			rootID = m.OriginalRowID.Int64
		}
		candidates := messageIDs(sourceConvID, tokens, m.DateSent)
		pending = append(pending, pendingMessage{row: m, chatID: sourceConvID, id: candidates[0], aliases: candidates[1:], rootID: rootID, senderID: author})
		ids = append(ids, candidates...)
		lookupRows = append(lookupRows, m.RowID)
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
		return 0, fmt.Errorf("read Signal attachments: %w", err)
	}
	roots := make([]int64, len(pending))
	visible := make([]int64, len(pending))
	for i, p := range pending {
		roots[i] = p.rootID
		visible[i] = p.row.RowID
	}
	reactions, err := fetchReactions(ctx, r.db, roots)
	reactionsRead := err == nil
	if err != nil {
		// Keep the archived reactions rather than replacing them with an
		// empty snapshot the backup did not actually contain.
		r.summary.Errors++
		r.imp.progress.OnError(err)
	}
	mentions, err := fetchMentions(ctx, r.db, visible)
	if err != nil {
		return 0, fmt.Errorf("read Signal mentions: %w", err)
	}

	var added int64
	for _, p := range pending {
		if r.limitReached() {
			break
		}
		rowAttachments := attachments[p.row.RowID]
		archivedID, isExisting, err := r.resolveArchivedID(existing, &p)
		if err != nil {
			return added, err
		}
		var rowReactions []sigReaction
		if reactionsRead {
			rowReactions = reactions[p.rootID]
		}
		messageID, err := r.writeMessage(ctx, conversationID, archivedID, conversation, members, p, rowAttachments,
			reactionsRead, rowReactions, mentions[p.row.RowID])
		if err != nil {
			if ctx.Err() != nil {
				return added, ctx.Err()
			}
			r.summary.Errors++
			r.imp.progress.OnError(fmt.Errorf("write Signal message %s: %w", p.id, err))
			continue
		}
		if isExisting {
			r.summary.MessagesUpdated++
		} else {
			// Pre-191 backups can repeat a message; a later copy is an update.
			existing[p.id] = messageID
			r.written++
			r.summary.MessagesAdded++
			added++
		}
	}
	return added, nil
}

// resolveArchivedID finds p in the archive and moves an archived alias to
// p's preferred ID.
func (r *run) resolveArchivedID(existing map[string]int64, p *pendingMessage) (int64, bool, error) {
	preferredID := p.id
	archivedID, isExisting := keepArchivedID(existing, p)
	if !isExisting || p.id == preferredID {
		return archivedID, isExisting, nil
	}
	rekeyed, err := r.imp.store.RekeyMessageSourceID(archivedID, p.id, preferredID)
	if err != nil {
		return 0, false, err
	}
	if !rekeyed {
		return 0, false, fmt.Errorf("message %s changed; run the import again", p.id)
	}
	delete(existing, p.id)
	existing[preferredID] = archivedID
	p.id = preferredID
	return archivedID, true, nil
}

// keepArchivedID resolves p to the ID the archive already holds it under,
// keeping a PNI or phone ID the way conversationIdentity keeps a chat ID.
func keepArchivedID(existing map[string]int64, p *pendingMessage) (int64, bool) {
	for _, id := range append([]string{p.id}, p.aliases...) {
		if archivedID, ok := existing[id]; ok {
			p.id = id
			return archivedID, true
		}
	}
	return 0, false
}

func (r *run) writeMessage(
	ctx context.Context, conversationID, archivedID int64, conversation store.ConversationPersistData, members []int64, p pendingMessage,
	attachments []sigAttachment, reactionsRead bool, reactions []sigReaction, mentions []sigMention,
) (int64, error) {
	m := p.row
	st := r.imp.store
	var previous sigMessage
	var archivedBody string
	var previousValid bool
	if archivedID != 0 {
		var archivedRaw []byte
		var err error
		archivedBody, archivedRaw, err = st.GetMessageBodyAndRawContext(ctx, archivedID)
		if err != nil {
			return 0, fmt.Errorf("read archived Signal revision: %w", err)
		}
		previousValid = json.Unmarshal(archivedRaw, &previous) == nil
		if previousValid && previous.RevisionDateSent > m.RevisionDateSent {
			return archivedID, nil
		}
	}
	var found, missing int64
	body, preservedBody := "", false
	if m.Body.Valid {
		body = applyMentions(m.Body.String, mentions, r.displayName)
	}
	for _, a := range attachments {
		if a.ContentType == "text/x-signal-plain" {
			found++
			if name, ok := r.files[a.ID]; ok {
				file, err := export.OpenAttachmentSource(filepath.Join(r.opts.AttachmentFilesDir, name), r.opts.MaxAttachmentSize)
				var full []byte
				if err == nil {
					full, err = io.ReadAll(io.LimitReader(file, r.opts.MaxAttachmentSize+1))
					err = errors.Join(err, file.Close())
					if int64(len(full)) > r.opts.MaxAttachmentSize {
						err = fmt.Errorf("long text exceeds maximum attachment size of %d bytes", r.opts.MaxAttachmentSize)
					}
				}
				if err == nil && len(full) > 0 {
					body = applyMentions(string(full), mentions, r.displayName)
					continue
				}
				if err != nil {
					r.summary.Errors++
					r.imp.progress.OnError(fmt.Errorf("read long text: %w", err))
				}
			}
			missing++
			if archivedID != 0 {
				if previousValid && previous.RevisionDateSent == m.RevisionDateSent && previous.Body == m.Body {
					body, preservedBody = archivedBody, true
				}
			}
		}
	}
	for _, a := range attachments {
		if !preservedBody && a.Caption != "" && a.Caption != body {
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
		return 0, err
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
		return 0, err
	}
	writes, mediaFound, mediaMissing, err := r.prepareAttachments(ctx, archivedID, attachments)
	if err != nil {
		return 0, err
	}
	senderAddr := r.recipients[p.senderID].E164
	if p.senderID == r.selfRecipientID {
		senderAddr = r.opts.Phone
	}
	if senderID.Valid {
		conversation.Participants = []store.ConversationParticipantRef{{ParticipantID: senderID.Int64, Role: "member"}}
	}
	data := store.MessagePersistData{
		Conversation: &conversation,
		Message:      &msg, BodyText: sql.NullString{String: body, Valid: body != ""},
		RawMIME: raw, RawFormat: rawFormat, Edited: m.OriginalRowID.Valid,
		Attachments: writes, PreserveLabels: true,
		FTS: &store.FTSDoc{Body: body, FromAddr: senderAddr},
	}
	if senderID.Valid {
		data.Recipients = append(data.Recipients, store.RecipientSet{
			Type: "from", ParticipantIDs: []int64{senderID.Int64}, DisplayNames: []string{r.displayName(p.senderID)},
		})
	}
	to := store.RecipientSet{Type: "to"}
	seen := make(map[int64]bool)
	if senderID.Valid {
		seen[senderID.Int64] = true
	}
	for _, member := range members {
		pid, ok, _ := r.participant(ctx, member)
		if !ok {
			// Preserve archived recipients when the backup's set is incomplete.
			to.ParticipantIDs = nil
			break
		}
		if !seen[pid] {
			seen[pid] = true
			to.ParticipantIDs = append(to.ParticipantIDs, pid)
			to.DisplayNames = append(to.DisplayNames, r.displayName(member))
		}
	}
	if len(to.ParticipantIDs) > 0 && archivedID == 0 {
		data.Recipients = append(data.Recipients, to)
	}
	parentID, err := r.quotedMessageID(ctx, p.chatID, conversation.ConversationType == "direct_chat", m)
	if err != nil {
		return 0, err
	}
	if parentID != "" {
		metadata, err := json.Marshal(map[string]string{"referenced_message_id": parentID})
		if err != nil {
			return 0, err
		}
		data.Metadata = &sql.NullString{String: string(metadata), Valid: true}
		existing, err := st.MessageExistsBatchContext(ctx, r.sourceID, []string{parentID})
		if err != nil {
			return 0, err
		}
		if id, ok := existing[parentID]; ok {
			msg.ReplyToMessageID = sql.NullInt64{Int64: id, Valid: true}
		}
	}

	if reactionsRead {
		// The backup holds each message's current reactions; replace the
		// archived set so removed and changed reactions disappear.
		refs := make([]store.ReactionRef, 0, len(reactions))
		for _, reaction := range reactions {
			pid, ok, err := r.participant(ctx, reaction.AuthorID)
			if err != nil {
				return 0, err
			}
			if !ok || reaction.Emoji == "" {
				continue
			}
			refs = append(refs, store.ReactionRef{
				ParticipantID: pid, Type: reactionTypeEmoji, Value: reaction.Emoji,
				CreatedAt: millisTime(reaction.DateSent).Time,
			})
		}
		data.Reactions = &refs
	}
	messageID, err := st.PersistMessageContext(ctx, &data)
	if err != nil {
		return 0, err
	}
	r.summary.AttachmentsFound += found + mediaFound
	r.summary.AttachmentsMissing += missing + mediaMissing
	if data.Edited && (archivedID == 0 || previous.RevisionDateSent != m.RevisionDateSent) {
		r.summary.MessagesEdited++
	}
	if data.Reactions != nil {
		r.summary.ReactionsAdded += data.ReactionsAdded
	}
	if msg.ReplyToMessageID.Valid {
		r.summary.RepliesLinked++
	} else if parentID != "" {
		r.pendingReplies[p.id] = true
	} else if m.QuoteID > 0 && m.QuoteAuthor > 0 && parentID == "" {
		r.summary.RepliesUnresolved++
	}
	return messageID, nil
}

// quotedMessageID resolves quotes to an edit's original identity before writing.
func (r *run) quotedMessageID(ctx context.Context, chatID string, direct bool, m sigMessage) (string, error) {
	if m.QuoteID <= 0 || m.QuoteAuthor <= 0 {
		return "", nil
	}
	tokens := r.tokens(m.QuoteAuthor, direct)
	if len(tokens) == 0 {
		return "", nil
	}
	timestamp := m.QuoteID
	var original int64
	err := r.db.QueryRowContext(ctx, `SELECT COALESCE(o.date_sent, m.date_sent)
      FROM message m LEFT JOIN message o ON o._id = m.original_message_id
      WHERE m.thread_id = ? AND m.from_recipient_id = ? AND m.date_sent = ? LIMIT 1`, m.ThreadID, m.QuoteAuthor, m.QuoteID).Scan(&original)
	if err == nil {
		timestamp = original
	} else if !errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("resolve quoted revision: %w", err)
	}
	candidates := messageIDs(chatID, tokens, timestamp)
	existing, err := r.imp.store.MessageExistsBatchContext(ctx, r.sourceID, candidates)
	if err != nil {
		return "", fmt.Errorf("look up archived Signal quote: %w", err)
	}
	p := pendingMessage{id: candidates[0], aliases: candidates[1:]}
	keepArchivedID(existing, &p)
	return p.id, nil
}

func (r *run) resolveReplies(ctx context.Context) error {
	before, err := r.imp.store.ListUnresolvedMessageReplies(r.sourceID, MessageType)
	if err != nil {
		return err
	}
	if err := r.imp.store.ResolveDeferredMessageRepliesContext(ctx, r.sourceID, MessageType, r.opts.BatchSize); err != nil {
		return fmt.Errorf("resolve Signal replies: %w", err)
	}
	after, err := r.imp.store.ListUnresolvedMessageReplies(r.sourceID, MessageType)
	if err != nil {
		return err
	}
	r.summary.RepliesLinked += int64(len(before) - len(after))
	for _, reply := range after {
		if r.pendingReplies[reply.SourceMessageID] {
			r.summary.RepliesUnresolved++
		}
	}
	return nil
}

// prepareAttachments stores new source parts and refreshes changed metadata.
func (r *run) prepareAttachments(ctx context.Context, archivedID int64, attachments []sigAttachment) ([]store.AttachmentWrite, int64, int64, error) {
	var writes []store.AttachmentWrite
	archived := make(map[string]store.AttachmentRef)
	if archivedID != 0 {
		refs, err := r.imp.store.MessageMIMEAttachmentsContext(ctx, archivedID)
		if err != nil {
			return nil, 0, 0, err
		}
		for _, ref := range refs {
			archived[ref.SourcePartKey] = ref
		}
	}
	var found, missing int64
	occurrences := make(map[string]int)
	for _, a := range attachments {
		if a.ContentType == "text/x-signal-plain" {
			continue
		}
		found++
		name, ok := r.files[a.ID]
		if !ok || r.opts.AttachmentsDir == "" {
			missing++
			continue
		}
		storagePath, contentHash, size, err := r.storeAttachment(ctx, filepath.Join(r.opts.AttachmentFilesDir, name))
		if err != nil {
			r.summary.Errors++
			r.imp.progress.OnError(fmt.Errorf("store attachment %d: %w", a.ID, err))
		}
		if err != nil || storagePath == "" {
			missing++
			continue
		}
		key := fmt.Sprintf("signal:attachment:%s:%d", contentHash, occurrences[contentHash])
		occurrences[contentHash]++
		mediaType := attachmentMediaType(a)
		role, roleSource := a.Role, store.AttachmentRoleSourceImporterSemantics
		if role != store.AttachmentRoleStandalone {
			roleSource = store.AttachmentRoleSourceProviderExplicit
		}
		if mediaType == "sticker" {
			role, roleSource = store.AttachmentRoleSticker, store.AttachmentRoleSourceProviderExplicit
		}
		if ref, ok := archived[key]; ok && ref.Filename == a.FileName && ref.MimeType == a.ContentType &&
			ref.Width == a.Width && ref.Height == a.Height && ref.MediaType == mediaType &&
			ref.Role == role && ref.RoleSource == roleSource {
			continue
		}
		writes = append(writes, store.AttachmentWrite{Filename: a.FileName, MIMEType: a.ContentType, StoragePath: storagePath,
			ContentHash: contentHash, Size: size, MediaType: mediaType, Width: a.Width, Height: a.Height,
			Role: role, RoleSource: roleSource, SourcePartKey: key})
	}
	return writes, found, missing, nil
}

func (r *run) storeAttachment(ctx context.Context, source string) (string, string, int64, error) {
	hash, size, err := export.HashAttachmentSource(ctx, source, r.opts.MaxAttachmentSize)
	if err != nil {
		return "", "", 0, err
	}
	blobs, err := attachmentstore.New(store.NewPackCatalog(r.imp.store), r.opts.AttachmentsDir)
	if err != nil {
		return "", hash, size, err
	}
	defer func() { _ = blobs.Close() }()
	reader, _, err := blobs.OpenStream(ctx, hash)
	if err == nil {
		_, readErr := io.Copy(io.Discard, reader)
		if err := errors.Join(readErr, reader.Close()); err != nil {
			return "", hash, size, err
		}
		return hash[:2] + "/" + hash, hash, size, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", hash, size, err
	}
	storagePath, hash, size, err := export.StoreAttachmentFromPathVerified(ctx, r.opts.AttachmentsDir, source, hash, size, r.opts.MaxAttachmentSize)
	if err == nil {
		r.summary.AttachmentsStored++
	}
	return storagePath, hash, size, err
}

func millisTime(ms int64) sql.NullTime {
	if ms <= 0 {
		return sql.NullTime{}
	}
	return sql.NullTime{Time: time.UnixMilli(ms).UTC(), Valid: true}
}
