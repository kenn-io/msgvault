package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strconv"

	"github.com/gotd/td/constant"
	"go.kenn.io/msgvault/internal/store"
)

func messageReplyChatID(chatID int64, message *Message) (int64, error) {
	if message.ReplyMarkedPeerID != 0 {
		if message.ReplyPeerID != "" {
			return 0, errors.New("conflicting Telegram reply peers")
		}
		return message.ReplyMarkedPeerID, nil
	}
	if message.ReplyPeerID != "" {
		return ExportReplyPeerID(message.ReplyPeerID)
	}
	return chatID, nil
}

func telegramReplySourceID(ctx context.Context, st *store.Store, sourceID, chatID int64, message *Message, link *store.TelegramHistoryMigration) (string, bool, error) {
	peerID, err := messageReplyChatID(chatID, message)
	if err != nil {
		return "", false, err
	}
	if peerID != chatID && message.ReplyTo < 0 {
		link, err = st.TelegramHistoryMigrationContext(ctx, sourceID, peerID)
		if err != nil {
			return "", false, err
		}
	} else if peerID != chatID {
		link = nil
	}
	key, err := store.TelegramHistoryMessageKey(peerID, message.ReplyTo, link)
	if err != nil {
		return "", false, err
	}
	if peerID > 0 && peerID != chatID {
		legacy, err := st.HasLegacyTelegramConversationKeyContext(ctx, sourceID, peerID)
		if err != nil {
			return "", false, err
		}
		if legacy {
			return key, false, nil
		}
		ambiguousDirect, err := st.HasAmbiguousTelegramDirectKeyContext(ctx, sourceID, peerID)
		if err != nil {
			return "", false, err
		}
		if ambiguousDirect {
			return key, false, nil
		}
	}
	return key, true, nil
}

// LiveWriter shares participant resolution and source preflight across a sync.
// The caller must hold the source's sync execution for its entire lifetime.
type LiveWriter struct {
	store                       *store.Store
	resolver                    *participantResolver
	sourceID, ownerID, ownerPID int64
	ownerName                   string
	typedPeers                  map[int64]bool
	unrepairedShiftedChats      map[int64]struct{}
	dirtyConversations          map[int64]struct{}
	counts                      SyncCounts
}

// NewLiveWriter verifies the source and refuses unsafe legacy identity states.
func NewLiveWriter(ctx context.Context, st *store.Store, sourceID, ownerID int64, ownerName string) (*LiveWriter, error) {
	if st == nil || sourceID <= 0 || ownerID <= 0 {
		return nil, errors.New("invalid Telegram live writer")
	}
	source, err := st.GetSourceByIDContext(ctx, sourceID)
	if err != nil {
		return nil, fmt.Errorf("load Telegram source: %w", err)
	}
	if source == nil || source.SourceType != SourceType || source.Identifier != OwnerSourceIdentifier(ownerID) {
		return nil, errors.New("Telegram source does not match the live account")
	}
	legacy, err := st.HasLegacyTelegramConversationKeysContext(ctx, sourceID)
	if err != nil {
		return nil, err
	}
	if legacy {
		return nil, errors.New("legacy Telegram conversation keys require migration before live sync")
	}
	ambiguous, err := st.HasAmbiguousTelegramDirectKeysContext(ctx, sourceID)
	if err != nil {
		return nil, err
	}
	if ambiguous {
		return nil, errors.New("ambiguous legacy Telegram direct key requires peer verification before live sync")
	}
	shiftedChats, err := st.TelegramShiftedHistoryChannelsContext(ctx, sourceID)
	if err != nil {
		return nil, err
	}
	unrepairedShiftedChats := make(map[int64]struct{}, len(shiftedChats))
	for _, chatID := range shiftedChats {
		unrepairedShiftedChats[chatID] = struct{}{}
	}
	resolver := newParticipantResolver(st, sourceID)
	ownerPID, err := resolver.ensureOwner(ctx, ownerID, ownerName)
	if err != nil {
		return nil, err
	}
	return &LiveWriter{store: st, resolver: resolver, sourceID: sourceID, ownerID: ownerID, ownerPID: ownerPID, ownerName: ownerName, typedPeers: make(map[int64]bool), unrepairedShiftedChats: unrepairedShiftedChats, dirtyConversations: make(map[int64]struct{})}, nil
}

// RequiresShiftedHistoryRepair reports whether a peer still has Desktop
// message IDs that need verified migration linkage before it can be written.
func (w *LiveWriter) RequiresShiftedHistoryRepair(chatID int64) bool {
	if w == nil || chatID == 0 {
		return false
	}
	_, ok := w.unrepairedShiftedChats[chatID]
	return ok
}

// OwnerID identifies the account used by this writer.
func (w *LiveWriter) OwnerID() int64 { return w.ownerID }

// Counts returns the successful message writes attempted by this writer.
func (w *LiveWriter) Counts() SyncCounts { return w.counts }

