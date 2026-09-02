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
	_, provider, err := config.ActiveProviderConfig()
	require.NoError(t, err)

	assert.False(config.Enabled)
	assert.Equal(peoplesweep.ProtocolOpenAIChat, provider.Protocol)
	assert.Equal("https://api.openai.com/v1", provider.Endpoint)
	assert.Equal("OPENAI_API_KEY", provider.CredentialEnv)
	assert.Equal(time.Minute, provider.RequestTimeout)
}

func TestLoadPeopleSweepProviderConfig(t *testing.T) {
	assert := assert.New(t)
	path := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(t, os.WriteFile(path, []byte(`
[people.sweep]
enabled = true
provider = "primary"

[people.sweep.providers.primary]
protocol = "openai_chat"
endpoint = "https://api.example.test/v1/"
model = "gpt-test"
auth = "bearer"
credential = "env"
credential_env = "TEST_KEY"
output_mode = "native_json_schema"
token_limit_parameter = "max_completion_tokens"
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
	name, provider, err := loaded.People.Sweep.ActiveProviderConfig()
	require.NoError(t, err)
	assert.True(loaded.People.Sweep.Enabled)
	assert.Equal("primary", name)
	assert.Equal(peoplesweep.ProtocolOpenAIChat, provider.Protocol)
	assert.Equal("https://api.example.test/v1/", provider.Endpoint)
	assert.Equal("gpt-test", provider.Model)
	assert.Equal("TEST_KEY", provider.CredentialEnv)
	assert.Equal(peoplesweep.AuthBearer, provider.Auth)
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
provider = "primary"

[people.sweep.providers.primary]
protocol = "openai_chat"
endpoint = "https://api.example.test/v1"
model = "gpt-test"
auth = "bearer"
credential = "env"
credential_env = "TEST_KEY"
output_mode = "native_json_schema"
token_limit_parameter = "max_completion_tokens"
retention_posture = "zero_retention"
training_posture = "no_training"
allowed_sources = ["raw_image"]
source_since = "2025-01-01"
`), 0o600))

	_, err := Load(path, "")
	require.ErrorContains(t, err, "allowed_sources")
}

func TestLoadRejectsEmptyPeopleProviderCredentialEnv(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(t, os.WriteFile(path, []byte(`
[people.sweep]
enabled = true
provider = "primary"

[people.sweep.providers.primary]
protocol = "openai_chat"
endpoint = "https://api.example.test/v1"
model = "gpt-test"
auth = "bearer"
credential = "env"
credential_env = ""
output_mode = "native_json_schema"
token_limit_parameter = "max_completion_tokens"
retention_posture = "zero_retention"
training_posture = "no_training"
allowed_sources = ["conversation_text"]
source_since = "2025-01-01"
`), 0o600))

	_, err := Load(path, "")
	require.ErrorContains(t, err, "credential_env")
}

func TestLoadAllowsUnauthenticatedLoopbackPeopleProvider(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	path := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(os.WriteFile(path, []byte(`
[people.sweep]
enabled = true
provider = "local"

[people.sweep.providers.local]
protocol = "openai_chat"
endpoint = "http://127.0.0.1:11434/v1"
model = "local-model"
auth = "none"
credential = "none"
output_mode = "native_json_schema"
token_limit_parameter = "max_completion_tokens"
retention_posture = "local_only"
training_posture = "local_only"
allowed_sources = ["conversation_text"]
source_since = "2025-01-01"
`), 0o600))

	loaded, err := Load(path, "")
	require.NoError(err)
	_, provider, err := loaded.People.Sweep.ActiveProviderConfig()
	require.NoError(err)
	assert.Equal(peoplesweep.AuthNone, provider.Auth)
	assert.Empty(provider.CredentialEnv)
}

func TestLoadCodexPeopleProviderUsesCodexOnlyDefaults(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	path := filepath.Join(t.TempDir(), "config.toml")
	requirements.NoError(os.WriteFile(path, []byte(`
[people.sweep]
enabled = true
provider = "codex"

[people.sweep.providers.codex]
protocol = "codex_app_server"
model = "gpt-test"
auth = "none"
credential = "none"
reasoning_effort = "high"
retention_posture = "zero_retention"
training_posture = "no_training"
allowed_sources = ["conversation_text"]
source_since = "2025-01-01"
`), 0o600))

	loaded, err := Load(path, "")
	requirements.NoError(err)
	_, provider, err := loaded.People.Sweep.ActiveProviderConfig()
	requirements.NoError(err)
	checks.Equal(peoplesweep.ProtocolCodexAppServer, provider.Protocol)
	checks.Empty(provider.Endpoint)
	checks.Empty(provider.CredentialEnv)
	checks.Equal("codex", provider.Executable)
	checks.Equal(peoplesweep.CodexExecutionBoundaryV1, provider.ExecutionBoundary)
}

