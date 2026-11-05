package store

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/calevent"
	"go.kenn.io/msgvault/internal/calrecur"
)

func TestCalendarFractionalExceptionPreservesGeneratedOccurrences(t *testing.T) {
	requirements, assertions := require.New(t), assert.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, master := calendarFixture(t, st, "master", `{"id":"weekly","iCalUID":"fractional@example.org","start":{"dateTime":"2027-03-02T17:00:00Z"},"end":{"dateTime":"2027-03-02T18:00:00Z"},"recurrence":["RRULE:FREQ=WEEKLY;COUNT=3"]}`)
	_, exception := calendarFixture(t, st, "fractional-edit", `{"id":"fractional-edit","iCalUID":"fractional@example.org","recurringEventId":"weekly","originalStartTime":{"dateTime":"2027-03-09T17:00:00.123456Z"},"start":{"dateTime":"2027-03-09T19:00:00Z"},"end":{"dateTime":"2027-03-09T20:00:00Z"}}`)
	_, second := calendarFixture(t, st, "weekly|2027-03-09T17:00:00Z", `{"id":"fractional-second","iCalUID":"fractional@example.org","recurringEventId":"weekly","originalStartTime":{"dateTime":"2027-03-09T17:00:00.654321Z"},"start":{"dateTime":"2027-03-09T21:00:00Z"},"end":{"dateTime":"2027-03-09T22:00:00Z"}}`)
	_, single := calendarFixture(t, st, "unrelated", `{"id":"unrelated","start":{"dateTime":"2027-03-10T12:00:00Z"},"end":{"dateTime":"2027-03-10T13:00:00Z"}}`)

	from := time.Date(2027, 3, 1, 0, 0, 0, 0, time.UTC)
	page, err := st.ListCalendarOccurrencesContext(t.Context(), from, from.AddDate(0, 1, 0))
	requirements.NoError(err)
	var ids []int64
	var starts []string
	for _, occurrence := range page.Occurrences {
		ids = append(ids, occurrence.MessageID)
		starts = append(starts, occurrence.Start.UTC().Format(time.RFC3339))
		if occurrence.MessageID == exception || occurrence.MessageID == second {
			assertions.Equal(fmt.Sprintf("evt:%d", occurrence.MessageID), occurrence.Ref)
		}
		if occurrence.MessageID == exception {
			assertions.Equal("2027-03-09T20:00:00Z", occurrence.End.UTC().Format(time.RFC3339))
		}
	}
	assertions.Equal([]int64{master, master, exception, second, single, master}, ids)
	assertions.Equal([]string{"2027-03-02T17:00:00Z", "2027-03-09T17:00:00Z", "2027-03-09T19:00:00Z", "2027-03-09T21:00:00Z", "2027-03-10T12:00:00Z", "2027-03-16T17:00:00Z"}, starts)
	assertions.Equal([]CalendarUnexpandable{{MessageID: exception, Reason: "exception original start must have whole-second precision"}, {MessageID: second, Reason: "exception original start must have whole-second precision"}}, page.Unexpandable)
}

func TestCalendarExceptionRowsPreservesFractionalIdentity(t *testing.T) {
	rows := []CalendarEventRow{
		fractionalExceptionRow(1, 123_456_000),
		fractionalExceptionRow(2, 123_456_000),
		fractionalExceptionRow(3, 654_321_000),
		fractionalExceptionRow(4, 0),
	}

	got := calendarExceptionRows(rows)
	require.Len(t, got, 3)
	assert.Equal(t, []int64{2, 3, 4}, []int64{got[0].MessageID, got[1].MessageID, got[2].MessageID})
}

func FuzzCalendarExceptionRowsPreservesFractionalIdentity(f *testing.F) {
	f.Add(uint32(0), uint32(1))
	f.Add(uint32(123_456_000), uint32(654_321_000))
	f.Fuzz(func(t *testing.T, firstNanos, secondNanos uint32) {
		if firstNanos >= 1_000_000_000 || secondNanos >= 1_000_000_000 || firstNanos == secondNanos {
			t.Skip()
		}

		got := calendarExceptionRows([]CalendarEventRow{
			fractionalExceptionRow(1, int(firstNanos)),
			fractionalExceptionRow(2, int(secondNanos)),
		})
		require.Len(t, got, 2)
	})
}

func fractionalExceptionRow(messageID int64, nanos int) CalendarEventRow {
	original := calrecur.Identity{
		Instant: time.Date(2027, time.March, 14, 10, 0, 0, nanos, time.UTC),
	}
	return CalendarEventRow{
		MessageID:                messageID,
		SourceID:                 1,
		ProviderObservationOrder: messageID,
		Event: calevent.Event{
			Kind:             meetingExceptionKind,
			SeriesProviderID: "weekly",
			Original:         &original,
		},
	}
}
