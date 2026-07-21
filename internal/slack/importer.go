package slack

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"time"

	"go.kenn.io/msgvault/internal/store"
)

const sourceTypeSlack = "slack"

// checkpointMinInterval throttles checkpoint flushes: the state blob is
// O(conversations + tracked threads) JSON. A variable so tests can disable
// the throttle.
var checkpointMinInterval = 15 * time.Second

const (
	// checkpointPageInterval flushes the sync checkpoint every N pages inside
	// a single conversation backfill (a busy channel can hold years of pages).
	checkpointPageInterval = 10
	// maintenanceRescanWindow bounds the explicit --maintenance rescan.
	maintenanceRescanWindow = 30 * 24 * time.Hour
	// maxRescanPages caps the rescan walk for pathologically busy channels;
	// busier windows are only fully repaired by --full runs.
	maxRescanPages = 10
)

// convScope carries per-conversation state through the persist call chain.
type convScope struct {
	channelID  string
	convID     int64
	sourceID   int64
	syncID     int64
	opts       ImportOptions
	cs         *ConvState
	budgetUsed int
}

// committed is the run's total committed work for this conversation:
// messages actually processed plus the reply forecasts of recorded-but-
// undrained thread debt. Charging the forecast at recording time keeps the
// root walk loosely aligned with thread progress — a --limit budget bounds
// what a run commits to, not just what it has retrieved so far.
func (cc *convScope) committed() int {
	n := cc.budgetUsed
	if cc.cs != nil {
		n += cc.cs.PendingForecast()
	}
	return n
}

func (cc *convScope) limitReached() bool {
	return cc.opts.Limit > 0 && cc.committed() >= cc.opts.Limit
}

// actualsExhausted reports the budget spent on work actually performed. The
// thread drain gates on this rather than limitReached: draining converts
// forecast into actuals (roughly budget-neutral), so outstanding forecast
// must not block paying the very debt it accounts for.
func (cc *convScope) actualsExhausted() bool {
	return cc.opts.Limit > 0 && cc.budgetUsed >= cc.opts.Limit
}

// pageBudget sizes history page requests to the remaining --limit budget
// (net of committed thread debt), so a small limit cannot be overshot by an
// entire 999-message page.
func (cc *convScope) pageBudget() int {
	if cc.opts.Limit <= 0 {
		return historyPageLimit
	}
	remaining := max(cc.opts.Limit-cc.committed(), 1)
	return min(remaining, historyPageLimit)
}

// drainPageBudget sizes thread-drain page requests to the remaining actual
// budget (see actualsExhausted), floored at 2: the response may lead with
// the already-archived parent, and a one-message page holding only the
// parent would advance nothing. The floor bounds the overshoot at one
// message per drain visit.
func (cc *convScope) drainPageBudget() int {
	if cc.opts.Limit <= 0 {
		return historyPageLimit
	}
	remaining := max(cc.opts.Limit-cc.budgetUsed, 2)
	return min(remaining, historyPageLimit)
}

// Importer ingests one Slack workspace user's conversations into the
// msgvault store. One Import run covers one workspace (= one source).
type Importer struct {
	store          *store.Store
	client         *Client
	res            *participantResolver
	lastCheckpoint time.Time
	// now is a clock hook for tests.
	now func() time.Time
	// opts/sourceID scope the current run (the importer is single-threaded;
	// set at the top of Import/BackfillMedia).
	opts     ImportOptions
	sourceID int64
}

// NewImporter creates an Importer backed by the given store and Slack client.
func NewImporter(s *store.Store, c *Client, teamID string) *Importer {
	return &Importer{store: s, client: c, res: newParticipantResolver(s, teamID), now: time.Now}
}

// loadResumeState rebuilds the sync state for a source: the last successful
// run's cursor blob merged with the latest interrupted checkpoint.
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

