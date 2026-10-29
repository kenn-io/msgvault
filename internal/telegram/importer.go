package telegram

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	json "encoding/json/v2"

	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/textutil"
)

// syncType names the sync_runs row this importer writes.
const syncType = "import-telegram"

// telegramCheckpoint is the JSON payload stored in sync_runs.cursor_before.
// RootDir and Fingerprint guard a resume against different export content,
// and CompletedChats names the chats that finished. Resume is chat-granular:
// a resumed run skips exactly the completed chats and reprocesses the
// interrupted one from its start, which append-only skipping makes safe.
type telegramCheckpoint struct {
	RootDir        string  `json:"root_dir"`
	Fingerprint    string  `json:"fingerprint"`
	CompletedChats []int64 `json:"completed_chats"`
}

// ImportOptions configures Import.
type ImportOptions struct {
	// Me overrides the exporting user's numeric Telegram ID when the export
	// does not identify one (single-chat exports). Empty means take the
	// export's personal_information.user_id.
	Me string
	// RootDir is the Telegram Desktop export directory.
	RootDir string
	// AttachmentsDir is the content-addressed attachment storage root.
	// Empty disables byte storage; attachment rows are still written.
	AttachmentsDir string
	// Limit caps the number of regular messages processed across the whole
	// export. 0 means no limit.
	Limit int
	// NoResume ignores any checkpoint and starts a fresh scan.
	NoResume bool
	// CheckpointEvery is the number of messages between checkpoints.
	// Defaults to 200 when <= 0.
	CheckpointEvery int
	// Logger receives progress; a discard logger is used when nil.
	Logger *slog.Logger
}

// ImportSummary describes one run.
type ImportSummary struct {
	Duration               time.Duration
	ChatsProcessed         int
	ServiceMessagesSkipped int64
	MessagesProcessed      int64
	MessagesAdded          int64
	MessagesSkipped        int64
	ParticipantsResolved   int
	AttachmentsFound       int
	AttachmentsStored      int
	AttachmentsMissing     int
	FromMeCount            int64
	Errors                 int
	WasResumed             bool
}

