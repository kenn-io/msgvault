package cmd

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/apiprotocol"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/deletion"
	mcpserver "go.kenn.io/msgvault/internal/mcp"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestMCPDeletionReviewJourney(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	cfg := &config.Config{Data: config.DataConfig{DataDir: t.TempDir()}}
	store := testutil.NewSQLiteTestStore(t)
	backend := sourceOperationFixture(t, api.NewServerWithOptions(api.ServerOptions{Config: cfg, Store: &storeAPIAdapter{store: store, config: cfg}, Logger: slog.New(slog.DiscardHandler)}))
	manager, err := deletion.NewManager(filepath.Join(cfg.Data.DataDir, "deletions"))
	require.NoError(err)
	pending := deletion.NewManifest("pending", []string{"synthetic-1"})
	require.NoError(manager.SaveManifest(pending))
	id := pending.ID
	opts := daemonMCPServeOptions(t.Context(), backend.client, nil)
	handler, closeHandler := mcpserver.NewHTTPHandler(opts, mcpserver.HTTPOptions{AllowWrites: true})
	t.Cleanup(closeHandler)
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	client := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "deletion-journey", Version: "1"}, nil)
	session, err := client.Connect(t.Context(), &sdkmcp.StreamableClientTransport{Endpoint: server.URL}, nil)
	require.NoError(err)
	t.Cleanup(func() { _ = session.Close() })
	call := func(name string, args map[string]any) map[string]any {
		t.Helper()
		result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: name, Arguments: args})
		require.NoError(err)
		require.False(result.IsError, "%s: %+v", name, result)
		data, err := json.Marshal(result.StructuredContent)
		require.NoError(err)
		var output map[string]any
		require.NoError(json.Unmarshal(data, &output))
		return output
	}
	list := call("list_deletions", map[string]any{"status": "pending"})
	rows, ok := list["manifests"].([]any)
	require.True(ok)
	require.Len(rows, 1)
	row, ok := rows[0].(map[string]any)
	require.True(ok)
	assert.Equal(id, row["id"])
	receipt := call("get_deletion", map[string]any{"batch_id": id})
	assert.Equal(id, receipt["id"])
	assert.Equal("pending", receipt["status"])
	claimed, err := manager.ClaimManifest(id, deletion.MethodTrash)
	require.NoError(err)
	receipt = call("get_deletion", map[string]any{"batch_id": id})
	assert.Equal("in_progress", receipt["status"])
	claimed.Execution.Failed = 1
	claimed.Execution.FailedIDs = []string{"synthetic-1"}
	now := time.Now()
	claimed.Execution.CompletedAt = &now
	require.NoError(manager.WriteInProgressManifest(claimed, id))
	require.NoError(manager.FinalizeInProgress(id, deletion.StatusFailed))
	receipt = call("get_deletion", map[string]any{"batch_id": id})
	assert.Equal("failed", receipt["status"])
	execution, ok := receipt["execution"].(map[string]any)
	require.True(ok)
	assert.Equal([]any{"synthetic-1"}, execution["failed_ids"])
	list = call("list_deletions", map[string]any{"status": "failed"})
	rows, ok = list["manifests"].([]any)
	require.True(ok)
	require.Len(rows, 1)
	row, ok = rows[0].(map[string]any)
	require.True(ok)
	assert.Equal(id, row["id"])
}

func TestMCPDeletionReviewRefusals(t *testing.T) {
	require := require.New(t)
	cfg := &config.Config{Data: config.DataConfig{DataDir: t.TempDir()}}
	store := testutil.NewSQLiteTestStore(t)
	backend := sourceOperationFixture(t, api.NewServerWithOptions(api.ServerOptions{Config: cfg, Store: &storeAPIAdapter{store: store, config: cfg}, Logger: slog.New(slog.DiscardHandler)}))
	result, err := backend.ExecuteOperation(t.Context(), "list_deletions", map[string]any{"status": "bogus"})
	require.NoError(err)
	assertLoopRefusal(t, result, "invalid_status", false)
	unavailable := sourceOperationFixture(t, api.NewServerWithOptions(api.ServerOptions{Config: cfg, Logger: slog.New(slog.DiscardHandler)}))
	result, err = unavailable.ExecuteOperation(t.Context(), "list_deletions", nil)
	require.NoError(err)
	assertLoopRefusal(t, result, "cli_store_unavailable", false)
}

func TestMCPDeletionReviewStatusDiscovery(t *testing.T) {
	assert := assert.New(t)
	routes := []apiprotocol.MCPRouteDescriptor{
		{OperationID: "listDeletions", Method: http.MethodGet, Path: "/api/v1/deletions", QueryParameters: []string{"status"}},
	}
	assert.Contains(newDaemonMCPOperations(nil, &apiprotocol.MCPCapabilities{Routes: routes}).capabilities(), "list_deletions")
	routes[0].QueryParameters = nil
	assert.NotContains(newDaemonMCPOperations(nil, &apiprotocol.MCPCapabilities{Routes: routes}).capabilities(), "list_deletions")
}

func TestMCPDeletionReviewUnsafeIDs(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)
	client, err := daemonclient.New(daemonclient.Config{URL: server.URL, AllowInsecure: true})
	require.NoError(err)
	backend := &daemonMCPOperations{client: client, supported: []string{"get_deletion"}}
	for _, id := range []string{"", ".", "..", "../x", "a/b", `a\b`, "a?b", "a#b"} {
		result, err := backend.ExecuteOperation(t.Context(), "get_deletion", map[string]any{"batch_id": id})
		require.NoError(err)
		assertLoopRefusal(t, result, "invalid_manifest_id", false)
	}
	assert.Zero(calls.Load())
}
