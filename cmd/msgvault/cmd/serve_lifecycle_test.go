package cmd

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/daemon"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
)

func TestServeCommandHasLifecycleSubcommands(t *testing.T) {
	assert := assert.New(t)

	names := map[string]bool{}
	for _, sub := range serveCmd.Commands() {
		names[sub.Name()] = true
	}
	assert.True(names["status"], "serve must expose status")
	assert.True(names["start"], "serve must expose start")
	assert.True(names["stop"], "serve must expose stop")
	assert.True(names["restart"], "serve must expose restart")
}

func TestServeStatusLines(t *testing.T) {
	assert := assert.New(t)

	rt := &DaemonRuntime{
		Record: daemon.RuntimeRecord{
			PID:       4242,
			Version:   "v9.9.9",
			StartedAt: time.Now().Add(-90 * time.Second),
		},
		Host:             "127.0.0.1",
		Port:             8080,
		APISchemaVersion: api.APISchemaVersion,
	}

	out := strings.Join(serveStatusLines(rt), "\n")
	assert.Contains(out, "msgvault running at http://127.0.0.1:8080")
	assert.Contains(out, "pid:     4242")
	assert.Contains(out, "version: v9.9.9")
	assert.Contains(out, "api:     "+api.APISchemaVersion)
	assert.Contains(out, "uptime:")
}

func TestServeStatusPrintsVectorLine(t *testing.T) {
	tests := []struct {
		name     string
		health   string
		wantLine string
		wantNone bool
	}{
		{"initializing", `{"status":"ok","vector":{"status":"initializing"}}`,
			"vector:  initializing", false},
		{"error with detail", `{"status":"ok","vector":{"status":"error","error":"migration exploded"}}`,
			"vector:  error (migration exploded)", false},
		{"stale with detail", `{"status":"ok","vector":{"status":"stale","error":"active=\"old:1\" configured=\"new:2\""}}`,
			`vector:  stale (active="old:1" configured="new:2")`, false},
		{"disabled omits line", `{"status":"ok"}`, "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, "/health", r.URL.Path)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tt.health))
			}))
			defer srv.Close()

			vh := fetchDaemonVectorHealth(context.Background(), srv.URL)
			lines := vectorStatusLines(vh)
			if tt.wantNone {
				assert.Empty(t, lines)
				return
			}
			require.Len(t, lines, 1)
			assert.Contains(t, lines[0], tt.wantLine)
		})
	}
}

func TestRunServeStatusIncludesVectorHealth(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	dataDir := t.TempDir()

	mux := http.NewServeMux()
	mux.Handle("/api/ping", daemon.NewPingHandler(daemon.PingHandlerOptions{
		Service: daemonService,
		Version: Version,
	}))
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok","vector":{"status":"initializing"}}`))
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	host, portText, err := net.SplitHostPort(server.Listener.Addr().String())
	require.NoError(err, "split listener address")
	port, err := strconv.Atoi(portText)
	require.NoError(err, "parse listener port")

	_, err = daemonRuntimeStore(dataDir).Write(daemon.RuntimeRecord{
		PID:     os.Getpid(),
		Network: daemon.NetworkTCP,
		Address: net.JoinHostPort(host, portText),
		Service: daemonService,
		Version: Version,
		Metadata: map[string]string{
			runtimeHost:             host,
			runtimePort:             strconv.Itoa(port),
			runtimeAPIVersion:       strconv.Itoa(daemonAPIVersion),
			runtimeAPISchemaVersion: api.APISchemaVersion,
		},
	})
	require.NoError(err, "write runtime record")

	cmd, stdout, stderr := lifecycleTestCommand()
	cmd.SetContext(context.Background())
	require.NoError(runServeStatus(cmd, dataDir), "runServeStatus")

	out := stdout.String()
	assert.Contains(out, "msgvault running at", "status shows the running daemon")
	assert.Contains(out, "vector:  initializing", "status includes daemon vector health")
	assert.Empty(stderr.String(), "status must not write to stderr")
}

func TestRunServeStatusNoDaemonWritesOnlyStdout(t *testing.T) {
	cmd, stdout, stderr := lifecycleTestCommand()

	require.NoError(t, runServeStatus(cmd, t.TempDir()))

	assert.Equal(t, "No msgvault daemon is running.\n", stdout.String())
	assert.Empty(t, stderr.String())
}

