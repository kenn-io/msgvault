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
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/daemon"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonauth"
)

func TestDaemonAndServeLifecycleCommandSurfaces(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	daemonNames := map[string]bool{}
	for _, sub := range daemonCmd.Commands() {
		daemonNames[sub.Name()] = true
		assert.False(sub.Hidden, "daemon %s must be visible", sub.Name())
	}
	for _, name := range []string{"start", "status", "stop", "restart"} {
		assert.True(daemonNames[name], "daemon must expose %s", name)
		compat, _, err := serveCmd.Find([]string{name})
		require.NoError(err)
		assert.Equal(name, compat.Name())
		assert.True(compat.Hidden, "serve %s must be hidden", name)
	}
}

func TestDaemonAndServeStatusHaveIdenticalBehavior(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	dataDir := t.TempDir()
	oldCfg := cfg
	cfg = lifecycleTestConfig(dataDir)
	t.Cleanup(func() { cfg = oldCfg })

	run := func(args ...string) (string, error) {
		root := newTestRootCmd()
		root.SilenceUsage = true
		root.AddCommand(newDaemonCommand())
		compatServe := &cobra.Command{Use: "serve"}
		addServeLifecycleCommands(compatServe)
		root.AddCommand(compatServe)
		var stdout bytes.Buffer
		root.SetOut(&stdout)
		root.SetErr(io.Discard)
		root.SetArgs(args)
		err := root.ExecuteContext(context.Background())
		return stdout.String(), err
	}

	daemonOut, daemonErr := run("daemon", "status")
	serveOut, serveErr := run("serve", "status")
	require.NoError(daemonErr)
	require.NoError(serveErr)
	assert.Equal(daemonOut, serveOut)
	assert.Equal("No msgvault daemon is running.\n", daemonOut)
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
			assert := assert.New(t)
			require := require.New(t)
			mux := http.NewServeMux()
			mux.HandleFunc("/api/v1/health", func(w http.ResponseWriter, r *http.Request) {
				assert.Equal("/api/v1/health", r.URL.Path)
				w.WriteHeader(http.StatusNotFound)
			})
			mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
				assert.Equal("/health", r.URL.Path)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tt.health))
			})
			srv := httptest.NewServer(mux)
			defer srv.Close()

			health := fetchDaemonHealth(context.Background(), srv.URL)
			require.NotNil(health, "health response")
			lines := vectorStatusLines(health.Vector)
			if tt.wantNone {
				assert.Empty(lines)
				return
			}
			require.Len(lines, 1)
			assert.Contains(lines[0], tt.wantLine)
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
	startedAt := time.Now().Add(-14 * time.Minute).UTC().Format(time.RFC3339)
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok","vector":{"status":"initializing"},` +
			`"operation":{"busy":true,"label":"background embedding work","started_at":"` + startedAt + `"}}`))
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
			runtimeCreateTime:       matchingProcessCreateTime(t),
		},
	})
	require.NoError(err, "write runtime record")

	cmd, stdout, stderr := lifecycleTestCommand()
	cmd.SetContext(context.Background())
	require.NoError(runServeStatus(cmd, dataDir), "runServeStatus")

	out := stdout.String()
	assert.Contains(out, "msgvault running at", "status shows the running daemon")
	assert.Contains(out, "vector:  initializing", "status includes daemon vector health")
	assert.Contains(out, "busy:    background embedding work (running for 14m",
		"status includes the active archive operation")
	assert.Empty(stderr.String(), "status must not write to stderr")
}

