package config

import (
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestLoadOmiSources(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	path := writeMeetingConfig(t, `[[omi]]
account_email = " User@Example.COM "
api_key = "omi_dev_synthetic"
schedule = "0 */6 * * *"
enabled = true
`)
	cfg, err := Load(path, "")
	require.NoError(err)
	src := cfg.GetOmiSource("DEFAULT")
	require.NotNil(src)
	assert.Equal("user@example.com", src.AccountEmail)
	assert.Equal("https://api.omi.me", src.BaseURL)
	require.Len(cfg.ScheduledOmiSources(), 1)
	src.Enabled = false
	cfg.Omi = []OmiSource{*src}
	assert.Empty(cfg.ScheduledOmiSources())
}

func TestLoadOmiInvalidConfig(t *testing.T) {
	for _, content := range []string{
		`[[omi]]
identifier="work"`,
		`[[omi]]
account_email="user@example.com"
base_url="https://example.com/v1/dev"`,
		`[[omi]]
identifier="work"
account_email="user@example.com"
[[omi]]
identifier="WORK"
account_email="other@example.com"`,
	} {
		t.Run(content, func(t *testing.T) { _, err := Load(writeMeetingConfig(t, content), ""); require.Error(t, err) })
	}
}