func TestRunServeStatusReturnsRuntimeListError(t *testing.T) {
	assert := assert.New(t)

	dataDir := runtimeDataDirFile(t)
	cmd, stdout, stderr := lifecycleTestCommand()

	err := runServeStatus(cmd, dataDir)

	require.Error(t, err, "status should surface runtime-store failures")
	assert.Contains(err.Error(), "list daemon runtimes", "runtime list error")
	assert.Empty(stdout.String())
	assert.Empty(stderr.String())
}

func TestStopLiveDaemonsReturnsRuntimeListError(t *testing.T) {
	assert := assert.New(t)

	dataDir := runtimeDataDirFile(t)
	cmd, stdout, stderr := lifecycleTestCommand()

	err := stopLiveDaemons(cmd, dataDir, false)

	require.Error(t, err, "stop should surface runtime-store failures")
	assert.Contains(err.Error(), "list daemon runtimes", "runtime list error")
	assert.Empty(stdout.String())
	assert.Empty(stderr.String())
}

func TestWaitForBackgroundServeReadyReturnsRuntimeListError(t *testing.T) {
	assert := assert.New(t)

	dataDir := runtimeDataDirFile(t)

	rt, ready, err := waitForBackgroundServeReady(
		context.Background(),
		dataDir,
		nil,
		time.Millisecond,
	)

	require.Error(t, err, "wait should surface runtime-store failures")
	assert.Contains(err.Error(), "list daemon runtimes", "runtime list error")
	assert.False(ready, "ready")
	assert.Nil(rt, "runtime")
}

func TestWaitForDaemonRuntimeCancelsDuringProbe(t *testing.T) {
	assert := assert.New(t)
	require :=
		require.New(t)

	dataDir := t.TempDir()
	block := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	var cancelProbe sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		cancelProbe.Do(func() {
			cancel()
		})
		<-block
	}))
	t.Cleanup(func() {
		close(block)
		cancel()
		server.Close()
	})
	host, portText, err := net.SplitHostPort(server.Listener.Addr().String())
	require.NoError(
		err, "split server address")

	port, err := strconv.Atoi(portText)
	require.NoError(
		err, "parse server port")

	_, err = daemonRuntimeStore(dataDir).Write(daemon.RuntimeRecord{
		PID:     os.Getpid(),
		Network: daemon.NetworkTCP,
		Address: net.JoinHostPort(host, portText),
		Service: daemonService,
		Metadata: map[string]string{
			runtimeHost:             host,
			runtimePort:             strconv.Itoa(port),
			runtimeAPIVersion:       strconv.Itoa(daemonAPIVersion),
			runtimeAPISchemaVersion: api.APISchemaVersion,
		},
	})
	require.NoError(
		err, "write runtime record")

	start := time.Now()
	rt, ready, err := waitForDaemonRuntime(ctx, dataDir, time.Second, daemonRuntimeReady, nil)
	elapsed := time.Since(start)
	require.ErrorIs(err, context.Canceled, "wait error")
	assert.False(ready, "ready")
	assert.Nil(rt, "runtime")
	assert.Less(elapsed, 250*time.Millisecond, "wait should not sit through daemon probe timeout")
}

func TestWaitForRecordedDaemonExitRemovesRecordWhenGone(t *testing.T) {
	assert := assert.New(t)
	require :=
		require.New(t)

	recordPath := filepath.Join(t.TempDir(), "runtime.json")
	require.NoError(
		os.WriteFile(recordPath, []byte("runtime"), 0o600), "write runtime record")

	rec := daemon.RuntimeRecord{SourcePath: recordPath}
	calls := 0

	exited := waitForRecordedDaemonExit(
		rec,
		100*time.Millisecond,
		time.Millisecond,
		func(daemon.RuntimeRecord) bool {
			calls++
			return calls < 3
		},
	)
	require.True(exited, "wait should observe daemon exit")
	assert.Equal(3, calls, "poll count")
	assert.NoFileExists(recordPath, "runtime record")
}

