package cmd

import (
	"context"
	"encoding/json/v2"
	"log/slog"
	"net/http"
	"net/http/httptest"
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

func TestMCPOperationFailureCompletion(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name, method, body, want string
		status                   int
	}{
		{"post-commit credential conflict", http.MethodPatch, `{"error":"credential_conflict"}`,
			`{"error":"operation_refused","operation_may_have_completed":true}`, http.StatusPreconditionFailed},
		{"unclassified server failure", http.MethodPost, `{"error":"settings_write_failed"}`,
			`{"error":"operation_refused","operation_may_have_completed":true}`, http.StatusInternalServerError},
		{"known pre-commit conflict", http.MethodPatch, `{"error":"settings_conflict"}`,
			`{"error":"settings_conflict","operation_may_have_completed":false}`, http.StatusPreconditionFailed},
		{"read failure", http.MethodGet, `{"error":"credential_conflict"}`,
			`{"error":"operation_refused","operation_may_have_completed":false}`, http.StatusPreconditionFailed},
		{"read validation failure", http.MethodGet, `{"error":"validation_failed"}`,
			`{"error":"validation_failed","operation_may_have_completed":false}`, http.StatusUnprocessableEntity},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			requirements, assertions := require.New(t), assert.New(t)
			client := newMCPDaemonClient(t, func(w http.ResponseWriter, r *http.Request) {
				assertions.Equal(tt.method, r.Method)
				assertions.Equal("/api/v1/settings", r.URL.Path)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(tt.body))
			})
			result, err := readMCPOperationJSON[generated.SettingsResponse](t.Context(), client, tt.method, "/api/v1/settings", nil, http.StatusOK)
			requirements.NoError(err)
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
	capabilities, err := client.MCPCapabilities(context.Background())
	requirements.NoError(err)
	backend := newDaemonMCPOperations(client, capabilities)
	assertions.Contains(backend.capabilities(), "get_source_scheduler_status")
	assertions.Contains(backend.capabilities(), "sync_source")
	result, err := backend.ExecuteOperation(context.Background(), "get_source_scheduler_status", map[string]any{})
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
	capabilities, err := client.MCPCapabilities(context.Background())
	requirements.NoError(err)
	backend := newDaemonMCPOperations(client, capabilities)
	result, err := backend.ExecuteOperation(context.Background(), "get_source_scheduler_status", map[string]any{})
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
	capabilities, err := client.MCPCapabilities(context.Background())
	requirements.NoError(err)
	backend := newDaemonMCPOperations(client, capabilities)
	args := map[string]any{"account": "sender@example.com", "source_type": "gmail"}
	disclosure, err := backend.OperationDisclosure(context.Background(), "sync_source", args)
	requirements.NoError(err)
	assertions.Contains(disclosure, "sender@example.com")
	select {
	case <-started:
		assertions.Fail("disclosure started synchronization")
	default:
	}
	result, err := backend.ExecuteOperation(context.Background(), "sync_source", args)
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
