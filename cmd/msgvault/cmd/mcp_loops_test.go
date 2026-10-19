package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/apiprotocol"
	"go.kenn.io/msgvault/internal/config"
	mcpserver "go.kenn.io/msgvault/internal/mcp"
)

func loopMCPRouteDescriptors() []apiprotocol.MCPRouteDescriptor {
	return []apiprotocol.MCPRouteDescriptor{
		{OperationID: "listOpenLoops", Method: http.MethodGet, Path: "/api/v1/loops"},
		{OperationID: "getWaitingLoops", Method: http.MethodGet, Path: "/api/v1/loops/waiting"},
		{OperationID: "getOpenLoop", Method: http.MethodGet, Path: "/api/v1/loops/{id}"},
		{OperationID: "closeOpenLoop", Method: http.MethodPost, Path: "/api/v1/loops/{id}/done"},
		{OperationID: "snoozeOpenLoop", Method: http.MethodPost, Path: "/api/v1/loops/{id}/snooze"},
		{OperationID: "listLoopSuppressions", Method: http.MethodGet, Path: "/api/v1/loops/suppressions"},
		{OperationID: "createLoopSuppression", Method: http.MethodPost, Path: "/api/v1/loops/suppressions"},
		{OperationID: "deleteLoopSuppression", Method: http.MethodDelete, Path: "/api/v1/loops/suppressions/{id}"},
	}
}
func TestMCPLoopsDiscoveryUsesActualDaemonRoutes(t *testing.T) {
	wrongMethod := loopMCPRouteDescriptors()
	wrongMethod[0].Method = http.MethodPost
	wrongPath := loopMCPRouteDescriptors()
	wrongPath[0].Path = "/api/v1/other"
	for _, tc := range []struct {
		name         string
		localEnabled bool
		routes       []apiprotocol.MCPRouteDescriptor
		want         bool
	}{
		{"remote enabled despite local disabled", false, loopMCPRouteDescriptors(), true},
		{"remote disabled despite local enabled", true, nil, false},
		{"partial capability is unavailable", true, loopMCPRouteDescriptors()[:7], false},
		{"wrong method is unavailable", true, wrongMethod, false},
		{"wrong path is unavailable", true, wrongPath, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.NewDefaultConfig()
			cfg.Loops.Enabled = tc.localEnabled
			state := testInvocationWithConfig(cfg)
			client := newMCPDaemonClient(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/api/v1/health":
					_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok", "api_schema_version": "3.0.0"})
				case "/api/v1/mcp/capabilities":
					_ = json.NewEncoder(w).Encode(apiprotocol.MCPCapabilities{Version: 1, Routes: tc.routes})
				default:
					http.NotFound(w, r)
				}
			})
			opts := daemonMCPServeOptions(t.Context(), client, state)
			assert.Equal(t, tc.want, opts.Loops != nil)
			delegated, err := delegatedMCPServeOptions(t.Context(), client)
			require.NoError(t, err)
			assert.Nil(t, delegated.Loops)
		})
	}
}
func TestMCPLoopWriteFlagIsScopedToRootInvocation(t *testing.T) {
	// Exercise the registered MCP command and the root's cleanup on success,
	// argument parsing failure and runtime failure, using a synthetic daemon.
	assertions := assert.New(t)
	requirements := require.New(t)

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/health":
			requirements.NoError(json.NewEncoder(w).Encode(map[string]any{"status": "ok", "api_schema_version": api.APISchemaVersion}))
		case "/api/v1/mcp/capabilities":
			requirements.NoError(json.NewEncoder(w).Encode(apiprotocol.MCPCapabilities{Version: 1, Routes: loopMCPRouteDescriptors()}))
		case "/api/session":
			requirements.NoError(json.NewEncoder(w).Encode(map[string]any{"auth_mode": "owner"}))
		default:
			http.NotFound(w, r)
		}
	})
	daemon := httptest.NewServer(handler)
	t.Cleanup(daemon.Close)
	home := t.TempDir()
	configPath := filepath.Join(home, "config.toml")
	requirements.NoError(os.WriteFile(configPath, []byte(fmt.Sprintf("[remote]\nurl = %q\napi_key = \"key\"\nallow_insecure = true\n", daemon.URL)), 0o600))
	// Reuse the production root. Reparenting this package-global Cobra command
	// would retain inherited flags from its original parent.
	root := rootCmd
	oldOut, oldErr := root.OutOrStdout(), root.ErrOrStderr()
	clearInvocationFlags(root)
	oldServe, oldHTTP := serveMCPStdioWithOptions, mcpHTTPAddr
	mcpHTTPAddr = ""
	t.Cleanup(func() {
		serveMCPStdioWithOptions = oldServe
		mcpHTTPAddr = oldHTTP
		root.SetArgs(nil)
		root.SetOut(oldOut)
		root.SetErr(oldErr)
		clearInvocationFlags(root)
	})
	var choices []bool
	var serveErr error
	serveMCPStdioWithOptions = func(_ context.Context, opts mcpserver.ServeOptions) error {
		choices = append(choices, opts.AllowLoopWrites)
		return serveErr
	}
	var out, stderr bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&stderr)
	execute := func(args ...string) error {
		root.SetArgs(append([]string{"--home", home, "--config", configPath, "--no-log-file", "--log-level=warn", "mcp"}, args...))
		return executeRootContext(t.Context(), root)
	}
	requirements.NoError(execute("--allow-loop-writes"))
	requirements.NoError(execute())
	assertions.Equal([]bool{true, false}, choices)
	requirements.Error(execute("--allow-loop-writes", "--unknown-loop-flag"))
	requirements.NoError(execute())
	assertions.False(choices[len(choices)-1])
	serveErr = errors.New("synthetic serve failure")
	requirements.ErrorIs(execute("--allow-loop-writes"), serveErr)
	serveErr = nil
	requirements.NoError(execute())
	assertions.False(choices[len(choices)-1])
}