func TestServeStatusCommandUsesAuthenticatedHealthForOperationDetails(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	dataDir := t.TempDir()

	mux := http.NewServeMux()
	mux.Handle("/api/ping", daemon.NewPingHandler(daemon.PingHandlerOptions{
		Service: daemonService,
		Version: Version,
	}))
	startedAt := time.Now().Add(-14 * time.Minute).UTC().Format(time.RFC3339)
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok","operation":{"busy":true}}`))
	})
	var gotAPIKey string
	mux.HandleFunc("/api/v1/health", func(w http.ResponseWriter, r *http.Request) {
		gotAPIKey = r.Header.Get("X-Api-Key")
		if gotAPIKey != "secret-key" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok","operation":{"busy":true,` +
			`"label":"background embedding work","started_at":"` + startedAt + `"}}`))
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
			runtimeAuthFingerprint:  daemonAPIKeyFingerprint("secret-key"),
			runtimeCreateTime:       matchingProcessCreateTime(t),
		},
	})
	require.NoError(err, "write runtime record")

	oldCfg := cfg
	cfg = lifecycleTestConfig(dataDir)
	cfg.Server.APIKey = "secret-key"
	t.Cleanup(func() { cfg = oldCfg })

	cmd, stdout, stderr := lifecycleTestCommand()
	cmd.SetContext(context.Background())
	statusCmd, _, err := serveCmd.Find([]string{"status"})
	require.NoError(err, "find serve status")
	require.NoError(statusCmd.RunE(cmd, nil), "serve status")

	out := stdout.String()
	assert.Equal("secret-key", gotAPIKey, "authenticated health API key")
	assert.Contains(out, "busy:    background embedding work (running for 14m",
		"status includes the detailed active archive operation")
	assert.NotContains(out, "archive operation in progress",
		"status must not fall back to redacted public health when authenticated health is available")
	assert.Empty(stderr.String(), "status must not write to stderr")
}

func TestFetchDaemonOperationUsesAuthenticatedHealth(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok","operation":{"busy":true}}`))
	})
	var gotAPIKey string
	startedAt := time.Now().Add(-14 * time.Minute).UTC().Format(time.RFC3339)
	mux.HandleFunc("/api/v1/health", func(w http.ResponseWriter, r *http.Request) {
		gotAPIKey = r.Header.Get("X-Api-Key")
		if gotAPIKey != "secret-key" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok","operation":{"busy":true,` +
			`"label":"background embedding work","started_at":"` + startedAt + `"}}`))
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	host, portText, err := net.SplitHostPort(server.Listener.Addr().String())
	require.NoError(err, "split listener address")

	op := fetchDaemonOperation(daemon.RuntimeRecord{
		PID:     os.Getpid(),
		Network: daemon.NetworkTCP,
		Address: net.JoinHostPort(host, portText),
		Service: daemonService,
		Metadata: map[string]string{
			runtimeHost: host,
			runtimePort: portText,
		},
	}, "secret-key")

	require.NotNil(op, "operation")
	assert.Equal("secret-key", gotAPIKey, "authenticated health API key")
	assert.True(op.Busy, "busy")
	assert.Equal("background embedding work", op.Label)
	require.NotNil(op.StartedAt, "started_at")
	assert.WithinDuration(time.Now().Add(-14*time.Minute), *op.StartedAt, time.Minute)
}

func TestRunServeStatusReportsStartupPhase(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	dataDir := t.TempDir()
	_, err := daemonRuntimeStore(dataDir).Write(daemon.RuntimeRecord{
		PID:     os.Getpid(),
		Network: daemon.NetworkTCP,
		Address: "127.0.0.1:1",
		Service: daemonService,
		Version: "v-test",
		Metadata: map[string]string{
			runtimeStartupPhase: "building analytics cache",
		},
	})
	require.NoError(err, "write starting runtime record")

	cmd, stdout, stderr := lifecycleTestCommand()
	require.NoError(runServeStatus(cmd, dataDir), "runServeStatus")

	assert.Contains(stdout.String(),
		"msgvault daemon starting (pid "+strconv.Itoa(os.Getpid())+"): building analytics cache",
		"starting line")
	assert.Contains(stdout.String(), "elapsed:", "elapsed line")
	assert.NotContains(stdout.String(), "not responding to daemon ping", "legacy unresponsive line")
	assert.Empty(stderr.String())
}

