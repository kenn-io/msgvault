package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/daemon"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/fileutil"
	"go.kenn.io/msgvault/internal/mcpdiscovery"
	"go.kenn.io/msgvault/internal/providercredentials"
)

func setMCPTokenTestFlags(t *testing.T, values map[string]string) {
	t.Helper()
	oldAddr, oldInsecure := mcpHTTPAddr, mcpHTTPAllowInsecure
	mcpHTTPAllowInsecure = false
	oldContext := mcpCmd.Context()
	t.Cleanup(func() { mcpHTTPAddr, mcpHTTPAllowInsecure = oldAddr, oldInsecure; mcpCmd.SetContext(oldContext) })
	for _, name := range []string{"http-token-file", "http-token-env"} {
		flag := mcpCmd.Flags().Lookup(name)
		require.NotNil(t, flag, "MCP must expose independent inbound credential sources")
		oldValue, oldChanged := flag.Value.String(), flag.Changed
		t.Cleanup(func() { assert.NoError(t, flag.Value.Set(oldValue)); flag.Changed = oldChanged })
		require.NoError(t, flag.Value.Set(""))
		flag.Changed = false
		if value, ok := values[name]; ok {
			require.NoError(t, flag.Value.Set(value))
			flag.Changed = true
		}
	}
}

func TestMCPTokenFailuresBeforeBackendConnection(t *testing.T) {
	for _, tc := range []struct {
		name, address string
		flags         map[string]string
	}{
		{"missing file", "127.0.0.1:0", map[string]string{"http-token-file": "missing-secret", "http-token-env": "MSGVAULT_TEST_MCP_LOWER_KEY"}},
		{"empty file flag", "127.0.0.1:0", map[string]string{"http-token-file": ""}},
		{"empty env flag", "127.0.0.1:0", map[string]string{"http-token-env": ""}},
		{"missing named environment", "127.0.0.1:0", map[string]string{"http-token-env": "MSGVAULT_TEST_MCP_MISSING_KEY"}},
		{"token without HTTP", "", map[string]string{"http-token-env": "MSGVAULT_TEST_MCP_LOWER_KEY"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("MSGVAULT_TEST_MCP_LOWER_KEY", "lower-key")
			t.Setenv("MSGVAULT_TEST_MCP_MISSING_KEY", "")
			var calls atomic.Int32
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { calls.Add(1); w.WriteHeader(http.StatusOK) }))
			defer backend.Close()
			cfg := credentialsTestConfig(t)
			cfg.Remote = config.RemoteConfig{URL: backend.URL, AllowInsecure: true}
			setMCPTokenTestFlags(t, tc.flags)
			mcpHTTPAddr = tc.address
			mcpCmd.SetContext(withStoreResolverConfig(t, cfg))
			require.Error(t, mcpCmd.RunE(mcpCmd, nil))
			assert.Zero(t, calls.Load(), "invalid inbound credentials must fail before opening the backend")
		})
	}
}

