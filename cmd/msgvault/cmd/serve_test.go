package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	imaplib "go.kenn.io/msgvault/internal/imap"
	"go.kenn.io/msgvault/internal/oauth"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/scheduler"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestServeConfigParsing(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	// Create temp config with scheduled accounts
	tmpDir := t.TempDir()
	configContent := `
[oauth]
client_secrets = "/path/to/secrets.json"

[server]
api_port = 9090
api_key = "test-key"

[[accounts]]
email = "user1@gmail.com"
schedule = "0 2 * * *"
enabled = true

[[accounts]]
email = "user2@gmail.com"
schedule = "0 3 * * *"
enabled = true

[[accounts]]
email = "disabled@gmail.com"
schedule = "0 4 * * *"
enabled = false
`
	configPath := filepath.Join(tmpDir, "config.toml")
	require.NoError(os.WriteFile(configPath, []byte(configContent), 0644), "write config")

	cfg, err := config.Load(configPath, "")
	require.NoError(err, "Load")

	// Verify server config
	assert.Equal(9090, cfg.Server.APIPort, "APIPort")
	assert.Equal("test-key", cfg.Server.APIKey, "APIKey")

	// Verify scheduled accounts
	scheduled := cfg.ScheduledAccounts()
	assert.Len(scheduled, 2, "len(ScheduledAccounts())")

	// Verify specific accounts
	acc := cfg.GetAccountSchedule("user1@gmail.com")
	require.NotNil(acc, "GetAccountSchedule(user1)")
	assert.Equal("0 2 * * *", acc.Schedule, "user1 schedule")

	// Disabled account should still be retrievable but not in scheduled list
	disabled := cfg.GetAccountSchedule("disabled@gmail.com")
	assert.NotNil(disabled, "GetAccountSchedule(disabled)")
}

func TestSchedulerWithConfig(t *testing.T) {
	cfg := &config.Config{
		Accounts: []config.AccountSchedule{
			{Email: "test1@gmail.com", Schedule: "0 2 * * *", Enabled: true},
			{Email: "test2@gmail.com", Schedule: "0 3 * * *", Enabled: true},
			{Email: "test3@gmail.com", Schedule: "invalid", Enabled: true},
		},
	}

	var syncCalls []string
	sched := scheduler.New(func(ctx context.Context, email string) error {
		syncCalls = append(syncCalls, email)
		return nil
	})

	count, errs := sched.AddAccountsFromConfig(cfg)

	// Should schedule 2 valid accounts
	assert.Equal(t, 2, count, "scheduled count")

	// Should have 1 error for invalid cron
	assert.Len(t, errs, 1, "len(errs)")

	// Verify status
	statuses := sched.Status()
	assert.Len(t, statuses, 2, "len(Status())")
}

func TestServeCmdNoAccounts(t *testing.T) {
	// Create temp config without accounts
	tmpDir := t.TempDir()
	configContent := `
[oauth]
client_secrets = "/path/to/secrets.json"
`
	configPath := filepath.Join(tmpDir, "config.toml")
	require.NoError(t, os.WriteFile(configPath, []byte(configContent), 0644), "write config")

	cfg, err := config.Load(configPath, "")
	require.NoError(t, err, "Load")

	scheduled := cfg.ScheduledAccounts()
	assert.Empty(t, scheduled, "expected no scheduled accounts")
}

func TestServeOAuthValidationAllowsMicrosoftOnly(t *testing.T) {
	assert.True(t, hasServeOAuthConfig(&config.Config{
		Microsoft: config.MicrosoftConfig{ClientID: "azure-client-id"},
	}))
}

func TestServeOAuthValidationReportsNoProviders(t *testing.T) {
	assert.False(t, hasServeOAuthConfig(&config.Config{}))
}

func TestRunServeStartsReadOnlyWithoutOAuthConfig(t *testing.T) {
	oldCfg := cfg
	dataDir := t.TempDir()
	cfg = lifecycleTestConfig(dataDir)
	cfg.Server.APIPort = freeTCPPort(t)
	t.Cleanup(func() { cfg = oldCfg })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := &cobra.Command{Use: "serve"}
	cmd.SetContext(ctx)
	errCh := make(chan error, 1)
	go func() {
		errCh <- runServe(cmd, nil)
	}()

	waitForServeHealth(t, cfg.Server.APIPort, errCh)
	cancel()

	select {
	case err := <-errCh:
		require.NoError(t, err, "runServe")
	case <-time.After(5 * time.Second):
		require.FailNow(t, "runServe did not stop after context cancellation")
	}
}

func TestRunServeAutoSelectsAPIPortWhenUnconfigured(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	oldCfg := cfg
	dataDir := t.TempDir()
	cfg = lifecycleTestConfig(dataDir)
	cfg.Server.APIPort = 0 // auto-select an open port
	t.Cleanup(func() { cfg = oldCfg })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := &cobra.Command{Use: "serve"}
	cmd.SetContext(ctx)
	errCh := make(chan error, 1)
	go func() {
		errCh <- runServe(cmd, nil)
	}()

	// Discover the auto-selected port the same way clients do: through the
	// daemon runtime record, not the configured port (which is 0).
	rt, ready, err := waitForDaemonRuntime(ctx, dataDir, 5*time.Second, daemonRuntimeReady, errCh)
	require.NoError(err, "wait for daemon runtime record")
	require.True(ready, "daemon runtime record did not become ready")
	assert.NotZero(rt.Port, "runtime record must record the bound ephemeral port")

	url := fmt.Sprintf("http://%s/health", net.JoinHostPort(rt.Host, strconv.Itoa(rt.Port)))
	resp, err := http.Get(url) //nolint:gosec // local test server
	require.NoError(err, "GET /health on discovered address")
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(http.StatusOK, resp.StatusCode, "/health status")

	cancel()
	select {
	case err := <-errCh:
		require.NoError(err, "runServe")
	case <-time.After(5 * time.Second):
		require.FailNow("runServe did not stop after context cancellation")
	}
}