func TestRunServeStatusKeepsInitializingRecordWhenOwnershipHeldDespiteCreateTimeMismatch(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	dataDir := t.TempDir()
	owner, err := tryAcquireDaemonOwnerLock(dataDir)
	require.NoError(err, "acquire daemon ownership")
	t.Cleanup(func() { require.NoError(owner.Close(), "release daemon ownership") })
	stubProcessCreateTimeMillis(t, func(int) (int64, bool) { return 1_000, true })

	_, err = daemonRuntimeStore(dataDir).Write(daemon.RuntimeRecord{
		PID:     os.Getpid(),
		Network: daemon.NetworkTCP,
		Address: "127.0.0.1:1",
		Service: daemonService,
		Version: "v-test",
		Metadata: map[string]string{
			runtimeCreateTime:   "10000",
			runtimeStartupPhase: "migrating archive schema",
		},
	})
	require.NoError(err, "write starting runtime record")

	cmd, stdout, stderr := lifecycleTestCommand()
	require.NoError(runServeStatus(cmd, dataDir), "runServeStatus")

	assert.Contains(stdout.String(),
		"msgvault daemon starting (pid "+strconv.Itoa(os.Getpid())+"): migrating archive schema",
		"held ownership keeps the initializing record visible")
	assert.Empty(stderr.String())
}

func TestStopTargetRequiresProcessIdentityDespiteRespondingPing(t *testing.T) {
	require := require.New(t)

	server := httptest.NewServer(daemon.NewPingHandler(daemon.PingHandlerOptions{
		Service: daemonService,
		Version: Version,
	}))
	t.Cleanup(server.Close)
	stubProcessCreateTimeMillis(t, func(int) (int64, bool) { return 1_000, true })

	rec := daemon.RuntimeRecord{
		PID:     os.Getpid(),
		Network: daemon.NetworkTCP,
		Address: server.Listener.Addr().String(),
		Service: daemonService,
		Metadata: map[string]string{
			runtimeCreateTime: "10000",
		},
	}

	info, err := probeDaemonRuntimeRecord(context.Background(), rec)
	require.NoError(err, "precondition: recorded endpoint responds to daemon ping")
	require.Equal(rec.PID, info.PID, "precondition: ping claims the recorded pid")
	require.False(stopTargetConfirmed(rec),
		"unauthenticated ping must not authorize signaling a reused PID")
}

func TestStopDaemonRuntimeRecordRejectsConfirmedProcessIdentityMismatch(t *testing.T) {
	require := require.New(t)
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(server.Close)
	host, portText, err := net.SplitHostPort(server.Listener.Addr().String())
	require.NoError(err, "split listener address")
	stubProcessCreateTimeMillis(t, func(int) (int64, bool) { return 1_000, true })

	rec := daemon.RuntimeRecord{
		PID:     os.Getpid(),
		Network: daemon.NetworkTCP,
		Address: net.JoinHostPort(host, portText),
		Service: daemonService,
		Metadata: map[string]string{
			runtimeHost:          host,
			runtimePort:          portText,
			runtimeCreateTime:    "10000",
			runtimeShutdownToken: "private-runtime-secret",
		},
	}

	err = stopDaemonRuntimeRecord(io.Discard, t.TempDir(), rec, "configured-api-key", time.Second)

	require.ErrorIs(err, errDaemonIdentityUnconfirmed, "mismatched process must be rejected")
	require.ErrorContains(err, "belongs to a different process")
	assert.Zero(t, requests.Load(), "mismatched records must not reach HTTP shutdown or auth endpoints")
}

func TestStopLiveDaemonsUsesAuthenticatedHTTPWhenCreateTimeUnknown(t *testing.T) {
	testStopLiveDaemonsUsesAuthenticatedHTTP(t, "", 0, false, true)
}

func TestStopLiveDaemonsUsesAuthenticatedHTTPWhenCreateTimeSkewed(t *testing.T) {
	testStopLiveDaemonsUsesAuthenticatedHTTP(t, "6000", 5_000, true, true)
}

func TestStopLiveDaemonsUsesLegacyShutdownWhenCreateTimeUnknown(t *testing.T) {
	testStopLiveDaemonsUsesAuthenticatedHTTP(t, "", 0, false, false)
}

func TestStopLiveDaemonsUsesLegacyShutdownWhenCreateTimeSkewed(t *testing.T) {
	testStopLiveDaemonsUsesAuthenticatedHTTP(t, "6000", 5_000, true, false)
}

