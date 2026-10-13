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
			stats, err := IterWithStats(t.Context(), s, until, 100, func(i Instance) bool {
				got = append(got, i.Identity.Key())
				return true
			})
			requirements.NoError(err)
			assertions.Equal(tc.wantKeys, got)
			assertions.Equal(tc.candidates, stats.Candidates)
			occurrences, stats, err := ExpandWithStats(t.Context(), s, time.Hour, 0, nil, start, until, 100)
			requirements.NoError(err)
			assertions.Equal(tc.candidates, stats.Candidates)
			var occurrenceKeys []string
			for _, o := range occurrences {
				occurrenceKeys = append(occurrenceKeys, o.Original.Key())
			}
			assertions.Equal(tc.wantKeys, occurrenceKeys)
			end, bounded, stats := ConservativeEndWithStats(t.Context(), s, time.Hour, 0, 100)
			requirements.True(bounded)
			assertions.Equal(tc.candidates, stats.Candidates)
			assertions.Equal(instant(t, tc.wantEnd), end.UTC())
		})
	}
}

func TestExpansionStatsIncludeExceptionSuppressionAndOverflow(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	s := parsed(t, "RRULE:FREQ=DAILY;COUNT=2", "RRULE:FREQ=DAILY;COUNT=2", "RDATE:20270303T170000Z", "EXDATE:20270302T170000Z")
	until := instant(t, "2027-03-05T00:00:00Z")
	exc := []Exception{{Original: timed(t, "2027-03-03T17:00:00Z"), Cancelled: true, Ref: 42}}
	got, stats, err := ExpandWithStats(t.Context(), s, time.Hour, 0, exc, s.DTStart, until, 6)
	requirements.NoError(err)
	requirements.Len(got, 1)
	assertions.True(got[0].Cancelled)
	assertions.Equal(int64(42), got[0].ExceptionRef)
	assertions.Equal(6, stats.Candidates)
	got, stats, err = ExpandWithStats(t.Context(), s, time.Hour, 0, exc, s.DTStart, until, 5)
	requirements.ErrorIs(err, ErrBudgetExceeded)
	assertions.Empty(got)
	assertions.Equal(6, stats.Candidates)
	stats, err = IterWithStats(t.Context(), s, until, 5, func(Instance) bool { return true })
	requirements.ErrorIs(err, ErrBudgetExceeded)
	assertions.Equal(6, stats.Candidates)
	end, bounded, stats := ConservativeEndWithStats(t.Context(), s, time.Hour, 0, 5)
	assertions.False(bounded)
	assertions.True(end.IsZero())
	assertions.Equal(6, stats.Candidates)
}

func TestIterationStatsRetainLookaheadOnEarlyStopAndCancellation(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	s := parsed(t, "RRULE:FREQ=DAILY;COUNT=2", "RRULE:FREQ=DAILY;COUNT=2")
	until := instant(t, "2027-03-05T00:00:00Z")
	stats, err := IterWithStats(t.Context(), s, until, 100, func(Instance) bool { return false })
	requirements.NoError(err)
	assertions.Equal(3, stats.Candidates)
	ctx, cancel := context.WithCancel(t.Context())
	stats, err = IterWithStats(ctx, s, until, 100, func(Instance) bool { cancel(); return true })
	requirements.ErrorIs(err, context.Canceled)
	assertions.Equal(3, stats.Candidates)
	stats, err = IterWithStats(ctx, s, until, 100, func(Instance) bool { return true })
	requirements.ErrorIs(err, context.Canceled)
	assertions.Zero(stats.Candidates)
	_, bounded, stats := ConservativeEndWithStats(ctx, s, time.Hour, 0, 100)
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
	stats, err := IterWithStats(t.Context(), s, instant(t, "2027-11-07T09:15:00Z"), 100, func(Instance) bool { emitted++; return true })
	requirements.NoError(err)
	assertions.Zero(emitted)
	// The rule pulled the civil 01:30 candidate, then rejected the supplied
	// later-fold DTSTART because it is after the absolute horizon.
	assertions.Equal(1, stats.Candidates)
}

func TestCivilDateBoundaryExportedSemantics(t *testing.T) {
	for _, tc := range []struct {
		date, zone, want string
		exists           bool
	}{
		{"2027-03-13", "", "2027-03-13T00:00:00Z", true},
		{"2018-11-04", "America/Sao_Paulo", "2018-11-04T03:00:00Z", true},
		{"2011-12-30", "Pacific/Apia", "2011-12-30T10:00:00Z", false},
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
		stats, err := IterWithStats(t.Context(), s, instant(t, "2028-01-01T00:00:00Z"), 100, func(Instance) bool { emitted++; return true })
		requirements.NoError(err)
		assertions.Zero(emitted)
		assertions.Equal(1+3*count, stats.Candidates) // DTSTART, RDATEs, and both rules.
	})
}
