package cmd

import (
	"encoding/json/v2"
	"log/slog"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/operations"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func TestMCPOperationHistoryPreservesRealOpaquePagingAndFailures(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	st := testutil.NewSQLiteTestStore(t)
	source, err := st.GetOrCreateSource("gmail", "synthetic-history@example.com")
	requirements.NoError(err)
	for range 2 {
		id, err := st.StartSync(source.ID, "incremental")
		requirements.NoError(err)
		requirements.NoError(st.CompleteSync(id, ""))
	}
	run, err := st.StartCardDAVSyncRunContext(t.Context(), store.CardDAVSyncRunStart{Trigger: store.CardDAVSyncTriggerManual})
	requirements.NoError(err)
	_, err = st.FinishCardDAVSyncRunContext(t.Context(), run.ID, store.CardDAVSyncRunFinish{State: store.CardDAVSyncRunPartial, Created: 2, ErrorCode: "upstream_failed", ErrorMessage: "synthetic-private-contact@example.com"})
	requirements.NoError(err)
	// Pin fixture timestamps to exercise the producer's stable tie ordering.
	const timestamp = "2026-09-01 12:00:00"
	_, err = st.DB().ExecContext(t.Context(), "UPDATE sync_runs SET started_at = ?, completed_at = ?", timestamp, timestamp)
	requirements.NoError(err)
	_, err = st.DB().ExecContext(t.Context(), "UPDATE carddav_sync_runs SET started_at = ?, finished_at = ?", timestamp, timestamp)
	requirements.NoError(err)
	adapter := &storeAPIAdapter{store: st}
	backend := sourceOperationFixture(t, api.NewServerWithOptions(api.ServerOptions{Config: config.NewDefaultConfig(), Store: adapter, OperationHistoryReader: adapter, Logger: slog.New(slog.DiscardHandler)}))
	for _, name := range []string{"list_operation_runs", "get_operation_run", "get_operation_status"} {
		requirements.Contains(backend.capabilities(), name)
	}
	args := map[string]any{"limit": 1, "started_from": "2026-09-01T00:00:00Z", "started_before": "2026-09-02T00:00:00Z"}
	seen := map[string]bool{}
	var partialID, firstCursor string
	for pageIndex := range 3 {
		result, err := backend.ExecuteOperation(t.Context(), "list_operation_runs", args)
		requirements.NoError(err)
		page := operationOutput[generated.OperationRunsResponse](t, result)
		requirements.Len(page.Runs, 1)
		current := page.Runs[0]
		assertions.False(seen[current.ID], "a same-time cursor must not repeat a run")
		seen[current.ID] = true
		assertions.Contains(current.ID, "op2.")
		if string(current.Kind) == string(operations.KindCardDAVSync) {
			partialID = current.ID
			assertions.Equal("partial", string(current.State))
			requirements.NotNil(current.ErrorData)
		}
		if pageIndex == 0 {
			requirements.NotNil(page.NextCursor)
			firstCursor = *page.NextCursor
		}
		if pageIndex < 2 {
			requirements.NotNil(page.NextCursor)
			args["cursor"] = *page.NextCursor
		} else {
			assertions.Nil(page.NextCursor)
		}
		wire, err := json.Marshal(page)
		requirements.NoError(err)
		assertions.NotContains(string(wire), "synthetic-private-contact")
		assertions.NotContains(string(wire), "synthetic-history")
	}
	requirements.NotEmpty(partialID)
	result, err := backend.ExecuteOperation(t.Context(), "get_operation_run", map[string]any{"run_id": partialID})
	requirements.NoError(err)
	detail := operationOutput[generated.OperationRunDetail](t, result)
	assertions.Equal(partialID, detail.ID)
	assertions.Equal("partial", string(detail.State))
	// The actual owning parser checks filter binding rather than interpreting a cursor here.
	args["cursor"], args["kind"] = firstCursor, "source_sync"
	result, err = backend.ExecuteOperation(t.Context(), "list_operation_runs", args)
	requirements.NoError(err)
	assertions.True(result.IsError)
	failure, err := json.Marshal(result.Output)
	requirements.NoError(err)
	assertions.Contains(string(failure), "invalid_cursor")

	other := testutil.NewSQLiteTestStore(t)
	otherAdapter := &storeAPIAdapter{store: other}
	otherBackend := sourceOperationFixture(t, api.NewServerWithOptions(api.ServerOptions{Config: config.NewDefaultConfig(), Store: otherAdapter, OperationHistoryReader: otherAdapter, Logger: slog.New(slog.DiscardHandler)}))
	result, err = otherBackend.ExecuteOperation(t.Context(), "get_operation_run", map[string]any{"run_id": partialID})
	requirements.NoError(err)
	assertions.True(result.IsError)

	session := operationMCPSession(t, backend, nil, nil)
	for name, arguments := range map[string]map[string]any{"list_operation_runs": {"state": "partial"}, "get_operation_run": {"run_id": partialID}, "get_operation_status": {}} {
		called, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: name, Arguments: arguments})
		requirements.NoError(err)
		requirements.False(called.IsError)
		assertions.NotNil(called.StructuredContent)
	}
	status, err := backend.ExecuteOperation(t.Context(), "get_operation_status", nil)
	requirements.NoError(err)
	lanes := operationOutput[generated.OperationStatusResponse](t, status)
	assertions.NotEmpty(lanes.Lanes)
	latest, err := st.GetLatestSync(source.ID)
	requirements.NoError(err)
	assertions.Equal("completed", latest.Status, "history reads must not start another sync")
}

