package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/attachmentpolicy"
)

const twilioAccountFixture = "AC00000000000000000000000000000001"

func TestLoadTwilioSource(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	path := writeMeetingConfig(t, `
[[twilio]]
account_email = " User@Example.COM "
account_sid = "AC00000000000000000000000000000001"
auth_token = "synthetic-token"
enabled = true
schedule = "15 */6 * * *"
recording_keys = { CR00000000000000000000000000000001 = "keys/recording.pem" }
external_media_hosts = ["media.example.com"]
external_media = { RE00000000000000000000000000000001 = "https://media.example.com/audio.wav?signature=synthetic" }
`)
	cfg, err := Load(path, "")
	require.NoError(err)
	source := cfg.GetTwilioSource("DEFAULT")
	require.NotNil(source)
	assert.Equal("user@example.com", source.AccountEmail)
	assert.Equal("us1", source.Region)
	assert.Len(cfg.ScheduledTwilioSources(), 1)
	assert.Equal(filepath.Join(filepath.Dir(path), "keys/recording.pem"), source.RecordingKeys["CR00000000000000000000000000000001"])
	assert.Equal(attachmentpolicy.SkipReason(""), source.MediaPolicy().Evaluate(attachmentpolicy.Conversation{Type: "meeting"}, 250<<20))
	assert.Equal(attachmentpolicy.SkipSizeCap, source.MediaPolicy().Evaluate(attachmentpolicy.Conversation{Type: "meeting"}, (250<<20)+1))
}

func TestLoadTwilioRejectsInvalidConfiguration(t *testing.T) {
	base := "[[twilio]]\naccount_email = 'user@example.com'\naccount_sid = '" + twilioAccountFixture + "'\n"
	for _, test := range []struct{ name, content, want string }{
		{"missing credential", "", "api_key_sid"},
		{"key without secret", "api_key_sid = 'SK00000000000000000000000000000001'\n", "api_key_secret"},
		{"mixed credentials", "auth_token = 'do-not-print-this'\napi_key_sid = 'SK00000000000000000000000000000001'\napi_key_secret = 'another-private-secret'\n", "mutually exclusive"},
		{"unknown region", "auth_token = 'synthetic'\nregion = 'xx1'\n", "region"},
		{"negative cap", "auth_token = 'synthetic'\nmax_media_mb = -1\n", "max_media_mb"},
		{"nonlocal key", "auth_token = 'synthetic'\nrecording_keys = { CR00000000000000000000000000000001 = 'https://keys.example.com/key.pem' }\n", "local"},
		{"unsafe external media", "auth_token = 'synthetic'\nexternal_media_hosts = ['media.example.com']\nexternal_media = { RE00000000000000000000000000000001 = 'http://media.example.com/audio' }\n", "HTTPS"},
		{"unapproved media host", "auth_token = 'synthetic'\nexternal_media = { RE00000000000000000000000000000001 = 'https://media.example.com/audio' }\n", "external_media_hosts"},
		{"unsafe host", "auth_token = 'synthetic'\nexternal_media_hosts = ['127.0.0.1']\n", "external_media_hosts"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := Load(writeMeetingConfig(t, base+test.content), "")
			require.ErrorContains(t, err, test.want)
			assert.NotContains(t, err.Error(), "do-not-print-this")
			assert.NotContains(t, err.Error(), "another-private-secret")
		})
	}
}

func TestLoadTwilioRegionalCredentialsAndDisabledMedia(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	cfg, err := Load(writeMeetingConfig(t, `
[[twilio]]
identifier = "ireland"
account_email = "user@example.com"
account_sid = "AC00000000000000000000000000000001"
api_key_sid = "SK00000000000000000000000000000001"
api_key_secret = "synthetic"
region = "ie1"
media = false
max_media_mb = 40
`), "")
	require.NoError(err)
	source := cfg.GetTwilioSource("ireland")
	require.NotNil(source)
	assert.Equal(int64(40<<20), source.MediaPolicy().MaxBytes)
	assert.Equal(attachmentpolicy.SkipAccountPolicy, source.MediaPolicy().Evaluate(attachmentpolicy.Conversation{}, 1))
	assert.Empty(cfg.ScheduledTwilioSources())
}

func TestLoadTwilioDuplicateSourceLabels(t *testing.T) {
	entry := "account_email='user@example.com'\naccount_sid='" + twilioAccountFixture + "'\nauth_token='synthetic'\n"
	_, err := Load(writeMeetingConfig(t, "[[twilio]]\nidentifier='work'\n"+entry+"[[twilio]]\nidentifier='WORK'\n"+entry), "")
	require.ErrorContains(t, err, "duplicate identifier")
}

func TestEditConfigRejectsInvalidTwilioSchedule(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	path := writeMeetingConfig(t, "[[twilio]]\naccount_email='user@example.com'\naccount_sid='"+twilioAccountFixture+"'\nauth_token='synthetic'\nschedule='15 */6 * * *'\n")
	before, err := os.ReadFile(path)
	require.NoError(err)
	snapshot, err := ReadConfigFile(path)
	require.NoError(err)
	_, err = EditConfigFile(path, snapshot.ETag, []Edit{{Key: "twilio.schedule", Value: "not a cron"}})
	require.ErrorIs(err, ErrInvalidConfigCandidate)
	assert.Contains(err.Error(), "twilio[0].schedule")
	after, err := os.ReadFile(path)
	require.NoError(err)
	assert.Equal(before, after)
}
