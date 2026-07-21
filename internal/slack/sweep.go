package slack

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/store"
)

const (
	// sweepLagMargin keeps certification behind real time so a sweep cannot
	// certify an instant whose messages may not be indexed yet.
	sweepLagMargin = 10 * time.Minute
	// sweepTruncationCeiling is search.messages' reachable-result ceiling
	// per query (count 100 × page cap 100). A single-day total beyond it
	// fails the run: results past the ceiling cannot be reached by paging,
	// and re-querying the same day serves the same first 10k (ascending
	// order is stable), so no amount of retrying drains it. Recovery is
	// `--full` (backfill's inline thread fetches need no search); per-scope
	// `in:`-batch narrowing is the specified (unbuilt) sweep-native escape
	// hatch — see docs/internal/slack-reply-sweep-design.md.
	sweepTruncationCeiling = searchPageLimit * maxSearchPages
)

// sweepTarget is a done conversation eligible for reply archiving.
type sweepTarget struct {
	convID int64
}

// sweepBudget bounds a limited run's sweep work (limit 0 = unlimited). Days
// searched and canonically fetched messages both charge it: fetches are the
// message work, and the per-day charge keeps a long catch-up from paging
// through months of queries on a run that promised to be small. Exhaustion
// parks certification at the last safe boundary WITHOUT failing the run —
// per-day commits are durable, so repeated limited runs converge.
type sweepBudget struct{ limit, used int }

func (b *sweepBudget) exhausted() bool { return b.limit > 0 && b.used >= b.limit }

// sweepReplies discovers thread replies created since each conversation's
// certification stamp via search.messages (threads:replies) and archives
// them through canonical conversations.replies fetches.
//
// Certification is per conversation (ConvState.SweptThrough) with the
// workspace watermark (SweepWatermark) tracking the current target set as a
// whole. A conversation that re-enters the target set behind the watermark —
// it was excluded, gone, or filtered while sweeps advanced — first recovers
// its gap with a channel-scoped sweep; then one workspace-wide sweep runs
// from the watermark. Certification derives only from fully-searched
// intervals, never from fetched content (a canonical fetch returns whole
// threads, including replies newer than the search index horizon).
func (imp *Importer) sweepReplies(ctx context.Context, syncID int64, targets map[string]sweepTarget, state *SyncState, sum *ImportSummary) error {
	if len(targets) == 0 {
		return nil
	}
	// Date modifiers evaluate in the user's CURRENT profile timezone at
	// query time (probed live, including retroactive re-filing after a tz
	// change) using the IANA zone's HISTORICAL DST rules (probed live
	// against a corpus spanning DST transitions: a January day boundary
	// follows the winter offset even when queried in summer, and the
	// transition day itself is served as a 23-hour day). Day arithmetic
	// must therefore use the zone read this run, not a fixed offset.
	loc := imp.res.tzLocation(imp.opts.UserID)
	offset := imp.res.tzOffset(imp.opts.UserID)
	budget := &sweepBudget{limit: imp.opts.Limit}
	now := imp.now().UTC()
	horizon := tsFormat(now.Add(-sweepLagMargin))

	ids := make([]string, 0, len(targets))
	for cid := range targets {
		ids = append(ids, cid)
	}
	sort.Strings(ids)

	// Adopt a certification stamp for targets that never carried one:
	// max(own backfill pin, workspace watermark). The pin is exact for a
	// conversation that just completed backfill (inline thread fetches
	// covered every reply up to it, and the pin always postdates the
	// watermark at completion time); the watermark is the correct reading
	// for legacy state written before per-conversation stamps existed,
	// because that code swept every target on every run.
	for _, cid := range ids {
		cs := state.EnsureConv(cid)
		if cs.SweptThrough != "" {
			continue
		}
		edge := cs.BackfillLatest
		if edge == "" {
			edge = cs.Cursor
		}
		cs.SweptThrough = edge
		if tsLess(cs.SweptThrough, state.SweepWatermark) {
			cs.SweptThrough = state.SweepWatermark
		}
	}

	// Gap recovery: a target certified behind the workspace watermark
	// missed sweeps while absent from the target set.
	for _, cid := range ids {
		cs := state.Conversations[cid]
		if state.SweepWatermark == "" || !tsLess(cs.SweptThrough, state.SweepWatermark) {
			continue
		}
		if !strings.HasPrefix(cid, "C") {
			// in:<#ID> scoping is only probed reliable for channel IDs.
			// DMs and group DMs recover through the thread catch-up walk
			// instead (fetches every thread, so the gap's bounds are moot);
			// the flag persists until a clean pass, so stamping forward
			// here cannot lose the debt.
			cs.ThreadsPending = true
			cs.SweptThrough = state.SweepWatermark
			continue
		}
		err := imp.sweepRange(ctx, syncID, cid, cs.SweptThrough, tsTime(state.SweepWatermark), state.SweepWatermark,
			map[string]sweepTarget{cid: targets[cid]}, loc, budget, state, sum,
			func(certified string) { cs.SweptThrough = certified })
		if err != nil {
			return err
		}
	}

	// Workspace-wide sweep from the watermark (first sweep: from the
	// earliest certification among targets).
	floor := state.SweepWatermark
	if floor == "" {
		for _, cid := range ids {
			st := state.Conversations[cid].SweptThrough
			if st == "" {
				continue
			}
			if floor == "" || tsLess(st, floor) {
				floor = st
			}
		}
	}
	if floor == "" {
		return nil // nothing backfilled yet; backfill owns all replies so far
	}
	// Search runs through NOW — replies inside the lag window are archived
	// too — but certification caps at the horizon, so anything the index
	// may not have served yet is re-swept next run (idempotent upserts).
	return imp.sweepRange(ctx, syncID, "", floor, now, horizon, targets, loc, budget, state, sum,
		func(certified string) {
			state.SweepWatermark, state.SweepOffset = certified, offset
			for _, cid := range ids {
				cs := state.Conversations[cid]
				// Only a conversation already certified through the sweep's
				// floor is contiguously covered to the new boundary; one
				// parked behind (a failed gap sweep) keeps its stamp so the
				// gap is retried next run.
				if !tsLess(cs.SweptThrough, floor) && tsLess(cs.SweptThrough, certified) {
					cs.SweptThrough = certified
				}
			}
		})
}

