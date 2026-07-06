package beeper

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"go.kenn.io/msgvault/internal/store"
)

const sourceTypeBeeper = "beeper"

const (
	// checkpointPageInterval flushes the sync checkpoint every N pages inside a
	// single chat backfill. Per-chat-only checkpointing is insufficient here:
	// one chat can hold over a million messages (tens of thousands of pages).
	checkpointPageInterval = 25
	// reconcileWindow bounds the head re-walk that catches in-place edits,
	// deletions, and reaction changes the incremental cursor cannot see.
	reconcileWindow = 24 * time.Hour
	// maxReconcilePages caps the reconciliation walk for pathologically busy chats.
	maxReconcilePages = 50
)

// replyPair links a reply to its parent by source message ID.
type replyPair struct {
	child  string
	parent string
}

// chatContext carries per-chat state through the persist call chain.
type chatContext struct {
	chatID   string
	convID   int64
	sourceID int64
	syncID   int64
	replies  []replyPair
	// persisted collects internal message IDs for embedding enqueue.
	persisted []int64
}

// Importer ingests Beeper Desktop messages into the msgvault store. One
// Import run covers one Beeper account (= one msgvault source).
type Importer struct {
	store  *store.Store
	client *Client
	res    *participantResolver
}

// NewImporter creates an Importer backed by the given store and Beeper client.
func NewImporter(s *store.Store, c *Client) *Importer {
	return &Importer{store: s, client: c, res: newParticipantResolver(s)}
}

// Import runs a backfill-then-incremental sync of opts.AccountID's chats.
// New chats backfill their full locally-available history (resumable across
// interrupted runs); completed chats fetch only messages newer than the
// stored cursor. Returns a summary of the run.
func (imp *Importer) Import(ctx context.Context, opts ImportOptions) (*ImportSummary, error) {
	start := time.Now()
	if opts.AccountID == "" {
		return nil, errors.New("beeper account ID required")
	}
	src, err := imp.store.GetOrCreateSource(sourceTypeBeeper, opts.AccountID)
	if err != nil {
		return nil, err
	}
	sum := &ImportSummary{SourceID: src.ID}

	// Build the starting SyncState by merging the last successful sync's cursor
	// (baseline) with the latest interrupted checkpoint (if any), so a resumed
	// run skips already-covered work. opts.Full skips this entirely so every
	// message is re-fetched and re-persisted (repair path).
	state := NewSyncState()
	if !opts.Full {
		if prev, perr := imp.store.GetLastSuccessfulSync(src.ID); perr == nil && prev != nil && prev.CursorAfter.Valid {
			if s, lerr := LoadSyncState(prev.CursorAfter.String); lerr == nil {
				state = s
			}
		}
		if cp, cerr := imp.store.GetLatestCheckpointedSync(src.ID); cerr == nil && cp != nil && cp.CursorBefore.Valid {
			if cpState, lerr := LoadSyncState(cp.CursorBefore.String); lerr == nil {
				state.Merge(cpState)
			}
		}
	}

	syncID, err := imp.store.StartSync(src.ID, sourceTypeBeeper)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			_ = imp.store.FailSync(syncID, err.Error())
		}
	}()

	// Message IDs are only unique per Beeper installation; verify the anchor
	// message still exists unchanged before trusting stored cursors.
	if err = imp.verifyAnchor(ctx, state.Anchor); err != nil {
		return sum, err
	}

	chats, err := imp.enumerateChats(ctx, syncID, opts, state, sum)
	if err != nil {
		return sum, err
	}

	maxActivity := parseWatermark(state.ListWatermark)
	total := len(chats)
	for idx := range chats {
		ch := &chats[idx]
		if err = ctx.Err(); err != nil {
			return sum, err
		}
		if opts.ChatID != "" && ch.ID != opts.ChatID {
			continue
		}
		if ch.LastActivity.After(maxActivity) {
			maxActivity = ch.LastActivity
		}
		var convCount int64
		convCount, err = imp.syncChat(ctx, syncID, src.ID, ch, opts, state, sum)
		if err != nil {
			return sum, err
		}
		sum.ChatsProcessed++
		if opts.Progress != nil {
			opts.Progress(fmt.Sprintf("chat %d/%d (%s): %d messages", idx+1, total, ch.Network, convCount))
		}
		// Flush checkpoint so an interrupted run can resume from this point.
		imp.checkpoint(syncID, state, sum)
	}
	// Advance the discovery watermark only for unscoped, fetch-clean runs: a
	// scoped run never visited the other chats, and a fetch error means some
	// chat's messages are still missing — both must stay discoverable.
	if opts.ChatID == "" && sum.FetchErrors == 0 && !maxActivity.IsZero() {
		state.ListWatermark = formatWatermark(maxActivity)
	}

	if err = imp.store.RecomputeConversationStats(src.ID); err != nil {
		return sum, err
	}
	blob, _ := state.Marshal()
	if err = imp.store.CompleteSync(syncID, blob); err != nil {
		return sum, err
	}
	sum.Duration = time.Since(start)
	return sum, nil
}

