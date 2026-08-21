package peoplesweep_test

import (
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/peoplesweep"
)

func validConfig() peoplesweep.Config {
	return peoplesweep.Config{
		Enabled: true,
		Provider: peoplesweep.ProviderConfig{
			Kind:             peoplesweep.ProviderOpenAICompatible,
			Endpoint:         "https://api.example.test/v1/",
			Model:            "gpt-test",
			APIKeyEnv:        "TEST_KEY",
			RetentionPosture: "zero_retention",
			TrainingPosture:  "no_training",
			AllowedSources: []peoplesweep.SourceClass{
				peoplesweep.SourceMeetingText,
				peoplesweep.SourceConversationText,
			},
			SourceSince:    "2025-01-01",
			SourceUntil:    "2025-12-31",
			RequestTimeout: 45 * time.Second,
		},
	}
}

func TestConfigDefaultsStayDisabled(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	var config peoplesweep.Config
	config.ApplyDefaults()

	assert.False(config.Enabled)
	assert.Equal(peoplesweep.ProviderOpenAICompatible, config.Provider.Kind)
	assert.Equal("https://api.openai.com/v1", config.Provider.Endpoint)
	assert.Equal("OPENAI_API_KEY", config.Provider.APIKeyEnv)
	assert.Equal(time.Minute, config.Provider.RequestTimeout)
	require.NoError(config.Validate())
	_, err := config.Profile()
	require.ErrorContains(err, "disabled")
}

func TestOrcaRouterProviderDefaults(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	config := peoplesweep.Config{Enabled: true, Provider: peoplesweep.ProviderConfig{
		Kind:             peoplesweep.ProviderOrcaRouter,
		RetentionPosture: "zero_retention",
		TrainingPosture:  "no_training",
		AllowedSources: []peoplesweep.SourceClass{
			peoplesweep.SourceMeetingText,
			peoplesweep.SourceConversationText,
		},
		SourceSince: "2025-01-01",
	}}
	config.ApplyDefaults()

	assert.Equal(peoplesweep.ProviderOrcaRouter, config.Provider.Kind)
	assert.Equal(peoplesweep.OrcaRouterDefaultEndpoint, config.Provider.Endpoint)
	assert.Equal(peoplesweep.OrcaRouterDefaultModel, config.Provider.Model)
	assert.Equal(peoplesweep.OrcaRouterDefaultAPIKeyEnv, config.Provider.APIKeyEnv)
	require.NoError(config.Validate())

	profile, err := config.Profile()
	require.NoError(err)
	assert.Equal(peoplesweep.OrcaRouterDefaultEndpoint, profile.Endpoint)
	assert.Equal(peoplesweep.OrcaRouterDefaultModel, profile.Model)
	assert.Equal(peoplesweep.OrcaRouterDefaultAPIKeyEnv, profile.APIKeyEnv)
}

func TestOrcaRouterProviderKeepsExplicitSettings(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	config := peoplesweep.Config{Enabled: true, Provider: peoplesweep.ProviderConfig{
		Kind:             peoplesweep.ProviderOrcaRouter,
		Endpoint:         "https://proxy.example.test/v1",
		Model:            "my-model",
		APIKeyEnv:        "MY_KEY",
		RetentionPosture: "zero_retention",
		TrainingPosture:  "no_training",
		AllowedSources: []peoplesweep.SourceClass{
			peoplesweep.SourceMeetingText,
		},
		SourceSince: "2025-01-01",
	}}
	config.ApplyDefaults()

	assert.Equal("https://proxy.example.test/v1", config.Provider.Endpoint)
	assert.Equal("my-model", config.Provider.Model)
	assert.Equal("MY_KEY", config.Provider.APIKeyEnv)
	require.NoError(config.Validate())
}

func TestProviderProfileHasStableCanonicalPolicy(t *testing.T) {
	assert := assert.New(t)
	profile, err := validConfig().Profile()
	require.NoError(t, err)

	assert.Equal("https://api.example.test/v1", profile.Endpoint)
	assert.Equal([]peoplesweep.SourceClass{
		peoplesweep.SourceConversationText,
		peoplesweep.SourceMeetingText,
	}, profile.AllowedSources)
	assert.JSONEq(`{
		"kind":"openai_compatible",
		"endpoint":"https://api.example.test/v1",
		"model":"gpt-test",
		"api_key_env":"TEST_KEY",
		"allow_anonymous":false,
		"retention_posture":"zero_retention",
		"training_posture":"no_training",
		"allowed_sources":["conversation_text","meeting_text"],
		"source_since":"2025-01-01",
		"source_until":"2025-12-31",
		"allow_sensitive":false
	}`, string(profile.PolicyJSON))
	assert.Equal(
		"6cdee4ab2dcc785c032067378de1121841725c602019c4046ec0ca3c32fdcb75",
		profile.Fingerprint)
	assert.NoError(profile.Validate())
}

