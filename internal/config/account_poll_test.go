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
	path := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(t, os.WriteFile(path, []byte("[[accounts]]\nemail = 'user@example.com'\nschedule = '0 * * * *'\nenabled = true\npoll_interval = '15s'\n"), 0600))
	cfg, err := Load(path, "")
	require.NoError(t, err)
	assert.Equal(t, 15*time.Second, cfg.Accounts[0].PollInterval)
	require.NoError(t, cfg.Save())
	reloaded, err := Load(path, "")
	require.NoError(t, err)
	assert.Equal(t, 15*time.Second, reloaded.Accounts[0].PollInterval)
	require.NoError(t, os.WriteFile(path, []byte("[[accounts]]\nemail = 'user@example.com'\nschedule = '0 * * * *'\nenabled = true\npoll_interval = '14s'\n"), 0600))
	_, err = Load(path, "")
	require.ErrorContains(t, err, "poll_interval")
}

func TestPollingAccountValidation(t *testing.T) {
	for _, tc := range []struct {
		name     string
		interval time.Duration
		want     string
	}{
		{"off", 0, ""}, {"minimum", 15 * time.Second, ""}, {"maximum", 120 * time.Second, ""},
		{"too short", 14 * time.Second, "poll_interval"}, {"too long", 121 * time.Second, "poll_interval"}, {"fractional", 15500 * time.Millisecond, "poll_interval"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			account := AccountSchedule{Email: "user@example.com", Enabled: true, Schedule: "0 * * * *", PollInterval: tc.interval}
			err := ValidatePollingAccounts([]AccountSchedule{account})
			if tc.want == "" {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, tc.want)
			}
		})
	}
	account := AccountSchedule{Email: "user@example.com", Enabled: true, Schedule: "0 * * * *", PollInterval: 15 * time.Second}
	assert.NoError(t, ValidatePollingAccounts([]AccountSchedule{account, {Email: account.Email, Enabled: true, Schedule: account.Schedule}}))
	duplicate := account
	duplicate.Email = "USER@example.com"
	assert.ErrorContains(t, ValidatePollingAccounts([]AccountSchedule{account, duplicate}), "duplicate polling account")
	account.Schedule = ", * * * *"
	assert.ErrorContains(t, ValidatePollingAccounts([]AccountSchedule{account}), "invalid cron expression")
}
