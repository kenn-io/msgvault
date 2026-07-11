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

// errRetryPage aborts the current chat walk without advancing its cursor, so
// the next run re-fetches the same page (upserts make that idempotent).
var errRetryPage = errors.New("retry page next run")

// checkpointMinInterval throttles checkpoint flushes: the state blob is
// O(chats) JSON, so rewriting it after every chat of a large account is
// mostly wasted I/O. Interruption loses at most this much progress.
// A variable so tests can disable the throttle.
var checkpointMinInterval = 15 * time.Second

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

// chatScope carries per-chat state through the persist call chain: the chat
// and store IDs, the run options, and the chat's cursor state (whose
// PendingReplies buffer persistMessage appends to, keeping checkpoints
// consistent with the cursor by construction).
type chatScope struct {
	chatID   string
	convID   int64
	sourceID int64
	syncID   int64
	opts     ImportOptions
	cs       *ChatState
}

// Importer ingests Beeper Desktop messages into the msgvault store. One
// Import run covers one Beeper account (= one msgvault source).
type Importer struct {
	store  *store.Store
	client *Client
	res    *participantResolver
	// lastCheckpoint throttles checkpoint flushes (see checkpointMinInterval).
	lastCheckpoint time.Time
}

// NewImporter creates an Importer backed by the given store and Beeper client.
func NewImporter(s *store.Store, c *Client) *Importer {
	return &Importer{store: s, client: c, res: newParticipantResolver(s)}
}