// verifyAnchor re-fetches the anchor probe message and fails when it is gone
// or its timestamp changed, indicating the installation re-assigned message
// IDs (reinstall/re-index). Failing fast prevents silently duplicating the
// whole archive under new IDs.
func (imp *Importer) verifyAnchor(ctx context.Context, a *AnchorProbe) error {
	if a == nil {
		return nil
	}
	m, err := imp.client.GetMessage(ctx, a.ChatID, a.MessageID)
	if errors.Is(err, ErrNotFound) {
		return fmt.Errorf("beeper message IDs appear re-assigned (Beeper Desktop reinstall or re-index?): anchor message %s no longer exists; remove and re-add this account to avoid duplicate archives", a.MessageID)
	}
	if err != nil {
		return fmt.Errorf("verify beeper sync anchor: %w", err)
	}
	if !m.Timestamp.Equal(a.Timestamp) {
		return fmt.Errorf("beeper message IDs appear re-assigned (Beeper Desktop reinstall or re-index?): anchor message %s changed timestamp; remove and re-add this account to avoid duplicate archives", a.MessageID)
	}
	return nil
}

// enumerateChats lists the chats this run must visit: every chat with
// activity after the watermark (all chats on first/full runs), plus any chat
// whose backfill is unfinished even without new activity.
func (imp *Importer) enumerateChats(ctx context.Context, syncID int64, opts ImportOptions, state *SyncState, sum *ImportSummary) ([]Chat, error) {
	params := SearchChatsParams{AccountID: opts.AccountID}
	if !opts.Full && state.ListWatermark != "" {
		if wm := parseWatermark(state.ListWatermark); !wm.IsZero() {
			// Overlap by an hour so clock skew or a mid-listing crash cannot
			// permanently hide a chat from enumeration.
			params.LastActivityAfter = wm.Add(-time.Hour)
		}
	}
	var chats []Chat
	seen := map[string]bool{}
	err := imp.client.AllChats(ctx, params, func(ch Chat) error {
		seen[ch.ID] = true
		chats = append(chats, ch)
		return nil
	})
	if err != nil {
		return nil, err
	}
	for chatID, cs := range state.Chats {
		if cs == nil || cs.Done || seen[chatID] {
			continue
		}
		detail, gerr := imp.client.GetChat(ctx, chatID)
		if errors.Is(gerr, ErrNotFound) {
			// The chat no longer exists in Beeper (left/deleted); there is
			// nothing more to fetch. Mark it complete so it stops pinning the
			// discovery watermark; the archived messages are kept.
			cs.Done = true
			imp.recordItem(syncID, chatID, "fetch", store.SyncRunItemStatusSkipped, "beeper_chat_gone", gerr)
			continue
		}
		if gerr != nil {
			imp.recordItem(syncID, chatID, "fetch", store.SyncRunItemStatusError, "beeper_fetch_error", gerr)
			sum.FetchErrors++
			sum.Errors++
			continue
		}
		chats = append(chats, *detail)
	}
	return chats, nil
}