func TestRunServeFailsBeforeArchiveWorkWhenAPIPortInUse(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	ln, err := net.Listen("tcp", net.JoinHostPort(defaultDaemonBindAddr, "0"))
	require.NoError(err, "reserve API port")
	t.Cleanup(func() { _ = ln.Close() })
	addr, ok := ln.Addr().(*net.TCPAddr)
	require.True(ok, "listener address must be TCP")

	oldCfg := cfg
	dataDir := t.TempDir()
	cfg = lifecycleTestConfig(dataDir)
	cfg.Server.APIPort = addr.Port
	t.Cleanup(func() { cfg = oldCfg })

	cmd := &cobra.Command{Use: "serve"}
	cmd.SetContext(context.Background())
	err = runServe(cmd, nil)

	require.Error(err, "runServe")
	assert.Contains(err.Error(), "API server address unavailable")
	assert.Contains(err.Error(), net.JoinHostPort(defaultDaemonBindAddr, strconv.Itoa(addr.Port)))
	assert.NoFileExists(filepath.Join(dataDir, "msgvault.db"), "serve must not touch the archive when the API port is unavailable")
}

type recordingServeAPIServer struct {
	events *[]string
}

func (s recordingServeAPIServer) Shutdown(context.Context) error {
	*s.events = append(*s.events, "api-shutdown")
	return nil
}

type recordingServeScheduler struct {
	events *[]string
	ctx    context.Context
}

func (s recordingServeScheduler) Stop() context.Context {
	*s.events = append(*s.events, "scheduler-stop")
	return s.ctx
}

type recordingServeGate struct {
	events *[]string
}

func (g recordingServeGate) StartDrain() {
	*g.events = append(*g.events, "gate-start-drain")
}

func (g recordingServeGate) Wait(context.Context) error {
	*g.events = append(*g.events, "gate-wait")
	return nil
}

func TestShutdownServeRuntimeDrainsGateAroundHTTPAndScheduler(t *testing.T) {
	doneCtx, done := context.WithCancel(context.Background())
	done()
	events := []string{}

	err := shutdownServeRuntime(
		context.Background(),
		io.Discard,
		recordingServeAPIServer{events: &events},
		recordingServeScheduler{events: &events, ctx: doneCtx},
		recordingServeGate{events: &events},
	)

	require.NoError(t, err, "shutdownServeRuntime")
	assert.Equal(t, []string{
		"gate-start-drain",
		"api-shutdown",
		"scheduler-stop",
		"gate-wait",
	}, events, "shutdown order")
}

func freeTCPPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err, "listen on free port")
	defer func() { require.NoError(t, ln.Close(), "close listener") }()
	addr, ok := ln.Addr().(*net.TCPAddr)
	require.True(t, ok, "listener address must be TCP")
	return addr.Port
}

func waitForServeHealth(t *testing.T, port int, errCh <-chan error) {
	t.Helper()
	url := fmt.Sprintf("http://127.0.0.1:%d/health", port)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-errCh:
			require.NoError(t, err, "runServe exited before health was ready")
			require.FailNow(t, "runServe exited before health was ready")
		default:
		}
		resp, err := http.Get(url) //nolint:gosec // local test server
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	require.FailNow(t, "serve health endpoint did not become ready")
}

func TestRunDaemonSQLQueryRebuildsStaleCacheOutOfProcess(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	dataDir := t.TempDir()
	c := lifecycleTestConfig(dataDir)
	s, err := store.Open(c.DatabaseDSN())
	require.NoError(err, "open store")
	defer func() { _ = s.Close() }()
	require.NoError(s.InitSchema(), "init schema")
	engine := query.NewEngine(s.DB(), false)
	defer func() { _ = engine.Close() }()

	sentinel := errors.New("subprocess sentinel")
	var called bool
	var gotFullRebuild bool
	old := buildCacheSubprocessForRun
	buildCacheSubprocessForRun = func(_ context.Context, fullRebuild bool) error {
		called = true
		gotFullRebuild = fullRebuild
		return sentinel
	}
	t.Cleanup(func() { buildCacheSubprocessForRun = old })

	_, err = runDaemonSQLQuery(context.Background(), c, s, engine, "select 1")

	require.Error(err, "query should fail with subprocess sentinel")
	require.ErrorIs(err, sentinel, "error")
	assert.True(called, "subprocess rebuild should be called")
	assert.True(gotFullRebuild, "missing cache should request full rebuild")
}

func TestOpenDaemonAnalyticsEngineForceSQLSkipsCacheBuild(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	c, s := openTestDaemonAnalyticsStore(t)
	c.Analytics.Engine = config.AnalyticsEngineSQL
	c.Analytics.AutoBuildCache = true
	stubBuildCacheSubprocess(t, func(context.Context, bool) error {
		require.FailNow("engine=sql must not build analytics cache")
		return nil
	})

	engine, mode, err := openDaemonAnalyticsEngine(context.Background(), c, s)
	require.NoError(err, "openDaemonAnalyticsEngine")
	defer func() { _ = engine.Close() }()

	assert.IsType(&query.SQLiteEngine{}, engine)
	assert.Equal(api.AnalyticsModeSQL, mode, "engine=sql is a deliberate live-SQL choice")
}

