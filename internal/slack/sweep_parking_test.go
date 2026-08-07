package slack

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func parkedSweepTS(base time.Time, minutes int) string {
	instant := base.Add(-25 * time.Hour).Add(time.Duration(minutes) * time.Minute)
	return strconv.FormatInt(instant.Unix(), 10) + ".000100"
}

func parkedSweepFreshTS(base time.Time, seconds int) string {
	instant := base.Add(time.Duration(2+seconds) * time.Second)
	return strconv.FormatInt(instant.Unix(), 10) + ".000100"
}

func pinParkedSweepClock(imp *Importer, at time.Time) {
	imp.now = func() time.Time { return at }
}

func parkedSweepWorkspace(t *testing.T, base time.Time) (*fakeSlack, string) {
	t.Helper()
	f := newFakeSlack(t)
	f.users = []map[string]any{
		{"id": "UME", "name": "me", "profile": map[string]any{"email": "me@example.com"}},
	}
	rootTS := parkedSweepTS(base, -14400)
	f.convs = []*fakeConv{{
		ID: "C09", Name: "archive", Kind: "public", Members: []string{"UME"},
		Msgs: []fakeMsg{
			{TS: rootTS, User: "UME", Text: "ancient root",
				Replies: []fakeMsg{{TS: parkedSweepTS(base, -14390), ThreadTS: rootTS, User: "UME", Text: "ancient reply"}}},
			{TS: parkedSweepTS(base, 0), User: "UME", Text: "recent chatter"},
		},
	}}
	return f, rootTS
}

func addParkedSweepReply(f *fakeSlack, rootTS, replyTS string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	root := f.conv("C09").findRoot(rootTS)
	root.Replies = append(root.Replies, fakeMsg{
		TS: replyTS, ThreadTS: rootTS, User: "UME", Text: "late reply",
	})
	return "C09:" + replyTS
}

func requireParkedSweepReply(t *testing.T, imp *Importer, sourceMessageID string) {
	t.Helper()
	require := require.New(t)
	var archived int
	require.NoError(imp.store.DB().QueryRow(imp.store.Rebind(
		`SELECT COUNT(*) FROM messages WHERE source_message_id = ?`), sourceMessageID).Scan(&archived))
	require.Equal(1, archived, "repeated --limit 1 runs must archive the late reply")
}

func TestSweepConvergesFloorJustAfterMidnight(t *testing.T) {
	require := require.New(t)
	midnight := time.Date(2026, 8, 7, 0, 0, 0, 0, time.UTC)
	base := midnight.Add(3 * time.Minute)
	f, rootTS := parkedSweepWorkspace(t, base)
	imp, opts := testImporter(t, f)

	pinParkedSweepClock(imp, base)
	_, err := imp.Import(context.Background(), opts)
	require.NoError(err)
	replyID := addParkedSweepReply(f, rootTS, parkedSweepFreshTS(base, 0))

	limited := opts
	limited.Limit = 1
	for _, offset := range []time.Duration{time.Minute, 30 * time.Minute, 2 * time.Hour} {
		pinParkedSweepClock(imp, base.Add(offset))
		_, err = imp.Import(context.Background(), limited)
		require.NoError(err)
	}
	requireParkedSweepReply(t, imp, replyID)
}

func TestSweepConvergesFloorExactlyOnMidnight(t *testing.T) {
	require := require.New(t)
	midnight := time.Date(2026, 8, 7, 0, 0, 0, 0, time.UTC)
	base := midnight.Add(-time.Second)
	f, rootTS := parkedSweepWorkspace(t, base)
	imp, opts := testImporter(t, f)

	pinParkedSweepClock(imp, base)
	_, err := imp.Import(context.Background(), opts)
	require.NoError(err)
	replyID := addParkedSweepReply(f, rootTS, parkedSweepFreshTS(base, 0))

	limited := opts
	limited.Limit = 1
	pinParkedSweepClock(imp, base.Add(time.Minute))
	_, err = imp.Import(context.Background(), limited)
	require.NoError(err)
	for _, offset := range []time.Duration{time.Hour, 2 * time.Hour, 3 * time.Hour} {
		pinParkedSweepClock(imp, midnight.Add(offset))
		_, err = imp.Import(context.Background(), limited)
		require.NoError(err)
	}
	requireParkedSweepReply(t, imp, replyID)
}

func TestSweepLimitOneConvergesFromDayBehindBacklog(t *testing.T) {
	require := require.New(t)
	midnight := time.Date(2026, 8, 7, 0, 0, 0, 0, time.UTC)
	base := midnight.Add(-36 * time.Hour)
	f, rootTS := parkedSweepWorkspace(t, base)
	imp, opts := testImporter(t, f)

	pinParkedSweepClock(imp, base)
	_, err := imp.Import(context.Background(), opts)
	require.NoError(err)
	replyID := addParkedSweepReply(f, rootTS, parkedSweepFreshTS(base, 25*60*60))

	limited := opts
	limited.Limit = 1
	runNow := base.Add(48 * time.Hour)
	for i := range 5 {
		pinParkedSweepClock(imp, runNow.Add(time.Duration(i)*time.Minute))
		_, err = imp.Import(context.Background(), limited)
		require.NoError(err)
	}
	requireParkedSweepReply(t, imp, replyID)
}

