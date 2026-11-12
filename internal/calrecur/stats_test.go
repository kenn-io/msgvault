package calrecur

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRecurrenceStatsCountUnderlyingCandidates(t *testing.T) {
	for _, tc := range []struct {
		name, zone, start, until, wantEnd string
		lines                             []string
		wantKeys                          []string
		candidates                        int
	}{
		{"gap", "America/Los_Angeles", "2027-03-07T10:30:00Z", "2027-03-22T00:00:00Z", "2027-03-21T10:30:00Z",
			[]string{"RRULE:FREQ=WEEKLY;COUNT=2"}, []string{"20270307T103000Z", "20270321T093000Z"}, 4},
		{"skipped midnight", "America/Sao_Paulo", "2018-11-03T12:00:00Z", "2018-11-06T00:00:00Z", "2018-11-04T12:00:00Z",
			[]string{"RRULE:FREQ=DAILY;COUNT=2"}, []string{"20181103T120000Z", "20181104T110000Z"}, 3},
		{"duplicates and exclusion", "UTC", "2027-03-02T17:00:00Z", "2027-03-05T00:00:00Z", "2027-03-03T18:00:00Z",
			[]string{"RRULE:FREQ=DAILY;COUNT=2", "RRULE:FREQ=DAILY;COUNT=2", "RDATE:20270303T170000Z", "EXDATE:20270302T170000Z"},
			[]string{"20270303T170000Z"}, 6},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			zone, err := time.LoadLocation(tc.zone)
			requirements.NoError(err)
			start, until := instant(t, tc.start), instant(t, tc.until)
			s, err := ParseSet(start, false, "", zone, tc.lines)
			requirements.NoError(err)
			var got []string
			stats, err := testIterWithStats(t.Context(), s, until, 100, func(i instance) bool {
				got = append(got, i.Identity.Key())
				return true
			})
			requirements.NoError(err)
			assertions.Equal(tc.wantKeys, got)
			assertions.Equal(tc.candidates, stats.Candidates)
			occurrences, count, err := Expand(t.Context(), s, time.Hour, 0, start, until, 100)
			requirements.NoError(err)
			assertions.Equal(tc.candidates, count)
			var occurrenceKeys []string
			for _, o := range occurrences {
				occurrenceKeys = append(occurrenceKeys, o.Original.Key())
			}
			assertions.Equal(tc.wantKeys, occurrenceKeys)
			batch, _ := testExpandWindows(t.Context(), s, time.Hour, 0, []Window{{From: start, To: until, Budget: 100}})
			requirements.NoError(batch[0].Err)
			assertions.Equal(tc.candidates, batch[0].Candidates)
			assertions.Equal(occurrences, batch[0].Occurrences)
			end, bounded, stats := testConservativeEndWithStats(t.Context(), s, time.Hour, 0, 100)
			requirements.True(bounded)
			assertions.Equal(tc.candidates, stats.Candidates)
			assertions.Equal(instant(t, tc.wantEnd), end.UTC())
		})
	}
}

