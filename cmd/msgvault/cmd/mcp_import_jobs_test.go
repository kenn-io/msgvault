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
	st := &importMCPStore{storeAPIAdapter: &storeAPIAdapter{store: testutil.NewSQLiteTestStore(t)}, requests: make(chan api.CLISyncRequest, 1), release: make(chan struct{})}
	source, err := st.store.GetOrCreateSource("gmail", "archive@example.com")
	require.NoError(t, err)
	srv := api.NewServerWithOptions(api.ServerOptions{Config: &config.Config{Data: config.DataConfig{DataDir: t.TempDir()}}, Store: st, Logger: slog.New(slog.DiscardHandler)})
	t.Cleanup(func() { close(st.release); assert.NoError(t, srv.Shutdown(context.WithoutCancel(t.Context()))) })
	backend := sourceOperationFixture(t, srv)
	args := map[string]any{"account": "archive@example.com", "limit": float64(25), "after": "2024-01-01", "before": "2024-02-01", "query": "label:example", "noresume": true}
	result, err := backend.ExecuteOperation(t.Context(), "create_import_job", args)
	require.NoError(t, err)
	created := operationOutput[generated.ImportJobResponse](t, result)
	assert.Equal(t, generated.ImportJobResponseStatusPending, created.Status)
	req := <-st.requests
	assert.Equal(t, source.ID, req.SourceID)
	assert.Equal(t, 25, req.Limit)
	assert.Equal(t, "2024-01-01", req.After)
	assert.Equal(t, "2024-02-01", req.Before)
	assert.Equal(t, "label:example", req.Query)
	assert.True(t, req.NoResume)
	result, err = backend.ExecuteOperation(t.Context(), "create_import_job", args)
	require.NoError(t, err)
	assert.True(t, result.IsError)
	assert.Equal(t, operationFailure("sync_already_active", false).Output, result.Output)
	result, err = backend.ExecuteOperation(t.Context(), "get_import_job", map[string]any{"job_id": created.JobID})
	require.NoError(t, err)
	assert.Equal(t, created.JobID, operationOutput[generated.ImportJobResponse](t, result).JobID)
	require.NoError(t, st.store.FinishSyncOperation(t.Context(), created.JobID, "done"))
	result, err = backend.ExecuteOperation(t.Context(), "get_import_job", map[string]any{"job_id": created.JobID})
	require.NoError(t, err)
	completed := operationOutput[generated.ImportJobResponse](t, result)
	assert.Equal(t, generated.ImportJobResponseStatusDone, completed.Status)
	assert.NotNil(t, completed.Summary)
	assert.NotNil(t, completed.FinishedAt)
	for _, args := range []map[string]any{{"account": "archive@example.com"}, {"account": "archive@example.com", "limit": 0}, {"job_id": "../unsafe"}} {
		name := "create_import_job"
		if _, ok := args["job_id"]; ok {
			name = "get_import_job"
		}
		result, err = backend.ExecuteOperation(t.Context(), name, args)
		require.NoError(t, err)
		assert.True(t, result.IsError)
	}
}

func TestMCPImportDiscoveryAndNativeValidation(t *testing.T) {
	st := &importMCPStore{storeAPIAdapter: &storeAPIAdapter{store: testutil.NewSQLiteTestStore(t)}, requests: make(chan api.CLISyncRequest, 1), release: make(chan struct{})}
	_, err := st.store.GetOrCreateSource("imap", "imap@example.com")
	require.NoError(t, err)
	srv := api.NewServerWithOptions(api.ServerOptions{Config: &config.Config{Data: config.DataConfig{DataDir: t.TempDir()}}, Store: st, Logger: slog.New(slog.DiscardHandler)})
	t.Cleanup(func() { close(st.release); assert.NoError(t, srv.Shutdown(context.WithoutCancel(t.Context()))) })
	backend := sourceOperationFixture(t, srv)
	assert.Contains(t, backend.capabilities(), "create_import_job")
	assert.Contains(t, backend.capabilities(), "get_import_job")
	capabilities, err := backend.client.MCPCapabilities(t.Context())
	require.NoError(t, err)
	for i := range capabilities.Routes {
		if capabilities.Routes[i].OperationID == "createImportJob" {
			capabilities.Routes[i].RequestProperties = nil
		}
	}
	missing := newDaemonMCPOperations(backend.client, capabilities)
	assert.NotContains(t, missing.capabilities(), "create_import_job")
	assert.Contains(t, missing.capabilities(), "get_import_job")
	for _, tc := range []struct {
		args map[string]any
		code string
	}{
		{map[string]any{"account": "imap@example.com", "limit": 3, "query": "example"}, "validation_failed"},
		{map[string]any{"account": "imap@example.com", "limit": 3, "after": "2024-03-01", "before": "2024-02-01"}, "validation_failed"},
		{map[string]any{"account": "missing@example.com", "limit": 3}, "not_found"},
	} {
		result, err := backend.ExecuteOperation(t.Context(), "create_import_job", tc.args)
		require.NoError(t, err)
		assert.True(t, result.IsError)
		assert.Equal(t, operationFailure(tc.code, false).Output, result.Output)
	}
	disclosure, err := backend.OperationDisclosure(t.Context(), "create_import_job", map[string]any{"account": "imap@example.com", "limit": 3, "noresume": true})
	require.NoError(t, err)
	assert.Contains(t, disclosure, "imap@example.com")
	assert.Contains(t, disclosure, `"limit":3`)
	assert.Contains(t, disclosure, `"noresume":true`)
	assert.True(t, importJobRefusal("validation_failed"))
	assert.False(t, importJobRefusal("internal_error"))
}

func TestMCPImportUnsafeIDDoesNotReachDaemon(t *testing.T) {
	backend := &daemonMCPOperations{supported: []string{"get_import_job"}}
	for _, id := range []string{"", ".", "..", "../other", "job?x=1", "job%2fother"} {
		result, err := backend.ExecuteOperation(t.Context(), "get_import_job", map[string]any{"job_id": id})
		require.NoError(t, err)
		assert.True(t, result.IsError)
		assert.Equal(t, operationFailure("invalid_arguments", false).Output, result.Output)
	}
}

func TestMCPImportSDKRequiresApproval(t *testing.T) {
	st := &importMCPStore{storeAPIAdapter: &storeAPIAdapter{store: testutil.NewSQLiteTestStore(t)}, requests: make(chan api.CLISyncRequest, 1), release: make(chan struct{})}
	srv := api.NewServerWithOptions(api.ServerOptions{Config: &config.Config{Data: config.DataConfig{DataDir: t.TempDir()}}, Store: st, Logger: slog.New(slog.DiscardHandler)})
	t.Cleanup(func() { close(st.release); assert.NoError(t, srv.Shutdown(context.WithoutCancel(t.Context()))) })
	backend := sourceOperationFixture(t, srv)
	called := false
	session := operationMCPSession(t, backend, []mcpserver.OperationFamily{mcpserver.OperationFamilyImports}, &sdkmcp.ClientOptions{ElicitationHandler: func(context.Context, *sdkmcp.ElicitRequest) (*sdkmcp.ElicitResult, error) {
		called = true
		return &sdkmcp.ElicitResult{Action: "accept", Content: map[string]any{"confirm": true}}, nil
	}})
	result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "create_import_job", Arguments: map[string]any{"account": "missing@example.com", "limit": 3}})
	require.NoError(t, err)
	assert.True(t, called)
	assert.True(t, result.IsError)
	assert.Equal(t, "not_found", result.StructuredContent.(map[string]any)["error"])
}
