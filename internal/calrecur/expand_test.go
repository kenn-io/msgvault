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

func TestExpandRejectsStartOnlyExceptionWithReversedBounds(t *testing.T) {
	original := timed(t, "2027-03-02T17:00:00Z")
	movedStart := instant(t, "2027-03-02T19:00:00Z")
	got, err := Expand(t.Context(), parsed(t, "RRULE:FREQ=DAILY;COUNT=1"), time.Hour, 0,
		[]Exception{{Original: original, Start: &movedStart}},
		instant(t, "2027-03-02T17:00:00Z"), instant(t, "2027-03-02T20:00:00Z"), 100)
	require.ErrorIs(t, err, ErrUnsupportedRecurrence)
	assert.Empty(t, got)
}

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
			got, err := Expand(t.Context(), tc.set, tc.duration, tc.durationDays, nil,
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
			got, err := Expand(t.Context(), parsed(t, "RRULE:FREQ=DAILY;COUNT=1"), -time.Hour, 0, nil, from, tc.to, 100)
			requirements.ErrorIs(err, ErrUnsupportedRecurrence)
			assertions.Empty(got)
		})
	}
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

func TestExpandAllDayDurationUsesCivilEndDateWhenMidnightIsSkipped(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	zone, err := time.LoadLocation("America/Sao_Paulo")
	requirements.NoError(err)
	s, err := ParseSet(time.Time{}, true, "2018-11-03", zone, []string{"RRULE:FREQ=DAILY;COUNT=1"})
	requirements.NoError(err)

	got, err := Expand(t.Context(), s, 0, 1, nil, instant(t, "2018-11-02T00:00:00Z"), instant(t, "2018-11-06T00:00:00Z"), 100)
	requirements.NoError(err)
	requirements.Len(got, 1)
	assertions.Equal("2018-11-03", got[0].StartDate)
	assertions.Equal("2018-11-04", got[0].EndDate)
	assertions.Equal(time.Date(2018, 11, 4, 1, 0, 0, 0, zone), got[0].End)
}

func TestExpandAllDayExceptionPreservesCivilBoundsWhenMidnightIsSkipped(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	zone, err := time.LoadLocation("America/Sao_Paulo")
	requirements.NoError(err)
	s, err := ParseSet(time.Time{}, true, "2018-11-03", zone, []string{"RRULE:FREQ=DAILY;COUNT=1"})
	requirements.NoError(err)
	exception := Exception{
		Original:  Identity{Date: "2018-11-03", AllDay: true},
		StartDate: "2018-11-04",
		EndDate:   "2018-11-05",
	}

	got, err := Expand(t.Context(), s, 0, 0, []Exception{exception}, instant(t, "2018-11-02T00:00:00Z"), instant(t, "2018-11-06T00:00:00Z"), 100)
	requirements.NoError(err)
	requirements.Len(got, 1)
	assertions.Equal("2018-11-04", got[0].StartDate)
	assertions.Equal("2018-11-05", got[0].EndDate)
	assertions.Equal(time.Date(2018, 11, 4, 1, 0, 0, 0, zone), got[0].Start)
	assertions.Equal(time.Date(2018, 11, 5, 0, 0, 0, 0, zone), got[0].End)
}

func TestExpandAllDayUsesNextLocalBoundaryWhenExclusiveDateIsSkipped(t *testing.T) {
	requirements := require.New(t)
	zone, err := time.LoadLocation("Pacific/Apia")
	requirements.NoError(err)
	s, err := ParseSet(time.Time{}, true, "2011-12-29", zone, nil)
	requirements.NoError(err)

	got, err := Expand(t.Context(), s, 0, 1, nil, instant(t, "2011-12-29T00:00:00Z"), instant(t, "2012-01-01T00:00:00Z"), 100)
	requirements.NoError(err)
	requirements.Len(got, 1)
	assert.Equal(t, "2011-12-30", got[0].EndDate)
	assert.Equal(t, time.Date(2011, 12, 31, 0, 0, 0, 0, zone), got[0].End)
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

	got, err := Expand(t.Context(), s, 0, 1, nil, from, from.Add(time.Minute), 100)
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
	got, err := Expand(t.Context(), s, 0, 1, nil, from, to, 100)
	requirements.NoError(err)
	requirements.Len(got, 1)
	assertions.Equal("2009-11-01", got[0].StartDate)
	assertions.Equal(from, got[0].Start.UTC())
}

func TestExpandDoesNotChargeBudgetForExclusiveEndCandidate(t *testing.T) {
	s := parsed(t, "RDATE:20270303T170000Z")
	got, err := Expand(t.Context(), s, time.Hour, 0, nil,
		instant(t, "2027-03-02T16:00:00Z"), instant(t, "2027-03-03T17:00:00Z"), 1)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "20270302T170000Z", got[0].Original.Key())
}

