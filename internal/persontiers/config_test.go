package persontiers

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestConfigValidation(t *testing.T) {
	require.NoError(t, DefaultConfig().Validate())
	for _, tc := range []struct {
		name   string
		change func(*Config)
	}{
		{"short zero", func(c *Config) { c.ShortWindowDays = 0 }},
		{"long too large", func(c *Config) { c.LongWindowDays = 3651 }},
		{"inverted windows", func(c *Config) { c.ShortWindowDays = 366 }},
		{"one way short", func(c *Config) { c.Tier1.MinInboundShort = 0 }},
		{"one way long", func(c *Config) { c.Tier2.MinOutboundLong = 0 }},
		{"no active days", func(c *Config) { c.Tier1.MinActiveDaysShort = 0 }},
		{"negative alternative", func(c *Config) { c.Tier2.MinMeetingDaysLong = -1 }},
		{"one channel", func(c *Config) { c.Tier1.BreadthMinChannelsLong = 1 }},
		{"empty pattern", func(c *Config) { c.ExcludeAddressPatterns = []string{""} }},
		{"upper pattern", func(c *Config) { c.ExcludeAddressPatterns = []string{"Alert"} }},
		{"address pattern", func(c *Config) { c.ExcludeAddressPatterns = []string{"alert@example.com"} }},
		{"middle wildcard", func(c *Config) { c.ExcludeDomainPatterns = []string{"resource.*.example.com"} }},
		{"double wildcard", func(c *Config) { c.ExcludeAddressPatterns = []string{"alerts**"} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := DefaultConfig()
			tc.change(&c)
			assert.Error(t, c.Validate())
		})
	}
	c := DefaultConfig()
	c.Tier1.BreadthMinChannelsLong = 0
	c.Tier1.BreadthMinMeetingDaysLong = 0
	c.Tier2.MinMeetingDaysLong = 0
	require.NoError(t, c.Validate())
}
