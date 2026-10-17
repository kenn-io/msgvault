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
		{"negative grace", func(c *Config) { c.DemotionGraceDays = -1 }},
		{"excess grace", func(c *Config) { c.DemotionGraceDays = 366 }},
		{"minimum one", func(c *Config) { c.MinimumTier = 1 }},
		{"retention zero", func(c *Config) { c.PreviewRetentionDays = 0 }},
		{"one way short", func(c *Config) { c.Tier1.MinInboundShort = 0 }},
		{"one way long", func(c *Config) { c.Tier2.MinOutboundLong = 0 }},
		{"no active days", func(c *Config) { c.Tier1.MinActiveDaysShort = 0 }},
		{"negative alternative", func(c *Config) { c.Tier2.MinMeetingDaysLong = -1 }},
		{"one channel", func(c *Config) { c.Tier1.BreadthMinChannelsLong = 1 }},
		{"unknown lane", func(c *Config) { c.Tier1Lanes = []string{"contacts"} }},
		{"duplicate lane", func(c *Config) { c.Tier1Lanes = []string{"tracking", "tracking"} }},
		{"brief without tracking", func(c *Config) { c.Tier1Lanes = []string{"briefs"} }},
		{"tier two enrichment", func(c *Config) { c.Tier2Lanes = []string{"tracking", "enrichment"} }},
		{"non subset", func(c *Config) { c.Tier1Lanes = []string{}; c.Tier2Lanes = []string{"tracking"} }},
		{"empty pattern", func(c *Config) { c.ExcludeAddressPatterns = []string{""} }},
		{"upper pattern", func(c *Config) { c.ExcludeAddressPatterns = []string{"Alert"} }},
		{"address pattern", func(c *Config) { c.ExcludeAddressPatterns = []string{"alert@example.com"} }},
		{"middle wildcard", func(c *Config) { c.ExcludeDomainPatterns = []string{"resource.*.example.com"} }},
		{"double wildcard", func(c *Config) { c.ExcludeAddressPatterns = []string{"alerts**"} }},
		{"schedule", func(c *Config) { c.Schedule = "bad schedule" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := DefaultConfig()
			tc.change(&c)
			assert.Error(t, c.Validate())
		})
	}
	c := DefaultConfig()
	c.DemotionGraceDays = 0
	c.Tier1.BreadthMinChannelsLong = 0
	c.Tier1.BreadthMinMeetingDaysLong = 0
	c.Tier2.MinMeetingDaysLong = 0
	require.NoError(t, c.Validate())
}

func TestConfigFingerprint(t *testing.T) {
	assert := assert.New(t)
	c := DefaultConfig()
	fingerprint := c.Fingerprint()
	c.Enabled = true
	c.Apply = true
	c.Schedule = "0 3 * * *"
	c.PreviewRetentionDays = 20
	assert.Equal(fingerprint, c.Fingerprint())
	c.Tier1Lanes = []string{"enrichment", "briefs", "tracking"}
	assert.Equal(fingerprint, c.Fingerprint())
	c.ShortWindowDays = 89
	assert.NotEqual(fingerprint, c.Fingerprint())
	c = DefaultConfig()
	c.Tier1.MinInboundShort++
	assert.NotEqual(fingerprint, c.Fingerprint())
	c = DefaultConfig()
	c.ExcludeListMessages = false
	assert.NotEqual(fingerprint, c.Fingerprint())
}