// Import reads a Telegram Desktop JSON export under opts.RootDir into st.
// The import is append-only: messages are keyed on the chat's decimal id
// and "<chat-id>:<message-id>", a run imports only messages the archive
// does not yet carry, and a later export never overwrites the bodies,
// recipients, attachments, or reply links an earlier import wrote. Each
// new message persists atomically — row, body, recipients, edited flag,
// and search document in one transaction — so an interruption can only
// lose whole messages, never leave one half-written. Attachments and
// reply links follow the commit inside their chat; a failure there fails
// the chat, which checkpoint resume then reprocesses whole.
func Import(ctx context.Context, st *store.Store, opts ImportOptions) (*ImportSummary, error) {
	start := time.Now()
	summary := &ImportSummary{}
	logger := opts.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	if opts.CheckpointEvery <= 0 {
		opts.CheckpointEvery = 200
	}

	if info, err := os.Stat(opts.RootDir); err != nil {
		return nil, fmt.Errorf("telegram: root dir: %w", err)
	} else if !info.IsDir() {
		return nil, fmt.Errorf("telegram: root is not a directory: %s", opts.RootDir)
	}
	absRoot, err := filepath.Abs(opts.RootDir)
	if err != nil {
		return nil, fmt.Errorf("telegram: abs root: %w", err)
	}

	export, err := OpenExport(absRoot)
	if err != nil {
		return nil, err
	}
	defer func() { _ = export.Close() }()
	ownerID := export.OwnerID()
	ownerName := export.OwnerName()
	if opts.Me != "" {
		me, err := strconv.ParseInt(strings.TrimSpace(opts.Me), 10, 64)
		if err != nil || me <= 0 {
			return nil, fmt.Errorf("telegram: --me must be a numeric Telegram user ID, got %q", opts.Me)
		}
		ownerID = me
	}
	if ownerID <= 0 {
		return nil, ErrNoOwner
	}

	source, err := st.GetOrCreateSource(SourceType, OwnerSourceIdentifier(ownerID))
	if err != nil {
		return nil, fmt.Errorf("telegram: source: %w", err)
	}

	// Resume only an interrupted run (GetLatestCheckpointedSyncByType
	// ignores completed ones) whose checkpoint names this root and the
	// exact export bytes. A different root — or a replaced, newer, or
	// date-ranged result.json at the same path — starts a fresh
	// deduplicating scan; the source_message_id check makes that safe.
	completed := map[int64]bool{}
	cp := store.Checkpoint{}
	if !opts.NoResume {
		prev, err := st.GetLatestCheckpointedSyncByType(source.ID, syncType)
		if err != nil && !errors.Is(err, store.ErrSyncRunNotFound) {
			return nil, fmt.Errorf("telegram: check checkpointed sync: %w", err)
		}
		if prev != nil && prev.CursorBefore.Valid && prev.CursorBefore.String != "" {
			var prior telegramCheckpoint
			if err := json.Unmarshal([]byte(prev.CursorBefore.String), &prior); err == nil &&
				prior.RootDir == absRoot && prior.Fingerprint == export.Fingerprint() {
				for _, chatID := range prior.CompletedChats {
					completed[chatID] = true
				}
				summary.WasResumed = true
				cp.MessagesProcessed = prev.MessagesProcessed
				cp.MessagesAdded = prev.MessagesAdded
				cp.ErrorsCount = prev.ErrorsCount
				summary.MessagesProcessed = prev.MessagesProcessed
				summary.MessagesAdded = prev.MessagesAdded
				logger.Info("telegram: resuming import",
					"root", absRoot, "completed_chats", len(completed))
			} else if err != nil {
				logger.Warn("telegram: unreadable checkpoint; starting fresh", "err", err)
			} else {
				logger.Info("telegram: checkpoint does not match this export; starting fresh",
					"checkpoint_root", prior.RootDir, "root", absRoot)
			}
		}
	}

	syncID, err := st.StartSync(source.ID, syncType)
	if err != nil {
		return nil, fmt.Errorf("telegram: start sync: %w", err)
	}
	st = st.ScopedToSync(source.ID, syncID)
	var syncErr error
	refreshStats := func() {
		if err := st.RecomputeConversationStats(source.ID); err != nil {
			logger.Warn("telegram: recompute stats", "err", err)
		}
	}
	defer func() {
		if syncErr != nil {
			// Chats an earlier point of the run — or an earlier run —
			// already committed keep fresh derived state even though the
			// run dies here.
			refreshStats()
			_ = st.FailSync(syncID, syncErr.Error())
		} else {
			_ = st.CompleteSync(syncID, "")
		}
	}()

	resolver := newParticipantResolver(st, source.ID)
	ownerPID, err := resolver.ensureOwner(ctx, ownerID, ownerName)
	if err != nil {
		syncErr = err
		return nil, err
	}
	for {
		if ctx.Err() != nil {
			syncErr = ctx.Err()
			return summary, ctx.Err()
		}
		if opts.Limit > 0 && summary.MessagesProcessed >= int64(opts.Limit) {
			break
		}
		chat, err := export.NextChat()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			syncErr = err
			return nil, err
		}
		if completed[chat.ID] {
			continue
		}
		// A resumed run reprocesses every chat the checkpoint does not
		// name as complete, so media rows an interruption cost are
		// re-ensured for already-archived messages of those chats.
		ensureMedia := summary.WasResumed
		if err := importChat(ctx, st, resolver, source.ID, ownerID, ownerPID, ownerName, absRoot, chat, export.Fingerprint(), completed, ensureMedia, opts, logger, &cp, syncID, summary); err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				syncErr = err
				return summary, err
			}
			if errors.Is(err, errLimitReached) {
				break
			}
			// A chat that fails is never marked complete: the run stops
			// with the error and the next run retries the chat.
			syncErr = err
			return summary, err
		}
		completed[chat.ID] = true
		if !opts.NoResume {
			_ = saveCheckpoint(st, syncID, absRoot, export.Fingerprint(), completed, &cp, summary)
		}
	}

	refreshStats()
	summary.ParticipantsResolved = resolver.resolvedCount()
	summary.Duration = time.Since(start)
	return summary, nil
}

// errLimitReached signals that ImportOptions.Limit tripped mid-chat; the
// caller stops without marking the partial chat complete.
var errLimitReached = errors.New("telegram: import limit reached")

// pendingReply is one new message's reply link, resolved against the store
// after the whole chat is imported so parents earlier in the chat, parents
// archived by earlier exports, and forward references all link.
type pendingReply struct {
	childSrcID  string
	parentSrcID string
}

