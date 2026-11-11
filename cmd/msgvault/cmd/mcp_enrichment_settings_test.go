package cmd

import (
	"context"
	"encoding/json/v2"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/apiprotocol"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
	mcpserver "go.kenn.io/msgvault/internal/mcp"
	"go.kenn.io/msgvault/pkg/client/generated"
)

const enrichmentExaFixture = `[[people.enrichment.providers]]
name = "example"
kind = "exa"
enabled = false
endpoint = "https://provider.example/search"
mode = "people"
allowed_identifiers = ["name"]
target_keys = ["attribute:bio"]
retention_posture = "zero_retention"
training_posture = "no_training"
refresh_interval = "24h"
max_requests_per_run = 5
max_requests_per_day = 50
`

func TestEnrichmentReplacementRequiresReconstructablePolicy(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	enabled := true
	kind, endpoint, poll, refresh, timeout, interval, age, retention, training := "sixtyfour", "https://provider.example/search", "https://provider.example/poll", "24h", "1m", "1s", "1h", "zero_retention", "no_training"
	ids, keys := []string{"name"}, []string{"attribute:bio"}
	sensitive := false
	retries, run, day := int64(2), int64(10), int64(20)
	provider := enrichmentProvider{Credential: &generated.SecretSettingState{}, Name: "example", Kind: &kind, Enabled: &enabled, Endpoint: &endpoint, PollEndpoint: &poll, AllowedIdentifiers: &ids, TargetKeys: &keys, AllowSensitiveTargets: &sensitive, RetentionPosture: &retention, TrainingPosture: &training, RefreshInterval: &refresh, RequestTimeout: &timeout, PollInterval: &interval, MaxJobAge: &age, MaxRetries: &retries, MaxRequestsPerRun: &run, MaxRequestsPerDay: &day}
	replacement, ok := enrichmentReplacement(provider)
	require.True(ok)
	assert.Equal(endpoint, replacement.Endpoint)
	assert.Equal(ids, replacement.AllowedIdentifiers)
	assert.Equal(keys, replacement.TargetKeys)
	assert.Equal(day, replacement.MaxRequestsPerDay)
	provider.PollEndpoint = nil
	provider.Kind = new("exa")
	provider.Credential = &generated.SecretSettingState{}
	replacement, ok = enrichmentReplacement(provider)
	require.True(ok)
	assert.Equal("", *replacement.PollEndpoint)
	provider.PollEndpoint = &poll
	_, ok = enrichmentReplacement(provider)
	assert.False(ok)
	provider.PollEndpoint = nil
	provider.Credential = nil
	_, ok = enrichmentReplacement(provider)
	assert.False(ok)
	provider.PollEndpoint = &poll
	provider.Kind = new("sixtyfour")
	_, ok = enrichmentReplacement(provider)
	assert.False(ok)
	provider.Credential = &generated.SecretSettingState{}
	provider.Endpoint = nil
	_, ok = enrichmentReplacement(provider)
	assert.False(ok)
}

