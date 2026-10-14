package meetingcontent

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGranolaFallbackSkipsRoomParticipants(t *testing.T) {
	content := Decode("granola_json", []byte(`{
 "owner":{"email":"owner@example.org","name":"Example Owner"},
 "calendar_event":{"organiser":"owner@example.org","invitees":[
 {"email":"room@resource.calendar.google.com"},{"email":"human@example.org"}]}
 }`), nil)
	require.Len(t, content.SourceParticipants, 2)
	for _, p := range content.SourceParticipants {
		assert.NotEqual(t, "room@resource.calendar.google.com", p.Email)
	}
}