// sweepRange drains the threads:replies search from floor's day through
// searchEnd's day for one scope ("" = workspace-wide; a channel ID =
// in:<#ID>-scoped), day by day in the user's current timezone, archiving
// every hit above floor. commit is invoked with each newly certified
// boundary — end of a completed day, capped at ceiling (the lag horizon for
// the workspace sweep; searchEnd may exceed it so fresh replies are fetched
// without being certified) — so multi-day catch-ups checkpoint per day.
// Discovery and fetch failures are recorded, certification parks at the
// last safe boundary, and the sweep stops; only store/context failures
// return an error.
func (imp *Importer) sweepRange(ctx context.Context, syncID int64, scope, floor string, searchEnd time.Time, ceiling string, targets map[string]sweepTarget, loc *time.Location, budget *sweepBudget, state *SyncState, sum *ImportSummary, commit func(certified string)) error {
	day := tsTime(floor).In(loc)
	day = time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, loc)
	end := searchEnd.In(loc)
	for !day.After(end) {
		if budget.exhausted() {
			return nil // certification stays at the last committed boundary
		}
		budget.used++
		dayStr := day.Format("2006-01-02")
		item := dayStr
		if scope != "" {
			item = scope + ":" + dayStr
		}
		hits, truncated, err := imp.sweepDay(ctx, syncID, scope, dayStr, targets)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			// Discovery failure: nothing this day was processed;
			// certification stays where the last complete day left it.
			imp.recordItem(syncID, item, "sweep", store.SyncRunItemStatusError, "slack_search_error", err)
			sum.FetchErrors++
			sum.Errors++
			return nil
		}
		complete, parked, err := imp.fetchSweepHits(ctx, syncID, hits, floor, targets, budget, sum)
		if err != nil {
			return err // store/context failure: fatal for the run
		}
		if !complete {
			// A canonical fetch failed mid-day; resume from just before it.
			if parked != "" && tsLess(floor, parked) {
				commit(minTS(parked, ceiling))
			}
			return nil
		}
		if truncated {
			// Ascending order means the reachable results ARE the day's
			// earliest, so the last processed hit is a safe boundary — but
			// the rest of the day is unreachable and the run must fail
			// loudly rather than skip past it.
			imp.recordItem(syncID, item, "sweep", store.SyncRunItemStatusError, "slack_sweep_truncated",
				fmt.Errorf("day %s exceeds the %d reachable results per query; run --full to recover its replies (see the sweep design doc)", dayStr, sweepTruncationCeiling))
			sum.FetchErrors++
			sum.Errors++
			if len(hits) > 0 && tsLess(floor, hits[len(hits)-1].TS) {
				commit(minTS(hits[len(hits)-1].TS, ceiling))
			}
			return nil
		}
		nextDay := nextDayStart(day, loc)
		commit(minTS(tsFormat(nextDay.UTC()), ceiling))
		imp.checkpoint(syncID, state, sum)
		day = nextDay
	}
	return nil
}

// sweepDay runs the paged threads:replies query for one user-tz day,
// returning hits filtered to sweep targets (ascending by ts) and whether
// the day's total exceeds the reachable-result ceiling.
func (imp *Importer) sweepDay(ctx context.Context, syncID int64, scope, day string, targets map[string]sweepTarget) ([]SearchMatch, bool, error) {
	var hits []SearchMatch
	truncated := false
	for page := 1; page <= maxSearchPages; page++ {
		sp, err := imp.client.SearchMessagesPage(ctx, imp.sweepQuery(scope, day, syncID, page), page)
		if err != nil {
			return nil, false, err
		}
		// Past-the-ceiling requests are silently CLAMPED to page 1 (probed
		// live): a mismatched echo means the walk must stop, not loop.
		if sp.Page != page {
			break
		}
		for _, m := range sp.Matches {
			if _, ok := targets[m.ChannelID]; ok {
				hits = append(hits, m)
			}
		}
		if page == 1 && sp.Total > sweepTruncationCeiling {
			truncated = true
		}
		if page >= sp.Pages {
			break
		}
	}
	sort.Slice(hits, func(i, j int) bool { return tsLess(hits[i].TS, hits[j].TS) })
	return hits, truncated, nil
}