func TestMCPLoopbackInterfaceReusesKeylessDaemon(t *testing.T) { //nolint:paralleltest // process environment and MCP flags
	assert := assert.New(t)
	require := require.New(t)
	clearServerKeyEnvironment(t)
	interfaces, err := net.Interfaces()
	require.NoError(err)
	var loopback string
	for _, iface := range interfaces {
		if iface.Flags&net.FlagLoopback != 0 && iface.Flags&net.FlagUp != 0 {
			loopback = iface.Name
			break
		}
	}
	if loopback == "" {
		t.Skip("no active loopback interface")
	}
	home := t.TempDir()
	path := filepath.Join(home, "config.toml")
	require.NoError(os.WriteFile(path, []byte("[server]\nbind_addr = 'iface:"+loopback+"'\ndaemon_auto_start = false\ndaemon_auto_restart = 'never'\n"), 0o600))
	cfg, err := config.Load(path, home)
	require.NoError(err)
	mux := http.NewServeMux()
	mux.Handle("/api/ping", daemon.NewPingHandler(daemon.PingHandlerOptions{Service: daemonService, Version: Version}))
	mux.HandleFunc("/api/v1/health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	backend := httptest.NewServer(mux)
	t.Cleanup(backend.Close)
	rt := daemonRuntimeForHTTPServer(t, backend, daemonAPIKeyFingerprint(""))
	_, err = daemonRuntimeStore(home).Write(rt.Record)
	require.NoError(err)

	setMCPTokenTestFlags(t, nil)
	mcpHTTPAddr = "127.0.0.1:0"
	mcpCmd.SetContext(withStoreResolverConfig(t, cfg))
	_, key, err := prepareMCPHTTP(mcpCmd, cfg)
	require.NoError(err)
	assert.Empty(key, "a loopback interface must keep the existing keyless daemon usable")
	_, err = os.Stat(cfg.ServerKeyFilePath())
	require.ErrorIs(err, os.ErrNotExist)

	// A new client must still discover and authenticate the same daemon.
	fresh, err := config.Load(path, home)
	require.NoError(err)
	client, _, err := OpenHTTPStore(withStoreResolverConfig(t, fresh))
	require.NoError(err)
	require.NoError(client.Close())
}

func TestMCPLocalDaemonKeyLifecycle(t *testing.T) { //nolint:paralleltest // process environment and MCP flags
	for _, tc := range []struct {
		name, address string
		autostart     bool
		wantError     bool
	}{
		{"reuse loopback daemon with bind override", "127.0.0.1:0", false, false},
		{"reject public MCP with keyless daemon", "0.0.0.0:0", false, true},
		{"use key created during daemon startup", "0.0.0.0:0", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			clearServerKeyEnvironment(t)
			t.Setenv("MSGVAULT_REMOTE_URL", "")
			t.Setenv("MSGVAULT_ALLOW_INSECURE", "false")
			home := t.TempDir()
			port := freeTCPPort(t)
			path := filepath.Join(home, "config.toml")
			require.NoError(os.WriteFile(path, []byte(fmt.Sprintf("[server]\nbind_addr = '0.0.0.0'\napi_port = %d\ndaemon_auto_start = %t\ndaemon_auto_restart = 'never'\n[analytics]\nengine = 'sql'\n", port, tc.autostart)), 0o600))
			cfg, err := config.Load(path, home)
			require.NoError(err)
			daemonCtx, stopDaemon := context.WithCancel(t.Context())
			daemonDone := make(chan error, 1)
			started := false
			startDaemon := func() (*backgroundServeProcess, error) {
				overrides := config.RuntimeOverrides{}
				if !tc.autostart {
					overrides.BindAddr = new("127.0.0.1")
				}
				owner, err := config.LoadWithOverrides(path, home, overrides)
				if err != nil {
					return nil, err
				}
				command := &cobra.Command{Use: "serve"}
				command.SetContext(testInvocationContext(daemonCtx, owner, invocationOptions{}))
				started = true
				go func() { daemonDone <- runServe(command, nil) }()
				return &backgroundServeProcess{PID: os.Getpid(), Wait: daemonDone}, nil
			}
			t.Cleanup(func() {
				stopDaemon()
				if started {
					select {
					case err := <-daemonDone:
						assert.NoError(err)
					case <-time.After(serveLifecycleTestTimeout):
						assert.Fail("daemon did not stop")
					}
				}
			})
			if tc.autostart {
				// Replace only process spawning; run the real daemon, including
				// ownership, key creation, runtime publication, and HTTP handlers.
				stubStartServeBackgroundProcess(t, func(*config.Config, backgroundServeStartOptions) (*backgroundServeProcess, error) {
					return startDaemon()
				})
			} else {
				_, err = startDaemon()
				require.NoError(err)
				waitForServeHealthBounded(t, port, daemonDone)
			}
			setMCPTokenTestFlags(t, nil)
			mcpHTTPAddr = tc.address
			ctx, cancel := context.WithCancel(withStoreResolverConfig(t, cfg))
			defer cancel()
			mcpCmd.SetContext(ctx)
			if tc.wantError {
				require.ErrorContains(mcpCmd.RunE(mcpCmd, nil), "refusing to bind a non-loopback address")
			} else {
				done := make(chan error, 1)
				exited := false
				go func() { done <- mcpCmd.RunE(mcpCmd, nil) }()
				t.Cleanup(func() {
					cancel()
					if exited {
						return
					}
					select {
					case err := <-done:
						assert.ErrorIs(err, context.Canceled)
					case <-time.After(serveLifecycleTestTimeout):
						assert.Fail("MCP did not stop")
					}
				})
				var endpoint string
				var serveErr error
				require.Eventually(func() bool {
					select {
					case serveErr = <-done:
						exited = true
						return true
					default:
					}
					entries, err := mcpdiscovery.List(filepath.Join(home, "mcp"))
					if err != nil || len(entries) != 1 {
						return false
					}
					endpoint = entries[0].URL
					return true
				}, serveLifecycleTestTimeout, 20*time.Millisecond)
				require.False(exited, "MCP stopped before becoming ready: %v", serveErr)
				parsed, err := url.Parse(endpoint)
				require.NoError(err)
				_, listenPort, err := net.SplitHostPort(parsed.Host)
				require.NoError(err)
				parsed.Host = net.JoinHostPort("127.0.0.1", listenPort)
				var key string
				if tc.autostart {
					key, err = providercredentials.ReadSecretFile(cfg.ServerKeyFilePath())
					require.NoError(err)
				}
				client := &http.Client{Timeout: serveLifecycleTestTimeout}
				for _, token := range []string{"", key} {
					request, err := http.NewRequestWithContext(ctx, http.MethodPost, parsed.String(), strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"example-client","version":"test"}}}`))
					require.NoError(err)
					request.Header.Set("Content-Type", "application/json")
					request.Header.Set("Accept", "application/json, text/event-stream")
					if token != "" {
						request.Header.Set("Authorization", "Bearer "+token)
					}
					response, err := client.Do(request)
					require.NoError(err)
					body, err := io.ReadAll(response.Body)
					require.NoError(err)
					require.NoError(response.Body.Close())
					if tc.autostart && token == "" {
						assert.Equal(http.StatusUnauthorized, response.StatusCode)
					} else {
						assert.Equal(http.StatusOK, response.StatusCode, string(body))
						assert.Contains(string(body), `"protocolVersion"`)
					}
				}
			}
			if !tc.autostart {
				_, err = os.Stat(cfg.ServerKeyFilePath())
				require.ErrorIs(err, os.ErrNotExist)
			}
			fresh, err := config.Load(path, home)
			require.NoError(err)
			client, _, err := OpenHTTPStore(withStoreResolverConfig(t, fresh))
			require.NoError(err, "fresh clients must still connect to the same daemon")
			require.NoError(client.Close())
		})
	}
}

func TestMCPIndependentInboundTokenWithEnvironmentOnlyBackend(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	var wrongBackendKey atomic.Bool
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Api-Key") != "backend-key" {
			wrongBackendKey.Store(true)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/v1/health" {
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok", "api_schema_version": api.APISchemaVersion})
			return
		}
		http.NotFound(w, r)
	}))
	defer backend.Close()
	home := t.TempDir()
	file := filepath.Join(t.TempDir(), "inbound-key")
	require.NoError(fileutil.SecureWriteFile(file, []byte("inbound-key\n"), 0o400))
	t.Setenv("MSGVAULT_REMOTE_URL", backend.URL)
	t.Setenv("MSGVAULT_REMOTE_API_KEY", "backend-key")
	t.Setenv("MSGVAULT_REMOTE_ALLOW_INSECURE", "true")
	t.Setenv("MSGVAULT_API_KEY_FILE", filepath.Join(home, "unused-missing-server-key"))
	t.Setenv("MSGVAULT_TEST_MCP_LOWER_KEY", "lower-priority-key")
	cfg, err := config.Load("", home)
	require.NoError(err)
	setMCPTokenTestFlags(t, map[string]string{"http-token-file": file, "http-token-env": "MSGVAULT_TEST_MCP_LOWER_KEY"})
	mcpHTTPAddr = "0.0.0.0:0"
	ctx, cancel := context.WithCancel(withStoreResolverConfig(t, cfg))
	mcpCmd.SetContext(ctx)
	done := make(chan error, 1)
	go func() { done <- mcpCmd.RunE(mcpCmd, nil) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			assert.ErrorIs(err, context.Canceled)
		case <-time.After(serveLifecycleTestTimeout):
			assert.Fail("MCP did not stop")
		}
	})
	var endpoint string
	require.Eventually(func() bool {
		entries, err := mcpdiscovery.List(filepath.Join(home, "mcp"))
		if err != nil || len(entries) != 1 {
			return false
		}
		endpoint = entries[0].URL
		return true
	}, serveLifecycleTestTimeout, 20*time.Millisecond)
	parsed, err := url.Parse(endpoint)
	require.NoError(err)
	_, port, err := net.SplitHostPort(parsed.Host)
	require.NoError(err)
	parsed.Host = net.JoinHostPort("127.0.0.1", port)
	endpoint = parsed.String()
	client := &http.Client{Timeout: serveLifecycleTestTimeout}
	for _, token := range []string{"", "backend-key", "lower-priority-key", "inbound-key"} {
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"example-client","version":"test"}}}`))
		require.NoError(err)
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Accept", "application/json, text/event-stream")
		if token != "" {
			request.Header.Set("Authorization", "Bearer "+token)
		}
		response, err := client.Do(request)
		require.NoError(err)
		body, err := io.ReadAll(response.Body)
		require.NoError(err)
		require.NoError(response.Body.Close())
		if token == "inbound-key" {
			assert.Equal(http.StatusOK, response.StatusCode, string(body))
			assert.Contains(string(body), `"protocolVersion"`)
		} else {
			assert.Equal(http.StatusUnauthorized, response.StatusCode)
		}
	}
	assert.False(wrongBackendKey.Load())
}
