package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPocketEditRejectsInvalidScheduleWithoutReplacingConfig(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	path := filepath.Join(t.TempDir(), "config.toml")
	before := "[[pocket]]\nidentifier = \"personal\"\naccount_email = \"owner@example.com\"\nschedule = \"15 */6 * * *\"\nenabled = true\n"
	requirements.NoError(os.WriteFile(path, []byte(before), 0o600))
	snapshot, err := ReadConfigFile(path)
	requirements.NoError(err)

	_, err = EditConfigFile(path, snapshot.ETag, []Edit{{Key: "pocket.schedule", Value: "not a cron"}})
	requirements.ErrorIs(err, ErrInvalidConfigCandidate)
	assertions.Contains(err.Error(), "invalid pocket[0].schedule")
	got, err := os.ReadFile(path)
	requirements.NoError(err)
	assertions.Equal(before, string(got))
}

func TestPocketConfig(t *testing.T) {
	for _, tt := range []struct{ name, text, problem string }{
		{"defaults", "[[pocket]]\naccount_email = ' OWNER@example.com '\nenabled = true\nschedule = '0 */6 * * *'", ""},
		{"missing email", "[[pocket]]\nidentifier = 'personal'", "account_email"},
		{"duplicate", "[[pocket]]\nidentifier='one'\naccount_email='owner@example.com'\n[[pocket]]\nidentifier='ONE'\naccount_email='owner@example.com'", "duplicate"},
		{"blank multi", "[[pocket]]\naccount_email='owner@example.com'\n[[pocket]]\nidentifier='two'\naccount_email='owner@example.com'", "identifier"},
		{"whitespace multi", "[[pocket]]\nidentifier='   '\naccount_email='owner@example.com'\n[[pocket]]\nidentifier='two'\naccount_email='owner@example.com'", "identifier"},
		{"display address", "[[pocket]]\naccount_email='Owner <owner@example.com>'", "account_email"},
		{"bad env", "[[pocket]]\naccount_email='owner@example.com'\napi_key_env='POCKET-KEY'", "api_key_env"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			home := t.TempDir()
			path := filepath.Join(home, "config.toml")
			requirements.NoError(os.WriteFile(path, []byte(tt.text), 0600))
			cfg, err := Load(path, home)
			if tt.problem != "" {
				requirements.ErrorContains(err, tt.problem)
				return
			}
			requirements.NoError(err)
			requirements.Len(cfg.Pocket, 1)
			assertions.Equal("default", cfg.Pocket[0].Identifier)
			assertions.Equal("owner@example.com", cfg.Pocket[0].AccountEmail)
			assertions.Equal("POCKET_API_KEY", cfg.Pocket[0].APIKeyEnv)
			requirements.NotNil(cfg.GetPocketSource("default"))
			requirements.NotNil(cfg.GetPocketSource("DEFAULT"))
			assertions.Nil(cfg.GetPocketSource("missing"))
			assertions.Len(cfg.ScheduledPocketSources(), 1)
			cfg.Pocket[0].Enabled = false
			assertions.Empty(cfg.ScheduledPocketSources())
		})
	}
}
