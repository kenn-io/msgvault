package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/daemon"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
	mcpserver "go.kenn.io/msgvault/internal/mcp"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func publishMCPRecoveryDaemon(t *testing.T, cfg *config.Config, count int) *httptest.Server {
	t.Helper()
	requirements := require.New(t)
	mux := http.NewServeMux()
	mux.Handle("/api/ping", daemon.NewPingHandler(daemon.PingHandlerOptions{Service: daemonService, Version: Version}))
	mux.HandleFunc("/api/v1/health", func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, cfg.Server.AuthenticationKey(), r.Header.Get("X-Api-Key"))
		w.Header().Set("Content-Type", "application/json")
		assert.NoError(t, json.NewEncoder(w).Encode(map[string]any{"status": "ok", "api_schema_version": api.APISchemaVersion}))
	})
	mux.HandleFunc("/api/v1/stats", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		assert.NoError(t, json.NewEncoder(w).Encode(map[string]any{"total_messages": count}))
	})
	mux.HandleFunc("/api/v1/stats/total", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		assert.NoError(t, json.NewEncoder(w).Encode(map[string]any{"message_count": count}))
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	rt := daemonRuntimeForHTTPServer(t, server, daemonAPIKeyFingerprint(cfg.Server.AuthenticationKey()))
	_, err := daemonRuntimeStore(cfg.Data.DataDir).Write(rt.Record)
	requirements.NoError(err)
	return server
}

func TestMCPLocalRecoveryUsesCurrentRuntime(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	clearServerKeyEnvironment(t)
	cfg := lifecycleTestConfig(t.TempDir())
	cfg.Server.DaemonAutoStart = new(false)
	ctx := withStoreResolverConfig(t, cfg)
	initial := publishMCPRecoveryDaemon(t, cfg, 1)
	client, info, err := openMCPHTTPStore(ctx)
	requirements.NoError(err)
	t.Cleanup(func() { assert.NoError(t, client.Close()) })
	assertions.Equal(HTTPStoreLocalDaemon, info.Kind)
	initial.Close()
	publishMCPRecoveryDaemon(t, cfg, 7)
	stats, err := client.GetStats()
	requirements.NoError(err)
	assertions.Equal(int64(7), stats.MessageCount)
}

func TestMCPCommandRecoversEngineAfterRestart(t *testing.T) {
	requirements := require.New(t)
	clearServerKeyEnvironment(t)
	cfg := lifecycleTestConfig(t.TempDir())
	cfg.Server.DaemonAutoStart = new(false)
	initial := publishMCPRecoveryDaemon(t, cfg, 1)
	setMCPTokenTestFlags(t, nil)
	mcpHTTPAddr = ""
	previous := serveMCPStdioWithOptions
	t.Cleanup(func() { serveMCPStdioWithOptions = previous })
	stopped := errors.New("capture complete")
	serveMCPStdioWithOptions = func(ctx context.Context, opts mcpserver.ServeOptions) error {
		initial.Close()
		publishMCPRecoveryDaemon(t, cfg, 9)
		stats, err := opts.Engine.GetTotalStats(ctx, query.StatsOptions{})
		require.NoError(t, err)
		assert.Equal(t, int64(9), stats.MessageCount)
		return stopped
	}
	mcpCmd.SetContext(withStoreResolverConfig(t, cfg))
	requirements.ErrorIs(mcpCmd.RunE(mcpCmd, nil), stopped)
}

func TestMCPLocalRecoveryHonorsDisabledAutostart(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	clearServerKeyEnvironment(t)
	cfg := lifecycleTestConfig(t.TempDir())
	cfg.Server.DaemonAutoStart = new(false)
	initial := publishMCPRecoveryDaemon(t, cfg, 1)
	client, _, err := openMCPHTTPStore(withStoreResolverConfig(t, cfg))
	requirements.NoError(err)
	t.Cleanup(func() { assert.NoError(t, client.Close()) })
	initial.Close()
	// The fixture uses this test process; mimic the runtime cleanup on daemon exit.
	removeDaemonRuntime(cfg.Data.DataDir)
	_, err = client.GetStats()
	var unavailable *daemonclient.LocalDaemonUnavailableError
	requirements.ErrorAs(err, &unavailable)
	assertions.ErrorContains(err, "daemon_auto_start")
}

