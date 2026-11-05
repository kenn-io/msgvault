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
		{"Room@RESOURCE.Calendar.Google.COM", false, true},
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

func TestIsRoomDomain(t *testing.T) {
	for _, tt := range []struct {
		domain string
		want   bool
	}{
		{"resource.calendar.google.com", true},
		{"c.resource.calendar.google.com", true},
		{"RESOURCE.Calendar.Google.COM", true},
		{"notresource.calendar.google.com", false},
		{"resource.calendar.google.com.example.com", false},
		{"", false},
	} {
		t.Run(tt.domain, func(t *testing.T) {
			assert.Equal(t, tt.want, IsRoomDomain(tt.domain))
		})
	}
}
