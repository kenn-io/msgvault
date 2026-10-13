package calrecur

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTimedRecurrenceFoldBounds(t *testing.T) {
	for _, tc := range []struct {
		name, start, until string
		want               []string
	}{
		{"explicit later DTSTART", "2027-11-07T09:30:00Z", "2027-11-09T00:00:00Z", []string{"20271107T093000Z", "20271108T093000Z"}},
		{"horizon in repeated hour", "2027-11-06T08:30:00Z", "2027-11-07T09:15:00Z", []string{"20271106T083000Z", "20271107T083000Z"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requirements := require.New(t)
			zone, err := time.LoadLocation("America/Los_Angeles")
			requirements.NoError(err)
			s, err := ParseSet(instant(t, tc.start), false, "", zone, []string{"RRULE:FREQ=DAILY;COUNT=2"})
			requirements.NoError(err)
			var got []string
			err = Iter(t.Context(), s, instant(t, tc.until), 100, func(i Instance) bool {
				got = append(got, i.Identity.Key())
				return true
			})
			requirements.NoError(err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestTimedRecurrenceFoldUsesFirstOccurrence(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	zone, err := time.LoadLocation("Europe/Berlin")
	requirements.NoError(err)
	start := instant(t, "2027-10-30T00:30:00Z")

	s, err := ParseSet(start, false, "", zone, []string{
		"RRULE:FREQ=DAILY;COUNT=3",
		"EXDATE:20271031T003000Z",
	})
	requirements.NoError(err)
	var got []string
	err = Iter(t.Context(), s, instant(t, "2027-11-02T00:00:00Z"), 100, func(i Instance) bool {
		got = append(got, i.Identity.Key())
		return true
	})
	requirements.NoError(err)
	assertions.Equal([]string{"20271030T003000Z", "20271101T013000Z"}, got)

	s, err = ParseSet(start, false, "", zone, []string{"RRULE:FREQ=DAILY;COUNT=3"})
	requirements.NoError(err)
	got = nil
	err = Iter(t.Context(), s, instant(t, "2027-10-31T01:00:00Z"), 100, func(i Instance) bool {
		got = append(got, i.Identity.Key())
		return true
	})
	requirements.NoError(err)
	assertions.Equal([]string{"20271030T003000Z", "20271031T003000Z"}, got)

	s, err = ParseSet(start, false, "", zone, []string{"RRULE:FREQ=DAILY;COUNT=2"})
	requirements.NoError(err)
	end, bounded := ConservativeEnd(t.Context(), s, time.Hour, 0, 100)
	requirements.True(bounded)
	assertions.Equal(instant(t, "2027-10-31T01:30:00Z"), end.UTC())
}

func TestTimedRecurrenceGapHonorsUntilAndBudget(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	zone, err := time.LoadLocation("America/Los_Angeles")
	requirements.NoError(err)
	start := instant(t, "2027-03-07T10:30:00Z")
	s, err := ParseSet(start, false, "", zone, []string{"RRULE:FREQ=WEEKLY;COUNT=2;UNTIL=20270314T110000Z"})
	requirements.NoError(err)
	var got []string
	err = Iter(t.Context(), s, instant(t, "2027-03-22T00:00:00Z"), 100, func(i Instance) bool {
		got = append(got, i.Identity.Key())
		return true
	})
	requirements.NoError(err)
	assertions.Equal([]string{"20270307T103000Z"}, got)
	end, bounded := ConservativeEnd(t.Context(), s, time.Hour, 0, 100)
	requirements.True(bounded)
	assertions.Equal(instant(t, "2027-03-07T11:30:00Z"), end.UTC())
	s, err = ParseSet(start, false, "", zone, []string{"RRULE:FREQ=WEEKLY;COUNT=2"})
	requirements.NoError(err)
	err = Iter(t.Context(), s, instant(t, "2027-03-22T00:00:00Z"), 3, func(Instance) bool { return true })
	requirements.ErrorIs(err, ErrBudgetExceeded)
	_, bounded = ConservativeEnd(t.Context(), s, time.Hour, 0, 3)
	assertions.False(bounded)
}
