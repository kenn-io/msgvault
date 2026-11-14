package cmd

import (
	"context"
	"encoding/json/v2"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
	mcpserver "go.kenn.io/msgvault/internal/mcp"
	"go.kenn.io/msgvault/internal/scheduler"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func TestMCPOperationResponseFailures(t *testing.T) {
	t.Parallel()
	settings := func(ctx context.Context, client *daemonclient.Client, method, path string) (*mcpserver.OperationResult, error) {
		result, err := readMCPOperationJSON[slackSettingsResponse](ctx, client, method, path, nil, http.StatusOK)
		return slackPolicyProjection(result, true), err
	}
	person := func(ctx context.Context, client *daemonclient.Client, method, path string) (*mcpserver.OperationResult, error) {
		return readMCPOperationJSON[generated.Person](ctx, client, method, path, nil, http.StatusOK)
	}
	for _, tt := range []struct {
		name, method, body, want, path string
		status                         int
		interrupted, wantErr           bool
		read                           func(context.Context, *daemonclient.Client, string, string) (*mcpserver.OperationResult, error)
	}{
		{"post-commit credential conflict", http.MethodPatch, `{"error":"credential_conflict"}`,
			`{"error":"operation_refused","operation_may_have_completed":true}`, "/api/v1/settings", http.StatusPreconditionFailed, false, false, settings},
		{"unclassified server failure", http.MethodPost, `{"error":"settings_write_failed"}`,
			`{"error":"operation_refused","operation_may_have_completed":true}`, "/api/v1/settings", http.StatusInternalServerError, false, false, settings},
		{"known pre-commit conflict", http.MethodPatch, `{"error":"settings_conflict"}`,
			`{"error":"settings_conflict","operation_may_have_completed":false}`, "/api/v1/settings", http.StatusPreconditionFailed, false, false, settings},
		{"read failure", http.MethodGet, `{"error":"credential_conflict"}`,
			`{"error":"operation_refused","operation_may_have_completed":false}`, "/api/v1/settings", http.StatusPreconditionFailed, false, false, settings},
		{"oversized successful write receipt", http.MethodPatch, `{"data":"` + strings.Repeat("x", 1<<20) + `"}`,
			`{"error":"operation_receipt_unavailable","operation_may_have_completed":true}`, "/api/v1/settings", http.StatusOK, false, false, settings},
		{"interrupted write receipt", http.MethodPatch, `{"partial":`,
			`{"error":"operation_receipt_unavailable","operation_may_have_completed":true}`, "/api/v1/settings", http.StatusOK, true, true, settings},
		{"interrupted error response", http.MethodPatch, `{"error":`,
			`{"error":"operation_response_failed","operation_may_have_completed":true}`, "/api/v1/settings", http.StatusInternalServerError, true, true, settings},
		{"person receipt without participant list", http.MethodPatch, `{"id":1,"created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z","vcard_uid":"example"}`,
			`{"error":"operation_receipt_unavailable","operation_may_have_completed":true}`, "/api/v1/people/1", http.StatusOK, false, false, person},
		{"malformed write receipt", http.MethodPatch, `not json`,
			`{"error":"operation_receipt_unavailable","operation_may_have_completed":true}`, "/api/v1/settings", http.StatusOK, false, true, settings},
		{"oversized read", http.MethodGet, strings.Repeat("x", (1<<20)+1),
			`{"error":"operation_response_limit_exceeded","operation_may_have_completed":false}`, "/api/v1/settings", http.StatusOK, false, false, settings},
		{"read validation failure", http.MethodGet, `{"error":"validation_failed"}`,
			`{"error":"validation_failed","operation_may_have_completed":false}`, "/api/v1/settings", http.StatusUnprocessableEntity, false, false, settings},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			requirements, assertions := require.New(t), assert.New(t)
			client := newMCPDaemonClient(t, func(w http.ResponseWriter, r *http.Request) {
				assertions.Equal(tt.method, r.Method)
				assertions.Equal(tt.path, r.URL.Path)
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("ETag", `"settings"`)
				if tt.interrupted {
					w.Header().Set("Content-Length", "100")
				}
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(tt.body))
			})
			result, err := tt.read(t.Context(), client, tt.method, tt.path)
			if tt.wantErr {
				requirements.Error(err)
			} else {
				requirements.NoError(err)
			}
			requirements.NotNil(result)
			assertions.True(result.IsError)
			encoded, err := json.Marshal(result.Output)
			requirements.NoError(err)
			assertions.JSONEq(tt.want, string(encoded))
		})
	}
}