// sweepQuery builds one day's query. The negated nonce term is semantically
// inert and makes every query string unique: search results are cached by
// query string (probed live — a cached result can serve stale day filings
// and miss recently indexed messages).
func (imp *Importer) sweepQuery(scope, day string, syncID int64, page int) string {
	q := fmt.Sprintf(`threads:replies on:%s -"zqsweep%dp%d"`, day, syncID, page)
	if scope != "" {
		q = fmt.Sprintf("in:<#%s> ", scope) + q
	}
	return q
}

// fetchSweepHits archives discovered replies via canonical
// conversations.replies fetches, grouped one fetch per thread. Hits at or
// below floor are already archived (certification semantics) and skipped.
// Returns (complete, parked boundary, fatal error): on a fetch failure,
// complete=false and the parked boundary sits just before the failed
// thread's first hit — everything before it was fetched (ascending order).
func (imp *Importer) fetchSweepHits(ctx context.Context, syncID int64, hits []SearchMatch, floor string, targets map[string]sweepTarget, budget *sweepBudget, sum *ImportSummary) (bool, string, error) {
	type group struct {
		channelID string
		anchorTS  string // any ts within the thread; replies resolves it
		minHit    string
	}
	var groups []group
	index := map[string]int{}
	for _, h := range hits {
		if !tsLess(floor, h.TS) {
			continue // at/below the certification boundary: already archived
		}
		key := h.ChannelID + ":" + h.RootTS
		if h.RootTS == "" {
			key = h.ChannelID + ":solo:" + h.TS // unparseable permalink: per-hit fetch
		}
		if i, ok := index[key]; ok {
			if tsLess(h.TS, groups[i].minHit) {
				groups[i].minHit = h.TS
			}
			continue
		}
		index[key] = len(groups)
		groups = append(groups, group{channelID: h.ChannelID, anchorTS: h.TS, minHit: h.TS})
	}
	sort.Slice(groups, func(i, j int) bool { return tsLess(groups[i].minHit, groups[j].minHit) })

	for _, g := range groups {
		if err := ctx.Err(); err != nil {
			return false, "", err
		}
		if budget.exhausted() {
			// Budget-park, not a failure: everything before this group is
			// fetched (ascending order), so resuming here next run is safe.
			return false, tsMinusMicro(g.minHit), nil
		}
		target := targets[g.channelID]
		cc := &convScope{channelID: g.channelID, convID: target.convID, sourceID: imp.sourceID, syncID: syncID, opts: imp.opts}
		err := imp.fetchThread(ctx, cc, g.anchorTS, tsMinusMicro(g.minHit), sum)
		budget.used += cc.budgetUsed
		if errors.Is(err, ErrNotFound) {
			// Thread/channel gone between discovery and fetch: expected churn.
			imp.recordItem(syncID, sourceMessageID(g.channelID, g.anchorTS), "sweep", store.SyncRunItemStatusSkipped, "slack_thread_gone", err)
			continue
		}
		if err != nil {
			if ctx.Err() != nil {
				return false, "", ctx.Err()
			}
			imp.recordItem(syncID, sourceMessageID(g.channelID, g.anchorTS), "sweep", store.SyncRunItemStatusError, "slack_fetch_error", err)
			sum.FetchErrors++
			sum.Errors++
			// Ascending order: everything before this group is complete.
			return false, tsMinusMicro(g.minHit), nil
		}
	}
	return true, "", nil
}

// nextDayStart returns the start of the calendar day after day in loc,
// honoring the zone's historical DST rules: a transition day is 23 or 25
// hours long, and a midnight that does not exist normalizes forward. This
// boundary is load-bearing for certification — search files messages by the
// zone's civil day (probed live), so a fixed-offset boundary could certify
// an hour the day's query never served.
func nextDayStart(day time.Time, loc *time.Location) time.Time {
	return time.Date(day.Year(), day.Month(), day.Day()+1, 0, 0, 0, 0, loc)
}

// tsFormat renders a UTC instant as a Slack ts string (microsecond fraction).
func tsFormat(t time.Time) string {
	return fmt.Sprintf("%d.%06d", t.Unix(), t.Nanosecond()/1000)
}

// minTS returns the earlier of two Slack ts strings.
func minTS(a, b string) string {
	if tsLess(b, a) {
		return b
	}
	return a
}

// tsMinusMicro returns the ts one microsecond earlier (Slack ts strings have
// microsecond resolution), for exclusive-bound arithmetic.
func tsMinusMicro(ts string) string {
	if !strings.Contains(ts, ".") {
		return ts
	}
	t := tsTime(ts).Add(-time.Microsecond)
	return tsFormat(t)
}