// loadResumeState rebuilds the sync state for a source: the last successful
// run's cursor blob (baseline) merged with the latest interrupted checkpoint,
// so a resumed run skips already-covered work.
func (imp *Importer) loadResumeState(sourceID int64) *SyncState {
	state := NewSyncState()
	if prev, err := imp.store.GetLastSuccessfulSync(sourceID); err == nil && prev != nil && prev.CursorAfter.Valid {
		if s, lerr := LoadSyncState(prev.CursorAfter.String); lerr == nil {
			state = s
		}
	}
	if cp, err := imp.store.GetLatestCheckpointedSync(sourceID); err == nil && cp != nil && cp.CursorBefore.Valid {
		if cpState, lerr := LoadSyncState(cp.CursorBefore.String); lerr == nil {
			state.Merge(cpState)
		}
	}
	return state
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

	state := imp.loadResumeState(src.ID)
	if opts.Full {
		// Repair path: drop all cursors so every message is re-fetched and
		// upserted in place — but keep the anchors. Skipping their
		// verification would let a --full run against a reinstalled Beeper
		// Desktop (re-assigned message IDs) silently duplicate the archive.
		anchors := state.Anchors
		state = NewSyncState()
		state.Anchors = anchors
	}

	syncID, err := imp.store.StartSync(src.ID, sourceTypeBeeper)
	if err != nil {
		return nil, err
	}
	// Failures below must ASSIGN to err (never shadow it with :=) so this
	// defer records them on the run.
	defer func() {
		if err != nil {
			_ = imp.store.FailSync(syncID, err.Error())
		}
	}()

	// Persist the merged resume state immediately: if this run fails before
	// its first checkpoint (Beeper not running, anchor mismatch), the next run
	// finds it here instead of losing the previous run's progress — the store
	// only exposes the newest run's checkpoint.
	imp.checkpointNow(syncID, state, sum)

	// Message IDs are only unique per Beeper installation; verify the anchor
	// messages still exist unchanged before trusting stored cursors.
	if err = imp.verifyAnchors(ctx, syncID, src.ID, state); err != nil {
		return sum, err
	}

	reconcileCutoff := start.Add(-reconcileWindow)
	chats, err := imp.enumerateChats(ctx, syncID, opts, state, reconcileCutoff, sum)
	if err != nil {
		return sum, err
	}

	// Reconciliation re-walks each active chat's last-24h head to catch
	// in-place edits/deletions/reaction changes the forward-only cursor cannot
	// see. Cheap on re-runs: already-stored media is never re-downloaded.
	maxActivity := parseWatermark(state.ListWatermark)
	total := len(chats)
	for idx := range chats {
		ch := &chats[idx]
		if err = ctx.Err(); err != nil {
			return sum, err
		}
		if ch.LastActivity.After(maxActivity) {
			maxActivity = ch.LastActivity
		}
		var convCount int64
		convCount, err = imp.syncChat(ctx, syncID, src.ID, ch, opts, state, reconcileCutoff, sum)
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
	// Advance the discovery watermark only for fetch-clean runs: a fetch error
	// means some chat's messages are still missing, so it must stay
	// discoverable by the next run's lastActivityAfter filter.
	if sum.FetchErrors == 0 && !maxActivity.IsZero() {
		state.ListWatermark = formatWatermark(maxActivity)
	}

	// Never complete a run under-anchored: incremental-only runs skip the
	// backfill path that normally arms probes, and persisting none would
	// leave the reinstall guard on its slower archived-sample fallback.
	imp.rearmAnchors(ctx, chats, state)

	if err = imp.store.RecomputeConversationStats(src.ID); err != nil {
		return sum, err
	}
	// Mid-run checkpoints are throttled, so persist the final counters before
	// completing (CompleteSync only writes status and cursor).
	imp.checkpointNow(syncID, state, sum)
	blob, _ := state.Marshal()
	if err = imp.store.CompleteSync(syncID, blob); err != nil {
		return sum, err
	}
	sum.Duration = time.Since(start)
	return sum, nil
}

// enumerateChats lists the chats this run must visit: every chat active in
// the discovery overlap or reconciliation window (all chats on first/full
// runs), plus any chat whose backfill is unfinished even without new activity.
func (imp *Importer) enumerateChats(ctx context.Context, syncID int64, opts ImportOptions, state *SyncState, reconcileCutoff time.Time, sum *ImportSummary) ([]Chat, error) {
	params := SearchChatsParams{AccountID: opts.AccountID}
	if !opts.Full && state.ListWatermark != "" {
		if wm := parseWatermark(state.ListWatermark); !wm.IsZero() {
			// Overlap by an hour so clock skew or a mid-listing crash cannot
			// permanently hide a chat from enumeration. Also include every chat
			// inside the reconciliation window so in-place changes are revisited
			// even when their LastActivity did not advance.
			params.LastActivityAfter = wm.Add(-time.Hour)
			if reconcileCutoff.Before(params.LastActivityAfter) {
				params.LastActivityAfter = reconcileCutoff
			}
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
func (imp *Importer) syncChat(ctx context.Context, syncID, sourceID int64, ch *Chat, opts ImportOptions, state *SyncState, reconcileCutoff time.Time, sum *ImportSummary) (int64, error) {
	convID, err := imp.ensureConversation(ctx, syncID, sourceID, ch, sum)
	if err != nil {
		return 0, err
	}

	cs := state.EnsureChat(ch.ID)
	cc := &chatScope{chatID: ch.ID, convID: convID, sourceID: sourceID, syncID: syncID, opts: opts, cs: cs}
	before := sum.MessagesProcessed

	// A chat that was empty when backfilled has Done set but no incremental
	// cursor; re-walk it from scratch (cheap) so its first messages are seen.
	if !cs.Done || cs.Newest == "" {
		err = imp.backfillChat(ctx, cc, state, sum)
	} else {
		err = imp.incrementalChat(ctx, cc, sum)
		if err == nil {
			err = imp.reconcileChat(ctx, cc, reconcileCutoff, sum)
		}
	}
	if err != nil {
		return sum.MessagesProcessed - before, err
	}

	// Reply pairs link parents by lookup, so flushing waits until the
	// backfill has actually archived the parents; until then the pairs ride
	// along in the checkpointed chat state.
	if cs.Done {
		imp.flushReplies(cc, sum)
	}
	return sum.MessagesProcessed - before, nil
}

// ensureConversation upserts the conversation row and its membership,
// fetching the full participant list when the search listing truncated it
// (chat search returns at most 20 participants per chat).
func (imp *Importer) ensureConversation(ctx context.Context, syncID, sourceID int64, ch *Chat, sum *ImportSummary) (int64, error) {
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
	return convID, nil
}

// recentIDWindow remembers the message IDs of the last few pages of a
// backfill walk. The live API's degenerate end-of-history pages re-serve the
// immediately preceding tail, so a few pages of memory detect them — and stay
// bounded on multi-million-message chats.
type recentIDWindow struct {
	pages []map[string]struct{}
	max   int
}

// recentIDWindowPages bounds the duplicate-detection window (see recentIDWindow).
const recentIDWindowPages = 5

func newRecentIDWindow(maxPages int) *recentIDWindow {
	return &recentIDWindow{max: maxPages}
}

func (w *recentIDWindow) contains(id string) bool {
	for _, page := range w.pages {
		if _, ok := page[id]; ok {
			return true
		}
	}
	return false
}

func (w *recentIDWindow) add(pageIDs []string) {
	page := make(map[string]struct{}, len(pageIDs))
	for _, id := range pageIDs {
		page[id] = struct{}{}
	}
	w.pages = append(w.pages, page)
	if len(w.pages) > w.max {
		w.pages = w.pages[1:]
	}
}

// backfillChat walks the chat's history oldest-ward (direction=before) from
// the stored resume cursor until the beginning of locally-available history.
// The incremental cursor (Newest) is primed from the first page so later runs
// can extend forward. Fetch errors leave the chat resumable rather than
// failing the run.
func (imp *Importer) backfillChat(ctx context.Context, cc *chatScope, state *SyncState, sum *ImportSummary) error {
	cs := cc.cs
	pages := 0
	processed := 0
	recent := newRecentIDWindow(recentIDWindowPages)
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
		// The live API does not report hasMore=false at the beginning of
		// history (observed stuck true): exhaustion is signalled by an empty
		// page with null cursors.
		if len(page.Items) == 0 {
			cs.Done = true
			return nil
		}
		// Near the beginning of history the live API degenerates: it re-serves
		// the oldest messages under a synthetic decrementing cursor, still with
		// hasMore=true. Skip items this walk already persisted and treat a page
		// with nothing new as end-of-history.
		pageIDs := make([]string, 0, len(page.Items))
		newItems := 0
		for i := range page.Items {
			m := &page.Items[i]
			pageIDs = append(pageIDs, m.ID)
			if recent.contains(m.ID) {
				continue
			}
			newItems++
			if err := imp.processMessage(ctx, cc, m, false, sum); err != nil {
				return err
			}
			processed++
		}
		recent.add(pageIDs)
		armAnchorFromPage(state, cc.chatID, page.Items)
		if newItems == 0 {
			cs.Done = true
			return nil
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
		if cc.opts.Limit > 0 && processed >= cc.opts.Limit {
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
func (imp *Importer) incrementalChat(ctx context.Context, cc *chatScope, sum *ImportSummary) error {
	cs := cc.cs
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
		// An empty page (null cursors) marks the head; hasMore is not a
		// reliable signal on the live API.
		if len(page.Items) == 0 {
			return nil
		}
		for i := range page.Items {
			if err := imp.processMessage(ctx, cc, &page.Items[i], true, sum); err != nil {
				if errors.Is(err, errRetryPage) {
					return nil // cursor not advanced; next run retries this page
				}
				return err
			}
		}
		// A non-advancing cursor means the API is re-serving the same page
		// (same misbehavior the backfill defends against): stop rather than
		// spin forever.
		if page.NewestCursor == "" || page.NewestCursor == cursor {
			return nil
		}
		cursor = page.NewestCursor
		cs.Newest = cursor
		if !page.HasMore {
			return nil
		}
	}
}

// reconcileChat re-walks the head of the chat (newest-ward pages) re-upserting
// messages newer than cutoff. This catches in-place edits, deletions, and
// reaction changes on recent messages, which the forward-only incremental
// cursor cannot observe. Changes older than the window are only repaired by
// --full runs (documented limitation).
func (imp *Importer) reconcileChat(ctx context.Context, cc *chatScope, cutoff time.Time, sum *ImportSummary) error {
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
			// Targets in this window are re-persisted anyway, refreshing
			// their embedded reactions[], so REACTION events need no refetch.
			if err := imp.processMessage(ctx, cc, m, false, sum); err != nil {
				return err
			}
		}
		if reachedCutoff || len(page.Items) == 0 || !page.HasMore || page.OldestCursor == "" {
			return nil
		}
		cursor, direction = page.OldestCursor, "before"
	}
	return nil
}

// processMessage routes one message event: REACTION events refresh their
// target (when refetchReactionTarget is set), deletions tombstone, hidden
// events are skipped, and everything else persists.
func (imp *Importer) processMessage(ctx context.Context, cc *chatScope, m *Message, refetchReactionTarget bool, sum *ImportSummary) error {
	if m.Type == "REACTION" {
		// The target message's embedded reactions[] are the authoritative
		// current state. Backfill and reconcile walks visit the target
		// anyway; only the incremental walk must refetch it (the target is
		// older than its cursor).
		if !refetchReactionTarget || m.LinkedMessageID == "" {
			return nil
		}
		return imp.refreshReactionTarget(ctx, cc, m, sum)
	}
	if m.IsDeleted {
		if err := imp.store.MarkMessageDeleted(cc.sourceID, m.ID); err != nil {
			sum.Errors++
		}
		sum.MessagesProcessed++
		return nil
	}
	if m.IsHidden {
		return nil
	}
	err := imp.persistMessage(ctx, cc, m, sum)
	if err == nil {
		sum.MessagesProcessed++
	}
	return err
}

// refreshReactionTarget re-fetches and re-persists the message a REACTION
// event points at, refreshing its embedded reactions (and any edit). A 404
// target is expected churn; other fetch failures return errRetryPage so the
// incremental cursor does not advance past the event — the reaction would
// otherwise be lost, since its target is outside the reconcile window.
func (imp *Importer) refreshReactionTarget(ctx context.Context, cc *chatScope, m *Message, sum *ImportSummary) error {
	target, err := imp.client.GetMessage(ctx, cc.chatID, m.LinkedMessageID)
	if errors.Is(err, ErrNotFound) {
		imp.recordItem(cc.syncID, m.ID, "reaction", store.SyncRunItemStatusSkipped, "beeper_reaction_target_missing", err)
		return nil
	}
	if err != nil {
		imp.recordItem(cc.syncID, m.ID, "reaction", store.SyncRunItemStatusError, "beeper_fetch_error", err)
		sum.FetchErrors++
		sum.Errors++
		return errRetryPage
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
	if err := imp.persistMessage(ctx, cc, target, sum); err != nil {
		return err
	}
	sum.ReactionsRefreshed++
	return nil
}

// persistMessage writes a single message via the granular store path.
// Store-level failures are fatal (they indicate DB problems, not item churn);
// per-item auxiliary failures (FTS, recipients, reactions) are counted and
// recorded but do not abort the run.
func (imp *Importer) persistMessage(ctx context.Context, cc *chatScope, m *Message, sum *ImportSummary) error {
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

	if !cc.opts.NoMedia && cc.opts.AttachmentsDir != "" {
		imp.persistAttachments(ctx, cc.syncID, messageID, m, cc.opts, sum)
	}

	if err := imp.persistMentions(messageID, m, sum); err != nil {
		return err
	}
	if err := imp.persistReactions(messageID, m, sum); err != nil {
		return err
	}

	// Pages arrive newest-first, so a reply can precede its parent even
	// within one page; buffer all pairs in the (checkpointed) chat state and
	// link after the walk.
	if m.LinkedMessageID != "" {
		cc.cs.PendingReplies = append(cc.cs.PendingReplies, [2]string{m.ID, m.LinkedMessageID})
	}

	sum.MessagesAdded++
	return nil
}

// persistMentions writes "mention" recipient rows. No from/to rows are
// written: sender attribution lives in messages.sender_id and membership in
// conversation_participants (WhatsApp-importer precedent), which avoids a
// messages × group-size row explosion.
func (imp *Importer) persistMentions(messageID int64, m *Message, sum *ImportSummary) error {
	var ids []int64
	seen := map[int64]struct{}{}
	for _, uid := range m.Mentions {
		if uid == "" || uid == "@room" {
			continue
		}
		pid, err := imp.res.resolveID(uid, "")
		if err != nil {
			return err
		}
		if _, dup := seen[pid]; pid == 0 || dup {
			continue
		}
		seen[pid] = struct{}{}
		ids = append(ids, pid)
	}
	if err := imp.store.ReplaceMessageRecipients(messageID, "mention", ids, make([]string, len(ids))); err != nil {
		sum.Errors++
	}
	return nil
}

// persistReactions replaces the message's reactions from the embedded set.
// Embedded reactions carry no timestamp; created_at approximates with the
// target message's timestamp (cosmetic only).
func (imp *Importer) persistReactions(messageID int64, m *Message, sum *ImportSummary) error {
	reactions := make([]store.ReactionRef, 0, len(m.Reactions))
	for _, rc := range m.Reactions {
		pid, err := imp.res.resolveID(rc.ParticipantID, "")
		if err != nil {
			return err
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
	return nil
}

// flushReplies links the chat's buffered reply pairs now that both sides of
// each pair have been archived. SetReplyTo is idempotent; pairs whose parents
// are beyond locally-available history resolve to NULL.
func (imp *Importer) flushReplies(cc *chatScope, sum *ImportSummary) {
	for _, pr := range cc.cs.PendingReplies {
		if err := imp.store.SetReplyTo(cc.sourceID, pr[0], pr[1]); err != nil {
			sum.Errors++
		}
	}
	cc.cs.PendingReplies = nil
}

// checkpoint persists the sync state mid-run so an interrupted run resumes.
// Flushes are throttled — the blob is O(chats) JSON (see checkpointMinInterval).
func (imp *Importer) checkpoint(syncID int64, state *SyncState, sum *ImportSummary) {
	if time.Since(imp.lastCheckpoint) < checkpointMinInterval {
		return
	}
	imp.checkpointNow(syncID, state, sum)
}

// checkpointNow persists the sync state unconditionally: for the initial
// resume-state write and the final counters, which must never be skipped.
func (imp *Importer) checkpointNow(syncID int64, state *SyncState, sum *ImportSummary) {
	blob, err := state.Marshal()
	if err != nil {
		return
	}
	if imp.store.UpdateSyncCheckpoint(syncID, &store.Checkpoint{
		PageToken:         blob,
		MessagesProcessed: sum.MessagesProcessed,
		MessagesAdded:     sum.MessagesAdded,
		ErrorsCount:       sum.Errors,
	}) == nil {
		imp.lastCheckpoint = time.Now()
	}
}

// recordItem records a per-item outcome on the sync run.
func (imp *Importer) recordItem(syncID int64, sourceMessageID, phase, status, kind string, err error) {
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
