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
		// --full starts a repair SESSION, not a one-shot: the reset happens
		// once, stamped with a new state generation (see Merge), and the
		// session persists until every conversation's walk completes and
		// all thread debt is paid. A --full while a repair is already in
		// flight therefore CONTINUES it — interrupted and --limit-scoped
		// repairs converge across runs of any kind instead of restarting
		// at the newest page forever.
		if !state.RepairPending {
			gen := state.Generation + 1
			state = NewSyncState()
			state.Generation = gen
			state.RepairPending = true
		}
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
	// archives them canonically. Limited runs participate with a work
	// budget — certification parks safely when it runs out, so standing
	// --limit schedules still converge on reply discovery. --no-threads
	// skips it explicitly.
	if !opts.NoThreads {
		if err = imp.sweepReplies(ctx, syncID, targets, state, sum); err != nil {
			return sum, err
		}
		imp.checkpoint(syncID, state, sum)
	}

	if err = imp.store.RecomputeConversationStats(src.ID); err != nil {
		return sum, err
	}
	// A repair session ends only on a clean pass that leaves nothing owed
	// among the conversations this run could actually reach.
	if state.RepairPending && sum.FetchErrors == 0 {
		eligible := make(map[string]bool, len(convs))
		for i := range convs {
			eligible[convs[i].ID] = true
		}
		if state.RepairComplete(eligible) {
			state.RepairPending = false
		}
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

// syncConversation ensures the conversation row and membership, then walks
// the conversation's next pinned window (the initial backfill and every
// incremental fetch are the same walk). Thread replies are owed by the
// walks as recorded drain debt (paid before anything else each run) and
// discovered by the reply sweep thereafter.
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

	// One-page invariant: a walk never fetches a new history page while
	// thread debt is outstanding, which is what keeps the pending list
	// bounded by a single page's roots. --no-threads runs may still page
	// (they record conversation-level debt, never list entries).
	if len(cs.PendingThreads) == 0 || opts.NoThreads {
		if err := imp.walkWindow(ctx, cc, state, sum); err != nil {
			return 0, err
		}
	}
	// The maintenance rescan (edits and reaction repair) runs only when
	// explicitly requested: archives ignore post-capture mutations by
	// default. It never charges the fetch budget and is skipped on
	// scoped runs regardless.
	if cs.Done && opts.Maintenance && opts.Limit == 0 {
		if err := imp.rescanHead(ctx, cc, sum); err != nil {
			return 0, err
		}
	}
	// Thread catch-up: conversation-level thread debt (--no-threads
	// backfills, non-channel gap recovery). Runs as soon as the backfill is
	// complete — including the run that completes it — on any threaded run;
	// limited runs make budget-bounded progress through the persisted walk
	// cursor, and the debt clears only after a clean finish.
	if cs.Done && cs.ThreadsPending && !opts.NoThreads {
		if err := imp.threadCatchUp(ctx, cc, state, sum); err != nil {
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
			// Isolated (message archiving proceeds) but honest: a members
			// listing outage is a fetch failure and the run must report
			// partial, not success.
			imp.recordItem(syncID, c.ID, "membership", store.SyncRunItemStatusError, "slack_fetch_error", err)
			sum.FetchErrors++
			sum.Errors++
			return nil
		}
	}
	if err := imp.store.ReplaceConversationParticipants(convID, members); err != nil {
		return err
	}
	return nil
}

