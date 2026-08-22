package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/attachmentpolicy"
)

func TestMediaPolicyDefaultsRemainAllowAll(t *testing.T) {
	assert := assert.New(t)
	cfg := NewDefaultConfig()
	assert.Equal(attachmentpolicy.Policy{Scope: attachmentpolicy.ScopeAll, MaxBytes: 100 << 20}, cfg.Beeper.MediaPolicy("signal"))
	assert.Equal(attachmentpolicy.Policy{Scope: attachmentpolicy.ScopeAll, MaxBytes: 100 << 20}, cfg.Slack.MediaPolicy("T01"))
	assert.Equal(attachmentpolicy.Policy{Scope: attachmentpolicy.ScopeAll, MaxBytes: 50 << 20}, cfg.Discord.MediaPolicy("G01"))
	assert.Equal(attachmentpolicy.Policy{Scope: attachmentpolicy.ScopeAll, MaxBytes: 100 << 20}, cfg.Teams.MediaPolicy("user@example.com"))
}

func TestLoadMediaPolicyAccountOverrides(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	configPath := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(os.WriteFile(configPath, []byte(`
[beeper]
media = false
media_scope = "direct"
media_max_participants = 5
max_media_mb = 80
[beeper.accounts_config.signal]
media = true
max_media_mb = 20

[slack]
media = true
media_scope = "none"
max_media_mb = 90
[slack.accounts_config.T01]
media = false

[discord]
media_scope = "all"
media_max_participants = 10
max_media_mb = 70
[discord.guilds.G01]
media = false
max_media_mb = 30

[teams]
media = false
media_scope = "direct"
max_media_mb = 60
[teams.accounts_config."user@example.com"]
media = true
max_media_mb = 40
`), 0o644))

	cfg, err := Load(configPath, "")
	require.NoError(err)

	assert.Equal(attachmentpolicy.Policy{
		Scope: attachmentpolicy.ScopeDirect, MaxParticipants: 5, MaxBytes: 20 << 20,
	}, cfg.Beeper.MediaPolicy("signal"))
	assert.Equal(attachmentpolicy.Policy{
		Scope: attachmentpolicy.ScopeDirect, MaxParticipants: 5, MaxBytes: 80 << 20,
		DisabledReason: attachmentpolicy.SkipPolicyScope,
	}, cfg.Beeper.MediaPolicy("telegram"))
	assert.Equal(attachmentpolicy.Policy{
		Scope: attachmentpolicy.ScopeNone, MaxBytes: 90 << 20,
		DisabledReason: attachmentpolicy.SkipAccountPolicy,
	}, cfg.Slack.MediaPolicy("T01"))
	assert.Equal(attachmentpolicy.Policy{
		Scope: attachmentpolicy.ScopeAll, MaxParticipants: 10, MaxBytes: 30 << 20,
		DisabledReason: attachmentpolicy.SkipAccountPolicy,
	}, cfg.Discord.MediaPolicy("G01"))
	assert.Equal(attachmentpolicy.Policy{
		Scope: attachmentpolicy.ScopeDirect, MaxBytes: 40 << 20,
	}, cfg.Teams.MediaPolicy("user@example.com"))
}

func TestLoadRejectsInvalidMediaPolicies(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    string
	}{
		{name: "scope", content: "[slack]\nmedia_scope = \"rooms\"\n", want: "media_scope"},
		{name: "participants", content: "[beeper]\nmedia_max_participants = -1\n", want: "media_max_participants"},
		{name: "provider size", content: "[teams]\nmax_media_mb = -1\n", want: "max_media_mb"},
		{name: "account size", content: "[discord.guilds.G01]\nmax_media_mb = -1\n", want: "max_media_mb"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			configPath := filepath.Join(t.TempDir(), "config.toml")
			require.NoError(t, os.WriteFile(configPath, []byte(tt.content), 0o644))
			_, err := Load(configPath, "")
			assert.ErrorContains(t, err, tt.want)
		})
	}
}
