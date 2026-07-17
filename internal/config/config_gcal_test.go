package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadGCalSources(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	tmpDir := t.TempDir()
	t.Setenv("MSGVAULT_HOME", tmpDir)

	configContent := `
[[gcal]]
email = "dan@example.com"
oauth_app = "work"
calendars = ["primary", "team@group.calendar.google.com"]
schedule = "0 */6 * * *"
enabled = true

[[gcal]]
name = "secondary"
email = "alt@example.com"
enabled = false
`
	configPath := filepath.Join(tmpDir, "config.toml")
	require.NoError(os.WriteFile(configPath, []byte(configContent), 0644))

	cfg, err := Load(configPath, "")
	require.NoError(err)
	require.Len(cfg.GCal, 2)

	// First entry: name defaults to email (applyGCalDefaults).
	first := cfg.GCal[0]
	assert.Equal("dan@example.com", first.Name, "name defaults to email")
	assert.Equal("dan@example.com", first.Email)
	assert.Equal("work", first.OAuthApp)
	assert.Equal([]string{"primary", "team@group.calendar.google.com"}, first.Calendars)
	assert.True(first.Enabled)

	assert.Equal("secondary", cfg.GCal[1].Name, "explicit name preserved")

	// Lookup by name and by email.
	require.NotNil(cfg.GetGCalSource("dan@example.com"))
	require.NotNil(cfg.GetGCalSource("secondary"))
	assert.Equal("alt@example.com", cfg.GetGCalSource("secondary").Email)
	assert.Nil(cfg.GetGCalSource("nobody"))

	// Only the enabled+scheduled source is daemon-scheduled.
	scheduled := cfg.ScheduledGCalSources()
	require.Len(scheduled, 1)
	assert.Equal("dan@example.com", scheduled[0].Email)
}