// Import runs a backfill-then-incremental sync of the workspace user's
// conversations. New conversations backfill their full history (resumable
// across interrupted runs); completed ones fetch only messages newer than
// the stored cursor, then poll tracked threads for late replies.
func (imp *Importer) Import(ctx context.Context, opts ImportOptions) (*ImportSummary, error) {
	start := imp.now()
	if opts.TeamID == "" || opts.UserID == "" {
		return nil, errors.New("slack team and user IDs required")
	}
	src, err := imp.store.GetOrCreateSource(sourceTypeSlack, opts.TeamID+":"+opts.UserID)
	if err != nil {
		return nil, err
	}
	imp.opts, imp.sourceID = opts, src.ID
	sum := &ImportSummary{SourceID: src.ID}

	state := imp.loadResumeState(src.ID)
	if opts.Full {
		state = NewSyncState()
	}

	syncID, err := imp.store.StartSync(src.ID, sourceTypeSlack)
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
	// its first checkpoint, the next run must still find the prior progress.
	imp.checkpointNow(syncID, state, sum)

	// Identity resolution is load-bearing for cross-archive dedup: without
	// the member cache every sender would resolve as a bare ID, splitting
	// people from their mail identities.
	if err = imp.res.loadUsers(ctx, imp.client); err != nil {
		return sum, fmt.Errorf("refresh slack users: %w", err)
	}

	var convs []Conversation
	err = imp.client.AllConversations(ctx, func(c Conversation) error {
		if includeConversation(&c, &opts) {
			convs = append(convs, c)
		}
		return nil
	})
	if err != nil {
		return sum, fmt.Errorf("enumerate slack conversations: %w", err)
	}

	total := len(convs)
	targets := map[string]sweepTarget{}
	for idx := range convs {
		c := &convs[idx]
		if err = ctx.Err(); err != nil {
			return sum, err
		}
		before := sum.MessagesProcessed
		var convID int64
		if convID, err = imp.syncConversation(ctx, syncID, src.ID, c, opts, state, sum); err != nil {
			return sum, err
		}
		if state.EnsureConv(c.ID).Done {
			targets[c.ID] = sweepTarget{convID: convID}
		}
		sum.ConversationsProcessed++
		if opts.Progress != nil {
			opts.Progress(fmt.Sprintf("conversation %d/%d (%s): %d messages",
				idx+1, total, conversationTitle(c, imp.res.displayName), sum.MessagesProcessed-before))
		}
		// Flush checkpoint so an interrupted run resumes from this point.
		imp.checkpoint(syncID, state, sum)
	}

	// Reply sweep: discovers thread replies created since the watermark and
	// archives them canonically. Scoped (--limit) runs skip it, like all
	// steady-state phases; --no-threads skips it explicitly.
	if !opts.NoThreads && opts.Limit == 0 {
		if err = imp.sweepReplies(ctx, syncID, targets, state, sum); err != nil {
			return sum, err
		}
		imp.checkpoint(syncID, state, sum)
	}

	if err = imp.store.RecomputeConversationStats(src.ID); err != nil {
		return sum, err
	}
	// Mid-run checkpoints are throttled, so persist the final counters before
	// completing (CompleteSync only writes status and cursor).
	imp.checkpointNow(syncID, state, sum)
	if sum.FetchErrors > 0 {
		// Fetch failures are isolated so healthy conversations still sync,
		// but the run must remain failed and caller-visible; the checkpoint
		// above preserves all partial progress for the next attempt.
		sum.Duration = imp.now().Sub(start)
		err = fmt.Errorf("partial Slack sync: %d fetch error(s)", sum.FetchErrors)
		return sum, err
	}
	blob, _ := state.Marshal()
	if err = imp.store.CompleteSync(syncID, blob); err != nil {
		return sum, err
	}
	sum.Duration = imp.now().Sub(start)
	return sum, nil
}

// includeConversation applies the channel include/exclude name filters.
// DMs and group DMs are never filtered (the filters exist to skip noisy
// channels, not people).
func includeConversation(c *Conversation, opts *ImportOptions) bool {
	if c.IsIM || c.IsMpim {
		return true
	}
	if slices.Contains(opts.ExcludeChannels, c.Name) {
		return false
	}
	if len(opts.IncludeChannels) == 0 {
		return true
	}
	return slices.Contains(opts.IncludeChannels, c.Name)
}