func TestOpenDaemonAnalyticsEngineSkipsCacheBuildWhenDisabled(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	c, s := openTestDaemonAnalyticsStore(t)
	c.Analytics.Engine = config.AnalyticsEngineAuto
	c.Analytics.AutoBuildCache = false
	stubBuildCacheSubprocess(t, func(context.Context, bool) error {
		require.FailNow("auto_build_cache=false must not build analytics cache")
		return nil
	})

	engine, mode, err := openDaemonAnalyticsEngine(context.Background(), c, s)
	require.NoError(err, "openDaemonAnalyticsEngine")
	defer func() { _ = engine.Close() }()

	assert.IsType(&query.SQLiteEngine{}, engine)
	assert.Equal(api.AnalyticsModeSQLFallback, mode, "auto mode without a cache is a fallback")
}

func TestOpenDaemonAnalyticsEngineAutoDoesNotBlockStartupOnMissingCache(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	c, s := openTestDaemonAnalyticsStore(t)
	c.Analytics.Engine = config.AnalyticsEngineAuto
	c.Analytics.AutoBuildCache = true
	var gotFullRebuild bool
	stubBuildCacheSubprocess(t, func(_ context.Context, fullRebuild bool) error {
		gotFullRebuild = fullRebuild
		require.FailNow("auto mode must not block daemon startup on cache build")
		return nil
	})

	engine, mode, err := openDaemonAnalyticsEngine(context.Background(), c, s)
	require.NoError(err, "auto mode should start with live SQL when cache is missing")
	defer func() { _ = engine.Close() }()

	assert.False(gotFullRebuild, "startup should not request a synchronous cache rebuild")
	assert.IsType(&query.SQLiteEngine{}, engine)
	assert.Equal(api.AnalyticsModeSQLFallback, mode, "auto mode without a cache is a fallback")
}

func TestOpenDaemonAnalyticsEngineDuckDBRequiresCacheBuild(t *testing.T) {
	require := require.New(t)
	c, s := openTestDaemonAnalyticsStore(t)
	c.Analytics.Engine = config.AnalyticsEngineDuckDB
	c.Analytics.AutoBuildCache = true
	sentinel := errors.New("build failed")
	stubBuildCacheSubprocess(t, func(context.Context, bool) error {
		return sentinel
	})

	engine, _, err := openDaemonAnalyticsEngine(context.Background(), c, s)
	if engine != nil {
		_ = engine.Close()
	}

	require.Error(err, "duckdb mode should fail when the required cache build fails")
	require.ErrorIs(err, sentinel, "error")
}

func openTestDaemonAnalyticsStore(t *testing.T) (*config.Config, *store.Store) {
	t.Helper()
	c := lifecycleTestConfig(t.TempDir())
	s, err := store.Open(c.DatabaseDSN())
	require.NoError(t, err, "open store")
	t.Cleanup(func() { _ = s.Close() })
	require.NoError(t, s.InitSchema(), "init schema")
	return c, s
}

func stubBuildCacheSubprocess(
	t *testing.T,
	fn func(context.Context, bool) error,
) {
	t.Helper()
	old := buildCacheSubprocessForRun
	buildCacheSubprocessForRun = fn
	t.Cleanup(func() { buildCacheSubprocessForRun = old })
}

func TestStoreAPIAdapterServesSourceStatus(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	tmpDir := t.TempDir()

	s, err := store.Open(filepath.Join(tmpDir, "msgvault.db"))
	require.NoError(err, "open store")
	defer func() { _ = s.Close() }()
	require.NoError(s.InitSchema(), "init schema")

	source, err := s.GetOrCreateSource("gmail", "alice@example.com")
	require.NoError(err, "create source")
	require.NoError(s.UpdateSourceDisplayName(source.ID, "Alice"), "set display name")
	require.NoError(s.UpdateSourceSyncCursor(source.ID, "history-1"), "set sync cursor")

	completedID, err := s.StartSync(source.ID, "full")
	require.NoError(err, "start sync")
	require.NoError(s.UpdateSyncCheckpoint(completedID, &store.Checkpoint{
		MessagesProcessed: 3,
		MessagesAdded:     2,
		MessagesUpdated:   1,
	}), "update checkpoint")
	require.NoError(s.CompleteSync(completedID, "history-2"), "complete sync")

	adapter := &storeAPIAdapter{store: s}
	srv := api.NewServer(
		&config.Config{Server: config.ServerConfig{APIPort: 8080}},
		adapter,
		nil,
		slog.New(slog.DiscardHandler),
	)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/sources/status?source_type=gmail", nil)
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, req)

	require.Equal(http.StatusOK, w.Code, "body: %s", w.Body.String())

	var resp api.SourceStatusResponse
	require.NoError(json.NewDecoder(w.Body).Decode(&resp), "decode response")
	require.Len(resp.Sources, 1, "sources")

	got := resp.Sources[0]
	assert.Equal(source.ID, got.ID, "ID")
	assert.Equal("gmail", got.SourceType, "SourceType")
	assert.Equal("alice@example.com", got.Identifier, "Identifier")
	require.NotNil(got.DisplayName, "DisplayName")
	assert.Equal("Alice", *got.DisplayName, "DisplayName")
	assert.Nil(got.ActiveSync, "ActiveSync")
	require.NotNil(got.LatestSync, "LatestSync")
	assert.Equal(completedID, got.LatestSync.ID, "LatestSync.ID")
	require.NotNil(got.LastSuccessfulSync, "LastSuccessfulSync")
	assert.Equal(completedID, got.LastSuccessfulSync.ID, "LastSuccessfulSync.ID")
	assert.Equal(store.SyncStatusCompleted, got.LastSuccessfulSync.Status, "LastSuccessfulSync.Status")
	require.NotNil(got.LastSuccessfulSync.CursorAfter, "LastSuccessfulSync.CursorAfter")
	assert.Equal("history-2", *got.LastSuccessfulSync.CursorAfter, "LastSuccessfulSync.CursorAfter")
}

