package cmd

import (
	"context"
	"encoding/json/v2"
	"log/slog"
	"os"
	"slices"
	"strings"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/apiprotocol"
	"go.kenn.io/msgvault/internal/config"
	mcpserver "go.kenn.io/msgvault/internal/mcp"
	"go.kenn.io/msgvault/internal/providercredentials"
)

func TestMCPSettingsOperationsDiscoverActualOwningRoutes(t *testing.T) {
	requirements, assertions := require.New(t), assert.New(t)
	cfg := config.NewDefaultConfig()
	cfg.HomeDir = t.TempDir()
	cfg.Data.DataDir = cfg.HomeDir
	requirements.NoError(cfg.Save())
	backend := sourceOperationFixture(t, api.NewServerWithOptions(api.ServerOptions{Config: cfg, Logger: slog.New(slog.DiscardHandler)}))
	for _, name := range []string{"get_operational_settings", "update_operational_settings", "get_enrichment_policy", "update_enrichment_controls", "update_enrichment_policy"} {
		requirements.Contains(backend.capabilities(), name)
	}
	capabilities, err := backend.client.MCPCapabilities(t.Context())
	requirements.NoError(err)
	for _, dependency := range []struct{ route, tool string }{{"getSettings", "get_operational_settings"}, {"getSettings", "update_enrichment_policy"}, {"patchSettings", "update_operational_settings"}, {"patchSettings", "update_enrichment_controls"}, {"putSettingsPersonEnrichmentProvider", "update_enrichment_policy"}} {
		limited := *capabilities
		limited.Routes = slices.DeleteFunc(slices.Clone(capabilities.Routes), func(route apiprotocol.MCPRouteDescriptor) bool { return route.OperationID == dependency.route })
		assertions.NotContains(newDaemonMCPOperations(backend.client, &limited).capabilities(), dependency.tool)
	}
	limited := *capabilities
	limited.Routes = slices.Clone(capabilities.Routes)
	for i := range limited.Routes {
		if limited.Routes[i].OperationID == "patchSettings" || limited.Routes[i].OperationID == "putSettingsPersonEnrichmentProvider" {
			limited.Routes[i].RequestProperties = nil
		}
	}
	unsupported := newDaemonMCPOperations(backend.client, &limited).capabilities()
	for _, name := range []string{"update_operational_settings", "update_enrichment_controls", "update_enrichment_policy"} {
		assertions.NotContains(unsupported, name)
	}
	limited.Delegated = true
	assertions.NotContains(newDaemonMCPOperations(backend.client, &limited).capabilities(), "get_operational_settings")
}