// syncConversation ensures the conversation row and membership, then
// backfills or incrementally extends its messages. Thread replies are owed
// by the backfill as recorded drain debt (paid before anything else each
// run) and discovered by the reply sweep thereafter.
func (imp *Importer) syncConversation(ctx context.Context, syncID, sourceID int64, c *Conversation, opts ImportOptions, state *SyncState, sum *ImportSummary) (int64, error) {
	convID, err := imp.store.EnsureConversationWithType(sourceID, c.ID, conversationType(c), conversationTitle(c, imp.res.displayName))
	if err != nil {
		return 0, err
	}
	if err := imp.ensureMembership(ctx, syncID, convID, c, opts, sum); err != nil {
		return 0, err
	}

	cs := state.EnsureConv(c.ID)
	cc := &convScope{channelID: c.ID, convID: convID, sourceID: sourceID, syncID: syncID, opts: opts, cs: cs}

	// Pay outstanding thread-drain debt FIRST: the pages that recorded these
	// roots have already advanced past them, so the debt is senior to any
	// new work this run might take on.
	if len(cs.PendingThreads) > 0 && !opts.NoThreads {
		if err := imp.drainPendingThreads(ctx, cc, sum); err != nil {
			return 0, err
		}
	}

	if !cs.Done {
		// One-page invariant: the backfill never fetches a new history page
		// while thread debt is outstanding, which is what keeps the pending
		// list bounded by a single page's roots. --no-threads runs may still
		// page (they record conversation-level debt, never list entries).
		if len(cs.PendingThreads) == 0 || opts.NoThreads {
			if err := imp.backfillConversation(ctx, cc, state, sum); err != nil {
				return 0, err
			}
		}
	} else {
		if err := imp.incrementalConversation(ctx, cc, sum); err != nil {
			return 0, err
		}
		// The maintenance rescan (edits and reaction repair) runs only when
		// explicitly requested: archives ignore post-capture mutations by
		// default. It never charges the fetch budget and is skipped on
		// scoped runs regardless.
		if opts.Maintenance && opts.Limit == 0 {
			if err := imp.rescanHead(ctx, cc, sum); err != nil {
				return 0, err
			}
		}
	}
	// Thread catch-up: an earlier --no-threads backfill left roots without
	// their replies (and the sweep floor postdates them). Runs as soon as the
	// backfill is complete — including the run that completes it — on any
	// unlimited threaded run; the debt clears only after a clean pass.
	if cs.Done && cs.ThreadsPending && !opts.NoThreads && opts.Limit == 0 {
		if err := imp.threadCatchUp(ctx, cc, sum); err != nil {
			return 0, err
		}
	}
	return convID, nil
}

// ensureMembership records the conversation's member list. Membership fetch
// failures are counted but not fatal: message archiving must not be blocked
// by a members listing outage.
func (imp *Importer) ensureMembership(ctx context.Context, syncID, convID int64, c *Conversation, opts ImportOptions, sum *ImportSummary) error {
	var members []store.ConversationParticipantRef
	add := func(userID string) error {
		pid, err := imp.res.resolveID(userID)
		if err != nil {
			return err
		}
		if pid != 0 {
			members = append(members, store.ConversationParticipantRef{ParticipantID: pid, Role: "member"})
		}
		return nil
	}
	if c.IsIM {
		if err := add(c.User); err != nil {
			return err
		}
		if err := add(opts.UserID); err != nil {
			return err
		}
	} else {
		if err := imp.client.AllMembers(ctx, c.ID, add); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if errors.Is(err, ErrNotFound) {
				imp.recordItem(syncID, c.ID, "membership", store.SyncRunItemStatusSkipped, "slack_channel_gone", err)
				return nil
			}
			imp.recordItem(syncID, c.ID, "membership", store.SyncRunItemStatusError, "slack_fetch_error", err)
			sum.Errors++
			return nil
		}
	}
	if err := imp.store.ReplaceConversationParticipants(convID, members); err != nil {
		return err
	}
	return nil
}

