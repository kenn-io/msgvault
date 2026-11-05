package persontiers

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestExcludesAddressUsesLocalPartAndDomainPatterns(t *testing.T) {
	cfg := DefaultConfig()
	for _, tt := range []struct{ address, want string }{
		{"noreply@example.com", "automated_address"},
		{"NOREPLY@EXAMPLE.COM", "automated_address"},
		{"room-4@resource.calendar.google.com", "calendar_room"},
		{"ROOM-4@RESOURCE.CALENDAR.GOOGLE.COM", "calendar_room"},
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
