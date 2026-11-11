package cmd

import (
	"context"
	"encoding/json/v2"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	mcpserver "go.kenn.io/msgvault/internal/mcp"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestMCPOperationalSettingsRealRouteAndApproval(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	require.NoError(t, os.WriteFile(path, []byte("[analytics]\nauto_build_cache = true\nbuilder_memory_limit = '512MiB'\nbuilder_threads = 3\n[vector.embeddings]\nendpoint = 'https://secret.example.com'\n"), 0600))
	cfg, err := config.Load(path, dir)
	require.NoError(t, err)
	cfg.Data.DataDir = t.TempDir()
	st := testutil.NewSQLiteTestStore(t)
	backend := sourceOperationFixture(t, api.NewServerWithOptions(api.ServerOptions{Config: cfg, Store: &storeAPIAdapter{store: st}, Logger: slog.New(slog.DiscardHandler)}))
	read, err := backend.ExecuteOperation(t.Context(), "get_operational_settings", nil)
	require.NoError(t, err)
	policy := operationOutput[mcpserver.OperationalSettings](t, read)
	require.NotEmpty(t, policy.ETag)
	encoded, err := json.Marshal(policy)
	require.NoError(t, err)
	assert.NotContains(t, string(encoded), "secret.example.com")
	assert.NotContains(t, string(encoded), "endpoint")
	denied := operationMCPSession(t, backend, []mcpserver.OperationFamily{mcpserver.OperationFamilySettings}, &sdkmcp.ClientOptions{ElicitationHandler: func(context.Context, *sdkmcp.ElicitRequest) (*sdkmcp.ElicitResult, error) {
		return &sdkmcp.ElicitResult{Action: "decline"}, nil
	}})
	args := map[string]any{"etag": policy.ETag, "updates": []any{map[string]any{"key": "analytics.auto_build_cache", "value": map[string]any{"boolean": false}}, map[string]any{"key": "analytics.builder_memory_limit", "value": map[string]any{"string": ""}}}}
	result, err := denied.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "update_operational_settings", Arguments: args})
	require.NoError(t, err)
	assert.True(t, result.IsError)
	loaded, err := config.Load(path, dir)
	require.NoError(t, err)
	assert.True(t, loaded.Analytics.AutoBuildCache)
	approved := operationMCPSession(t, backend, []mcpserver.OperationFamily{mcpserver.OperationFamilySettings}, &sdkmcp.ClientOptions{ElicitationHandler: func(context.Context, *sdkmcp.ElicitRequest) (*sdkmcp.ElicitResult, error) {
		return &sdkmcp.ElicitResult{Action: "accept", Content: map[string]any{"confirm": true}}, nil
	}})
	result, err = approved.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "update_operational_settings", Arguments: args})
	require.NoError(t, err)
	require.False(t, result.IsError)
	encoded, err = json.Marshal(result.StructuredContent)
	require.NoError(t, err)
	var updated mcpserver.OperationalSettings
	require.NoError(t, json.Unmarshal(encoded, &updated))
	assert.True(t, updated.PendingRestart)
	assert.NotEqual(t, policy.ETag, updated.ETag)
	loaded, err = config.Load(path, dir)
	require.NoError(t, err)
	assert.False(t, loaded.Analytics.AutoBuildCache)
	assert.Empty(t, loaded.Analytics.BuilderMemoryLimit)
	assert.Equal(t, 3, loaded.Analytics.BuilderThreads)
	stale, err := backend.ExecuteOperation(t.Context(), "update_operational_settings", args)
	require.NoError(t, err)
	assert.True(t, stale.IsError)
	encoded, err = json.Marshal(stale.Output)
	require.NoError(t, err)
	assert.Contains(t, string(encoded), "settings_conflict")
	args["updates"] = []any{map[string]any{"key": "vector.embeddings.endpoint", "value": map[string]any{"string": "https://other.example.com"}}}
	rejected, err := backend.ExecuteOperation(t.Context(), "update_operational_settings", args)
	require.NoError(t, err)
	assert.True(t, rejected.IsError)
}

func TestOperationalSettingsCapabilitiesRequireExactRoutes(t *testing.T) {
	assert.Empty(t, operationalSettingsCapabilities(func(string, string, string, ...string) bool { return false }, nil))
	backend := newDaemonMCPOperations(nil, nil)
	result, err := backend.ExecuteOperation(t.Context(), "get_operational_settings", nil)
	require.NoError(t, err)
	assert.True(t, result.IsError)
}

func TestOperationalSettingsRejectInvalidValues(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	require.NoError(t, os.WriteFile(path, []byte("[analytics]\nauto_build_cache = true\n"), 0600))
	cfg, err := config.Load(path, dir)
	require.NoError(t, err)
	cfg.Data.DataDir = t.TempDir()
	backend := sourceOperationFixture(t, api.NewServer(cfg, nil, nil, slog.New(slog.DiscardHandler)))
	read, err := backend.ExecuteOperation(t.Context(), "get_operational_settings", nil)
	require.NoError(t, err)
	policy := operationOutput[mcpserver.OperationalSettings](t, read)
	for _, updates := range []any{
		[]any{map[string]any{"key": "vector.search.sqlite_accelerator", "value": map[string]any{"string": "invalid"}}},
		[]any{map[string]any{"key": "analytics.auto_build_cache", "value": map[string]any{"boolean": false}, "secret": map[string]any{"action": "clear"}}},
		[]any{map[string]any{"key": "analytics.auto_build_cache", "value": map[string]any{"boolean": false, "string": ""}}},
	} {
		result, err := backend.ExecuteOperation(t.Context(), "update_operational_settings", map[string]any{"etag": policy.ETag, "updates": updates})
		require.NoError(t, err)
		assert.True(t, result.IsError)
	}
	loaded, err := config.Load(path, dir)
	require.NoError(t, err)
	assert.True(t, loaded.Analytics.AutoBuildCache)
}
