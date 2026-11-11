package cmd

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

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
	call := func(name string, args map[string]any) *generated.ListDeletionsResponse {
		t.Helper()
		result, err := backend.ExecuteOperation(t.Context(), name, args)
		require.NoError(t, err)
		out := operationOutput[generated.ListDeletionsResponse](t, result)
		return &out
	}
	assert.Empty(t, call("list_deletions", nil).Manifests)
	manager, err := deletion.NewManager(filepath.Join(cfg.Data.DataDir, "deletions"))
	require.NoError(t, err)
	older := deletion.NewManifestForSource("older", []string{"synthetic-1"}, deletion.SourceReference{ID: 1, Type: "gmail", Identifier: "owner@example.test"})
	older.ID = "older"
	older.CreatedAt = time.Now().Add(-time.Hour)
	older.Filters.Account = "owner@example.test"
	require.NoError(t, manager.SaveManifest(older))
	newer := deletion.NewManifestForSource("newer", []string{"synthetic-2"}, deletion.SourceReference{ID: 1, Type: "gmail", Identifier: "owner@example.test"})
	newer.Summary = &deletion.Summary{MessageCount: 1}
	newer.ID = "newer"
	newer.Status = deletion.StatusFailed
	newer.Execution = &deletion.Execution{Failed: 1, FailedIDs: []string{"synthetic-2"}}
	require.NoError(t, manager.SaveManifest(newer))
	all := call("list_deletions", nil)
	require.Len(t, all.Manifests, 2)
	assert.Equal(t, "newer", all.Manifests[0].ID)
	assert.Equal(t, "older", all.Manifests[1].ID)
	pending := call("list_deletions", map[string]any{"status": "pending"})
	require.Len(t, pending.Manifests, 1)
	assert.Equal(t, "older", pending.Manifests[0].ID)
	result, err := backend.ExecuteOperation(t.Context(), "get_deletion", map[string]any{"manifest_id": "newer"})
	require.NoError(t, err)
	detail := operationOutput[generated.DeletionManifestDetail](t, result)
	assert.Equal(t, "failed", detail.Status)
	require.NotNil(t, detail.Summary)
	assert.Equal(t, int64(1), detail.Summary.MessageCount)
	require.NotNil(t, detail.Source)
	assert.Equal(t, "gmail", detail.Source.Type)
	require.NotNil(t, detail.Execution)
	assert.Equal(t, []string{"synthetic-2"}, detail.Execution.FailedIds)
	for _, tc := range []struct {
		name string
		args map[string]any
		code string
	}{
		{"get_deletion", map[string]any{"manifest_id": "missing"}, "not_found"},
		{"list_deletions", map[string]any{"status": "bogus"}, "invalid_status"},
		{"get_deletion", map[string]any{"manifest_id": "../unsafe"}, "invalid_manifest_id"},
	} {
		result, err := backend.ExecuteOperation(t.Context(), tc.name, tc.args)
		require.NoError(t, err)
		assertLoopRefusal(t, result, tc.code, false)
	}
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
		result, err := backend.ExecuteOperation(t.Context(), "get_deletion", map[string]any{"manifest_id": id})
		require.NoError(t, err)
		assertLoopRefusal(t, result, "invalid_manifest_id", false)
	}
	assert.Zero(t, calls.Load())
}

func TestMCPDeletionReviewRejectsBadResponses(t *testing.T) {
	for _, tc := range []struct {
		name, body, code string
		hasError         bool
	}{
		{"malformed", "{", "invalid_operation_response", true},
		{"oversized", strings.Repeat("x", (1<<20)+1), "operation_response_limit_exceeded", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, tc.body) }))
			t.Cleanup(server.Close)
			client, err := daemonclient.New(daemonclient.Config{URL: server.URL, AllowInsecure: true})
			require.NoError(t, err)
			backend := &daemonMCPOperations{client: client, supported: []string{"list_deletions"}}
			result, err := backend.ExecuteOperation(t.Context(), "list_deletions", nil)
			if tc.hasError {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			assertLoopRefusal(t, result, tc.code, false)
		})
	}
}