// backfillConversation walks the conversation's full history newest→oldest
// via cursor pages, resumable from BackfillCursor. The incremental cursor is
// primed from the newest message of the first page. Fetch errors leave the
// conversation resumable rather than failing the run.
func (imp *Importer) backfillConversation(ctx context.Context, cc *convScope, state *SyncState, sum *ImportSummary) error {
	cs := cc.cs
	// Pin the walk's upper bound BEFORE the first page: page cursors index
	// into the bounded window, so introducing the bound mid-walk would shift
	// the window under an already-issued cursor and skip messages. Messages
	// arriving after the pin are left for the incremental phase.
	if cs.BackfillLatest == "" {
		cs.BackfillLatest = fmt.Sprintf("%d.999999", imp.now().Unix())
	}
	pages := 0
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if cc.limitReached() {
			return nil // resumable: Done stays false
		}
		page, err := imp.client.historyPageWithLimit(ctx, HistoryParams{
			ChannelID: cc.channelID,
			Cursor:    cs.BackfillCursor,
			Latest:    cs.BackfillLatest,
		}, cc.pageBudget())
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if errors.Is(err, ErrNotFound) {
				// Enumerated but unreadable (observed live: a sandbox
				// provisioning-bot DM). There is nothing to fetch — recording
				// it as a hard error would wedge every future run into
				// partial failure.
				imp.recordItem(cc.syncID, cc.channelID, "fetch", store.SyncRunItemStatusSkipped, "slack_channel_gone", err)
				cs.Done = true
				return nil
			}
			imp.recordItem(cc.syncID, cc.channelID, "fetch", store.SyncRunItemStatusError, "slack_fetch_error", err)
			sum.FetchErrors++
			sum.Errors++
			return nil
		}
		// Pages arrive newest-first; the very first message of the walk
		// becomes the incremental cursor once the backfill completes.
		if cs.Cursor == "" && len(page.Messages) > 0 {
			cs.Cursor = page.Messages[0].TS
		}
		for i := range page.Messages {
			if err := imp.processMessage(ctx, cc, &page.Messages[i], sum); err != nil {
				return err
			}
		}
		cc.budgetUsed += len(page.Messages)
		// Record each discovered root as durable thread-drain debt BEFORE the
		// page's cursor advances: "cursor past page" means "page durable and
		// its thread debt recorded". Recording charges the root's
		// reply_count forecast against the budget (see committed), so a run
		// commits to the reply work even when the drain is deferred.
		if cc.opts.NoThreads {
			// Pages consumed threadless leave their roots un-fetched, and
			// the sweep floor postdates those replies: flag the debt for the
			// next threaded run's catch-up walk.
			for i := range page.Messages {
				if page.Messages[i].IsThreadRoot() {
					cs.ThreadsPending = true
					break
				}
			}
		} else {
			for i := range page.Messages {
				m := &page.Messages[i]
				if m.IsThreadRoot() {
					cs.RecordPendingThread(m.TS, m.ReplyCount)
				}
			}
			if err := imp.drainPendingThreads(ctx, cc, sum); err != nil {
				return err
			}
		}
		if !page.HasMore || page.NextCursor == "" {
			cs.Done = true
			cs.BackfillCursor = ""
			return nil
		}
		cs.BackfillCursor = page.NextCursor
		if len(cs.PendingThreads) > 0 {
			// The drain was clipped by the budget or a fetch failure: hold
			// further paging (one-page invariant). The debt and the advanced
			// cursor persist together, so the next run drains first and
			// resumes from the next page — every limited run makes durable
			// progress.
			return nil
		}
		pages++
		if pages%checkpointPageInterval == 0 {
			imp.checkpoint(cc.syncID, state, sum)
		}
	}
}

