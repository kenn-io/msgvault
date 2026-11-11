package cmd

import (
	"context"
	"log/slog"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	mcpserver "go.kenn.io/msgvault/internal/mcp"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/pkg/client/generated"
)

type importMCPStore struct {
	*storeAPIAdapter

	requests chan api.CLISyncRequest
	release  chan struct{}
}

func (s *importMCPStore) RunCLISync(ctx context.Context, req api.CLISyncRequest, _ func(api.CLISyncEvent) error) error {
	s.requests <- req
	select {
	case <-s.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestMCPImportJobNativeAcceptanceAndRefusals(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := &importMCPStore{storeAPIAdapter: &storeAPIAdapter{store: testutil.NewSQLiteTestStore(t)}, requests: make(chan api.CLISyncRequest, 1), release: make(chan struct{})}
	source, err := st.store.GetOrCreateSource("gmail", "archive@example.com")
	requirements.NoError(err)
	srv := api.NewServerWithOptions(api.ServerOptions{Config: &config.Config{Data: config.DataConfig{DataDir: t.TempDir()}}, Store: st, Logger: slog.New(slog.DiscardHandler)})
	t.Cleanup(func() { close(st.release); assertions.NoError(srv.Shutdown(context.WithoutCancel(t.Context()))) })
	backend := sourceOperationFixture(t, srv)
	args := map[string]any{"account": "archive@example.com", "limit": float64(25), "after": "2024-01-01", "before": "2024-02-01", "query": "label:example", "noresume": true}
	result, err := backend.ExecuteOperation(t.Context(), "create_import_job", args)
	requirements.NoError(err)
	created := operationOutput[generated.ImportJobResponse](t, result)
	assertions.Equal(generated.ImportJobResponseStatusPending, created.Status)
	req := <-st.requests
	assertions.Equal(source.ID, req.SourceID)
	assertions.Equal(25, req.Limit)
	assertions.Equal("2024-01-01", req.After)
	assertions.Equal("2024-02-01", req.Before)
	assertions.Equal("label:example", req.Query)
	assertions.True(req.NoResume)
	result, err = backend.ExecuteOperation(t.Context(), "create_import_job", args)
	requirements.NoError(err)
	assertions.True(result.IsError)
	assertions.Equal(operationFailure("sync_already_active", false).Output, result.Output)
	result, err = backend.ExecuteOperation(t.Context(), "get_import_job", map[string]any{"job_id": created.JobID})
	requirements.NoError(err)
	assertions.Equal(created.JobID, operationOutput[generated.ImportJobResponse](t, result).JobID)
	requirements.NoError(st.store.FinishSyncOperation(t.Context(), created.JobID, "done"))
	result, err = backend.ExecuteOperation(t.Context(), "get_import_job", map[string]any{"job_id": created.JobID})
	requirements.NoError(err)
	completed := operationOutput[generated.ImportJobResponse](t, result)
	assertions.Equal(generated.ImportJobResponseStatusDone, completed.Status)
	for _, args := range []map[string]any{{"account": "archive@example.com"}, {"account": "archive@example.com", "limit": 0}} {
		result, err = backend.ExecuteOperation(t.Context(), "create_import_job", args)
		requirements.NoError(err)
		assertions.True(result.IsError)
	}
}

func TestMCPImportDiscoveryAndNativeValidation(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := &importMCPStore{storeAPIAdapter: &storeAPIAdapter{store: testutil.NewSQLiteTestStore(t)}, requests: make(chan api.CLISyncRequest, 1), release: make(chan struct{})}
	_, err := st.store.GetOrCreateSource("imap", "imap@example.com")
	requirements.NoError(err)
	srv := api.NewServerWithOptions(api.ServerOptions{Config: &config.Config{Data: config.DataConfig{DataDir: t.TempDir()}}, Store: st, Logger: slog.New(slog.DiscardHandler)})
	t.Cleanup(func() { close(st.release); assertions.NoError(srv.Shutdown(context.WithoutCancel(t.Context()))) })
	backend := sourceOperationFixture(t, srv)
	assertions.Contains(backend.capabilities(), "create_import_job")
	assertions.Contains(backend.capabilities(), "get_import_job")
	capabilities, err := backend.client.MCPCapabilities(t.Context())
	requirements.NoError(err)
	for i := range capabilities.Routes {
		if capabilities.Routes[i].OperationID == "createImportJob" {
			capabilities.Routes[i].RequestProperties = nil
		}
	}
	missing := newDaemonMCPOperations(backend.client, capabilities)
	assertions.NotContains(missing.capabilities(), "create_import_job")
	assertions.Contains(missing.capabilities(), "get_import_job")
	result, err := backend.ExecuteOperation(t.Context(), "create_import_job", map[string]any{"account": "imap@example.com", "limit": 3, "query": "example"})
	requirements.NoError(err)
	assertions.True(result.IsError)
	assertions.Equal(operationFailure("validation_failed", false).Output, result.Output)
	disclosure, err := backend.OperationDisclosure(t.Context(), "create_import_job", map[string]any{"account": "imap@example.com", "limit": 3, "noresume": true})
	requirements.NoError(err)
	assertions.Contains(disclosure, "imap@example.com")
	assertions.Contains(disclosure, `"limit":3`)
	assertions.Contains(disclosure, `"noresume":true`)
}

func TestMCPImportUnsafeIDDoesNotReachDaemon(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	backend := &daemonMCPOperations{supported: []string{"get_import_job"}}
	for _, id := range []string{"", ".", "..", "../other", "job?x=1", "job%2fother"} {
		result, err := backend.ExecuteOperation(t.Context(), "get_import_job", map[string]any{"job_id": id})
		requirements.NoError(err)
		assertions.True(result.IsError)
		assertions.Equal(operationFailure("invalid_arguments", false).Output, result.Output)
	}
}

func TestMCPImportSDKRequiresApproval(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := &importMCPStore{storeAPIAdapter: &storeAPIAdapter{store: testutil.NewSQLiteTestStore(t)}, requests: make(chan api.CLISyncRequest, 1), release: make(chan struct{})}
	srv := api.NewServerWithOptions(api.ServerOptions{Config: &config.Config{Data: config.DataConfig{DataDir: t.TempDir()}}, Store: st, Logger: slog.New(slog.DiscardHandler)})
	t.Cleanup(func() { close(st.release); assertions.NoError(srv.Shutdown(context.WithoutCancel(t.Context()))) })
	backend := sourceOperationFixture(t, srv)
	_, err := st.store.GetOrCreateSource("gmail", "archive@example.com")
	requirements.NoError(err)
	approvals := 0
	session := operationMCPSession(t, backend, []mcpserver.OperationFamily{mcpserver.OperationFamilyImports}, &sdkmcp.ClientOptions{ElicitationHandler: func(_ context.Context, request *sdkmcp.ElicitRequest) (*sdkmcp.ElicitResult, error) {
		approvals++
		assertions.Contains(request.Params.Message, "archive@example.com")
		assertions.Contains(request.Params.Message, `"limit":3`)
		return &sdkmcp.ElicitResult{Action: "accept", Content: map[string]any{"confirm": true}}, nil
	}})
	result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "create_import_job", Arguments: map[string]any{"account": "archive@example.com", "limit": 3}})
	requirements.NoError(err)
	created := loopToolOutput[generated.ImportJobResponse](t, result)
	assertions.Equal(1, approvals)
	assertions.Equal(generated.ImportJobResponseStatusPending, created.Status)
	requirements.NotEmpty(created.JobID)
	<-st.requests
	result, err = session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "get_import_job", Arguments: map[string]any{"job_id": created.JobID}})
	requirements.NoError(err)
	polled := loopToolOutput[generated.ImportJobResponse](t, result)
	assertions.Equal(created.JobID, polled.JobID)
	assertions.Equal(generated.ImportJobResponseStatusPending, polled.Status)
	assertions.Equal(1, approvals)
}
