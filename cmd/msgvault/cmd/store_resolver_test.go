package cmd

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/daemon"
	"go.kenn.io/msgvault/internal/apiprotocol"
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
	stubStartServeBackgroundProcess(t, func(*config.Config, backgroundServeStartOptions) (*backgroundServeProcess, error) {
		require.FailNow(t, "configured remote must not start a local daemon")
		return nil, errors.New("unreachable")
	})

	st, info, err := OpenHTTPStore(context.Background())
	require.NoError(t, err, "OpenHTTPStore")
	t.Cleanup(func() { _ = st.Close() })

	assert.Equal(t, HTTPStoreConfiguredRemote, info.Kind)
	assert.Equal(t, "http://daemonclient.example:8080", info.URL)
}

func TestOpenHTTPStoreUsesCLIModeForConfiguredRemote(t *testing.T) {
	var marker atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/health", r.URL.Path)
		assert.Equal(t, "remote-daemon-secret", r.Header.Get("X-Api-Key"))
		marker.Store(r.Header.Get(apiprotocol.ClientClassHeader))
		w.Header().Set("Content-Type", "application/json")
		_, err := w.Write([]byte(`{"status":"ok"}`))
		assert.NoError(t, err, "write health response")
	}))
	t.Cleanup(srv.Close)

	withStoreResolverConfig(t, &config.Config{
		Remote: config.RemoteConfig{
			URL:           srv.URL,
			APIKey:        "remote-daemon-secret",
			AllowInsecure: true,
		},
	})

	st, _, err := OpenHTTPStore(context.Background())
	require.NoError(t, err, "OpenHTTPStore")
	t.Cleanup(func() { _ = st.Close() })

	assert.Zero(t, st.Timeout(), "configured remote operations use caller duration")
	_, err = st.GetHealth(context.Background())
	require.NoError(t, err, "GetHealth")
	assert.Equal(t, apiprotocol.ClientClassCLI, marker.Load())
}

