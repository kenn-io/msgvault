package cmd

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
	mcpserver "go.kenn.io/msgvault/internal/mcp"
	"go.kenn.io/msgvault/internal/peoplesweep"
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
		if emitErr := emit(api.CLIRunEvent{Type: "stderr", Data: "Error: " + err.Error() + "\n"}); emitErr != nil {
			return emitErr
		}
		return errors.New(cliSubprocessExitSentinel)
	}
	return emit(api.CLIRunEvent{Type: "stdout", Data: out.String()})
}

func TestPeopleSweepThroughDaemonRouterAndCobra(t *testing.T) {
	require, assert := require.New(t), assert.New(t)
	st, personID, cfg := newPersonSweepCommandStore(t, false)
	profile, err := cfg.Profile()
	require.NoError(err)
	_, err = st.StartPersonSweepRun(t.Context(), peoplesweep.StartRun{ID: "run-fixture", Kind: peoplesweep.RunManual, Mode: peoplesweep.RunIncremental, ProgramFingerprint: "program", CatalogFingerprint: "catalog", ProviderFingerprint: profile.Fingerprint, StartedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)})
	require.NoError(err)
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
	disclosure, err := backend.OperationDisclosure(t.Context(), "run_people_sweep", map[string]any{"person_id": 7, "limit": 3, "backstop": true})
	require.NoError(err)
	assert.JSONEq(`{"operation":"run_people_sweep","person_id":7,"limit":3,"backstop":true,"scope":"Run people maintenance with the configured provider and existing consent; omitted limit uses the daemon default."}`, disclosure)
	result, err := backend.ExecuteOperation(t.Context(), "get_people_sweep_status", nil)
	require.NoError(err)
	require.False(result.IsError)
	status, ok := result.Output.(*mcpserver.SweepStatus)
	require.True(ok)
	assert.Equal(cfg.Enabled, status.Enabled)
	assert.Equal(cfg.Schedule, status.Schedule)
	assert.Empty(fixture.request.Env)
	assert.Empty(fixture.request.Cwd)
	result, err = backend.ExecuteOperation(t.Context(), "list_people_sweep_history", map[string]any{"limit": 2})
	require.NoError(err)
	require.False(result.IsError)
	history, ok := result.Output.(*mcpserver.SweepHistory)
	require.True(ok)
	require.Len(history.Runs, 1)
	assert.Equal(string(peoplesweep.RunManual), history.Runs[0].Kind)
	assert.Equal(string(peoplesweep.RunIncremental), history.Runs[0].Mode)
	assert.Empty(history.Attempts)
	result, err = backend.ExecuteOperation(t.Context(), "list_people_sweep_history", map[string]any{"provider": "default"})
	require.NoError(err)
	require.False(result.IsError)
	filtered, ok := result.Output.(*mcpserver.SweepHistory)
	require.True(ok)
	require.Len(filtered.Runs, 1)
	assert.Equal(history.Runs[0], filtered.Runs[0])
	assert.Empty(fixture.request.Env)
	result, err = backend.ExecuteOperation(t.Context(), "list_people_sweep_history", map[string]any{"provider": "missing"})
	require.NoError(err)
	assert.True(result.IsError)
	result, err = backend.ExecuteOperation(t.Context(), "run_people_sweep", map[string]any{"person_id": personID})
	require.NoError(err)
	require.True(result.IsError)
	encoded, err := json.Marshal(result.Output)
	require.NoError(err)
	assert.JSONEq(`{"error":"person_not_tracked","operation_may_have_completed":false}`, string(encoded))
	result, err = backend.ExecuteOperation(t.Context(), "run_people_sweep", map[string]any{"limit": cfg.WorkBatchSize + 1})
	require.NoError(err)
	require.True(result.IsError)
	encoded, err = json.Marshal(result.Output)
	require.NoError(err)
	assert.JSONEq(`{"error":"invalid_limit","operation_may_have_completed":false}`, string(encoded))
	cfg.Enabled = false
	fixture.deps = localPersonSweepCommandDeps(cfg, st)
	result, err = backend.ExecuteOperation(t.Context(), "run_people_sweep", map[string]any{"limit": 1})
	require.NoError(err)
	require.True(result.IsError)
	encoded, err = json.Marshal(result.Output)
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
	for _, tc := range []struct {
		stderr, want string
		uncertain    bool
	}{
		{"Error: --limit must be between 1 and 25", "invalid_limit", false},
		{"Error: person 42 is not tracked", "person_not_tracked", false},
		{"Error: person 42 is not tracked elsewhere", "operation_refused", true},
	} {
		t.Run(tc.want+tc.stderr, func(t *testing.T) {
			require, assert := require.New(t), assert.New(t)
			client := newMCPDaemonClient(t, func(w http.ResponseWriter, r *http.Request) {
				_, _ = fmt.Fprintf(w, "{\"type\":\"stderr\",\"data\":%q}\n{\"type\":\"error\",\"error\":%q}\n", tc.stderr, cliSubprocessExitSentinel)
			})
			backend := newDaemonMCPOperations(client, nil)
			backend.supported = []string{"run_people_sweep"}
			result, err := backend.ExecuteOperation(t.Context(), "run_people_sweep", nil)
			require.NoError(err)
			require.True(result.IsError)
			data, err := json.Marshal(result.Output)
			require.NoError(err)
			assert.JSONEq(fmt.Sprintf(`{"error":%q,"operation_may_have_completed":%t}`, tc.want, tc.uncertain), string(data))
		})
	}
}
