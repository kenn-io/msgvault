package cmd

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/fileutil"
	"go.kenn.io/msgvault/internal/mcpdiscovery"
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
