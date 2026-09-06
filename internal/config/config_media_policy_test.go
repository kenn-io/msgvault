package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/attachmentpolicy"
)

func TestMediaPolicyDefaultsCapGroupRoomMedia(t *testing.T) {
	assert := assert.New(t)
	cfg := NewDefaultConfig()
	assert.Equal(attachmentpolicy.Policy{
		Scope: attachmentpolicy.ScopeAll, MaxParticipants: DefaultMediaMaxParticipants, MaxBytes: DefaultChatMaxMediaBytes,
	}, cfg.Beeper.MediaPolicy("signal"))
	assert.Equal(attachmentpolicy.Policy{
		Scope: attachmentpolicy.ScopeAll, MaxParticipants: DefaultMediaMaxParticipants, MaxBytes: DefaultChatMaxMediaBytes,
	}, cfg.Slack.MediaPolicy("T01"))
	assert.Equal(attachmentpolicy.Policy{
		Scope: attachmentpolicy.ScopeAll, MaxParticipants: DefaultMediaMaxParticipants, MaxBytes: DefaultDiscordMaxMediaBytes,
	}, cfg.Discord.MediaPolicy("G01"))
	assert.Equal(attachmentpolicy.Policy{
		Scope: attachmentpolicy.ScopeAll, MaxParticipants: DefaultMediaMaxParticipants, MaxBytes: DefaultChatMaxMediaBytes,
	}, cfg.Teams.MediaPolicy("user@example.com"))
}

func TestLoadMediaPolicyOmittedParticipantCapUsesDefault(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	configPath := filepath.Join(t.TempDir(), "config.toml")
	// [beeper] and [discord] set other media keys but omit the participant
	// cap; [slack] and [teams] have no table at all.
	require.NoError(os.WriteFile(configPath, []byte(`
[beeper]
media_scope = "direct"
max_media_mb = 80

[discord]
edit_rescan_window = "24h"
[discord.guilds.G01]
include = ["C01"]
`), 0o644))

	cfg, err := Load(configPath, "")
	require.NoError(err)

	assert.Equal(attachmentpolicy.Policy{
		Scope: attachmentpolicy.ScopeDirect, MaxParticipants: DefaultMediaMaxParticipants, MaxBytes: 80 << 20,
	}, cfg.Beeper.MediaPolicy("signal"))
	assert.Equal(attachmentpolicy.Policy{
		Scope: attachmentpolicy.ScopeAll, MaxParticipants: DefaultMediaMaxParticipants, MaxBytes: DefaultChatMaxMediaBytes,
	}, cfg.Slack.MediaPolicy("T01"))
	assert.Equal(attachmentpolicy.Policy{
		Scope: attachmentpolicy.ScopeAll, MaxParticipants: DefaultMediaMaxParticipants, MaxBytes: DefaultDiscordMaxMediaBytes,
	}, cfg.Discord.MediaPolicy("G01"))
	assert.Equal(attachmentpolicy.Policy{
		Scope: attachmentpolicy.ScopeAll, MaxParticipants: DefaultMediaMaxParticipants, MaxBytes: DefaultChatMaxMediaBytes,
	}, cfg.Teams.MediaPolicy("user@example.com"))
	assert.Equal([]string{"C01"}, cfg.Discord.Guilds["G01"].Include, "guild filters survive the default")
}

func TestLoadMediaPolicyExplicitZeroDisablesParticipantCap(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	configPath := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(os.WriteFile(configPath, []byte(`
[beeper]
media_max_participants = 0

[slack]
media_max_participants = 0

[discord]
media_max_participants = 0

[teams]
media_max_participants = 0
`), 0o644))

	cfg, err := Load(configPath, "")
	require.NoError(err)

	for name, policy := range map[string]attachmentpolicy.Policy{
		"beeper":  cfg.Beeper.MediaPolicy("signal"),
		"slack":   cfg.Slack.MediaPolicy("T01"),
		"discord": cfg.Discord.MediaPolicy("G01"),
		"teams":   cfg.Teams.MediaPolicy("user@example.com"),
	} {
		assert.Zero(policy.MaxParticipants, "%s: explicit zero means no cap", name)
		assert.Equal(attachmentpolicy.ScopeAll, policy.Scope, name)
		assert.Empty(policy.DisabledReason, name)
	}
	// A 300-person room is admitted once the cap is disabled.
	room := attachmentpolicy.Conversation{Type: "group_chat", ParticipantCount: 300}
	assert.True(cfg.Beeper.MediaPolicy("signal").Allows(room, 1<<20))
	assert.Equal(attachmentpolicy.SkipParticipantThreshold,
		NewDefaultConfig().Beeper.MediaPolicy("signal").Evaluate(room, 1<<20),
		"the default policy skips the same room")
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
	// [slack] and [teams] omit media_max_participants, so the account
	// overrides layer on top of the default cap.
	assert.Equal(attachmentpolicy.Policy{
		Scope: attachmentpolicy.ScopeNone, MaxParticipants: DefaultMediaMaxParticipants, MaxBytes: 90 << 20,
		DisabledReason: attachmentpolicy.SkipAccountPolicy,
	}, cfg.Slack.MediaPolicy("T01"))
	assert.Equal(attachmentpolicy.Policy{
		Scope: attachmentpolicy.ScopeAll, MaxParticipants: 10, MaxBytes: 30 << 20,
		DisabledReason: attachmentpolicy.SkipAccountPolicy,
	}, cfg.Discord.MediaPolicy("G01"))
	assert.Equal(attachmentpolicy.Policy{
		Scope: attachmentpolicy.ScopeDirect, MaxParticipants: DefaultMediaMaxParticipants, MaxBytes: 40 << 20,
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