// walkWindow walks one pinned window of the conversation's top-level
// history newest→oldest via cursor pages: the initial backfill covers
// ("", pin] and every later (incremental) walk covers (Cursor, pin]. The
// pin is the EXACT instant the walk started, never rounded forward — a
// forward-rounded pin would claim coverage of instants the walk cannot have
// covered and let arrivals inside the rounding shift the pinned pagination
// — and the window is inclusive of the pin, matching Cursor's
// covered-through meaning. Pinning happens BEFORE the first page: page
// cursors index into the bounded window, so introducing the bound mid-walk
// would shift the window under an already-issued cursor and skip messages.
// On completion Cursor advances to the pin (an empty window is one cheap
// page). Fetch errors leave the walk resumable rather than failing the run.
func (imp *Importer) walkWindow(ctx context.Context, cc *convScope, state *SyncState, sum *ImportSummary) error {
	cs := cc.cs
	initial := !cs.Done
	if cs.BackfillLatest == "" {
		cs.BackfillLatest = tsFormat(imp.now())
	}
	pages := 0
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if cc.limitReached() {
			return nil // resumable: the pin and page cursor persist
		}
		// The window floor overlaps back by the margin (like the sweep's):
		// the pin is OUR clock, message ts is SLACK's — skew between them
		// could otherwise hide a message created just below the pin after
		// the walk read that region. Overlap re-fetches resolve into
		// idempotent upserts.
		oldest := cs.Cursor
		if oldest != "" {
			oldest = overlapFloor(oldest)
		}
		page, err := imp.client.historyPageWithLimit(ctx, HistoryParams{
			ChannelID: cc.channelID,
			Cursor:    cs.BackfillCursor,
			Oldest:    oldest,
			Latest:    cs.BackfillLatest,
			Inclusive: true,
		}, cc.pageBudget())
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if errors.Is(err, ErrNotFound) {
				// Enumerated but unreadable (observed live: a sandbox
				// provisioning-bot DM) or since deleted. There is nothing to
				// fetch — recording it as a hard error would wedge every
				// future run into partial failure.
				imp.recordItem(cc.syncID, cc.channelID, "fetch", store.SyncRunItemStatusSkipped, "slack_channel_gone", err)
				cs.Done = true
				cs.BackfillCursor, cs.BackfillLatest = "", ""
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
		cc.budgetUsed += len(page.Messages)
		// Record each discovered root as durable thread-drain debt BEFORE the
		// page's cursor advances: "cursor past page" means "page durable and
		// its thread debt recorded". Recording charges the root's
		// reply_count forecast against the budget (see committed), so a run
		// commits to the reply work even when the drain is deferred.
		if cc.opts.NoThreads {
			// Initial-walk pages consumed threadless leave their roots
			// un-fetched, and the sweep floor postdates those replies: flag
			// the debt for the catch-up walk. Incremental windows need no
			// flag — their roots' replies all postdate the (stalled, since
			// sweeps skip --no-threads runs) watermark, so the next threaded
			// sweep owns them by creation time.
			if initial {
				for i := range page.Messages {
					if page.Messages[i].IsThreadRoot() {
						cs.ThreadsPending = true
						break
					}
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
			// Guard against a stale-merge resurrected window whose pin is
			// older than the covered-through bound: Cursor never regresses.
			if tsLess(cs.Cursor, cs.BackfillLatest) {
				cs.Cursor = cs.BackfillLatest
			}
			cs.BackfillLatest = ""
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
			if !m.IsThreadReply() {
				// The response leads with the parent regardless of bounds.
				// Skip it when already archived — no write, no charge:
				// re-persisting would refresh its content and reactions,
				// which is --maintenance work, not the drain's. It is only
				// processed when missing (this fetch is then the first to
				// see the root, and SetReplyTo needs it in place).
				if imp.parentArchived(cc.sourceID, cc.channelID, m.TS) {
					continue
				}
				if err := imp.processMessage(ctx, cc, m, sum); err != nil {
					return err
				}
				cc.budgetUsed++
				continue
			}
			if err := imp.processMessage(ctx, cc, m, sum); err != nil {
				return err
			}
			cc.budgetUsed++
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

// threadCatchUp re-walks a conversation's history fetching ONLY thread
// replies, paying conversation-level thread debt: a --no-threads backfill
// whose pages advanced past roots without fetches, or a non-channel
// conversation recovering a sweep gap. Pure re-read: every persist is an
// upsert. ThreadsPending clears only when the walk finishes with no drain
// debt left, so failures retry.
//
// The walk is shaped like the backfill: each page's roots are recorded as
// drain debt (charging their reply_count forecasts), paging holds while
// debt is outstanding, and the page cursor persists in CatchUpCursor — so
// limited runs make durable progress and a standing --limit schedule
// converges instead of restarting the walk forever.
//
// The upper bound pins at the WALK's start (persisted in CatchUpLatest;
// page cursors are only valid against the bound they were minted with) —
// not the original backfill pin: gap-recovery debt includes replies to
// roots created after the backfill, which a pin-bounded walk would never
// anchor. Roots newer than the walk pin need none of this (their replies
// postdate the sweep watermark by creation time), and the pin keeps the
// newest-first pagination window stable while the walk runs.
func (imp *Importer) threadCatchUp(ctx context.Context, cc *convScope, state *SyncState, sum *ImportSummary) error {
	cs := cc.cs
	if cs.CatchUpLatest == "" {
		cs.CatchUpLatest = tsFormat(imp.now()) // exact instant, never rounded forward
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if cc.limitReached() {
			return nil // resumes from CatchUpCursor next run
		}
		page, err := imp.client.historyPageWithLimit(ctx, HistoryParams{
			ChannelID: cc.channelID,
			Cursor:    cs.CatchUpCursor,
			Latest:    cs.CatchUpLatest,
			Inclusive: true,
		}, cc.pageBudget())
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if errors.Is(err, ErrNotFound) {
				// The conversation is gone: there is nothing left to fetch,
				// ever. Clearing the debt here keeps one deleted channel
				// from wedging every future workspace sync into failure.
				imp.recordItem(cc.syncID, cc.channelID, "fetch", store.SyncRunItemStatusSkipped, "slack_channel_gone", err)
				cs.ThreadsPending = false
				cs.CatchUpCursor, cs.CatchUpLatest = "", ""
				return nil
			}
			imp.recordItem(cc.syncID, cc.channelID, "fetch", store.SyncRunItemStatusError, "slack_fetch_error", err)
			sum.FetchErrors++
			sum.Errors++
			return nil // cursor stays; retried next run
		}
		// The page itself is only scanned for roots, but it still charges
		// the budget: re-reading history is the walk's dominant work, and
		// an uncharged scan would let a "limited" run page unboundedly.
		cc.budgetUsed += len(page.Messages)
		for i := range page.Messages {
			m := &page.Messages[i]
			if m.IsThreadRoot() {
				cs.RecordPendingThread(m.TS, m.ReplyCount)
			}
		}
		if err := imp.drainPendingThreads(ctx, cc, sum); err != nil {
			return err
		}
		if !page.HasMore || page.NextCursor == "" {
			// The WALK is complete: every root it owed is now recorded as
			// durable PendingThreads debt, which the drain-first step pays
			// unconditionally on every threaded run — so the flag (which
			// only schedules walks) and the cursor clear even when drain
			// debt remains. Keeping the flag here would re-visit this final
			// page forever, re-recording its already-drained threads.
			cs.ThreadsPending = false
			cs.CatchUpCursor, cs.CatchUpLatest = "", ""
			return nil
		}
		cs.CatchUpCursor = page.NextCursor
		if len(cs.PendingThreads) > 0 {
			return nil // one-page invariant: pay before paging further
		}
		imp.checkpoint(cc.syncID, state, sum)
	}
}

// parentArchived reports whether a thread parent is already in the archive.
// Uncertainty (a store error) reads as "not archived" so the parent gets
// processed — an idempotent upsert is the safe direction.
func (imp *Importer) parentArchived(sourceID int64, channelID, ts string) bool {
	ids, err := imp.store.MessageExistsBatch(sourceID, []string{sourceMessageID(channelID, ts)})
	if err != nil {
		return false
	}
	return len(ids) > 0
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
// failures are fatal too — a failed write means the local database is sick,
// and the held cursor makes the abort resumable — with ONE documented
// exemption: FTS, which is derived data with a repo-wide self-healing path
// (FTSNeedsBackfill + rebuild-fts exist precisely because importers
// warn-and-continue on it).
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
	// FTS is the one warn-and-continue store write: the index is derived
	// from the (fatally-checked) message row and body, holes are detected
	// by FTSNeedsBackfill's anti-join, and rebuild-fts repopulates them —
	// the same policy every other importer follows.
	if err := imp.store.UpsertFTS(messageID, "", text, imp.res.displayName(m.User), "", ""); err != nil {
		sum.Errors++
	}
	if m.Edited != nil {
		if err := imp.store.SetMessageEdited(messageID); err != nil {
			return fmt.Errorf("set message edited: %w", err)
		}
	}

	if err := imp.persistFiles(ctx, cc.syncID, messageID, m, cc.opts, sum); err != nil {
		return err
	}

	if err := imp.persistMentions(messageID, m); err != nil {
		return err
	}
	if err := imp.persistReactions(messageID, m); err != nil {
		return err
	}

	// Thread replies link to their root by source-message-ID lookup. Roots
	// always reach the archive before or with their replies (history pages
	// carry roots; the replies response carries the root first), and
	// SetReplyTo resolves to NULL harmlessly if one is missing.
	if m.IsThreadReply() {
		if err := imp.store.SetReplyTo(cc.sourceID, sourceMessageID(cc.channelID, m.TS), sourceMessageID(cc.channelID, m.ThreadTS)); err != nil {
			return fmt.Errorf("link thread reply: %w", err)
		}
	}

	sum.MessagesProcessed++
	sum.MessagesAdded++
	return nil
}

// persistMentions writes "mention" recipient rows. No from/to rows are
// written: sender attribution lives in messages.sender_id and membership in
// conversation_participants (WhatsApp/Beeper precedent).
func (imp *Importer) persistMentions(messageID int64, m *Message) error {
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
	return imp.store.ReplaceMessageRecipients(messageID, "mention", ids, names)
}

// persistReactions replaces the message's reactions from the embedded
// aggregates. Slack reactions carry no timestamp; created_at approximates
// with the target message's timestamp (cosmetic only). The API may truncate
// a reaction's user list on very popular messages — the archived raw JSON
// preserves the counts.
func (imp *Importer) persistReactions(messageID int64, m *Message) error {
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
	return imp.store.ReplaceReactions(messageID, reactions)
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
		if err = imp.persistFiles(ctx, syncID, item.MessageID, &m, opts, sum); err != nil {
			return sum, err
		}
		sum.MessagesProcessed++
	}
	imp.checkpointNow(syncID, state, sum)
	if err = imp.store.CompleteSync(syncID, stateBlob); err != nil {
		return sum, err
	}
	sum.Duration = imp.now().Sub(start)
	return sum, nil
}
