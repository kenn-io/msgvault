package meetingcontent

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestExplicitParticipants(t *testing.T) {
	for _, tc := range []struct {
		name, format, raw string
		want              []Participant
	}{
		{"circleback", "circleback_json", `{"meeting":{"organizer":{"email":"host@example.test"},"attendees":[{"email":"a@example.test"}]}}`, []Participant{{Email: "host@example.test", Role: "from"}, {Email: "a@example.test", Role: "to"}}},
		{"generic", "meeting_json", `{"organizer":{"email":"Host@Example.Test"},"attendees":[{"email":"a@example.test"},{"phone":"+15550100101"}]}`, []Participant{{Email: "host@example.test", Role: "from"}, {Email: "a@example.test", Role: "to"}, {Phone: "+15550100101", Role: "to"}}},
		{"granola", "granola_json", `{"calendar_event":{"organiser":"host@example.test","invitees":[{"email":"invitee@example.test"}]},"attendees":[{"email":"a@example.test"}]}`, []Participant{{Email: "host@example.test", Role: "from"}, {Email: "a@example.test", Role: "to"}}},
		{"owner and invitees", "granola_json", `{"owner":{"email":"owner@example.test"},"calendar_event":{"invitees":[{"email":"a@example.test"}]}}`, nil},
		{"explicit organizer only", "granola_json", `{"calendar_event":{"organiser":"host@example.test","invitees":[{"email":"a@example.test"}]}}`, []Participant{{Email: "host@example.test", Role: "from"}}},
		{"malformed", "meeting_json", `{"organizer":{"email":"host@example.test"},"attendees":[7]}`, nil},
		{"malformed alongside usable", "meeting_json", `{"organizer":{"email":"host@example.test"},"attendees":[{"email":"a@example.test"},7]}`, nil},
		{"null alongside usable", "meeting_json", `{"organizer":{"email":"host@example.test"},"attendees":[{"email":"a@example.test"},null]}`, nil},
		{"duplicate keys", "meeting_json", `{"organizer":{"email":"a@example.test"},"organizer":{"email":"b@example.test"}}`, nil},
		{"name only", "meeting_json", `{"attendees":[{"name":"A"}]}`, nil},
		{"unsupported", "omi_json", `{"organizer":{"email":"host@example.test"}}`, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := ExplicitParticipants(tc.format, []byte(tc.raw))
			if len(tc.want) == 0 {
				assert.Empty(t, got)
			} else {
				assert.Equal(t, tc.want, got)
			}
		})
	}
}