// drainPendingThreads pays the conversation's recorded thread debt head-
// first. Each thread resumes from its DrainedTo ts (oldest-exclusive), so
// progress is durable at reply granularity and the root is not refetched on
// resume. Fetched messages charge the budget as actuals while decrementing
// the entry's forecast — converting the charge recorded at discovery time
// rather than paying twice. Fetch failures park the entry at its resume
// point and are isolated like all fetch errors; only store/context failures
// abort the run.
func (imp *Importer) drainPendingThreads(ctx context.Context, cc *convScope, sum *ImportSummary) error {
	cs := cc.cs
	if cc.opts.Progress != nil && len(cs.PendingThreads) > 0 {
		cc.opts.Progress(fmt.Sprintf("%s: draining %d owed thread(s), ~%d replies remaining",
			cc.channelID, len(cs.PendingThreads), cs.PendingForecast()))
	}
	for len(cs.PendingThreads) > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		if cc.actualsExhausted() {
			return nil // debt persists; the next run drains first
		}
		pt := &cs.PendingThreads[0]
		oldest := pt.DrainedTo
		if oldest == "" {
			oldest = pt.RootTS // the root itself was archived with its page
		}
		page, err := imp.client.repliesPageWithLimit(ctx, cc.channelID, pt.RootTS, "", oldest, cc.drainPageBudget())
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if errors.Is(err, ErrNotFound) {
				// Thread gone between discovery and drain: expected churn.
				imp.recordItem(cc.syncID, sourceMessageID(cc.channelID, pt.RootTS), "thread", store.SyncRunItemStatusSkipped, "slack_thread_gone", err)
				cs.PendingThreads = cs.PendingThreads[1:]
				continue
			}
			imp.recordItem(cc.syncID, sourceMessageID(cc.channelID, pt.RootTS), "thread", store.SyncRunItemStatusError, "slack_fetch_error", err)
			sum.FetchErrors++
			sum.Errors++
			return nil // entry parked at DrainedTo; retried next run
		}
		progressed := false
		for i := range page.Messages {
			m := &page.Messages[i]
			if err := imp.processMessage(ctx, cc, m, sum); err != nil {
				return err
			}
			cc.budgetUsed++
			if !m.IsThreadReply() {
				continue // the included parent re-upserts harmlessly
			}
			sum.RepliesFetched++
			if pt.DrainedTo == "" || tsLess(pt.DrainedTo, m.TS) {
				pt.DrainedTo = m.TS
				progressed = true
			}
			if pt.Forecast > 0 {
				pt.Forecast--
			}
		}
		if !page.HasMore || page.NextCursor == "" {
			cs.PendingThreads = cs.PendingThreads[1:]
			continue
		}
		if !progressed {
			// More pages claimed but no reply advanced the resume point
			// (defensive: should be impossible with ascending replies).
			// Park rather than loop forever.
			imp.recordItem(cc.syncID, sourceMessageID(cc.channelID, pt.RootTS), "thread", store.SyncRunItemStatusError, "slack_drain_stalled",
				fmt.Errorf("thread %s drain made no progress past %s", pt.RootTS, oldest))
			sum.FetchErrors++
			sum.Errors++
			return nil
		}
	}
	return nil
}

// incrementalConversation fetches top-level messages newer than the stored
// cursor. History pages arrive NEWEST-first, so the ts cursor only advances
// once every page of the window has persisted — advancing it per page would
// let an interruption after page one permanently skip the older pages.
// A window interrupted by --limit or a fetch error checkpoints its page
// cursor (IncrCursor/IncrMaxTS) instead, so limited runs drain a backlog
// across runs rather than restarting from the newest page forever.
func (imp *Importer) incrementalConversation(ctx context.Context, cc *convScope, sum *ImportSummary) error {
	cs := cc.cs
	pageCursor := cs.IncrCursor
	maxTS := cs.IncrMaxTS
	if maxTS == "" {
		maxTS = cs.Cursor
	}
	checkpoint := func() {
		cs.IncrCursor = pageCursor
		cs.IncrMaxTS = maxTS
	}
	for {
		if err := ctx.Err(); err != nil {
			checkpoint()
			return err
		}
		if cc.limitReached() {
			checkpoint() // main cursor not advanced; next run resumes mid-window
			return nil
		}
		page, err := imp.client.historyPageWithLimit(ctx, HistoryParams{
			ChannelID: cc.channelID,
			Cursor:    pageCursor,
			Oldest:    cs.Cursor,
		}, cc.pageBudget())
		if err != nil {
			if ctx.Err() != nil {
				checkpoint()
				return ctx.Err()
			}
			if errors.Is(err, ErrNotFound) {
				// The conversation is gone (left/deleted); the archived
				// messages are kept and there is nothing left to fetch.
				imp.recordItem(cc.syncID, cc.channelID, "fetch", store.SyncRunItemStatusSkipped, "slack_channel_gone", err)
				return nil
			}
			imp.recordItem(cc.syncID, cc.channelID, "fetch", store.SyncRunItemStatusError, "slack_fetch_error", err)
			sum.FetchErrors++
			sum.Errors++
			checkpoint() // next run retries from this page
			return nil
		}
		for i := range page.Messages {
			m := &page.Messages[i]
			if err := imp.processMessage(ctx, cc, m, sum); err != nil {
				return err
			}
			if maxTS == "" || tsLess(maxTS, m.TS) {
				maxTS = m.TS
			}
		}
		cc.budgetUsed += len(page.Messages)
		if !page.HasMore || page.NextCursor == "" {
			cs.Cursor = maxTS // the whole window persisted cleanly
			cs.IncrCursor, cs.IncrMaxTS = "", ""
			return nil
		}
		pageCursor = page.NextCursor
	}
}