func TestStoreAPIAdapterRunCLISyncPacksOnlyAfterSubprocessSuccess(t *testing.T) {
	tests := []struct {
		name           string
		predecessorErr error
		wantPacked     bool
		wantAttempts   int
	}{
		{name: "success", wantPacked: true, wantAttempts: 1},
		{name: "failure", predecessorErr: errors.New("sync subprocess failed")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			f := newAttachmentMaintenanceFixture(t)
			hash := f.addLoose([]byte("CLI sync attachment payload"))
			adapter := &storeAPIAdapter{store: f.store, attachmentMaintenance: f.maintenance}
			var events []api.CLISyncEvent
			runnerCalls := 0

			err := adapter.runCLISyncWithRunner(
				context.Background(),
				api.CLISyncRequest{Email: "alice@example.com"},
				func(event api.CLISyncEvent) error {
					events = append(events, event)
					return nil
				},
				func(_ context.Context, args []string, _ func(string, string) error) error {
					runnerCalls++
					assert.Equal([]string{"sync", "alice@example.com"}, args)
					assert.Nil(f.packedEntry(hash), "packing must follow subprocess success")
					return tt.predecessorErr
				},
			)

			if tt.predecessorErr != nil {
				require.ErrorIs(err, tt.predecessorErr)
			} else {
				require.NoError(err)
			}
			assert.Equal(1, runnerCalls)
			assert.Equal(tt.wantPacked, f.packedEntry(hash) != nil)
			assert.Equal(tt.wantAttempts,
				strings.Count(f.logs.String(), "automatic attachment maintenance complete"))
			assert.Empty(events, "successful automatic maintenance writes no normal CLI output")
		})
	}
}

func TestStoreAPIAdapterRunCLICommandPacksOnlyAllowlistedSuccess(t *testing.T) {
	tests := []struct {
		name           string
		args           []string
		predecessorErr error
		wantPacked     bool
		wantAttempts   int
	}{
		{
			name:         "allowlisted success",
			args:         []string{importMboxCommand, "archive.mbox"},
			wantPacked:   true,
			wantAttempts: 1,
		},
		{
			name: "unrelated success",
			args: []string{"remove-account", "alice@example.com"},
		},
		{
			name:           "allowlisted failure",
			args:           []string{"sync-teams", "alice@example.com"},
			predecessorErr: errors.New("command subprocess failed"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			f := newAttachmentMaintenanceFixture(t)
			hash := f.addLoose([]byte("generic CLI attachment payload"))
			adapter := &storeAPIAdapter{store: f.store, attachmentMaintenance: f.maintenance}
			var events []api.CLIRunEvent
			runnerCalls := 0

			err := adapter.runCLICommandWithRunner(
				context.Background(),
				api.CLIRunRequest{Args: tt.args, Env: map[string]string{"TEST": "value"}, Cwd: "/tmp"},
				func(event api.CLIRunEvent) error {
					events = append(events, event)
					return nil
				},
				func(
					_ context.Context,
					args []string,
					env map[string]string,
					cwd string,
					_ func(string, string) error,
				) error {
					runnerCalls++
					assert.Equal(tt.args, args)
					assert.Equal(map[string]string{"TEST": "value"}, env)
					assert.Equal("/tmp", cwd)
					assert.Nil(f.packedEntry(hash), "packing must follow subprocess success")
					return tt.predecessorErr
				},
			)

			if tt.predecessorErr != nil {
				require.ErrorIs(err, tt.predecessorErr)
			} else {
				require.NoError(err)
			}
			assert.Equal(1, runnerCalls)
			assert.Equal(tt.wantPacked, f.packedEntry(hash) != nil)
			assert.Equal(tt.wantAttempts,
				strings.Count(f.logs.String(), "automatic attachment maintenance complete"))
			assert.Empty(events, "successful automatic maintenance writes no normal CLI output")
		})
	}
}

func TestStoreAPIAdapterInterceptsExplicitRepackInDaemonParent(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newAttachmentMaintenanceFixture(t)
	oldPackID := f.makeZeroLivePack([]byte("explicit parent repack dead bytes"))
	adapter := &storeAPIAdapter{store: f.store, attachmentMaintenance: f.maintenance}
	var events []api.CLIRunEvent

	err := adapter.runCLICommandWithRunner(
		context.Background(), api.CLIRunRequest{Args: []string{"repack-attachments"}},
		func(event api.CLIRunEvent) error {
			events = append(events, event)
			return nil
		},
		func(context.Context, []string, map[string]string, string, func(string, string) error) error {
			require.FailNow("explicit repack must never spawn a child process")
			return nil
		},
	)

	require.NoError(err)
	has, err := f.store.HasPackRecord(oldPackID)
	require.NoError(err)
	assert.False(has)
	require.Len(events, 1)
	assert.Equal("stdout", events[0].Type)
	assert.Contains(events[0].Data, "removed 1 old pack(s)")
}

