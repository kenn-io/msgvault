package calrecur

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExplicitGapAndFold(t *testing.T) {
	for _, tc := range []struct{ property, value, want string }{
		{"RDATE", "TZID=America/New_York:20070311T023000", "2007-03-11T07:30:00Z"},
		{"EXDATE", "TZID=America/New_York:20070311T023000", "2007-03-11T07:30:00Z"},
		{"RDATE", "TZID=Europe/Berlin:20271031T023000", "2027-10-31T00:30:00Z"},
	} {
		t.Run(tc.property+tc.value, func(t *testing.T) {
			s, err := ParseSet(instant(t, "2007-03-01T00:00:00Z"), false, "", time.UTC, []string{tc.property + ";" + tc.value})
			require.NoError(t, err)
			ids := s.RDates
			if tc.property == "EXDATE" {
				ids = s.ExDates
			}
			require.Len(t, ids, 1)
			assert.Equal(t, instant(t, tc.want), ids[0].Instant)
		})
	}
}
func TestCountIncludesUnmatchedStart(t *testing.T) {
	s := parsed(t, "RRULE:FREQ=WEEKLY;BYDAY=MO;COUNT=3")
	got, _, err := Expand(t.Context(), s, time.Hour, 0, s.DTStart, instant(t, "2027-04-01T00:00:00Z"), 100)
	require.NoError(t, err)
	require.Len(t, got, 3)
	assert.Equal(t, instant(t, "2027-03-15T17:00:00Z"), got[2].Start)
}

func TestDTStartMismatchUntilOnly(t *testing.T) {
	s := parsed(t, "RRULE:FREQ=WEEKLY;BYDAY=MO;UNTIL=20270315T170000Z")
	got, _, err := Expand(t.Context(), s, time.Hour, 0, instant(t, "2027-03-01T00:00:00Z"), instant(t, "2027-04-01T00:00:00Z"), 100)
	require.NoError(t, err)
	var starts []string
	for _, o := range got {
		starts = append(starts, o.Start.UTC().Format(time.RFC3339))
	}
	assert.Equal(t, []string{"2027-03-02T17:00:00Z", "2027-03-08T17:00:00Z", "2027-03-15T17:00:00Z"}, starts)
}
