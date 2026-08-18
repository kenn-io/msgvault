package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestActivityConfigDefaultsAndExplicitEmptySchedule(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	defaults := NewDefaultConfig().Activity
	assert.Equal("UTC", defaults.Timezone)
	assert.Equal(25, defaults.MaxDirectCounterparts)
	assert.Equal(500, defaults.BatchSize)
	assert.Equal("17 * * * *", defaults.Schedule)

	configPath := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(os.WriteFile(configPath, []byte(`
[activity]
timezone = "America/New_York"
max_direct_counterparts = 7
batch_size = 9
schedule = ""
`), 0o600))
	loaded, err := Load(configPath, "")
	require.NoError(err)
	assert.Equal("America/New_York", loaded.Activity.Timezone)
	assert.Equal(7, loaded.Activity.MaxDirectCounterparts)
	assert.Equal(9, loaded.Activity.BatchSize)
	assert.Empty(loaded.Activity.Schedule)
}

func TestActivityConfigValidation(t *testing.T) {
	tests := []struct {
		name   string
		config ActivityConfig
		ok     bool
	}{
		{
			name: "valid",
			config: ActivityConfig{
				Timezone: "Pacific/Kiritimati", MaxDirectCounterparts: 1,
				BatchSize: 1, Schedule: "*/5 * * * *",
			},
			ok: true,
		},
		{
			name: "disabled schedule",
			config: ActivityConfig{
				Timezone: "UTC", MaxDirectCounterparts: 25, BatchSize: 500,
			},
			ok: true,
		},
		{
			name: "invalid timezone",
			config: ActivityConfig{
				Timezone: "Not/AZone", MaxDirectCounterparts: 25, BatchSize: 500,
			},
		},
		{
			// time.LoadLocation accepts "Local", but replay keys on the
			// persisted name, so a host TZ change would silently skew new
			// projections without replaying existing rows.
			name: "host-dependent timezone",
			config: ActivityConfig{
				Timezone: "Local", MaxDirectCounterparts: 25, BatchSize: 500,
			},
		},
		{
			name: "negative counterparts",
			config: ActivityConfig{
				Timezone: "UTC", MaxDirectCounterparts: -1, BatchSize: 500,
			},
		},
		{
			name: "too many counterparts",
			config: ActivityConfig{
				Timezone: "UTC", MaxDirectCounterparts: 10_001, BatchSize: 500,
			},
		},
		{
			name: "negative batch",
			config: ActivityConfig{
				Timezone: "UTC", MaxDirectCounterparts: 25, BatchSize: -1,
			},
		},
		{
			name: "too large batch",
			config: ActivityConfig{
				Timezone: "UTC", MaxDirectCounterparts: 25, BatchSize: 10_001,
			},
		},
		{
			name: "six field schedule",
			config: ActivityConfig{
				Timezone: "UTC", MaxDirectCounterparts: 25, BatchSize: 500,
				Schedule: "* * * * * *",
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.config.Validate()
			if test.ok {
				assert.NoError(t, err)
			} else {
				assert.Error(t, err)
			}
		})
	}
}
