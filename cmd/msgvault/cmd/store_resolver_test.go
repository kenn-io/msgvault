package cmd

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/daemon"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
)

func TestOpenHTTPStoreUsesConfiguredRemoteWithoutDaemonAutostart(t *testing.T) {
	withStoreResolverConfig(t, &config.Config{
		Remote: config.RemoteConfig{
			URL:           "http://daemonclient.example:8080",
			AllowInsecure: true,
		},
	})
	stubStartServeBackgroundProcess(t, func(*config.Config) (*backgroundServeProcess, error) {
		require.FailNow(t, "configured remote must not start a local daemon")
		return nil, errors.New("unreachable")
	})

	st, info, err := OpenHTTPStore(context.Background())
	require.NoError(t, err, "OpenHTTPStore")
	t.Cleanup(func() { _ = st.Close() })

	assert.Equal(t, HTTPStoreConfiguredRemote, info.Kind)
	assert.Equal(t, "http://daemonclient.example:8080", info.URL)
}

func TestOpenHTTPStoreUsesLongTimeoutForConfiguredRemote(t *testing.T) {
	withStoreResolverConfig(t, &config.Config{
		Remote: config.RemoteConfig{
			URL:           "http://daemonclient.example:8080",
			AllowInsecure: true,
		},
	})

	st, _, err := OpenHTTPStore(context.Background())
	require.NoError(t, err, "OpenHTTPStore")
	t.Cleanup(func() { _ = st.Close() })

	assert.Equal(t, api.DaemonLongRequestTimeout, remoteStoreTimeoutForTest(t, st))
}

func TestOpenHTTPStoreStartsLocalDaemonWhenNoRemoteConfigured(t *testing.T) {
	dataDir := t.TempDir()
	withStoreResolverConfig(t, lifecycleTestConfig(dataDir))
	waitCh := make(chan error)
	var started bool
	stubStartServeBackgroundProcess(t, func(c *config.Config) (*backgroundServeProcess, error) {
		started = true
		assert.Equal(t, dataDir, c.Data.DataDir)
		return &backgroundServeProcess{
			PID:     4242,
			LogPath: "/tmp/msgvault-serve.log",
			Wait:    waitCh,
		}, nil
	})
	stubWaitForBackgroundServeReady(t, func(
		ctx context.Context,
		gotDataDir string,
		_ <-chan error,
		timeout time.Duration,
	) (*DaemonRuntime, bool, error) {
		assert.Equal(t, dataDir, gotDataDir)
		assert.Equal(t, 30*time.Second, timeout)
		require.NoError(t, ctx.Err())
		return &DaemonRuntime{
			Record: daemon.RuntimeRecord{PID: 4242},
			Host:   "127.0.0.1",
			Port:   9911,
			API:    daemonAPIVersion,
		}, true, nil
	})

	st, info, err := OpenHTTPStore(context.Background())
	require.NoError(t, err, "OpenHTTPStore")
	t.Cleanup(func() { _ = st.Close() })

	assert.True(t, started, "local daemon should be started")
	assert.Equal(t, HTTPStoreLocalDaemon, info.Kind)
	assert.Equal(t, "http://127.0.0.1:9911", info.URL)
}

