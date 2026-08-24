package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/peoplesweep"
	"go.kenn.io/msgvault/internal/personenrichment"
)

func TestPeopleSweepConfigDefaultsDisabled(t *testing.T) {
	assert := assert.New(t)
	config := NewDefaultConfig().People.Sweep

	assert.False(config.Enabled)
	assert.Equal(peoplesweep.ProviderOpenAICompatible, config.Provider.Kind)
	assert.Equal("https://api.openai.com/v1", config.Provider.Endpoint)
	assert.Equal("OPENAI_API_KEY", config.Provider.APIKeyEnv)
	assert.Equal(time.Minute, config.Provider.RequestTimeout)
}

func TestLoadPeopleSweepProviderConfig(t *testing.T) {
	assert := assert.New(t)
	path := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(t, os.WriteFile(path, []byte(`
[people.sweep]
enabled = true

[people.sweep.provider]
kind = "openai_compatible"
endpoint = "https://api.example.test/v1/"
model = "gpt-test"
api_key_env = "TEST_KEY"
allow_anonymous = false
retention_posture = "zero_retention"
training_posture = "no_training"
allowed_sources = ["meeting_text", "conversation_text"]
source_since = "2025-01-01"
source_until = "2025-12-31"
allow_sensitive = true
request_timeout = "45s"
`), 0o600))

	loaded, err := Load(path, "")
	require.NoError(t, err)
	provider := loaded.People.Sweep.Provider
	assert.True(loaded.People.Sweep.Enabled)
	assert.Equal(peoplesweep.ProviderOpenAICompatible, provider.Kind)
	assert.Equal("https://api.example.test/v1/", provider.Endpoint)
	assert.Equal("gpt-test", provider.Model)
	assert.Equal("TEST_KEY", provider.APIKeyEnv)
	assert.False(provider.AllowAnonymous)
	assert.Equal("zero_retention", provider.RetentionPosture)
	assert.Equal("no_training", provider.TrainingPosture)
	assert.Equal([]peoplesweep.SourceClass{
		peoplesweep.SourceMeetingText,
		peoplesweep.SourceConversationText,
	}, provider.AllowedSources)
	assert.Equal("2025-01-01", provider.SourceSince)
	assert.Equal("2025-12-31", provider.SourceUntil)
	assert.True(provider.AllowSensitive)
	assert.Equal(45*time.Second, provider.RequestTimeout)
}

func TestLoadRejectsInvalidEnabledPeopleSweepProvider(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(t, os.WriteFile(path, []byte(`
[people.sweep]
enabled = true

[people.sweep.provider]
model = "gpt-test"
retention_posture = "zero_retention"
training_posture = "no_training"
allowed_sources = ["raw_image"]
source_since = "2025-01-01"
`), 0o600))

	_, err := Load(path, "")
	assert.ErrorContains(t, err, "allowed_sources")
}

func TestLoadDoesNotReplaceExplicitEmptyPeopleProviderKeyEnv(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(t, os.WriteFile(path, []byte(`
[people.sweep]
enabled = true

[people.sweep.provider]
model = "gpt-test"
api_key_env = ""
retention_posture = "zero_retention"
training_posture = "no_training"
allowed_sources = ["conversation_text"]
source_since = "2025-01-01"
`), 0o600))

	_, err := Load(path, "")
	assert.ErrorContains(t, err, "api_key_env")
}

func TestLoadAllowsAnonymousLoopbackPeopleProviderWithoutKeyEnv(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	path := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(os.WriteFile(path, []byte(`
[people.sweep]
enabled = true

[people.sweep.provider]
endpoint = "http://127.0.0.1:11434/v1"
model = "local-model"
allow_anonymous = true
retention_posture = "local_only"
training_posture = "local_only"
allowed_sources = ["conversation_text"]
source_since = "2025-01-01"
`), 0o600))

	loaded, err := Load(path, "")
	require.NoError(err)
	assert.True(loaded.People.Sweep.Provider.AllowAnonymous)
	assert.Empty(loaded.People.Sweep.Provider.APIKeyEnv)
}

