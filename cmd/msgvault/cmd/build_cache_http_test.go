package cmd

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/daemon"
	"go.kenn.io/msgvault/internal/config"
)

func TestBuildCacheAutostartFulfilledSkipsRedundantHTTPRequest(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)
	stubBuildCacheDaemonAutostart(t, server, startupCacheBuildOutcomeFulfilled, nil)

	cmd, stdout := buildCacheHTTPTestCommand()
	var err error
	captureStderrDuring(t, func() {
		err = runBuildCacheHTTP(cmd, false)
	})

	require.NoError(t, err)
	assert.Zero(t, requests.Load())
	assert.Equal(t,
		"Cache build complete. The daemon is running and using the analytics cache.\n",
		stdout.String(),
	)
}

func TestBuildCacheAutostartFailedReturnsErrorWithoutRetry(t *testing.T) {
	require := require.New(t)

	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)
	logPath := filepath.Join(t.TempDir(), "serve.log")
	stubBuildCacheDaemonAutostart(t, server, startupCacheBuildOutcomeFailed, &logPath)

	cmd, _ := buildCacheHTTPTestCommand()
	var err error
	captureStderrDuring(t, func() {
		err = runBuildCacheHTTP(cmd, false)
	})

	require.Error(err)
	require.ErrorContains(err, "analytics cache build failed during daemon startup")
	require.ErrorContains(err, "daemon is running with live SQL")
	require.ErrorContains(err, logPath)
	assert.Zero(t, requests.Load())
}

func TestBuildCacheAutostartFatalDuckDBFailureDoesNotReportSQLFallback(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)
	logPath := filepath.Join(t.TempDir(), "serve.log")
	stubBuildCacheDaemonAutostart(t, server, startupCacheBuildOutcomeFatal, &logPath)

	cmd, _ := buildCacheHTTPTestCommand()
	var err error
	captureStderrDuring(t, func() {
		err = runBuildCacheHTTP(cmd, false)
	})

	require.Error(err)
	assert.Contains(err.Error(), "required DuckDB initialization")
	assert.Contains(err.Error(), "daemon is shutting down")
	assert.NotContains(err.Error(), "live SQL")
	assert.Contains(err.Error(), logPath)
	assert.Zero(requests.Load())
}

func TestBuildCacheAutostartUnconsumedUsesHTTPRequest(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		assert.Equal(t, "/api/v1/cli/build-cache", r.URL.Path)
		w.Header().Set("Content-Type", "application/x-ndjson")
		_, _ = w.Write([]byte(`{"type":"stdout","data":"Built through HTTP.\n"}` + "\n"))
		_, _ = w.Write([]byte(`{"type":"complete"}` + "\n"))
	}))
	t.Cleanup(server.Close)
	stubBuildCacheDaemonAutostart(t, server, startupCacheBuildOutcomeUnconsumed, nil)

	cmd, stdout := buildCacheHTTPTestCommand()
	var err error
	captureStderrDuring(t, func() {
		err = runBuildCacheHTTP(cmd, false)
	})

	require.NoError(t, err)
	assert.Equal(t, int32(1), requests.Load())
	assert.Equal(t, "Built through HTTP.\n", stdout.String())
}

func TestBuildCacheFullRebuildPassesFullStartupIntent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)
	var gotIntent startupCacheBuildIntent
	stubBuildCacheDaemonAutostart(t, server, startupCacheBuildOutcomeFulfilled, nil,
		func(intent startupCacheBuildIntent) { gotIntent = intent })

	cmd, _ := buildCacheHTTPTestCommand()
	var err error
	captureStderrDuring(t, func() {
		err = runBuildCacheHTTP(cmd, true)
	})

	require.NoError(t, err)
	assert.Equal(t, startupCacheBuildIntentFull, gotIntent)
}

