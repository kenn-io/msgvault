package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep constructors available in nested scopes.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep constructors available in nested scopes.
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
	mcpserver "go.kenn.io/msgvault/internal/mcp"
	"go.kenn.io/msgvault/internal/mcpdiscovery"
	"go.kenn.io/msgvault/internal/mcpevents"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

// A disabled daemon must still let its owner find native resource lifetimes.
// The real daemon, owner authentication, CLI selection and SDK HTTP handler run
// here; only the native inventory observation is a synthetic provider boundary.
func TestMCPNativeResourceDiscoveryDefaultOffCLI(t *testing.T) {
	assert, require := Assert.New(t), Require.New(t)
	const owner = "synthetic-native-resource-owner"
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("gmail", "owner@example.net")
	require.NoError(err)
	resource, err := st.GetMCPResource(t.Context(), "source", source.ID)
	require.NoError(err)
	require.NoError(st.ObserveNativeSourcePermission(t.Context(), store.NativeSourcePermission{SourceID: source.ID, SourceReference: resource.Reference, SourceType: "gmail", NativeIdentity: source.Identifier, Known: true, MessageRead: true}))
	require.NoError(st.ObserveNativeMessageScopes(t.Context(), store.NativeMessageScopeInventory{SourceID: source.ID, SourceReference: resource.Reference, SourceType: "gmail", NativeIdentity: source.Identifier, Kind: "gmail_label", Complete: true, Scopes: []store.NativeMessageScope{{ProviderID: "INBOX", DisplayName: "Inbox"}}}))
	cfg := &config.Config{HomeDir: t.TempDir(), Data: config.DataConfig{DataDir: t.TempDir()}, Server: config.ServerConfig{APIKey: owner}}
	key := filepath.Join(t.TempDir(), "disabled-events.key")
	service, err := mcpevents.New(t.Context(), st, mcpevents.Options{Enabled: false, OwnerKey: owner, KeyPath: key})
	require.NoError(err)
	server := api.NewServerWithOptions(api.ServerOptions{Config: cfg, Store: &storeAPIAdapter{store: st, config: cfg}, Logger: slog.New(slog.DiscardHandler)})
	server.SetMCPEvents(service)
	t.Cleanup(func() { assert.NoError(server.Shutdown(context.Background())) })
	daemon := httptest.NewServer(server.Router())
	t.Cleanup(daemon.Close)
	client, err := daemonclient.New(daemonclient.Config{URL: daemon.URL, APIKey: owner, AllowInsecure: true})
	require.NoError(err)
	t.Cleanup(func() { assert.NoError(client.Close()) })
	ctx := withStoreResolverConfig(t, cfg)
	opts := daemonMCPServeOptions(ctx, client, invocationFromContext(ctx))
	require.Nil(opts.Events, "resource discovery must not advertise Events delivery")
	for _, tc := range []struct {
		name                                  string
		independent, delegated, keyless, want bool
	}{
		{name: "owner", want: true},
		{name: "independent credential", independent: true},
		{name: "delegated", delegated: true},
		{name: "keyless", keyless: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert, require := Assert.New(t), Require.New(t)
			selected := opts
			selected.DelegatedOnly = tc.delegated
			credential := owner
			if tc.keyless {
				credential = ""
			}
			directory := t.TempDir()
			runCtx, cancel := context.WithCancel(t.Context())
			done := make(chan error, 1)
			go func() {
				done <- mcpserver.ServeHTTPWithOptions(runCtx, selected, mcpserver.HTTPOptions{Addr: "127.0.0.1:0", APIKey: credential, IndependentCredential: tc.independent, DiscoveryDirectory: directory, BackendURL: daemon.URL})
			}()
			t.Cleanup(func() {
				cancel()
				select {
				case err := <-done:
					assert.True(err == nil || errors.Is(err, context.Canceled), "%v", err)
				case <-time.After(30 * time.Second):
					assert.Fail("native MCP listener did not stop")
				}
			})
			var endpoint string
			require.Eventually(func() bool {
				endpoints, err := mcpdiscovery.List(directory)
				if err != nil || len(endpoints) != 1 {
					return false
				}
				endpoint = endpoints[0].URL
				return endpoint != ""
			}, 30*time.Second, 20*time.Millisecond)
			call := func(method string, params map[string]any) (int, map[string]any) {
				params["_meta"] = map[string]any{"io.modelcontextprotocol/protocolVersion": "2026-07-28", "io.modelcontextprotocol/clientCapabilities": map[string]any{}}
				raw, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
				require.NoError(err)
				request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, endpoint, bytes.NewReader(raw))
				require.NoError(err)
				if credential != "" {
					request.Header.Set("Authorization", "Bearer "+credential)
				}
				request.Header.Set("Content-Type", "application/json")
				request.Header.Set("Accept", "application/json, text/event-stream")
				request.Header.Set("Mcp-Protocol-Version", "2026-07-28")
				request.Header.Set("Mcp-Method", method)
				if name, ok := params["name"].(string); ok {
					request.Header.Set("Mcp-Name", name)
				}
				response, err := http.DefaultClient.Do(request)
				require.NoError(err)
				defer func() { _ = response.Body.Close() }()
				if response.StatusCode == http.StatusNotFound {
					require.Equal("events/list", method)
					return response.StatusCode, nil
				}
				require.Contains([]int{http.StatusOK, http.StatusBadRequest}, response.StatusCode)
				var wire map[string]any
				require.NoError(json.NewDecoder(response.Body).Decode(&wire))
				return response.StatusCode, wire
			}
			status, wire := call("tools/call", map[string]any{"name": "list_mcp_event_resources", "arguments": map[string]any{"kind": "inbox"}})
			if !tc.want {
				require.NotNil(wire["error"], "non-owner transport exposed native resource discovery")
				return
			}
			require.Nil(wire["error"], "native resource discovery is missing: %v", wire["error"])
			require.Equal(http.StatusOK, status)
			result, ok := wire["result"].(map[string]any)
			require.True(ok)
			require.NotEqual(true, result["isError"], "%v", result["content"])
			raw, err := json.Marshal(result["structuredContent"])
			require.NoError(err)
			var page mcpevents.ResourcePage
			require.NoError(json.Unmarshal(raw, &page))
			require.Len(page.Resources, 1)
			inbox := page.Resources[0]
			assert.Equal("gmail_label", inbox.ScopeKind)
			assert.NotEmpty(inbox.ID)
			assert.NotEmpty(inbox.Reference)
			assert.Empty(inbox.Capabilities.Events)
			assert.Equal("events_disabled", inbox.Capabilities.UnavailableReason)
			status, events := call("events/list", map[string]any{})
			require.True(status == http.StatusNotFound || events["error"] != nil, "resource discovery unexpectedly advertised Events methods")
		})
	}
	assert.NoFileExists(key, "resource discovery must not create a signing key")
}
