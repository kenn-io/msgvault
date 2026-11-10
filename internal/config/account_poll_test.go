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
	for _, tc := range []struct {
		name, interval string
		valid          bool
	}{
		{"off", "0s", true}, {"minimum", "15s", true}, {"maximum", "120s", true},
		{"too short", "14s", false}, {"too long", "121s", false}, {"fractional", "15.5s", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			require.NoError(t, os.WriteFile(path, []byte("[[accounts]]\nemail = 'user@example.com'\nschedule = '0 * * * *'\nenabled = true\npoll_interval = '"+tc.interval+"'\n"), 0600))
			cfg, err := Load(path, "")
			if !tc.valid {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			d, err := time.ParseDuration(tc.interval)
			require.NoError(t, err)
			assert.Equal(t, d, cfg.Accounts[0].PollInterval)
			require.NoError(t, cfg.Save())
			reloaded, err := Load(path, "")
			require.NoError(t, err)
			assert.Equal(t, d, reloaded.Accounts[0].PollInterval)
		})
	}
}

func TestPollingAccountValidation(t *testing.T) {
	account := AccountSchedule{Email: "user@example.com", Enabled: true, Schedule: "0 * * * *", PollInterval: 15 * time.Second}
	assert.NoError(t, ValidatePollingAccounts([]AccountSchedule{account, {Email: account.Email, Enabled: true, Schedule: account.Schedule}}))
	duplicate := account
	duplicate.Email = "USER@example.com"
	assert.Error(t, ValidatePollingAccounts([]AccountSchedule{account, duplicate}))
	account.Schedule = "@hourly"
	assert.Error(t, ValidatePollingAccounts([]AccountSchedule{account}))
	account.Schedule = ", * * * *"
	assert.Error(t, ValidatePollingAccounts([]AccountSchedule{account}))
}
