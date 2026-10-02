package inline

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/store"
)

type Importer struct {
	store          *store.Store
	client         Client
	mediaTransport http.RoundTripper
}

func NewImporter(st *store.Store, client Client) *Importer {
	return &Importer{store: st, client: client}
}

type messageMetadata struct {
	ChatID              int64     `json:"chat_id"`
	ReplyToMessageID    int64     `json:"reply_to_message_id,omitempty"`
	ReferencedMessageID string    `json:"referenced_message_id,omitempty"`
	EditedAt            time.Time `json:"edited_at,omitzero"`
	Media               []Media   `json:"media,omitempty"`
	RawFormat           string    `json:"raw_format"`
	ProjectionFormat    string    `json:"projection_format"`
}

func (imp *Importer) validateOptions(ctx context.Context, opts ImportOptions) (ImportOptions, error) {
	if imp.store == nil || imp.client == nil {
		return opts, errors.New("inline importer requires a store and client")
	}
	if !validID(opts.Account.UserID) || opts.Limit < 0 {
		return opts, errors.New("valid Inline account and non-negative message limit required")
	}
	origin, err := CanonicalOrigin(opts.Account.Origin)
	if err != nil {
		return opts, err
	}
	opts.Account.Origin = origin
	seen := map[int64]bool{}
	for _, id := range opts.ChatIDs {
		if !validID(id) || seen[id] {
			return opts, errors.New("inline selected chat IDs must be valid and unique")
		}
		seen[id] = true
	}
	opts.ChatIDs = slices.Clone(opts.ChatIDs)
	if err := opts.MediaPolicy.Validate(); err != nil {
		return opts, err
	}
	actual, err := imp.client.Me(ctx)
	if err != nil {
		return opts, fmt.Errorf("authenticate Inline account: %w", err)
	}
	actualOrigin, err := CanonicalOrigin(actual.Origin)
	if err != nil {
		return opts, err
	}
	if actual.UserID != opts.Account.UserID || actualOrigin != origin {
		return opts, errors.New("inline client authenticated a different account")
	}
	if len(opts.ChatIDs) == 0 {
		catalog, discoverErr := imp.client.Discover(ctx)
		if discoverErr != nil {
			return opts, fmt.Errorf("discover Inline conversations: %w", discoverErr)
		}
		for _, chat := range catalog {
			if !validID(chat.ID) || seen[chat.ID] {
				return opts, errors.New("inline discovery returned an invalid or repeated chat identity")
			}
			seen[chat.ID] = true
			opts.ChatIDs = append(opts.ChatIDs, chat.ID)
		}
		slices.Sort(opts.ChatIDs)
	}
	return opts, nil
}

func (imp *Importer) loadState(sourceID int64, account string) (*SyncState, error) {
	checkpoint, err := imp.store.GetLatestCheckpointedSync(sourceID)
	if err == nil {
		if !checkpoint.CursorBefore.Valid {
			return nil, errors.New("inline checkpoint is missing")
		}
		return LoadSyncState(checkpoint.CursorBefore.String, account)
	}
	if !errors.Is(err, store.ErrSyncRunNotFound) {
		return nil, err
	}
	previous, err := imp.store.GetLastSuccessfulSync(sourceID)
	if err == nil {
		if !previous.CursorAfter.Valid {
			return nil, errors.New("inline completed cursor is missing")
		}
		return LoadSyncState(previous.CursorAfter.String, account)
	}
	if !errors.Is(err, store.ErrSyncRunNotFound) {
		return nil, err
	}
	return LoadSyncState("", account)
}

func checkpointFor(state *SyncState, sum *ImportSummary) (*store.Checkpoint, error) {
	blob, err := state.Marshal()
	if err != nil {
		return nil, err
	}
	return &store.Checkpoint{PageToken: blob, MessagesProcessed: int64(sum.MessagesProcessed), MessagesAdded: int64(sum.MessagesAdded), MessagesUpdated: int64(sum.MessagesUpdated), ErrorsCount: int64(sum.Errors)}, nil
}