func TestProviderProfileFingerprintCoversConsentPolicy(t *testing.T) {
	base := validConfig()
	want, err := base.Profile()
	require.NoError(t, err)

	mutations := []struct {
		name   string
		mutate func(*peoplesweep.Config)
	}{
		{"kind", func(c *peoplesweep.Config) { c.Provider.Kind = "other" }},
		{"endpoint", func(c *peoplesweep.Config) { c.Provider.Endpoint = "https://other.example.test/v1" }},
		{"model", func(c *peoplesweep.Config) { c.Provider.Model = "another-model" }},
		{"key env", func(c *peoplesweep.Config) { c.Provider.APIKeyEnv = "OTHER_API_KEY" }},
		{"anonymous", func(c *peoplesweep.Config) {
			c.Provider.Endpoint = "http://127.0.0.1:11434/v1"
			c.Provider.APIKeyEnv = ""
			c.Provider.AllowAnonymous = true
		}},
		{"retention", func(c *peoplesweep.Config) { c.Provider.RetentionPosture = "provider_policy" }},
		{"training", func(c *peoplesweep.Config) { c.Provider.TrainingPosture = "provider_policy" }},
		{"sources", func(c *peoplesweep.Config) {
			c.Provider.AllowedSources = append(c.Provider.AllowedSources, peoplesweep.SourceDocumentText)
		}},
		{"source since", func(c *peoplesweep.Config) { c.Provider.SourceSince = "2024-01-01" }},
		{"source until", func(c *peoplesweep.Config) { c.Provider.SourceUntil = "2026-01-01" }},
		{"sensitive", func(c *peoplesweep.Config) { c.Provider.AllowSensitive = true }},
	}
	for _, mutation := range mutations {
		t.Run(mutation.name, func(t *testing.T) {
			gotConfig := base
			gotConfig.Provider.AllowedSources = slices.Clone(base.Provider.AllowedSources)
			mutation.mutate(&gotConfig)
			got, profileErr := gotConfig.Profile()
			if mutation.name == "kind" {
				require.Error(t, profileErr)
				return
			}
			require.NoError(t, profileErr)
			assert.NotEqual(t, want.Fingerprint, got.Fingerprint)
		})
	}

	t.Run("source order is canonical", func(t *testing.T) {
		reordered := base
		reordered.Provider.AllowedSources = []peoplesweep.SourceClass{
			peoplesweep.SourceConversationText,
			peoplesweep.SourceMeetingText,
		}
		got, profileErr := reordered.Profile()
		require.NoError(t, profileErr)
		assert.Equal(t, want.Fingerprint, got.Fingerprint)
	})

	t.Run("timeout is operational", func(t *testing.T) {
		changed := base
		changed.Provider.RequestTimeout = 2 * time.Minute
		got, profileErr := changed.Profile()
		require.NoError(t, profileErr)
		assert.Equal(t, want.Fingerprint, got.Fingerprint)
	})
}

func TestProviderProfileValidateRejectsTampering(t *testing.T) {
	profile, err := validConfig().Profile()
	require.NoError(t, err)

	changedField := profile
	changedField.Model = "changed"
	require.ErrorContains(t, changedField.Validate(), "fingerprint")

	changedPolicy := profile
	changedPolicy.PolicyJSON = []byte(`{"different":true}`)
	require.ErrorContains(t, changedPolicy.Validate(), "policy")
}

func TestProviderProfileValidateRejectsNonCanonicalPublicFields(t *testing.T) {
	profile, err := validConfig().Profile()
	require.NoError(t, err)

	mutations := []struct {
		name   string
		mutate func(*peoplesweep.ProviderProfile)
	}{
		{"endpoint slash", func(p *peoplesweep.ProviderProfile) { p.Endpoint += "/" }},
		{"model whitespace", func(p *peoplesweep.ProviderProfile) { p.Model = " " + p.Model }},
		{"retention whitespace", func(p *peoplesweep.ProviderProfile) { p.RetentionPosture += " " }},
		{"training whitespace", func(p *peoplesweep.ProviderProfile) { p.TrainingPosture += " " }},
		{"source order", func(p *peoplesweep.ProviderProfile) {
			p.AllowedSources = []peoplesweep.SourceClass{
				peoplesweep.SourceMeetingText,
				peoplesweep.SourceConversationText,
			}
		}},
	}
	for _, mutation := range mutations {
		t.Run(mutation.name, func(t *testing.T) {
			changed := profile
			changed.AllowedSources = slices.Clone(profile.AllowedSources)
			mutation.mutate(&changed)
			assert.ErrorContains(t, changed.Validate(), "canonical")
		})
	}
}

