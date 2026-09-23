package peoplesweep_test

import (
	"bytes"
	"testing"

	"github.com/BurntSushi/toml"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/peoplesweep"
)

func TestProviderPresetRoundTripAndFingerprint(t *testing.T) {
	assertChecks := assert.New(t)
	requireChecks := require.New(t)
	base := validConfig()
	legacy, err := base.Profile()
	requireChecks.NoError(err)
	assertChecks.NotContains(string(legacy.PolicyJSON), "preset_id")

	provider := activeProvider(base)
	provider.PresetID = "openrouter"
	provider.Endpoint = "https://openrouter.ai/api/v1"
	setActiveProvider(&base, provider)
	profile, err := base.Profile()
	requireChecks.NoError(err)
	assertChecks.Equal("openrouter", profile.PresetID)
	assertChecks.NotEqual(legacy.Fingerprint, profile.Fingerprint)
	assertChecks.Contains(string(profile.PolicyJSON), `"preset_id":"openrouter"`)
	requireChecks.NoError(profile.Validate())
	stored, err := peoplesweep.CanonicalStoredProviderProfile(profile)
	requireChecks.NoError(err)
	assertChecks.Equal(profile.Fingerprint, stored.Fingerprint)

	var encoded bytes.Buffer
	requireChecks.NoError(toml.NewEncoder(&encoded).Encode(provider))
	var decoded peoplesweep.ProviderConfig
	_, err = toml.Decode(encoded.String(), &decoded)
	requireChecks.NoError(err)
	assertChecks.Equal("openrouter", decoded.PresetID)
	assertChecks.Equal("openrouter", peoplesweep.ProviderTOMLValues(decoded)["preset_id"])
}

func TestProviderPresetRejectsEndpointProtocolAndAuthSwap(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*peoplesweep.ProviderConfig)
	}{
		{"endpoint", func(p *peoplesweep.ProviderConfig) { p.Endpoint = "https://elsewhere.example.test/v1" }},
		{"protocol", func(p *peoplesweep.ProviderConfig) { p.Protocol = peoplesweep.ProtocolOpenAIResponses }},
		{"auth", func(p *peoplesweep.ProviderConfig) { p.Auth = peoplesweep.AuthXAPIKey }},
	} {
		t.Run(test.name, func(t *testing.T) {
			config := validConfig()
			provider, err := peoplesweep.PresetProviderConfig("venice", "venice/model")
			require.NoError(t, err)
			provider.Credential = peoplesweep.CredentialEnv
			provider.CredentialEnv = "TEST_KEY"
			provider.RetentionPosture = "operator_asserted"
			provider.TrainingPosture = "operator_asserted"
			provider.AllowedSources = []peoplesweep.SourceClass{peoplesweep.SourceConversationText}
			provider.SourceSince = "2025-01-01"
			provider.RequestTimeout = activeProvider(config).RequestTimeout
			test.change(&provider)
			setActiveProvider(&config, provider)
			require.ErrorContains(t, config.Validate(), "preset")
		})
	}
}