func (imp *Importer) checkpoint(ctx context.Context, runID int64, state *SyncState, sum *ImportSummary) error {
	checkpoint, err := checkpointFor(state, sum)
	if err != nil {
		return err
	}
	return imp.store.UpdateSyncCheckpointContext(ctx, runID, checkpoint)
}

// Import captures discovered chats, or an explicit selected restriction. A run retains
// both older history debt and any pinned scan; --full resumes an active repair.
func (imp *Importer) Import(ctx context.Context, opts ImportOptions) (sum *ImportSummary, err error) {
	started := time.Now()
	sum = &ImportSummary{}
	defer func() { sum.Duration = time.Since(started) }()
	opts, err = imp.validateOptions(ctx, opts)
	if err != nil {
		return sum, err
	}
	source, err := imp.store.GetSourceByTypeAndIdentifier(SourceType, opts.Account.Identifier())
	if err != nil {
		return sum, err
	}
	sum.SourceID = source.ID
	state, err := imp.loadState(source.ID, opts.Account.Identifier())
	if err != nil {
		return sum, fmt.Errorf("load Inline resume state: %w", err)
	}
	// Paused chats retain their own repair debt, but cannot prevent a full
	// repair of the currently selected scope from starting or completing.
	state.RepairPending = false
	for _, id := range opts.ChatIDs {
		if state.chat(id).RepairActive {
			state.RepairPending = true
		}
	}
	if opts.Full {
		if !state.RepairPending {
			state.RepairScope = nil
		}
		for _, id := range opts.ChatIDs {
			if slices.Contains(state.RepairScope, id) {
				continue
			}
			state.RepairScope = append(state.RepairScope, id)
			chat := state.chat(id)
			if chat.RepairActive {
				continue
			}
			chat.RepairActive = true
			chat.RepairBefore = 0
			chat.RepairHead = 0
			chat.ScanActive = false
			chat.ScanBefore = 0
			chat.ScanHead = 0
			chat.ScanFloor = 0
		}
		state.RepairPending = true
	}
	runID, err := imp.store.StartSyncContext(ctx, source.ID, SourceType)
	if err != nil {
		return sum, err
	}
	scoped := &Importer{store: imp.store.ScopedToSync(source.ID, runID), client: imp.client, mediaTransport: imp.mediaTransport}
	defer func() {
		if err != nil {
			checkpoint, cpErr := checkpointFor(state, sum)
			if cpErr == nil {
				err = errors.Join(err, scoped.store.FailSyncWithCheckpoint(runID, "Inline sync did not complete", checkpoint))
			} else {
				err = errors.Join(err, scoped.store.FailSync(runID, "Inline sync did not complete"), cpErr)
			}
		}
	}()
	if err = scoped.checkpoint(ctx, runID, state, sum); err != nil {
		return sum, err
	}
	for _, chatID := range opts.ChatIDs {
		if err = ctx.Err(); err != nil {
			return sum, err
		}
		if opts.Limit > 0 && sum.MessagesProcessed >= opts.Limit {
			break
		}
		var conversation Conversation
		conversation, err = scoped.client.Conversation(ctx, chatID)
		if err != nil {
			return sum, fmt.Errorf("read selected Inline chat %d: %w", chatID, err)
		}
		if conversation.ID != chatID {
			return sum, errors.New("inline conversation response does not match selected chat")
		}
		var conversationID int64
		conversationID, err = scoped.persistConversation(source.ID, conversation)
		if err != nil {
			return sum, err
		}
		chat := state.chat(chatID)
		if chat.RepairActive {
			err = scoped.walk(ctx, source.ID, runID, conversationID, conversation, chat, state, sum, opts, "repair")
		} else {
			if chat.Initialized {
				if !chat.ScanActive {
					chat.ScanActive = true
					chat.ScanFloor = chat.Head
					chat.ScanBefore = 0
					chat.ScanHead = 0
					if err = scoped.checkpoint(ctx, runID, state, sum); err != nil {
						return sum, err
					}
				}
				err = scoped.walk(ctx, source.ID, runID, conversationID, conversation, chat, state, sum, opts, "scan")
			}
			if err == nil && !chat.ScanActive && !chat.HistoryDone {
				err = scoped.walk(ctx, source.ID, runID, conversationID, conversation, chat, state, sum, opts, "history")
			}
		}
		if err != nil {
			return sum, err
		}
		sum.ConversationsProcessed++
		if opts.Progress != nil {
			opts.Progress(fmt.Sprintf("chat:%d: %d messages archived", chatID, sum.MessagesProcessed))
		}
	}
	if err = scoped.repairReplies(ctx, source.ID, opts.ChatIDs); err != nil {
		return sum, err
	}
	if err = scoped.store.RecomputeConversationStatsContext(ctx, source.ID); err != nil {
		return sum, err
	}
	state.RepairPending = false
	for _, id := range opts.ChatIDs {
		if state.chat(id).RepairActive {
			state.RepairPending = true
		}
	}
	if !state.RepairPending {
		state.RepairScope = nil
	}
	if err = scoped.checkpoint(ctx, runID, state, sum); err != nil {
		return sum, err
	}
	var blob string
	blob, err = state.Marshal()
	if err != nil {
		return sum, err
	}
	err = scoped.store.CompleteSyncContext(ctx, runID, blob)
	return sum, err
}

