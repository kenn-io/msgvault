package cmd

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
	mcpserver "go.kenn.io/msgvault/internal/mcp"
)

type sweepCLIFixture struct {
	*storeAPIAdapter
	deps    personSweepCommandDeps
	request api.CLIRunRequest
}

func (f *sweepCLIFixture) RunCLICommand(ctx context.Context, req api.CLIRunRequest, emit func(api.CLIRunEvent) error) error {
	f.request = req
	root := &cobra.Command{Use: "msgvault", SilenceErrors: true, SilenceUsage: true}
	person := &cobra.Command{Use: "person"}
	person.AddCommand(newPersonSweepCommand(f.deps))
	person.AddCommand(newPersonProviderCommand(localPersonProviderDeps(f.deps.config(), f.store, nil)))
	root.AddCommand(person)
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(req.Args)
	if err := root.ExecuteContext(ctx); err != nil {
		return err
	}
	return emit(api.CLIRunEvent{Type: "stdout", Data: out.String()})
}

func TestPeopleSweepThroughDaemonRouterAndCobra(t *testing.T) {
	require, assert := require.New(t), assert.New(t)
	st, _, cfg := newPersonSweepCommandStore(t, false)
	deps := localPersonSweepCommandDeps(cfg, st)
	fixture := &sweepCLIFixture{storeAPIAdapter: &storeAPIAdapter{store: st}, deps: deps}
	conf := config.NewDefaultConfig()
	conf.Data.DataDir = t.TempDir()
	server := httptest.NewServer(api.NewServerWithOptions(api.ServerOptions{Config: conf, Store: fixture, Logger: slog.New(slog.DiscardHandler)}).Router())
	t.Cleanup(server.Close)
	client, err := daemonclient.New(daemonclient.Config{URL: server.URL, AllowInsecure: true})
	require.NoError(err)
	caps, err := client.MCPCapabilities(t.Context())
	require.NoError(err)
	backend := newDaemonMCPOperations(client, caps)
	result, err := backend.ExecuteOperation(t.Context(), "get_people_sweep_status", nil)
	require.NoError(err)
	require.False(result.IsError)
	status, ok := result.Output.(*mcpserver.SweepStatus)
	require.True(ok)
	assert.Equal(cfg.Enabled, status.Enabled)
	assert.Empty(fixture.request.Env)
	assert.Empty(fixture.request.Cwd)
	result, err = backend.ExecuteOperation(t.Context(), "list_people_sweep_history", map[string]any{"limit": 2})
	require.NoError(err)
	require.False(result.IsError)
	history, ok := result.Output.(*mcpserver.SweepHistory)
	require.True(ok)
	assert.Empty(history.Runs)
	assert.Empty(history.Attempts)
	assert.Empty(fixture.request.Env)
	result, err = backend.ExecuteOperation(t.Context(), "list_people_sweep_history", map[string]any{"provider": "default"})
	require.NoError(err)
	require.False(result.IsError)
	assert.Empty(fixture.request.Env)
	result, err = backend.ExecuteOperation(t.Context(), "list_people_sweep_history", map[string]any{"provider": "missing"})
	require.NoError(err)
	assert.True(result.IsError)
	cfg.Enabled = false
	fixture.deps = localPersonSweepCommandDeps(cfg, st)
	result, err = backend.ExecuteOperation(t.Context(), "run_people_sweep", map[string]any{"limit": 1})
	require.NoError(err)
	require.True(result.IsError)
	encoded, err := json.Marshal(result.Output)
	require.NoError(err)
	assert.JSONEq(`{"error":"people_sweep_disabled","operation_may_have_completed":false}`, string(encoded))
	assert.Empty(fixture.request.Env)
}

func TestPeopleSweepFixedArgumentsAndSafeErrors(t *testing.T) {
	require, assert := require.New(t), assert.New(t)
	cmd, pos, _, err := sweepCommand("list_people_sweep_history", map[string]any{"provider": "example", "person_id": int64(2), "limit": 3})
	require.NoError(err)
	args, err := daemonCLIArgsFromCobra(cmd, pos)
	require.NoError(err)
	assert.Equal([]string{"person", "provider", "history", "--json", "--limit=3", "--person=2", "example"}, args)
	_, _, _, err = sweepCommand("run_people_sweep", map[string]any{"person_id": 0})
	require.Error(err)
	_, _, _, err = sweepCommand("list_people_sweep_history", map[string]any{"limit": 201})
	require.Error(err)
	_, _, _, err = sweepCommand("list_people_sweep_history", map[string]any{"provider": ""})
	require.Error(err)
	_, _, _, err = sweepCommand("list_people_sweep_history", map[string]any{"provider": "--bad"})
	require.Error(err)
	client := newMCPDaemonClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":"operation_in_progress","message":"private diagnostic"}`))
	})
	backend := newDaemonMCPOperations(client, nil)
	backend.supported = []string{"get_people_sweep_status"}
	result, err := backend.ExecuteOperation(t.Context(), "get_people_sweep_status", nil)
	require.NoError(err)
	require.True(result.IsError)
	data, err := json.Marshal(result.Output)
	require.NoError(err)
	assert.JSONEq(`{"error":"operation_in_progress","operation_may_have_completed":false}`, string(data))
}