// importChat imports one chat append-only. A message the archive already
// carries is skipped as a new write; its reply link is still filled when
// absent, and a resumed run re-ensures its attachment rows, whose
// byte-identical export makes the preserving upsert a no-op except for
// rows an interruption lost.
func importChat(
	ctx context.Context,
	st *store.Store,
	resolver *participantResolver,
	sourceID, ownerID, ownerPID int64,
	ownerName, absRoot string,
	chat *Chat,
	fingerprint string,
	completed map[int64]bool,
	ensureMedia bool,
	opts ImportOptions,
	logger *slog.Logger,
	cp *store.Checkpoint,
	syncID int64,
	summary *ImportSummary,
) error {
	convID, err := st.EnsureConversationWithType(sourceID, strconv.FormatInt(chat.ID, 10), chat.ConvType(), chat.Name)
	if err != nil {
		return fmt.Errorf("ensure conversation: %w", err)
	}
	summary.ChatsProcessed++
	_ = st.EnsureConversationParticipant(convID, ownerPID, "member")

	// In a personal or bot chat the export determines each message's
	// "to" recipient: the chat's other participant.
	direct := chat.Type == chatTypePersonalChat || chat.Type == chatTypeBotChat
	peerPID, peerName := directPeer(ctx, resolver, ownerID, chat, logger)

	// One batched lookup decides what this run may import: a message the
	// archive already carries is skipped whole. Every archived message of
	// a completed chat is fully written, because each new message — row,
	// body, recipients, edited flag, and search document — persisted in
	// one transaction.
	srcIDs := make([]string, 0, len(chat.Messages))
	for i := range chat.Messages {
		if !chat.Messages[i].Service {
			srcIDs = append(srcIDs, fmt.Sprintf("%d:%d", chat.ID, chat.Messages[i].ID))
		}
	}
	archived, err := st.MessageExistsBatchContext(ctx, sourceID, srcIDs)
	if err != nil {
		return fmt.Errorf("check existing messages: %w", err)
	}

	var replies []pendingReply
	// replyOf records one message's reply link for the post-chat pass.
	replyOf := func(m *Message) {
		if m.ReplyTo != 0 {
			replies = append(replies, pendingReply{
				childSrcID:  fmt.Sprintf("%d:%d", chat.ID, m.ID),
				parentSrcID: fmt.Sprintf("%d:%d", chat.ID, m.ReplyTo),
			})
		}
	}
	for mi := range chat.Messages {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if opts.Limit > 0 && summary.MessagesProcessed >= int64(opts.Limit) {
			return errLimitReached
		}
		m := &chat.Messages[mi]
		if m.Service {
			summary.ServiceMessagesSkipped++
			continue
		}

		srcMsgID := fmt.Sprintf("%d:%d", chat.ID, m.ID)
		messageID, existed := archived[srcMsgID]
		summary.MessagesProcessed++
		if existed {
			summary.MessagesSkipped++
			replyOf(m)
			if ensureMedia {
				if err := storeAttachments(ctx, st, absRoot, chat.ID, m, messageID, opts, logger, summary); err != nil {
					return fmt.Errorf("re-ensure attachments for %s: %w", srcMsgID, err)
				}
			}
			continue
		}

		senderID := sql.NullInt64{}
		isFromMe := false
		if m.FromID != "" {
			pid, isOwner, err := resolver.chatMember(ctx, ownerID, ownerPID, m)
			if err != nil {
				logger.Warn("telegram: resolve sender", "sender", m.FromID, "err", err)
			} else if pid != 0 {
				senderID = sql.NullInt64{Int64: pid, Valid: true}
				isFromMe = isOwner
				_ = st.EnsureConversationParticipant(convID, pid, "member")
			}
		}
		if isFromMe {
			summary.FromMeCount++
		}

		candidates := attachmentCandidates(m)
		snippet := textutil.SnippetFromBody(m.Text)
		data := &store.MessagePersistData{
			Message: &store.Message{
				ConversationID:  convID,
				SourceID:        sourceID,
				SourceMessageID: srcMsgID,
				MessageType:     MessageType,
				SentAt:          sql.NullTime{Time: m.SentAt, Valid: !m.SentAt.IsZero()},
				ReceivedAt:      sql.NullTime{Time: m.SentAt, Valid: !m.SentAt.IsZero()},
				SenderID:        senderID,
				IsFromMe:        isFromMe,
				Snippet:         sql.NullString{String: snippet, Valid: snippet != ""},
				SizeEstimate:    int64(len(m.Text)),
				HasAttachments:  len(candidates) > 0,
				AttachmentCount: len(candidates),
			},
			BodyText: sql.NullString{String: m.Text, Valid: m.Text != ""},
			// The edited flag is archive metadata the plain upsert does
			// not write; the atomic persist sets it exactly once, on
			// first import.
			Edited: sql.NullBool{Bool: m.Edited, Valid: true},
			FTS:    &store.FTSDoc{Subject: chat.Name, Body: m.Text, FromAddr: m.FromName},
		}
		if senderID.Valid {
			data.Recipients = recipientSets(m, senderID.Int64, isFromMe, ownerPID, direct, peerPID, peerName, ownerName)
		}
		messageID, err = st.PersistMessageContext(ctx, data)
		if err != nil {
			summary.Errors++
			summary.MessagesSkipped++
			logger.Warn("telegram: persist message", "err", err)
			continue
		}
		summary.MessagesAdded++
		replyOf(m)

		// Attachments and their stats are deferred writes that follow the
		// atomic commit. A failure fails the chat — it is never marked
		// complete — so checkpoint resume reprocesses it whole.
		if err := storeAttachments(ctx, st, absRoot, chat.ID, m, messageID, opts, logger, summary); err != nil {
			return fmt.Errorf("store attachments for %s: %w", srcMsgID, err)
		}
		if len(candidates) > 0 {
			if err := st.RecomputeMessageAttachmentStatsContext(ctx, messageID); err != nil {
				return fmt.Errorf("recompute attachment stats for %s: %w", srcMsgID, err)
			}
		}

		if !opts.NoResume && summary.MessagesProcessed > 0 &&
			summary.MessagesProcessed%int64(opts.CheckpointEvery) == 0 {
			// The in-progress chat is deliberately absent: a resume
			// reprocesses it from the start.
			_ = saveCheckpoint(st, syncID, absRoot, fingerprint, completed, cp, summary)
		}
	}

	// Reply parents resolve through the store after the chat is imported,
	// so a parent archived by an earlier export and a parent that appears
	// later in this chat both link. The pass covers skipped-as-existing
	// messages too and fills only NULL links: a link a --limit cut or an
	// interrupted run left absent is restored, and a link an earlier
	// import wrote is never rewritten. A failure fails the chat so the
	// next run retries the pass.
	for _, reply := range replies {
		if err := st.SetReplyToIfMissingContext(ctx, sourceID, reply.childSrcID, reply.parentSrcID); err != nil {
			return fmt.Errorf("link reply %s: %w", reply.childSrcID, err)
		}
	}
	return nil
}

