package cmd

import (
	"context"
	"encoding/json/v2"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/apiprotocol"
	"go.kenn.io/msgvault/internal/config"
	mcpserver "go.kenn.io/msgvault/internal/mcp"
	"go.kenn.io/msgvault/internal/providercredentials"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestMCPOperationalSettingsRealRouteAndApproval(t *testing.T) {
	require, assert := require.New(t), assert.New(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	require.NoError(os.WriteFile(path, []byte("[analytics]\nauto_build_cache = true\nbuilder_memory_limit = '512MiB'\nbuilder_threads = 3\n[vector.embeddings]\nendpoint = 'https://secret.example.com'\n"), 0600))
	cfg, err := config.Load(path, dir)
	require.NoError(err)
	cfg.Data.DataDir = t.TempDir()
	credentials, err := providercredentials.Read(cfg.TokensDir())
	require.NoError(err)
	_, err = providercredentials.Put(cfg.TokensDir(), credentials.ETag, providercredentials.VectorEmbeddingsID, "https://old.example.com", "synthetic-secret")
	require.NoError(err)
	st := testutil.NewSQLiteTestStore(t)
	backend := sourceOperationFixture(t, api.NewServerWithOptions(api.ServerOptions{Config: cfg, Store: &storeAPIAdapter{store: st}, Logger: slog.New(slog.DiscardHandler)}))
	read, err := backend.ExecuteOperation(t.Context(), "get_operational_settings", nil)
	require.NoError(err)
	policy := operationOutput[mcpserver.OperationalSettings](t, read)
	require.NotEmpty(policy.ETag)
	encoded, err := json.Marshal(policy)
	require.NoError(err)
	assert.NotContains(string(encoded), "secret.example.com")
	denied := operationMCPSession(t, backend, []mcpserver.OperationFamily{mcpserver.OperationFamilySettings}, &sdkmcp.ClientOptions{ElicitationHandler: func(context.Context, *sdkmcp.ElicitRequest) (*sdkmcp.ElicitResult, error) {
		return &sdkmcp.ElicitResult{Action: "decline"}, nil
	}})
	args := map[string]any{"etag": policy.ETag, "updates": []any{map[string]any{"key": "analytics.auto_build_cache", "value": map[string]any{"boolean": false}}, map[string]any{"key": "analytics.builder_memory_limit", "value": map[string]any{"string": ""}}}}
	result, err := denied.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "update_operational_settings", Arguments: args})
	require.NoError(err)
	assert.True(result.IsError)
	loaded, err := config.Load(path, dir)
	require.NoError(err)
	assert.True(loaded.Analytics.AutoBuildCache)
	approved := operationMCPSession(t, backend, []mcpserver.OperationFamily{mcpserver.OperationFamilySettings}, &sdkmcp.ClientOptions{ElicitationHandler: func(_ context.Context, request *sdkmcp.ElicitRequest) (*sdkmcp.ElicitResult, error) {
		assert.Contains(request.Params.Message, "delete stored provider credentials")
		assert.Contains(request.Params.Message, "analytics.auto_build_cache")
		assert.NotContains(request.Params.Message, "analytics.builder_threads")
		assert.NotContains(request.Params.Message, "synthetic-secret")
		return &sdkmcp.ElicitResult{Action: "accept", Content: map[string]any{"confirm": true}}, nil
	}})
	result, err = approved.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "update_operational_settings", Arguments: args})
	require.NoError(err)
	require.False(result.IsError)
	encoded, err = json.Marshal(result.StructuredContent)
	require.NoError(err)
	var updated mcpserver.OperationalSettings
	require.NoError(json.Unmarshal(encoded, &updated))
	assert.True(updated.PendingRestart)
	assert.NotEqual(policy.ETag, updated.ETag)
	loaded, err = config.Load(path, dir)
	require.NoError(err)
	assert.False(loaded.Analytics.AutoBuildCache)
	assert.Empty(loaded.Analytics.BuilderMemoryLimit)
	assert.Equal(3, loaded.Analytics.BuilderThreads)
	credentials, err = providercredentials.Read(cfg.TokensDir())
	require.NoError(err)
	assert.False(credentials.Stored(providercredentials.VectorEmbeddingsID))
	stale, err := backend.ExecuteOperation(t.Context(), "update_operational_settings", args)
	require.NoError(err)
	assert.True(stale.IsError)
	encoded, err = json.Marshal(stale.Output)
	require.NoError(err)
	assert.Contains(string(encoded), "settings_conflict")
	// The SDK validates the original request before rounding can reach approval.
	for _, value := range []int64{9007199254740992, 9007199254740993, -9007199254740992} {
		result, err = approved.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "update_operational_settings", Arguments: map[string]any{
			"etag": updated.ETag, "updates": []any{map[string]any{"key": "analytics.builder_threads", "value": map[string]any{"integer": value}}},
		}})
		require.NoError(err)
		assert.True(result.IsError)
	}
	for _, value := range []map[string]any{{"boolean": false, "string": ""}, {"integer": "3"}, {}} {
		result, err = approved.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "update_operational_settings", Arguments: map[string]any{
			"etag": updated.ETag, "updates": []any{map[string]any{"key": "analytics.builder_threads", "value": value}},
		}})
		require.NoError(err)
		assert.True(result.IsError)
	}
	loaded, err = config.Load(path, dir)
	require.NoError(err)
	assert.Equal(3, loaded.Analytics.BuilderThreads)
	result, err = approved.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "update_operational_settings", Arguments: map[string]any{
		"etag": "stale", "updates": []any{map[string]any{"key": "analytics.builder_threads", "value": map[string]any{"integer": int64(9007199254740991)}}},
	}})
	require.NoError(err)
	require.True(result.IsError)
	encoded, err = json.Marshal(result.StructuredContent)
	require.NoError(err)
	assert.Contains(string(encoded), "settings_conflict")
}