func TestStoreAPIAdapterExplicitRepackAcceptsLoggingPassthroughFlags(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newAttachmentMaintenanceFixture(t)
	oldPackID := f.makeZeroLivePack([]byte("explicit repack with root logging flags"))
	adapter := &storeAPIAdapter{store: f.store, attachmentMaintenance: f.maintenance}

	err := adapter.runCLICommandWithRunner(
		context.Background(), api.CLIRunRequest{Args: []string{
			"repack-attachments", "--log-level=debug", "--log-sql",
			"--log-sql-slow-ms=250", "--verbose",
		}}, nil,
		func(context.Context, []string, map[string]string, string, func(string, string) error) error {
			require.FailNow("explicit repack with root flags must still run in the daemon parent")
			return nil
		},
	)

	require.NoError(err)
	has, err := f.store.HasPackRecord(oldPackID)
	require.NoError(err)
	assert.False(has)
}

func TestRepackAttachmentsParentArgsAllowed(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want bool
	}{
		{name: "none", want: true},
		{name: "forwarded logging", args: []string{
			"--log-level=debug", "--log-sql", "--log-sql-slow-ms=250", "--verbose",
		}, want: true},
		{name: "non-forwarded root flag", args: []string{"--config=/tmp/config.toml"}},
		{name: "command-specific flag", args: []string{"--target-size=1024"}},
		{name: "positional argument", args: []string{"unexpected"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, repackAttachmentsParentArgsAllowed(tt.args))
		})
	}
}

func TestStoreAPIAdapterRepackAfterSuccessfulRemovalOnly(t *testing.T) {
	tests := []struct {
		name           string
		predecessorErr error
		wantRemoved    bool
	}{
		{name: "successful removal", wantRemoved: true},
		{name: "failed removal", predecessorErr: errors.New("remove failed")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			f := newAttachmentMaintenanceFixture(t)
			oldPackID := f.makeZeroLivePack([]byte("post removal dead bytes"))
			adapter := &storeAPIAdapter{store: f.store, attachmentMaintenance: f.maintenance}
			runnerCalls := 0

			err := adapter.runCLICommandWithRunner(
				context.Background(),
				api.CLIRunRequest{Args: []string{"remove-account", "alice@example.com", "--yes"}},
				nil,
				func(context.Context, []string, map[string]string, string, func(string, string) error) error {
					runnerCalls++
					return tt.predecessorErr
				},
			)
			if tt.predecessorErr != nil {
				require.ErrorIs(err, tt.predecessorErr)
			} else {
				require.NoError(err)
			}
			assert.Equal(1, runnerCalls)
			has, hasErr := f.store.HasPackRecord(oldPackID)
			require.NoError(hasErr)
			assert.Equal(!tt.wantRemoved, has)
		})
	}
}

func TestStoreAPIAdapterPostRemovalRepackWarningPreservesSuccess(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	maintenance, _ := newFailingAttachmentMaintenance(t)
	adapter := &storeAPIAdapter{store: maintenance.store, attachmentMaintenance: maintenance}
	var events []api.CLIRunEvent

	err := adapter.runCLICommandWithRunner(
		context.Background(), api.CLIRunRequest{Args: []string{"remove-account", "alice@example.com", "--yes"}},
		func(event api.CLIRunEvent) error {
			events = append(events, event)
			return nil
		},
		func(context.Context, []string, map[string]string, string, func(string, string) error) error {
			return nil
		},
	)

	require.NoError(err)
	require.Len(events, 1)
	assert.Equal("stderr", events[0].Type)
	assert.Contains(events[0].Data, "repack-attachments")
}

func TestStoreAPIAdapterPostRemovalRepackCancellationPreservesSuccess(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newAttachmentMaintenanceFixture(t)
	oldPackID := f.makeZeroLivePack([]byte("canceled post-removal repack remains retryable"))
	adapter := &storeAPIAdapter{store: f.store, attachmentMaintenance: f.maintenance}
	ctx, cancel := context.WithCancel(context.Background())
	var events []api.CLIRunEvent

	err := adapter.runCLICommandWithRunner(
		ctx, api.CLIRunRequest{Args: []string{"remove-account", "alice@example.com", "--yes"}},
		func(event api.CLIRunEvent) error {
			events = append(events, event)
			return nil
		},
		func(context.Context, []string, map[string]string, string, func(string, string) error) error {
			cancel()
			return nil
		},
	)

	require.NoError(err, "maintenance cancellation cannot erase committed removal success")
	assert.Empty(events, "cancellation is informational, not a streamed warning")
	has, err := f.store.HasPackRecord(oldPackID)
	require.NoError(err)
	assert.True(has, "canceled cleanup remains inventoried for retry")
	assert.Contains(f.logs.String(), "automatic attachment repack canceled")
}

func TestStoreAPIAdapterExplicitRepackCancellationFailsFast(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newAttachmentMaintenanceFixture(t)
	oldPackID := f.makeZeroLivePack([]byte("explicit canceled repack is fail-fast"))
	adapter := &storeAPIAdapter{store: f.store, attachmentMaintenance: f.maintenance}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := adapter.runCLICommandWithRunner(
		ctx, api.CLIRunRequest{Args: []string{"repack-attachments"}}, nil,
		func(context.Context, []string, map[string]string, string, func(string, string) error) error {
			require.FailNow("explicit repack must never spawn a child process")
			return nil
		},
	)

	require.ErrorIs(err, context.Canceled)
	has, getErr := f.store.HasPackRecord(oldPackID)
	require.NoError(getErr)
	assert.True(has, "fail-fast cancellation leaves physical inventory untouched")
}

