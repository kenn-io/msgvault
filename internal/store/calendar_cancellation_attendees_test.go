package store

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/calevent"
)

func TestCalendarCancellationDamagedEvidenceKeepsOccurrences(t *testing.T) {
	for _, format := range []string{"ics", "gcal_json"} {
		for _, bounded := range []bool{false, true} {
			for _, compression := range []string{"", "zlib"} {
				t.Run(fmt.Sprintf("%s/bounded=%t/compression=%s", format, bounded, compression), func(t *testing.T) {
					assertions, requirements := assert.New(t), require.New(t)
					st := newRFC822IDBackfillBackendStore(t)
					var master, cancelled, sibling int64
					if format == "ics" {
						source := icsSource(t, st)
						imported, err := st.PersistICalendarContext(t.Context(), source, calendarDocument("REQUEST", "DTSTART:20270302T120000Z\r\nDURATION:PT1H\r\nRRULE:FREQ=DAILY;COUNT=2\r\nSUMMARY:Master"))
						requirements.NoError(err)
						master = imported.MessageIDs[0]
						bounds := ""
						if bounded {
							bounds = "\r\nDTSTART:20270303T140000Z\r\nDURATION:PT1H"
						}
						imported, err = st.PersistICalendarContext(t.Context(), source, calendarDocument("CANCEL", "RECURRENCE-ID:20270303T120000Z\r\nSTATUS:CANCELLED\r\nSUMMARY:Cancellation"+bounds))
						requirements.NoError(err)
						cancelled = imported.MessageIDs[0]
					} else {
						_, master = calendarFixture(t, st, "master", `{"id":"master","start":{"dateTime":"2027-03-02T12:00:00Z"},"end":{"dateTime":"2027-03-02T13:00:00Z"},"recurrence":["RRULE:FREQ=DAILY;COUNT=2"]}`)
						bounds := ""
						if bounded {
							bounds = `,"start":{"dateTime":"2027-03-03T14:00:00Z"},"end":{"dateTime":"2027-03-03T15:00:00Z"}`
						}
						_, cancelled = calendarFixture(t, st, "cancelled", `{"id":"cancelled","recurringEventId":"master","originalStartTime":{"dateTime":"2027-03-03T12:00:00Z"},"status":"cancelled"`+bounds+`}`)
					}
					_, sibling = calendarFixture(t, st, "sibling", `{"id":"sibling","start":{"dateTime":"2027-03-03T16:00:00Z"},"end":{"dateTime":"2027-03-03T17:00:00Z"}}`)
					_, err := st.db.Exec(`UPDATE message_raw SET raw_data=?, compression=? WHERE message_id=?`, []byte("unreadable retained event"), sql.NullString{String: compression, Valid: compression != ""}, cancelled)
					requirements.NoError(err)

					from := time.Date(2027, 3, 3, 0, 0, 0, 0, time.UTC)
					page, err := st.ListCalendarOccurrencesContext(t.Context(), from, from.AddDate(0, 0, 1))
					requirements.NoError(err)
					requirements.Len(page.Occurrences, 2)
					assertions.Equal(cancelled, page.Occurrences[0].MessageID)
					assertions.Equal(master, page.Occurrences[0].SeriesMessageID)
					assertions.Equal("cancelled", page.Occurrences[0].Status)
					wantStart, wantEnd := "2027-03-03T12:00:00Z", "2027-03-03T13:00:00Z"
					if bounded {
						wantStart, wantEnd = "2027-03-03T14:00:00Z", "2027-03-03T15:00:00Z"
						assertions.Empty(page.Unexpandable)
					} else {
						requirements.Len(page.Unexpandable, 1)
						assertions.Equal(cancelled, page.Unexpandable[0].MessageID)
						assertions.NotEmpty(page.Unexpandable[0].Reason)
					}
					assertions.Equal(wantStart, page.Occurrences[0].Start.Format(time.RFC3339))
					assertions.Equal(wantEnd, page.Occurrences[0].End.Format(time.RFC3339))
					assertions.Equal(sibling, page.Occurrences[1].MessageID)
					assertions.Equal("confirmed", page.Occurrences[1].Status)
				})
			}
		}
	}
}

func TestInheritCalendarCancellationReadErrorsPropagate(t *testing.T) {
	requirements := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, id := calendarFixture(t, st, "cancelled", `{"id":"cancelled","recurringEventId":"master","originalStartTime":{"dateTime":"2027-03-03T12:00:00Z"},"status":"cancelled"}`)
	row := CalendarEventRow{MessageID: id}
	_, err := st.db.Exec(`DELETE FROM message_raw WHERE message_id=?`, id)
	requirements.NoError(err)
	_, diagnostic, err := inheritCalendarCancellation(t.Context(), st.db, row, CalendarEventRow{})
	requirements.ErrorIs(err, sql.ErrNoRows)
	requirements.Empty(diagnostic)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, diagnostic, err = inheritCalendarCancellation(ctx, st.db, row, CalendarEventRow{})
	requirements.ErrorIs(err, context.Canceled)
	requirements.Empty(diagnostic)
}

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
			page, err := st.ListCalendarOccurrencesContext(t.Context(), from, from.AddDate(0, 0, 1))
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