// directPeer resolves the other participant of a personal or bot chat.
// The identity comes from stable chat metadata — in both chat types the
// export's chat id is the peer's own user id — so an outgoing-only export
// still names its recipient. The first non-owner user the messages reveal
// is only a fallback for an export that does not follow that shape. Zero
// means the export names no peer.
func directPeer(
	ctx context.Context,
	resolver *participantResolver,
	ownerID int64,
	chat *Chat,
	logger *slog.Logger,
) (int64, string) {
	if chat.ID > 0 {
		pid, err := resolver.resolve(ctx, "user"+strconv.FormatInt(chat.ID, 10), chat.Name)
		if err != nil {
			logger.Warn("telegram: resolve peer from chat id", "chat", chat.ID, "err", err)
		} else if pid != 0 {
			return pid, chat.Name
		}
	}
	for i := range chat.Messages {
		m := &chat.Messages[i]
		if m.Service {
			continue
		}
		if kind, id, ok := parseSenderID(m.FromID); ok && kind == "user" && id != strconv.FormatInt(ownerID, 10) {
			pid, err := resolver.resolve(ctx, m.FromID, m.FromName)
			if err != nil {
				logger.Warn("telegram: resolve peer from messages", "sender", m.FromID, "err", err)
				break
			}
			if pid != 0 {
				return pid, m.FromName
			}
			break
		}
	}
	return 0, ""
}

