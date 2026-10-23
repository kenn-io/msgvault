package persontiers

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestConfigValidation(t *testing.T) {
	require.NoError(t, DefaultConfig().Validate())
	for _, tc := range []struct {
		name     string
		change   func(*Config)
		contains string
	}{
		{"short zero", func(c *Config) { c.ShortWindowDays = 0 }, "windows must satisfy"},
		{"long too large", func(c *Config) { c.LongWindowDays = 3651 }, "windows must satisfy"},
		{"inverted windows", func(c *Config) { c.ShortWindowDays = 366 }, "windows must satisfy"},
		{"one way short", func(c *Config) { c.Tier1.MinInboundShort = 0 }, "tier1.min_inbound_short must be at least 1"},
		{"one way long", func(c *Config) { c.Tier2.MinOutboundLong = 0 }, "tier2.min_outbound_long must be at least 1"},
		{"no active days", func(c *Config) { c.Tier1.MinActiveDaysShort = 0 }, "tier1.min_active_days_short must be at least 1"},
		{"negative alternative", func(c *Config) { c.Tier2.MinMeetingDaysLong = -1 }, "meeting thresholds must be 0"},
		{"one channel", func(c *Config) { c.Tier1.BreadthMinChannelsLong = 1 }, "breadth_min_channels_long must be 0 or at least 2"},
		{"empty pattern", func(c *Config) { c.ExcludeAddressPatterns = []string{""} }, `exclusion pattern ""`},
		{"upper pattern", func(c *Config) { c.ExcludeAddressPatterns = []string{"Alert"} }, `exclusion pattern "Alert"`},
		{"address pattern", func(c *Config) { c.ExcludeAddressPatterns = []string{"alert@example.com"} }, `exclusion pattern "alert@example.com"`},
		{"middle wildcard", func(c *Config) { c.ExcludeDomainPatterns = []string{"resource.*.example.com"} }, `exclusion pattern "resource.*.example.com"`},
		{"double wildcard", func(c *Config) { c.ExcludeAddressPatterns = []string{"alerts**"} }, `exclusion pattern "alerts**"`},
		{"first bad threshold in fixed order", func(c *Config) {
			c.Tier2.MinActiveDaysLong = 0
			c.Tier1.MinInboundShort = 0
			c.Tier1.MinOutboundShort = 0
		}, "tier1.min_outbound_short"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := DefaultConfig()
			tc.change(&c)
			err := c.Validate()
			require.Error(t, err)
			assert.ErrorContains(t, err, tc.contains)
		})
	}
}
