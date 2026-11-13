package meetingcontent

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExplicitParticipants(t *testing.T) {
	for _, tc := range []struct {
		name, format, raw string
		count             int
	}{
		{"circleback", "circleback_json", `{"meeting":{"organizer":{"email":"host@example.test"},"attendees":[{"email":"a@example.test"}]}}`, 2},
		{"generic", "meeting_json", `{"organizer":{"email":"Host@Example.Test"},"attendees":[{"email":"a@example.test"},{"phone":"+15550100101"}]}`, 3},
		{"granola", "granola_json", `{"calendar_event":{"organiser":"host@example.test","invitees":[{"email":"invitee@example.test"}]},"attendees":[{"email":"a@example.test"}]}`, 2},
		{"owner and invitees", "granola_json", `{"owner":{"email":"owner@example.test"},"calendar_event":{"invitees":[{"email":"a@example.test"}]}}`, 0},
		{"explicit organizer only", "granola_json", `{"calendar_event":{"organiser":"host@example.test","invitees":[{"email":"a@example.test"}]}}`, 1},
		{"malformed", "meeting_json", `{"organizer":{"email":"host@example.test"},"attendees":[7]}`, 0},
		{"malformed alongside usable", "meeting_json", `{"organizer":{"email":"host@example.test"},"attendees":[{"email":"a@example.test"},7]}`, 0},
		{"null alongside usable", "meeting_json", `{"organizer":{"email":"host@example.test"},"attendees":[{"email":"a@example.test"},null]}`, 0},
		{"duplicate keys", "meeting_json", `{"organizer":{"email":"a@example.test"},"organizer":{"email":"b@example.test"}}`, 0},
		{"name only", "meeting_json", `{"attendees":[{"name":"A"}]}`, 0},
		{"unsupported", "omi_json", `{"organizer":{"email":"host@example.test"}}`, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := ExplicitParticipants(tc.format, []byte(tc.raw))
			require.Len(t, got, tc.count)
			if tc.count > 0 {
				assert.Equal(t, "from", got[0].Role)
				assert.Equal(t, "host@example.test", got[0].Email)
			}
		})
	}
}
