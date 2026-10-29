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
	// UnsupportedEntriesSkipped counts "unsupported" entries: objects
	// Telegram Desktop's serializer emits with only an id and a type when
	// it cannot represent the entry.
	UnsupportedEntriesSkipped int64
	MessagesProcessed         int64
	MessagesAdded             int64
	MessagesSkipped           int64
	ParticipantsResolved      int
	AttachmentsFound          int
	AttachmentsStored         int
	AttachmentsMissing        int
	FromMeCount               int64
	Errors                    int
	WasResumed                bool
}

// Import reads a Telegram Desktop JSON export under opts.RootDir into st.
// The import is append-only: messages are keyed on the chat's decimal id
// and "<chat-id>:<message-id>", a run imports only messages the archive
// does not yet carry, and a later export never overwrites the bodies,
// recipients, attachments, or reply links an earlier import wrote. Each
// new message persists atomically — row, body, recipients, edited flag,
// search document, and the message's own recovery state (its reply
// reference and intended attachment manifest) in one transaction — so an
// interruption can only lose whole messages, never leave one
// half-written. Attachments and reply links follow the commit inside
// their chat; the next run — resumed or not — restores what an
// interruption left behind, reading the message's archived recovery
// state rather than the newer export's shape. After every successful
// run, one bounded pass retries the source's still-NULL reply links, so
// a child imported before its parent links once any later export
// archives the parent.
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
		if err := importChat(ctx, st, resolver, source.ID, ownerID, ownerPID, ownerName, absRoot, chat, export.Fingerprint(), completed, opts, logger, &cp, syncID, summary); err != nil {
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
			if err := saveCheckpoint(st, syncID, absRoot, export.Fingerprint(), completed, &cp, summary); err != nil {
				// A checkpoint that did not persist means a later
				// interruption cannot resume; the caller must hear it.
				syncErr = fmt.Errorf("telegram: save checkpoint: %w", err)
				return summary, syncErr
			}
		}
	}

	// Reply children whose parents were absent at import time stay
	// unlinked until any export of this source archives the parent.
	// Retry the remaining NULL links on every successful run — a retry
	// that adds zero messages can still follow a committed parent —
	// reading only replies the archive still carries unlinked. The
	// bounded pass fills only NULL links, so a link an earlier import
	// wrote is never rewritten.
	if err := resolveDeferredReplies(ctx, st, source.ID); err != nil {
		syncErr = err
		return summary, err
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

// telegramMessageMetadata is the recovery state a message persists
// atomically with its row. ReferencedMessageID is the parent's
// source-wide message id ("<chat-id>:<message-id>") captured at the
// child's first import, so the deferred pass can resolve the link after
// any later export archives the parent. AttachmentPartKeys is the
// message's intended attachment manifest — the source-part keys of the
// export the first import archived — and bounds every later recovery
// write: recovery fills exactly the manifest-listed occurrences, never
// adding keys beyond it. A nil AttachmentPartKeys marks rows persisted
// before manifests were recorded; those keep the legacy restore rules.
type telegramMessageMetadata struct {
	ReferencedMessageID string   `json:"referenced_message_id,omitempty"`
	AttachmentPartKeys  []string `json:"attachment_part_keys"`
}

// telegramReplyPageSize bounds one keyset page of the deferred reply pass.
const telegramReplyPageSize = 500

// resolveDeferredReplies links telegram replies whose parents were absent
// when the child was archived and have since arrived in any export of the
// source. Each child's recorded parent reference drives the lookup, and
// only NULL links are filled, so a link an earlier import wrote is never
// rewritten.
func resolveDeferredReplies(ctx context.Context, st *store.Store, sourceID int64) error {
	afterID := int64(0)
	for {
		unresolved, err := st.ListUnresolvedMessageRepliesAfterContext(ctx,
			sourceID, MessageType, afterID, telegramReplyPageSize)
		if err != nil {
			return fmt.Errorf("telegram: list deferred replies: %w", err)
		}
		for _, reply := range unresolved {
			var metadata telegramMessageMetadata
			if err := json.Unmarshal([]byte(reply.Metadata), &metadata); err != nil {
				return fmt.Errorf("telegram: decode reply metadata for %s: %w", reply.SourceMessageID, err)
			}
			if metadata.ReferencedMessageID != "" {
				if err := st.SetReplyToIfMissingContext(ctx, sourceID,
					reply.SourceMessageID, metadata.ReferencedMessageID); err != nil {
					return fmt.Errorf("telegram: resolve deferred reply %s: %w", reply.SourceMessageID, err)
				}
			}
			afterID = reply.MessageID
		}
		if len(unresolved) < telegramReplyPageSize {
			return nil
		}
	}
}

// importChat imports one chat append-only. A message the archive already
// carries is skipped as a new write, and its recovery — reply link and
// attachments alike — reads the recovery state archived with the message,
// never the current export's shape: a newer snapshot cannot move a reply
// reference, turn a non-reply into a reply, or extend an attachment
// manifest. Attachment rows are re-ensured so an interruption between
// occurrences is repaired: only the fields of manifest-listed keys the
// archive lacks are filled, never rewritten or extended. Legacy rows
// persisted before recovery state existed keep the restore rules their
// rows can express.
func importChat(
	ctx context.Context,
	st *store.Store,
	resolver *participantResolver,
	sourceID, ownerID, ownerPID int64,
	ownerName, absRoot string,
	chat *Chat,
	fingerprint string,
	completed map[int64]bool,
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
	if err := st.EnsureConversationParticipant(convID, ownerPID, "member"); err != nil {
		return fmt.Errorf("ensure owner membership: %w", err)
	}

	// In a personal or bot chat the export determines each message's
	// "to" recipient: the chat's other participant. The peer resolves
	// before any message commits, so a chat whose peer cannot resolve
	// fails whole instead of committing recipient-less messages.
	direct := chat.Type == chatTypePersonalChat || chat.Type == chatTypeBotChat
	var peerPID int64
	var peerName string
	if direct {
		peerPID, peerName, err = directPeer(ctx, resolver, chat)
		if err != nil {
			return fmt.Errorf("resolve chat peer: %w", err)
		}
	}

	// One batched lookup decides what this run may import: a message the
	// archive already carries is skipped whole. The same query returns
	// each archived message's recovery state — the metadata persisted
	// atomically with its row — because every archived message of a
	// completed chat is fully written: row, body, recipients, edited
	// flag, search document, and recovery state in one transaction.
	srcIDs := make([]string, 0, len(chat.Messages))
	for i := range chat.Messages {
		if !chat.Messages[i].Service && !chat.Messages[i].Unsupported {
			srcIDs = append(srcIDs, fmt.Sprintf("%d:%d", chat.ID, chat.Messages[i].ID))
		}
	}
	archived, err := st.MessageMetadataBatchContext(ctx, sourceID, srcIDs)
	if err != nil {
		return fmt.Errorf("check existing messages: %w", err)
	}

	var replies []pendingReply
	// replyOf records one new message's reply link for the post-chat
	// pass; at first import the export is the archived snapshot.
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
		if m.Unsupported {
			summary.UnsupportedEntriesSkipped++
			continue
		}

		srcMsgID := fmt.Sprintf("%d:%d", chat.ID, m.ID)
		summary.MessagesProcessed++
		candidates := attachmentCandidates(m)
		if record, existed := archived[srcMsgID]; existed {
			messageID := record.ID
			summary.MessagesSkipped++
			// Recovery is driven by the recovery state the message
			// archived at first import, never by this export's shape.
			var recovery telegramMessageMetadata
			if record.Metadata.Valid {
				if err := json.Unmarshal([]byte(record.Metadata.String), &recovery); err != nil {
					return fmt.Errorf("decode recovery state for %s: %w", srcMsgID, err)
				}
			}
			// The archived reference is the only parent an existing
			// message may resolve to: an overlapping export cannot move
			// an unresolved reference to a different parent or turn an
			// archived non-reply into a reply. The post-chat pass still
			// fills only NULL links.
			if recovery.ReferencedMessageID != "" {
				replies = append(replies, pendingReply{
					childSrcID:  srcMsgID,
					parentSrcID: recovery.ReferencedMessageID,
				})
			}
			// Attachment recovery must not depend on checkpoint state:
			// a failure or interruption after the message commit but
			// before the first checkpoint leaves the attachment absent,
			// and this ordinary run is the next run.
			if len(candidates) > 0 {
				// The recorded manifest — the source-part keys the
				// first import intended — bounds the restore: exactly
				// the manifest-listed occurrences the archive lacks are
				// filled, so media a later export renamed or added
				// never creates a new occurrence.
				var restore map[string]struct{}
				if recovery.AttachmentPartKeys != nil {
					restore = make(map[string]struct{}, len(recovery.AttachmentPartKeys))
					for _, key := range recovery.AttachmentPartKeys {
						restore[key] = struct{}{}
					}
				} else {
					// Legacy rows predate the recorded manifest. A
					// message with no attachment rows is restored from
					// this export — the torn-write case; a populated
					// row set is kept exactly, filling only missing
					// fields of the source-part keys it already
					// carries.
					archivedKeys, err := st.MessageAttachmentSourcePartKeysContext(ctx, messageID)
					if err != nil {
						return fmt.Errorf("load attachment manifest for %s: %w", srcMsgID, err)
					}
					restore = archivedKeys
					if len(archivedKeys) == 0 {
						restore = nil
					}
				}
				wrote, err := storeAttachments(ctx, st, absRoot, chat.ID, m, messageID, restore, opts, logger, summary)
				if err != nil {
					return fmt.Errorf("re-ensure attachments for %s: %w", srcMsgID, err)
				}
				// Every recovered write refreshes the derived flags a
				// consumer reads; an insert without the recompute would
				// leave them describing a manifest the message no
				// longer has.
				if wrote {
					if err := st.RecomputeMessageAttachmentStatsContext(ctx, messageID); err != nil {
						return fmt.Errorf("recompute attachment stats for %s: %w", srcMsgID, err)
					}
				}
			}
			continue
		}

		senderID := sql.NullInt64{}
		isFromMe := false
		if m.FromID != "" {
			// A sender that cannot resolve fails the chat before the
			// message commits: append-only skipping would otherwise
			// make the NULL sender permanent on the next run.
			pid, isOwner, err := resolver.chatMember(ctx, ownerID, ownerPID, m)
			if err != nil {
				return fmt.Errorf("resolve sender for %s: %w", srcMsgID, err)
			}
			if pid != 0 {
				senderID = sql.NullInt64{Int64: pid, Valid: true}
				isFromMe = isOwner
				if err := st.EnsureConversationParticipant(convID, pid, "member"); err != nil {
					return fmt.Errorf("ensure sender membership for %s: %w", srcMsgID, err)
				}
			}
		}
		if isFromMe {
			summary.FromMeCount++
		}

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
		// The message's recovery state persists atomically with the row:
		// the reply reference drives the deferred archive-wide pass after
		// any later export archives the parent this export lacks, and the
		// attachment manifest — the source-part keys this export intends,
		// which at first import is the archived snapshot — bounds every
		// later recovery write. The manifest records a message without
		// media as empty, so a later export cannot add media to it.
		meta := telegramMessageMetadata{
			AttachmentPartKeys: make([]string, 0, len(candidates)),
		}
		for _, cand := range candidates {
			meta.AttachmentPartKeys = append(meta.AttachmentPartKeys,
				syntheticAttachmentHash(chat.ID, m.ID, cand.RelPath))
		}
		if m.ReplyTo != 0 {
			meta.ReferencedMessageID = fmt.Sprintf("%d:%d", chat.ID, m.ReplyTo)
		}
		encoded, err := json.Marshal(meta, json.Deterministic(true))
		if err != nil {
			return fmt.Errorf("marshal recovery state for %s: %w", srcMsgID, err)
		}
		data.Metadata = &sql.NullString{String: string(encoded), Valid: true}
		messageID, err := st.PersistMessageContext(ctx, data)
		if err != nil {
			// The atomic persist rolled back, so the message is not in
			// the archive. Failing the chat keeps it out of the
			// completed set — a resume reprocesses the chat whole
			// instead of skipping the message as lost.
			summary.Errors++
			return fmt.Errorf("persist message %s: %w", srcMsgID, err)
		}
		summary.MessagesAdded++
		replyOf(m)

		// Attachments and their stats are deferred writes that follow the
		// atomic commit. A failure fails the chat — it is never marked
		// complete — so the next run reprocesses it whole.
		wrote, err := storeAttachments(ctx, st, absRoot, chat.ID, m, messageID, nil, opts, logger, summary)
		if err != nil {
			return fmt.Errorf("store attachments for %s: %w", srcMsgID, err)
		}
		if wrote {
			if err := st.RecomputeMessageAttachmentStatsContext(ctx, messageID); err != nil {
				return fmt.Errorf("recompute attachment stats for %s: %w", srcMsgID, err)
			}
		}

		if !opts.NoResume && summary.MessagesProcessed > 0 &&
			summary.MessagesProcessed%int64(opts.CheckpointEvery) == 0 {
			// The in-progress chat is deliberately absent: a resume
			// reprocesses it from the start. A checkpoint that fails to
			// persist fails the chat — a later interruption could not
			// resume.
			if err := saveCheckpoint(st, syncID, absRoot, fingerprint, completed, cp, summary); err != nil {
				return fmt.Errorf("save checkpoint: %w", err)
			}
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
// still names its recipient. A chat without that shape is malformed and
// errors rather than guessing a peer from message senders.
func directPeer(
	ctx context.Context,
	resolver *participantResolver,
	chat *Chat,
) (int64, string, error) {
	if chat.ID <= 0 {
		return 0, "", fmt.Errorf("telegram: %s %d carries no peer user id", chat.Type, chat.ID)
	}
	pid, err := resolver.resolve(ctx, "user"+strconv.FormatInt(chat.ID, 10), chat.Name)
	if err != nil {
		return 0, "", fmt.Errorf("telegram: resolve %s peer: %w", chat.Type, err)
	}
	return pid, chat.Name, nil
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

// storeAttachments stores each media candidate for one message. The write
// is append-only: a missing row is inserted and an occurrence an earlier
// import populated keeps its stored bytes and metadata even when the file
// at the same export path now differs or is gone. A nil archivedKeys
// restores every candidate; a non-nil set — the message's recorded
// manifest, or a legacy row's archived keys — allows only candidates
// whose source-part key the set carries, so media a later export renamed
// or added never creates a new occurrence. It reports whether any
// candidate was written.
func storeAttachments(
	ctx context.Context,
	st *store.Store,
	absRoot string,
	chatID int64,
	m *Message,
	messageID int64,
	archivedKeys map[string]struct{},
	opts ImportOptions,
	logger *slog.Logger,
	summary *ImportSummary,
) (bool, error) {
	wrote := false
	for _, cand := range attachmentCandidates(m) {
		partKey := syntheticAttachmentHash(chatID, m.ID, cand.RelPath)
		if archivedKeys != nil {
			if _, archived := archivedKeys[partKey]; !archived {
				continue
			}
		}
		summary.AttachmentsFound++
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
			// No bytes now. Keep the hash empty so the append-only ensure
			// inserts a placeholder without claiming stored data.
			summary.AttachmentsMissing++
			write.ContentHash = ""
			write.Size = 0
		} else {
			summary.AttachmentsStored++
		}
		// The ensure fills only rows and stored data an interruption left
		// missing; it never rewrites an archived occurrence.
		if err := st.EnsureAttachmentRecordAppendOnly(ctx, messageID, write); err != nil {
			return wrote, fmt.Errorf("ensure attachment %s: %w", cand.RelPath, err)
		}
		wrote = true
	}
	return wrote, nil
}

// saveCheckpoint persists the completed-chat set. An error fails the
// calling run: without a durable checkpoint, a later interruption cannot
// resume.
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