func TestOperationalSettingsCapabilitiesRequireExactRoutes(t *testing.T) {
	get := apiprotocol.MCPRouteDescriptor{OperationID: "getSettings", Method: "GET", Path: "/api/v1/settings"}
	patch := apiprotocol.MCPRouteDescriptor{OperationID: "patchSettings", Method: "PATCH", Path: "/api/v1/settings", RequestProperties: []string{"updates"}}
	for _, tc := range []struct {
		name        string
		routes      []apiprotocol.MCPRouteDescriptor
		read, write bool
	}{
		{"absent", nil, false, false},
		{"get only", []apiprotocol.MCPRouteDescriptor{get}, true, false},
		{"wrong get id", []apiprotocol.MCPRouteDescriptor{{OperationID: "other", Method: "GET", Path: get.Path}, patch}, false, false},
		{"wrong get method", []apiprotocol.MCPRouteDescriptor{{OperationID: "getSettings", Method: "POST", Path: get.Path}, patch}, false, false},
		{"wrong get path", []apiprotocol.MCPRouteDescriptor{{OperationID: "getSettings", Method: "GET", Path: "/wrong"}, patch}, false, false},
		{"wrong patch method", []apiprotocol.MCPRouteDescriptor{get, {OperationID: "patchSettings", Method: "POST", Path: patch.Path, RequestProperties: patch.RequestProperties}}, true, false},
		{"wrong patch id", []apiprotocol.MCPRouteDescriptor{get, {OperationID: "other", Method: "PATCH", Path: patch.Path, RequestProperties: patch.RequestProperties}}, true, false},
		{"wrong patch path", []apiprotocol.MCPRouteDescriptor{get, {OperationID: "patchSettings", Method: "PATCH", Path: "/wrong", RequestProperties: patch.RequestProperties}}, true, false},
		{"missing updates", []apiprotocol.MCPRouteDescriptor{get, {OperationID: "patchSettings", Method: "PATCH", Path: patch.Path}}, true, false},
		{"complete", []apiprotocol.MCPRouteDescriptor{get, patch}, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			names := newDaemonMCPOperations(nil, &apiprotocol.MCPCapabilities{Routes: tc.routes}).capabilities()
			assert.Equal(tc.read, slices.Contains(names, "get_operational_settings"))
			assert.Equal(tc.write, slices.Contains(names, "update_operational_settings"))
		})
	}
}

func TestOperationalSettingsRejectInvalidValues(t *testing.T) {
	require, assert := require.New(t), assert.New(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	require.NoError(os.WriteFile(path, []byte("[analytics]\nauto_build_cache = true\n"), 0600))
	cfg, err := config.Load(path, dir)
	require.NoError(err)
	cfg.Data.DataDir = t.TempDir()
	backend := sourceOperationFixture(t, api.NewServer(cfg, nil, nil, slog.New(slog.DiscardHandler)))
	read, err := backend.ExecuteOperation(t.Context(), "get_operational_settings", nil)
	require.NoError(err)
	policy := operationOutput[mcpserver.OperationalSettings](t, read)
	before, err := os.ReadFile(path)
	require.NoError(err)
	for _, tc := range []struct {
		updates   []any
		code      string
		uncertain bool
	}{
		{[]any{map[string]any{"key": "vector.search.sqlite_accelerator", "value": map[string]any{"string": "invalid"}}}, "validation_failed", true},
		{[]any{map[string]any{"key": "analytics.auto_build_cache", "value": map[string]any{"boolean": false}, "secret": map[string]any{"action": "clear"}}}, "invalid_arguments", false},
		{[]any{map[string]any{"key": "vector.embeddings.endpoint", "value": map[string]any{"string": "https://other.example.com"}}}, "invalid_arguments", false},
	} {
		result, err := backend.ExecuteOperation(t.Context(), "update_operational_settings", map[string]any{"etag": policy.ETag, "updates": tc.updates})
		require.NoError(err)
		require.True(result.IsError)
		encoded, err := json.Marshal(result.Output)
		require.NoError(err)
		var failure struct {
			Error     string `json:"error"`
			Uncertain bool   `json:"operation_may_have_completed"`
		}
		require.NoError(json.Unmarshal(encoded, &failure))
		assert.Equal(tc.code, failure.Error)
		assert.Equal(tc.uncertain, failure.Uncertain)
	}
	after, err := os.ReadFile(path)
	require.NoError(err)
	assert.Equal(before, after)
	loaded, err := config.Load(path, dir)
	require.NoError(err)
	assert.True(loaded.Analytics.AutoBuildCache)
}