func TestRunServeStartAlreadyRunningWritesOnlyStdout(t *testing.T) {
	assert := assert.New(t)
	require :=
		require.New(t)

	dataDir := t.TempDir()
	server := httptestPingDaemon(t)
	portText := strconv.Itoa(server.Port)
	_, err := daemonRuntimeStore(dataDir).Write(daemon.RuntimeRecord{
		PID:     os.Getpid(),
		Network: daemon.NetworkTCP,
		Address: net.JoinHostPort(server.Host, portText),
		Service: daemonService,
		Version: Version,
		Metadata: map[string]string{
			runtimeHost:       server.Host,
			runtimePort:       portText,
			runtimeAPIVersion: strconv.Itoa(daemonAPIVersion),
		},
	})
	require.NoError(
		err, "write runtime")

	cmd, stdout, stderr := lifecycleTestCommand()
	require.NoError(runServeStart(cmd, lifecycleTestConfig(dataDir)))
	assert.Equal(
		"msgvault already running at http://"+net.JoinHostPort(server.Host, portText)+
			" (pid "+strconv.Itoa(os.Getpid())+")\n",
		stdout.String())
	assert.Empty(stderr.String())
}

func TestRunServeStartDoesNotDowngradeNewerDaemon(t *testing.T) {
	assert := assert.New(t)
	require :=
		require.New(t)

	withTestVersion(t, "v1.0.0")
	dataDir := t.TempDir()
	server := httptestPingDaemon(t)
	portText := strconv.Itoa(server.Port)
	_, err := daemonRuntimeStore(dataDir).Write(daemon.RuntimeRecord{
		PID:     os.Getpid(),
		Network: daemon.NetworkTCP,
		Address: net.JoinHostPort(server.Host, portText),
		Service: daemonService,
		Version: "v1.1.0",
		Metadata: map[string]string{
			runtimeHost:       server.Host,
			runtimePort:       portText,
			runtimeAPIVersion: strconv.Itoa(daemonAPIVersion),
		},
	})
	require.NoError(
		err, "write runtime")

	stubStopDaemonRuntimeForUpgrade(t, func(config.Config, *DaemonRuntime) error {
		require.Fail("older CLI must not stop a newer daemon")
		return nil
	})
	stubStartServeBackgroundProcess(t, func(*config.Config, backgroundServeStartOptions) (*backgroundServeProcess, error) {
		require.FailNow("older CLI must not start over a newer daemon")
		return nil, errors.New("unreachable")
	})
	cmd, stdout, stderr := lifecycleTestCommand()
	require.NoError(runServeStart(cmd, lifecycleTestConfig(dataDir)))
	assert.Equal(
		"msgvault already running at http://"+net.JoinHostPort(server.Host, portText)+
			" (pid "+strconv.Itoa(os.Getpid())+")\n",
		stdout.String())
	assert.Empty(stderr.String())
}

func TestRunServeStartUpgradesOlderDaemon(t *testing.T) {
	assert := assert.New(t)
	require :=
		require.New(t)

	withTestVersion(t, "v1.1.0")
	dataDir := t.TempDir()
	server := httptestPingDaemon(t)
	portText := strconv.Itoa(server.Port)
	_, err := daemonRuntimeStore(dataDir).Write(daemon.RuntimeRecord{
		PID:     os.Getpid(),
		Network: daemon.NetworkTCP,
		Address: net.JoinHostPort(server.Host, portText),
		Service: daemonService,
		Version: "v1.0.0",
		Metadata: map[string]string{
			runtimeHost:       server.Host,
			runtimePort:       portText,
			runtimeAPIVersion: strconv.Itoa(daemonAPIVersion),
		},
	})
	require.NoError(
		err, "write runtime")

	var stoppedPID int
	stubStopDaemonRuntimeForUpgrade(t, func(_ config.Config, rt *DaemonRuntime) error {
		stoppedPID = rt.Record.PID
		return nil
	})
	waitCh := make(chan error)
	stubStartServeBackgroundProcess(t, func(*config.Config, backgroundServeStartOptions) (*backgroundServeProcess, error) {
		return &backgroundServeProcess{
			PID:     777,
			LogPath: "/tmp/msgvault-serve.log",
			Wait:    waitCh,
		}, nil
	})
	stubWaitForBackgroundServeReady(t, func(
		context.Context,
		string,
		<-chan error,
		time.Duration,
	) (*DaemonRuntime, bool, error) {
		return &DaemonRuntime{
			Record: daemon.RuntimeRecord{PID: 777},
			Host:   "127.0.0.1",
			Port:   9090,
		}, true, nil
	})
	cmd, stdout, stderr := lifecycleTestCommand()
	require.NoError(runServeStart(cmd, lifecycleTestConfig(dataDir)))
	assert.Equal(os.Getpid(), stoppedPID, "stopped older daemon")
	assert.Equal(
		"msgvault running at http://127.0.0.1:9090 (pid 777)\n"+
			"Logs: /tmp/msgvault-serve.log\n",
		stdout.String())
	assert.Empty(stderr.String())
}