func TestMCPSettingsTypedNativeWritesPreserveFalseZeroETagAndSecretBoundary(t *testing.T) {
	requirements, assertions := require.New(t), assert.New(t)
	cfg := config.NewDefaultConfig()
	cfg.HomeDir = t.TempDir()
	cfg.Data.DataDir = cfg.HomeDir
	cfg.Vector.DBPath = cfg.HomeDir + "/synthetic-private-vectors.db"
	cfg.Vector.Embeddings.Endpoint = "https://embedding.example.test/v1"
	requirements.NoError(cfg.Save())
	credentials, err := providercredentials.Read(cfg.TokensDir())
	requirements.NoError(err)
	_, err = providercredentials.Put(cfg.TokensDir(), credentials.ETag, providercredentials.VectorEmbeddingsID, cfg.Vector.Embeddings.Endpoint, "fixture-secret-hint-abcdef")
	requirements.NoError(err)
	backend := sourceOperationFixture(t, api.NewServerWithOptions(api.ServerOptions{Config: cfg, Logger: slog.New(slog.DiscardHandler)}))
	result, err := backend.ExecuteOperation(t.Context(), "get_operational_settings", nil)
	requirements.NoError(err)
	requirements.False(result.IsError)
	current := operationOutput[mcpserver.OperationalSettings](t, result)
	requirements.NotEmpty(current.ETag)
	wire, err := json.Marshal(current)
	requirements.NoError(err)
	assertions.NotContains(string(wire), "fixture-secret-hint-abcdef")
	assertions.NotContains(string(wire), "fix…def")
	assertions.NotContains(string(wire), cfg.HomeDir)
	for _, setting := range current.Settings {
		if !slices.Contains(mcpserver.OperationalSettingKeys(), setting.Key) {
			requirements.NotNil(setting.ReadOnly)
			assertions.True(*setting.ReadOnly)
			assertions.Nil(setting.Value)
		}
	}
	args := map[string]any{"etag": current.ETag, "updates": []map[string]any{
		{"key": "web.theme", "value": map[string]any{"string": "dark"}},
		{"key": "slack.dms", "value": map[string]any{"boolean": false}},
		{"key": "slack.max_media_mb", "value": map[string]any{"integer": 0}},
		{"key": "slack.channels", "value": map[string]any{"strings": []string{}}},
	}}
	approvals := 0
	session := operationMCPSession(t, backend, []mcpserver.OperationFamily{mcpserver.OperationFamilySettings}, &sdkmcp.ClientOptions{ElicitationHandler: func(_ context.Context, request *sdkmcp.ElicitRequest) (*sdkmcp.ElicitResult, error) {
		approvals++
		assertions.Contains(request.Params.Message, strings.Trim(current.ETag, `"`))
		return &sdkmcp.ElicitResult{Action: "accept", Content: map[string]any{"confirm": true}}, nil
	}})
	called, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "update_operational_settings", Arguments: args})
	requirements.NoError(err)
	requirements.False(called.IsError, "%s", settingsMCPDiagnostic(called))
	assertions.Equal(1, approvals)
	loaded, err := config.Load(cfg.ConfigFilePath(), cfg.HomeDir)
	requirements.NoError(err)
	assertions.Equal("dark", loaded.Web.Theme)
	requirements.NotNil(loaded.Slack.DMs)
	assertions.False(*loaded.Slack.DMs)
	assertions.Zero(loaded.Slack.MaxMediaMB)
	assertions.Empty(loaded.Slack.Channels)
	wire, err = json.Marshal(called.StructuredContent)
	requirements.NoError(err)
	assertions.Contains(string(wire), `"pending_restart":true`)
	assertions.NotContains(string(wire), "fix…def")
	called, err = session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "update_operational_settings", Arguments: args})
	requirements.NoError(err)
	assertions.True(called.IsError)
	assertions.Equal(1, approvals, "stale observation refuses before confirmation")
	result, err = backend.ExecuteOperation(t.Context(), "get_operational_settings", nil)
	requirements.NoError(err)
	current = operationOutput[mcpserver.OperationalSettings](t, result)
	before, err := os.ReadFile(cfg.ConfigFilePath())
	requirements.NoError(err)
	invalid := []map[string]any{
		{"key": "vector.embeddings.endpoint", "value": map[string]any{"string": "https://different.example.test"}},
		{"key": "server.api_key", "value": map[string]any{"string": "secret"}},
		{"key": "web.theme", "secret": map[string]any{"action": "clear"}},
		{"key": "web.theme", "value": map[string]any{"string": nil}},
		{"key": "web.theme", "value": map[string]any{"string": "light", "boolean": false}},
		{"key": "web.theme", "value": map[string]any{"string": "light", "arbitrary": true}},
	}
	for _, update := range invalid {
		refused, err := backend.ExecuteOperation(t.Context(), "update_operational_settings", map[string]any{"etag": current.ETag, "updates": []map[string]any{update}})
		requirements.NoError(err)
		assertions.True(refused.IsError)
	}
	refused, err := backend.ExecuteOperation(t.Context(), "update_operational_settings", map[string]any{"etag": current.ETag, "updates": []map[string]any{{"key": "web.theme", "value": map[string]any{"boolean": false}}, {"key": "log.level", "value": map[string]any{"string": "debug"}}}})
	requirements.NoError(err)
	assertions.True(refused.IsError, "native cross-field/type validation refuses the entire transaction")
	after, err := os.ReadFile(cfg.ConfigFilePath())
	requirements.NoError(err)
	assertions.Equal(string(before), string(after))
}

