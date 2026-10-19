package store

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/calevent"
)

func TestListCalendarOccurrencesCancellationInheritance(t *testing.T) {
	for _, tc := range []struct {
		name, fields, subject string
		want                  []calevent.Attendee
	}{
		{"omitted attendees", ``, "Example event", []calevent.Attendee{
			{Email: "guest@example.com", Role: "required", RSVP: "accepted"},
			{Email: "owner@example.com", DisplayName: "Series Owner", Role: "organizer", RSVP: "accepted", IsCopySelf: true},
		}},
		{"unchanged organizer", `"organizer":{"email":" OWNER@EXAMPLE.COM "}`, "Example event", []calevent.Attendee{
			{Email: "guest@example.com", Role: "required", RSVP: "accepted"},
			{Email: "owner@example.com", DisplayName: "Series Owner", Role: "organizer", RSVP: "accepted", IsCopySelf: true},
		}},
		{"attendee marked organizer", `"attendees":[{"email":"owner@example.com","organizer":true,"responseStatus":"declined","displayName":"Cancellation Owner","self":true}]`, "Example event", []calevent.Attendee{
			{Email: "owner@example.com", DisplayName: "Cancellation Owner", Role: "organizer", RSVP: "declined", IsCopySelf: true},
		}},
		{"email-less attendees", `"attendees":[{"displayName":"First Guest","responseStatus":"accepted"},{"displayName":"Second Guest","responseStatus":"declined"}]`, "Example event", []calevent.Attendee{
			{DisplayName: "First Guest", Role: "required", RSVP: "accepted"},
			{DisplayName: "Second Guest", Role: "required", RSVP: "declined"},
			{Email: "owner@example.com", DisplayName: "Series Owner", Role: "organizer", RSVP: "accepted", IsCopySelf: true},
		}},
		{"replacement attendees", `"summary":"Cancelled planning","attendees":[{"email":"other@example.com","responseStatus":"declined"}]`, "Cancelled planning", []calevent.Attendee{
			{Email: "other@example.com", Role: "required", RSVP: "declined"},
			{Email: "owner@example.com", DisplayName: "Series Owner", Role: "organizer", RSVP: "accepted", IsCopySelf: true},
		}},
		{"empty attendees", `"summary":"","attendees":[]`, "", []calevent.Attendee{
			{Email: "owner@example.com", DisplayName: "Series Owner", Role: "organizer", RSVP: "accepted", IsCopySelf: true},
		}},
		{"organizer in attendees", `"attendees":[{"email":" OWNER@EXAMPLE.COM ","responseStatus":"declined","displayName":"Cancellation Owner","self":true}]`, "Example event", []calevent.Attendee{
			{Email: "owner@example.com", DisplayName: "Cancellation Owner", Role: "organizer", RSVP: "declined", IsCopySelf: true},
		}},
		{"replacement organizer", `"organizer":{"email":"new@example.com"}`, "Example event", []calevent.Attendee{
			{Email: "guest@example.com", Role: "required", RSVP: "accepted"},
			{Email: "new@example.com", Role: "organizer", RSVP: "unknown"},
		}},
		{"promoted attendee", `"organizer":{"email":" GUEST@EXAMPLE.COM "}`, "Example event", []calevent.Attendee{
			{Email: "guest@example.com", Role: "organizer", RSVP: "accepted"},
		}},
		{"supplied organizer and attendee details", `"organizer":{"email":"guest@example.com"},"attendees":[{"email":"guest@example.com","responseStatus":"accepted","displayName":"Cancellation Guest","self":true}]`, "Example event", []calevent.Attendee{
			{Email: "guest@example.com", DisplayName: "Cancellation Guest", Role: "organizer", RSVP: "accepted", IsCopySelf: true},
		}},
		{"empty organizer", `"organizer":{}`, "Example event", []calevent.Attendee{
			{Email: "guest@example.com", Role: "required", RSVP: "accepted"},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			_, master := calendarFixture(t, st, "master", `{"id":"weekly","status":"confirmed","start":{"dateTime":"2027-03-02T17:00:00Z"},"end":{"dateTime":"2027-03-02T18:00:00Z"},"recurrence":["RRULE:FREQ=WEEKLY;COUNT=2"],"organizer":{"email":"owner@example.com"},"attendees":[{"email":"guest@example.com","responseStatus":"accepted"},{"email":"owner@example.com","organizer":true,"responseStatus":"accepted","displayName":"Series Owner","self":true}]}`)
			fields := tc.fields
			if fields != "" {
				fields = "," + fields
			}
			_, cancelled := calendarFixture(t, st, "cancelled", `{"id":"cancelled","recurringEventId":"weekly","originalStartTime":{"dateTime":"2027-03-09T17:00:00Z"},"status":"cancelled"`+fields+`}`)
			subject := tc.subject
			if tc.name == "omitted attendees" {
				subject = ""
			}
			_, err := st.db.Exec(`UPDATE messages SET subject = ? WHERE id = ?`, subject, cancelled)
			require.NoError(err)
			from := time.Date(2027, 3, 9, 0, 0, 0, 0, time.UTC)
			page, err := st.ListCalendarOccurrencesContext(t.Context(), CalendarOccurrenceQuery{From: from, To: from.AddDate(0, 0, 1)})
			require.NoError(err)
			assert.Empty(page.Unexpandable)
			require.Len(page.Occurrences, 1)
			assert.Equal(cancelled, page.Occurrences[0].MessageID)
			assert.Equal(master, page.Occurrences[0].SeriesMessageID)
			assert.Equal(tc.subject, page.Occurrences[0].Subject)
			assert.Equal("cancelled", page.Occurrences[0].Status)
			assert.ElementsMatch(tc.want, page.Occurrences[0].Attendees)
		})
	}
}
