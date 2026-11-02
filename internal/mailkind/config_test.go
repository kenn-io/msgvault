package mailkind

import (
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestConfigAndDisclosure(t *testing.T) {
	cfg := Config{}
	cfg.ApplyDefaults()
	require.NoError(t, cfg.Validate())
	_, err := cfg.Disclosure()
	require.Error(t, err)
	cfg.Enabled = true
	require.Error(t, cfg.Validate())
	cfg.RetentionDeclaration = "operator-reviewed provider retention"
	disclosure, err := cfg.Disclosure()
	require.NoError(t, err)
	fingerprint, err := disclosure.Fingerprint()
	require.NoError(t, err)
	assert.Len(t, fingerprint, 64)
	for _, mutate := range []func(*Disclosure){
		func(d *Disclosure) { d.RetentionDeclaration += " changed" },
		func(d *Disclosure) { d.CredentialEnv = "ANOTHER_TEST_KEY" },
		func(d *Disclosure) { d.MinimumTransactional = 0.95 },
		func(d *Disclosure) { d.MinimumChoiceProbability = 0.85 },
	} {
		changed := disclosure
		mutate(&changed)
		actual, err := changed.Fingerprint()
		require.NoError(t, err)
		assert.NotEqual(t, fingerprint, actual)
	}
	for _, mutate := range []func(*Disclosure){
		func(d *Disclosure) { d.Endpoint = "https://other.example/v1/systemone" },
		func(d *Disclosure) { d.ModelID = "unreviewed-model" },
		func(d *Disclosure) { d.PacketSchema += "-changed" },
		func(d *Disclosure) { d.DataFields += " undeclared fields" },
		func(d *Disclosure) { d.PolicyVersion += "-changed" },
		func(d *Disclosure) { d.QuestionVersion += "-changed" },
	} {
		changed := disclosure
		mutate(&changed)
		_, err := changed.Fingerprint()
		require.Error(t, err)
	}
	for _, value := range []float64{0, 0.79, 1.01, math.NaN(), math.Inf(1)} {
		bad := cfg
		bad.MinimumChoiceProbability = value
		require.Error(t, bad.Validate())
	}
	cfg.CredentialEnv = "MSGVAULT_TEST_MAIL_KIND_KEY"
	t.Setenv(cfg.CredentialEnv, "")
	_, err = cfg.CredentialFromEnvironment()
	require.Error(t, err)
	t.Setenv(cfg.CredentialEnv, "synthetic-key")
	credential, err := cfg.CredentialFromEnvironment()
	require.NoError(t, err)
	assert.Equal(t, "synthetic-key", credential)
}
