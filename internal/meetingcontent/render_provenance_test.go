package meetingcontent

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func FuzzRenderMergesParticipantProvenance(f *testing.F) {
	f.Add(true, uint8(0))
	f.Add(false, uint8(2))
	f.Fuzz(func(t *testing.T, upper bool, padding uint8) {
		assertions, requirements := assert.New(t), require.New(t)
		email := "guest@example.org"
		if upper {
			email = strings.ToUpper(email)
		}
		email = strings.Repeat(" ", int(padding%8)) + email + " "
		entry := Entry{
			Meeting:      MeetingRef{MessageID: 1},
			Participants: []Participant{{ParticipantID: new(int64(7)), Name: "Archived Guest", Email: email, Role: "to"}},
			Content:      Decode("granola_json", []byte(`{"calendar_event":{"invitees":[{"email":"guest@example.org"}]}}`), nil),
		}
		result, err := Render("synthetic-archive", []Entry{entry}, PacketOptions{Format: FormatJSON, MaxBytes: 16384})
		requirements.NoError(err)
		var packet Packet
		requirements.NoError(json.Unmarshal([]byte(result.Content), &packet))
		requirements.Len(packet.Meetings, 1)
		requirements.Len(packet.Meetings[0].Participants, 1)
		participant := packet.Meetings[0].Participants[0]
		assertions.Equal("invited", participant.Provenance)
		assertions.Equal("Archived Guest", participant.Name)
		assertions.Equal(entry.Participants[0].ParticipantID, participant.ParticipantID)
		assertions.Empty(entry.Participants[0].Provenance, "render must not mutate archived participants")
	})
}

func TestRenderEmptySourceProvenancePreservesArchivedLabel(t *testing.T) {
	entry := normalizeEntry(Entry{
		Participants: []Participant{{Email: "guest@example.org", Role: "to", Provenance: "provider_attendee"}},
		Content:      Content{SourceParticipants: []Participant{{Email: "guest@example.org", Role: "to"}}},
	})
	require.Len(t, entry.Participants, 1)
	assert.Equal(t, "provider_attendee", entry.Participants[0].Provenance)
}