func TestLoadCodexPeopleProviderUsesCodexOnlyDefaults(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	path := filepath.Join(t.TempDir(), "config.toml")
	requirements.NoError(os.WriteFile(path, []byte(`
[people.sweep]
enabled = true

[people.sweep.provider]
kind = "codex_app_server"
model = "gpt-test"
reasoning_effort = "high"
retention_posture = "zero_retention"
training_posture = "no_training"
allowed_sources = ["conversation_text"]
source_since = "2025-01-01"
`), 0o600))

	loaded, err := Load(path, "")
	requirements.NoError(err)
	provider := loaded.People.Sweep.Provider
	checks.Equal(peoplesweep.ProviderCodexAppServer, provider.Kind)
	checks.Empty(provider.Endpoint)
	checks.Empty(provider.APIKeyEnv)
	checks.Equal("codex", provider.Executable)
	checks.Equal(peoplesweep.CodexExecutionBoundaryV1, provider.ExecutionBoundary)
}

func TestLoadRejectsAnonymousPeopleProviderWithExplicitKeyEnv(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	path := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(os.WriteFile(path, []byte(`
[people.sweep]
enabled = true

[people.sweep.provider]
endpoint = "http://127.0.0.1:11434/v1"
model = "local-model"
api_key_env = "LOCAL_KEY"
allow_anonymous = true
retention_posture = "local_only"
training_posture = "local_only"
allowed_sources = ["conversation_text"]
source_since = "2025-01-01"
`), 0o600))

	_, err := Load(path, "")
	assert.ErrorContains(err, "anonymous mode cannot also configure api_key_env")
}

func TestPeopleSweepExistingTOMLRemainsCompatibleBesideEnrichment(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	path := filepath.Join(t.TempDir(), "config.toml")
	existing := []byte(`[people.sweep]
enabled = true

[people.sweep.provider]
kind = "openai_compatible"
endpoint = "https://api.example.test/v1/"
model = "gpt-test"
api_key_env = "TEST_KEY"
retention_posture = "zero_retention"
training_posture = "no_training"
allowed_sources = ["meeting_text", "conversation_text"]
source_since = "2025-01-01"
source_until = "2025-12-31"
allow_sensitive = true
request_timeout = "45s"
`)
	requirements.NoError(os.WriteFile(path, existing, 0o600))

	loaded, err := Load(path, "")
	requirements.NoError(err)
	checks.True(loaded.People.Sweep.Enabled)
	checks.Equal("gpt-test", loaded.People.Sweep.Provider.Model)
	checks.Equal(45*time.Second, loaded.People.Sweep.Provider.RequestTimeout)
	checks.False(loaded.People.Enrichment.Enabled)
	checks.Equal("*/15 * * * *", loaded.People.Enrichment.Schedule)

	invalid := []byte(`[people.sweep]
enabled = true

[people.sweep.provider]
model = "gpt-test"
retention_posture = "zero_retention"
training_posture = "no_training"
allowed_sources = ["raw_image"]
source_since = "2025-01-01"
`)
	requirements.NoError(os.WriteFile(path, invalid, 0o600))
	_, err = Load(path, "")
	requirements.ErrorContains(err, "allowed_sources")
}

func TestPeopleEnrichmentTOMLLoadsSiblingConfiguration(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	path := filepath.Join(t.TempDir(), "config.toml")
	requirements.NoError(os.WriteFile(path, []byte(`[people.enrichment]
enabled = true
schedule = "0 * * * *"
batch_size = 10
lease_duration = "10m"
suppression_key_env = "SUPPRESSION_KEY"

[[people.enrichment.providers]]
name = "exa-primary"
kind = "exa"
enabled = true
api_key_env = "EXA_KEY"
allowed_identifiers = ["name", "email"]
target_keys = ["attribute:bio"]
retention_posture = "zero_retention"
training_posture = "no_training"
refresh_interval = "24h"
max_requests_per_run = 10
max_requests_per_day = 100
`), 0o600))

	loaded, err := Load(path, "")
	requirements.NoError(err)
	checks.True(loaded.People.Enrichment.Enabled)
	checks.Equal("0 * * * *", loaded.People.Enrichment.Schedule)
	requirements.Len(loaded.People.Enrichment.Providers, 1)
	provider := loaded.People.Enrichment.Providers[0]
	checks.Equal(personenrichment.ProviderExa, provider.Kind)
	checks.Equal("https://api.exa.ai/search", provider.Endpoint)
	checks.Equal("people", provider.Mode)
	checks.Equal(1, provider.NumResults)
	checks.Equal(time.Minute, provider.RequestTimeout)
	checks.Equal(30*time.Second, provider.PollInterval)
	checks.Equal(15*time.Minute, provider.MaxJobAge)
	checks.Equal(5, provider.MaxRetries)
}
