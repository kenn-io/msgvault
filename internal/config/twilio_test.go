package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/attachmentpolicy"
)

// Loading a [[twilio]] entry fills defaults and resolves its media policy;
// credentials are checked only when the source is used.
func TestLoadTwilioSource(t *testing.T) {
	meeting := attachmentpolicy.Conversation{Type: "meeting"}
	for _, tc := range []struct {
		name, body, wantErr string
		check               func(*assert.Assertions, TwilioSource)
	}{
		{"defaults", `
[[twilio]]
account_email = " User@Example.COM "
account_sid = "AC00000000000000000000000000000001"
auth_token = "synthetic-token"
enabled = true
schedule = "15 */6 * * *"
`, "", func(assert *assert.Assertions, source TwilioSource) {
			assert.Equal("default", source.Identifier)
			assert.Equal("user@example.com", source.AccountEmail)
			assert.Equal("us1", source.Region)
			assert.Equal("15 */6 * * *", source.Schedule)
			assert.Equal(attachmentpolicy.SkipReason(""), source.MediaPolicy().Evaluate(meeting, 250<<20))
			assert.Equal(attachmentpolicy.SkipSizeCap, source.MediaPolicy().Evaluate(meeting, (250<<20)+1))
		}},
		{"incomplete credentials", "[[twilio]]\naccount_email = 'user@example.com'\nenabled = false\n", "", nil},
		{"regional credentials and disabled media", `
[[twilio]]
identifier = "ireland"
account_email = "user@example.com"
account_sid = "AC00000000000000000000000000000001"
api_key_sid = "SK00000000000000000000000000000001"
api_key_secret = "synthetic"
region = "ie1"
media = false
max_media_mb = 40
`, "", func(assert *assert.Assertions, source TwilioSource) {
			assert.Equal(int64(40<<20), source.MediaPolicy().MaxBytes)
			assert.Equal(attachmentpolicy.SkipPolicyScope, source.MediaPolicy().Evaluate(attachmentpolicy.Conversation{}, 1))
		}},
		{"negative media cap", "[[twilio]]\naccount_email = 'user@example.com'\nmax_media_mb = -1\n", "max_media_mb", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := Load(writeMeetingConfig(t, tc.body), "")
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			require.Len(t, cfg.Twilio, 1)
			if tc.check != nil {
				tc.check(assert.New(t), cfg.Twilio[0])
			}
		})
	}
}