func TestLoadRejectsUnauthenticatedPeopleProviderWithCredentialEnv(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(t, os.WriteFile(path, []byte(`
[people.sweep]
enabled = true
provider = "local"

[people.sweep.providers.local]
protocol = "openai_chat"
endpoint = "http://127.0.0.1:11434/v1"
model = "local-model"
auth = "none"
credential = "none"
credential_env = "LOCAL_KEY"
output_mode = "native_json_schema"
token_limit_parameter = "max_completion_tokens"
retention_posture = "local_only"
training_posture = "local_only"
allowed_sources = ["conversation_text"]
source_since = "2025-01-01"
`), 0o600))

	_, err := Load(path, "")
	require.ErrorContains(t, err, "credential_env requires credential=env")
}

func TestConfigLoadsNamedProviderProfiles(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	path := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(os.WriteFile(path, []byte(`
[people.sweep]
enabled = true
provider = "glm"

[people.sweep.providers.glm]
protocol = "openai_chat"
endpoint = "https://api.z.ai/api/paas/v4"
model = "glm-5.3"
auth = "bearer"
credential = "env"
credential_env = "ZAI_API_KEY"
output_mode = "json_object"
token_limit_parameter = "max_tokens"
reasoning_effort = "max"
retention_posture = "provider-declared"
training_posture = "provider-declared"
allowed_sources = ["conversation_text"]
source_since = "2026-01-01"
`), 0o600))

	loaded, err := Load(path, "")
	require.NoError(err)
	name, provider, err := loaded.People.Sweep.ActiveProviderConfig()
	require.NoError(err)
	assert.Equal("glm", name)
	assert.Equal(peoplesweep.ProtocolOpenAIChat, provider.Protocol)
	assert.Equal("https://api.z.ai/api/paas/v4", provider.Endpoint)
	assert.Equal("glm-5.3", provider.Model)
	assert.Equal(peoplesweep.AuthBearer, provider.Auth)
	assert.Equal(peoplesweep.CredentialEnv, provider.Credential)
	assert.Equal("ZAI_API_KEY", provider.CredentialEnv)
	assert.Equal(peoplesweep.OutputModeJSONObject, provider.OutputMode)
	assert.Equal("max_tokens", provider.TokenLimitParameter)
	assert.Equal("max", provider.ReasoningEffort)
}

func TestSaveReloadsNamedPeopleProviderSelection(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	path := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(os.WriteFile(path, []byte(`
[people.sweep]
enabled = true
provider = "glm"

[people.sweep.providers.glm]
protocol = "openai_chat"
endpoint = "https://api.z.ai/api/paas/v4"
model = "glm-5.3"
auth = "bearer"
credential = "env"
credential_env = "ZAI_API_KEY"
output_mode = "json_object"
token_limit_parameter = "max_tokens"
retention_posture = "provider-declared"
training_posture = "provider-declared"
allowed_sources = ["conversation_text"]
source_since = "2026-01-01"
`), 0o600))

	loaded, err := Load(path, "")
	require.NoError(err)
	require.NoError(loaded.Save())

	saved, err := os.ReadFile(path)
	require.NoError(err)
	assert.Contains(string(saved), `provider = "glm"`)

	reloaded, err := Load(path, "")
	require.NoError(err)
	assert.Equal("glm", reloaded.People.Sweep.Provider.Name)
}

// TestConfigAllowsPublishedProfilesWithoutSelectionWhileDisabled covers the
// onboarding order: `person provider add` publishes a profile before anyone
// selects it, and the file must stay loadable in between.
func TestConfigAllowsPublishedProfilesWithoutSelectionWhileDisabled(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	path := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(os.WriteFile(path, []byte(`
[people.sweep.providers.glm]
protocol = "openai_chat"
endpoint = "https://api.z.ai/api/paas/v4"
model = "glm-5.3"
auth = "bearer"
credential = "env"
credential_env = "ZAI_API_KEY"
output_mode = "json_object"
token_limit_parameter = "max_tokens"
retention_posture = "provider-declared"
training_posture = "provider-declared"
allowed_sources = ["conversation_text"]
source_since = "2026-01-01"
`), 0o600))

	loaded, err := Load(path, "")
	require.NoError(err)
	assert.False(loaded.People.Sweep.Enabled)
	assert.Empty(loaded.People.Sweep.Provider.Name)
	assert.Contains(loaded.People.Sweep.Providers, "glm")

	enabled := loaded.People.Sweep
	enabled.Enabled = true
	require.ErrorContains(enabled.Validate(), "provider profile name is required")
}

func TestConfigRejectsProviderTableAsSelector(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(t, os.WriteFile(path, []byte(`
[people.sweep.provider]
protocol = "openai_chat"
`), 0o600))

	_, err := Load(path, "")
	require.ErrorContains(t, err, "provider must be a profile name")
}

func TestConfigRejectsMissingActiveProvider(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(t, os.WriteFile(path, []byte(`
[people.sweep]
provider = "missing"

[people.sweep.providers.glm]
protocol = "openai_chat"
`), 0o600))

	_, err := Load(path, "")
	require.ErrorContains(t, err, "missing")
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