func TestMCPOperationHistoryRefusesInvalidInputsAndUnavailableStore(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	backend := sourceOperationFixture(t, api.NewServerWithOptions(api.ServerOptions{Config: config.NewDefaultConfig(), Logger: slog.New(slog.DiscardHandler)}))
	requirements.Contains(backend.capabilities(), "list_operation_runs")
	requirements.Contains(backend.capabilities(), "get_operation_run")
	requirements.Contains(backend.capabilities(), "get_operation_status")
	for _, input := range []map[string]any{{"kind": "arbitrary"}, {"lane": "arbitrary"}, {"state": "arbitrary"}, {"limit": 0}, {"limit": 101}, {"limit": nil}, {"cursor": ""}, {"started_from": "bad"}, {"started_before": "2026-09-01T01:00:00+01:00"}, {"started_from": "2026-09-02T00:00:00Z", "started_before": "2026-09-01T00:00:00Z"}, {"path": "/health"}} {
		result, err := backend.ExecuteOperation(t.Context(), "list_operation_runs", input)
		requirements.NoError(err)
		assertions.True(result.IsError)
	}
	for _, input := range []map[string]any{{}, {"run_id": 1}, {"run_id": nil}, {"run_id": ""}, {"run_id": "../status"}} {
		result, err := backend.ExecuteOperation(t.Context(), "get_operation_run", input)
		requirements.NoError(err)
		assertions.True(result.IsError)
	}
	result, err := backend.ExecuteOperation(t.Context(), "list_operation_runs", nil)
	requirements.NoError(err)
	assertions.True(result.IsError)
	status, err := backend.ExecuteOperation(t.Context(), "get_operation_status", nil)
	requirements.NoError(err)
	lanes := operationOutput[generated.OperationStatusResponse](t, status)
	requirements.NotEmpty(lanes.Lanes)
	for _, lane := range lanes.Lanes {
		assertions.Equal("unavailable", string(lane.HistoryAvailability))
		assertions.NotNil(lane.UnavailableCode)
		assertions.Empty(lane.SupportedActions)
	}
	session := operationMCPSession(t, backend, nil, nil)
	called, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "get_operation_run", Arguments: map[string]any{"run_id": "not-an-archive-token"}})
	requirements.NoError(err)
	assertions.True(called.IsError)
	assertions.NotNil(called.StructuredContent)
}

func TestMCPOperationHistoryRequiresActualFilterDiscovery(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	backend := sourceOperationFixture(t, api.NewServerWithOptions(api.ServerOptions{Config: config.NewDefaultConfig(), Logger: slog.New(slog.DiscardHandler)}))
	capabilities, err := backend.client.MCPCapabilities(t.Context())
	requirements.NoError(err)
	for index := range capabilities.Routes {
		if capabilities.Routes[index].OperationID == "listOperationRuns" {
			capabilities.Routes[index].QueryParameters = []string{"kind"}
		}
	}
	restricted := newDaemonMCPOperations(backend.client, capabilities)
	assertions.NotContains(restricted.capabilities(), "list_operation_runs")
	assertions.Contains(restricted.capabilities(), "get_operation_status")
}