// threadCatchUp re-walks a conversation's history fetching ONLY thread
// replies, paying conversation-level thread debt: a --no-threads backfill
// whose pages advanced past roots without fetches, or a non-channel
// conversation recovering a sweep gap. Pure re-read: every persist is an
// upsert. ThreadsPending clears only on a clean pass, so failures retry.
//
// The walk pins its upper bound at its own start time — NOT the original
// backfill pin: gap-recovery debt includes replies to roots created after
// the backfill, which a pin-bounded walk would never anchor. Roots newer
// than the fresh pin need no walk at all (their replies postdate the sweep
// watermark by creation time), and the pin keeps the newest-first
// pagination window stable while the walk runs.
func (imp *Importer) threadCatchUp(ctx context.Context, cc *convScope, sum *ImportSummary) error {
	latest := fmt.Sprintf("%d.999999", imp.now().Unix())
	pageCursor := ""
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		page, err := imp.client.historyPageWithLimit(ctx, HistoryParams{
			ChannelID: cc.channelID,
			Cursor:    pageCursor,
			Latest:    latest,
		}, historyPageLimit)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			imp.recordItem(cc.syncID, cc.channelID, "fetch", store.SyncRunItemStatusError, "slack_fetch_error", err)
			sum.FetchErrors++
			sum.Errors++
			return nil // ThreadsPending stays set; retried next run
		}
		for i := range page.Messages {
			m := &page.Messages[i]
			if !m.IsThreadRoot() {
				continue
			}
			if terr := imp.fetchThread(ctx, cc, m.TS, "", sum); terr != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				imp.recordItem(cc.syncID, sourceMessageID(cc.channelID, m.TS), "thread", store.SyncRunItemStatusError, "slack_fetch_error", terr)
				sum.FetchErrors++
				sum.Errors++
				return nil // ThreadsPending stays set; retried next run
			}
		}
		if !page.HasMore || page.NextCursor == "" {
			cc.cs.ThreadsPending = false
			return nil
		}
		pageCursor = page.NextCursor
	}
}

// fetchThread canonically fetches a whole thread from oldest (exclusive)
// onward, persisting every message (the response's included parent
// re-upserts harmlessly). Its callers — the reply sweep and the thread
// catch-up walk — only run on unlimited syncs; budget-bounded thread
// fetching is the backfill drain's job (see drainPendingThreads).
func (imp *Importer) fetchThread(ctx context.Context, cc *convScope, anchorTS, oldest string, sum *ImportSummary) error {
	pageCursor := ""
	for {
		page, err := imp.client.repliesPageWithLimit(ctx, cc.channelID, anchorTS, pageCursor, oldest, historyPageLimit)
		if err != nil {
			return err
		}
		for i := range page.Messages {
			m := &page.Messages[i]
			if err := imp.processMessage(ctx, cc, m, sum); err != nil {
				return err
			}
			if m.IsThreadReply() {
				sum.RepliesFetched++
			}
		}
		cc.budgetUsed += len(page.Messages)
		if !page.HasMore || page.NextCursor == "" {
			return nil
		}
		pageCursor = page.NextCursor
	}
}