func TestMCPRecoveryLeavesConfiguredRemoteFixed(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	cfg := lifecycleTestConfig(t.TempDir())
	cfg.Server.DaemonAutoStart = new(false)
	initial := httptest.NewServer(http.NotFoundHandler())
	cfg.Remote = config.RemoteConfig{URL: initial.URL, AllowInsecure: true}
	client, info, err := openMCPHTTPStore(withStoreResolverConfig(t, cfg))
	requirements.NoError(err)
	t.Cleanup(func() { assert.NoError(t, client.Close()) })
	assertions.Equal(HTTPStoreConfiguredRemote, info.Kind)
	initial.Close()
	publishMCPRecoveryDaemon(t, cfg, 8)
	_, err = client.GetStats()
	requirements.Error(err)
	var unavailable *daemonclient.LocalDaemonUnavailableError
	assertions.NotErrorAs(err, &unavailable)
}

func TestMCPLocalRecoveryStartsReplacementDaemon(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	clearServerKeyEnvironment(t)
	home := t.TempDir()
	dataDir := t.TempDir()
	path := filepath.Join(t.TempDir(), "custom.toml")
	contents := "[server]\napi_port=0\ndaemon_auto_restart='never'\n[analytics]\nengine='sql'\n[data]\ndata_dir='" + filepath.ToSlash(dataDir) + "'\n"
	requirements.NoError(os.WriteFile(path, []byte(contents), 0o600))
	cfg, err := config.Load(path, home)
	requirements.NoError(err)
	assertions.Equal(dataDir, cfg.Data.DataDir)
	initial := publishMCPRecoveryDaemon(t, cfg, 1)
	ctx := withStoreResolverConfig(t, cfg)
	invocationFromContext(ctx).options = invocationOptions{cfgFile: path, cfgFileChanged: true}
	client, _, err := openMCPHTTPStore(ctx)
	requirements.NoError(err)
	t.Cleanup(func() { assert.NoError(t, client.Close()) })
	initial.Close()
	// The fixture uses this test process; mimic the runtime cleanup on daemon exit.
	removeDaemonRuntime(cfg.Data.DataDir)
	daemonCtx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	started := false
	t.Cleanup(func() {
		cancel()
		if started {
			select {
			case err := <-done:
				assert.NoError(t, err)
			case <-time.After(serveLifecycleTestTimeout):
				assert.Fail(t, "replacement daemon did not stop")
			}
		}
	})
	stubStartServeBackgroundProcess(t, func(_ *config.Config, options backgroundServeStartOptions) (*backgroundServeProcess, error) {
		if options.Invocation == nil || !options.Invocation.cfgFileChanged || options.Invocation.cfgFile != path {
			return nil, errors.New("recovery did not preserve the original --config option")
		}
		owner, err := config.Load(options.Invocation.cfgFile, home)
		if err != nil {
			return nil, err
		}
		if owner.Data.DataDir != dataDir {
			return nil, errors.New("recovery selected a different archive directory")
		}
		command := &cobra.Command{Use: "serve"}
		command.SetContext(testInvocationContext(daemonCtx, owner, invocationOptions{}))
		launched := make(chan error, 1)
		started = true
		go func() { err := runServe(command, nil); launched <- err; done <- err }()
		return &backgroundServeProcess{PID: os.Getpid(), Wait: launched}, nil
	})
	requestCtx, cancelRequest := context.WithCancel(t.Context())
	defer cancelRequest()
	assertions.Nil(invocationFromContext(requestCtx), "an MCP tool request has no CLI invocation metadata")
	response, err := client.DoGeneratedRequestWithContext(requestCtx, http.MethodGet, "/api/v1/stats", &generated.RunCLIRequestOptions{})
	requirements.NoError(err)
	defer func() { requirements.NoError(response.Body.Close()) }()
	var stats struct {
		TotalMessages int64 `json:"total_messages"`
	}
	requirements.NoError(json.NewDecoder(response.Body).Decode(&stats))
	assertions.Zero(stats.TotalMessages)
	assertions.True(started)
}