// recipientSets builds the recipient rows one message's export entry
// determines: from = the resolved sender. A personal or bot chat also
// determines to = the other participant; group and channel messages stay
// sender-only.
func recipientSets(
	m *Message,
	senderPID int64,
	isFromMe bool,
	ownerPID int64,
	direct bool,
	peerPID int64,
	peerName, ownerName string,
) []store.RecipientSet {
	sets := []store.RecipientSet{{
		Type: "from", ParticipantIDs: []int64{senderPID}, DisplayNames: []string{m.FromName},
	}}
	if !direct {
		return sets
	}
	switch {
	case isFromMe && peerPID != 0:
		sets = append(sets, store.RecipientSet{
			Type: "to", ParticipantIDs: []int64{peerPID}, DisplayNames: []string{peerName},
		})
	case !isFromMe:
		sets = append(sets, store.RecipientSet{
			Type: "to", ParticipantIDs: []int64{ownerPID}, DisplayNames: []string{ownerName},
		})
	}
	return sets
}

// storeAttachments stores each media candidate for one message. A missing
// file preserves previously stored bytes: the keyed row keeps its hash, so
// an interruption-torn import can be resumed without losing archived media.
func storeAttachments(
	ctx context.Context,
	st *store.Store,
	absRoot string,
	chatID int64,
	m *Message,
	messageID int64,
	opts ImportOptions,
	logger *slog.Logger,
	summary *ImportSummary,
) error {
	for _, cand := range attachmentCandidates(m) {
		summary.AttachmentsFound++
		partKey := syntheticAttachmentHash(chatID, m.ID, cand.RelPath)
		storagePath, contentHash, size, err := storeAttachment(opts.AttachmentsDir, absRoot, cand.RelPath)
		if err != nil {
			logger.Warn("telegram: media path rejected", "path", cand.RelPath, "err", err)
		}
		role := store.AttachmentRoleStandalone
		roleSource := store.AttachmentRoleSourceImporterSemantics
		switch cand.MediaType {
		case "sticker":
			role = store.AttachmentRoleSticker
			roleSource = store.AttachmentRoleSourceProviderExplicit
		case "thumbnail":
			role = store.AttachmentRoleThumbnail
			roleSource = store.AttachmentRoleSourceProviderExplicit
		}
		mimeType := cand.MIMEType
		if mimeType == "" {
			mimeType = mimeTypeFor(cand.Filename)
		}
		write := store.AttachmentWrite{
			Filename:           cand.Filename,
			MIMEType:           mimeType,
			StoragePath:        storagePath,
			ContentHash:        contentHash,
			Size:               size,
			SourceAttachmentID: cand.RelPath,
			MediaType:          cand.MediaType,
			Role:               role,
			RoleSource:         roleSource,
			SourcePartKey:      partKey,
		}
		if storagePath == "" {
			// No bytes now. Keep the hash empty so
			// UpsertAttachmentRecordPreservingStored retains a previously
			// stored blob under this key.
			summary.AttachmentsMissing++
			write.ContentHash = ""
			write.Size = 0
		} else {
			summary.AttachmentsStored++
		}
		if err := st.UpsertAttachmentRecordPreservingStored(ctx, messageID, write); err != nil {
			return fmt.Errorf("upsert attachment %s: %w", cand.RelPath, err)
		}
	}
	return nil
}

// saveCheckpoint persists the completed-chat set. Errors are returned but
// the caller logs rather than aborts.
func saveCheckpoint(
	st *store.Store, syncID int64,
	absRoot, fingerprint string,
	completed map[int64]bool,
	cp *store.Checkpoint, summary *ImportSummary,
) error {
	chats := make([]int64, 0, len(completed))
	for chatID, done := range completed {
		if done {
			chats = append(chats, chatID)
		}
	}
	slices.Sort(chats)
	b, err := json.Marshal(telegramCheckpoint{
		RootDir:        absRoot,
		Fingerprint:    fingerprint,
		CompletedChats: chats,
	}, json.Deterministic(true))
	if err != nil {
		return fmt.Errorf("marshal telegram checkpoint: %w", err)
	}
	cp.PageToken = string(b)
	cp.MessagesProcessed = summary.MessagesProcessed
	cp.MessagesAdded = summary.MessagesAdded
	cp.ErrorsCount = int64(summary.Errors)
	return st.UpdateSyncCheckpoint(syncID, cp)
}
