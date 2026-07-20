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
	// sweepLagMargin keeps the watermark behind real time so a sweep cannot
	// certify an instant whose messages may not be indexed yet.
	sweepLagMargin = 10 * time.Minute
	// sweepTruncationCeiling is search.messages' reachable-result ceiling
	// per query (count 100 × page cap 100). A single-day total beyond it is
	// logged as truncation; in:-batch narrowing is the specified (unbuilt)
	// escape hatch — see docs/internal/slack-reply-sweep-design.md.
	sweepTruncationCeiling = searchPageLimit * maxSearchPages
)

// sweepTarget is a done conversation eligible for reply archiving.
type sweepTarget struct {
	convID int64
}

// sweepReplies discovers thread replies created since the sweep watermark
// via search.messages (threads:replies) and archives them through canonical
// conversations.replies fetches. The watermark advances only behind
// persisted work; a failed fetch parks it just before the failed thread's
// first hit so the next run resumes exactly there.
func (imp *Importer) sweepReplies(ctx context.Context, syncID int64, targets map[string]sweepTarget, state *SyncState, sum *ImportSummary) error {
	floor := state.SweepWatermark
	if floor == "" {
		floor = firstSweepFloor(state)
	}
	if floor == "" {
		return nil // nothing backfilled yet; backfill owns all replies so far
	}
	// Date modifiers evaluate in the user's CURRENT profile timezone at
	// query time (probed live, including retroactive re-filing after a tz
	// change), so day arithmetic must use the offset read this run.
	offset := imp.res.tzOffset(imp.opts.UserID)

	now := imp.now().UTC()
	horizon := now.Add(-sweepLagMargin)
	newWatermark := floor

	for _, day := range sweepDays(tsTime(floor), now, offset) {
		hits, err := imp.sweepDay(ctx, syncID, day, targets)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			// Discovery failure: nothing this day was processed; the
			// watermark stays where persisted work ended.
			imp.recordItem(syncID, day, "sweep", store.SyncRunItemStatusError, "slack_search_error", err)
			sum.FetchErrors++
			sum.Errors++
			state.SweepWatermark, state.SweepOffset = newWatermark, offset
			return nil
		}
		complete, high, err := imp.fetchSweepHits(ctx, syncID, hits, floor, targets, sum)
		if err != nil {
			return err // store-level failure: fatal for the run
		}
		if high != "" && tsLess(newWatermark, high) {
			newWatermark = high
		}
		if !complete {
			// A canonical fetch failed mid-day; resume from just before it.
			state.SweepWatermark, state.SweepOffset = newWatermark, offset
			return nil
		}
	}
	// Clean completion: certify everything up to the lag horizon.
	horizonTS := fmt.Sprintf("%d.%06d", horizon.Unix(), horizon.Nanosecond()/1000)
	if tsLess(newWatermark, horizonTS) {
		newWatermark = horizonTS
	}
	state.SweepWatermark, state.SweepOffset = newWatermark, offset
	return nil
}

// sweepDay runs the paged threads:replies query for one user-tz day,
// returning hits filtered to sweep targets, ascending by ts.
func (imp *Importer) sweepDay(ctx context.Context, syncID int64, day string, targets map[string]sweepTarget) ([]SearchMatch, error) {
	var hits []SearchMatch
	for page := 1; page <= maxSearchPages; page++ {
		sp, err := imp.client.SearchMessagesPage(ctx, imp.sweepQuery(day, syncID, page), page)
		if err != nil {
			return nil, err
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
			imp.recordItem(syncID, day, "sweep", store.SyncRunItemStatusSkipped, "slack_sweep_truncated",
				fmt.Errorf("day %s has %d matches, beyond the %d reachable per query; results past the ceiling are not swept", day, sp.Total, sweepTruncationCeiling))
		}
		if page >= sp.Pages {
			break
		}
	}
	sort.Slice(hits, func(i, j int) bool { return tsLess(hits[i].TS, hits[j].TS) })
	return hits, nil
}

// sweepQuery builds the day's query. The negated nonce term is semantically
// inert and makes every query string unique: search results are cached by
// query string (probed live — a cached result can serve stale day filings
// and miss recently indexed messages).
func (imp *Importer) sweepQuery(day string, syncID int64, page int) string {
	return fmt.Sprintf(`threads:replies on:%s -"zqsweep%dp%d"`, day, syncID, page)
}