func TestOpenHTTPStoreRootContextCancelsLocalDaemonRequest(t *testing.T) {
	var marker atomic.Value
	requestCanceled := make(chan struct{})
	mux := http.NewServeMux()
	mux.Handle("/api/ping", daemon.NewPingHandler(daemon.PingHandlerOptions{
		Service: daemonService,
		Version: Version,
	}))
	mux.HandleFunc("/api/v1/health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, err := w.Write([]byte(`{"status":"ok"}`))
		assert.NoError(t, err, "write health response")
	})
	mux.HandleFunc("/api/v1/stats", func(_ http.ResponseWriter, r *http.Request) {
		marker.Store(r.Header.Get(apiprotocol.ClientClassHeader))
		<-r.Context().Done()
		close(requestCanceled)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	dataDir := t.TempDir()
	withStoreResolverConfig(t, lifecycleTestConfig(dataDir))
	rt := daemonRuntimeForHTTPServer(t, srv, daemonAPIKeyFingerprint(""))
	_, err := daemonRuntimeStore(dataDir).Write(rt.Record)
	require.NoError(t, err, "write daemon runtime")

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	st, _, err := OpenHTTPStore(ctx)
	require.NoError(t, err, "OpenHTTPStore")
	t.Cleanup(func() { _ = st.Close() })
	assert.Zero(t, st.Timeout(), "local daemon operations use caller duration")

	done := make(chan error, 1)
	go func() {
		_, err := st.GetStats()
		done <- err
	}()
	require.Eventually(t, func() bool {
		return marker.Load() != nil
	}, 2*time.Second, 10*time.Millisecond, "stats request starts")
	cancel()

	require.Eventually(t, func() bool {
		select {
		case <-requestCanceled:
			return true
		default:
			return false
		}
	}, 2*time.Second, 10*time.Millisecond, "root cancellation reaches stats request")
	assert.Equal(t, apiprotocol.ClientClassCLI, marker.Load())
	require.Error(t, <-done, "canceled stats request")
}

func TestOpenHTTPStoreStartsLocalDaemonWhenNoRemoteConfigured(t *testing.T) {
	assert := assert.New(t)

	dataDir := t.TempDir()
	withStoreResolverConfig(t, lifecycleTestConfig(dataDir))
	waitCh := make(chan error)
	var started bool
	stubStartServeBackgroundProcess(t, func(c *config.Config, _ backgroundServeStartOptions) (*backgroundServeProcess, error) {
		started = true
		assert.Equal(dataDir, c.Data.DataDir)
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
		assert.Equal(dataDir, gotDataDir)
		assert.Greater(timeout, 30*time.Second)
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
	assert.True(started, "local daemon should be started")
	assert.Equal(HTTPStoreLocalDaemon, info.Kind)
	assert.Equal("http://127.0.0.1:9911", info.URL)
}

func TestOpenHTTPStoreReportsLocalDaemonStartupToStderr(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	dataDir := t.TempDir()
	withStoreResolverConfig(t, lifecycleTestConfig(dataDir))
	waitCh := make(chan error)
	stubStartServeBackgroundProcess(t, func(*config.Config, backgroundServeStartOptions) (*backgroundServeProcess, error) {
		return &backgroundServeProcess{
			PID:     4242,
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
			Record: daemon.RuntimeRecord{PID: 4242},
			Host:   "127.0.0.1",
			Port:   9911,
			API:    daemonAPIVersion,
		}, true, nil
	})

	var st *daemonclient.Client
	var err error
	stderr := captureStderrDuring(t, func() {
		st, _, err = OpenHTTPStore(context.Background())
	})
	require.NoError(err, "OpenHTTPStore")
	t.Cleanup(func() { _ = st.Close() })

	assert.Contains(stderr, "Starting local msgvault daemon")
	assert.Contains(stderr, "pid 4242")
	assert.Contains(stderr, "Logs: /tmp/msgvault-serve.log")
	assert.NotContains(stderr, "Waiting for the daemon to become ready",
		"fast startups must not print the slow-start preamble")
}

func TestOpenHTTPStoreIncludesLastDaemonLogWhenStartupExits(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	dataDir := t.TempDir()
	withStoreResolverConfig(t, lifecycleTestConfig(dataDir))
	logPath := filepath.Join(dataDir, "serve.log")
	require.NoError(os.WriteFile(logPath, []byte("Error: API server address unavailable at 127.0.0.1:8080\n"), 0o600), "write serve log")
	waitCh := make(chan error)
	stubStartServeBackgroundProcess(t, func(*config.Config, backgroundServeStartOptions) (*backgroundServeProcess, error) {
		return &backgroundServeProcess{
			PID:     4242,
			LogPath: logPath,
			Wait:    waitCh,
		}, nil
	})
	stubWaitForBackgroundServeReady(t, func(
		context.Context,
		string,
		<-chan error,
		time.Duration,
	) (*DaemonRuntime, bool, error) {
		return nil, false, errors.New("exit status 1")
	})

	st, _, err := OpenHTTPStore(context.Background())
	if st != nil {
		t.Cleanup(func() { _ = st.Close() })
	}

	require.Error(err, "OpenHTTPStore")
	assert.Contains(err.Error(), "exit status 1")
	assert.Contains(err.Error(), "Last log: Error: API server address unavailable at 127.0.0.1:8080")
	assert.Contains(err.Error(), "Logs: "+logPath)
}

func TestOpenHTTPStoreTakesOverWhenConcurrentDaemonStartExits(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	dataDir := t.TempDir()
	withStoreResolverConfig(t, lifecycleTestConfig(dataDir))
	heldLock, ok := acquireBackgroundLaunchLock(dataDir)
	require.True(ok, "test should hold background launch lock")
	t.Cleanup(func() { _ = heldLock.Unlock() })
	time.AfterFunc(50*time.Millisecond, func() {
		_ = heldLock.Unlock()
	})

	started := make(chan struct{})
	waitCh := make(chan error)
	stubStartServeBackgroundProcess(t, func(*config.Config, backgroundServeStartOptions) (*backgroundServeProcess, error) {
		close(started)
		return &backgroundServeProcess{
			PID:     4242,
			LogPath: filepath.Join(dataDir, "serve.log"),
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
			Record: daemon.RuntimeRecord{PID: 4242},
			Host:   "127.0.0.1",
			Port:   9911,
			API:    daemonAPIVersion,
		}, true, nil
	})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	st, info, err := OpenHTTPStore(ctx)
	require.NoError(err, "OpenHTTPStore")
	t.Cleanup(func() { _ = st.Close() })

	assert.Equal(HTTPStoreLocalDaemon, info.Kind)
	select {
	case <-started:
	case <-ctx.Done():
		require.Fail("local daemon was not started after launch lock released")
	}
}

func TestOpenHTTPStoreUsesServerAPIKeyForLocalDaemon(t *testing.T) {
	assert := assert.New(t)
	require := require.New(
		t)

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
	mux.HandleFunc("/api/v1/health", func(w http.ResponseWriter, r *http.Request) {
		gotAPIKey = r.Header.Get("X-Api-Key")
		if gotAPIKey != localCfg.Server.APIKey {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	mux.HandleFunc("/api/v1/stats", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"total_messages":7}`))
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	host, portText, err := net.SplitHostPort(server.Listener.Addr().String())
	require.NoError(
		err, "split listener address")

	port, err := strconv.Atoi(portText)
	require.NoError(
		err, "parse listener port")

	_, err = daemonRuntimeStore(dataDir).Write(daemon.RuntimeRecord{
		PID:     os.Getpid(),
		Network: daemon.NetworkTCP,
		Address: net.JoinHostPort(host, portText),
		Service: daemonService,
		Version: Version,
		Metadata: map[string]string{
			runtimeHost:            host,
			runtimePort:            strconv.Itoa(port),
			runtimeAPIVersion:      strconv.Itoa(daemonAPIVersion),
			runtimeAuthFingerprint: daemonAPIKeyFingerprint(localCfg.Server.APIKey),
		},
	})
	require.NoError(
		err, "write runtime")

	st, info, err := OpenHTTPStore(context.Background())
	require.NoError(
		err, "OpenHTTPStore")

	t.Cleanup(func() { _ = st.Close() })

	stats, err := st.GetStats()
	require.NoError(
		err, "GetStats")

	assert.Equal(HTTPStoreLocalDaemon, info.Kind)
	assert.Equal(int64(7), stats.MessageCount)
	assert.Equal(localCfg.Server.APIKey, gotAPIKey)
}

func TestOpenHTTPStoreRejectsLocalDaemonWithStaleServerAPIKey(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	dataDir := t.TempDir()
	localCfg := lifecycleTestConfig(dataDir)
	localCfg.Server.APIKey = "new-local-daemon-secret"
	withStoreResolverConfig(t, localCfg)

	var statsCalled bool
	mux := http.NewServeMux()
	mux.Handle("/api/ping", daemon.NewPingHandler(daemon.PingHandlerOptions{
		Service: daemonService,
		Version: Version,
	}))
	mux.HandleFunc("/api/v1/health", func(w http.ResponseWriter, r *http.Request) {
		statsCalled = true
		assert.Equal(localCfg.Server.APIKey, r.Header.Get("X-Api-Key"), "auth probe uses current server api key")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"unauthorized","message":"Invalid or missing API key"}`))
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
			runtimeHost:            host,
			runtimePort:            strconv.Itoa(port),
			runtimeAPIVersion:      strconv.Itoa(daemonAPIVersion),
			runtimeAuthFingerprint: daemonAPIKeyFingerprint(localCfg.Server.APIKey),
		},
	})
	require.NoError(err, "write runtime")

	st, _, err := OpenHTTPStore(context.Background())
	if st != nil {
		t.Cleanup(func() { _ = st.Close() })
	}

	require.Error(err, "OpenHTTPStore should reject a daemon using stale authentication")
	assert.Contains(err.Error(), "api_key", "error names the key mismatch")
	assert.Contains(err.Error(), "msgvault daemon restart", "error gives a daemon lifecycle remedy")
	assert.True(statsCalled, "runtime reuse should probe an authenticated endpoint")
}

func TestOpenHTTPStoreRejectsLocalDaemonWithChangedServerAPIKeyFingerprint(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	dataDir := t.TempDir()
	localCfg := lifecycleTestConfig(dataDir)
	localCfg.Server.APIKey = "new-local-daemon-secret"
	withStoreResolverConfig(t, localCfg)

	var statsCalled bool
	mux := http.NewServeMux()
	mux.Handle("/api/ping", daemon.NewPingHandler(daemon.PingHandlerOptions{
		Service: daemonService,
		Version: Version,
	}))
	mux.HandleFunc("/api/v1/health", func(w http.ResponseWriter, _ *http.Request) {
		statsCalled = true
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
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
			runtimeHost:            host,
			runtimePort:            strconv.Itoa(port),
			runtimeAPIVersion:      strconv.Itoa(daemonAPIVersion),
			runtimeAuthFingerprint: daemonAPIKeyFingerprint("old-local-daemon-secret"),
		},
	})
	require.NoError(err, "write runtime")

	st, _, err := OpenHTTPStore(context.Background())
	if st != nil {
		t.Cleanup(func() { _ = st.Close() })
	}

	require.Error(err, "OpenHTTPStore should reject a daemon started with a different api key")
	assert.Contains(err.Error(), "api_key", "error names the key mismatch")
	assert.Contains(err.Error(), "msgvault daemon restart", "error gives a daemon lifecycle remedy")
	assert.False(statsCalled, "runtime reuse should reject stale auth metadata before routed requests")
}

