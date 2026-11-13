package cmd

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/gmail"
	mcpserver "go.kenn.io/msgvault/internal/mcp"
	msgsync "go.kenn.io/msgvault/internal/sync"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/pkg/client/generated"
)

type importMCPStore struct {
	*storeAPIAdapter

	requests chan api.CLISyncRequest
	release  chan struct{}
	runner   func(context.Context, api.CLISyncRequest) error
}

func (s *importMCPStore) RunCLISync(ctx context.Context, req api.CLISyncRequest, emit func(api.CLISyncEvent) error) error {
	if s.runner != nil {
		return s.runCLISyncOperationWithRunner(ctx, req, emit, func(ctx context.Context, _ []string, _ func(string, string) error) error {
			return s.runner(ctx, req)
		})
	}
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
}

func TestMCPImportDiscoveryAndDisclosure(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := &storeAPIAdapter{store: testutil.NewSQLiteTestStore(t)}
	srv := api.NewServerWithOptions(api.ServerOptions{Config: &config.Config{Data: config.DataConfig{DataDir: t.TempDir()}}, Store: st, Logger: slog.New(slog.DiscardHandler)})
	t.Cleanup(func() { assertions.NoError(srv.Shutdown(context.WithoutCancel(t.Context()))) })
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
	disclosure, err := backend.OperationDisclosure(t.Context(), "create_import_job", map[string]any{"account": " imap@example.com ", "limit": float64(3), "noresume": true})
	requirements.NoError(err)
	assertions.Contains(disclosure, `"account":"imap@example.com"`)
	assertions.Contains(disclosure, `"limit":3`)
	assertions.Contains(disclosure, `"noresume":true`)
	for _, id := range []string{"", ".", "..", "../other", "job?x=1", "job%2fother"} {
		result, err := backend.ExecuteOperation(t.Context(), "get_import_job", map[string]any{"job_id": id})
		requirements.NoError(err)
		assertions.True(result.IsError)
		assertions.Equal(operationFailure("invalid_arguments", false).Output, result.Output)
	}
}

func TestMCPImportDateValidationBeforeApproval(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := &storeAPIAdapter{store: testutil.NewSQLiteTestStore(t)}
	srv := api.NewServerWithOptions(api.ServerOptions{Config: &config.Config{Data: config.DataConfig{DataDir: t.TempDir()}}, Store: st, Logger: slog.New(slog.DiscardHandler)})
	t.Cleanup(func() { assertions.NoError(srv.Shutdown(context.WithoutCancel(t.Context()))) })
	backend := sourceOperationFixture(t, srv)
	args := map[string]any{"account": "archive@example.com", "limit": float64(3), "after": "2024-02-30"}
	_, err := backend.OperationDisclosure(t.Context(), "create_import_job", args)
	var refusal *mcpserver.OperationRefusalError
	requirements.ErrorAs(err, &refusal)
	assertions.Equal("invalid_arguments", refusal.Code)
	result, err := backend.ExecuteOperation(t.Context(), "create_import_job", args)
	requirements.NoError(err)
	assertions.True(result.IsError)
	assertions.Equal(operationFailure("validation_failed", false).Output, result.Output)
}

func TestMCPImportSDKDateValidation(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := &importMCPStore{storeAPIAdapter: &storeAPIAdapter{store: testutil.NewSQLiteTestStore(t)}, requests: make(chan api.CLISyncRequest, 1), release: make(chan struct{})}
	srv := api.NewServerWithOptions(api.ServerOptions{Config: &config.Config{Data: config.DataConfig{DataDir: t.TempDir()}}, Store: st, Logger: slog.New(slog.DiscardHandler)})
	t.Cleanup(func() { close(st.release); assertions.NoError(srv.Shutdown(context.WithoutCancel(t.Context()))) })
	_, err := st.store.GetOrCreateSource("gmail", "archive@example.com")
	requirements.NoError(err)
	approvals := 0
	session := operationMCPSession(t, sourceOperationFixture(t, srv), []mcpserver.OperationFamily{mcpserver.OperationFamilyImports}, &sdkmcp.ClientOptions{ElicitationHandler: func(context.Context, *sdkmcp.ElicitRequest) (*sdkmcp.ElicitResult, error) {
		approvals++
		return &sdkmcp.ElicitResult{Action: "accept", Content: map[string]any{"confirm": true}}, nil
	}})
	for _, args := range []map[string]any{
		{"account": "archive@example.com", "limit": 3, "after": "2024-02-30"},
		{"account": "archive@example.com", "limit": 3, "after": "2024/01/01"},
		{"account": "archive@example.com", "limit": 3, "before": "2024/01/01"},
		{"account": "archive@example.com", "limit": 3, "before": "2024-02-30"},
		{"account": "archive@example.com", "limit": 3, "after": "2024-01-01", "before": "2024-01-01"},
		{"account": "archive@example.com", "limit": 3, "after": "2024-02-01", "before": "2024-01-01"},
		{"account": "archive@example.com"},
		{"account": "archive@example.com", "limit": 0},
	} {
		result, callErr := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "create_import_job", Arguments: args})
		requirements.NoError(callErr)
		assertions.True(result.IsError, "%v", args)
		assertions.Zero(approvals)
	}
	result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "create_import_job", Arguments: map[string]any{"account": "archive@example.com", "limit": 3, "after": "2024-02-29"}})
	requirements.NoError(err)
	assertions.False(result.IsError)
	assertions.Equal(1, approvals)
	<-st.requests
}