func testStopLiveDaemonsUsesAuthenticatedHTTP(
	t *testing.T,
	recordedCreateTime string,
	liveCreateTime int64,
	liveCreateTimeOK bool,
	identityEndpointSupported bool,
) {
	t.Helper()
	require := require.New(t)
	assert := assert.New(t)
	dataDir := t.TempDir()
	stubProcessCreateTimeMillis(t, func(int) (int64, bool) { return liveCreateTime, liveCreateTimeOK })
	const runtimeSecret = "private-runtime-secret"

	owner, err := tryAcquireDaemonOwnerLock(dataDir)
	require.NoError(err, "acquire daemon ownership")
	var releaseOwner sync.Once
	t.Cleanup(func() { releaseOwner.Do(func() { _ = owner.Close() }) })

	shutdownTokens := make(chan string, 1)
	var apiKeySent atomic.Bool
	ping := daemon.NewPingHandler(daemon.PingHandlerOptions{
		Service: daemonService,
		Version: Version,
	})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Api-Key") != "" {
			apiKeySent.Store(true)
		}
		switch r.URL.Path {
		case daemon.DefaultPingPath:
			ping.ServeHTTP(w, r)
		case api.DaemonIdentityPath:
			if !identityEndpointSupported {
				http.NotFound(w, r)
				return
			}
			proof, proofErr := daemonauth.Proof(runtimeSecret,
				r.Header.Get(api.DaemonIdentityChallengeHeader), os.Getpid())
			if proofErr != nil {
				http.Error(w, "invalid challenge", http.StatusBadRequest)
				return
			}
			w.Header().Set(api.DaemonIdentityProofHeader, proof)
			w.WriteHeader(http.StatusNoContent)
		case api.DaemonShutdownPath:
			shutdownTokens <- r.Header.Get(api.DaemonShutdownTokenHeader)
			w.WriteHeader(http.StatusAccepted)
			go func() {
				time.Sleep(25 * time.Millisecond)
				releaseOwner.Do(func() { _ = owner.Close() })
			}()
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	host, portText, err := net.SplitHostPort(server.Listener.Addr().String())
	require.NoError(err, "split listener address")
	metadata := map[string]string{
		runtimeHost:          host,
		runtimePort:          portText,
		runtimeAPIVersion:    strconv.Itoa(daemonAPIVersion),
		runtimeShutdownToken: runtimeSecret,
	}
	if recordedCreateTime != "" {
		metadata[runtimeCreateTime] = recordedCreateTime
	}

	_, err = daemonRuntimeStore(dataDir).Write(daemon.RuntimeRecord{
		PID:      os.Getpid(),
		Network:  daemon.NetworkTCP,
		Address:  net.JoinHostPort(host, portText),
		Service:  daemonService,
		Version:  Version,
		Metadata: metadata,
	})
	require.NoError(err, "write runtime record")
	cmd, stdout, _ := lifecycleTestCommand()

	require.NoError(stopLiveDaemonsWithAPIKey(cmd, dataDir, "configured-api-key", false),
		"stop daemon with indeterminate process identity")

	select {
	case got := <-shutdownTokens:
		assert.Equal(runtimeSecret, got, "authenticated shutdown token")
	default:
		assert.Fail("shutdown endpoint was not called")
	}
	ownershipHeld, ownershipErr := daemonOwnerLockHeld(dataDir)
	require.NoError(ownershipErr, "probe daemon ownership after stop")
	assert.False(ownershipHeld, "stop waits for daemon ownership release")
	assert.Contains(stdout.String(), "Stopped msgvault", "stop confirmation")
	if !identityEndpointSupported {
		assert.False(apiKeySent.Load(), "legacy shutdown must not transmit the API key")
	}
}