// rescanHead re-pages the conversation's trailing thread-lookback window,
// re-upserting messages in place. It serves two purposes the ts cursor
// cannot (history is keyed by original ts): catching edits and reaction
// changes, and DISCOVERING newly-created thread roots — a first reply to an
// older message never appears in cursor-bounded history, but the re-read
// parent now carries reply_count > 0 and gets tracked for reply polling.
// The window matches the thread lookback so root discovery covers the whole
// tracking period. Its upper bound is the cursor message INCLUSIVE: with
// the default exclusive bounds, edits to the newest archived message would
// stay invisible until a newer message moved the cursor past it.
func (imp *Importer) rescanHead(ctx context.Context, cc *convScope, sum *ImportSummary) error {
	oldest := fmt.Sprintf("%d.000000", imp.now().Add(-maintenanceRescanWindow).Unix())
	if cc.cs.Cursor != "" && tsLess(cc.cs.Cursor, oldest) {
		// Everything newer than the cursor was just fetched by the
		// incremental pass; nothing older than it has been archived yet.
		return nil
	}
	pageCursor := ""
	for range maxRescanPages {
		if err := ctx.Err(); err != nil {
			return err
		}
		// Full pages, no budget interplay: the rescan only runs on unlimited
		// syncs (see syncConversation) and never charges the fetch budget.
		page, err := imp.client.historyPageWithLimit(ctx, HistoryParams{
			ChannelID: cc.channelID,
			Cursor:    pageCursor,
			Oldest:    oldest,
			Latest:    cc.cs.Cursor,
			Inclusive: true,
		}, historyPageLimit)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if errors.Is(err, ErrNotFound) {
				imp.recordItem(cc.syncID, cc.channelID, "fetch", store.SyncRunItemStatusSkipped, "slack_channel_gone", err)
				return nil
			}
			imp.recordItem(cc.syncID, cc.channelID, "fetch", store.SyncRunItemStatusError, "slack_fetch_error", err)
			sum.FetchErrors++
			sum.Errors++
			return nil
		}
		for i := range page.Messages {
			if err := imp.processMessage(ctx, cc, &page.Messages[i], sum); err != nil {
				return err
			}
		}
		if !page.HasMore || page.NextCursor == "" {
			return nil
		}
		pageCursor = page.NextCursor
	}
	return nil
}

// processMessage persists one message and its auxiliary rows. Store-level
// failures are fatal (they indicate DB problems); per-item auxiliary
// failures (FTS, recipients, reactions) are counted but do not abort the run.
func (imp *Importer) processMessage(ctx context.Context, cc *convScope, m *Message, sum *ImportSummary) error {
	if m.Type != "message" || m.TS == "" {
		return nil
	}

	msg, text := mapMessage(m, cc.channelID, cc.convID, cc.sourceID, m.User == cc.opts.UserID, imp.res.displayName)
	var senderPID int64
	var err error
	if m.User != "" {
		senderPID, err = imp.res.resolveID(m.User)
	} else if m.BotID != "" {
		senderPID, err = imp.res.resolveBot(m.BotID, m.Username)
	}
	if err != nil {
		return err
	}
	if senderPID != 0 {
		msg.SenderID = sql.NullInt64{Int64: senderPID, Valid: true}
	}
	messageID, err := imp.store.UpsertMessage(&msg)
	if err != nil {
		return err
	}
	if err := imp.store.UpsertMessageBody(messageID, sql.NullString{String: text, Valid: text != ""}, sql.NullString{}); err != nil {
		return err
	}
	// Archive the exact original message JSON (captured at decode time) so
	// no API field is lost to our partial struct modelling.
	raw := []byte(m.Raw)
	if len(raw) == 0 {
		return fmt.Errorf("slack message %s has no raw JSON archive", msg.SourceMessageID)
	}
	if err := imp.store.UpsertMessageRawWithFormat(messageID, raw, "slack_json"); err != nil {
		return fmt.Errorf("archive slack message raw: %w", err)
	}
	if err := imp.store.UpsertFTS(messageID, "", text, imp.res.displayName(m.User), "", ""); err != nil {
		sum.Errors++
	}
	if m.Edited != nil {
		if err := imp.store.SetMessageEdited(messageID); err != nil {
			sum.Errors++
		}
	}

	imp.persistFiles(ctx, cc.syncID, messageID, m, cc.opts, sum)

	if err := imp.persistMentions(messageID, m, sum); err != nil {
		return err
	}
	if err := imp.persistReactions(messageID, m, sum); err != nil {
		return err
	}

	// Thread replies link to their root by source-message-ID lookup. Roots
	// always reach the archive before or with their replies (history pages
	// carry roots; the replies response carries the root first), and
	// SetReplyTo resolves to NULL harmlessly if one is missing.
	if m.IsThreadReply() {
		if err := imp.store.SetReplyTo(cc.sourceID, sourceMessageID(cc.channelID, m.TS), sourceMessageID(cc.channelID, m.ThreadTS)); err != nil {
			sum.Errors++
		}
	}

	sum.MessagesProcessed++
	sum.MessagesAdded++
	return nil
}