func TestRunServeStartHonorsNeverAutoRestartPolicy(t *testing.T) {
	assert := assert.New(t)
	require :=
		require.New(t)

	withTestVersion(t, "v1.1.0")
	dataDir := t.TempDir()
	server := httptestPingDaemon(t)
	portText := strconv.Itoa(server.Port)
	_, err := daemonRuntimeStore(dataDir).Write(daemon.RuntimeRecord{
		PID:     os.Getpid(),
		Network: daemon.NetworkTCP,
		Address: net.JoinHostPort(server.Host, portText),
		Service: daemonService,
		Version: "v1.0.0",
		Metadata: map[string]string{
			runtimeHost:       server.Host,
			runtimePort:       portText,
			runtimeAPIVersion: strconv.Itoa(daemonAPIVersion),
		},
	})
	require.NoError(
		err, "write runtime")

	stubStopDaemonRuntimeForUpgrade(t, func(config.Config, *DaemonRuntime) error {
		require.FailNow("never policy must not stop a compatible daemon")
		return errors.New("unreachable")
	})
	stubStartServeBackgroundProcess(t, func(*config.Config, backgroundServeStartOptions) (*backgroundServeProcess, error) {
		require.FailNow("never policy must not start over a compatible daemon")
		return nil, errors.New("unreachable")
	})
	cfg := lifecycleTestConfig(dataDir)
	cfg.Server.DaemonAutoRestart = config.DaemonAutoRestartNever
	cmd, stdout, stderr := lifecycleTestCommand()
	require.NoError(runServeStart(cmd, cfg))
	assert.Equal(
		"msgvault already running at http://"+net.JoinHostPort(server.Host, portText)+
			" (pid "+strconv.Itoa(os.Getpid())+")\n",
		stdout.String())
	assert.Empty(stderr.String())
}

func TestRunServeStartUpgradesOlderIncompatibleDaemon(t *testing.T) {
	assert := assert.New(t)
	require :=
		require.New(t)

	withTestVersion(t, "v1.1.0")
	dataDir := t.TempDir()
	server := httptestPingDaemon(t)
	portText := strconv.Itoa(server.Port)
	_, err := daemonRuntimeStore(dataDir).Write(daemon.RuntimeRecord{
		PID:     os.Getpid(),
		Network: daemon.NetworkTCP,
		Address: net.JoinHostPort(server.Host, portText),
		Service: daemonService,
		Version: "v1.0.0",
		Metadata: map[string]string{
			runtimeHost:       server.Host,
			runtimePort:       portText,
			runtimeAPIVersion: strconv.Itoa(daemonAPIVersion - 1),
		},
	})
	require.NoError(
		err, "write runtime")

	var stoppedPID int
	stubStopDaemonRuntimeForUpgrade(t, func(_ config.Config, rt *DaemonRuntime) error {
		stoppedPID = rt.Record.PID
		return nil
	})
	waitCh := make(chan error)
	stubStartServeBackgroundProcess(t, func(*config.Config, backgroundServeStartOptions) (*backgroundServeProcess, error) {
		return &backgroundServeProcess{
			PID:     779,
			LogPath: "/tmp/msgvault-serve.log",
			Wait:    waitCh,
		}, nil
	})
	stubWaitForBackgroundServeReady(t, func(
		context.Context,
		string,
		<-chan error,
		time.Duration,
	) (*DaemonRuntime, bool, error) {
		return &DaemonRuntime{
			Record: daemon.RuntimeRecord{PID: 779},
			Host:   "127.0.0.1",
			Port:   9092,
		}, true, nil
	})
	cmd, stdout, stderr := lifecycleTestCommand()
	require.NoError(runServeStart(cmd, lifecycleTestConfig(dataDir)))
	assert.Equal(os.Getpid(), stoppedPID, "stopped older incompatible daemon")
	assert.Equal(
		"msgvault running at http://127.0.0.1:9092 (pid 779)\n"+
			"Logs: /tmp/msgvault-serve.log\n",
		stdout.String())
	assert.Empty(stderr.String())
}

