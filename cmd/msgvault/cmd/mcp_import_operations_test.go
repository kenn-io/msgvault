package cmd

import (
	"context"
	"encoding/json/v2"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/apiprotocol"
	"go.kenn.io/msgvault/internal/config"
	mcpserver "go.kenn.io/msgvault/internal/mcp"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/pkg/client/generated"
)

// Cancel the real worker before subprocess admission. Durable acceptance and
// terminal writes still use the production adapter and Store; no CLI command
// or provider is stubbed and no external provider request can start.
type cancelledMCPImportWorker struct {
	*storeAPIAdapter

	finished chan cancelledMCPImportResult
}

type cancelledMCPImportResult struct {
	request api.CLISyncRequest
	err     error
}

func (a *cancelledMCPImportWorker) RunCLISync(ctx context.Context, request api.CLISyncRequest, emit func(api.CLISyncEvent) error) error {
	ctx, cancel := context.WithCancel(ctx)
	cancel()
	err := a.storeAPIAdapter.RunCLISync(ctx, request, emit)
	a.finished <- cancelledMCPImportResult{request, err}
	return err
}

func TestMCPImportJobsUseRealDurableAcceptanceAndPoll(t *testing.T) {
	requirements, assertions := require.New(t), assert.New(t)
	cfg := config.NewDefaultConfig()
	cfg.HomeDir = t.TempDir()
	cfg.Data.DataDir = cfg.HomeDir
	requirements.NoError(cfg.Save())
	st := testutil.NewSQLiteTestStore(t)
	source, err := st.GetOrCreateSource("gmail", "archive@example.com")
	requirements.NoError(err)
	worker := &cancelledMCPImportWorker{storeAPIAdapter: &storeAPIAdapter{store: st, config: cfg, options: invocationOptions{cfgFile: cfg.ConfigFilePath(), homeDir: cfg.HomeDir, cfgFileChanged: true, homeDirChanged: true}}, finished: make(chan cancelledMCPImportResult, 2)}
	server := api.NewServerWithOptions(api.ServerOptions{Config: cfg, Store: worker, Logger: slog.New(slog.DiscardHandler)})
	t.Cleanup(func() { requirements.NoError(server.Shutdown(context.Background())) })
	backend := sourceOperationFixture(t, server)
	requirements.Contains(backend.capabilities(), "create_import_job")
	requirements.Contains(backend.capabilities(), "get_import_job")
	approvals := 0
	session := operationMCPSession(t, backend, []mcpserver.OperationFamily{mcpserver.OperationFamilySources}, &sdkmcp.ClientOptions{ElicitationHandler: func(_ context.Context, request *sdkmcp.ElicitRequest) (*sdkmcp.ElicitResult, error) {
		approvals++
		if approvals == 1 {
			assertions.Contains(request.Params.Message, "unlimited")
			assertions.Contains(request.Params.Message, "2026-01-01")
			assertions.Contains(request.Params.Message, "gmail")
		} else {
			assertions.Contains(request.Params.Message, "bounded by the requested message limit")
			assertions.Contains(request.Params.Message, "imap")
		}
		return &sdkmcp.ElicitResult{Action: "accept", Content: map[string]any{"confirm": true}}, nil
	}})
	called, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "create_import_job", Arguments: map[string]any{"account": "ARCHIVE@EXAMPLE.COM", "after": "2026-01-01", "before": "2026-02-01", "limit": 0, "query": "label:archive", "noresume": false}})
	requirements.NoError(err)
	requirements.False(called.IsError, "%s", settingsMCPDiagnostic(called))
	data, err := json.Marshal(called.StructuredContent)
	requirements.NoError(err)
	var accepted generated.ImportJobResponse
	requirements.NoError(json.Unmarshal(data, &accepted))
	assertions.Equal("pending", string(accepted.Status))
	requirements.NotEmpty(accepted.JobID)
	assertions.Equal(1, approvals)
	select {
	case finished := <-worker.finished:
		requirements.ErrorIs(finished.err, context.Canceled)
		assertions.Equal(source.ID, finished.request.SourceID)
		assertions.True(finished.request.SourceIDSet)
		assertions.True(finished.request.Full)
		assertions.Zero(finished.request.Limit)
		assertions.False(finished.request.NoResume)
		assertions.Equal("2026-01-01", finished.request.After)
		assertions.Equal("2026-02-01", finished.request.Before)
		assertions.Equal("label:archive", finished.request.Query)
		assertions.Equal(accepted.JobID, finished.request.OperationID)
	case <-time.After(5 * time.Second):
		requirements.FailNow("import worker did not reach its terminal transition")
	}
	durable, err := st.GetSyncOperation(accepted.JobID)
	requirements.NoError(err)
	assertions.Equal(source.ID, durable.SourceID)
	assertions.Equal("failed", durable.Status)
	called, err = session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "get_import_job", Arguments: map[string]any{"job_id": accepted.JobID}})
	requirements.NoError(err)
	requirements.False(called.IsError)
	data, err = json.Marshal(called.StructuredContent)
	requirements.NoError(err)
	var current generated.ImportJobResponse
	requirements.NoError(json.Unmarshal(data, &current))
	assertions.Equal(accepted.JobID, current.JobID)
	assertions.Equal("failed", string(current.Status))
	requirements.NotNil(current.ErrorData)
	assertions.Equal("import failed", *current.ErrorData)
	requirements.NotNil(current.FinishedAt)
	assertions.NotContains(string(data), "context canceled")
	assertions.Equal(1, approvals, "polling requires no additional approval")
	imap, err := st.GetOrCreateSource("imap", "imap@example.com")
	requirements.NoError(err)
	called, err = session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "create_import_job", Arguments: map[string]any{"account": "imap@example.com", "limit": 3, "noresume": true}})
	requirements.NoError(err)
	requirements.False(called.IsError, "%s", settingsMCPDiagnostic(called))
	select {
	case finished := <-worker.finished:
		requirements.ErrorIs(finished.err, context.Canceled)
		assertions.Equal(imap.ID, finished.request.SourceID)
		assertions.Equal(3, finished.request.Limit)
		assertions.True(finished.request.NoResume)
		assertions.Empty(finished.request.After)
		assertions.Empty(finished.request.Before)
		assertions.Empty(finished.request.Query)
	case <-time.After(5 * time.Second):
		requirements.FailNow("IMAP worker did not reach its terminal transition")
	}
	assertions.Equal(2, approvals)
}

