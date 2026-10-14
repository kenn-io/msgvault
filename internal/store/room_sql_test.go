package store

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/calevent"
)

func TestRoomSQLMatchesSharedClassifier(t *testing.T) {
	st := newRFC822IDBackfillBackendStore(t)
	for _, email := range []string{"room@resource.calendar.google.com", " ROOM@c.resource.calendar.google.com ", "room@notresource.calendar.google.com", "room@resource.calendar.google.com.example.org", "resource.calendar.google.com", "room@a@resource.calendar.google.com", "human@example.org", ""} {
		var got bool
		require.NoError(t, st.DB().QueryRow(st.Rebind(`SELECT `+RoomAddressSQL("input.email")+` FROM (SELECT ? AS email) input`), email).Scan(&got))
		assert.Equal(t, calevent.IsRoomAttendee(email, false), got, email)
	}
}

func TestRoomAddressLookupUsesNormalizedEmailIndex(t *testing.T) {
	st, err := OpenForTest(filepath.Join(t.TempDir(), "room-index.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })
	require.NoError(t, st.InitSchemaContext(t.Context()))

	plan := explainPlan(t, st, `SELECT EXISTS (
		SELECT 1 FROM calendar_event_attendees room_attendee
		WHERE room_attendee.role = 'room'
		  AND lower(trim(room_attendee.email)) = lower(trim(?))
		  AND EXISTS (SELECT 1 FROM messages room_message
		    WHERE room_message.id = room_attendee.message_id
		      AND room_message.message_type = 'calendar_event'
		      AND room_message.deleted_at IS NULL)
	)`, "room@example.org")
	assert.Contains(t, plan, "idx_calendar_event_attendees_room_email",
		"room classifier should seek attendee evidence by normalized email:\n%s", plan)
}