func TestMeetingHistoryExpandsSeriesOnce(t *testing.T) {
	start := instant(t, "2020-01-01T00:00:00Z")
	s, err := ParseSet(start, false, "", time.UTC, []string{"RRULE:FREQ=DAILY"})
	require.NoError(t, err)
	var windows []Window
	for year := 2020; year <= 2021; year++ {
		from := time.Date(year, 1, 1, 0, 0, 0, 0, time.UTC)
		windows = append(windows, Window{From: from.Add(-28 * time.Hour), To: from.AddDate(1, 0, 0).Add(28 * time.Hour), Budget: 50000})
	}
	results, physical := testExpandWindows(t.Context(), s, time.Hour, 0, windows)
	require.Len(t, results, 2)
	require.NoError(t, results[0].Err)
	require.NoError(t, results[1].Err)
	assert.Equal(t, 369, results[0].Candidates)
	assert.Equal(t, 734, results[1].Candidates)
	assert.LessOrEqual(t, physical, 734, "a shared traversal should generate each prefix once")
	windows[0].Budget = 400
	windows[1].Budget = 50000
	results, _ = testExpandWindows(t.Context(), s, time.Hour, 0, windows)
	require.NoError(t, results[0].Err)
	require.NoError(t, results[1].Err)
	assert.Equal(t, 369, results[0].Candidates)
	assert.Equal(t, 734, results[1].Candidates)
	windows[0].Budget = 100
	results, _ = testExpandWindows(t.Context(), s, time.Hour, 0, windows)
	assert.ErrorIs(t, results[0].Err, ErrBudgetExceeded)
	assert.Equal(t, 101, results[0].Candidates)
	assert.Empty(t, results[0].Occurrences)
	require.NoError(t, results[1].Err)
	assert.Equal(t, 734, results[1].Candidates)
}

func TestExpandWindowsMatchesIndependentHorizons(t *testing.T) {
	zone, err := time.LoadLocation("America/Los_Angeles")
	require.NoError(t, err)
	start := instant(t, "2027-03-07T10:30:00Z")
	s, err := ParseSet(start, false, "", zone, []string{"RRULE:FREQ=WEEKLY;COUNT=40", "RRULE:FREQ=WEEKLY;COUNT=40", "RDATE:20270321T093000Z", "EXDATE:20270328T093000Z"})
	require.NoError(t, err)
	windows := []Window{
		{From: instant(t, "2027-03-01T00:00:00Z"), To: instant(t, "2027-03-22T00:00:00Z"), Budget: 100},
		{From: instant(t, "2027-03-22T00:00:00Z"), To: instant(t, "2027-11-08T09:15:00Z"), Budget: 1000},
	}
	batch, _ := testExpandWindows(t.Context(), s, time.Hour, 0, windows)
	for i, w := range windows {
		occurrences, count, err := Expand(t.Context(), s, time.Hour, 0, w.From, w.To, w.Budget)
		require.NoError(t, err)
		require.NoError(t, batch[i].Err)
		assert.Equal(t, count, batch[i].Candidates)
		assert.Equal(t, occurrences, batch[i].Occurrences)
	}
}

func TestExpandWindowsUsesEachWindowsExceptions(t *testing.T) {
	start := instant(t, "2024-01-02T10:00:00Z")
	s, err := ParseSet(start, false, "", time.UTC, []string{"RRULE:FREQ=WEEKLY;COUNT=3"})
	require.NoError(t, err)
	from, to := start.Add(-time.Hour), start.AddDate(0, 0, 20)
	first := Identity{Instant: start}
	second := Identity{Instant: start.AddDate(0, 0, 7)}
	batch, _ := testExpandWindows(t.Context(), s, time.Hour, 0, []Window{
		{From: from, To: to, Budget: 100, ExDates: []Identity{first}},
		{From: from, To: to, Budget: 100, ExDates: []Identity{second}},
	})
	for _, result := range batch {
		require.NoError(t, result.Err)
	}
	require.Len(t, batch[0].Occurrences, 2)
	require.Len(t, batch[1].Occurrences, 2)
	assert.Equal(t, "20240109T100000Z", batch[0].Occurrences[0].Original.Key())
	assert.Equal(t, "20240102T100000Z", batch[1].Occurrences[0].Original.Key())
}

func TestExpandWindowsStopsPullingAfterBudgetFailure(t *testing.T) {
	start := instant(t, "2020-01-01T00:00:00Z")
	s, err := ParseSet(start, false, "", time.UTC, []string{"RRULE:FREQ=DAILY"})
	require.NoError(t, err)
	results, pulls := testExpandWindows(t.Context(), s, time.Hour, 0, []Window{
		{From: start, To: instant(t, "2021-01-02T04:00:00Z"), Budget: 400},
		{From: instant(t, "2021-01-01T00:00:00Z"), To: instant(t, "2022-01-02T04:00:00Z"), Budget: 1},
	})
	require.NoError(t, results[0].Err)
	assert.ErrorIs(t, results[1].Err, ErrBudgetExceeded)
	// A stream needs one lookahead to identify the shorter window's end.
	assert.Equal(t, 369, pulls)
}