func TestExpandAllDayDoesNotChargeBudgetForExclusiveEndCandidate(t *testing.T) {
	requirements := require.New(t)
	s, err := ParseSet(time.Time{}, true, "2027-03-02", time.UTC, []string{"RRULE:FREQ=DAILY;COUNT=2"})
	requirements.NoError(err)
	got, err := Expand(t.Context(), s, 0, 1, nil,
		instant(t, "2027-03-02T00:00:00Z"), instant(t, "2027-03-03T00:00:00Z"), 2)
	requirements.NoError(err)
	requirements.Len(got, 1)
	assert.Equal(t, "2027-03-02", got[0].StartDate)
}

func TestExpandDetachedExceptionOverridesSkippedRDate(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	zone, err := time.LoadLocation("Pacific/Apia")
	requirements.NoError(err)
	s, err := ParseSet(time.Time{}, true, "2011-12-29", zone, []string{"RDATE;VALUE=DATE:20111230"})
	requirements.NoError(err)
	start := instant(t, "2011-12-31T10:00:00Z")
	end := start.Add(time.Hour)
	exception := Exception{
		Original: Identity{Date: "2011-12-30", AllDay: true},
		Start:    &start,
		End:      &end,
	}

	got, err := Expand(t.Context(), s, 0, 1, []Exception{exception}, start.Add(-time.Hour), end.Add(time.Hour), 100)
	requirements.NoError(err)
	requirements.Len(got, 1)
	assertions.Equal(exception.Original, got[0].Original)
	assertions.Equal(start, got[0].Start)
	assertions.Equal(end, got[0].End)
}

func TestContainsDetachedExceptionOnSkippedCivilDate(t *testing.T) {
	requirements := require.New(t)
	zone, err := time.LoadLocation("Pacific/Apia")
	requirements.NoError(err)
	s, err := ParseSet(time.Time{}, true, "2011-12-29", zone, []string{"RDATE;VALUE=DATE:20111230"})
	requirements.NoError(err)
	original := Identity{Date: "2011-12-30", AllDay: true}
	found, err := Contains(t.Context(), s, []Exception{{Original: original, Cancelled: true}}, original, 100)
	requirements.NoError(err)
	assert.True(t, found)
}

func TestContainsAfterDetachedExceptionOnSkippedCivilDate(t *testing.T) {
	requirements := require.New(t)
	zone, err := time.LoadLocation("Pacific/Apia")
	requirements.NoError(err)
	s, err := ParseSet(time.Time{}, true, "2011-12-29", zone,
		[]string{"RDATE;VALUE=DATE:20111230", "RDATE;VALUE=DATE:20111231"})
	requirements.NoError(err)
	exception := Exception{
		Original:  Identity{Date: "2011-12-30", AllDay: true},
		Cancelled: true,
	}

	found, err := Contains(t.Context(), s, []Exception{exception}, Identity{Date: "2011-12-31", AllDay: true}, 100)
	requirements.NoError(err)
	requirements.True(found)
}