func TestRunServeRestartDoesNotLaunchOverInitializingIdentityMismatch(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	dataDir := t.TempDir()
	owner, err := tryAcquireDaemonOwnerLock(dataDir)
	require.NoError(err, "acquire daemon ownership")
	t.Cleanup(func() { require.NoError(owner.Close(), "release daemon ownership") })
	stubProcessCreateTimeMillis(t, func(int) (int64, bool) { return 1_000, true })

	_, err = daemonRuntimeStore(dataDir).Write(daemon.RuntimeRecord{
		PID:     os.Getpid(),
		Network: daemon.NetworkTCP,
		Address: "127.0.0.1:1",
		Service: daemonService,
		Version: Version,
		Metadata: map[string]string{
			runtimeCreateTime:   "10000",
			runtimeStartupPhase: "migrating archive schema",
		},
	})
	require.NoError(err, "write starting runtime record")

	started := false
	stubStartServeBackgroundProcess(t, func(*config.Config, backgroundServeStartOptions) (*backgroundServeProcess, error) {
		started = true
		return nil, errors.New("unexpected background launch")
	})
	cmd, _, _ := lifecycleTestCommand()

	err = runServeRestart(cmd, lifecycleTestConfig(dataDir))

	require.Error(err, "restart must stop when daemon ownership is already held")
	require.ErrorContains(err, "daemon lock", "error explains the ownership conflict")
	assert.False(started, "restart must not launch a duplicate daemon")
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
			runtimeCreateTime:       matchingProcessCreateTime(t),
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

func TestRecordedDaemonStillPresentTreatsToleranceSkewAsLive(t *testing.T) {
	stubProcessCreateTimeMillis(t, func(int) (int64, bool) { return 5_000, true })
	rec := daemon.RuntimeRecord{
		PID: os.Getpid(),
		Metadata: map[string]string{
			runtimeCreateTime: "6000",
		},
	}

	assert.True(t, recordedDaemonStillPresent(rec),
		"timestamp jitter must not make a signaled daemon look exited")
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
			runtimeCreateTime: matchingProcessCreateTime(t),
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

func TestRunServeStartRecognizesLegacyDaemonWithIndeterminateCreateTime(t *testing.T) {
	tests := []struct {
		name               string
		recordedCreateTime string
		liveCreateTime     int64
		liveCreateTimeOK   bool
	}{
		{name: "unknown", liveCreateTimeOK: false},
		{name: "skewed", recordedCreateTime: "6000", liveCreateTime: 5_000, liveCreateTimeOK: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			dataDir := t.TempDir()
			stubProcessCreateTimeMillis(t, func(int) (int64, bool) {
				return tt.liveCreateTime, tt.liveCreateTimeOK
			})

			owner, err := tryAcquireDaemonOwnerLock(dataDir)
			require.NoError(err, "acquire daemon ownership")
			t.Cleanup(func() { _ = owner.Close() })

			var apiKeySent atomic.Bool
			ping := daemon.NewPingHandler(daemon.PingHandlerOptions{
				Service: daemonService,
				Version: Version,
			})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("X-Api-Key") != "" {
					apiKeySent.Store(true)
				}
				if r.URL.Path == daemon.DefaultPingPath {
					ping.ServeHTTP(w, r)
					return
				}
				http.NotFound(w, r)
			}))
			t.Cleanup(server.Close)
			host, portText, err := net.SplitHostPort(server.Listener.Addr().String())
			require.NoError(err, "split listener address")
			metadata := map[string]string{
				runtimeHost:          host,
				runtimePort:          portText,
				runtimeAPIVersion:    strconv.Itoa(daemonAPIVersion),
				runtimeShutdownToken: "private-runtime-secret",
			}
			if tt.recordedCreateTime != "" {
				metadata[runtimeCreateTime] = tt.recordedCreateTime
			}
			_, err = daemonRuntimeStore(dataDir).Write(daemon.RuntimeRecord{
				PID:      os.Getpid(),
				Network:  daemon.NetworkTCP,
				Address:  net.JoinHostPort(host, portText),
				Service:  daemonService,
				Version:  Version,
				Metadata: metadata,
			})
			require.NoError(err, "write runtime record")
			stubStartServeBackgroundProcess(t, func(*config.Config, backgroundServeStartOptions) (*backgroundServeProcess, error) {
				require.FailNow("start must reuse the legacy daemon")
				return nil, errors.New("unreachable")
			})

			cmd, stdout, _ := lifecycleTestCommand()
			require.NoError(runServeStart(cmd, lifecycleTestConfig(dataDir)))
			assert.Contains(stdout.String(), "msgvault already running")
			assert.False(apiKeySent.Load(), "legacy discovery must not transmit the API key")
		})
	}
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
			runtimeCreateTime: matchingProcessCreateTime(t),
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
			runtimeCreateTime: matchingProcessCreateTime(t),
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
			runtimeCreateTime: matchingProcessCreateTime(t),
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
			runtimeCreateTime: matchingProcessCreateTime(t),
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
			runtimeCreateTime: matchingProcessCreateTime(t),
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
	assert.Contains(err.Error(), "msgvault daemon stop")
	assert.Empty(stdout.String())
	assert.Empty(stderr.String())
}