func TestOpenHTTPStoreUsesServerAPIKeyForLocalDaemon(t *testing.T) {
	dataDir := t.TempDir()
	localCfg := lifecycleTestConfig(dataDir)
	localCfg.Server.APIKey = "local-daemon-secret"
	withStoreResolverConfig(t, localCfg)

	var gotAPIKey string
	mux := http.NewServeMux()
	mux.Handle("/api/ping", daemon.NewPingHandler(daemon.PingHandlerOptions{
		Service: daemonService,
		Version: Version,
	}))
	mux.HandleFunc("/api/v1/stats", func(w http.ResponseWriter, r *http.Request) {
		gotAPIKey = r.Header.Get("X-Api-Key")
		if gotAPIKey != localCfg.Server.APIKey {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"total_messages":7}`))
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	host, portText, err := net.SplitHostPort(server.Listener.Addr().String())
	require.NoError(t, err, "split listener address")
	port, err := strconv.Atoi(portText)
	require.NoError(t, err, "parse listener port")

	_, err = daemonRuntimeStore(dataDir).Write(daemon.RuntimeRecord{
		PID:     os.Getpid(),
		Network: daemon.NetworkTCP,
		Address: net.JoinHostPort(host, portText),
		Service: daemonService,
		Version: Version,
		Metadata: map[string]string{
			runtimeHost:       host,
			runtimePort:       strconv.Itoa(port),
			runtimeAPIVersion: strconv.Itoa(daemonAPIVersion),
		},
	})
	require.NoError(t, err, "write runtime")

	st, info, err := OpenHTTPStore(context.Background())
	require.NoError(t, err, "OpenHTTPStore")
	t.Cleanup(func() { _ = st.Close() })

	stats, err := st.GetStats()
	require.NoError(t, err, "GetStats")
	assert.Equal(t, HTTPStoreLocalDaemon, info.Kind)
	assert.Equal(t, int64(7), stats.MessageCount)
	assert.Equal(t, localCfg.Server.APIKey, gotAPIKey)
}

func TestOpenHTTPStoreHonorsNeverAutoRestartPolicy(t *testing.T) {
	withTestVersion(t, "v1.1.0")
	dataDir := t.TempDir()
	localCfg := lifecycleTestConfig(dataDir)
	localCfg.Server.DaemonAutoRestart = config.DaemonAutoRestartNever
	withStoreResolverConfig(t, localCfg)

	mux := http.NewServeMux()
	mux.Handle("/api/ping", daemon.NewPingHandler(daemon.PingHandlerOptions{
		Service: daemonService,
		Version: "v1.0.0",
	}))
	mux.HandleFunc("/api/v1/stats", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"total_messages":9}`))
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	host, portText, err := net.SplitHostPort(server.Listener.Addr().String())
	require.NoError(t, err, "split listener address")
	port, err := strconv.Atoi(portText)
	require.NoError(t, err, "parse listener port")

	_, err = daemonRuntimeStore(dataDir).Write(daemon.RuntimeRecord{
		PID:     os.Getpid(),
		Network: daemon.NetworkTCP,
		Address: net.JoinHostPort(host, portText),
		Service: daemonService,
		Version: "v1.0.0",
		Metadata: map[string]string{
			runtimeHost:       host,
			runtimePort:       strconv.Itoa(port),
			runtimeAPIVersion: strconv.Itoa(daemonAPIVersion),
		},
	})
	require.NoError(t, err, "write runtime")
	stubStartServeBackgroundProcess(t, func(*config.Config) (*backgroundServeProcess, error) {
		require.FailNow(t, "never policy must not start over a compatible daemon")
		return nil, errors.New("unreachable")
	})

	st, info, err := OpenHTTPStore(context.Background())
	require.NoError(t, err, "OpenHTTPStore")
	t.Cleanup(func() { _ = st.Close() })

	stats, err := st.GetStats()
	require.NoError(t, err, "GetStats")
	assert.Equal(t, HTTPStoreLocalDaemon, info.Kind)
	assert.Equal(t, int64(9), stats.MessageCount)
}

func TestOpenHTTPStoreLocalFlagUsesLocalDaemonInsteadOfConfiguredRemote(t *testing.T) {
	dataDir := t.TempDir()
	c := lifecycleTestConfig(dataDir)
	c.Remote.URL = "http://daemonclient.example:8080"
	c.Remote.AllowInsecure = true
	withStoreResolverConfig(t, c)
	useLocal = true

	waitCh := make(chan error)
	var started bool
	stubStartServeBackgroundProcess(t, func(got *config.Config) (*backgroundServeProcess, error) {
		started = true
		assert.Equal(t, dataDir, got.Data.DataDir)
		return &backgroundServeProcess{
			PID:     4242,
			LogPath: "/tmp/msgvault-serve.log",
			Wait:    waitCh,
		}, nil
	})
	stubWaitForBackgroundServeReady(t, func(
		ctx context.Context,
		gotDataDir string,
		_ <-chan error,
		timeout time.Duration,
	) (*DaemonRuntime, bool, error) {
		assert.Equal(t, dataDir, gotDataDir)
		assert.Equal(t, 30*time.Second, timeout)
		require.NoError(t, ctx.Err())
		return &DaemonRuntime{
			Record: daemon.RuntimeRecord{PID: 4242},
			Host:   "127.0.0.1",
			Port:   9911,
			API:    daemonAPIVersion,
		}, true, nil
	})

	st, info, err := OpenHTTPStore(context.Background())
	require.NoError(t, err, "OpenHTTPStore")
	t.Cleanup(func() { _ = st.Close() })

	assert.True(t, started, "--local should start/use the local daemon")
	assert.Equal(t, HTTPStoreLocalDaemon, info.Kind)
	assert.Equal(t, "http://127.0.0.1:9911", info.URL)
}

func withStoreResolverConfig(t *testing.T, c *config.Config) {
	t.Helper()
	oldCfg := cfg
	oldUseLocal := useLocal
	cfg = c
	useLocal = false
	t.Cleanup(func() {
		cfg = oldCfg
		useLocal = oldUseLocal
	})
}

func remoteStoreTimeoutForTest(t *testing.T, st *daemonclient.Client) time.Duration {
	t.Helper()
	require.NotNil(t, st, "daemon client")
	return st.Timeout()
}
