package config

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/BurntSushi/toml"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func loadPollFixture(t *testing.T, accounts string) (*Config, error) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	require.NoError(t, os.WriteFile(path, []byte(accounts), 0o600))
	return Load(path, dir)
}

// Ignoring a misspelled or unsupported interval would silently disable freshness.
func TestAccountPollingRejectsInvalidIntervals(t *testing.T) {
	for _, interval := range []string{"1s", "121s", "30.5s", "-30s", "invalid"} {
		t.Run(interval, func(t *testing.T) {
			_, err := loadPollFixture(t, fmt.Sprintf(`[[accounts]]
email = "sample@example.test"
schedule = "0 * * * *"
enabled = true
poll_interval = %q
`, interval))
			assert.ErrorContains(t, err, "poll_interval")
		})
	}
}

func TestAccountPollingRequiresCronBackstop(t *testing.T) {
	for _, schedule := range []string{"", "@every 30s", "* * * * * *", "invalid"} {
		t.Run(schedule, func(t *testing.T) {
			_, err := loadPollFixture(t, fmt.Sprintf(`[[accounts]]
email = "sample@example.test"
schedule = %q
enabled = true
poll_interval = "30s"
`, schedule))
			assert.ErrorContains(t, err, "schedule")
		})
	}
}

func TestAccountPollingRejectsDuplicateEnabledKeys(t *testing.T) {
	_, err := loadPollFixture(t, `[[accounts]]
email = "sample@example.test"
schedule = "0 * * * *"
enabled = true
poll_interval = "30s"
[[accounts]]
email = "sample@example.test"
schedule = "0 2 * * *"
enabled = true
`)
	assert.ErrorContains(t, err, "duplicate")
}

func TestAccountPollingSaveLoad(t *testing.T) {
	cfg, err := loadPollFixture(t, `[[accounts]]
email = "sample@example.test"
schedule = "0 * * * *"
enabled = true
poll_interval = "30s"
`)
	require.NoError(t, err)
	require.NoError(t, cfg.Save())
	reloaded, err := Load(cfg.configPath, cfg.HomeDir)
	require.NoError(t, err)
	var encoded bytes.Buffer
	require.NoError(t, toml.NewEncoder(&encoded).Encode(reloaded))
	var saved struct {
		Accounts []struct {
			PollInterval string `toml:"poll_interval"`
		} `toml:"accounts"`
	}
	_, err = toml.Decode(encoded.String(), &saved)
	require.NoError(t, err)
	require.Len(t, saved.Accounts, 1)
	assert.Equal(t, "30s", saved.Accounts[0].PollInterval)
}

func TestAccountPollingOffRemainsValid(t *testing.T) {
	for _, setting := range []string{"", `poll_interval = "0s"`} {
		_, err := loadPollFixture(t, "[[accounts]]\nemail = \"sample@example.test\"\nenabled = false\n"+setting+"\n")
		assert.NoError(t, err)
	}
}
