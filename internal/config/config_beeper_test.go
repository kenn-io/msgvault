package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadBeeperConfig(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	tmpDir := t.TempDir()
	t.Setenv("MSGVAULT_HOME", tmpDir)

	configContent := `
[beeper]
url = "http://localhost:9999"
enabled = true
schedule = "*/30 * * * *"
accounts = ["signal", "telegram"]
exclude_accounts = ["whatsapp"]
rate_limit_qps = 10
`
	configPath := filepath.Join(tmpDir, "config.toml")
	require.NoError(os.WriteFile(configPath, []byte(configContent), 0644))

	cfg, err := Load(configPath, "")
	require.NoError(err)

	assert.Equal("http://localhost:9999", cfg.Beeper.URL)
	assert.Equal("http://localhost:9999", cfg.Beeper.URL)
	assert.True(cfg.Beeper.Enabled)
	assert.Equal("*/30 * * * *", cfg.Beeper.Schedule)
	assert.Equal([]string{"signal", "telegram"}, cfg.Beeper.Accounts)
	assert.Equal([]string{"whatsapp"}, cfg.Beeper.ExcludeAccounts)
	assert.InEpsilon(10.0, cfg.Beeper.RateLimitQPS, 0.001)
}

func TestBeeperConfigDefaults(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	tmpDir := t.TempDir()
	t.Setenv("MSGVAULT_HOME", tmpDir)

	configPath := filepath.Join(tmpDir, "config.toml")
	require.NoError(os.WriteFile(configPath, []byte(""), 0644))

	cfg, err := Load(configPath, "")
	require.NoError(err)

	assert.Empty(cfg.Beeper.URL, "empty URL selects the client loopback default")
	assert.False(cfg.Beeper.Enabled)
	assert.Empty(cfg.Beeper.Schedule)
	assert.True(cfg.Beeper.MediaEnabled(), "media defaults on")
	assert.Equal(int64(100<<20), cfg.Beeper.MaxMediaBytes(), "default 100 MiB cap")
}

func TestBeeperMediaConfig(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	tmpDir := t.TempDir()
	t.Setenv("MSGVAULT_HOME", tmpDir)

	configContent := `
[beeper]
media = false
max_media_mb = 5
`
	configPath := filepath.Join(tmpDir, "config.toml")
	require.NoError(os.WriteFile(configPath, []byte(configContent), 0644))

	cfg, err := Load(configPath, "")
	require.NoError(err)
	assert.False(cfg.Beeper.MediaEnabled())
	assert.Equal(int64(5<<20), cfg.Beeper.MaxMediaBytes())
}

func TestBeeperAccountIncluded(t *testing.T) {
	tests := []struct {
		name      string
		cfg       BeeperConfig
		accountID string
		want      bool
	}{
		{"no filters includes all", BeeperConfig{}, "signal", true},
		{"include list match", BeeperConfig{Accounts: []string{"signal"}}, "signal", true},
		{"include list miss", BeeperConfig{Accounts: []string{"signal"}}, "telegram", false},
		{"exclude wins", BeeperConfig{ExcludeAccounts: []string{"whatsapp"}}, "whatsapp", false},
		{"exclude beats include", BeeperConfig{Accounts: []string{"whatsapp"}, ExcludeAccounts: []string{"whatsapp"}}, "whatsapp", false},
		{"exclude other passes", BeeperConfig{ExcludeAccounts: []string{"whatsapp"}}, "signal", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, tt.cfg.AccountIncluded(tt.accountID))
		})
	}
}
