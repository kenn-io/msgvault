package cmd

import (
	"log/slog"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/apiprotocol"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func TestMCPOperationReadsDiscoverNativeRoutes(t *testing.T) {
	cfg := &config.Config{}
	cfg.Data.DataDir = t.TempDir()
	st := testutil.NewSQLiteTestStore(t)
	source, err := st.GetOrCreateSource("gmail", "reader@example.com")
	require.NoError(t, err)
	started := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	for i := range 2 {
		_, err = st.DB().ExecContext(t.Context(), `INSERT INTO sync_runs (source_id, started_at, completed_at, status, messages_processed, messages_added, messages_updated, errors_count) VALUES (?, ?, ?, 'completed', 3, 1, 0, 0)`, source.ID, started.Add(time.Duration(i)*time.Minute).Format("2006-01-02 15:04:05"), started.Add(time.Duration(i)*time.Minute+time.Second).Format("2006-01-02 15:04:05"))
		require.NoError(t, err)
	}
	backend := sourceOperationFixture(t, api.NewServerWithOptions(api.ServerOptions{Config: cfg, Store: st, OperationHistoryReader: st, Logger: slog.New(slog.DiscardHandler)}))
	assert.Subset(t, backend.capabilities(), []string{"get_operation_status", "list_operation_runs", "get_operation_run"})
	result, err := backend.ExecuteOperation(t.Context(), "get_operation_status", nil)
	require.NoError(t, err)
	status := operationOutput[generated.OperationStatusResponse](t, result)
	assert.NotEmpty(t, status.Lanes)
	result, err = backend.ExecuteOperation(t.Context(), "list_operation_runs", map[string]any{"kind": "source_sync", "limit": 1})
	require.NoError(t, err)
	page := operationOutput[generated.OperationRunsResponse](t, result)
	require.Len(t, page.Runs, 1)
	require.NotNil(t, page.NextCursor)
	result, err = backend.ExecuteOperation(t.Context(), "get_operation_run", map[string]any{"id": page.Runs[0].ID})
	require.NoError(t, err)
	assert.Equal(t, page.Runs[0].ID, operationOutput[generated.OperationRunDetail](t, result).ID)
	result, err = backend.ExecuteOperation(t.Context(), "list_operation_runs", map[string]any{"kind": "source_sync", "limit": 1, "cursor": *page.NextCursor})
	require.NoError(t, err)
	next := operationOutput[generated.OperationRunsResponse](t, result)
	require.Len(t, next.Runs, 1)
	assert.NotEqual(t, page.Runs[0].ID, next.Runs[0].ID)
	result, err = backend.ExecuteOperation(t.Context(), "list_operation_runs", map[string]any{"cursor": "bad"})
	require.NoError(t, err)
	require.True(t, result.IsError)
	assert.Equal(t, operationFailure("invalid_cursor", false).Output, result.Output)
	result, err = backend.ExecuteOperation(t.Context(), "get_operation_run", map[string]any{"id": "../status"})
	require.NoError(t, err)
	require.True(t, result.IsError)
	assert.Equal(t, operationFailure("invalid_arguments", false).Output, result.Output)
}

func TestMCPOperationStatusSDKStructuredResult(t *testing.T) {
	cfg := &config.Config{}
	cfg.Data.DataDir = t.TempDir()
	st := testutil.NewSQLiteTestStore(t)
	backend := sourceOperationFixture(t, api.NewServerWithOptions(api.ServerOptions{Config: cfg, Store: st, OperationHistoryReader: st, Logger: slog.New(slog.DiscardHandler)}))
	session := operationMCPSession(t, backend, nil, nil)
	result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "get_operation_status", Arguments: map[string]any{}})
	require.NoError(t, err)
	require.False(t, result.IsError)
	structured, ok := result.StructuredContent.(map[string]any)
	require.True(t, ok)
	assert.Contains(t, structured, "lanes")
}

func TestMCPOperationReadsRequireExactCapabilities(t *testing.T) {
	capabilities := &apiprotocol.MCPCapabilities{Routes: []apiprotocol.MCPRouteDescriptor{
		{OperationID: "getOperationStatus", Method: "GET", Path: "/api/v1/operations/status"},
		{OperationID: "listOperationRuns", Method: "GET", Path: "/api/v1/operations/runs", QueryParameters: []string{"kind", "lane", "state", "started_from", "started_before", "limit"}},
		{OperationID: "getOperationRun", Method: "GET", Path: "/api/v1/operations/runs/{id}"},
	}}
	backend := newDaemonMCPOperations(nil, capabilities)
	assert.Contains(t, backend.capabilities(), "get_operation_status")
	assert.Contains(t, backend.capabilities(), "get_operation_run")
	assert.NotContains(t, backend.capabilities(), "list_operation_runs")
	result, err := backend.ExecuteOperation(t.Context(), "list_operation_runs", nil)
	require.NoError(t, err)
	assert.Equal(t, operationFailure("operation_not_supported", false).Output, result.Output)
}
