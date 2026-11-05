package calrecur

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExpandRejectsNegativeDurations(t *testing.T) {
	zone, err := time.LoadLocation("UTC")
	require.NoError(t, err)
	allDay, err := ParseSet(time.Time{}, true, "2027-03-02", zone, nil)
	require.NoError(t, err)
	for _, tc := range []struct {
		name         string
		set          Set
		duration     time.Duration
		durationDays int
	}{
		{"timed duration", parsed(t, "RRULE:FREQ=DAILY;COUNT=1"), -time.Hour, 0},
		{"all-day duration", allDay, 0, -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, _, err := Expand(t.Context(), tc.set, tc.duration, tc.durationDays,
				instant(t, "2027-02-28T00:00:00Z"), instant(t, "2027-03-04T00:00:00Z"), 100)
			require.ErrorIs(t, err, ErrUnsupportedRecurrence)
			assert.Empty(t, got)
		})
	}
}

func TestExpandRejectsNegativeDurationForEmptyOrReversedWindow(t *testing.T) {
	from := instant(t, "2027-03-02T17:00:00Z")
	for _, tc := range []struct {
		name string
		to   time.Time
	}{
		{name: "empty", to: from},
		{name: "reversed", to: instant(t, "2027-03-01T17:00:00Z")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			got, _, err := Expand(t.Context(), parsed(t, "RRULE:FREQ=DAILY;COUNT=1"), -time.Hour, 0, from, tc.to, 100)
			requirements.ErrorIs(err, ErrUnsupportedRecurrence)
			assertions.Empty(got)
		})
	}
}

func TestExpandAllDayCalendarDurationAcrossDST(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	zone, err := time.LoadLocation("America/Los_Angeles")
	requirements.NoError(err)
	s, err := ParseSet(time.Time{}, true, "2027-03-13", zone, []string{"RRULE:FREQ=DAILY;COUNT=2"})
	requirements.NoError(err)
	got, _, err := Expand(t.Context(), s, 48*time.Hour, 2, instant(t, "2027-03-13T00:00:00Z"), instant(t, "2027-03-16T00:00:00Z"), 100)
	requirements.NoError(err)
	requirements.Len(got, 2)
	assertions.Equal("2027-03-15", got[0].EndDate)
	assertions.Equal(47*time.Hour, got[0].End.Sub(got[0].Start))
	assertions.Equal(instant(t, "2027-03-15T07:00:00Z"), got[0].End.UTC())
}

func TestExpandAllDayWindowIncludesFirstMidnightBeforeRollback(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	zone, err := time.LoadLocation("America/St_Johns")
	requirements.NoError(err)
	s, err := ParseSet(time.Time{}, true, "2009-11-01", zone, nil)
	requirements.NoError(err)

	from := instant(t, "2009-11-01T02:30:00Z")
	boundary, exists, err := CivilDateBoundary("2009-11-01", zone)
	requirements.NoError(err)
	assertions.True(exists)
	assertions.Equal(from, boundary.UTC())

	got, _, err := Expand(t.Context(), s, 0, 1, from, from.Add(time.Minute), 100)
	requirements.NoError(err)
	requirements.Len(got, 1)
	assertions.Equal("2009-11-01", got[0].StartDate)
	assertions.Equal(from, got[0].Start.UTC())
}

func TestExpandAllDayWindowEndingAfterRollbackIncludesEarlierOccurrence(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	zone, err := time.LoadLocation("America/St_Johns")
	requirements.NoError(err)
	s, err := ParseSet(time.Time{}, true, "2009-11-01", zone, []string{"RRULE:FREQ=DAILY;COUNT=1"})
	requirements.NoError(err)

	from := instant(t, "2009-11-01T02:30:00Z")
	to := instant(t, "2009-11-01T02:32:00Z")
	got, _, err := Expand(t.Context(), s, 0, 1, from, to, 100)
	requirements.NoError(err)
	requirements.Len(got, 1)
	assertions.Equal("2009-11-01", got[0].StartDate)
	assertions.Equal(from, got[0].Start.UTC())
}

