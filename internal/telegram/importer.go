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
// does not yet carry, and a later export never mutates the bodies,
// recipients, attachments, or reply links of archived messages.
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
	defer func() {
		if syncErr != nil {
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

	if err := st.RecomputeConversationStats(source.ID); err != nil {
		logger.Warn("telegram: recompute stats", "err", err)
	}
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

// importChat imports one chat append-only. Existing source messages are
// skipped entirely; only unseen messages are written. ensureMedia re-ensures
// attachment rows of already-archived messages during a valid resume, whose
// byte-identical export makes the preserving upsert a no-op except for rows
// an interruption lost.
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

	// In a personal or bot chat the export determines the message's "to"
	// recipient: the chat's other participant. The peer is the first
	// non-owner user the chat's messages reveal; a chat whose messages
	// never name one carries no "to" recipients at all.
	direct := chat.Type == chatTypePersonalChat || chat.Type == chatTypeBotChat
	var peerPID int64
	var peerName string
	if direct {
		for i := range chat.Messages {
			m := &chat.Messages[i]
			if m.Service {
				continue
			}
			if kind, id, ok := parseSenderID(m.FromID); ok && kind == "user" && id != strconv.FormatInt(ownerID, 10) {
				pid, err := resolver.resolve(ctx, m.FromID, m.FromName)
				if err != nil {
					logger.Warn("telegram: resolve peer", "sender", m.FromID, "err", err)
					break
				}
				if pid != 0 {
					peerPID, peerName = pid, m.FromName
				}
				break
			}
		}
	}

	// One batched lookup decides what this run may import: a message the
	// archive already carries is skipped whole.
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
			if ensureMedia {
				if err := storeAttachments(ctx, st, absRoot, chat.ID, m, messageID, opts, logger, summary); err != nil {
					logger.Warn("telegram: re-ensure attachments", "err", err)
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
		snippet := store.SnippetFromBody(m.Text)
		msgRow := &store.Message{
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
		}
		messageID, err = st.UpsertMessage(msgRow)
		if err != nil {
			summary.Errors++
			summary.MessagesSkipped++
			logger.Warn("telegram: upsert message", "err", err)
			continue
		}
		summary.MessagesAdded++

		if err := st.UpsertMessageBody(messageID, sql.NullString{String: m.Text, Valid: m.Text != ""}, sql.NullString{}); err != nil {
			summary.Errors++
			logger.Warn("telegram: upsert body", "err", err)
		}

		// The edited flag is archive metadata UpsertMessage does not
		// write; it is set once, on first import.
		if m.Edited {
			if err := st.SetMessageEditedStateContext(ctx, messageID, true); err != nil {
				summary.Errors++
				logger.Warn("telegram: set edited state", "err", err)
			}
		}

		// Recipients: from = the real sender. A personal or bot chat also
		// determines to = the other participant; group and channel
		// messages stay sender-only.
		if senderID.Valid {
			if err := st.ReplaceMessageRecipients(messageID, "from", []int64{senderID.Int64}, []string{m.FromName}); err != nil {
				logger.Warn("telegram: replace from recipients", "err", err)
			}
			if direct {
				switch {
				case isFromMe && peerPID != 0:
					if err := st.ReplaceMessageRecipients(messageID, "to", []int64{peerPID}, []string{peerName}); err != nil {
						logger.Warn("telegram: replace to recipients", "err", err)
					}
				case !isFromMe:
					if err := st.ReplaceMessageRecipients(messageID, "to", []int64{ownerPID}, []string{ownerName}); err != nil {
						logger.Warn("telegram: replace to recipients", "err", err)
					}
				}
			}
		}

		if err := storeAttachments(ctx, st, absRoot, chat.ID, m, messageID, opts, logger, summary); err != nil {
			logger.Warn("telegram: store attachments", "err", err)
		}
		if len(candidates) > 0 {
			if err := st.RecomputeMessageAttachmentStats(messageID); err != nil {
				logger.Warn("telegram: recompute attachment stats", "err", err)
			}
		}

		if err := st.UpsertFTS(messageID, chat.Name, m.Text, m.FromName, "", ""); err != nil {
			logger.Warn("telegram: upsert fts", "err", err)
		}

		if m.ReplyTo != 0 {
			replies = append(replies, pendingReply{
				childSrcID:  srcMsgID,
				parentSrcID: fmt.Sprintf("%d:%d", chat.ID, m.ReplyTo),
			})
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
	// later in this chat both link.
	for _, reply := range replies {
		if err := st.SetReplyToContext(ctx, sourceID, reply.childSrcID, reply.parentSrcID); err != nil {
			summary.Errors++
			logger.Warn("telegram: set reply link", "err", err)
		}
	}
	return nil
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
