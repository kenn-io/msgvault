package persontiers

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExcludesAddressUsesLocalPartAndDomainPatterns(t *testing.T) {
	cfg := DefaultConfig()
	for _, tt := range []struct{ address, want string }{
		{"noreply@example.com", "automated_address"},
		{"NOREPLY@EXAMPLE.COM", "automated_address"},
		{"room-4@resource.calendar.google.com", "calendar_room"},
		{"ROOM-4@RESOURCE.CALENDAR.GOOGLE.COM", "calendar_room"},
		{"room-4@c.resource.calendar.google.com", "calendar_room"},
		{"ROOM-4@FOO.C.RESOURCE.CALENDAR.GOOGLE.COM", "calendar_room"},
		{"room-4@notresource.calendar.google.com", ""},
		{"room-4@resource.calendar.google.com.example.test", ""},
		{"room-4@calendar.google.com", ""},
		{"alice@example.com", ""}, {"alice@noreply.example.com", ""},
		{"noreply-human@example.com", ""}, {"noreply", ""}, {"", ""},
		{"noreply@resource.calendar.google.com", "calendar_room"},
	} {
		t.Run(tt.address, func(t *testing.T) { assert.Equal(t, tt.want, cfg.ExcludesAddress(tt.address)) })
	}
	cfg.ExcludeAddressPatterns = []string{"bot-*"}
	cfg.ExcludeDomainPatterns = []string{"rooms.*"}
	for _, tt := range []struct{ address, want string }{
		{"BOT-alerts@example.com", "automated_address"},
		{"bot@example.com", ""}, {"alice@ROOMS.EXAMPLE.COM", "calendar_room"},
		{"bot-alerts@rooms.example.com", "calendar_room"},
	} {
		t.Run(tt.address, func(t *testing.T) { assert.Equal(t, tt.want, cfg.ExcludesAddress(tt.address)) })
	}
}

func TestDomainSubdomainPatternsCanBeConfigured(t *testing.T) {
	assert := assert.New(t)
	cfg := DefaultConfig()
	cfg.ExcludeDomainPatterns = []string{"*.rooms.example.com"}
	require.NoError(t, cfg.Validate())
	assert.Equal("calendar_room", cfg.ExcludesAddress("room@floor.rooms.example.com"))
	assert.Empty(cfg.ExcludesAddress("room@rooms.example.com"))
	assert.Empty(cfg.ExcludesAddress("room@otherrooms.example.com"))
	assert.Empty(cfg.ExcludesAddress("room@c.resource.calendar.google.com"))
}
