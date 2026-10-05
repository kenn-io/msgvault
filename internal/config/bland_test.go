package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/attachmentpolicy"
)

func TestLoadBlandSource(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	cfg, err := Load(writeMeetingConfig(t, `
[[bland]]
account_email = "Owner@example.com"
api_key = "test-key"
encrypted_key = "test-byot"
enabled = true
schedule = "0 */6 * * *"
max_media_mb = 12
media = false
`), "")
	require.NoError(err)
	require.Len(cfg.Bland, 1)
	source := cfg.Bland[0]
	assert.Equal("owner@example.com", source.AccountEmail)
	assert.Equal("test-byot", source.EncryptedKey)
	assert.Equal(int64(12<<20), source.MediaPolicy().MaxBytes)
	assert.Equal(attachmentpolicy.SkipPolicyScope, source.MediaPolicy().DisabledReason)
	assert.Len(cfg.ScheduledBlandSources(), 1)
}