// syncChat ensures the conversation and its participants, then backfills or
// incrementally extends the chat's messages. Returns the number of messages
// processed for this chat.
func (imp *Importer) syncChat(ctx context.Context, syncID, sourceID int64, ch *Chat, opts ImportOptions, state *SyncState, sum *ImportSummary) (int64, error) {
	// Chat search returns at most 20 participants per chat; fetch the full
	// list only when it is truncated.
	detail := ch
	if ch.Participants.HasMore {
		d, gerr := imp.client.GetChat(ctx, ch.ID)
		if gerr != nil {
			imp.recordItem(syncID, ch.ID, "fetch", store.SyncRunItemStatusError, "beeper_fetch_error", gerr)
			sum.FetchErrors++
			sum.Errors++
		} else {
			detail = d
		}
	}
	convID, err := imp.store.EnsureConversationWithType(sourceID, ch.ID, conversationType(ch.Type), ch.Title)
	if err != nil {
		return 0, err
	}
	for i := range detail.Participants.Items {
		p := &detail.Participants.Items[i]
		pid, rerr := imp.res.resolveUser(&p.User)
		if rerr != nil {
			return 0, rerr
		}
		if pid == 0 {
			continue
		}
		role := "member"
		if p.IsAdmin {
			role = "admin"
		}
		if cerr := imp.store.EnsureConversationParticipant(convID, pid, role); cerr != nil {
			sum.Errors++
		}
	}

	cc := &chatContext{chatID: ch.ID, convID: convID, sourceID: sourceID, syncID: syncID}
	cs := state.Chat(ch.ID)
	before := sum.MessagesProcessed

	// A chat that was empty when backfilled has Done set but no incremental
	// cursor; re-walk it from scratch (cheap) so its first messages are seen.
	if !cs.Done || cs.Newest == "" {
		err = imp.backfillChat(ctx, cc, cs, opts, state, sum)
	} else {
		err = imp.incrementalChat(ctx, cc, cs, sum)
		if err == nil {
			err = imp.reconcileChat(ctx, cc, sum)
		}
	}
	if err != nil {
		return sum.MessagesProcessed - before, err
	}

	imp.flushReplies(cc, sum)
	imp.enqueueEmbeddings(ctx, opts, sum, cc.persisted)
	return sum.MessagesProcessed - before, nil
}

// backfillChat walks the chat's history oldest-ward (direction=before) from
// the stored resume cursor until the beginning of locally-available history.
// The incremental cursor (Newest) is primed from the first page so later runs
// can extend forward. Fetch errors leave the chat resumable rather than
// failing the run.
func (imp *Importer) backfillChat(ctx context.Context, cc *chatContext, cs *ChatState, opts ImportOptions, state *SyncState, sum *ImportSummary) error {
	pages := 0
	processed := 0
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		cursor, direction := cs.Oldest, "before"
		if cursor == "" {
			direction = "" // first page: newest messages
		}
		page, err := imp.client.ListMessagesPage(ctx, cc.chatID, cursor, direction)
		if err != nil {
			imp.recordItem(cc.syncID, cc.chatID, "fetch", store.SyncRunItemStatusError, "beeper_fetch_error", err)
			sum.FetchErrors++
			sum.Errors++
			return nil
		}
		if cs.Newest == "" && page.NewestCursor != "" {
			cs.Newest = page.NewestCursor
		}
		for i := range page.Items {
			if err := imp.processMessage(ctx, cc, &page.Items[i], false, sum); err != nil {
				return err
			}
			processed++
		}
		if state.Anchor == nil {
			state.Anchor = anchorFrom(cc.chatID, page.Items)
		}
		if page.OldestCursor != "" {
			cs.Oldest = page.OldestCursor
		}
		pages++
		if pages%checkpointPageInterval == 0 {
			imp.checkpoint(cc.syncID, state, sum)
		}
		if !page.HasMore {
			cs.Done = true
			return nil
		}
		if opts.Limit > 0 && processed >= opts.Limit {
			return nil // resumable: Done stays false
		}
		if page.OldestCursor == "" {
			// Defensive: hasMore without a cursor would loop on the same page.
			return nil
		}
	}
}