func TestSweepLimitOneOverlapHitDoesNotStarveUncertifiedDay(t *testing.T) {
	require := require.New(t)
	midnight := time.Date(2026, 8, 7, 0, 0, 0, 0, time.UTC)
	base := midnight.Add(3 * time.Minute)
	f, rootTS := parkedSweepWorkspace(t, base)
	imp, opts := testImporter(t, f)

	pinParkedSweepClock(imp, base)
	_, err := imp.Import(context.Background(), opts)
	require.NoError(err)
	pinParkedSweepClock(imp, base.Add(time.Minute))
	sum, err := imp.Import(context.Background(), opts)
	require.NoError(err)
	state := requireResumeState(t, imp, sum.SourceID)
	initialWatermark := state.SweepWatermark
	require.NotEmpty(initialWatermark, "test setup: warm-up sweep must establish the workspace watermark")
	floorTime := tsTime(initialWatermark)

	// The reply appears after the initial run with a source timestamp inside
	// its certified overlap. Search must turn it into debt without letting its
	// fetch consume the only unit needed to reach the first uncertified day.
	overlapReply := tsFormat(floorTime.Add(-5 * time.Minute))
	require.True(tsLess(overlapFloor(initialWatermark), overlapReply), "test setup: reply must be above queryFloor")
	require.True(tsLess(overlapReply, initialWatermark), "test setup: reply must be below the persisted floor")
	replyID := addParkedSweepReply(f, rootTS, overlapReply)

	limited := opts
	limited.Limit = 1
	pinParkedSweepClock(imp, base.Add(2*time.Minute))
	_, err = imp.Import(context.Background(), limited)
	require.NoError(err)
	state = requireResumeState(t, imp, sum.SourceID)
	require.True(tsLess(initialWatermark, state.SweepWatermark),
		"the first limited run must reserve enough budget to reach uncertified coverage")

	for minute := 3; minute <= 13; minute++ {
		pinParkedSweepClock(imp, base.Add(time.Duration(minute)*time.Minute))
		_, err = imp.Import(context.Background(), limited)
		require.NoError(err)
	}

	state = requireResumeState(t, imp, sum.SourceID)
	require.True(tsLess(tsFormat(floorTime.Add(10*time.Minute)), state.SweepWatermark),
		"the watermark must advance far enough for the hit to leave the overlap")
	requireParkedSweepReply(t, imp, replyID)
}

func TestSweepRangeExhaustedAtEntrySkipsFreeOverlapSearch(t *testing.T) {
	require := require.New(t)
	f := newFakeSlack(t)
	imp, opts := testImporter(t, f)
	src, err := imp.store.GetOrCreateSource(sourceTypeSlack, "T01:UME")
	require.NoError(err)
	syncID, err := imp.store.StartSync(src.ID, sourceTypeSlack)
	require.NoError(err)
	imp.opts = opts
	imp.sourceID = src.ID

	var searches []string
	f.onSearch = func(query string, page int) {
		if page == 1 {
			searches = append(searches, query)
		}
	}
	state := NewSyncState()
	sum := &ImportSummary{SourceID: src.ID}
	budget := &sweepBudget{limit: 1}
	target := func(channelID string) map[string]sweepTarget {
		return map[string]sweepTarget{channelID: {}}
	}

	// The first gap range spends the shared budget on an uncertified day.
	firstFloor := time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)
	firstEnd := firstFloor.Add(time.Hour)
	err = imp.sweepRange(context.Background(), syncID, "C01", tsFormat(firstFloor), firstEnd, tsFormat(firstEnd),
		target("C01"), time.UTC, budget, state, sum, func(string) {})
	require.NoError(err)
	require.True(budget.exhausted(), "test setup: first gap range must spend the shared budget")

	// The next channel begins inside the post-midnight overlap band. Its
	// previous day would be free if this range had started with capacity, but
	// the exhausted shared budget must stop before issuing another search.
	secondFloor := time.Date(2026, 8, 7, 0, 3, 0, 0, time.UTC)
	secondEnd := secondFloor.Add(time.Minute)
	err = imp.sweepRange(context.Background(), syncID, "C02", tsFormat(secondFloor), secondEnd, tsFormat(secondEnd),
		target("C02"), time.UTC, budget, state, sum, func(string) {})
	require.NoError(err)
	require.Len(searches, 1, "an exhausted shared budget must not search another channel's free overlap day")
	require.Contains(searches[0], "in:<#C01>")
}