func TestOpenHTTPStoreRejectsLegacyLocalDaemonAfterServerAPIKeyRemoved(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	dataDir := t.TempDir()
	localCfg := lifecycleTestConfig(dataDir)
	withStoreResolverConfig(t, localCfg)

	var statsCalled bool
	mux := http.NewServeMux()
	mux.Handle("/api/ping", daemon.NewPingHandler(daemon.PingHandlerOptions{
		Service: daemonService,
		Version: Version,
	}))
	mux.HandleFunc("/api/v1/health", func(w http.ResponseWriter, r *http.Request) {
		statsCalled = true
		assert.Empty(r.Header.Get("X-Api-Key"), "removed api key should probe without credentials")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"unauthorized","message":"Invalid or missing API key"}`))
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
			runtimeHost:       host,
			runtimePort:       strconv.Itoa(port),
			runtimeAPIVersion: strconv.Itoa(daemonAPIVersion),
		},
	})
	require.NoError(err, "write runtime")

	st, _, err := OpenHTTPStore(context.Background())
	if st != nil {
		t.Cleanup(func() { _ = st.Close() })
	}

	require.Error(err, "OpenHTTPStore should reject a legacy daemon that still requires an api key")
	assert.Contains(err.Error(), "api_key", "error names the key mismatch")
	assert.Contains(err.Error(), "msgvault daemon restart", "error gives a daemon lifecycle remedy")
	assert.True(statsCalled, "missing auth metadata should be verified with a live probe")
}

func TestProbeLocalDaemonAuthDoesNotWaitForStats(t *testing.T) {
	var statsCalled atomic.Bool
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/health", func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "secret", r.Header.Get("X-Api-Key"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	mux.HandleFunc("/api/v1/stats", func(w http.ResponseWriter, _ *http.Request) {
		statsCalled.Store(true)
		time.Sleep(3 * localDaemonAuthProbeTimeout)
		w.WriteHeader(http.StatusOK)
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	rt := daemonRuntimeForHTTPServer(t, server, daemonAPIKeyFingerprint("secret"))
	c := lifecycleTestConfig(t.TempDir())
	c.Server.APIKey = "secret"

	start := time.Now()
	require.NoError(t, probeLocalDaemonAuth(context.Background(), rt, c))
	assert.Less(t, time.Since(start), localDaemonAuthProbeTimeout)
	assert.False(t, statsCalled.Load(), "auth probe never performs archive statistics")
}

func TestProbeLocalDaemonAuthRespectsParentDeadline(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/health", func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(3 * localDaemonAuthProbeTimeout):
		}
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	rt := daemonRuntimeForHTTPServer(t, server, daemonAPIKeyFingerprint("secret"))
	c := lifecycleTestConfig(t.TempDir())
	c.Server.APIKey = "secret"
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	t.Cleanup(cancel)

	err := probeLocalDaemonAuth(ctx, rt, c)
	require.Error(t, err)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Contains(t, err.Error(), "probe local daemon authentication")
}

func daemonRuntimeForHTTPServer(t *testing.T, server *httptest.Server, authFingerprint string) *DaemonRuntime {
	t.Helper()
	host, portText, err := net.SplitHostPort(server.Listener.Addr().String())
	require.NoError(t, err, "split listener address")
	port, err := strconv.Atoi(portText)
	require.NoError(t, err, "parse listener port")

	return &DaemonRuntime{
		Record: daemon.RuntimeRecord{
			PID:     os.Getpid(),
			Network: daemon.NetworkTCP,
			Address: net.JoinHostPort(host, portText),
			Service: daemonService,
			Version: Version,
			Metadata: map[string]string{
				runtimeHost:            host,
				runtimePort:            portText,
				runtimeAPIVersion:      strconv.Itoa(daemonAPIVersion),
				runtimeAuthFingerprint: authFingerprint,
			},
		},
		Host: host,
		Port: port,
		API:  daemonAPIVersion,
	}
}

func TestOpenHTTPStoreHonorsNeverAutoRestartPolicy(t *testing.T) {
	assert := assert.New(t)
	require := require.New(
		t)

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
	mux.HandleFunc("/api/v1/health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	host, portText, err := net.SplitHostPort(server.Listener.Addr().String())
	require.NoError(
		err, "split listener address")

	port, err := strconv.Atoi(portText)
	require.NoError(
		err, "parse listener port")

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
	require.NoError(
		err, "write runtime")

	stubStartServeBackgroundProcess(t, func(*config.Config, backgroundServeStartOptions) (*backgroundServeProcess, error) {
		require.FailNow("never policy must not start over a compatible daemon")
		return nil, errors.New("unreachable")
	})

	st, info, err := OpenHTTPStore(context.Background())
	require.NoError(
		err, "OpenHTTPStore")

	t.Cleanup(func() { _ = st.Close() })

	stats, err := st.GetStats()
	require.NoError(
		err, "GetStats")

	assert.Equal(HTTPStoreLocalDaemon, info.Kind)
	assert.Equal(int64(9), stats.MessageCount)
}

func TestOpenHTTPStoreLocalFlagUsesLocalDaemonInsteadOfConfiguredRemote(t *testing.T) {
	assert := assert.New(t)

	dataDir := t.TempDir()
	c := lifecycleTestConfig(dataDir)
	c.Remote.URL = "http://daemonclient.example:8080"
	c.Remote.AllowInsecure = true
	withStoreResolverConfig(t, c)
	useLocal = true

	waitCh := make(chan error)
	var started bool
	stubStartServeBackgroundProcess(t, func(got *config.Config, _ backgroundServeStartOptions) (*backgroundServeProcess, error) {
		started = true
		assert.Equal(dataDir, got.Data.DataDir)
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
		assert.Equal(dataDir, gotDataDir)
		assert.Greater(timeout, 30*time.Second)
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
	assert.True(started, "--local should start/use the local daemon")
	assert.Equal(HTTPStoreLocalDaemon, info.Kind)
	assert.Equal("http://127.0.0.1:9911", info.URL)
}

func TestWaitForUsableBackgroundRuntimeReturnsLockWhenNoDaemon(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	dataDir := t.TempDir()

	rt, lock, err := waitForUsableBackgroundRuntimeOrLaunchLock(
		context.Background(), dataDir, config.DaemonAutoRestartNewer, time.Second,
	)
	require.NoError(err, "wait should not error when no daemon is running")
	assert.Nil(rt, "no runtime")
	require.NotNil(lock, "should acquire launch lock when no daemon is starting")
	_ = lock.Unlock()
}

func TestWaitForUsableBackgroundRuntimeWaitsWhileChildInitializing(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	dataDir := t.TempDir()

	// A live process (this test) owns a runtime record whose recorded
	// endpoint is not answering the daemon ping — i.e. a `daemon start`
	// child that is still initializing after its parent released the lock.
	_, err := daemonRuntimeStore(dataDir).Write(daemon.RuntimeRecord{
		PID:     os.Getpid(),
		Network: daemon.NetworkTCP,
		Address: "127.0.0.1:1",
		Service: daemonService,
		Version: Version,
		Metadata: map[string]string{
			runtimeHost:       "127.0.0.1",
			runtimePort:       "1",
			runtimeAPIVersion: strconv.Itoa(daemonAPIVersion),
		},
	})
	require.NoError(err, "write runtime record")

	rt, lock, err := waitForUsableBackgroundRuntimeOrLaunchLock(
		context.Background(), dataDir, config.DaemonAutoRestartNewer, 750*time.Millisecond,
	)
	require.NoError(err, "wait should reach the timeout path without error")
	assert.Nil(rt, "initializing child is not yet usable")
	assert.Nil(lock, "must not hand out the launch lock while a child is starting")
}

// TestDaemonStartInProgressDetectsInitializingChild verifies the predicate the
// launch-lock guard relies on: a live process holding a runtime record that is
// not answering the daemon ping (a `daemon start` child still initializing)
// reports in-progress, so both the direct and waited launch-lock acquisition
// paths refuse to spawn a duplicate daemon. An empty data dir reports false.
func TestDaemonStartInProgressDetectsInitializingChild(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	empty := t.TempDir()
	inProgress, err := daemonStartInProgress(context.Background(), empty)
	require.NoError(err, "no records should not error")
	assert.False(inProgress, "no runtime records means no start in progress")

	dataDir := t.TempDir()
	_, err = daemonRuntimeStore(dataDir).Write(daemon.RuntimeRecord{
		PID:     os.Getpid(),
		Network: daemon.NetworkTCP,
		Address: "127.0.0.1:1",
		Service: daemonService,
		Version: Version,
		Metadata: map[string]string{
			runtimeHost:       "127.0.0.1",
			runtimePort:       "1",
			runtimeAPIVersion: strconv.Itoa(daemonAPIVersion),
		},
	})
	require.NoError(err, "write runtime record")

	inProgress, err = daemonStartInProgress(context.Background(), dataDir)
	require.NoError(err, "live-but-unready record should not error")
	assert.True(inProgress, "an initializing child must report a start in progress")
}

func TestWaitForUsableBackgroundRuntimeTakesOverUpgradeEligibleDaemon(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	withTestVersion(t, "v1.1.0")
	dataDir := t.TempDir()
	ping := httptestPingDaemon(t)

	// An older, ping-responding daemon is eligible for upgrade under the
	// "newer" policy, so takeover is allowed and a lock is returned.
	_, err := daemonRuntimeStore(dataDir).Write(daemon.RuntimeRecord{
		PID:     os.Getpid(),
		Network: daemon.NetworkTCP,
		Address: net.JoinHostPort(ping.Host, strconv.Itoa(ping.Port)),
		Service: daemonService,
		Version: "v1.0.0",
		Metadata: map[string]string{
			runtimeHost:       ping.Host,
			runtimePort:       strconv.Itoa(ping.Port),
			runtimeAPIVersion: strconv.Itoa(daemonAPIVersion),
		},
	})
	require.NoError(err, "write runtime record")

	rt, lock, err := waitForUsableBackgroundRuntimeOrLaunchLock(
		context.Background(), dataDir, config.DaemonAutoRestartNewer, time.Second,
	)
	require.NoError(err, "wait should not error")
	assert.Nil(rt, "upgrade-eligible daemon must not be returned as usable")
	require.NotNil(lock, "ping-responding upgrade-eligible daemon should allow takeover")
	_ = lock.Unlock()
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

func captureStderrDuring(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stderr
	r, w, err := os.Pipe()
	require.NoError(t, err, "create stderr pipe")
	os.Stderr = w
	t.Cleanup(func() { os.Stderr = old })

	fn()

	require.NoError(t, w.Close(), "close stderr writer")
	os.Stderr = old
	var buf bytes.Buffer
	_, err = io.Copy(&buf, r)
	require.NoError(t, err, "read stderr")
	require.NoError(t, r.Close(), "close stderr reader")
	return buf.String()
}