func TestExpandDoesNotChargeBudgetForExclusiveEndCandidate(t *testing.T) {
	s := parsed(t, "RDATE:20270303T170000Z")
	got, _, err := Expand(t.Context(), s, time.Hour, 0,
		instant(t, "2027-03-02T16:00:00Z"), instant(t, "2027-03-03T17:00:00Z"), 1)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "20270302T170000Z", got[0].Original.Key())
}

func TestExpandAllDayDoesNotChargeBudgetForExclusiveEndCandidate(t *testing.T) {
	requirements := require.New(t)
	s, err := ParseSet(time.Time{}, true, "2027-03-02", time.UTC, []string{"RRULE:FREQ=DAILY;COUNT=2"})
	requirements.NoError(err)
	got, _, err := Expand(t.Context(), s, 0, 1,
		instant(t, "2027-03-02T00:00:00Z"), instant(t, "2027-03-03T00:00:00Z"), 2)
	requirements.NoError(err)
	requirements.Len(got, 1)
	assert.Equal(t, "2027-03-02", got[0].StartDate)
}

func TestExpandHalfOpenWindow(t *testing.T) {
	s := parsed(t, "RRULE:FREQ=DAILY;COUNT=2")
	start := instant(t, "2027-03-02T17:00:00Z")
	for _, tc := range []struct {
		name     string
		from, to time.Time
	}{
		{"touching end", start.Add(time.Hour), start.Add(2 * time.Hour)},
		{"touching start", start.Add(-time.Hour), start},
		{"empty", start.Add(30 * time.Minute), start.Add(30 * time.Minute)},
		{"reversed", start.Add(45 * time.Minute), start.Add(15 * time.Minute)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, _, err := Expand(t.Context(), s, time.Hour, 0, tc.from, tc.to, 100)
			require.NoError(t, err)
			assert.Empty(t, got)
		})
	}
}

func TestExpandZeroLengthOccurrenceAtWindowBounds(t *testing.T) {
	s := parsed(t, "RRULE:FREQ=DAILY;COUNT=2")
	start := instant(t, "2027-03-02T17:00:00Z")
	for _, tc := range []struct {
		name     string
		from, to time.Time
		want     int
	}{
		{"at start", start, start.Add(time.Hour), 1},
		{"at exclusive end", start.Add(-time.Hour), start, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, _, err := Expand(t.Context(), s, 0, 0, tc.from, tc.to, 100)
			require.NoError(t, err)
			assert.Len(t, got, tc.want)
		})
	}
}

func TestTimedDateOnlyUntilIncludesLocalDate(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	zone, err := time.LoadLocation("America/Los_Angeles")
	require.NoError(err)
	// 23:30 local is the next UTC day, so a UTC reading of UNTIL would drop it.
	s, err := ParseSet(instant(t, "2027-03-01T23:30:00-08:00"), false, "", zone, []string{"RRULE:FREQ=DAILY;UNTIL=20270303"})
	require.NoError(err)
	got, _, err := Expand(t.Context(), s, time.Hour, 0, instant(t, "2027-03-01T00:00:00Z"), instant(t, "2027-03-10T00:00:00Z"), 100)
	require.NoError(err)
	starts := make([]time.Time, 0, len(got))
	for _, o := range got {
		starts = append(starts, o.Start.UTC())
	}
	assert.Equal([]time.Time{
		instant(t, "2027-03-02T07:30:00Z"), instant(t, "2027-03-03T07:30:00Z"), instant(t, "2027-03-04T07:30:00Z"),
	}, starts)
	end, bounded := ConservativeEnd(t.Context(), s, time.Hour, 0, 100)
	assert.True(bounded)
	assert.Equal(instant(t, "2027-03-04T08:30:00Z"), end.UTC())
}