func TestExpandWindowsAcrossSkippedCivilDate(t *testing.T) {
	zone, err := time.LoadLocation("Pacific/Apia")
	require.NoError(t, err)
	s, err := ParseSet(time.Time{}, true, "2011-12-29", zone, []string{"RRULE:FREQ=DAILY;COUNT=3", "RDATE;VALUE=DATE:20111230"})
	require.NoError(t, err)
	windows := []Window{
		{From: instant(t, "2011-12-28T00:00:00Z"), To: instant(t, "2011-12-31T00:00:00Z"), Budget: 100},
		{From: instant(t, "2011-12-31T00:00:00Z"), To: instant(t, "2012-01-03T00:00:00Z"), Budget: 100},
	}
	batch, _ := testExpandWindows(t.Context(), s, 0, 1, windows)
	for i, w := range windows {
		occurrences, count, err := Expand(t.Context(), s, 0, 1, w.From, w.To, w.Budget)
		require.NoError(t, err)
		require.NoError(t, batch[i].Err)
		assert.Equal(t, count, batch[i].Candidates)
		assert.Equal(t, occurrences, batch[i].Occurrences)
	}
}

func TestIterationStatsRetainLookaheadOnEarlyStopAndCancellation(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	s := parsed(t, "RRULE:FREQ=DAILY;COUNT=2", "RRULE:FREQ=DAILY;COUNT=2")
	until := instant(t, "2027-03-05T00:00:00Z")
	stats, err := testIterWithStats(t.Context(), s, until, 100, func(instance) bool { return false })
	requirements.NoError(err)
	assertions.Equal(3, stats.Candidates)
	ctx, cancel := context.WithCancel(t.Context())
	stats, err = testIterWithStats(ctx, s, until, 100, func(instance) bool { cancel(); return true })
	requirements.ErrorIs(err, context.Canceled)
	assertions.Equal(3, stats.Candidates)
	stats, err = testIterWithStats(ctx, s, until, 100, func(instance) bool { return true })
	requirements.ErrorIs(err, context.Canceled)
	assertions.Zero(stats.Candidates)
	_, bounded, stats := testConservativeEndWithStats(ctx, s, time.Hour, 0, 100)
	assertions.False(bounded)
	assertions.Zero(stats.Candidates)
}

func TestIterationStatsCountCandidateBeyondAbsoluteFoldHorizon(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	zone, err := time.LoadLocation("America/Los_Angeles")
	requirements.NoError(err)
	s, err := ParseSet(instant(t, "2027-11-07T09:30:00Z"), false, "", zone, []string{"RRULE:FREQ=DAILY;COUNT=2"})
	requirements.NoError(err)
	emitted := 0
	stats, err := testIterWithStats(t.Context(), s, instant(t, "2027-11-07T09:15:00Z"), 100, func(instance) bool { emitted++; return true })
	requirements.NoError(err)
	assertions.Zero(emitted)
	// The rule pulled the civil 01:30 candidate, then rejected the supplied
	// later-fold DTSTART because it is after the absolute horizon.
	assertions.Equal(1, stats.Candidates)
	batch, _ := testExpandWindows(t.Context(), s, time.Hour, 0, []Window{
		{From: instant(t, "2027-11-01T00:00:00Z"), To: instant(t, "2027-11-07T09:15:00Z"), Budget: 100},
		{From: instant(t, "2027-11-07T09:15:00Z"), To: instant(t, "2027-11-10T00:00:00Z"), Budget: 100},
	})
	requirements.NoError(batch[0].Err)
	assertions.Equal(1, batch[0].Candidates)
	assertions.Empty(batch[0].Occurrences)
	requirements.NoError(batch[1].Err)
}

