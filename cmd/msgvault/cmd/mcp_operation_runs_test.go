package cmd

import (
	"encoding/json/v2"
	"log/slog"
	"strings"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/apiprotocol"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func operationRunFixture(t *testing.T) (*daemonMCPOperations, *store.Store, time.Time) {
	t.Helper()
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
	return sourceOperationFixture(t, api.NewServerWithOptions(api.ServerOptions{Config: cfg, Store: st, OperationHistoryReader: st, Logger: slog.New(slog.DiscardHandler)})), st, started
}

func TestMCPOperationReadsDiscoverNativeRoutes(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	backend, st, started := operationRunFixture(t)
	assert.Subset(backend.capabilities(), []string{"get_operation_status", "list_operation_runs", "get_operation_run"})
	result, err := backend.ExecuteOperation(t.Context(), "get_operation_status", nil)
	require.NoError(err)
	status := operationOutput[generated.OperationStatusResponse](t, result)
	assert.NotEmpty(status.Lanes)
	result, err = backend.ExecuteOperation(t.Context(), "list_operation_runs", map[string]any{"kind": "source_sync", "limit": 1})
	require.NoError(err)
	page := operationOutput[generated.OperationRunsResponse](t, result)
	require.Len(page.Runs, 1)
	require.NotNil(page.NextCursor)
	result, err = backend.ExecuteOperation(t.Context(), "get_operation_run", map[string]any{"id": page.Runs[0].ID})
	require.NoError(err)
	assert.Equal(page.Runs[0].ID, operationOutput[generated.OperationRunDetail](t, result).ID)
	result, err = backend.ExecuteOperation(t.Context(), "list_operation_runs", map[string]any{"kind": "source_sync", "limit": 1, "cursor": *page.NextCursor})
	require.NoError(err)
	next := operationOutput[generated.OperationRunsResponse](t, result)
	require.Len(next.Runs, 1)
	assert.Equal(started.Add(time.Minute), page.Runs[0].StartedAt)
	assert.Equal(started, next.Runs[0].StartedAt)
	result, err = backend.ExecuteOperation(t.Context(), "list_operation_runs", map[string]any{"cursor": "bad"})
	require.NoError(err)
	require.True(result.IsError)
	assert.Equal(operationFailure("invalid_cursor", false).Output, result.Output)
	for _, id := range []string{"../status", ".", ".."} {
		result, err = backend.ExecuteOperation(t.Context(), "get_operation_run", map[string]any{"id": id})
		require.NoError(err)
		require.True(result.IsError)
		assert.Equal(operationFailure("invalid_arguments", false).Output, result.Output)
	}
	result, err = backend.ExecuteOperation(t.Context(), "get_operation_run", map[string]any{"id": "op2." + strings.Repeat("1", 32) + ".bad"})
	require.NoError(err)
	require.True(result.IsError)
	assert.Equal(operationFailure("invalid_operation_run_id", false).Output, result.Output)
	for _, field := range []string{"started_from", "started_before"} {
		for _, value := range []string{"2026-01-02T03:04:05.000Z", "2026-01-02T03:04:05+00:00"} {
			result, err = backend.ExecuteOperation(t.Context(), "list_operation_runs", map[string]any{field: value})
			require.NoError(err)
			require.True(result.IsError)
			assert.Equal(operationFailure("invalid_"+field, false).Output, result.Output)
		}
	}
	result, err = backend.ExecuteOperation(t.Context(), "list_operation_runs", map[string]any{"kind": "source_sync", "started_from": "2026-01-02T03:05:05Z"})
	require.NoError(err)
	filtered := operationOutput[generated.OperationRunsResponse](t, result)
	require.Len(filtered.Runs, 1)
	assert.Equal(started.Add(time.Minute), filtered.Runs[0].StartedAt)
	result, err = backend.ExecuteOperation(t.Context(), "list_operation_runs", map[string]any{"kind": "person_sweep", "cursor": *page.NextCursor})
	require.NoError(err)
	require.True(result.IsError)
	assert.Equal(operationFailure("invalid_cursor", false).Output, result.Output)
	_, err = st.DB().ExecContext(t.Context(), `DELETE FROM sync_runs`)
	require.NoError(err)
	result, err = backend.ExecuteOperation(t.Context(), "get_operation_run", map[string]any{"id": page.Runs[0].ID})
	require.NoError(err)
	require.True(result.IsError)
	assert.Equal(operationFailure("operation_run_not_found", false).Output, result.Output)
}

