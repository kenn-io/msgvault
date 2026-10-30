package config

import (
	"os"
	"path/filepath"
	"testing"

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