// incrementalChat fetches messages newer than the stored cursor
// (direction=after, oldest→newest) and advances the cursor.
func (imp *Importer) incrementalChat(ctx context.Context, cc *chatContext, cs *ChatState, sum *ImportSummary) error {
	cursor := cs.Newest
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		page, err := imp.client.ListMessagesPage(ctx, cc.chatID, cursor, "after")
		if err != nil {
			imp.recordItem(cc.syncID, cc.chatID, "fetch", store.SyncRunItemStatusError, "beeper_fetch_error", err)
			sum.FetchErrors++
			sum.Errors++
			return nil
		}
		for i := range page.Items {
			if err := imp.processMessage(ctx, cc, &page.Items[i], true, sum); err != nil {
				return err
			}
		}
		if page.NewestCursor != "" {
			cursor = page.NewestCursor
			cs.Newest = cursor
		}
		if !page.HasMore || page.NewestCursor == "" {
			return nil
		}
	}
}

// reconcileChat re-walks the head of the chat (newest-ward pages) re-upserting
// messages from the last reconcileWindow. This catches in-place edits,
// deletions, and reaction changes on recent messages, which the forward-only
// incremental cursor cannot observe. Changes older than the window are only
// repaired by --full runs (documented limitation).
func (imp *Importer) reconcileChat(ctx context.Context, cc *chatContext, sum *ImportSummary) error {
	cutoff := time.Now().Add(-reconcileWindow)
	cursor, direction := "", ""
	for range maxReconcilePages {
		if err := ctx.Err(); err != nil {
			return err
		}
		page, err := imp.client.ListMessagesPage(ctx, cc.chatID, cursor, direction)
		if err != nil {
			imp.recordItem(cc.syncID, cc.chatID, "fetch", store.SyncRunItemStatusError, "beeper_fetch_error", err)
			sum.FetchErrors++
			sum.Errors++
			return nil
		}
		reachedCutoff := false
		for i := range page.Items {
			m := &page.Items[i]
			if m.Timestamp.Before(cutoff) {
				reachedCutoff = true
				continue
			}
			// Backfill mode: embedded reactions[] on each message are
			// authoritative here, so REACTION events need no target refetch.
			if err := imp.processMessage(ctx, cc, m, false, sum); err != nil {
				return err
			}
		}
		if reachedCutoff || !page.HasMore || page.OldestCursor == "" {
			return nil
		}
		cursor, direction = page.OldestCursor, "before"
	}
	return nil
}

// processMessage routes one message event: REACTION events refresh their
// target (incremental only), deletions tombstone, hidden events are skipped,
// and everything else persists.
func (imp *Importer) processMessage(ctx context.Context, cc *chatContext, m *Message, incremental bool, sum *ImportSummary) error {
	if m.Type == "REACTION" {
		// The target message's embedded reactions[] are the authoritative
		// current state. During backfill/reconcile the target is visited
		// anyway; during incremental the target is older than the cursor, so
		// refetch and re-persist it (refreshing reactions and any edit).
		if !incremental || m.LinkedMessageID == "" {
			return nil
		}
		target, err := imp.client.GetMessage(ctx, cc.chatID, m.LinkedMessageID)
		if err != nil {
			imp.recordItem(cc.syncID, m.ID, "reaction", store.SyncRunItemStatusSkipped, "beeper_reaction_target_missing", err)
			return nil
		}
		if target.IsDeleted {
			if derr := imp.store.MarkMessageDeleted(cc.sourceID, target.ID); derr != nil {
				sum.Errors++
			}
			return nil
		}
		if target.IsHidden || target.Type == "REACTION" {
			return nil
		}
		if err := imp.persistMessage(cc, target, true, sum); err != nil {
			return err
		}
		sum.ReactionsRefreshed++
		return nil
	}
	if m.IsDeleted {
		if err := imp.store.MarkMessageDeleted(cc.sourceID, m.ID); err != nil {
			sum.Errors++
		}
		sum.MessagesProcessed++
		return nil
	}
	if m.IsHidden {
		sum.HiddenSkipped++
		return nil
	}
	err := imp.persistMessage(cc, m, incremental, sum)
	if err == nil {
		sum.MessagesProcessed++
	}
	return err
}

