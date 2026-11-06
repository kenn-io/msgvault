package store

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/calrecur"
)

func TestMeetingVersionlessCancellationOverridesVersionedException(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, master := calendarFixture(t, st, "master", linkSeriesRaw)
	original := `"originalStartTime":{"dateTime":"2027-03-14T10:00:00Z"}`
	calendarFixture(t, st, "confirmed", `{"id":"edited","iCalUID":"weekly@example.org","recurringEventId":"series",`+original+`,"start":{"dateTime":"2027-03-14T12:00:00Z"},"end":{"dateTime":"2027-03-14T13:00:00Z"},"updated":"2027-03-10T12:00:00Z","sequence":7}`)
	_, cancelled := calendarFixture(t, st, "cancelled", `{"id":"cancelled","iCalUID":"weekly@example.org","recurringEventId":"series",`+original+`,"status":"cancelled"}`)

	from := time.Date(2027, 3, 14, 10, 0, 0, 0, time.UTC)
	page, err := st.ListCalendarOccurrencesContext(t.Context(), from, from.Add(time.Hour))
	requirements.NoError(err)
	requirements.Len(page.Occurrences, 1)
	assertions.Equal(cancelled, page.Occurrences[0].MessageID)
	assertions.Equal(master, page.Occurrences[0].SeriesMessageID)
	assertions.Equal("cancelled", page.Occurrences[0].Status)
	assertions.Equal(from, page.Occurrences[0].Start)
	assertions.Equal(from.Add(time.Hour), page.Occurrences[0].End)

	ctx, release, err := st.BeginReadSnapshotContext(t.Context())
	requirements.NoError(err)
	defer release()
	budget := calrecur.RequestReadBudget
	occurrences, err := st.automaticMeetingOccurrences(ctx, st.meetingReader(), meetingLinkKey{
		Kind:        "ical_uid",
		Value:       "weekly@example.org",
		ScheduledAt: &from,
	}, &budget)
	requirements.NoError(err)
	requirements.Len(occurrences, 1)
	assertions.Equal(cancelled, occurrences[0].Row.MessageID)
	assertions.Equal("cancelled", occurrences[0].Row.Event.Status)
}