func TestCivilDateBoundaryExportedSemantics(t *testing.T) {
	for _, tc := range []struct {
		date, zone, want string
		exists           bool
	}{
		{"2027-03-13", "", "2027-03-13T00:00:00Z", true},
		{"2018-11-04", "America/Sao_Paulo", "2018-11-04T03:00:00Z", true},
		{"2011-12-30", "Pacific/Apia", "2011-12-30T10:00:00Z", false},
		{"2040-12-31", "America/New_York", "2040-12-31T05:00:00Z", true},
		{"2041-01-01", "America/New_York", "2041-01-01T05:00:00Z", true},
	} {
		t.Run(tc.date+tc.zone, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			var zone *time.Location
			var err error
			if tc.zone != "" {
				zone, err = time.LoadLocation(tc.zone)
				requirements.NoError(err)
			}
			got, exists, err := CivilDateBoundary(tc.date, zone)
			requirements.NoError(err)
			assertions.Equal(instant(t, tc.want), got.UTC())
			assertions.Equal(tc.exists, exists)
		})
	}
	_, _, err := CivilDateBoundary("2027-02-30", time.UTC)
	require.Error(t, err)
}

func FuzzIterationStatsIncludeExcludedDuplicateCandidates(f *testing.F) {
	f.Add(uint8(2))
	f.Fuzz(func(t *testing.T, countByte uint8) {
		requirements := require.New(t)
		assertions := assert.New(t)
		count := int(countByte%16) + 1
		line := fmt.Sprintf("RRULE:FREQ=DAILY;COUNT=%d", count)
		s := parsed(t, line, line)
		for day := range count {
			id := Identity{Instant: s.DTStart.AddDate(0, 0, day)}
			s.RDates = append(s.RDates, id)
			s.ExDates = append(s.ExDates, id)
		}
		emitted := 0
		stats, err := testIterWithStats(t.Context(), s, instant(t, "2028-01-01T00:00:00Z"), 100, func(instance) bool { emitted++; return true })
		requirements.NoError(err)
		assertions.Zero(emitted)
		assertions.Equal(1+3*count, stats.Candidates) // DTSTART, RDATEs, and both rules.
	})
}

func testIter(ctx context.Context, s Set, until time.Time, budget int, yield func(instance) bool) error {
	_, err := testIterWithStats(ctx, s, until, budget, yield)
	return err
}
func testIterWithStats(ctx context.Context, s Set, until time.Time, budget int, yield func(instance) bool) (struct{ Candidates int }, error) {
	counter := &candidateBudget{ctx: ctx, limit: budget}
	err := iter(ctx, s, until, counter, yield)
	return struct{ Candidates int }{counter.used}, err
}
func testConservativeEndWithStats(ctx context.Context, s Set, d time.Duration, days, budget int) (time.Time, bool, struct{ Candidates int }) {
	counter := &candidateBudget{ctx: ctx, limit: budget}
	end, ok := conservativeEnd(ctx, s, d, days, counter)
	return end, ok, struct{ Candidates int }{counter.used}
}

func testExpandWindows(ctx context.Context, s Set, duration time.Duration, days int, windows []Window) ([]WindowResult, int) {
	var horizon time.Time
	for _, w := range windows {
		if w.Budget > 0 && w.To.After(w.From) && w.To.After(horizon) {
			horizon = w.To
		}
	}
	if horizon.IsZero() {
		horizon = windows[0].To
	}
	expander, err := NewWindowExpander(ctx, s, duration, days, horizon)
	if err != nil {
		return []WindowResult{{Err: err}}, 0
	}
	results := make([]WindowResult, len(windows))
	for i, w := range windows {
		results[i] = expander.Expand(ctx, w)
	}
	return results, expander.pulls
}