// fetchSweepHits archives discovered replies via canonical
// conversations.replies fetches, grouped one fetch per thread. Hits at or
// below floor are already archived (watermark semantics) and skipped.
// Returns (complete, highest persisted ts−safe watermark, fatal error):
// on a fetch failure, complete=false and the returned high watermark sits
// just before the failed thread's first hit.
func (imp *Importer) fetchSweepHits(ctx context.Context, syncID int64, hits []SearchMatch, floor string, targets map[string]sweepTarget, sum *ImportSummary) (bool, string, error) {
	type group struct {
		channelID string
		anchorTS  string // any ts within the thread; replies resolves it
		minHit    string
	}
	var groups []group
	index := map[string]int{}
	for _, h := range hits {
		if !tsLess(floor, h.TS) {
			continue // at/below the watermark: already archived
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

	high := ""
	for _, g := range groups {
		if err := ctx.Err(); err != nil {
			return false, high, err
		}
		target := targets[g.channelID]
		persistedTo, err := imp.fetchThread(ctx, syncID, target.convID, g.channelID, g.anchorTS, tsMinusMicro(g.minHit), sum)
		if errors.Is(err, ErrNotFound) {
			// Thread/channel gone between discovery and fetch: expected churn.
			imp.recordItem(syncID, sourceMessageID(g.channelID, g.anchorTS), "sweep", store.SyncRunItemStatusSkipped, "slack_thread_gone", err)
			continue
		}
		if err != nil {
			if ctx.Err() != nil {
				return false, high, ctx.Err()
			}
			imp.recordItem(syncID, sourceMessageID(g.channelID, g.anchorTS), "sweep", store.SyncRunItemStatusError, "slack_fetch_error", err)
			sum.FetchErrors++
			sum.Errors++
			// Ascending order: everything before this group is complete.
			return false, tsMinusMicro(g.minHit), nil
		}
		if persistedTo != "" && (high == "" || tsLess(high, persistedTo)) {
			high = persistedTo
		}
	}
	return true, high, nil
}

// fetchThread canonically fetches a thread from oldest (exclusive) onward,
// persisting every message (the response's included parent re-upserts
// harmlessly). Returns the newest persisted ts.
func (imp *Importer) fetchThread(ctx context.Context, syncID, convID int64, channelID, anchorTS, oldest string, sum *ImportSummary) (string, error) {
	cc := &convScope{channelID: channelID, convID: convID, sourceID: imp.sourceID, syncID: syncID, opts: imp.opts}
	pageCursor := ""
	high := ""
	for {
		page, err := imp.client.repliesPageWithLimit(ctx, channelID, anchorTS, pageCursor, oldest, historyPageLimit)
		if err != nil {
			return high, err
		}
		for i := range page.Messages {
			m := &page.Messages[i]
			if err := imp.processMessage(ctx, cc, m, sum); err != nil {
				return high, err
			}
			if m.IsThreadReply() {
				sum.RepliesFetched++
				if high == "" || tsLess(high, m.TS) {
					high = m.TS
				}
			}
		}
		if !page.HasMore || page.NextCursor == "" {
			return high, nil
		}
		pageCursor = page.NextCursor
	}
}

// firstSweepFloor derives the initial watermark: the earliest pinned
// backfill edge among completed conversations. Replies existing before each
// conversation's backfill were fetched by the backfill's inline per-root
// walk; everything created after the earliest pin is sweep territory.
func firstSweepFloor(state *SyncState) string {
	floor := ""
	for _, cs := range state.Conversations {
		if !cs.Done {
			continue
		}
		edge := cs.BackfillLatest
		if edge == "" {
			edge = cs.Cursor
		}
		if edge == "" {
			continue
		}
		if floor == "" || tsLess(edge, floor) {
			floor = edge
		}
	}
	return floor
}

// sweepDays lists the user-tz calendar days from the floor instant through
// now, ascending, formatted for on: modifiers (YYYY-MM-DD).
func sweepDays(floor, now time.Time, offsetSeconds int) []string {
	loc := time.FixedZone("user", offsetSeconds)
	day := floor.In(loc)
	day = time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, loc)
	end := now.In(loc)
	var days []string
	for !day.After(end) {
		days = append(days, day.Format("2006-01-02"))
		day = day.AddDate(0, 0, 1)
	}
	return days
}

// tsMinusMicro returns the ts one microsecond earlier (Slack ts strings have
// microsecond resolution), for exclusive-bound arithmetic.
func tsMinusMicro(ts string) string {
	sec, frac, ok := strings.Cut(ts, ".")
	if !ok {
		return ts
	}
	t := tsTime(ts).Add(-time.Microsecond)
	_ = sec
	_ = frac
	return fmt.Sprintf("%d.%06d", t.Unix(), t.Nanosecond()/1000)
}