// persistMessage writes a single message via the granular store path.
// Store-level failures are fatal (they indicate DB problems, not item churn);
// per-item auxiliary failures (FTS, recipients, reactions) are counted and
// recorded but do not abort the run.
func (imp *Importer) persistMessage(cc *chatContext, m *Message, incremental bool, sum *ImportSummary) error {
	msg, text := mapMessage(m, cc.convID, cc.sourceID)
	senderPID, err := imp.res.resolveID(m.SenderID, m.SenderName)
	if err != nil {
		return err
	}
	if senderPID != 0 {
		msg.SenderID = sql.NullInt64{Int64: senderPID, Valid: true}
		if cerr := imp.store.EnsureConversationParticipant(cc.convID, senderPID, "member"); cerr != nil {
			sum.Errors++
		}
	}
	messageID, err := imp.store.UpsertMessage(&msg)
	if err != nil {
		return err
	}
	if err := imp.store.UpsertMessageBody(messageID, sql.NullString{String: text, Valid: text != ""}, sql.NullString{}); err != nil {
		return err
	}
	// Archive the exact original message JSON. m.Raw is captured verbatim at
	// decode time (Message.UnmarshalJSON) so it preserves every API field
	// including ones we do not model; fall back to re-marshalling only if a
	// message was constructed without going through a decode.
	raw := []byte(m.Raw)
	if len(raw) == 0 {
		marshaled, merr := json.Marshal(m)
		if merr != nil {
			return fmt.Errorf("marshal beeper message raw archive: %w", merr)
		}
		raw = marshaled
	}
	if err := imp.store.UpsertMessageRawWithFormat(messageID, raw, "beeper_json"); err != nil {
		return fmt.Errorf("archive beeper message raw: %w", err)
	}
	if err := imp.store.UpsertFTS(messageID, "", text, m.SenderName, "", ""); err != nil {
		sum.Errors++
	}
	if m.EditedTimestamp != nil && !m.EditedTimestamp.IsZero() {
		if err := imp.store.SetMessageEdited(messageID); err != nil {
			sum.Errors++
		}
	}

	// Mentions become "mention" recipient rows. No from/to rows are written:
	// sender attribution lives in messages.sender_id and membership in
	// conversation_participants (WhatsApp-importer precedent), which avoids a
	// messages × group-size row explosion.
	var mentionIDs []int64
	var mentionNames []string
	for _, uid := range m.Mentions {
		if uid == "" || uid == "@room" {
			continue
		}
		pid, rerr := imp.res.resolveID(uid, "")
		if rerr != nil {
			return rerr
		}
		if pid == 0 {
			continue
		}
		mentionIDs = append(mentionIDs, pid)
		mentionNames = append(mentionNames, "")
	}
	mentionIDs, mentionNames = dedupRecipients(mentionIDs, mentionNames)
	if err := imp.store.ReplaceMessageRecipients(messageID, "mention", mentionIDs, mentionNames); err != nil {
		sum.Errors++
	}

	// Embedded reactions carry no timestamp; approximate created_at with the
	// target message's timestamp (cosmetic only).
	reactions := make([]store.ReactionRef, 0, len(m.Reactions))
	for _, rc := range m.Reactions {
		pid, rerr := imp.res.resolveID(rc.ParticipantID, "")
		if rerr != nil {
			return rerr
		}
		if pid == 0 {
			continue
		}
		typ := "key"
		if rc.Emoji {
			typ = "emoji"
		}
		reactions = append(reactions, store.ReactionRef{
			ParticipantID: pid,
			Type:          typ,
			Value:         rc.ReactionKey,
			CreatedAt:     m.Timestamp,
		})
	}
	if err := imp.store.ReplaceReactions(messageID, reactions); err != nil {
		sum.Errors++
	}

	if m.LinkedMessageID != "" {
		if incremental {
			// Incremental messages reply to older, already-archived parents.
			if err := imp.store.SetReplyTo(cc.sourceID, m.ID, m.LinkedMessageID); err != nil {
				sum.Errors++
			}
		} else {
			// Backfill walks newest→oldest, so the parent is not yet
			// archived; buffer and link after the walk.
			cc.replies = append(cc.replies, replyPair{child: m.ID, parent: m.LinkedMessageID})
		}
	}

	cc.persisted = append(cc.persisted, messageID)
	sum.MessagesAdded++
	return nil
}