func TestMCPEnrichmentUsesNativeReplacementWithoutCreationOrDestinationChange(t *testing.T) {
	requirements, assertions := require.New(t), assert.New(t)
	cfg := config.NewDefaultConfig()
	cfg.HomeDir = t.TempDir()
	cfg.Data.DataDir = cfg.HomeDir
	cfg.People.Enrichment = personEnrichmentCLIConfig("MCP_SYNTHETIC_SUPPRESSION_KEY")
	cfg.People.Enrichment.Enabled = false
	cfg.People.Enrichment.ApplyDefaults()
	requirements.NoError(cfg.Save())
	original := cfg.People.Enrichment.Providers[0]
	credentials, err := providercredentials.Read(cfg.TokensDir())
	requirements.NoError(err)
	credentials, err = providercredentials.Put(cfg.TokensDir(), credentials.ETag, providercredentials.PersonEnrichmentID(original.Name), original.Endpoint, "synthetic-enrichment-secret-123456")
	requirements.NoError(err)
	backend := sourceOperationFixture(t, api.NewServerWithOptions(api.ServerOptions{Config: cfg, Logger: slog.New(slog.DiscardHandler)}))
	read := func() mcpserver.EnrichmentPolicy {
		t.Helper()
		result, err := backend.ExecuteOperation(t.Context(), "get_enrichment_policy", nil)
		requirements.NoError(err)
		requirements.False(result.IsError)
		return operationOutput[mcpserver.EnrichmentPolicy](t, result)
	}
	current := read()
	requirements.Len(current.Providers, 1)
	requirements.NotNil(current.Providers[0].Credential)
	assertions.True(current.Providers[0].Credential.Configured)
	assertions.Nil(current.Providers[0].Credential.Hint)
	session := operationMCPSession(t, backend, []mcpserver.OperationFamily{mcpserver.OperationFamilyEnrichment}, &sdkmcp.ClientOptions{ElicitationHandler: func(_ context.Context, request *sdkmcp.ElicitRequest) (*sdkmcp.ElicitResult, error) {
		assertions.Contains(request.Params.Message, original.Endpoint)
		assertions.NotContains(request.Params.Message, "synthetic-enrichment-secret-123456")
		return &sdkmcp.ElicitResult{Action: "accept", Content: map[string]any{"confirm": true}}, nil
	}})
	called, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "get_enrichment_policy"})
	requirements.NoError(err)
	requirements.False(called.IsError, "%s", settingsMCPDiagnostic(called))
	args := map[string]any{"etag": current.ETag, "name": original.Name, "changes": map[string]any{"enabled": false, "allow_sensitive_targets": false, "allowed_identifiers": []string{}, "target_keys": []string{}, "max_requests_per_run": 0, "max_requests_per_day": 0}}
	called, err = session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "update_enrichment_policy", Arguments: args})
	requirements.NoError(err)
	loaded, err := config.Load(cfg.ConfigFilePath(), cfg.HomeDir)
	requirements.NoError(err)
	requirements.False(called.IsError, "%s", settingsMCPDiagnostic(called))
	requirements.Len(loaded.People.Enrichment.Providers, 1)
	changed := loaded.People.Enrichment.Providers[0]
	assertions.False(changed.Enabled)
	assertions.False(changed.AllowSensitiveTargets)
	assertions.Empty(changed.AllowedIdentifiers)
	assertions.Empty(changed.TargetKeys)
	assertions.Zero(changed.MaxRequestsPerRun)
	assertions.Zero(changed.MaxRequestsPerDay)
	assertions.Equal(original.Endpoint, changed.Endpoint)
	assertions.Equal(original.PollEndpoint, changed.PollEndpoint)
	assertions.Equal(original.Kind, changed.Kind)
	assertions.Equal(original.APIKeyEnv, changed.APIKeyEnv)
	assertions.Equal(original.RefreshInterval, changed.RefreshInterval)
	assertions.Equal(original.RequestTimeout, changed.RequestTimeout)
	assertions.Equal(original.MaxRetries, changed.MaxRetries)
	afterCredentials, err := providercredentials.Read(cfg.TokensDir())
	requirements.NoError(err)
	assertions.Equal(credentials.ETag, afterCredentials.ETag, "preserved destination retains the stored credential")
	current = read()
	controls := map[string]any{"etag": current.ETag, "updates": []map[string]any{{"key": "people.enrichment.enabled", "value": map[string]any{"boolean": false}}, {"key": "people.enrichment.schedule", "value": map[string]any{"string": "0 */3 * * *"}}, {"key": "people.enrichment.batch_size", "value": map[string]any{"integer": 3}}}}
	result, err := backend.ExecuteOperation(t.Context(), "update_enrichment_controls", controls)
	requirements.NoError(err)
	requirements.False(result.IsError)
	loaded, err = config.Load(cfg.ConfigFilePath(), cfg.HomeDir)
	requirements.NoError(err)
	assertions.False(loaded.People.Enrichment.Enabled)
	assertions.Equal("0 */3 * * *", loaded.People.Enrichment.Schedule)
	assertions.Equal(3, loaded.People.Enrichment.BatchSize)
	current = read()
	requirements.Len(current.Controls, len(mcpserver.EnrichmentControlKeys()))
	for _, setting := range current.Controls {
		requirements.NotNil(setting.Value, "effective global control remains readable: %s", setting.Key)
	}
	before, err := os.ReadFile(cfg.ConfigFilePath())
	requirements.NoError(err)
	for _, changes := range []map[string]any{{"endpoint": "https://different.example.test/search"}, {"poll_endpoint": "https://different.example.test"}, {"api_key_env": "UNTRUSTED"}, {"kind": "sixtyfour"}, {"enabled": nil}} {
		refused, err := backend.ExecuteOperation(t.Context(), "update_enrichment_policy", map[string]any{"etag": current.ETag, "name": original.Name, "changes": changes})
		requirements.NoError(err)
		assertions.True(refused.IsError)
	}
	refused, err := backend.ExecuteOperation(t.Context(), "update_enrichment_policy", map[string]any{"etag": current.ETag, "name": "unknown-provider", "changes": map[string]any{"enabled": false}})
	requirements.NoError(err)
	assertions.True(refused.IsError)
	refused, err = backend.ExecuteOperation(t.Context(), "update_enrichment_policy", args)
	requirements.NoError(err)
	assertions.True(refused.IsError, "stale ETag cannot trigger replacement")
	after, err := os.ReadFile(cfg.ConfigFilePath())
	requirements.NoError(err)
	assertions.Equal(string(before), string(after))
}

