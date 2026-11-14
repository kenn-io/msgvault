package persontiers

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestExcludesAddressUsesLocalPartAndDomainPatterns(t *testing.T) {
	defaults := DefaultConfig()
	for _, tc := range []struct {
		name   string
		config Config
		rows   []struct{ address, want string }
	}{
		{"local", Config{ExcludeAddressPatterns: defaults.ExcludeAddressPatterns}, []struct{ address, want string }{
			{"noreply@example.com", "automated_address"},
			{"NOREPLY@EXAMPLE.COM", "automated_address"},
			{"alice@example.com", ""}, {"alice@noreply.example.com", ""},
			{"noreply-human@example.com", ""}, {"noreply", ""}, {"", ""},
		}},
		{"domain", Config{ExcludeDomainPatterns: defaults.ExcludeDomainPatterns}, []struct{ address, want string }{
			{"room-4@resource.calendar.google.com", "calendar_room"},
			{"ROOM-4@RESOURCE.CALENDAR.GOOGLE.COM", "calendar_room"},
			{"noreply@resource.calendar.google.com", "calendar_room"},
		}},
		{"custom local", Config{ExcludeAddressPatterns: []string{"bot-*"}}, []struct{ address, want string }{
			{"BOT-alerts@example.com", "automated_address"}, {"bot@example.com", ""},
			{"mybot-alerts@example.com", ""},
		}},
		{"custom domain", Config{ExcludeDomainPatterns: []string{"rooms.*"}}, []struct{ address, want string }{
			{"alice@ROOMS.EXAMPLE.COM", "calendar_room"},
			{"bot-alerts@rooms.example.com", "calendar_room"},
			{"alice@myrooms.example.com", ""},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, row := range tc.rows {
				t.Run(row.address, func(t *testing.T) {
					assert.Equal(t, row.want, tc.config.ExcludesAddress(row.address))
				})
			}
		})
	}
}