func TestReportBackgroundLaunchInProgressUsesCanonicalCommand(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cmd, stdout, _ := lifecycleTestCommand()
	cmd.SetContext(ctx)

	reportBackgroundLaunchInProgress(cmd, t.TempDir())

	assert.Equal(t, "msgvault daemon start is already in progress.\n", stdout.String())
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

func TestRunServeStartNotReadyPrintsWebUIURLForFixedPort(t *testing.T) {
	dataDir := t.TempDir()
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
		return nil, false, nil
	})
	cmd, stdout, stderr := lifecycleTestCommand()

	require.NoError(t, runServeStart(cmd, lifecycleTestConfig(dataDir)))

	assert.Equal(t,
		"msgvault starting in background (pid 777)\n"+
			"Web UI: http://127.0.0.1:8080\n"+
			"Logs: /tmp/msgvault-serve.log\n",
		stdout.String())
	assert.Empty(t, stderr.String())
}

func TestRunServeStartNotReadyPointsAtStatusForAutoPort(t *testing.T) {
	dataDir := t.TempDir()
	waitCh := make(chan error)
	stubStartServeBackgroundProcess(t, func(*config.Config, backgroundServeStartOptions) (*backgroundServeProcess, error) {
		return &backgroundServeProcess{
			PID:     776,
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
		return nil, false, nil
	})
	cfg := lifecycleTestConfig(dataDir)
	cfg.Server.APIPort = 0
	cmd, stdout, stderr := lifecycleTestCommand()

	require.NoError(t, runServeStart(cmd, cfg))

	assert.Equal(t,
		"msgvault starting in background (pid 776)\n"+
			"Web UI: run `msgvault daemon status` for the URL once ready\n"+
			"Logs: /tmp/msgvault-serve.log\n",
		stdout.String())
	assert.Empty(t, stderr.String())
}

func TestStopBackgroundServeStartupTerminatesProcess(t *testing.T) {
	cmd := helperProcessCommand(context.Background(), "block")
	require.NoError(t, cmd.Start(), "start blocking helper")
	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()
	proc := &backgroundServeProcess{
		PID:     cmd.Process.Pid,
		Process: cmd.Process,
		Wait:    waitCh,
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })

	err := stopBackgroundServeStartup(proc, 2*time.Second)

	require.NoError(t, err)
}

type recordingBackgroundProcessTree struct {
	terminateCalls atomic.Int32
	closeCalls     atomic.Int32
	terminate      func() error
}

func (t *recordingBackgroundProcessTree) Attach(*os.Process) error {
	return nil
}

func (t *recordingBackgroundProcessTree) Terminate() error {
	t.terminateCalls.Add(1)
	return t.terminate()
}

func (t *recordingBackgroundProcessTree) Close() error {
	t.closeCalls.Add(1)
	return nil
}

func TestStopBackgroundServeStartupTerminatesProcessTree(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	cmd := helperProcessCommand(context.Background(), "block")
	require.NoError(cmd.Start(), "start blocking helper")
	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()
	tree := &recordingBackgroundProcessTree{terminate: cmd.Process.Kill}
	proc := &backgroundServeProcess{
		PID:         cmd.Process.Pid,
		Process:     cmd.Process,
		ProcessTree: tree,
		Wait:        waitCh,
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })

	require.NoError(stopBackgroundServeStartup(proc, 2*time.Second))
	assert.Equal(int32(1), tree.terminateCalls.Load(), "terminate process tree")
	assert.Equal(int32(1), tree.closeCalls.Load(), "close process tree handle")
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