func TestRunServeStartRefusesNewerIncompatibleDaemon(t *testing.T) {
	assert := assert.New(t)
	require :=
		require.New(t)

	withTestVersion(t, "v1.0.0")
	dataDir := t.TempDir()
	server := httptestPingDaemon(t)
	portText := strconv.Itoa(server.Port)
	_, err := daemonRuntimeStore(dataDir).Write(daemon.RuntimeRecord{
		PID:     os.Getpid(),
		Network: daemon.NetworkTCP,
		Address: net.JoinHostPort(server.Host, portText),
		Service: daemonService,
		Version: "v1.1.0",
		Metadata: map[string]string{
			runtimeHost:       server.Host,
			runtimePort:       portText,
			runtimeAPIVersion: strconv.Itoa(daemonAPIVersion + 1),
		},
	})
	require.NoError(
		err, "write runtime")

	stubStopDaemonRuntimeForUpgrade(t, func(config.Config, *DaemonRuntime) error {
		require.FailNow("older CLI must not stop a newer incompatible daemon")
		return errors.New("unreachable")
	})
	stubStartServeBackgroundProcess(t, func(*config.Config, backgroundServeStartOptions) (*backgroundServeProcess, error) {
		require.FailNow("older CLI must not start over a newer incompatible daemon")
		return nil, errors.New("unreachable")
	})
	cmd, stdout, stderr := lifecycleTestCommand()

	err = runServeStart(cmd, lifecycleTestConfig(dataDir))
	require.Error(err, "newer incompatible daemon must be refused")
	assert.Contains(err.Error(), "incompatible daemon is already running")
	assert.Contains(err.Error(), "msgvault serve stop")
	assert.Empty(stdout.String())
	assert.Empty(stderr.String())
}

func TestRunServeRestartStartsWhenNoDaemonIsRunning(t *testing.T) {
	dataDir := t.TempDir()
	waitCh := make(chan error)
	stubStartServeBackgroundProcess(t, func(*config.Config, backgroundServeStartOptions) (*backgroundServeProcess, error) {
		return &backgroundServeProcess{
			PID:     778,
			LogPath: "/tmp/msgvault-serve.log",
			Wait:    waitCh,
		}, nil
	})
	stubWaitForBackgroundServeReady(t, func(
		context.Context,
		string,
		<-chan error,
		time.Duration,
	) (*DaemonRuntime, bool, error) {
		return &DaemonRuntime{
			Record: daemon.RuntimeRecord{PID: 778},
			Host:   "127.0.0.1",
			Port:   9091,
		}, true, nil
	})
	cmd, stdout, stderr := lifecycleTestCommand()

	require.NoError(t, runServeRestart(cmd, lifecycleTestConfig(dataDir)))

	assert.Equal(t,
		"msgvault running at http://127.0.0.1:9091 (pid 778)\n"+
			"Logs: /tmp/msgvault-serve.log\n",
		stdout.String())
	assert.Empty(t, stderr.String())
}

func TestServeStopGraceTimeoutCoversDaemonShutdownBudget(t *testing.T) {
	assert.GreaterOrEqual(t,
		serveStopGraceTimeout,
		serveAPIShutdownTimeout+serveSchedulerStopTimeout+30*time.Minute,
		"stop fallback must not kill before operation drain can finish")
}

func TestRequestDaemonShutdownUsesRuntimeToken(t *testing.T) {
	assert := assert.New(t)
	require :=
		require.New(t)

	var gotToken string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(api.DaemonShutdownPath, r.URL.Path, "path")
		assert.Equal(http.MethodPost, r.Method, "method")
		gotToken = r.Header.Get(api.DaemonShutdownTokenHeader)
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(server.Close)
	host, portText, err := net.SplitHostPort(server.Listener.Addr().String())
	require.NoError(
		err, "split listener address")

	requested, err := requestDaemonShutdown(daemon.RuntimeRecord{
		PID:     os.Getpid(),
		Network: daemon.NetworkTCP,
		Address: net.JoinHostPort(host, portText),
		Service: daemonService,
		Metadata: map[string]string{
			runtimeHost:          host,
			runtimePort:          portText,
			runtimeAPIVersion:    strconv.Itoa(daemonAPIVersion),
			runtimeShutdownToken: "test-runtime-token",
		},
	})
	require.NoError(
		err, "request shutdown")

	assert.True(requested, "shutdown requested")
	assert.Equal("test-runtime-token", gotToken, "shutdown token")
}

