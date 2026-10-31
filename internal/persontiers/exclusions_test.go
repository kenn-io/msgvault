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
	assert.NoError(cfg.Validate())
	assert.Equal("calendar_room", cfg.ExcludesAddress("room@floor.rooms.example.com"))
	assert.Empty(cfg.ExcludesAddress("room@rooms.example.com"))
	assert.Empty(cfg.ExcludesAddress("room@otherrooms.example.com"))
	assert.Empty(cfg.ExcludesAddress("room@c.resource.calendar.google.com"))
	cfg.ExcludeAddressPatterns = []string{"*.bot"}
	assert.ErrorIs(cfg.Validate(), ErrInvalidConfig)
}

func TestExcludedPreservesMultiAddressAndPhoneRules(t *testing.T) {
	cfg := DefaultConfig()
	for _, tt := range []struct {
		name    string
		signals Signals
		want    string
	}{
		{"all automated across room and email", Signals{Emails: []string{"noreply@resource.calendar.google.com", "noreply@example.com"}}, "automated_address"},
		{"room overrides phone", Signals{Emails: []string{"noreply@resource.calendar.google.com"}, HasPhoneOrChat: true}, "calendar_room"},
		{"phone defeats automated", Signals{Emails: []string{"noreply@example.com"}, HasPhoneOrChat: true}, ""},
		{"human and automated", Signals{Emails: []string{"alice@example.com", "noreply@example.com"}}, ""},
		{"mixed exclusions retain human eligibility", Signals{Emails: []string{"room-4@resource.calendar.google.com", "noreply@example.com"}}, ""},
		{"owner", Signals{Owner: true, Emails: []string{"alice@example.com"}}, "owner"},
		{"no email", Signals{}, ""},
	} {
		t.Run(tt.name, func(t *testing.T) { assert.Equal(t, tt.want, excluded(tt.signals, cfg)) })
	}
}