// flushReplies links buffered reply pairs now that both sides of each pair
// have been archived. SetReplyTo is idempotent; pairs whose parents are
// beyond locally-available history resolve to NULL.
func (imp *Importer) flushReplies(cc *chatContext, sum *ImportSummary) {
	for _, rp := range cc.replies {
		if err := imp.store.SetReplyTo(cc.sourceID, rp.child, rp.parent); err != nil {
			sum.Errors++
		}
	}
	cc.replies = cc.replies[:0]
}

func (imp *Importer) enqueueEmbeddings(ctx context.Context, opts ImportOptions, sum *ImportSummary, messageIDs []int64) {
	if opts.EmbedEnqueuer == nil || len(messageIDs) == 0 {
		return
	}
	if err := opts.EmbedEnqueuer.EnqueueMessages(ctx, messageIDs); err != nil {
		sum.Errors++
	}
}

// checkpoint persists the sync state mid-run so an interrupted run resumes.
func (imp *Importer) checkpoint(syncID int64, state *SyncState, sum *ImportSummary) {
	blob, err := state.Marshal()
	if err != nil {
		return
	}
	_ = imp.store.UpdateSyncCheckpoint(syncID, &store.Checkpoint{
		PageToken:         blob,
		MessagesProcessed: sum.MessagesProcessed,
		MessagesAdded:     sum.MessagesAdded,
		ErrorsCount:       sum.Errors,
	})
}

// recordItem records a per-item outcome on the sync run. syncID 0 (item
// failures before the run row exists) is skipped.
func (imp *Importer) recordItem(syncID int64, sourceMessageID, phase, status, kind string, err error) {
	if syncID == 0 {
		return
	}
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	_ = imp.store.RecordSyncRunItem(store.SyncRunItem{
		SyncRunID:       syncID,
		SourceMessageID: sourceMessageID,
		Phase:           phase,
		Status:          status,
		ErrorKind:       kind,
		ErrorMessage:    msg,
	})
}

// anchorFrom picks a stable anchor message from a page: a regular content
// message (not a reaction, tombstone, or hidden event), preferring the newest.
func anchorFrom(chatID string, items []Message) *AnchorProbe {
	var best *Message
	for i := range items {
		m := &items[i]
		if m.Type == "REACTION" || m.IsDeleted || m.IsHidden {
			continue
		}
		if best == nil || m.Timestamp.After(best.Timestamp) {
			best = m
		}
	}
	if best == nil {
		return nil
	}
	return &AnchorProbe{ChatID: chatID, MessageID: best.ID, Timestamp: best.Timestamp}
}

// dedupRecipients removes duplicate participant IDs from ids/names slices,
// preserving first-seen order and skipping zero IDs.
func dedupRecipients(ids []int64, names []string) ([]int64, []string) {
	seen := make(map[int64]struct{}, len(ids))
	outIDs := make([]int64, 0, len(ids))
	outNames := make([]string, 0, len(ids))
	for i, id := range ids {
		if id == 0 {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		outIDs = append(outIDs, id)
		n := ""
		if i < len(names) {
			n = names[i]
		}
		outNames = append(outNames, n)
	}
	return outIDs, outNames
}

func parseWatermark(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

// formatWatermark renders a fixed-width UTC RFC3339 string so watermarks are
// order-comparable as strings (see SyncState.Merge).
func formatWatermark(t time.Time) string {
	return t.UTC().Truncate(time.Second).Format(time.RFC3339)
}
