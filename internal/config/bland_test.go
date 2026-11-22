package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/attachmentpolicy"
)

// Loading a [[bland]] entry fills defaults and resolves its media policy,
// which shares the call-recording cap; the API key is checked only when the
// source is used.
func TestLoadBlandSource(t *testing.T) {
	meeting := attachmentpolicy.Conversation{Type: "meeting"}
	for _, tc := range []struct {
		name, body, wantErr string
		check               func(*assert.Assertions, *Config, BlandSource)
	}{
		{"defaults", `
[[bland]]
account_email = " Owner@Example.com "
api_key = "synthetic-key"
enabled = true
schedule = "0 */6 * * *"
`, "", func(assert *assert.Assertions, cfg *Config, source BlandSource) {
			assert.Equal("default", source.Identifier)
			assert.Equal("owner@example.com", source.AccountEmail)
			assert.Equal(attachmentpolicy.SkipReason(""), source.MediaPolicy().Evaluate(meeting, 1<<30))
			assert.Equal(attachmentpolicy.SkipSizeCap, source.MediaPolicy().Evaluate(meeting, (1<<30)+1))
			assert.Len(cfg.ScheduledBlandSources(), 1)
		}},
		{"missing API key", "[[bland]]\naccount_email = 'owner@example.com'\n", "", nil},
		{"encrypted key and disabled media", `
[[bland]]
identifier = "work"
account_email = "owner@example.com"
api_key = "synthetic-key"
encrypted_key = "synthetic-byot"
media = false
max_media_mb = 12
`, "", func(assert *assert.Assertions, cfg *Config, source BlandSource) {
			assert.Equal("synthetic-byot", source.EncryptedKey)
			assert.Equal(int64(12<<20), source.MediaPolicy().MaxBytes)
			assert.Equal(attachmentpolicy.SkipPolicyScope, source.MediaPolicy().Evaluate(attachmentpolicy.Conversation{}, 1))
			assert.Empty(cfg.ScheduledBlandSources())
			assert.Equal("work", cfg.GetBlandSource("WORK").Identifier)
		}},
		{"negative media cap", "[[bland]]\naccount_email = 'owner@example.com'\nmax_media_mb = -1\n", "max_media_mb", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := Load(writeMeetingConfig(t, tc.body), "")
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			require.Len(t, cfg.Bland, 1)
			if tc.check != nil {
				tc.check(assert.New(t), cfg, cfg.Bland[0])
			}
		})
	}
}