// Write persists a message and marks its conversation for a stats refresh.
// The caller flushes stats after a page or batch and before checkpointing.
func (w *LiveWriter) Write(ctx context.Context, chat Chat, message Message) error {
	if w == nil || chat.ID == 0 || message.ID <= 0 || message.SentAt.IsZero() || message.RawJSON == "" {
		return errors.New("invalid Telegram live message")
	}
	if w.RequiresShiftedHistoryRepair(chat.ID) {
		return errors.New("Telegram shifted group history requires verified migration linkage before live sync")
	}
	if !w.typedPeers[chat.ID] && (chat.Type == chatTypePersonalChat || chat.Type == chatTypeBotChat) && chat.ID >= constant.MinMTProtoMonoforumID && chat.ID <= constant.MaxMTProtoMonoforumID {
		if err := w.store.EnsureTelegramTypedUserContext(ctx, w.sourceID, chat.ID, chat.Name); err != nil {
			return err
		}
		w.typedPeers[chat.ID] = true
	}
	if !w.typedPeers[chat.ID] && (chat.Type == chatTypePrivateSupergroup || chat.Type == chatTypePublicSupergroup) && constant.TDLibPeerID(chat.ID).IsMonoforum() {
		if err := w.store.EnsureTelegramTypedMonoforumContext(ctx, w.sourceID, chat.ID, chat.Name); err != nil {
			return err
		}
		w.typedPeers[chat.ID] = true
	}
	chat.Messages = []Message{message}
	conversationID, err := w.store.EnsureConversationWithTypeContext(ctx, w.sourceID,
		strconv.FormatInt(chat.ID, 10), chat.ConvType(), chat.Name)
	if err != nil {
		return fmt.Errorf("ensure Telegram conversation: %w", err)
	}
	w.dirtyConversations[conversationID] = struct{}{}
	summary := &ImportSummary{}
	writeErr := importChat(ctx, w.store, w.resolver, w.sourceID, w.ownerID, w.ownerPID, w.ownerName, "", &chat, "", nil,
		ImportOptions{NoResume: true}, slog.New(slog.DiscardHandler), nil, 0, summary)
	if writeErr != nil {
		return writeErr
	}
	w.counts.Processed += summary.MessagesProcessed
	w.counts.Added += summary.MessagesAdded
	return nil
}

// FlushConversationStats refreshes each conversation written since the last
// flush, including commits made before a later write error or cancellation.
func (w *LiveWriter) FlushConversationStats(ctx context.Context) error {
	if w == nil {
		return errors.New("invalid Telegram live writer")
	}
	ids := make([]int64, 0, len(w.dirtyConversations))
	for id := range w.dirtyConversations {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	uncanceled := context.WithoutCancel(ctx)
	for _, id := range ids {
		if err := w.store.RecomputeConversationStatsForConversationContext(uncanceled, id); err != nil {
			return fmt.Errorf("recompute Telegram conversation stats: %w", err)
		}
		delete(w.dirtyConversations, id)
	}
	return nil
}

// ResolveReplies retries verified deferred links before a page checkpoint.
func (w *LiveWriter) ResolveReplies(ctx context.Context) error {
	if w == nil {
		return errors.New("invalid Telegram live writer")
	}
	return resolveDeferredReplies(ctx, w.store, w.sourceID)
}

// ResolveRepliesForMessages retries only the messages from one history page.
// Their archived recovery metadata is the source of truth for parent links.
func (w *LiveWriter) ResolveRepliesForMessages(ctx context.Context, chatID int64, messageIDs []int64) error {
	if w == nil || chatID == 0 {
		return errors.New("invalid Telegram live reply page")
	}
	if len(messageIDs) == 0 {
		return nil
	}
	historyLink, err := w.store.TelegramHistoryMigrationContext(ctx, w.sourceID, chatID)
	if err != nil {
		return fmt.Errorf("load Telegram history linkage: %w", err)
	}
	sourceMessageIDs := make([]string, 0, len(messageIDs))
	for _, id := range messageIDs {
		if id <= 0 {
			return errors.New("invalid Telegram reply message ID")
		}
		key, err := store.TelegramHistoryMessageKey(chatID, id, historyLink)
		if err != nil {
			return err
		}
		sourceMessageIDs = append(sourceMessageIDs, key)
	}
	archived, err := w.store.MessageMetadataBatchContext(ctx, w.sourceID, sourceMessageIDs)
	if err != nil {
		return fmt.Errorf("load Telegram page reply metadata: %w", err)
	}
	for _, childID := range sourceMessageIDs {
		record, ok := archived[childID]
		if !ok || !record.Metadata.Valid {
			continue
		}
		var metadata telegramMessageMetadata
		if err := json.Unmarshal([]byte(record.Metadata.String), &metadata); err != nil {
			return fmt.Errorf("decode reply metadata for %s: %w", childID, err)
		}
		if metadata.ReferencedMessageID == "" || !metadata.ReplyPeerVerified {
			continue
		}
		if err := w.store.SetReplyToIfMissingContext(ctx, w.sourceID, childID, metadata.ReferencedMessageID); err != nil {
			return fmt.Errorf("resolve deferred reply %s: %w", childID, err)
		}
	}
	return nil
}

// WriteLiveMessage is a single-message convenience wrapper around LiveWriter.
func WriteLiveMessage(ctx context.Context, st *store.Store, sourceID, ownerID int64, ownerName string, chat Chat, message Message) error {
	w, err := NewLiveWriter(ctx, st, sourceID, ownerID, ownerName)
	if err != nil {
		return err
	}
	writeErr := w.Write(ctx, chat, message)
	statsErr := w.FlushConversationStats(context.WithoutCancel(ctx))
	var repliesErr error
	if writeErr == nil {
		repliesErr = w.ResolveReplies(ctx)
	}
	return errors.Join(writeErr, statsErr, repliesErr)
}
