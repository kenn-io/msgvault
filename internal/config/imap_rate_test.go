package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIMAPCommandRateConfig(t *testing.T) {
	for _, tt := range []struct {
		name, content string
		want          int
		invalid       bool
	}{
		{"default", "", 0, false},
		{"off", "[sync]\nimap_max_commands_per_minute = 0\n", 0, false},
		{"paced", "[sync]\nimap_max_commands_per_minute = 1800\n", 1800, false},
		{"negative", "[sync]\nimap_max_commands_per_minute = -1\n", 0, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			home := t.TempDir()
			path := filepath.Join(home, "config.toml")
			require.NoError(os.WriteFile(path, []byte(tt.content), 0o600))
			cfg, err := Load(path, home)
			if tt.invalid {
				require.ErrorContains(err, "sync.imap_max_commands_per_minute must be non-negative")
				return
			}
			require.NoError(err)
			cfg.Data.DataDir = home
			assert.Equal(tt.want, cfg.Sync.IMAPMaxCommandsPerMinute)
			require.NoError(cfg.Save())
			loaded, err := Load(path, home)
			require.NoError(err)
			assert.Equal(tt.want, loaded.Sync.IMAPMaxCommandsPerMinute)
		})
	}
}