func TestConfigValidationRejectsUnsafeOrAmbiguousPolicies(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*peoplesweep.Config)
		want   string
	}{
		{"missing model", func(c *peoplesweep.Config) { c.Provider.Model = "" }, "model"},
		{"unsupported kind", func(c *peoplesweep.Config) { c.Provider.Kind = "other" }, "kind"},
		{"remote http", func(c *peoplesweep.Config) { c.Provider.Endpoint = "http://api.example.test/v1" }, "HTTPS"},
		{"URL credentials", func(c *peoplesweep.Config) { c.Provider.Endpoint = "https://user:pass@api.example.test/v1" }, "credentials"},
		{"URL query", func(c *peoplesweep.Config) { c.Provider.Endpoint = "https://api.example.test/v1?x=1" }, "query"},
		{"URL fragment", func(c *peoplesweep.Config) { c.Provider.Endpoint = "https://api.example.test/v1#x" }, "fragment"},
		{"invalid key env", func(c *peoplesweep.Config) { c.Provider.APIKeyEnv = "bad-name" }, "api_key_env"},
		{"missing auth", func(c *peoplesweep.Config) { c.Provider.APIKeyEnv = "" }, "anonymous"},
		{"anonymous remote", func(c *peoplesweep.Config) {
			c.Provider.APIKeyEnv = ""
			c.Provider.AllowAnonymous = true
		}, "loopback"},
		{"missing retention", func(c *peoplesweep.Config) { c.Provider.RetentionPosture = "" }, "retention"},
		{"unknown retention", func(c *peoplesweep.Config) { c.Provider.RetentionPosture = "unknown" }, "retention"},
		{"missing training", func(c *peoplesweep.Config) { c.Provider.TrainingPosture = "" }, "training"},
		{"unknown training", func(c *peoplesweep.Config) { c.Provider.TrainingPosture = "unknown" }, "training"},
		{"missing sources", func(c *peoplesweep.Config) { c.Provider.AllowedSources = nil }, "allowed_sources"},
		{"unknown source", func(c *peoplesweep.Config) {
			c.Provider.AllowedSources = []peoplesweep.SourceClass{"raw_image"}
		}, "allowed_sources"},
		{"duplicate source", func(c *peoplesweep.Config) {
			c.Provider.AllowedSources = []peoplesweep.SourceClass{
				peoplesweep.SourceConversationText,
				peoplesweep.SourceConversationText,
			}
		}, "duplicate"},
		{"missing start", func(c *peoplesweep.Config) { c.Provider.SourceSince = "" }, "source_since"},
		{"invalid start", func(c *peoplesweep.Config) { c.Provider.SourceSince = "2025-02-30" }, "source_since"},
		{"invalid end", func(c *peoplesweep.Config) { c.Provider.SourceUntil = "tomorrow" }, "source_until"},
		{"reversed dates", func(c *peoplesweep.Config) { c.Provider.SourceUntil = "2024-12-31" }, "before"},
		{"zero timeout", func(c *peoplesweep.Config) { c.Provider.RequestTimeout = 0 }, "request_timeout"},
		{"negative timeout", func(c *peoplesweep.Config) { c.Provider.RequestTimeout = -time.Second }, "request_timeout"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := validConfig()
			test.mutate(&config)
			_, err := config.Profile()
			assert.ErrorContains(t, err, test.want)
		})
	}
}

func TestConfigAllowsExplicitAnonymousLoopback(t *testing.T) {
	for _, endpoint := range []string{
		"http://127.0.0.1:11434/v1",
		"http://[::1]:11434/v1",
		"http://localhost:11434/v1",
	} {
		t.Run(endpoint, func(t *testing.T) {
			config := validConfig()
			config.Provider.Endpoint = endpoint
			config.Provider.APIKeyEnv = ""
			config.Provider.AllowAnonymous = true

			profile, err := config.Profile()
			require.NoError(t, err)
			assert.True(t, profile.AllowAnonymous)
			assert.Empty(t, profile.APIKeyEnv)
		})
	}
}
