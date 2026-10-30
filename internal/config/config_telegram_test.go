package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/attachmentpolicy"
)

func TestTelegramMediaPolicyDefaultsAndOverrides(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	assert.Equal(attachmentpolicy.Policy{Scope: attachmentpolicy.ScopeAll, MaxParticipants: DefaultMediaMaxParticipants, MaxBytes: DefaultChatMaxMediaBytes}, NewDefaultConfig().Telegram.MediaPolicy("user123"))
	path := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(os.WriteFile(path, []byte(`[telegram]
media = false
media_scope = "direct"
media_max_participants = 0
max_media_mb = 80
[telegram.accounts_config.user123]
media = true
max_media_mb = 40
`), 0600))
	cfg, err := Load(path, "")
	require.NoError(err)
	assert.Equal(attachmentpolicy.Policy{Scope: attachmentpolicy.ScopeDirect, MaxParticipants: 0, MaxBytes: 40 << 20}, cfg.Telegram.MediaPolicy("user123"))
	assert.Equal(attachmentpolicy.SkipPolicyScope, cfg.Telegram.MediaPolicy("user456").DisabledReason)
}

func TestTelegramMediaPolicyRejectsInvalidSettings(t *testing.T) {
	for _, value := range []string{"media_scope = \"invalid\"", "max_media_mb = -1", "media_max_participants = -1"} {
		t.Run(value, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			require.NoError(t, os.WriteFile(path, []byte("[telegram]\n"+value+"\n"), 0600))
			_, err := Load(path, "")
			require.ErrorContains(t, err, "telegram")
		})
	}
}

func TestTelegramRequestLimits(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	defaults := NewDefaultConfig().Telegram
	assert.Equal(2.0, defaults.RequestsPerSecond)
	assert.Equal(5*time.Minute, defaults.MaxFloodWait)
	path := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(os.WriteFile(path, []byte("[telegram]\nrequests_per_second = 0.5\nmax_flood_wait = \"12s\"\n"), 0600))
	cfg, err := Load(path, "")
	require.NoError(err)
	assert.Equal(0.5, cfg.Telegram.RequestsPerSecond)
	assert.Equal(12*time.Second, cfg.Telegram.MaxFloodWait)
	for _, setting := range []string{"requests_per_second = -1.0", "requests_per_second = 0.0", "max_flood_wait = \"-1s\""} {
		require.NoError(os.WriteFile(path, []byte("[telegram]\n"+setting+"\n"), 0600))
		_, err := Load(path, "")
		require.ErrorContains(err, "telegram")
	}
}

func TestTelegramPerAccountChannelSelection(t *testing.T) {
	require := require.New(t)

	path := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(os.WriteFile(path, []byte("[telegram.accounts_config.user123]\nchannels = [-1000000000123]\n"), 0600))
	cfg, err := Load(path, "")
	require.NoError(err)
	assert.Equal(t, []int64{-1000000000123}, cfg.Telegram.AccountsConfig["user123"].Channels)
	require.NoError(os.WriteFile(path, []byte("[telegram.accounts_config.user123]\nchannels = [123]\n"), 0600))
	_, err = Load(path, "")
	require.ErrorContains(err, "channel")
}
