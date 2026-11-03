package store

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/calevent"
)

func TestListedSparseCalDAVCancellationInheritsMasterDetails(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	masterRaw := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nBEGIN:VEVENT\r\n" +
		"UID:weekly@example.org\r\nDTSTART;VALUE=DATE:20270302\r\nDTEND;VALUE=DATE:20270303\r\n" +
		"RRULE:FREQ=DAILY;COUNT=2\r\nSUMMARY:Master title\r\nDESCRIPTION:Master description\r\n" +
		"LOCATION:Conference room\r\nURL:https://meet.example.org/weekly\r\n" +
		"ATTENDEE;CN=Guest;PARTSTAT=ACCEPTED:mailto:guest@example.org\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
	sourceID, masterID := caldavEventFixture(t, st, "weekly-master", masterRaw)
	cancellationRaw := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\n" +
		"BEGIN:VEVENT\r\nUID:weekly@example.org\r\nDTSTART;VALUE=DATE:20270302\r\nDTEND;VALUE=DATE:20270303\r\n" +
		"RRULE:FREQ=DAILY;COUNT=2\r\nSUMMARY:Master title\r\nDESCRIPTION:Master description\r\n" +
		"LOCATION:Conference room\r\nURL:https://meet.example.org/weekly\r\n" +
		"ATTENDEE;CN=Guest;PARTSTAT=ACCEPTED:mailto:guest@example.org\r\nEND:VEVENT\r\n" +
		"BEGIN:VEVENT\r\nUID:weekly@example.org\r\nRECURRENCE-ID;VALUE=DATE:20270303\r\n" +
		"STATUS:CANCELLED\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
	components, err := calevent.ParseCalDAVResource(t.Context(), []byte(cancellationRaw), "UTC")
	requirements.NoError(err)
	requirements.Len(components, 2)
	cancellationIDFromResource := components[1].Event.ProviderEventID
	cancellationSourceID, cancellationID := caldavEventFixture(t, st, "weekly-cancellation", cancellationRaw, cancellationIDFromResource)
	assertions.Equal(sourceID, cancellationSourceID)

	from := time.Date(2027, 3, 2, 0, 0, 0, 0, time.UTC)
	page, err := st.ListCalendarOccurrencesContext(t.Context(), CalendarOccurrenceQuery{
		From: from, To: from.AddDate(0, 0, 2),
	})
	requirements.NoError(err)
	requirements.Empty(page.Unexpandable)
	requirements.Len(page.Occurrences, 2)
	occurrence := page.Occurrences[1]
	assertions.Equal(cancellationID, occurrence.MessageID)
	assertions.Equal(masterID, occurrence.SeriesMessageID)
	assertions.Equal("cancelled", occurrence.Status)
	assertions.Equal("Master title", occurrence.Subject)
	requirements.Len(occurrence.Attendees, 1)
	assertions.Equal("guest@example.org", occurrence.Attendees[0].Email)

	detail, err := st.GetCalendarOccurrenceContext(t.Context(), occurrence.Ref)
	requirements.NoError(err)
	assertions.Equal(calevent.Content{
		Location:    "Conference room",
		Description: "Master description",
		JoinURL:     "https://meet.example.org/weekly",
	}, detail.Content)
}
