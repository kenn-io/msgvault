package cmd

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/apiprotocol"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/deletion"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func TestMCPDeletionReviewRealRouter(t *testing.T) {
	cfg := &config.Config{Data: config.DataConfig{DataDir: t.TempDir()}}
	store := testutil.NewSQLiteTestStore(t)
	backend := sourceOperationFixture(t, api.NewServerWithOptions(api.ServerOptions{Config: cfg, Store: &storeAPIAdapter{store: store, config: cfg}, Logger: slog.New(slog.DiscardHandler)}))
	manager, err := deletion.NewManager(filepath.Join(cfg.Data.DataDir, "deletions"))
	require.NoError(t, err)
	pending := deletion.NewManifest("pending", []string{"synthetic-1"})
	pending.ID = "pending-batch"
	require.NoError(t, manager.SaveManifest(pending))
	failed := deletion.NewManifest("failed", []string{"synthetic-2"})
	failed.ID = "failed-batch"
	failed.Status = deletion.StatusFailed
	require.NoError(t, manager.SaveManifest(failed))
	result, err := backend.ExecuteOperation(t.Context(), "list_deletions", map[string]any{"status": "pending"})
	require.NoError(t, err)
	list := operationOutput[generated.ListDeletionsResponse](t, result)
	require.Len(t, list.Manifests, 1)
	assert.Equal(t, "pending-batch", list.Manifests[0].ID)
	result, err = backend.ExecuteOperation(t.Context(), "get_deletion", map[string]any{"batch_id": "failed-batch"})
	require.NoError(t, err)
	detail := operationOutput[generated.DeletionManifestDetail](t, result)
	assert.Equal(t, "failed-batch", detail.ID)
	result, err = backend.ExecuteOperation(t.Context(), "list_deletions", map[string]any{"status": "bogus"})
	require.NoError(t, err)
	assertLoopRefusal(t, result, "invalid_status", false)
	unavailable := sourceOperationFixture(t, api.NewServerWithOptions(api.ServerOptions{Config: cfg, Logger: slog.New(slog.DiscardHandler)}))
	result, err = unavailable.ExecuteOperation(t.Context(), "list_deletions", nil)
	require.NoError(t, err)
	assertLoopRefusal(t, result, "cli_store_unavailable", false)
}

func TestMCPDeletionReviewDiscoveryAndUnsafeIDs(t *testing.T) {
	routes := []apiprotocol.MCPRouteDescriptor{
		{OperationID: "listDeletions", Method: http.MethodGet, Path: "/api/v1/deletions", QueryParameters: []string{"status"}},
		{OperationID: "getDeletion", Method: http.MethodGet, Path: "/api/v1/deletions/{id}"},
	}
	assert.Empty(t, newDaemonMCPOperations(nil, nil).capabilities())
	assert.ElementsMatch(t, []string{"list_deletions", "get_deletion"}, newDaemonMCPOperations(nil, &apiprotocol.MCPCapabilities{Routes: routes}).capabilities())
	routes[0].QueryParameters = nil
	routes[1].Method = http.MethodPost
	assert.NotContains(t, newDaemonMCPOperations(nil, &apiprotocol.MCPCapabilities{Routes: routes}).capabilities(), "list_deletions")
	assert.NotContains(t, newDaemonMCPOperations(nil, &apiprotocol.MCPCapabilities{Routes: routes}).capabilities(), "get_deletion")
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)
	client, err := daemonclient.New(daemonclient.Config{URL: server.URL, AllowInsecure: true})
	require.NoError(t, err)
	backend := &daemonMCPOperations{client: client, supported: []string{"get_deletion"}}
	for _, id := range []string{"", ".", "..", "../x", "a/b", `a\b`, "a?b", "a#b"} {
		result, err := backend.ExecuteOperation(t.Context(), "get_deletion", map[string]any{"batch_id": id})
		require.NoError(t, err)
		assertLoopRefusal(t, result, "invalid_manifest_id", false)
	}
	assert.Zero(t, calls.Load())
}
