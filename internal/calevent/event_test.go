package calevent

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

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
