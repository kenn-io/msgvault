package store

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/calrecur"
)

func TestCalendarFractionalOriginalKeepsWholeSecondCancellation(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, master := calendarFixture(t, st, "master", `{"id":"weekly","start":{"dateTime":"2027-03-02T17:00:00Z"},"end":{"dateTime":"2027-03-02T18:00:00Z"},"recurrence":["RRULE:FREQ=WEEKLY;COUNT=3"]}`)
	_, cancelled := calendarFixture(t, st, "cancelled", `{"id":"cancelled","recurringEventId":"weekly","originalStartTime":{"dateTime":"2027-03-09T17:00:00Z"},"status":"cancelled"}`)
	_, fractional := calendarFixture(t, st, "fractional", `{"id":"fractional","recurringEventId":"weekly","originalStartTime":{"dateTime":"2027-03-09T17:00:00.123456Z"},"start":{"dateTime":"2027-03-09T19:00:00Z"},"end":{"dateTime":"2027-03-09T20:00:00Z"}}`)
	from := time.Date(2027, 3, 9, 0, 0, 0, 0, time.UTC)
	page, err := st.ListCalendarOccurrencesContext(t.Context(), from, from.AddDate(0, 0, 1))
	requirements.NoError(err)
	requirements.Len(page.Occurrences, 2)
	assertions.Equal(cancelled, page.Occurrences[0].MessageID)
	assertions.Equal("cancelled", page.Occurrences[0].Status)
	assertions.Equal(identityRef(master)+"@20270309T170000Z", page.Occurrences[0].Ref)
	assertions.Equal(fractional, page.Occurrences[1].MessageID)
	assertions.Equal("confirmed", page.Occurrences[1].Status)
	assertions.Equal(identityRef(fractional), page.Occurrences[1].Ref)
	assertions.Equal(from.Add(19*time.Hour), page.Occurrences[1].Start)
	assertions.Equal(from.Add(20*time.Hour), page.Occurrences[1].End)
	ctx, release, err := st.BeginReadSnapshotContext(t.Context())
	requirements.NoError(err)
	defer release()
	budget := calrecur.RequestReadBudget
	rows, err := meetingEventRows(ctx, st.meetingReader(), "e.message_id=?", []any{master}, &budget)
	requirements.NoError(err)
	requirements.Len(rows, 1)
	expanded, err := expandMeetingSeries(ctx, st.meetingReader(), rows[0], from, from.AddDate(0, 0, 1), &budget)
	requirements.NoError(err)
	byRef := map[string]linkOccurrence{}
	for _, occurrence := range expanded {
		byRef[formatMeetingOccurrence(occurrence.Ref)] = occurrence
	}
	cancelledOccurrence := byRef[identityRef(master)+"@20270309T170000Z"]
	assertions.Equal(cancelled, cancelledOccurrence.Row.MessageID)
	assertions.Equal("cancelled", cancelledOccurrence.Row.Event.Status)
	fractionalOccurrence := byRef[identityRef(fractional)]
	assertions.Equal(meetingIdentity{EventMessageID: fractional, OccurrenceKey: "20270309T170000Z"}, fractionalOccurrence.Identity)
	assertions.Equal(from.Add(19*time.Hour), fractionalOccurrence.Start)
	assertions.Equal(from.Add(20*time.Hour), fractionalOccurrence.End)
}
