package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPollingAccountConfig(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	path := filepath.Join(t.TempDir(), "config.toml")
	requirements.NoError(os.WriteFile(path, []byte("[[accounts]]\nemail = 'user@example.com'\nschedule = '0 * * * *'\nenabled = true\npoll_interval = '15s'\n"), 0600))
	cfg, err := Load(path, "")
	requirements.NoError(err)
	checks.Equal(15*time.Second, cfg.Accounts[0].PollInterval)
	requirements.NoError(cfg.Save())
	reloaded, err := Load(path, "")
	requirements.NoError(err)
	checks.Equal(15*time.Second, reloaded.Accounts[0].PollInterval)
	requirements.NoError(os.WriteFile(path, []byte("[[accounts]]\nemail = 'user@example.com'\nschedule = '0 * * * *'\nenabled = true\npoll_interval = '14s'\n"), 0600))
	_, err = Load(path, "")
	requirements.NoError(err)
}

func TestPollingAccountValidation(t *testing.T) {
	for _, tc := range []struct {
		name     string
		interval time.Duration
		want     string
	}{
		{"minimum", 15 * time.Second, ""}, {"maximum", 120 * time.Second, ""},
		{"too short", 14 * time.Second, "poll_interval"}, {"too long", 121 * time.Second, "poll_interval"}, {"fractional", 15500 * time.Millisecond, "poll_interval"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidatePollInterval(AccountSchedule{Email: "user@example.com", Enabled: true, Schedule: "0 * * * *", PollInterval: tc.interval})
			if tc.want == "" {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, tc.want)
			}
		})
	}
}