func TestExpandRejectsReversedAllDayDatesAcrossSkippedCivilDate(t *testing.T) {
	requirements := require.New(t)
	zone, err := time.LoadLocation("Pacific/Apia")
	requirements.NoError(err)
	s, err := ParseSet(time.Time{}, true, "2011-12-29", zone, nil)
	requirements.NoError(err)
	exception := Exception{
		Original:  Identity{Date: "2011-12-29", AllDay: true},
		StartDate: "2011-12-31",
		EndDate:   "2011-12-30",
	}
	boundary := time.Date(2011, 12, 31, 0, 0, 0, 0, zone)

	got, err := Expand(t.Context(), s, 0, 1, []Exception{exception}, boundary.Add(-time.Hour), boundary.Add(time.Hour), 100)
	requirements.ErrorIs(err, ErrUnsupportedRecurrence)
	assert.Empty(t, got)
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

func TestExpandExceptionEffectiveType(t *testing.T) {
	zone, err := time.LoadLocation("America/Los_Angeles")
	require.NoError(t, err)
	nominalStart := instant(t, "2027-03-13T17:00:00Z")
	movedStart, movedEnd := instant(t, "2027-06-01T09:00:00Z"), instant(t, "2027-06-01T10:00:00Z")
	earlyEnd := instant(t, "2027-03-13T10:00:00Z")
	conflictingStart := instant(t, "2027-06-01T17:00:00Z")
	dateEnd := instant(t, "2027-03-15T18:00:00Z")
	for _, tc := range []struct {
		name                       string
		masterAllDay               bool
		exception                  Exception
		wantAllDay                 bool
		wantStart, wantEnd         string
		wantStartDate, wantEndDate string
	}{
		{"all-day to timed", true, Exception{Start: &movedStart, End: &movedEnd}, false,
			"2027-06-01T09:00:00Z", "2027-06-01T10:00:00Z", "", ""},
		{"timed to all-day", false, Exception{StartDate: "2027-03-13", EndDate: "2027-03-15"}, true,
			"2027-03-13T08:00:00Z", "2027-03-15T07:00:00Z", "2027-03-13", "2027-03-15"},
		{"timed to moved all-day", false, Exception{StartDate: "2027-06-01", EndDate: "2027-06-02"}, true,
			"2027-06-01T07:00:00Z", "2027-06-02T07:00:00Z", "2027-06-01", "2027-06-02"},
		{"dates take precedence over instants", false, Exception{Start: &conflictingStart, End: &movedEnd, StartDate: "2027-03-13", EndDate: "2027-03-15"}, true,
			"2027-03-13T08:00:00Z", "2027-03-15T07:00:00Z", "2027-03-13", "2027-03-15"},
		{"date start with instant end", false, Exception{StartDate: "2027-03-13", End: &dateEnd}, true,
			"2027-03-13T08:00:00Z", "2027-03-15T07:00:00Z", "2027-03-13", "2027-03-15"},
		{"end-only timed conversion", true, Exception{End: &earlyEnd}, false,
			"2027-03-13T08:00:00Z", "2027-03-13T10:00:00Z", "", ""},
		{"end-only all-day conversion", false, Exception{EndDate: "2027-03-15"}, true,
			"2027-03-13T08:00:00Z", "2027-03-15T07:00:00Z", "2027-03-13", "2027-03-15"},
		{"sparse all-day tombstone", true, Exception{Cancelled: true}, true,
			"2027-03-13T08:00:00Z", "2027-03-15T07:00:00Z", "2027-03-13", "2027-03-15"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			s, err := ParseSet(nominalStart, tc.masterAllDay, "2027-03-13", zone, []string{"RRULE:FREQ=DAILY;COUNT=1"})
			requirements.NoError(err)
			original := Identity{Instant: nominalStart}
			if tc.masterAllDay {
				original = Identity{Date: "2027-03-13", AllDay: true}
			}
			e := tc.exception
			e.Original, e.Ref = original, 42
			start, end := instant(t, tc.wantStart), instant(t, tc.wantEnd)
			got, err := Expand(t.Context(), s, time.Hour, 2, []Exception{e}, start, end, 100)
			requirements.NoError(err)
			requirements.Len(got, 1)
			assertions.Equal(original, got[0].Original)
			assertions.Equal(tc.wantAllDay, got[0].AllDay)
			assertions.Equal(start, got[0].Start.UTC())
			assertions.Equal(end, got[0].End.UTC())
			assertions.Equal(tc.wantStartDate, got[0].StartDate)
			assertions.Equal(tc.wantEndDate, got[0].EndDate)
			assertions.True(got[0].BoundsKnown)
			assertions.Equal(e.Cancelled, got[0].Cancelled)
			assertions.Equal(int64(42), got[0].ExceptionRef)
			if tc.name == "all-day to timed" {
				got, err = Expand(t.Context(), s, time.Hour, 2, []Exception{e},
					instant(t, "2027-03-13T08:00:00Z"), instant(t, "2027-03-13T09:00:00Z"), 100)
				requirements.NoError(err)
				assertions.Empty(got)
			}
		})
	}
}

func TestExpandExceptionInvalidDates(t *testing.T) {
	for _, e := range []Exception{
		{StartDate: "2027-02-30", EndDate: "2027-03-15"},
		{StartDate: "2027-03-13", EndDate: "invalid"},
	} {
		t.Run(e.StartDate+"/"+e.EndDate, func(t *testing.T) {
			e.Original = timed(t, "2027-03-02T17:00:00Z")
			got, err := Expand(t.Context(), parsed(t, "RRULE:FREQ=DAILY;COUNT=1"), time.Hour, 0, []Exception{e},
				instant(t, "2027-03-01T00:00:00Z"), instant(t, "2027-03-16T00:00:00Z"), 100)
			require.ErrorIs(t, err, ErrUnsupportedRecurrence)
			assert.Empty(t, got)
		})
	}
}

func FuzzExpandTimedException(f *testing.F) {
	f.Add(uint8(9), uint8(0), uint8(59))
	f.Fuzz(func(t *testing.T, hour, minute, durationMinutes uint8) {
		requirements := require.New(t)
		assertions := assert.New(t)
		s, err := ParseSet(time.Time{}, true, "2027-03-13", nil, []string{"RRULE:FREQ=DAILY;COUNT=1"})
		requirements.NoError(err)
		start := time.Date(2027, 6, 1, int(hour%24), int(minute%60), 0, 0, time.UTC)
		end := start.Add(time.Duration(int(durationMinutes)+1) * time.Minute)
		original := Identity{Date: "2027-03-13", AllDay: true}
		got, err := Expand(t.Context(), s, time.Hour, 1, []Exception{{Original: original, Start: &start, End: &end}}, start, end, 100)
		requirements.NoError(err)
		requirements.Len(got, 1)
		assertions.Equal(original, got[0].Original)
		assertions.False(got[0].AllDay)
		assertions.Equal(start, got[0].Start)
		assertions.Equal(end, got[0].End)
		assertions.Empty(got[0].StartDate)
		assertions.Empty(got[0].EndDate)
	})
}
