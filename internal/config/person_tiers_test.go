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
		{"explicit zero", `[people.tiers]
exclude_list_messages = false
[people.tiers.tier1]
breadth_min_channels_long = 0
breadth_min_meeting_days_long = 0
[people.tiers.tier2]
min_meeting_days_long = 0
`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			path := filepath.Join(t.TempDir(), "config.toml")
			require.NoError(os.WriteFile(path, []byte(tc.content), 0o600))
			c, err := Load(path, "")
			require.NoError(err)
			if tc.defaults {
				assert.Equal(persontiers.DefaultConfig(), c.People.Tiers)
			} else {
				assert.Zero(c.People.Tiers.Tier1.BreadthMinChannelsLong)
				assert.Zero(c.People.Tiers.Tier1.BreadthMinMeetingDaysLong)
				assert.Zero(c.People.Tiers.Tier2.MinMeetingDaysLong)
				assert.False(c.People.Tiers.ExcludeListMessages)
			}
		})
	}
}

func TestPeopleTierConfigRejectsInvalidSettings(t *testing.T) {
	for _, content := range []string{
		"[people.tiers]\nshort_window_days=0",
		"[people.tiers]\nschedule='0 3 * * *'",
	} {
		t.Run(content, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
			_, err := Load(path, "")
			assert.ErrorContains(t, err, "people.tiers")
		})
	}
}

// Clients load leniently and keep daemon-only errors aside. Tier previews must
// still see their own errors, and only their own.
func TestPeopleTiersErrorKeepsOnlyTierErrors(t *testing.T) {
	for _, tc := range []struct {
		name, content string
		tierError     bool
	}{
		{"misspelled threshold", "[people.tiers.tier1]\nmin_outbound_shrt = 3\n", true},
		{"zero threshold", "[people.tiers.tier1]\nmin_outbound_short = 0\n", true},
		{"other daemon setting", "[people.identity_scoring]\nunknown_key = 1\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)
			path := filepath.Join(t.TempDir(), "config.toml")
			require.NoError(os.WriteFile(path, []byte(tc.content), 0o600))
			snapshot, err := ReadConfigFile(path)
			require.NoError(err)
			c, err := LoadClientConfigFile(snapshot, "")
			require.NoError(err)
			require.Error(c.DaemonConfigError())
			if tc.tierError {
				require.ErrorIs(c.PeopleTiersError(), persontiers.ErrInvalidConfig)
			} else {
				require.NoError(c.PeopleTiersError())
			}
		})
	}
}