func TestMCPImportJobsRefuseAmbiguityInvalidBoundsAndOwnerSourceLocks(t *testing.T) {
	requirements, assertions := require.New(t), assert.New(t)
	cfg := config.NewDefaultConfig()
	cfg.HomeDir = t.TempDir()
	cfg.Data.DataDir = cfg.HomeDir
	requirements.NoError(cfg.Save())
	st := testutil.NewSQLiteTestStore(t)
	mail, err := st.GetOrCreateSource("gmail", "archive@example.com")
	requirements.NoError(err)
	requirements.NoError(st.UpdateSourceDisplayName(mail.ID, "Primary Archive"))
	_, err = st.GetOrCreateSource("mbox", "archive@example.com")
	requirements.NoError(err)
	_, err = st.GetOrCreateSource("imap", "imap@example.com")
	requirements.NoError(err)
	_, err = st.GetOrCreateSource("mbox", "offline@example.com")
	requirements.NoError(err)
	worker := &cancelledMCPImportWorker{storeAPIAdapter: &storeAPIAdapter{store: st, config: cfg}, finished: make(chan cancelledMCPImportResult, 16)}
	server := api.NewServerWithOptions(api.ServerOptions{Config: cfg, Store: worker, Logger: slog.New(slog.DiscardHandler)})
	t.Cleanup(func() { requirements.NoError(server.Shutdown(context.Background())) })
	backend := sourceOperationFixture(t, server)
	for _, account := range []string{"archive@example.com", "primary archive"} {
		disclosure, err := backend.OperationDisclosure(t.Context(), "create_import_job", map[string]any{"account": account})
		requirements.NoError(err)
		assertions.Contains(disclosure, "unlimited")
		assertions.Contains(disclosure, "absent dates are unbounded")
	}
	for _, args := range []map[string]any{
		{"account": "unknown@example.com"}, {"account": "offline@example.com"}, {"account": nil},
		{"account": "imap@example.com", "query": "label:archive"},
		{"account": "archive@example.com", "limit": -1},
		{"account": "archive@example.com", "limit": 9007199254740992},
		{"account": "archive@example.com", "limit": nil},
		{"account": "archive@example.com", "after": "2026-02-30"},
		{"account": "archive@example.com", "after": ""},
		{"account": "archive@example.com", "before": nil},
		{"account": "archive@example.com", "after": "0001-01-01", "before": "0001-01-01"},
		{"account": "archive@example.com", "after": "2026-02-01", "before": "2026-01-01"},
		{"account": "archive@example.com", "query": strings.Repeat("q", 4097)},
		{"account": "archive@example.com", "noresume": "false"},
		{"account": "archive@example.com", "env": map[string]any{}},
	} {
		_, err = backend.OperationDisclosure(t.Context(), "create_import_job", args)
		requirements.Error(err)
		result, err := backend.ExecuteOperation(t.Context(), "create_import_job", args)
		requirements.NoError(err)
		assertions.True(result.IsError)
	}
	_, err = st.CreateSyncOperation(mail.ID, "reserved-import")
	requirements.NoError(err)
	result, err := backend.ExecuteOperation(t.Context(), "create_import_job", map[string]any{"account": "primary archive", "limit": 3})
	requirements.NoError(err)
	requirements.True(result.IsError)
	data, err := json.Marshal(result.Output)
	requirements.NoError(err)
	assertions.Contains(string(data), "sync_already_active")
	assertions.Empty(worker.finished, "refusals do not schedule a worker")
	_, err = st.GetOrCreateSource("imap", "archive@example.com")
	requirements.NoError(err)
	_, err = backend.OperationDisclosure(t.Context(), "create_import_job", map[string]any{"account": "archive@example.com"})
	var refusal *mcpserver.OperationRefusalError
	requirements.ErrorAs(err, &refusal)
	assertions.Equal("ambiguous_account", refusal.Code)
	capabilities, err := backend.client.MCPCapabilities(t.Context())
	requirements.NoError(err)
	for _, id := range []string{"createImportJob", "getImportJob", "listSourceStatus"} {
		limited := *capabilities
		limited.Routes = slices.DeleteFunc(slices.Clone(capabilities.Routes), func(route apiprotocol.MCPRouteDescriptor) bool { return route.OperationID == id })
		assertions.NotContains(newDaemonMCPOperations(backend.client, &limited).capabilities(), "create_import_job")
	}
	limited := *capabilities
	limited.Routes = slices.Clone(capabilities.Routes)
	for i := range limited.Routes {
		if limited.Routes[i].OperationID == "createImportJob" {
			limited.Routes[i].RequestProperties = slices.DeleteFunc(slices.Clone(limited.Routes[i].RequestProperties), func(name string) bool { return name == "noresume" })
		}
	}
	assertions.NotContains(newDaemonMCPOperations(backend.client, &limited).capabilities(), "create_import_job")
}

