package config

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const twentyConfig = `[[twenty]]
account_email = " Recorder@Example.COM "
base_url = "https://api.twenty.com/"
api_key = "example-key"
schedule = "*/30 * * * *"
enabled = true
`

func TestTwentyConfig(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	cfg, err := Load(writeMeetingConfig(t, twentyConfig), "")
	require.NoError(err)
	require.Len(cfg.Twenty, 1)
	assert.Equal("default", cfg.Twenty[0].Identifier)
	assert.Equal("recorder@example.com", cfg.Twenty[0].AccountEmail)
	assert.Equal("https://api.twenty.com", cfg.Twenty[0].BaseURL)
	require.NotNil(cfg.GetTwentySource("DEFAULT"))
	assert.Nil(cfg.GetTwentySource("missing"))
	require.Len(cfg.ScheduledTwentySources(), 1)
	cfg.Twenty[0].Enabled = false
	assert.Empty(cfg.ScheduledTwentySources())
	cfg.Twenty[0].Enabled = true
	cfg.Twenty[0].Schedule = ""
	assert.Empty(cfg.ScheduledTwentySources())
}

func TestTwentyConfigRejectsInvalidSources(t *testing.T) {
	for _, text := range []string{
		`[[twenty]]
base_url="https://api.twenty.com"`,
		`[[twenty]]
account_email="display name"
base_url="https://api.twenty.com"`,
		`[[twenty]]
account_email="recorder@example.com"
base_url="http://daemon.example"`,
		`[[twenty]]
account_email="recorder@example.com"
base_url="https://daemon.example/prefix"`,
		twentyConfig + "\n" + twentyConfig,
		`[[twenty]]
identifier="work"
account_email="recorder@example.com"
base_url="https://api.twenty.com"
[[twenty]]
identifier="WORK"
account_email="other@example.com"
base_url="https://api.twenty.com"`,
	} {
		t.Run(text, func(t *testing.T) {
			require := require.New(t)
			_, err := Load(writeMeetingConfig(t, text), "")
			require.Error(err)
		})
	}
}

func TestTwentyConfigEditValidatesCron(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	path := writeMeetingConfig(t, twentyConfig)
	snapshot, err := ReadConfigFile(path)
	require.NoError(err)
	_, err = EditConfigFile(path, snapshot.ETag, []Edit{{Key: "twenty.schedule", Value: "not a cron"}})
	require.ErrorIs(err, ErrInvalidConfigCandidate)
	assert.Contains(err.Error(), "invalid twenty[0].schedule")
	got, err := os.ReadFile(path)
	require.NoError(err)
	assert.Equal(twentyConfig, string(got))
}
