package calrecur

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func timed(t *testing.T, value string) Identity {
	t.Helper()
	return Identity{Instant: instant(t, value)}
}

func TestExpandWeekly6WeeksExdateCancelledMoved(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	s := parsed(t, "RRULE:FREQ=WEEKLY;COUNT=6", "EXDATE:20270309T170000Z")
	movedStart, movedEnd := instant(t, "2027-03-24T18:00:00Z"), instant(t, "2027-03-24T19:00:00Z")
	exc := []Exception{
		{Original: timed(t, "2027-03-16T17:00:00Z"), Cancelled: true, Ref: 11},
		{Original: timed(t, "2027-03-23T17:00:00Z"), Start: &movedStart, End: &movedEnd, Ref: 12},
	}
	got, err := Expand(t.Context(), s, time.Hour, 0, exc, instant(t, "2027-03-01T00:00:00Z"), instant(t, "2027-04-13T00:00:00Z"), 100)
	requirements.NoError(err)
	requirements.Len(got, 5)
	assertions.True(got[1].Cancelled)
	assertions.Equal(int64(11), got[1].ExceptionRef)
	assertions.Equal(movedStart, got[2].Start)
	assertions.Equal("20270323T170000Z", got[2].Original.Key())
	live := 0
	for _, o := range got {
		if !o.Cancelled {
			live++
		}
	}
	assertions.Equal(4, live)
}

func TestExpandSparseTombstoneInheritsBounds(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	s := parsed(t, "RRULE:FREQ=DAILY;COUNT=2")
	got, err := Expand(t.Context(), s, time.Hour, 0, []Exception{{Original: timed(t, "2027-03-03T17:00:00Z"), Cancelled: true, Ref: 42}},
		instant(t, "2027-03-03T00:00:00Z"), instant(t, "2027-03-04T00:00:00Z"), 100)
	requirements.NoError(err)
	requirements.Len(got, 1)
	assertions.Equal(instant(t, "2027-03-03T17:00:00Z"), got[0].Start)
	assertions.Equal(instant(t, "2027-03-03T18:00:00Z"), got[0].End)
	assertions.True(got[0].Cancelled)
}

func TestExpandMovedIntoWindow(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	s := parsed(t, "RRULE:FREQ=DAILY;COUNT=2", "EXDATE:20270303T170000Z")
	start, end := instant(t, "2027-06-01T17:00:00Z"), instant(t, "2027-06-01T18:00:00Z")
	got, err := Expand(t.Context(), s, time.Hour, 0, []Exception{{Original: timed(t, "2027-03-03T17:00:00Z"), Start: &start, End: &end, Ref: 7}},
		instant(t, "2027-06-01T00:00:00Z"), instant(t, "2027-06-02T00:00:00Z"), 100)
	requirements.NoError(err)
	requirements.Len(got, 1)
	assertions.Equal("20270303T170000Z", got[0].Original.Key())
	assertions.Equal(start, got[0].Start)
	assertions.Equal(int64(7), got[0].ExceptionRef)
}

func TestExpandMovedOutOfWindow(t *testing.T) {
	start, end := instant(t, "2027-06-01T17:00:00Z"), instant(t, "2027-06-01T18:00:00Z")
	got, err := Expand(t.Context(), parsed(t, "RRULE:FREQ=DAILY;COUNT=2"), time.Hour, 0,
		[]Exception{{Original: timed(t, "2027-03-03T17:00:00Z"), Start: &start, End: &end}},
		instant(t, "2027-03-03T00:00:00Z"), instant(t, "2027-03-04T00:00:00Z"), 100)
	require.NoError(t, err)
	assert.Empty(t, got)
}

func TestContainsExceptionBeatsExdate(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	s := parsed(t, "RRULE:FREQ=DAILY;COUNT=2", "EXDATE:20270303T170000Z")
	id := timed(t, "2027-03-03T17:00:00Z")
	found, err := Contains(t.Context(), s, nil, id, 100)
	requirements.NoError(err)
	assertions.False(found)
	found, err = Contains(t.Context(), s, []Exception{{Original: id, Cancelled: true}}, id, 100)
	requirements.NoError(err)
	assertions.True(found)
}

func TestExpandAllDayCalendarDurationAcrossDST(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	zone, err := time.LoadLocation("America/Los_Angeles")
	requirements.NoError(err)
	s, err := ParseSet(time.Time{}, true, "2027-03-13", zone, []string{"RRULE:FREQ=DAILY;COUNT=2"})
	requirements.NoError(err)
	got, err := Expand(t.Context(), s, 48*time.Hour, 2, nil, instant(t, "2027-03-13T00:00:00Z"), instant(t, "2027-03-16T00:00:00Z"), 100)
	requirements.NoError(err)
	requirements.Len(got, 2)
	assertions.Equal("2027-03-15", got[0].EndDate)
	assertions.Equal(47*time.Hour, got[0].End.Sub(got[0].Start))
	assertions.Equal(instant(t, "2027-03-15T07:00:00Z"), got[0].End.UTC())
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
			got, err := Expand(t.Context(), s, time.Hour, 0, nil, tc.from, tc.to, 100)
			require.NoError(t, err)
			assert.Empty(t, got)
		})
	}
}