func TestMCPOperationStatusSDKStructuredResult(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	backend, _, _ := operationRunFixture(t)
	session := operationMCPSession(t, backend, nil, nil)
	result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "get_operation_status", Arguments: map[string]any{}})
	require.NoError(err)
	require.False(result.IsError)
	structured, ok := result.StructuredContent.(map[string]any)
	require.True(ok)
	assert.Contains(structured, "lanes")
	result, err = session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "list_operation_runs", Arguments: map[string]any{"kind": "source_sync", "limit": 1}})
	require.NoError(err)
	require.False(result.IsError)
	data, err := json.Marshal(result.StructuredContent)
	require.NoError(err)
	var page generated.OperationRunsResponse
	require.NoError(json.Unmarshal(data, &page))
	require.Len(page.Runs, 1)
	require.NotNil(page.NextCursor)
	result, err = session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "get_operation_run", Arguments: map[string]any{"id": page.Runs[0].ID}})
	require.NoError(err)
	require.False(result.IsError)
	data, err = json.Marshal(result.StructuredContent)
	require.NoError(err)
	var detail generated.OperationRunDetail
	require.NoError(json.Unmarshal(data, &detail))
	assert.Equal(page.Runs[0].ID, detail.ID)
	result, err = session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "list_operation_runs", Arguments: map[string]any{"kind": "source_sync", "limit": 1, "cursor": *page.NextCursor}})
	require.NoError(err)
	require.False(result.IsError)
	data, err = json.Marshal(result.StructuredContent)
	require.NoError(err)
	var next generated.OperationRunsResponse
	require.NoError(json.Unmarshal(data, &next))
	require.Len(next.Runs, 1)
	assert.NotEqual(page.Runs[0].StartedAt, next.Runs[0].StartedAt)
	result, err = session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "list_operation_runs", Arguments: map[string]any{"limit": 101}})
	require.NoError(err)
	require.True(result.IsError)
	limitData, err := json.Marshal(result.StructuredContent)
	require.NoError(err)
	assert.JSONEq(`{"error":"invalid_limit","operation_may_have_completed":false}`, string(limitData))
	result, err = session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "list_operation_runs", Arguments: map[string]any{"started_from": "2026-01-02T03:04:05.000Z"}})
	require.NoError(err)
	require.True(result.IsError)
	data, err = json.Marshal(result.StructuredContent)
	require.NoError(err)
	assert.JSONEq(`{"error":"invalid_started_from","operation_may_have_completed":false}`, string(data))
}

func TestMCPOperationReadsRequireExactCapabilities(t *testing.T) {
	for _, omitted := range []string{"getOperationStatus", "getOperationRun"} {
		t.Run("missing "+omitted, func(t *testing.T) {
			assert := assert.New(t)
			routes := []apiprotocol.MCPRouteDescriptor{
				{OperationID: "getOperationStatus", Method: "GET", Path: "/api/v1/operations/status"},
				{OperationID: "listOperationRuns", Method: "GET", Path: "/api/v1/operations/runs", QueryParameters: []string{"kind", "lane", "state", "started_from", "started_before", "limit", "cursor"}},
				{OperationID: "getOperationRun", Method: "GET", Path: "/api/v1/operations/runs/{id}"},
			}
			filtered := make([]apiprotocol.MCPRouteDescriptor, 0, 2)
			for _, route := range routes {
				if route.OperationID != omitted {
					filtered = append(filtered, route)
				}
			}
			names := newDaemonMCPOperations(nil, &apiprotocol.MCPCapabilities{Routes: filtered}).capabilities()
			assert.Contains(names, "list_operation_runs")
			if omitted == "getOperationStatus" {
				assert.NotContains(names, "get_operation_status")
				assert.Contains(names, "get_operation_run")
			} else {
				assert.NotContains(names, "get_operation_run")
				assert.Contains(names, "get_operation_status")
			}
		})
	}
	for _, tc := range []struct {
		name, id, method, path string
		query                  []string
	}{
		{"missing query", "listOperationRuns", "GET", "/api/v1/operations/runs", []string{"kind", "lane", "state", "started_from", "started_before", "limit"}},
		{"wrong method", "listOperationRuns", "POST", "/api/v1/operations/runs", []string{"kind", "lane", "state", "started_from", "started_before", "limit", "cursor"}},
		{"wrong path", "listOperationRuns", "GET", "/api/v1/operations/other", []string{"kind", "lane", "state", "started_from", "started_before", "limit", "cursor"}},
		{"wrong id", "other", "GET", "/api/v1/operations/runs", []string{"kind", "lane", "state", "started_from", "started_before", "limit", "cursor"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			capabilities := &apiprotocol.MCPCapabilities{Routes: []apiprotocol.MCPRouteDescriptor{
				{OperationID: "getOperationStatus", Method: "GET", Path: "/api/v1/operations/status"},
				{OperationID: tc.id, Method: tc.method, Path: tc.path, QueryParameters: tc.query},
				{OperationID: "getOperationRun", Method: "GET", Path: "/api/v1/operations/runs/{id}"},
			}}
			names := newDaemonMCPOperations(nil, capabilities).capabilities()
			assert.Contains(t, names, "get_operation_status")
			assert.Contains(t, names, "get_operation_run")
			assert.NotContains(t, names, "list_operation_runs")
		})
	}
}