// persistMentions writes "mention" recipient rows. No from/to rows are
// written: sender attribution lives in messages.sender_id and membership in
// conversation_participants (WhatsApp/Beeper precedent).
func (imp *Importer) persistMentions(messageID int64, m *Message, sum *ImportSummary) error {
	var ids []int64
	var names []string
	for _, uid := range m.MentionedUserIDs() {
		pid, err := imp.res.resolveID(uid)
		if err != nil {
			return err
		}
		if pid == 0 {
			continue
		}
		ids = append(ids, pid)
		names = append(names, imp.res.displayName(uid))
	}
	if err := imp.store.ReplaceMessageRecipients(messageID, "mention", ids, names); err != nil {
		sum.Errors++
	}
	return nil
}

// persistReactions replaces the message's reactions from the embedded
// aggregates. Slack reactions carry no timestamp; created_at approximates
// with the target message's timestamp (cosmetic only). The API may truncate
// a reaction's user list on very popular messages — the archived raw JSON
// preserves the counts.
func (imp *Importer) persistReactions(messageID int64, m *Message, sum *ImportSummary) error {
	var reactions []store.ReactionRef
	for _, rc := range m.Reactions {
		for _, uid := range rc.Users {
			pid, err := imp.res.resolveID(uid)
			if err != nil {
				return err
			}
			if pid == 0 {
				continue
			}
			reactions = append(reactions, store.ReactionRef{
				ParticipantID: pid,
				Type:          "emoji",
				Value:         rc.Name,
				CreatedAt:     tsTime(m.TS),
			})
		}
	}
	if err := imp.store.ReplaceReactions(messageID, reactions); err != nil {
		sum.Errors++
	}
	return nil
}

// checkpoint persists the sync state mid-run so an interrupted run resumes.
// Flushes are throttled (see checkpointMinInterval).
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
		MessagesProcessed: int64(sum.MessagesProcessed),
		MessagesAdded:     int64(sum.MessagesAdded),
		ErrorsCount:       int64(sum.Errors),
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

// BackfillMedia retries pending Slack file downloads for one workspace:
// every message that still has a pending marker is re-read from the archived
// raw JSON and its files re-persisted. Idempotent (content-addressed
// storage, replace-by-prefix rows).
func (imp *Importer) BackfillMedia(ctx context.Context, opts ImportOptions) (*ImportSummary, error) {
	start := imp.now()
	if opts.AttachmentsDir == "" {
		return nil, errors.New("attachments dir required")
	}
	src, err := imp.store.GetOrCreateSource(sourceTypeSlack, opts.TeamID+":"+opts.UserID)
	if err != nil {
		return nil, err
	}
	imp.opts, imp.sourceID = opts, src.ID
	sum := &ImportSummary{SourceID: src.ID}
	// This run's sync_runs row becomes the source's newest completed run and
	// Import loads its cursor_after as the resume baseline — carry the
	// existing sync state forward verbatim or the next sync would restart.
	state := imp.loadResumeState(src.ID)
	stateBlob, err := state.Marshal()
	if err != nil {
		return nil, err
	}
	syncID, err := imp.store.StartSync(src.ID, "slack_media")
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			_ = imp.store.FailSync(syncID, err.Error())
		}
	}()
	imp.checkpointNow(syncID, state, sum)

	pending, err := imp.store.ListSlackPendingAttachmentMessages(src.ID)
	if err != nil {
		return sum, err
	}
	for _, item := range pending {
		if err = ctx.Err(); err != nil {
			return sum, err
		}
		raw, rerr := imp.store.GetMessageRaw(item.MessageID)
		if rerr != nil || len(raw) == 0 {
			sum.Errors++
			continue
		}
		var m Message
		if uerr := m.UnmarshalJSON(raw); uerr != nil {
			sum.Errors++
			continue
		}
		imp.persistFiles(ctx, syncID, item.MessageID, &m, opts, sum)
		sum.MessagesProcessed++
	}
	imp.checkpointNow(syncID, state, sum)
	if err = imp.store.CompleteSync(syncID, stateBlob); err != nil {
		return sum, err
	}
	sum.Duration = imp.now().Sub(start)
	return sum, nil
}
