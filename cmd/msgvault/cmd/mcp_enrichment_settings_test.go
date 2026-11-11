package cmd

import (
	"encoding/json/v2"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/apiprotocol"
	"go.kenn.io/msgvault/internal/config"
	mcpserver "go.kenn.io/msgvault/internal/mcp"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEnrichmentReplacementRequiresReconstructablePolicy(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	enabled := true
	kind, endpoint, poll, refresh, timeout, interval, age, retention, training := "exa", "https://provider.example/search", "https://provider.example/poll", "24h", "1m", "1s", "1h", "zero_retention", "no_training"
	ids, keys := []string{"name"}, []string{"attribute:bio"}
	sensitive := false
	retries, run, day := int64(2), int64(10), int64(20)
	provider := enrichmentProvider{Name: "example", Kind: &kind, Enabled: &enabled, Endpoint: &endpoint, PollEndpoint: &poll, AllowedIdentifiers: &ids, TargetKeys: &keys, AllowSensitiveTargets: &sensitive, RetentionPosture: &retention, TrainingPosture: &training, RefreshInterval: &refresh, RequestTimeout: &timeout, PollInterval: &interval, MaxJobAge: &age, MaxRetries: &retries, MaxRequestsPerRun: &run, MaxRequestsPerDay: &day}
	replacement, ok := enrichmentReplacement(provider)
	require.True(ok)
	assert.Equal(endpoint, replacement.Endpoint)
	assert.Equal(ids, replacement.AllowedIdentifiers)
	assert.Equal(keys, replacement.TargetKeys)
	assert.Equal(day, replacement.MaxRequestsPerDay)
	provider.PollEndpoint = nil
	_, ok = enrichmentReplacement(provider)
	assert.False(ok)
	provider.PollEndpoint = &poll
	provider.Endpoint = nil
	_, ok = enrichmentReplacement(provider)
	assert.False(ok)
}

func TestEnrichmentPolicyNativeReplacement(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	directory := t.TempDir()
	path := filepath.Join(directory, "config.toml")
	require.NoError(os.WriteFile(path, []byte(`[[people.enrichment.providers]]
name = "example"
kind = "sixtyfour"
enabled = false
endpoint = "https://provider.example/search"
poll_endpoint = "https://provider.example/poll"
tier = "standard"
allowed_identifiers = ["name"]
target_keys = ["attribute:bio"]
retention_posture = "zero_retention"
training_posture = "no_training"
refresh_interval = "24h"
max_requests_per_run = 5
max_requests_per_day = 50
`), 0o600))
	cfg, err := config.Load(path, directory)
	require.NoError(err)
	cfg.Data.DataDir = directory
	backend := sourceOperationFixture(t, api.NewServer(cfg, nil, nil, slog.New(slog.DiscardHandler)))
	result, handled, err := backend.executeEnrichmentOperation(t.Context(), "get_enrichment_policy", nil)
	require.NoError(err)
	require.True(handled)
	policy := operationOutput[mcpserver.EnrichmentPolicy](t, result)
	require.Len(policy.Providers, 1)
	require.NotEmpty(policy.ETag)
	data, err := json.Marshal(policy)
	require.NoError(err)
	assert.NotContains(string(data), "provider.example")
	args := map[string]any{"name": "example", "etag": policy.ETag, "max_requests_per_day": 25}
	disclosure, handled, err := backend.enrichmentOperationDisclosure(t.Context(), "update_enrichment_policy", args)
	require.NoError(err)
	require.True(handled)
	assert.NotContains(disclosure, "provider.example")
	result, handled, err = backend.executeEnrichmentOperation(t.Context(), "update_enrichment_policy", args)
	require.NoError(err)
	require.True(handled)
	updated := operationOutput[mcpserver.EnrichmentPolicy](t, result)
	assert.Equal(int64(25), updated.Providers[0].MaxRequestsPerDay)
	reloaded, err := config.Load(path, directory)
	require.NoError(err)
	require.Len(reloaded.People.Enrichment.Providers, 1)
	native := reloaded.People.Enrichment.Providers[0]
	assert.Equal("https://provider.example/search", native.Endpoint)
	assert.Equal("https://provider.example/poll", native.PollEndpoint)
	assert.Equal([]string{"attribute:bio"}, native.TargetKeys)
	assert.Equal(int64(5), native.MaxRequestsPerRun)
	result, _, err = backend.executeEnrichmentOperation(t.Context(), "update_enrichment_policy", args)
	require.NoError(err)
	require.True(result.IsError)
	encoded, err := json.Marshal(result.Output)
	require.NoError(err)
	assert.Contains(string(encoded), "settings_conflict")
}

func TestEnrichmentCapabilityRequiresExactRouteAndProperties(t *testing.T) {
	assert := assert.New(t)
	props := []string{"kind", "enabled", "endpoint", "poll_endpoint", "mode", "tier", "num_results", "allowed_identifiers", "target_keys", "allow_sensitive_targets", "retention_posture", "training_posture", "refresh_interval", "request_timeout", "poll_interval", "max_job_age", "max_retries", "max_requests_per_run", "max_requests_per_day"}
	caps := &apiprotocol.MCPCapabilities{Routes: []apiprotocol.MCPRouteDescriptor{{OperationID: "putSettingsPersonEnrichmentProvider", RequestProperties: props}}}
	hasRoute := func(id, method, path string, _ ...string) bool {
		return (id == "getSettings" && method == http.MethodGet && path == "/api/v1/settings") || (id == "putSettingsPersonEnrichmentProvider" && method == http.MethodPut && path == "/api/v1/settings/person-enrichment/providers/{name}")
	}
	assert.Equal([]string{"get_enrichment_policy", "update_enrichment_policy"}, enrichmentMCPCapabilities(hasRoute, caps))
	caps.Routes[0].RequestProperties = props[:len(props)-1]
	assert.Equal([]string{"get_enrichment_policy"}, enrichmentMCPCapabilities(hasRoute, caps))
	assert.Empty(enrichmentMCPCapabilities(func(string, string, string, ...string) bool { return false }, caps))
}

func TestEnrichmentProjectionRejectsMissingPolicyFields(t *testing.T) {
	assert := assert.New(t)
	enabled := false
	retries, run, day := int64(1), int64(2), int64(3)
	result := enrichmentProjection(&mcpserver.OperationResult{ETag: `"revision"`, Output: enrichmentSettings{Providers: []enrichmentProvider{{Name: "example", Enabled: &enabled, MaxRetries: &retries, MaxRequestsPerRun: &run, MaxRequestsPerDay: &day}}}}, false)
	assert.False(result.IsError)
	policy := result.Output.(mcpserver.EnrichmentPolicy)
	assert.Equal(`"revision"`, policy.ETag)
	assert.False(policy.Providers[0].Enabled)
	broken := enrichmentProjection(&mcpserver.OperationResult{ETag: `"revision"`, Output: enrichmentSettings{Providers: []enrichmentProvider{{Name: "example"}}}}, false)
	assert.True(broken.IsError)
}