func TestEnrichmentPolicyNativeReplacement(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	directory := t.TempDir()
	path := filepath.Join(directory, "config.toml")
	require.NoError(os.WriteFile(path, []byte(enrichmentExaFixture), 0o600))
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
	for _, field := range []string{"num_results", "max_retries"} {
		_, _, err = backend.enrichmentOperationDisclosure(t.Context(), "update_enrichment_policy", map[string]any{"name": "example", "etag": policy.ETag, field: 0})
		require.Error(err)
		assert.Contains(err.Error(), "provider_policy_zero_uses_default")
	}
	args := map[string]any{"name": "example", "etag": policy.ETag, "max_requests_per_day": 25, "max_requests_per_run": 7, "num_results": 3, "max_retries": 2}
	disclosure, handled, err := backend.enrichmentOperationDisclosure(t.Context(), "update_enrichment_policy", args)
	require.NoError(err)
	require.True(handled)
	assert.NotContains(disclosure, "provider.example")
	result, handled, err = backend.executeEnrichmentOperation(t.Context(), "update_enrichment_policy", args)
	require.NoError(err)
	require.True(handled)
	updated := operationOutput[mcpserver.EnrichmentPolicy](t, result)
	assert.Equal(int64(25), updated.Providers[0].MaxRequestsPerDay)
	require.NotNil(updated.Providers[0].NumResults)
	assert.Equal(int64(3), *updated.Providers[0].NumResults)
	assert.Equal(int64(2), updated.Providers[0].MaxRetries)
	reloaded, err := config.Load(path, directory)
	require.NoError(err)
	require.Len(reloaded.People.Enrichment.Providers, 1)
	native := reloaded.People.Enrichment.Providers[0]
	assert.Equal("https://provider.example/search", native.Endpoint)
	assert.Empty(native.PollEndpoint)
	assert.Equal([]string{"attribute:bio"}, native.TargetKeys)
	assert.Equal(int64(7), native.MaxRequestsPerRun)
	assert.Equal("zero_retention", native.RetentionPosture)
	assert.Equal("no_training", native.TrainingPosture)
	assert.Equal("name", string(native.AllowedIdentifiers[0]))
	result, _, err = backend.executeEnrichmentOperation(t.Context(), "update_enrichment_policy", args)
	require.NoError(err)
	require.True(result.IsError)
	encoded, err := json.Marshal(result.Output)
	require.NoError(err)
	assert.Contains(string(encoded), "settings_conflict")
	enable := map[string]any{"name": "example", "etag": updated.ETag, "enabled": true, "num_results": 1}
	disclosure, _, err = backend.enrichmentOperationDisclosure(t.Context(), "update_enrichment_policy", enable)
	require.NoError(err)
	assert.Contains(disclosure, "allowed_identifiers")
	assert.Contains(disclosure, "zero_retention")
	result, _, err = backend.executeEnrichmentOperation(t.Context(), "update_enrichment_policy", enable)
	require.NoError(err)
	enabledPolicy := operationOutput[mcpserver.EnrichmentPolicy](t, result)
	assert.True(enabledPolicy.Providers[0].Enabled)

	// A real settings snapshot with enrichment enabled cannot lose its last provider.
	secondDir := t.TempDir()
	secondPath := filepath.Join(secondDir, "config.toml")
	initial := "[people.enrichment]\nenabled = true\nsuppression_key_env = \"TEST_SUPPRESSION_KEY\"\n" + strings.Replace(enrichmentExaFixture, "enabled = false", "enabled = true", 1)
	require.NoError(os.WriteFile(secondPath, []byte(initial), 0o600))
	secondCfg, err := config.Load(secondPath, secondDir)
	require.NoError(err)
	secondCfg.Data.DataDir = secondDir
	second := sourceOperationFixture(t, api.NewServer(secondCfg, nil, nil, slog.New(slog.DiscardHandler)))
	result, _, err = second.executeEnrichmentOperation(t.Context(), "get_enrichment_policy", nil)
	require.NoError(err)
	current := operationOutput[mcpserver.EnrichmentPolicy](t, result)
	disable := map[string]any{"name": "example", "etag": current.ETag, "enabled": false}
	_, _, err = second.enrichmentOperationDisclosure(t.Context(), "update_enrichment_policy", disable)
	require.Error(err)
	assert.Contains(err.Error(), "provider_policy_requires_enabled_provider")
	result, _, err = second.executeEnrichmentOperation(t.Context(), "update_enrichment_policy", disable)
	require.NoError(err)
	assert.True(result.IsError)
	after, err := os.ReadFile(secondPath)
	require.NoError(err)
	assert.Equal(initial, string(after))
}

func TestEnrichmentCapabilityRequiresExactRouteAndProperties(t *testing.T) {
	assert := assert.New(t)
	props := []string{"kind", "enabled", "endpoint", "poll_endpoint", "mode", "tier", "num_results", "allowed_identifiers", "target_keys", "allow_sensitive_targets", "retention_posture", "training_posture", "refresh_interval", "request_timeout", "poll_interval", "max_job_age", "max_retries", "max_requests_per_run", "max_requests_per_day"}
	caps := &apiprotocol.MCPCapabilities{Routes: []apiprotocol.MCPRouteDescriptor{{OperationID: "putSettingsPersonEnrichmentProvider", RequestProperties: props}}}
	hasRoute := func(id, method, path string, _ ...string) bool {
		return (id == "getSettings" && method == http.MethodGet && path == "/api/v1/settings") || (id == "putSettingsPersonEnrichmentProvider" && method == http.MethodPut && path == "/api/v1/settings/person-enrichment/providers/{name}")
	}
	assert.Equal([]string{"get_enrichment_policy", "update_enrichment_policy"}, enrichmentMCPCapabilities(hasRoute, caps))
	for i := range props {
		caps.Routes[0].RequestProperties = append(append([]string{}, props[:i]...), props[i+1:]...)
		assert.Equal([]string{"get_enrichment_policy"}, enrichmentMCPCapabilities(hasRoute, caps), props[i])
	}
	caps.Routes[0].RequestProperties = append(append([]string{}, props...), "future_field")
	assert.Equal([]string{"get_enrichment_policy"}, enrichmentMCPCapabilities(hasRoute, caps))
	assert.Empty(enrichmentMCPCapabilities(func(string, string, string, ...string) bool { return false }, caps))
}

func TestEnrichmentProjectionRejectsMissingPolicyFields(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("ETag", `"revision"`)
		_, _ = w.Write([]byte(`{"person_enrichment_providers":[{"name":"example"}]}`))
	}))
	t.Cleanup(server.Close)
	client, err := daemonclient.New(daemonclient.Config{URL: server.URL, AllowInsecure: true})
	require.NoError(err)
	backend := &daemonMCPOperations{client: client}
	result, _, err := backend.executeEnrichmentOperation(t.Context(), "get_enrichment_policy", nil)
	require.NoError(err)
	assert.True(result.IsError)
}

