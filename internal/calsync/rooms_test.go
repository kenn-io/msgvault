package calsync

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/gcal"
)

func TestCalsyncSkipsRoomAttendeesAndOrganizer(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	mock := gcal.NewMockAPI()
	mock.Calendars = []gcal.Calendar{{ID: "primary", AccessRole: "owner", TimeZone: "UTC"}}
	event := timedEvent("planning", "Planning",
		gcal.Attendee{Email: "resource@example.com", Resource: true, Organizer: true},
		gcal.Attendee{Email: "room@c.resource.calendar.google.com"},
		gcal.Attendee{Email: "human@example.org", DisplayName: "Example Human"})
	event.Organizer = gcal.Person{Email: "RESOURCE@example.com", DisplayName: "Example Room"}
	mock.FullEvents["primary"] = [][]gcal.Event{{event}}
	syncer, st := newSyncer(t, mock, Options{})
	_, err := syncer.Full(t.Context())
	requirements.NoError(err)
	var count int
	requirements.NoError(st.DB().QueryRow(`SELECT count(*) FROM participants WHERE lower(email_address) IN ('resource@example.com','room@c.resource.calendar.google.com')`).Scan(&count))
	assertions.Zero(count)
	requirements.NoError(st.DB().QueryRow(`SELECT count(*) FROM message_recipients WHERE recipient_type = 'to'`).Scan(&count))
	assertions.Equal(1, count)
	requirements.NoError(st.DB().QueryRow(`SELECT count(*) FROM calendar_event_attendees WHERE role = 'room'`).Scan(&count))
	assertions.Equal(2, count)
	requirements.NoError(st.DB().QueryRow(`SELECT count(*) FROM messages WHERE sender_id IS NOT NULL`).Scan(&count))
	assertions.Zero(count)
	if !st.IsPostgreSQL() {
		requirements.NoError(st.DB().QueryRow(`SELECT count(*) FROM messages_fts WHERE messages_fts MATCH 'to_addr:resource'`).Scan(&count))
		assertions.Equal(1, count, "room addresses remain searchable")
	}
}
