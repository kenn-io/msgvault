package calevent

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestJoinKeyNormalization(t *testing.T) {
	for _, tt := range []struct{ url, want string }{
		{"https://meet.google.com/ABC-defg-HIJ?authuser=1#details", "meet:abc-defg-hij"},
		{"https://tenant.zoom.us/j/123456789?pwd=secret", "zoom:123456789"},
		{"https://zoom.us/j/123456789", "zoom:123456789"},
		{"https://teams.microsoft.com/l/meetup-join/19%3ameeting_example%40thread.v2/0?context=example", "teams:19:meeting_example@thread.v2"},
		{"https://teams.microsoft.com/l/meetup-join/19%3ameeting%2Fexample%40thread.v2/0", "teams:19:meeting/example@thread.v2"},
		{"https://teams.microsoft.com/l%2Fmeetup-join%2Fthread", ""},
		{"https://teams.microsoft.com/l/meetup-join%2Fthread", ""},
		{"https://evilzoom.us/j/123456789", ""},
		{"https://zoom.us.example.com/j/123456789", ""},
		{"https://example.com/meeting", ""},
		{"https://zoom.us/j/not-a-number", ""},
		{"https://meet.google.com/", ""},
		{"https://meet.google.com/code/extra", ""},
		{"mailto:meet.google.com/code", ""},
		{"not a URL", ""},
	} {
		t.Run(tt.url, func(t *testing.T) {
			assertions := assert.New(t)
			assertions.Equal(tt.want, JoinKey(tt.url))
		})
	}
}

func TestIsRoomAttendee(t *testing.T) {
	for _, tt := range []struct {
		email          string
		resource, want bool
	}{
		{"room@example.com", true, true},
		{"room@resource.calendar.google.com", false, true},
		{" Room@c.resource.calendar.google.com ", false, true},
		{"room@evilresource.calendar.google.com", false, false},
		{"room@resource.calendar.google.com.example.com", false, false},
		{"person@example.com", false, false},
		{"", false, false},
	} {
		t.Run(tt.email, func(t *testing.T) {
			assertions := assert.New(t)
			assertions.Equal(tt.want, IsRoomAttendee(tt.email, tt.resource))
		})
	}
}