func TestMCPImportRealWorker(t *testing.T) {
	for _, fail := range []bool{false, true} {
		name := "success"
		if fail {
			name = "failure"
		}
		t.Run(name, func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			cfg := config.NewDefaultConfig()
			cfg.Data.DataDir = t.TempDir()
			cfg.Analytics.AutoBuildCache = true
			refreshes := make(chan struct{}, 2)
			jobs := newCacheBuildJobs(t.Context(), nil, func(context.Context, buildCacheMode) error { refreshes <- struct{}{}; return nil })
			st := &importMCPStore{storeAPIAdapter: &storeAPIAdapter{store: testutil.NewSQLiteTestStore(t), config: cfg, cacheJobs: jobs}}
			source, err := st.store.GetOrCreateSource("gmail", "archive@example.com")
			requirements.NoError(err)
			client := gmail.NewMockAPI()
			client.MessagePages = [][]string{{"one", "two", "three"}}
			for _, id := range client.MessagePages[0] {
				client.AddMessage(id, []byte("From: sender@example.com\r\nTo: archive@example.com\r\nSubject: Example\r\nMessage-ID: <"+id+"@example.com>\r\n\r\nExample body"), []string{"INBOX"})
			}
			if fail {
				client.ProfileError = errors.New("provider failed")
			}
			st.runner = func(ctx context.Context, req api.CLISyncRequest) error {
				opts := msgsync.DefaultOptions()
				opts.Limit = req.Limit
				opts.OperationID = req.OperationID
				_, syncErr := newMessageSyncer(client, st.store, opts, testInvocationWithConfig(cfg)).WithLogger(slog.New(slog.DiscardHandler)).FullWithFinalizer(ctx, source, nil)
				return syncErr
			}
			srv := api.NewServerWithOptions(api.ServerOptions{Config: cfg, Store: st, Logger: slog.New(slog.DiscardHandler)})
			t.Cleanup(func() {
				assertions.NoError(srv.Shutdown(context.WithoutCancel(t.Context())))
				ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), serveLifecycleTestTimeout)
				defer cancel()
				assertions.True(jobs.waitContext(ctx))
			})
			approvals := 0
			session := operationMCPSession(t, sourceOperationFixture(t, srv), []mcpserver.OperationFamily{mcpserver.OperationFamilyImports}, &sdkmcp.ClientOptions{ElicitationHandler: func(context.Context, *sdkmcp.ElicitRequest) (*sdkmcp.ElicitResult, error) {
				approvals++
				return &sdkmcp.ElicitResult{Action: "accept", Content: map[string]any{"confirm": true}}, nil
			}})
			result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "create_import_job", Arguments: map[string]any{"account": "archive@example.com", "limit": 2}})
			requirements.NoError(err)
			created := loopToolOutput[generated.ImportJobResponse](t, result)
			assertions.Equal(1, approvals)
			want := generated.ImportJobResponseStatusDone
			if fail {
				want = generated.ImportJobResponseStatusFailed
			}
			var polled generated.ImportJobResponse
			requirements.Eventually(func() bool {
				result, callErr := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "get_import_job", Arguments: map[string]any{"job_id": created.JobID}})
				if callErr != nil || result.IsError {
					return false
				}
				polled = loopToolOutput[generated.ImportJobResponse](t, result)
				return polled.Status == want
			}, 10*time.Second, 20*time.Millisecond)
			if fail {
				requirements.NotNil(polled.ErrorData)
				assertions.Equal("import failed", *polled.ErrorData)
			} else {
				assertions.EqualValues(2, polled.Added)
			}
			if !fail {
				ids, err := st.store.GetRandomMessageIDs(source.ID, 3)
				requirements.NoError(err)
				requirements.Len(ids, 2)
				for _, id := range ids {
					body, bodyErr := st.store.GetMessageBodyText(t.Context(), id)
					requirements.NoError(bodyErr)
					assertions.Contains(body, "Example body")
				}
			}
			select {
			case <-refreshes:
			case <-time.After(10 * time.Second):
				requirements.FailNow("cache refresh was not queued")
			}
			ctx, cancel := context.WithTimeout(t.Context(), serveLifecycleTestTimeout)
			defer cancel()
			requirements.True(jobs.waitContext(ctx))
			assertions.Empty(refreshes)
		})
	}
}
