package granola

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/meetingcontent"
	"go.kenn.io/msgvault/internal/store"
)

func TestImportedGranolaContextPreservesParticipantProvenance(t *testing.T) {
	for _, tc := range []struct{ name, attendees, want string }{
		{"invitees", "[]", "invited"},
		{"attendees", `[{"name":"Guest Example","email":"guest@example.org"}]`, "provider_attendee"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			raw := []byte(fmt.Sprintf(`{"id":"context-note","title":"Planning","created_at":"2027-03-14T10:00:00Z","updated_at":"2027-03-14T11:00:00Z","owner":{"email":"host@example.com"},"attendees":%s,"calendar_event":{"organiser":"host@example.com","scheduled_start_time":"2027-03-14T10:00:00Z","invitees":[{"email":"guest@example.org"}]},"summary_markdown":"Planning summary"}`, tc.attendees))
			imp, st := newTestImporter(t, &fakeAPI{notes: map[string][]byte{"context-note": raw}})
			_, err := imp.Import(t.Context(), ImportOptions{Identifier: "host@example.com", AccountEmail: "host@example.com"})
			requirements.NoError(err)
			var id int64
			requirements.NoError(st.DB().QueryRow(`SELECT id FROM messages WHERE source_message_id = 'context-note'`).Scan(&id))
			ids := []int64{id}
			result, err := st.GetMeetingContextContext(t.Context(), store.MeetingQueryScope{MessageIDs: &ids}, meetingcontent.PacketOptions{Format: meetingcontent.FormatJSON, MaxBytes: 16384})
			requirements.NoError(err)
			var packet meetingcontent.Packet
			requirements.NoError(json.Unmarshal([]byte(result.Content), &packet))
			requirements.Len(packet.Meetings, 1)
			var guests []meetingcontent.Participant
			for _, p := range packet.Meetings[0].Participants {
				if p.Email == "guest@example.org" {
					guests = append(guests, p)
				}
			}
			requirements.Len(guests, 1)
			assertions.Equal(tc.want, guests[0].Provenance)
			assertions.NotNil(guests[0].ParticipantID)
		})
	}
}