func TestNewDaemonIdleTrackerOnlyRunsForBackgroundServeChild(t *testing.T) {
	cfg := lifecycleTestConfig(t.TempDir())
	cfg.Server.DaemonIdleTimeout = time.Millisecond

	tracker := newDaemonIdleTracker(cfg, func() {
		require.FailNow(t, "foreground serve must not arm idle shutdown")
	})

	assert.Nil(t, tracker)
}

func TestNewDaemonIdleTrackerUsesServerConfigTimeout(t *testing.T) {
	t.Setenv(serveBackgroundChildEnv, "1")
	cfg := lifecycleTestConfig(t.TempDir())
	cfg.Server.DaemonIdleTimeout = 20 * time.Millisecond
	fired := make(chan struct{})

	tracker := newDaemonIdleTracker(cfg, func() { close(fired) })
	require.NotNil(t, tracker)

	go tracker.Run(t.Context())

	select {
	case <-fired:
	case <-time.After(time.Second):
		require.FailNow(t, "idle tracker did not fire")
	}
}

func TestNewDaemonIdleTrackerEnvOverrideDisables(t *testing.T) {
	t.Setenv(serveBackgroundChildEnv, "1")
	t.Setenv("MSGVAULT_DAEMON_IDLE_TIMEOUT", "0s")
	cfg := lifecycleTestConfig(t.TempDir())
	cfg.Server.DaemonIdleTimeout = 20 * time.Millisecond

	tracker := newDaemonIdleTracker(cfg, func() {
		require.FailNow(t, "idle tracker fired despite env disable")
	})

	assert.Nil(t, tracker)
}

func lifecycleTestCommand() (*cobra.Command, *bytes.Buffer, *bytes.Buffer) {
	cmd := &cobra.Command{Use: "test"}
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	cmd.SetOut(stdout)
	cmd.SetErr(stderr)
	return cmd, stdout, stderr
}

func runtimeDataDirFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "data-file")
	require.NoError(t, os.WriteFile(path, []byte("not a directory"), 0o600), "write data dir file")
	return path
}

func withTestVersion(t *testing.T, version string) {
	t.Helper()
	old := Version
	Version = version
	t.Cleanup(func() { Version = old })
}

func stubStopDaemonRuntimeForUpgrade(
	t *testing.T,
	fn func(config.Config, *DaemonRuntime) error,
) {
	t.Helper()
	old := stopDaemonRuntimeForUpgrade
	stopDaemonRuntimeForUpgrade = fn
	t.Cleanup(func() { stopDaemonRuntimeForUpgrade = old })
}

func stubStartServeBackgroundProcess(
	t *testing.T,
	fn func(*config.Config, backgroundServeStartOptions) (*backgroundServeProcess, error),
) {
	t.Helper()
	old := startServeBackgroundProcessForRun
	startServeBackgroundProcessForRun = fn
	t.Cleanup(func() { startServeBackgroundProcessForRun = old })
}

func stubWaitForBackgroundServeReady(
	t *testing.T,
	fn func(context.Context, string, <-chan error, time.Duration) (*DaemonRuntime, bool, error),
) {
	t.Helper()
	old := waitForBackgroundServeReadyForRun
	waitForBackgroundServeReadyForRun = fn
	t.Cleanup(func() { waitForBackgroundServeReadyForRun = old })
}

type lifecyclePingServer struct {
	Host string
	Port int
}

func httptestPingDaemon(t *testing.T) lifecyclePingServer {
	t.Helper()
	server := httptest.NewServer(daemon.NewPingHandler(daemon.PingHandlerOptions{
		Service: daemonService,
		Version: Version,
	}))
	t.Cleanup(server.Close)
	host, portText, err := net.SplitHostPort(server.Listener.Addr().String())
	require.NoError(t, err, "split ping listener address")
	port, err := strconv.Atoi(portText)
	require.NoError(t, err, "parse ping listener port")
	return lifecyclePingServer{Host: host, Port: port}
}

func lifecycleTestConfig(dataDir string) *config.Config {
	return &config.Config{
		HomeDir: dataDir,
		Data: config.DataConfig{
			DataDir: dataDir,
		},
		Server: config.ServerConfig{
			BindAddr: "127.0.0.1",
			APIPort:  8080,
		},
		Analytics: config.AnalyticsConfig{
			Engine:         config.AnalyticsEngineAuto,
			AutoBuildCache: true,
		},
	}
}
