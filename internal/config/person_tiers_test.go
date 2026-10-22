package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/persontiers"
)

func TestPeopleTierDefaultsAndExplicitZeros(t *testing.T) {
	for _, tc := range []struct {
		name, content string
		defaults      bool
	}{
		{"omitted", "", true},
		{"empty", "[people.tiers]\n", true},
		{"explicit zero", `[people.tiers]
demotion_grace_days = 0
exclude_list_messages = false
tier1_lanes = []
tier2_lanes = []
[people.tiers.tier1]
breadth_min_channels_long = 0
breadth_min_meeting_days_long = 0
[people.tiers.tier2]
min_meeting_days_long = 0
`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			require.NoError(t, os.WriteFile(path, []byte(tc.content), 0o600))
			c, err := Load(path, "")
			require.NoError(t, err)
			assert.False(t, c.People.Tiers.Enabled)
			assert.False(t, c.People.Tiers.Apply)
			if tc.defaults {
				assert.Equal(t, persontiers.DefaultConfig(), c.People.Tiers)
			} else {
				assert.Zero(t, c.People.Tiers.DemotionGraceDays)
				assert.Zero(t, c.People.Tiers.Tier1.BreadthMinChannelsLong)
				assert.Zero(t, c.People.Tiers.Tier1.BreadthMinMeetingDaysLong)
				assert.Zero(t, c.People.Tiers.Tier2.MinMeetingDaysLong)
				assert.False(t, c.People.Tiers.ExcludeListMessages)
				assert.Empty(t, c.People.Tiers.Tier1Lanes)
			}
		})
	}
}

func TestPeopleTierConfigRejectsInvalidSettings(t *testing.T) {
	for _, content := range []string{
		"[people.tiers]\nshort_window_days=0",
		"[people.tiers]\nminimum_tier=1",
		"[people.tiers]\nexclude_address_patterns=['Alert*']",
		"[people.tiers]\ntier2_lanes=['tracking','enrichment']",
		"[people.tiers]\ntypo_window_days=30",
		"[people.tiers.tier1]\nmin_outbound_short=0",
	} {
		t.Run(content, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
			_, err := Load(path, "")
			assert.ErrorContains(t, err, "people.tiers")
		})
	}
}