func TestDaemonMCPLaunchDiscoversRealOperationalTools(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	fixture := api.NewServerWithOptions(api.ServerOptions{Config: &config.Config{}, Logger: slog.New(slog.DiscardHandler)})
	server := httptest.NewServer(fixture.Router())
	t.Cleanup(server.Close)
	client, err := daemonclient.New(daemonclient.Config{URL: server.URL, AllowInsecure: true})
	requirements.NoError(err)
	opts := daemonMCPServeOptions(t.Context(), client, nil)
	requirements.NotNil(opts.Operations)
	assertions.Contains(opts.OperationCapabilities, "get_source_scheduler_status")
	assertions.Empty(opts.OperationWriteFamilies)
	assertions.False(opts.DelegatedOnly)
}

func TestDaemonMCPOperationsFailClosedWhenDiscoveryIsAbsent(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	backend := newDaemonMCPOperations(nil, nil)
	assertions.Empty(backend.capabilities())
	result, err := backend.ExecuteOperation(t.Context(), "get_source_scheduler_status", nil)
	requirements.NoError(err)
	assertions.True(result.IsError)
	_, err = backend.OperationDisclosure(t.Context(), "sync_source", nil)
	var refusal *mcpserver.OperationRefusalError
	requirements.ErrorAs(err, &refusal)
	assertions.Equal("operation_not_supported", refusal.Code)
}

func TestDaemonMCPOperationsDiscoverAndReadRealScheduler(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	st := testutil.NewTestStore(t)
	sched := scheduler.New(func(context.Context, string) error { return nil })
	requirements.NoError(sched.AddAccount("sender@example.com", "0 2 * * *"))
	fixture := api.NewServerWithOptions(api.ServerOptions{Config: &config.Config{}, Store: &storeAPIAdapter{store: st}, Scheduler: &schedulerAdapter{scheduler: sched}, Logger: slog.New(slog.DiscardHandler)})
	server := httptest.NewServer(fixture.Router())
	t.Cleanup(server.Close)
	client, err := daemonclient.New(daemonclient.Config{URL: server.URL, AllowInsecure: true})
	requirements.NoError(err)
	capabilities, err := client.MCPCapabilities(t.Context())
	requirements.NoError(err)
	backend := newDaemonMCPOperations(client, capabilities)
	assertions.Contains(backend.capabilities(), "get_source_scheduler_status")
	assertions.Contains(backend.capabilities(), "sync_source")
	result, err := backend.ExecuteOperation(t.Context(), "get_source_scheduler_status", map[string]any{})
	requirements.NoError(err)
	requirements.NotNil(result)
	assertions.False(result.IsError)
	encoded, err := json.Marshal(result.Output)
	requirements.NoError(err)
	var status generated.SchedulerStatusResponse
	requirements.NoError(json.Unmarshal(encoded, &status))
	requirements.Len(status.Accounts, 1)
	assertions.Equal("sender@example.com", status.Accounts[0].Email)
	assertions.False(status.Running)
}

func TestDaemonMCPOperationsPreserveRealRefusalWithoutErrorText(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	fixture := api.NewServerWithOptions(api.ServerOptions{Config: &config.Config{}, Logger: slog.New(slog.DiscardHandler)})
	server := httptest.NewServer(fixture.Router())
	t.Cleanup(server.Close)
	client, err := daemonclient.New(daemonclient.Config{URL: server.URL, AllowInsecure: true})
	requirements.NoError(err)
	capabilities, err := client.MCPCapabilities(t.Context())
	requirements.NoError(err)
	backend := newDaemonMCPOperations(client, capabilities)
	result, err := backend.ExecuteOperation(t.Context(), "get_source_scheduler_status", map[string]any{})
	requirements.NoError(err)
	requirements.NotNil(result)
	assertions.True(result.IsError)
	encoded, err := json.Marshal(result.Output)
	requirements.NoError(err)
	assertions.Contains(string(encoded), "scheduler_unavailable")
	assertions.NotContains(string(encoded), "Scheduler not available")
}

