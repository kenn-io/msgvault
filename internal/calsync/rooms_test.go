package calsync

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/gcal"
)

func TestCalsyncSkipsRoomAttendeesAndOrganizer(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	mock := gcal.NewMockAPI()
	mock.Calendars = []gcal.Calendar{{ID: "primary", AccessRole: "owner", TimeZone: "UTC"}}
	event := timedEvent("planning", "Planning",
		gcal.Attendee{Email: "resource@example.com", Resource: true, Organizer: true},
		gcal.Attendee{Email: "room@c.resource.calendar.google.com"},
		gcal.Attendee{Email: "human@example.org", DisplayName: "Example Human"})
	event.Organizer = gcal.Person{Email: "resource@example.com", DisplayName: "Example Room"}
	mock.FullEvents["primary"] = [][]gcal.Event{{event}}
	syncer, st := newSyncer(t, mock, Options{})
	_, err := syncer.Full(context.Background())
	requirements.NoError(err)
	var count int64
	requirements.NoError(st.DB().QueryRow(`SELECT count(*) FROM participants WHERE email_address IN ('resource@example.com','room@c.resource.calendar.google.com')`).Scan(&count))
	assertions.Zero(count)
	requirements.NoError(st.DB().QueryRow(`SELECT count(*) FROM message_recipients mr WHERE recipient_type='to'`).Scan(&count))
	assertions.Equal(int64(1), count)
	requirements.NoError(st.DB().QueryRow(`SELECT count(*) FROM calendar_event_attendees WHERE role='room'`).Scan(&count))
	assertions.Equal(int64(2), count, "typed room evidence remains queryable")
}