func TestEnrichmentPolicySDKRequiresApproval(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	directory := t.TempDir()
	path := filepath.Join(directory, "config.toml")
	require.NoError(os.WriteFile(path, []byte(enrichmentExaFixture), 0o600))
	cfg, err := config.Load(path, directory)
	require.NoError(err)
	cfg.Data.DataDir = directory
	backend := sourceOperationFixture(t, api.NewServer(cfg, nil, nil, slog.New(slog.DiscardHandler)))
	approvals := 0
	session := operationMCPSession(t, backend, []mcpserver.OperationFamily{mcpserver.OperationFamilyEnrichmentSettings}, &sdkmcp.ClientOptions{ElicitationHandler: func(_ context.Context, request *sdkmcp.ElicitRequest) (*sdkmcp.ElicitResult, error) {
		approvals++
		assert.Contains(request.Params.Message, "stored credential")
		assert.NotContains(request.Params.Message, "provider.example")
		return &sdkmcp.ElicitResult{Action: "accept", Content: map[string]any{"confirm": true}}, nil
	}})
	result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "get_enrichment_policy"})
	require.NoError(err)
	require.False(result.IsError)
	data, err := json.Marshal(result.StructuredContent)
	require.NoError(err)
	var policy mcpserver.EnrichmentPolicy
	require.NoError(json.Unmarshal(data, &policy))
	result, err = session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "update_enrichment_policy", Arguments: map[string]any{"name": "example", "etag": policy.ETag, "max_requests_per_day": 25}})
	require.NoError(err)
	require.False(result.IsError)
	assert.Equal(1, approvals)
}

func TestEnrichmentRefusesUnreconstructableAndUnknownProviders(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	directory := t.TempDir()
	path := filepath.Join(directory, "config.toml")
	require.NoError(os.WriteFile(path, []byte(enrichmentExaFixture), 0o600))
	cfg, err := config.Load(path, directory)
	require.NoError(err)
	cfg.Data.DataDir = directory
	backend := sourceOperationFixture(t, api.NewServer(cfg, nil, nil, slog.New(slog.DiscardHandler)))
	result, err := backend.ExecuteOperation(t.Context(), "get_enrichment_policy", nil)
	require.NoError(err)
	policy := operationOutput[mcpserver.EnrichmentPolicy](t, result)
	require.Len(policy.Providers, 1)
	for name, code := range map[string]string{"missing": "provider_not_found"} {
		result, err = backend.ExecuteOperation(t.Context(), "update_enrichment_policy", map[string]any{"name": name, "etag": policy.ETag, "enabled": true})
		require.NoError(err)
		require.True(result.IsError)
		encoded, err := json.Marshal(result.Output)
		require.NoError(err)
		assert.Contains(string(encoded), code)
	}
	reloaded, err := config.Load(path, directory)
	require.NoError(err)
	require.Len(reloaded.People.Enrichment.Providers, 1)
	assert.False(reloaded.People.Enrichment.Providers[0].Enabled)
}

func TestEnrichmentWritePreservesUncertainty(t *testing.T) {
	for _, tc := range []struct {
		status int
		code   string
	}{{http.StatusUnprocessableEntity, "validation_failed"}, {http.StatusInternalServerError, "settings_write_failed"}} {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			sent := ""
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.Method == http.MethodGet {
					w.Header().Set("ETag", `"revision"`)
					_, _ = w.Write([]byte(`{"person_enrichment_providers":[{"name":"example","kind":"sixtyfour","enabled":false,"endpoint":"https://provider.example/search","poll_endpoint":"https://provider.example/poll","allowed_identifiers":["name"],"target_keys":["attribute:bio"],"allow_sensitive_targets":false,"retention_posture":"zero_retention","training_posture":"no_training","refresh_interval":"24h","request_timeout":"1m","poll_interval":"1s","max_job_age":"1h","max_retries":1,"max_requests_per_run":5,"max_requests_per_day":50,"credential":{"configured":false,"source":"none"}}]}`))
					return
				}
				sent = r.Header.Get("If-Match")
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(`{"error":"` + tc.code + `"}`))
			}))
			t.Cleanup(server.Close)
			client, err := daemonclient.New(daemonclient.Config{URL: server.URL, AllowInsecure: true})
			require.NoError(err)
			backend := &daemonMCPOperations{client: client}
			result, _, err := backend.executeEnrichmentOperation(t.Context(), "update_enrichment_policy", map[string]any{"name": "example", "etag": `"revision"`, "max_requests_per_day": 25})
			require.NoError(err)
			require.True(result.IsError)
			encoded, err := json.Marshal(result.Output)
			require.NoError(err)
			assert.Equal(`"revision"`, sent)
			assert.Contains(string(encoded), `"operation_may_have_completed":true`)
		})
	}
}
