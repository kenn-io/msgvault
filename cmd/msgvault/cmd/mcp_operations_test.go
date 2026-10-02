package cmd

import (
	"context"
	"encoding/json/v2"
	"log/slog"
	"net/http/httptest"
	"testing"
	"time"

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

func TestDaemonMCPLaunchDelegatedDiscoveryFailureKeepsOwnerToolsHidden(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	fixture := api.NewServerWithOptions(api.ServerOptions{Config: &config.Config{}, Logger: slog.New(slog.DiscardHandler)})
	server := httptest.NewServer(fixture.Router())
	t.Cleanup(server.Close)
	client, err := daemonclient.New(daemonclient.Config{URL: server.URL, AgentToken: "synthetic-invalid-grant", AllowInsecure: true})
	requirements.NoError(err)
	opts := daemonMCPServeOptions(t.Context(), client, nil)
	assertions.True(opts.DelegatedOnly)
	assertions.Empty(opts.OperationCapabilities)
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