func TestBuildCacheUsesConfiguredRemoteHTTPAndPreservesOutput(t *testing.T) {
	assert := assert.New(t)

	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(http.MethodPost, r.Method, "method")
		assert.Equal("/api/v1/cli/build-cache", r.URL.Path, "path")
		assert.Equal("true", r.URL.Query().Get("full_rebuild"), "full_rebuild query")
		requests.Add(1)

		w.Header().Set("Content-Type", "application/x-ndjson")
		_, _ = w.Write([]byte(`{"type":"stdout","data":"Building cache...\n"}` + "\n"))
		_, _ = w.Write([]byte(`{"type":"stderr","data":"Warning: using CSV fallback\n"}` + "\n"))
		_, _ = w.Write([]byte(`{"type":"stdout","data":"Exported 42 messages to /tmp/msgvault-analytics\n"}` + "\n"))
		_, _ = w.Write([]byte(`{"type":"complete"}` + "\n"))
	}))
	t.Cleanup(server.Close)

	dataDir := t.TempDir()
	withStoreResolverConfig(t, &config.Config{
		HomeDir: dataDir,
		Data:    config.DataConfig{DataDir: dataDir},
		Remote: config.RemoteConfig{
			URL:           server.URL,
			AllowInsecure: true,
		},
	})
	oldLogger := logger
	logger = slog.New(slog.NewTextHandler(os.Stderr, nil))
	t.Cleanup(func() { logger = oldLogger })

	oldFullRebuild := fullRebuild
	fullRebuild = false
	t.Cleanup(func() { fullRebuild = oldFullRebuild })

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd := &cobra.Command{Use: buildCacheCmd.Use, RunE: buildCacheCmd.RunE}
	cmd.Flags().BoolVar(&fullRebuild, "full-rebuild", false, "Rebuild all cache files from scratch")
	cmd.SetArgs([]string{"--full-rebuild"})
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)

	err := cmd.Execute()
	require.NoError(t, err, "build-cache command")
	assert.Equal(int32(1), requests.Load(), "HTTP requests")
	assert.Equal("Building cache...\nExported 42 messages to /tmp/msgvault-analytics\n", stdout.String())
	assert.Equal("Warning: using CSV fallback\n", stderr.String())
}

func TestBuildCacheRunningLocalDaemonUsesSingleHTTPRequest(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	var requests atomic.Int32
	mux := http.NewServeMux()
	mux.Handle("/api/ping", daemon.NewPingHandler(daemon.PingHandlerOptions{
		Service: daemonService,
		Version: Version,
	}))
	mux.HandleFunc("/api/v1/health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	mux.HandleFunc("/api/v1/cli/build-cache", func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/x-ndjson")
		_, _ = w.Write([]byte(`{"type":"stdout","data":"Built through running daemon.\n"}` + "\n"))
		_, _ = w.Write([]byte(`{"type":"complete"}` + "\n"))
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	dataDir := t.TempDir()
	withStoreResolverConfig(t, lifecycleTestConfig(dataDir))
	rt := daemonRuntimeForHTTPServer(t, server, daemonAPIKeyFingerprint(""))
	_, err := daemonRuntimeStore(dataDir).Write(rt.Record)
	require.NoError(err, "write running daemon record")
	stubStartServeBackgroundProcess(t, func(
		*config.Config,
		backgroundServeStartOptions,
	) (*backgroundServeProcess, error) {
		require.FailNow("a running local daemon must not be restarted with cache intent")
		return nil, errors.New("unreachable daemon restart")
	})

	cmd, stdout := buildCacheHTTPTestCommand()
	err = runBuildCacheHTTP(cmd, false)

	require.NoError(err)
	assert.Equal(int32(1), requests.Load())
	assert.Equal("Built through running daemon.\n", stdout.String())
}

func buildCacheHTTPTestCommand() (*cobra.Command, *bytes.Buffer) {
	stdout := &bytes.Buffer{}
	cmd := &cobra.Command{Use: "build-cache"}
	cmd.SetContext(context.Background())
	cmd.SetOut(stdout)
	cmd.SetErr(&bytes.Buffer{})
	return cmd, stdout
}

func stubBuildCacheDaemonAutostart(
	t *testing.T,
	server *httptest.Server,
	outcome startupCacheBuildOutcome,
	logPathOverride *string,
	observeIntent ...func(startupCacheBuildIntent),
) {
	t.Helper()
	dataDir := t.TempDir()
	withStoreResolverConfig(t, lifecycleTestConfig(dataDir))
	waitCh := make(chan error)
	logPath := filepath.Join(dataDir, "serve.log")
	if logPathOverride != nil {
		logPath = *logPathOverride
	}
	stubStartServeBackgroundProcess(t, func(
		_ *config.Config,
		opts backgroundServeStartOptions,
	) (*backgroundServeProcess, error) {
		for _, observe := range observeIntent {
			observe(opts.CacheBuildIntent)
		}
		return &backgroundServeProcess{PID: 4242, LogPath: logPath, Wait: waitCh}, nil
	})
	stubWaitForBackgroundServeReady(t, func(
		context.Context,
		string,
		<-chan error,
		time.Duration,
	) (*DaemonRuntime, bool, error) {
		rt := daemonRuntimeForHTTPServer(t, server, daemonAPIKeyFingerprint(""))
		rt.Record.PID = 4242
		rt.Record.Metadata[runtimeStartupCacheBuildOutcome] = string(outcome)
		return rt, true, nil
	})
}