func restoreStopWaitPacing(t *testing.T, quiet, interval time.Duration) {
	t.Helper()
	oldQuiet, oldInterval := serveStopQuietWindow, serveStopProgressInterval
	serveStopQuietWindow, serveStopProgressInterval = quiet, interval
	t.Cleanup(func() {
		serveStopQuietWindow, serveStopProgressInterval = oldQuiet, oldInterval
	})
}

func TestDescribeDaemonStopWaitWithOperation(t *testing.T) {
	assert := assert.New(t)
	startedAt := time.Now().Add(-14 * time.Minute)

	out := describeDaemonStopWait(4242, &api.OperationHealth{
		Busy:      true,
		Label:     "background embedding work",
		StartedAt: &startedAt,
	}, 31*time.Minute)

	assert.Contains(out, "pid 4242")
	assert.Contains(out, "background embedding work")
	assert.Contains(out, "running for 14m")
	assert.Contains(out, "31m0s")
	assert.Contains(out, "Ctrl+C")
}

func TestDescribeDaemonStopWaitWithoutOperation(t *testing.T) {
	assert := assert.New(t)

	out := describeDaemonStopWait(4242, nil, 31*time.Minute)

	assert.NotContains(out, "finishing")
	assert.Contains(out, "Waiting up to 31m0s")
	assert.Contains(out, "pid 4242")
}

func TestDescribeDaemonStopWaitWithGenericBusyOperation(t *testing.T) {
	assert := assert.New(t)

	out := describeDaemonStopWait(4242, &api.OperationHealth{Busy: true}, 31*time.Minute)

	assert.Contains(out, "pid 4242")
	assert.Contains(out, "finishing an archive operation")
	assert.Contains(out, "Waiting up to 31m0s")
}

func TestWaitForDaemonExitWithProgressExplainsLongStops(t *testing.T) {
	restoreStopWaitPacing(t, 10*time.Millisecond, 20*time.Millisecond)
	out := &bytes.Buffer{}
	exitAt := time.Now().Add(75 * time.Millisecond)
	startedAt := time.Now().Add(-time.Minute)
	op := &api.OperationHealth{
		Busy:      true,
		Label:     "background embedding work",
		StartedAt: &startedAt,
	}

	exited := waitForDaemonExitWithProgress(out, daemon.RuntimeRecord{PID: 4242}, op,
		time.Second, time.Millisecond,
		func(daemon.RuntimeRecord) bool { return time.Now().Before(exitAt) })

	require.True(t, exited, "wait must observe daemon exit")
	assert.Contains(t, out.String(), "background embedding work")
	assert.Contains(t, out.String(), "Still waiting")
}

func TestWaitForDaemonExitWithProgressGivesUpAtGrace(t *testing.T) {
	restoreStopWaitPacing(t, 5*time.Millisecond, 10*time.Millisecond)
	out := &bytes.Buffer{}

	exited := waitForDaemonExitWithProgress(out, daemon.RuntimeRecord{PID: 4242}, nil,
		50*time.Millisecond, time.Millisecond,
		func(daemon.RuntimeRecord) bool { return true })

	assert.False(t, exited, "wait must give up at the grace deadline")
	assert.Contains(t, out.String(), "Waiting up to")
}

func TestWaitForDaemonExitWithProgressQuietOnFastExit(t *testing.T) {
	restoreStopWaitPacing(t, 50*time.Millisecond, 100*time.Millisecond)
	out := &bytes.Buffer{}

	exited := waitForDaemonExitWithProgress(out, daemon.RuntimeRecord{PID: 4242}, nil,
		time.Second, time.Millisecond,
		func(daemon.RuntimeRecord) bool { return false })

	require.True(t, exited, "wait must observe daemon exit")
	assert.Empty(t, out.String(), "fast exits must stay quiet")
}
