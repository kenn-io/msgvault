package granola

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGranolaFallbackSkipsRooms(t *testing.T) {
	var note Note
	require.NoError(t, note.UnmarshalJSON([]byte(`{"calendar_event":{"invitees":[{"email":"room@c.resource.calendar.google.com"},{"email":"human@example.org"}]}}`)))
	assert.Equal(t, []User{{Email: "human@example.org"}}, attendees(&note))
}

func TestGranolaAttendeesSkipRooms(t *testing.T) {
	note := Note{Attendees: []User{{Email: "room@resource.calendar.google.com"}, {Name: "Example Human", Email: "human@example.org"}}}
	assert.Equal(t, []User{{Name: "Example Human", Email: "human@example.org"}}, attendees(&note))
}