func TestStoreAPIAdapterMaintenanceWarningStreamsWithoutChangingSuccess(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	maintenance, logs := newFailingAttachmentMaintenance(t)
	adapter := &storeAPIAdapter{store: maintenance.store, attachmentMaintenance: maintenance}
	var events []api.CLISyncEvent

	err := adapter.runCLISyncWithRunner(
		context.Background(),
		api.CLISyncRequest{Email: "alice@example.com"},
		func(event api.CLISyncEvent) error {
			events = append(events, event)
			return nil
		},
		func(context.Context, []string, func(string, string) error) error { return nil },
	)

	require.NoError(err, "automatic maintenance failure must preserve sync success")
	require.Len(events, 1, "one concise warning event")
	assert.Equal("stderr", events[0].Type)
	assert.Contains(events[0].Data, "pack-attachments")
	assert.Contains(events[0].Data, "retry")
	assert.Contains(logs.String(), "automatic attachment maintenance failed")
}

func TestStoreAPIAdapterMaintenanceWarningStreamFailurePreservesSuccess(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	maintenance, logs := newFailingAttachmentMaintenance(t)
	adapter := &storeAPIAdapter{store: maintenance.store, attachmentMaintenance: maintenance}
	warningErr := errors.New("client disconnected")

	err := adapter.runCLICommandWithRunner(
		context.Background(),
		api.CLIRunRequest{Args: []string{importMboxCommand, "archive.mbox"}},
		func(api.CLIRunEvent) error { return warningErr },
		func(context.Context, []string, map[string]string, string, func(string, string) error) error {
			return nil
		},
	)

	require.NoError(err, "warning stream failure must preserve command success")
	assert.Contains(logs.String(), "failed to emit automatic attachment maintenance warning")
	assert.Contains(logs.String(), warningErr.Error())
}

func TestStoreAPIAdapterServesCLIInitDB(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	tmpDir := t.TempDir()

	s, err := store.Open(filepath.Join(tmpDir, "msgvault.db"))
	require.NoError(err, "open store")
	defer func() { _ = s.Close() }()
	require.NoError(s.InitSchema(), "init schema")

	adapter := &storeAPIAdapter{store: s}
	srv := api.NewServer(
		&config.Config{
			Identity: config.IdentityConfig{Addresses: []string{"alice@example.com"}},
			Server:   config.ServerConfig{APIPort: 8080},
		},
		adapter,
		nil,
		slog.New(slog.DiscardHandler),
	)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/cli/init-db", nil)
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, req)

	require.Equal(http.StatusOK, w.Code, "body: %s", w.Body.String())

	var resp struct {
		Notice string `json:"notice"`
		Stats  struct {
			TotalMessages int64 `json:"total_messages"`
			TotalAccounts int64 `json:"total_accounts"`
		} `json:"stats"`
	}
	require.NoError(json.NewDecoder(w.Body).Decode(&resp), "decode response")
	assert.Contains(resp.Notice, "legacy [identity] config", "migration notice")
	assert.Equal(int64(0), resp.Stats.TotalMessages, "messages")
	assert.Equal(int64(0), resp.Stats.TotalAccounts, "accounts")
}

func TestStoreAPIAdapterServesCLIDeleteDeduped(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	f := storetest.New(t)
	keepID := f.CreateMessage("keep")
	dropID := f.CreateMessage("drop")
	_, err := f.Store.MergeDuplicates(keepID, []int64{dropID}, "batch-a")
	require.NoError(err, "merge duplicate")

	adapter := &storeAPIAdapter{store: f.Store}
	srv := api.NewServer(
		&config.Config{Server: config.ServerConfig{APIPort: 8080}},
		adapter,
		nil,
		slog.New(slog.DiscardHandler),
	)

	planReq := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/cli/delete-deduped/plan",
		strings.NewReader(`{"batch_ids":["batch-a"]}`),
	)
	planReq.Header.Set("Content-Type", "application/json")
	planResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(planResp, planReq)

	require.Equal(http.StatusOK, planResp.Code, "plan body: %s", planResp.Body.String())
	var plan struct {
		Total      int64 `json:"total"`
		BatchCount int64 `json:"batch_count"`
		Batches    []struct {
			ID    string `json:"id"`
			Count int64  `json:"count"`
		} `json:"batches"`
	}
	require.NoError(json.NewDecoder(planResp.Body).Decode(&plan), "decode plan")
	assert.Equal(int64(1), plan.Total, "plan total")
	assert.Equal(int64(1), plan.BatchCount, "plan batch count")
	require.Len(plan.Batches, 1, "plan batches")

	executeReq := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/cli/delete-deduped",
		strings.NewReader(`{
			"batch_ids":["batch-a"],
			"no_backup": true,
			"expected_total": 1,
			"expected_batch_count": 1,
			"expected_batches": [{"id":"batch-a", "count":1}]
		}`),
	)
	executeReq.Header.Set("Content-Type", "application/json")
	executeResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(executeResp, executeReq)

	require.Equal(http.StatusOK, executeResp.Code, "execute body: %s", executeResp.Body.String())
	var executed struct {
		Deleted    int64 `json:"deleted"`
		BatchCount int64 `json:"batch_count"`
	}
	require.NoError(json.NewDecoder(executeResp.Body).Decode(&executed), "decode execute")
	assert.Equal(int64(1), executed.Deleted, "deleted")
	assert.Equal(int64(1), executed.BatchCount, "execute batch count")
}

// TestSetupVectorFeatures_Disabled verifies that when
// cfg.Vector.Enabled is false, setupVectorFeatures returns (nil, nil)
// regardless of build tag. Runs under both tagged and untagged builds.
func TestSetupVectorFeatures_Disabled(t *testing.T) {
	savedCfg := cfg
	defer func() { cfg = savedCfg }()
	cfg = &config.Config{}
	cfg.Vector.Enabled = false

	vf, err := setupVectorFeatures(context.Background(), nil, "", false)
	require.NoError(t, err, "setupVectorFeatures")
	assert.Nil(t, vf, "setupVectorFeatures should be nil when disabled")
}