func (imp *Importer) persistConversation(sourceID int64, conversation Conversation) (int64, error) {
	if !validID(conversation.ID) || conversation.ParentChatID < 0 || conversation.ParentChatID > MaxID || conversation.RootMessageID < 0 || conversation.RootMessageID > MaxID || conversation.MemberCount < 0 {
		return 0, errors.New("invalid Inline conversation identity or roster metadata")
	}
	id, err := imp.store.EnsureConversationWithType(sourceID, chatKey(conversation.ID), conversationType(conversation.Type), conversation.Title)
	if err != nil {
		return 0, err
	}
	metadata := map[string]any{"chat_id": conversation.ID, "parent_chat_id": conversation.ParentChatID, "root_message_id": conversation.RootMessageID, "source_type": conversation.Type}
	if len(conversation.Raw) > 0 {
		metadata["source"] = conversation.Raw
	}
	data, err := json.Marshal(metadata, json.Deterministic(true))
	if err != nil {
		return 0, err
	}
	if err = imp.store.SetConversationMetadata(id, sql.NullString{String: string(data), Valid: true}); err != nil {
		return 0, err
	}
	if conversation.MemberCountKnown {
		err = imp.store.SetConversationMemberCount(id, conversation.MemberCount)
	} else {
		err = imp.store.MarkConversationMemberCountUnknown(id)
	}
	return id, err
}

func (imp *Importer) walk(ctx context.Context, sourceID, runID, conversationID int64, conversation Conversation, chat *ChatState, state *SyncState, sum *ImportSummary, opts ImportOptions, phase string) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if opts.Limit > 0 && sum.MessagesProcessed >= opts.Limit {
			return nil
		}
		before := chat.HistoryBefore
		switch phase {
		case "scan":
			before = chat.ScanBefore
		case "repair":
			before = chat.RepairBefore
		}
		page, err := imp.client.Messages(ctx, conversation.ID, before)
		if err != nil {
			return fmt.Errorf("read Inline history: %w", err)
		}
		if err = validatePage(page, conversation.ID, before); err != nil {
			return err
		}
		if len(page.Messages) > 0 {
			switch phase {
			case "scan":
				if chat.ScanHead == 0 {
					chat.ScanHead = page.Messages[0].ID
				}
			case "repair":
				if chat.RepairHead == 0 {
					chat.RepairHead = page.Messages[0].ID
				}
			}
		}
		for _, message := range page.Messages {
			if phase == "scan" && message.ID <= chat.ScanFloor {
				return imp.finishWalk(ctx, runID, chat, state, sum, phase)
			}
			if opts.Limit > 0 && sum.MessagesProcessed >= opts.Limit {
				return nil
			}
			if err = imp.persistMessage(ctx, sourceID, conversationID, conversation, message, opts, sum); err != nil {
				return err
			}
			switch phase {
			case "scan":
				chat.ScanBefore = message.ID
			case "repair":
				chat.RepairBefore = message.ID
			default:
				if !chat.Initialized {
					chat.Initialized = true
					chat.Head = message.ID
				}
				chat.HistoryBefore = message.ID
			}
			if err = imp.checkpoint(ctx, runID, state, sum); err != nil {
				return err
			}
		}
		if !page.HasMore {
			return imp.finishWalk(ctx, runID, chat, state, sum, phase)
		}
	}
}