func TestDaemonMCPOperationsRealSyncAcceptance(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	st := testutil.NewTestStore(t)
	_, err := st.GetOrCreateSource("gmail", "sender@example.com")
	requirements.NoError(err)
	started := make(chan string, 1)
	sched := scheduler.New(func(_ context.Context, account string) error { started <- account; return nil })
	requirements.NoError(sched.AddAccount("sender@example.com", "0 2 * * *"))
	t.Cleanup(func() { sched.Stop() })
	fixture := api.NewServerWithOptions(api.ServerOptions{Config: &config.Config{}, Store: &storeAPIAdapter{store: st}, Scheduler: &schedulerAdapter{scheduler: sched}, Logger: slog.New(slog.DiscardHandler)})
	server := httptest.NewServer(fixture.Router())
	t.Cleanup(server.Close)
	client, err := daemonclient.New(daemonclient.Config{URL: server.URL, AllowInsecure: true})
	requirements.NoError(err)
	capabilities, err := client.MCPCapabilities(t.Context())
	requirements.NoError(err)
	backend := newDaemonMCPOperations(client, capabilities)
	args := map[string]any{"account": "sender@example.com", "source_type": "gmail"}
	disclosure, err := backend.OperationDisclosure(t.Context(), "sync_source", args)
	requirements.NoError(err)
	assertions.Contains(disclosure, "sender@example.com")
	select {
	case <-started:
		assertions.Fail("disclosure started synchronization")
	default:
	}
	result, err := backend.ExecuteOperation(t.Context(), "sync_source", args)
	requirements.NoError(err)
	requirements.NotNil(result)
	assertions.False(result.IsError)
	select {
	case account := <-started:
		assertions.Equal("sender@example.com", account)
	case <-time.After(10 * time.Second):
		requirements.FailNow("accepted scheduled sync did not start")
	}
}

func TestMCPSyncSourceConfirmsRunningAndQueuedJobs(t *testing.T) {
	for _, queued := range []bool{false, true} {
		name := "running"
		if queued {
			name = "queued"
		}
		t.Run(name, func(t *testing.T) {
			requirements, assertions := require.New(t), assert.New(t)
			st := testutil.NewTestStore(t)
			_, err := st.GetOrCreateSource("granola", "workspace")
			requirements.NoError(err)
			gate := api.NewSerialOperationGate()
			release := func() {}
			if queued {
				var ok bool
				release, ok = gate.BeginWork()
				requirements.True(ok)
			}
			started := make(chan struct{}, 1)
			sched := scheduler.New(func(context.Context, string) error { return nil }).WithWorkTracker(gate)
			requirements.NoError(sched.AddJob(scheduler.Job{
				Name: "granola:workspace", Schedule: "0 2 * * *",
				Run: func(ctx context.Context) error {
					started <- struct{}{}
					<-ctx.Done()
					return ctx.Err()
				},
			}))
			t.Cleanup(func() { release(); <-sched.Stop().Done() })
			backend := sourceOperationFixture(t, api.NewServerWithOptions(api.ServerOptions{
				Config: &config.Config{}, Store: &storeAPIAdapter{store: st},
				Scheduler: &schedulerAdapter{scheduler: sched}, Logger: slog.New(slog.DiscardHandler),
			}))
			var approvals atomic.Int32
			session := operationMCPSession(t, backend, []mcpserver.OperationFamily{mcpserver.OperationFamilySources}, &sdkmcp.ClientOptions{
				ElicitationHandler: func(context.Context, *sdkmcp.ElicitRequest) (*sdkmcp.ElicitResult, error) {
					approvals.Add(1)
					return &sdkmcp.ElicitResult{Action: "accept", Content: map[string]any{"confirm": true}}, nil
				},
			})
			want := []string{"started", "pending", "coalesced"}
			if queued {
				want = []string{"started", "coalesced"}
			}
			for i, disposition := range want {
				result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "sync_source", Arguments: map[string]any{
					"account": "workspace", "source_type": "granola",
				}})
				requirements.NoError(err)
				requirements.False(result.IsError, "expected %s; got %v", disposition, result.StructuredContent)
				data, err := json.Marshal(result.StructuredContent)
				requirements.NoError(err)
				var receipt generated.TriggerSyncResponse
				requirements.NoError(json.Unmarshal(data, &receipt))
				assertions.Equal("accepted", receipt.Status)
				assertions.Equal(new(disposition), receipt.Disposition)
				assertions.Equal(int32(i+1), approvals.Load())
				if i == 0 && !queued {
					select {
					case <-started:
					case <-time.After(10 * time.Second):
						requirements.FailNow("accepted generic sync did not start")
					}
				}
			}
			status := sched.JobStatus()
			requirements.Len(status, 1)
			assertions.Equal(queued, status[0].Queued)
			assertions.Equal(!queued, status[0].Pending)
		})
	}
}

