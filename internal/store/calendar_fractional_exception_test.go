package store

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCalendarFractionalExceptionPreservesGeneratedOccurrences(t *testing.T) {
	requirements, assertions := require.New(t), assert.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, master := calendarFixture(t, st, "master", `{"id":"weekly","start":{"dateTime":"2027-03-02T17:00:00Z"},"end":{"dateTime":"2027-03-02T18:00:00Z"},"recurrence":["RRULE:FREQ=WEEKLY;COUNT=3"]}`)
	_, exception := calendarFixture(t, st, "fractional-edit", `{"id":"fractional-edit","recurringEventId":"weekly","originalStartTime":{"dateTime":"2027-03-09T17:00:00.123456Z"},"start":{"dateTime":"2027-03-09T19:00:00Z"},"end":{"dateTime":"2027-03-09T20:00:00Z"}}`)
	_, single := calendarFixture(t, st, "unrelated", `{"id":"unrelated","start":{"dateTime":"2027-03-10T12:00:00Z"},"end":{"dateTime":"2027-03-10T13:00:00Z"}}`)

	from := time.Date(2027, 3, 1, 0, 0, 0, 0, time.UTC)
	page, err := st.ListCalendarOccurrencesContext(t.Context(), from, from.AddDate(0, 1, 0))
	requirements.NoError(err)
	var ids []int64
	var starts []string
	for _, occurrence := range page.Occurrences {
		ids = append(ids, occurrence.MessageID)
		starts = append(starts, occurrence.Start.UTC().Format(time.RFC3339))
		if occurrence.MessageID == exception {
			assertions.Equal("2027-03-09T20:00:00Z", occurrence.End.UTC().Format(time.RFC3339))
		}
	}
	assertions.Equal([]int64{master, master, exception, single, master}, ids)
	assertions.Equal([]string{"2027-03-02T17:00:00Z", "2027-03-09T17:00:00Z", "2027-03-09T19:00:00Z", "2027-03-10T12:00:00Z", "2027-03-16T17:00:00Z"}, starts)
	assertions.Equal([]CalendarUnexpandable{{MessageID: exception, Reason: "exception original start must have whole-second precision"}}, page.Unexpandable)
}