func TestMCPImportJobReadsPreserveCompletedCountsRecoveryAndArchiveBoundary(t *testing.T) {
	requirements, assertions := require.New(t), assert.New(t)
	st := testutil.NewSQLiteTestStore(t)
	source, err := st.GetOrCreateSource("gmail", "archive@example.com")
	requirements.NoError(err)
	_, err = st.CreateSyncOperation(source.ID, "completed-import")
	requirements.NoError(err)
	execution, err := st.AcquireSyncExecutionContext(t.Context(), source.ID)
	requirements.NoError(err)
	t.Cleanup(func() { requirements.NoError(execution.Release()) })
	runID, err := execution.StartSyncContext(t.Context(), "full", "completed-import")
	requirements.NoError(err)
	requirements.NoError(st.UpdateSyncCheckpoint(runID, &store.Checkpoint{MessagesProcessed: 12, MessagesAdded: 7, MessagesUpdated: 3, ErrorsCount: 1}))
	requirements.NoError(st.CompleteSync(runID, "finished"))
	requirements.NoError(execution.Release())
	requirements.NoError(st.FinishSyncOperation("completed-import", "done"))
	backend := sourceOperationFixture(t, api.NewServerWithOptions(api.ServerOptions{Config: config.NewDefaultConfig(), Store: &storeAPIAdapter{store: st}, Logger: slog.New(slog.DiscardHandler)}))
	session := operationMCPSession(t, backend, nil, nil)
	called, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "get_import_job", Arguments: map[string]any{"job_id": "completed-import"}})
	requirements.NoError(err)
	requirements.False(called.IsError)
	data, err := json.Marshal(called.StructuredContent)
	requirements.NoError(err)
	var receipt generated.ImportJobResponse
	requirements.NoError(json.Unmarshal(data, &receipt))
	assertions.Equal("done", string(receipt.Status))
	requirements.NotNil(receipt.Summary)
	assertions.Equal(int64(12), receipt.Summary.Processed)
	assertions.Equal(int64(7), receipt.Summary.Added)
	assertions.Equal(int64(3), receipt.Summary.Updated)
	assertions.Equal(int64(2), receipt.Summary.Skipped)
	assertions.Equal(int64(1), receipt.Summary.Errors)
	_, err = st.CreateSyncOperation(source.ID, "abandoned-import")
	requirements.NoError(err)
	recovered, err := st.FailUnfinishedSyncOperationsContext(t.Context())
	requirements.NoError(err)
	assertions.Equal(int64(1), recovered)
	result, err := backend.ExecuteOperation(t.Context(), "get_import_job", map[string]any{"job_id": "abandoned-import"})
	requirements.NoError(err)
	receipt = operationOutput[generated.ImportJobResponse](t, result)
	assertions.Equal("failed", string(receipt.Status))
	requirements.NotNil(receipt.FinishedAt)
	other := testutil.NewSQLiteTestStore(t)
	otherBackend := sourceOperationFixture(t, api.NewServerWithOptions(api.ServerOptions{Config: config.NewDefaultConfig(), Store: &storeAPIAdapter{store: other}, Logger: slog.New(slog.DiscardHandler)}))
	result, err = otherBackend.ExecuteOperation(t.Context(), "get_import_job", map[string]any{"job_id": "completed-import"})
	requirements.NoError(err)
	assertions.True(result.IsError)
	for _, args := range []map[string]any{{"job_id": nil}, {"job_id": ""}, {"job_id": "../completed-import"}, {"job_id": "completed-import", "command": "status"}} {
		result, err = backend.ExecuteOperation(t.Context(), "get_import_job", args)
		requirements.NoError(err)
		assertions.True(result.IsError)
	}
}