func TestMCPSyncSourceRejectsUnavailableSourcesBeforeConfirmation(t *testing.T) {
	for _, tc := range []struct {
		name, sourceType string
		active           bool
	}{
		{"unconfigured generic", "granola", false},
		{"unconfigured active generic", "granola", true},
		{"unsupported import", "mbox", false},
		{"busy account", "gmail", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requirements, assertions := require.New(t), assert.New(t)
			st := testutil.NewTestStore(t)
			source, err := st.GetOrCreateSource(tc.sourceType, "sender@example.com")
			requirements.NoError(err)
			if tc.active {
				_, err = st.StartSync(source.ID, "incremental")
				requirements.NoError(err)
			}
			sched := scheduler.New(func(context.Context, string) error { return nil })
			t.Cleanup(func() { <-sched.Stop().Done() })
			requirements.NoError(sched.AddAccount("sender@example.com", "0 2 * * *"))
			backend := sourceOperationFixture(t, api.NewServerWithOptions(api.ServerOptions{
				Config: &config.Config{}, Store: &storeAPIAdapter{store: st},
				Scheduler: &schedulerAdapter{scheduler: sched}, Logger: slog.New(slog.DiscardHandler),
			}))
			var approvals atomic.Int32
			session := operationMCPSession(t, backend, []mcpserver.OperationFamily{mcpserver.OperationFamilySources}, &sdkmcp.ClientOptions{
				ElicitationHandler: func(context.Context, *sdkmcp.ElicitRequest) (*sdkmcp.ElicitResult, error) {
					approvals.Add(1)
					return &sdkmcp.ElicitResult{Action: "accept", Content: map[string]any{"confirm": true}}, nil
				},
			})
			result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "sync_source", Arguments: map[string]any{
				"account": "sender@example.com", "source_type": tc.sourceType,
			}})
			requirements.NoError(err)
			assertions.True(result.IsError)
			assertions.Equal(map[string]any{"error": "source_not_schedulable"}, result.StructuredContent)
			assertions.Zero(approvals.Load())
		})
	}
}

func TestMCPOperationsLocalRecoveryFailure(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	server.Close()
	client, err := daemonclient.New(daemonclient.Config{URL: server.URL, AllowInsecure: true})
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, client.Close()) })
	client.EnableLocalDaemonRecovery(func(context.Context) (daemonclient.LocalDaemonEndpoint, error) {
		return daemonclient.LocalDaemonEndpoint{}, errors.New("synthetic discovery failure")
	})
	backend := &daemonMCPOperations{client: client, supported: []string{"get_source_scheduler_status", "sync_source"}}
	for _, name := range backend.supported {
		t.Run(name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			result, err := backend.ExecuteOperation(t.Context(), name, map[string]any{"account": "sender@example.com", "source_type": "gmail"})
			require.Error(err)
			require.NotNil(result)
			assert.True(result.IsError)
			data, marshalErr := json.Marshal(result.Output)
			require.NoError(marshalErr)
			assert.JSONEq(`{"error":"daemon_unavailable","operation_may_have_completed":false}`, string(data))
		})
	}
}
