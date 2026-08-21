package peoplesweep_test

import (
	"bytes"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/peoplesweep"
)

func validConfig() peoplesweep.Config {
	config := peoplesweep.Config{
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
	config.ApplyDefaults()
	return config
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

func TestCodexProviderFingerprintIncludesExecutionBoundaryAndEffort(t *testing.T) {
	base := validConfig()
	base.Provider = peoplesweep.ProviderConfig{
		Kind:              peoplesweep.ProviderCodexAppServer,
		Model:             "gpt-test",
		RetentionPosture:  "zero_retention",
		TrainingPosture:   "no_training",
		AllowedSources:    []peoplesweep.SourceClass{peoplesweep.SourceConversationText},
		SourceSince:       "2025-01-01",
		ReasoningEffort:   "high",
		ExecutionBoundary: peoplesweep.CodexExecutionBoundaryV1,
	}
	base.ApplyDefaults()
	want, err := base.Profile()
	require.NoError(t, err)

	for _, mutation := range []struct {
		name   string
		mutate func(*peoplesweep.Config)
	}{
		{"effort", func(c *peoplesweep.Config) { c.Provider.ReasoningEffort = "medium" }},
		{"boundary", func(c *peoplesweep.Config) { c.Provider.ExecutionBoundary = "different-boundary" }},
	} {
		t.Run(mutation.name, func(t *testing.T) {
			checks := assert.New(t)
			requirements := require.New(t)
			gotConfig := base
			gotConfig.Provider.AllowedSources = slices.Clone(base.Provider.AllowedSources)
			mutation.mutate(&gotConfig)
			got, profileErr := gotConfig.Profile()
			if mutation.name == "boundary" {
				requirements.ErrorContains(profileErr, "execution_boundary")
				return
			}
			requirements.NoError(profileErr)
			checks.NotEqual(want.Fingerprint, got.Fingerprint)
			checks.False(bytes.Equal(want.PolicyJSON, got.PolicyJSON))
		})
	}
}

// TestCodexProviderExecutableCannotSelfAttest catches a configured executable
// path being incorporated as if it were a released binary identity.
func TestCodexProviderExecutableCannotSelfAttest(t *testing.T) {
	base := validConfig()
	base.Provider = peoplesweep.ProviderConfig{
		Kind: peoplesweep.ProviderCodexAppServer, Model: "gpt-test", ReasoningEffort: "high",
		RetentionPosture: "zero_retention", TrainingPosture: "no_training",
		AllowedSources: []peoplesweep.SourceClass{peoplesweep.SourceConversationText},
		SourceSince:    "2025-01-01", Executable: "/synthetic/path/codex-one",
		ExecutionBoundary: peoplesweep.CodexExecutionBoundaryV1,
	}
	base.ApplyDefaults()
	want, err := base.Profile()
	require.NoError(t, err)

	base.Provider.Executable = "/synthetic/path/codex-two"
	got, err := base.Profile()
	require.NoError(t, err)
	assert.Equal(t, want, got)
}

func TestPositiveCostBudgetRequiresPrices(t *testing.T) {
	config := validConfig()
	config.Budgets.MaxEstimatedCostMicroUSDPerDay = 1
	config.Budgets.InputCostMicroUSDPerMillionTokens = 0
	config.Budgets.OutputCostMicroUSDPerMillionTokens = 0

	assert.ErrorContains(t, config.Validate(), "cost prices are required")
}

func TestOpenAIProviderProfileOperationalFieldsExcluded(t *testing.T) {
	base := validConfig()
	want, err := base.Profile()
	require.NoError(t, err)

	for _, mutation := range []struct {
		name   string
		mutate func(*peoplesweep.Config)
	}{
		{"timeout", func(c *peoplesweep.Config) { c.Provider.RequestTimeout = 2 * time.Minute }},
		{"executable", func(c *peoplesweep.Config) { c.Provider.Executable = "other-codex" }},
		{"schedule", func(c *peoplesweep.Config) { c.Schedule = "0 3 * * *" }},
		{"lease", func(c *peoplesweep.Config) { c.LeaseDuration = 30 * time.Minute }},
		{"retry", func(c *peoplesweep.Config) { c.RetryBase = 2 * time.Minute }},
	} {
		t.Run(mutation.name, func(t *testing.T) {
			gotConfig := base
			gotConfig.Provider.AllowedSources = slices.Clone(base.Provider.AllowedSources)
			mutation.mutate(&gotConfig)
			got, profileErr := gotConfig.Profile()
			require.NoError(t, profileErr)
			assert.Equal(t, want, got)
		})
	}
}

func TestProviderFingerprintIncludesEgressPolicy(t *testing.T) {
	base := validConfig()
	want, err := base.Profile()
	require.NoError(t, err)

	for _, mutation := range []struct {
		name   string
		mutate func(*peoplesweep.Config)
	}{
		{"endpoint", func(c *peoplesweep.Config) { c.Provider.Endpoint = "https://other.example.test/v1" }},
		{"model", func(c *peoplesweep.Config) { c.Provider.Model = "other-model" }},
		{"key environment", func(c *peoplesweep.Config) { c.Provider.APIKeyEnv = "OTHER_KEY" }},
		{"retention", func(c *peoplesweep.Config) { c.Provider.RetentionPosture = "provider-policy" }},
		{"training", func(c *peoplesweep.Config) { c.Provider.TrainingPosture = "provider-policy" }},
		{"sources", func(c *peoplesweep.Config) {
			c.Provider.AllowedSources = []peoplesweep.SourceClass{peoplesweep.SourceDocumentText}
		}},
		{"source since", func(c *peoplesweep.Config) { c.Provider.SourceSince = "2024-01-01" }},
		{"source until", func(c *peoplesweep.Config) { c.Provider.SourceUntil = "2026-01-01" }},
		{"sensitive", func(c *peoplesweep.Config) { c.Provider.AllowSensitive = true }},
	} {
		t.Run(mutation.name, func(t *testing.T) {
			gotConfig := base
			gotConfig.Provider.AllowedSources = slices.Clone(base.Provider.AllowedSources)
			mutation.mutate(&gotConfig)
			got, profileErr := gotConfig.Profile()
			require.NoError(t, profileErr)
			assert.NotEqual(t, want.Fingerprint, got.Fingerprint)
		})
	}
}

func TestPeopleSweepDefaults(t *testing.T) {
	checks := assert.New(t)
	var config peoplesweep.Config
	config.ApplyDefaults()

	checks.Equal("15 2 * * *", config.Schedule)
	checks.Equal(25, config.WorkBatchSize)
	checks.Equal(256, config.ChangeBatchSize)
	checks.Equal(2_000, config.HistoricalMessageCap)
	checks.Equal(8, config.ContextPerTarget)
	checks.Equal(131_072, config.EvidenceMaxBytes)
	checks.Equal(200, config.EvidenceMaxItems)
	checks.Equal(15*time.Minute, config.LeaseDuration)
	checks.Equal(24*time.Hour, config.BackstopInterval)
	checks.Equal(time.Minute, config.RetryBase)
	checks.Equal(6*time.Hour, config.RetryMax)
	checks.Equal(peoplesweep.BudgetConfig{
		MaxRequestsPerPerson: 4, MaxInputTokensPerPerson: 200_000, MaxOutputTokensPerPerson: 16_000,
		MaxRequestsPerRun: 100, MaxInputTokensPerRun: 1_000_000, MaxOutputTokensPerRun: 160_000,
		MaxRequestsPerDay: 500, MaxInputTokensPerDay: 5_000_000, MaxOutputTokensPerDay: 800_000,
	}, config.Budgets)
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
	assert.JSONEq(strings.ReplaceAll(`{
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
		"allow_sensitive":false,
		"reasoning_effort":"",
		"execution_boundary":"",
		"packet_renderer_policy":"person-sweep-packet-v1",
		"program_fingerprint":"PROGRAM_FINGERPRINT",
		"disclosed_packet_fields":["person_id","program_identity","catalog","current_projection","unresolved_claims","seed_evidence","retrieved_context"]
	}`, "PROGRAM_FINGERPRINT", peoplesweep.ProgramFingerprint()), string(profile.PolicyJSON))
	assert.Contains(string(profile.PolicyJSON), peoplesweep.ProgramFingerprint())
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
		{"attachment caption without hydration", func(c *peoplesweep.Config) {
			c.Provider.AllowedSources = []peoplesweep.SourceClass{peoplesweep.SourceAttachmentCaption}
		}, "not yet supported"},
		{"attachment OCR without hydration", func(c *peoplesweep.Config) {
			c.Provider.AllowedSources = []peoplesweep.SourceClass{peoplesweep.SourceAttachmentOCR}
		}, "not yet supported"},
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
		{"output budget below one request", func(c *peoplesweep.Config) {
			c.Budgets.MaxOutputTokensPerPerson = 4095
		}, "at least 4096"},
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

func TestConfigRejectsAuthenticatedLoopbackHTTP(t *testing.T) {
	config := validConfig()
	config.Provider.Endpoint = "http://127.0.0.1:11434/v1"

	assert.ErrorContains(t, config.Validate(), "anonymous loopback")
}