func TestMCPEnrichmentCannotReplaceAnUndisclosedHostDestination(t *testing.T) {
	requirements, assertions := require.New(t), assert.New(t)
	cfg := config.NewDefaultConfig()
	cfg.HomeDir = t.TempDir()
	cfg.Data.DataDir = cfg.HomeDir
	cfg.People.Enrichment = personEnrichmentCLIConfig("MCP_SYNTHETIC_SUPPRESSION_KEY")
	cfg.People.Enrichment.Enabled = false
	cfg.People.Enrichment.Providers[0].Enabled = false
	cfg.People.Enrichment.Providers[0].PollEndpoint = "opaque-host-owned-destination"
	requirements.NoError(cfg.Save())
	backend := sourceOperationFixture(t, api.NewServerWithOptions(api.ServerOptions{Config: cfg, Logger: slog.New(slog.DiscardHandler)}))
	result, err := backend.ExecuteOperation(t.Context(), "get_enrichment_policy", nil)
	requirements.NoError(err)
	current := operationOutput[mcpserver.EnrichmentPolicy](t, result)
	requirements.Len(current.Providers, 1)
	assertions.Nil(current.Providers[0].PollEndpoint, "owner omits an invalid host destination")
	before, err := os.ReadFile(cfg.ConfigFilePath())
	requirements.NoError(err)
	args := map[string]any{"etag": current.ETag, "name": current.Providers[0].Name, "changes": map[string]any{"enabled": false}}
	_, err = backend.OperationDisclosure(t.Context(), "update_enrichment_policy", args)
	requirements.Error(err, "cannot disclose or preserve the hidden destination")
	result, err = backend.ExecuteOperation(t.Context(), "update_enrichment_policy", args)
	requirements.NoError(err)
	assertions.True(result.IsError)
	after, err := os.ReadFile(cfg.ConfigFilePath())
	requirements.NoError(err)
	assertions.Equal(string(before), string(after))
}

func settingsMCPDiagnostic(result *sdkmcp.CallToolResult) string {
	data, _ := json.Marshal(result.StructuredContent)
	if len(data) > 1024 {
		data = data[:1024]
	}
	var text strings.Builder
	text.Write(data)
	for _, content := range result.Content {
		if value, ok := content.(*sdkmcp.TextContent); ok {
			fragment := value.Text
			if len(fragment) > 1024 {
				fragment = fragment[:1024]
			}
			text.WriteByte(' ')
			text.WriteString(fragment)
		}
	}
	return text.String()
}