// TestRunScheduledIMAPSync_NoCredentials verifies that the IMAP path
// in runScheduledSync is reachable — i.e. an IMAP source row makes the
// dispatcher build an IMAP client and surface a credentials error,
// rather than the misleading "oauth2: token expired and refresh token
// is not set" message reported in #329.
func TestRunScheduledIMAPSync_NoCredentials(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	savedCfg := cfg
	defer func() { cfg = savedCfg }()
	cfg = &config.Config{}
	cfg.Data.DataDir = t.TempDir()

	s, err := store.Open(filepath.Join(cfg.Data.DataDir, "msgvault.db"))
	require.NoError(err, "open store")
	defer func() { _ = s.Close() }()
	require.NoError(s.InitSchema(), "init schema")

	const imapID = "imaps://user@example.com@imap.example.com:993"
	_, err = s.GetOrCreateSource("imap", imapID)
	require.NoError(err, "create imap source")

	// getOAuthMgr is only invoked on the Gmail path; fail loudly so
	// any wrong-path dispatch is obvious.
	getOAuthMgr := func(app string) (*oauth.Manager, error) {
		assert.Fail("Gmail OAuth manager unexpectedly requested for IMAP source", "app=%q", app)
		// Unreachable: the assert.Fail above already failed the test; the
		// return only satisfies the signature.
		return nil, nil //nolint:nilnil // unreachable guard, see comment above
	}

	err = runScheduledSync(context.Background(), imapID, s, getOAuthMgr)
	require.Error(err, "runScheduledSync(imap, no creds) want credentials error")
	msg := err.Error()
	assert.False(strings.Contains(msg, "refresh token") || strings.Contains(msg, "token may be expired"),
		"IMAP path produced Gmail-flavoured error %q — dispatch is still Gmail-only", msg)
	assert.True(strings.Contains(msg, "no credentials") || strings.Contains(msg, "IMAP"),
		"error %q does not mention IMAP credentials", msg)
}

// TestRunScheduledIMAPSync_DispatchByDisplayName verifies the daemon
// resolves IMAP sources when config.toml lists the account as a plain
// email — i.e. the lookup key matches the source's display_name rather
// than its imaps:// identifier. Regression: a previous version only
// matched against identifier, so config-driven scheduled syncs fell
// through to the Gmail OAuth path (#329).
func TestRunScheduledIMAPSync_DispatchByDisplayName(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	savedCfg := cfg
	defer func() { cfg = savedCfg }()
	cfg = &config.Config{}
	cfg.Data.DataDir = t.TempDir()

	s, err := store.Open(filepath.Join(cfg.Data.DataDir, "msgvault.db"))
	require.NoError(err, "open store")
	defer func() { _ = s.Close() }()
	require.NoError(s.InitSchema(), "init schema")

	const (
		imapID    = "imaps://user@example.com@imap.example.com:993"
		imapEmail = "user@example.com"
	)
	src, err := s.GetOrCreateSource("imap", imapID)
	require.NoError(err, "create imap source")
	require.NoError(s.UpdateSourceDisplayName(src.ID, imapEmail), "set display_name")

	getOAuthMgr := func(app string) (*oauth.Manager, error) {
		assert.Fail("Gmail OAuth manager unexpectedly requested for IMAP source", "app=%q", app)
		// Unreachable: the assert.Fail above already failed the test; the
		// return only satisfies the signature.
		return nil, nil //nolint:nilnil // unreachable guard, see comment above
	}

	// Pass the email (as config.toml `email = "..."` would supply it),
	// not the imaps:// identifier. Dispatch must still land on the
	// IMAP path; absence of credentials produces an IMAP-shaped error.
	err = runScheduledSync(context.Background(), imapEmail, s, getOAuthMgr)
	require.Error(err, "runScheduledSync(email, no creds) want IMAP credentials error")
	msg := err.Error()
	assert.False(strings.Contains(msg, "refresh token") || strings.Contains(msg, "token may be expired"),
		"dispatch fell through to Gmail path: %q", msg)
	assert.Contains(msg, "IMAP", "error %q does not mention IMAP — dispatch likely missed the source", msg)
}