func (imp *Importer) finishWalk(ctx context.Context, runID int64, chat *ChatState, state *SyncState, sum *ImportSummary, phase string) error {
	switch phase {
	case "scan":
		chat.Head = max(chat.Head, chat.ScanHead)
		chat.ScanActive = false
		chat.ScanBefore = 0
		chat.ScanHead = 0
		chat.ScanFloor = 0
	case "repair":
		chat.Head = max(chat.Head, chat.RepairHead)
		chat.Initialized = true
		chat.HistoryDone = true
		chat.HistoryBefore = 0
		chat.RepairActive = false
		chat.RepairBefore = 0
		chat.RepairHead = 0
	default:
		chat.Initialized = true
		chat.HistoryDone = true
		chat.HistoryBefore = 0
	}
	return imp.checkpoint(ctx, runID, state, sum)
}

func (imp *Importer) persistMessage(ctx context.Context, sourceID, conversationID int64, conversation Conversation, message Message, opts ImportOptions, sum *ImportSummary) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if message.IsFromMe && message.SenderID != opts.Account.UserID {
		return errors.New("inline sent-by-me marker contradicts sender identity")
	}
	for _, media := range message.Media {
		if media.ChatID != message.ChatID || media.MessageID != message.ID {
			return errors.New("inline message media belongs to another message")
		}
	}
	key := messageKey(message.ChatID, message.ID)
	existing, err := imp.store.MessageExistsBatch(sourceID, []string{key})
	if err != nil {
		return err
	}
	senderID, err := imp.store.EnsureParticipantByIdentifier(SourceType, fmt.Sprintf("%s:user:%d", ProductionOrigin, message.SenderID), message.SenderName)
	if err != nil {
		return err
	}
	metadata := messageMetadata{ChatID: message.ChatID, ReplyToMessageID: message.ReplyToMessageID, EditedAt: message.EditedAt, Media: message.Media, RawFormat: message.RawFormat, ProjectionFormat: message.RawFormat}
	if message.ReplyToMessageID > 0 {
		metadata.ReferencedMessageID = messageKey(message.ChatID, message.ReplyToMessageID)
	}
	raw := []byte(message.Raw)
	if oldID := existing[key]; oldID != 0 {
		prior, readErr := imp.store.GetMessageMetadata(oldID)
		if readErr != nil {
			return readErr
		}
		if prior.Valid {
			var old messageMetadata
			if err = json.Unmarshal([]byte(prior.String), &old); err != nil {
				return err
			}
			if old.RawFormat == RawCLIFormat && message.RawFormat == RawMCPFormat {
				metadata.RawFormat = old.RawFormat
				raw = nil
			}
			// Provider projections and source removals must not erase archived
			// attachment provenance needed to retry a previously captured file.
			seenMedia := map[string]bool{}
			for _, media := range metadata.Media {
				seenMedia[media.ID] = true
			}
			for _, media := range old.Media {
				if !seenMedia[media.ID] {
					metadata.Media = append(metadata.Media, media)
				}
			}
		}
	}
	encoded, err := json.Marshal(metadata, json.Deterministic(true))
	if err != nil {
		return err
	}
	body := message.Text
	if strings.TrimSpace(body) == "" && len(message.Media) > 0 {
		body = "[" + message.Media[0].Role + "]"
	}
	preview := []rune(body)
	if len(preview) > 100 {
		preview = preview[:100]
	}
	stored := store.Message{SourceID: sourceID, ConversationID: conversationID, SourceMessageID: key, MessageType: SourceType, SentAt: sql.NullTime{Time: message.SentAt, Valid: true}, ReceivedAt: sql.NullTime{Time: message.SentAt, Valid: true}, SenderID: sql.NullInt64{Int64: senderID, Valid: true}, IsFromMe: message.SenderID == opts.Account.UserID, Snippet: sql.NullString{String: string(preview), Valid: body != ""}, SizeEstimate: int64(len(body)), PreserveAttachmentStats: true}
	metaValue := sql.NullString{String: string(encoded), Valid: true}
	messageID, err := imp.store.PersistMessageContext(ctx, &store.MessagePersistData{Message: &stored, Metadata: &metaValue, BodyText: sql.NullString{String: body, Valid: body != ""}, RawMIME: raw, RawFormat: message.RawFormat, PreserveLabels: true, Recipients: []store.RecipientSet{{Type: "from", ParticipantIDs: []int64{senderID}, DisplayNames: []string{message.SenderName}}}, FTS: &store.FTSDoc{Body: body, FromAddr: message.SenderName}})
	if err != nil {
		return err
	}
	if err = imp.store.EnsureConversationParticipant(conversationID, senderID, "member"); err != nil {
		return err
	}
	if !message.EditedAt.IsZero() {
		if err = imp.store.SetMessageEdited(messageID); err != nil {
			return err
		}
	}
	if message.Reactions != nil {
		refs := make([]store.ReactionRef, 0, len(message.Reactions))
		for _, reaction := range message.Reactions {
			if !validID(reaction.UserID) {
				return errors.New("invalid Inline reaction actor")
			}
			id, resolveErr := imp.store.EnsureParticipantByIdentifier(SourceType, fmt.Sprintf("%s:user:%d", ProductionOrigin, reaction.UserID), "")
			if resolveErr != nil {
				return resolveErr
			}
			refs = append(refs, store.ReactionRef{ParticipantID: id, Type: "emoji", Value: reaction.Emoji, CreatedAt: reaction.CreatedAt})
		}
		if err = imp.store.ReplaceReactions(messageID, refs); err != nil {
			return err
		}
	}
	if err = imp.persistMedia(ctx, messageID, conversation, message.Media, opts, sum); err != nil {
		return err
	}
	if err = imp.store.ClearMessageDeletedFromSource(sourceID, key); err != nil {
		return err
	}
	sum.MessagesProcessed++
	if existing[key] > 0 {
		sum.MessagesUpdated++
	} else {
		sum.MessagesAdded++
	}
	return nil
}

func (imp *Importer) repairReplies(ctx context.Context, sourceID int64, chatIDs []int64) error {
	selected := map[int64]bool{}
	for _, id := range chatIDs {
		selected[id] = true
	}
	var after int64
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		rows, err := imp.store.ListUnresolvedMessageRepliesAfter(sourceID, SourceType, after, 100)
		if err != nil {
			return err
		}
		if len(rows) == 0 {
			return nil
		}
		for _, row := range rows {
			after = row.MessageID
			var metadata messageMetadata
			if err = json.Unmarshal([]byte(row.Metadata), &metadata); err != nil {
				return err
			}
			if selected[metadata.ChatID] && metadata.ReplyToMessageID > 0 {
				if err = imp.store.SetReplyTo(sourceID, row.SourceMessageID, messageKey(metadata.ChatID, metadata.ReplyToMessageID)); err != nil {
					return err
				}
			}
		}
	}
}