// TestRunScheduledIMAPSync_DefaultIdentityIsDisplayName verifies the
// IMAP dispatch path writes the source's display_name (the email) as
// the default account identity — never the raw imaps:// identifier
// URL. Regression: a previous version passed src.Identifier, which
// would inject e.g. "imaps://user@host:993" into account_identities
// when the user had cleared their identities.
func TestRunScheduledIMAPSync_DefaultIdentityIsDisplayName(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	savedCfg := cfg
	defer func() { cfg = savedCfg }()
	cfg = &config.Config{}
	cfg.Data.DataDir = t.TempDir()

	s, err := store.Open(filepath.Join(cfg.Data.DataDir, "msgvault.db"))
	require.NoError(err, "open store")
	defer func() { _ = s.Close() }()
	require.NoError(s.InitSchema(), "init schema")

	// Use a closed port on loopback so buildAPIClient succeeds (the
	// client doesn't dial in its constructor) and confirmDefaultIdentity
	// fires before syncer.Full hits ECONNREFUSED.
	const (
		imapID    = "imaps://user@example.com@127.0.0.1:1"
		imapEmail = "user@example.com"
	)
	src, err := s.GetOrCreateSource("imap", imapID)
	require.NoError(err, "create imap source")
	require.NoError(s.UpdateSourceDisplayName(src.ID, imapEmail), "set display_name")
	require.NoError(s.UpdateSourceSyncConfig(src.ID,
		`{"host":"127.0.0.1","port":1,"username":"user@example.com","tls":true}`,
	), "set sync_config")
	require.NoError(imaplib.SaveCredentials(cfg.TokensDir(), imapID, "unused"), "save credentials")

	getOAuthMgr := func(app string) (*oauth.Manager, error) {
		assert.Fail("Gmail OAuth manager unexpectedly requested", "app=%q", app)
		// Unreachable: the assert.Fail above already failed the test; the
		// return only satisfies the signature.
		return nil, nil //nolint:nilnil // unreachable guard, see comment above
	}

	// Expected to fail at the IMAP connection; what matters is that
	// confirmDefaultIdentity ran first with the display_name.
	_ = runScheduledSync(context.Background(), imapID, s, getOAuthMgr)

	identities, err := s.ListAccountIdentities(src.ID)
	require.NoError(err, "ListAccountIdentities")
	require.NotEmpty(identities, "no identities written — confirmDefaultIdentity did not fire on the IMAP path")
	for _, id := range identities {
		if strings.HasPrefix(id.Address, "imaps://") ||
			strings.HasPrefix(id.Address, "imap://") ||
			strings.HasPrefix(id.Address, "imap+starttls://") {
			assert.Fail("identity is an IMAP URL — daemon polluted account_identities",
				"address=%q", id.Address)
		}
	}
	var foundEmail bool
	for _, id := range identities {
		if id.Address == imapEmail {
			foundEmail = true
			break
		}
	}
	assert.True(foundEmail, "identities = %+v, want one with Address=%q", identities, imapEmail)
}

// TestFindScheduledSyncSources verifies that the plural resolver returns
// ALL syncable source types for an identifier (imap + teams together),
// only the matching type for single-type identifiers, and an empty slice
// for unknown identifiers.
func TestFindScheduledSyncSources(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	tmpDir := t.TempDir()
	s, err := store.Open(filepath.Join(tmpDir, "msgvault.db"))
	require.NoError(err, "open store")
	defer func() { _ = s.Close() }()
	require.NoError(s.InitSchema(), "init schema")

	// Unknown identifier returns empty slice (not nil), enabling the
	// Gmail token-first fallback in runScheduledSync.
	got, err := findScheduledSyncSources(s, "missing@example.com")
	require.NoError(err, "findScheduledSyncSources(missing)")
	assert.Empty(got, "findScheduledSyncSources(missing) should be empty")

	// An address that has BOTH an IMAP source (display_name lookup) and
	// a Teams source must return both, in stable order imap then teams.
	const (
		imapID      = "imaps://nat@host@imap.example.com:993"
		sharedEmail = "nat@x.com"
	)
	imapSrc, err := s.GetOrCreateSource("imap", imapID)
	require.NoError(err, "create imap source")
	require.NoError(s.UpdateSourceDisplayName(imapSrc.ID, sharedEmail), "set imap display_name")

	teamsSrc, err := s.GetOrCreateSource("teams", sharedEmail)
	require.NoError(err, "create teams source")

	got, err = findScheduledSyncSources(s, sharedEmail)
	require.NoError(err, "findScheduledSyncSources(imap+teams)")
	require.Len(got, 2, "findScheduledSyncSources(imap+teams) should return 2 sources")
	assert.Equal("imap", got[0].SourceType, "first source should be imap")
	assert.Equal(imapSrc.ID, got[0].ID, "first source ID")
	assert.Equal("teams", got[1].SourceType, "second source should be teams")
	assert.Equal(teamsSrc.ID, got[1].ID, "second source ID")

	// A gmail-only identifier returns exactly one gmail source.
	const gmailAddr = "g@x.com"
	gmailSrc, err := s.GetOrCreateSource("gmail", gmailAddr)
	require.NoError(err, "create gmail source")

	got, err = findScheduledSyncSources(s, gmailAddr)
	require.NoError(err, "findScheduledSyncSources(gmail)")
	require.Len(got, 1, "findScheduledSyncSources(gmail) should return 1 source")
	assert.Equal("gmail", got[0].SourceType, "source should be gmail")
	assert.Equal(gmailSrc.ID, got[0].ID, "gmail source ID")

	// Non-syncable types (mbox) are ignored; returns empty.
	const mboxAddr = "mbox-only@example.com"
	_, err = s.GetOrCreateSource("mbox", mboxAddr)
	require.NoError(err, "create mbox source")

	got, err = findScheduledSyncSources(s, mboxAddr)
	require.NoError(err, "findScheduledSyncSources(mbox-only)")
	assert.Empty(got, "findScheduledSyncSources(mbox-only) should be empty")
}

func TestCronExpressionValidation(t *testing.T) {
	tests := []struct {
		name    string
		expr    string
		wantErr bool
	}{
		{"daily at 2am", "0 2 * * *", false},
		{"every 15 min", "*/15 * * * *", false},
		{"weekly sunday", "0 0 * * 0", false},
		{"monthly first", "0 0 1 * *", false},
		{"twice daily", "0 8,18 * * *", false},
		{"invalid", "not a cron", true},
		{"empty", "", true},
		{"too many fields", "* * * * * *", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := scheduler.ValidateCronExpr(tt.expr)
			if tt.wantErr {
				assert.Error(t, err, "ValidateCronExpr(%q)", tt.expr)
			} else {
				assert.NoError(t, err, "ValidateCronExpr(%q)", tt.expr)
			}
		})
	}
}
