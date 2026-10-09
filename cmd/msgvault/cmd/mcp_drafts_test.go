package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/apiprotocol"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
	imaplib "go.kenn.io/msgvault/internal/imap"
	mcpserver "go.kenn.io/msgvault/internal/mcp"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func mcpDraftTestDaemon(t *testing.T, adapter *storeAPIAdapter, wrap func(http.Handler) http.Handler) *httptest.Server {
	t.Helper()
	handler := api.NewServerWithOptions(api.ServerOptions{
		Config: &config.Config{HomeDir: t.TempDir(), Server: config.ServerConfig{APIKey: "owner-test-key", AgentAccess: true}},
		Store:  adapter, Logger: slog.New(slog.DiscardHandler),
	}).Router()
	if wrap != nil {
		handler = wrap(handler)
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return server
}

func mcpDraftAgentContext(t *testing.T, server *httptest.Server, sourceID int64, permissions []string) context.Context {
	t.Helper()
	require := require.New(t)
	owner, err := daemonclient.New(daemonclient.Config{URL: server.URL, APIKey: "owner-test-key", AllowInsecure: true})
	require.NoError(err)
	t.Cleanup(func() { _ = owner.Close() })
	grant, err := owner.IssueAgentToken(t.Context(), "MCP test agent", permissions, []int64{sourceID}, nil, time.Time{})
	require.NoError(err)
	tokenFile := filepath.Join(t.TempDir(), "agent.token")
	require.NoError(os.WriteFile(tokenFile, []byte(grant.Secret+"\n"), 0o600))
	cfg := config.NewDefaultConfig()
	cfg.HomeDir = t.TempDir()
	cfg.Data.DataDir = t.TempDir()
	cfg.Server.APIKey = "owner-key-in-config"
	cfg.Remote.APIKey = "remote-key-in-config"
	return testInvocationContext(t.Context(), cfg, invocationOptions{
		agentURL: server.URL, agentTokenFile: tokenFile, agentAllowInsecure: true,
		agentURLChanged: true, agentTokenChanged: true,
	})
}

func mcpDraftTestSession(ctx context.Context, t *testing.T) *sdkmcp.ClientSession {
	t.Helper()
	require := require.New(t)
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	clientTransport, serverTransport := sdkmcp.NewInMemoryTransports()
	previousServe, previousHTTP := serveMCPStdioWithOptions, mcpHTTPAddr
	serveMCPStdioWithOptions = func(ctx context.Context, opts mcpserver.ServeOptions) error {
		return mcpserver.ServeTransport(ctx, opts, serverTransport)
	}
	mcpHTTPAddr = ""
	cmd := &cobra.Command{Use: "mcp"}
	cmd.SetContext(ctx)
	done := make(chan error, 1)
	go func() { done <- mcpCmd.RunE(cmd, nil) }()
	client := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "draft-routing-test", Version: "1"}, &sdkmcp.ClientOptions{MultiRoundTrip: &sdkmcp.MultiRoundTripOptions{Disabled: true}})
	session, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		cancel()
		select {
		case runErr := <-done:
			t.Logf("MCP startup: %v", runErr)
		case <-time.After(time.Second):
		}
	}
	t.Cleanup(func() {
		if session != nil {
			_ = session.Close()
		}
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			require.Fail("MCP server did not stop after cancellation")
		}
		serveMCPStdioWithOptions, mcpHTTPAddr = previousServe, previousHTTP
	})
	require.NoError(err)
	return session
}

func mcpDraftToolNames(t *testing.T, session *sdkmcp.ClientSession) []string {
	t.Helper()
	listed, err := session.ListTools(t.Context(), nil)
	require.NoError(t, err)
	var names []string
	for _, tool := range listed.Tools {
		names = append(names, tool.Name)
	}
	return names
}

func mcpDraftResultText(t *testing.T, result *sdkmcp.CallToolResult) string {
	t.Helper()
	require := require.New(t)
	require.NotEmpty(result.Content)
	text, ok := result.Content[0].(*sdkmcp.TextContent)
	require.True(ok)
	return text.Text
}

func TestMCPDelegatedDraftToolsUseAgentGrant(t *testing.T) {
	fixture := newDraftReplyFixture(t)
	adapter := fixture.grantedAdapter()
	var providerCalls atomic.Int64
	factory := adapter.draftClientFactory
	adapter.draftClientFactory = func(ctx context.Context, source *store.Source) (*imaplib.Client, error) {
		providerCalls.Add(1)
		return factory(ctx, source)
	}
	server := mcpDraftTestDaemon(t, adapter, nil)
	t.Run("granted source", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)

		session := mcpDraftTestSession(mcpDraftAgentContext(t, server, fixture.source.ID, []string{"draft.create"}), t)
		names := mcpDraftToolNames(t, session)
		assert.Subset(names, []string{"calendar_conflicts", "calendar_freebusy", "draft_compose", "draft_delete", "draft_edit", "draft_get", "draft_recover", "draft_reply"})
		// A draft-only grant cannot read the archive, so its read tools are not offered.
		for _, readTool := range []string{"search_metadata", "search_message_bodies", "get_message"} {
			assert.NotContains(names, readTool)
		}
		assert.NotContains(names, "draft_send_as")
		assert.NotContains(names, "calendar_create")
		result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: mcpserver.ToolDraftReply, Arguments: map[string]any{"message_id": fixture.parentID, "from": testutil.IMAPTestUsername, "body": "reply body"}})
		require.NoError(err)
		assert.False(result.IsError)
		structured, ok := result.StructuredContent.(map[string]any)
		require.True(ok)
		assert.Equal("created", structured["status"])
		assert.InDelta(fixture.source.ID, structured["source_id"], 0)
		assert.Equal("Drafts", structured["mailbox"])
	})
	t.Run("not_permitted", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)

		other, err := fixture.store.GetOrCreateSource("imap", "other@example.com")
		require.NoError(err)
		session := mcpDraftTestSession(mcpDraftAgentContext(t, server, other.ID, []string{"draft.create"}), t)
		before := providerCalls.Load()
		result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: mcpserver.ToolDraftReply, Arguments: map[string]any{"message_id": fixture.parentID, "from": testutil.IMAPTestUsername, "body": "reply body"}})
		require.NoError(err)
		assert.True(result.IsError)
		require.NotEmpty(result.Content)
		text, ok := result.Content[0].(*sdkmcp.TextContent)
		require.True(ok)
		assert.Equal("not_permitted", text.Text)
		assert.Equal(before, providerCalls.Load())
	})
	t.Run("command is not allowed through the daemon CLI runner", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)

		session := mcpDraftTestSession(mcpDraftAgentContext(t, server, fixture.source.ID, []string{"draft.delete"}), t)
		before := providerCalls.Load()
		result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: mcpserver.ToolDraftEdit, Arguments: map[string]any{"draft_id": "test-draft", "revision": 1, "body": "replacement"}})
		require.NoError(err)
		assert.True(result.IsError)
		text := mcpDraftResultText(t, result)
		assert.Equal("command is not allowed through the daemon CLI runner", text)
		assert.Equal(before, providerCalls.Load())
	})
}

func TestMCPDelegatedNeverSendsOwnerCredential(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	fixture := newDraftReplyFixture(t)
	var mu sync.Mutex
	var headers []http.Header
	var record atomic.Bool
	server := mcpDraftTestDaemon(t, fixture.grantedAdapter(), func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if record.Load() {
				mu.Lock()
				headers = append(headers, r.Header.Clone())
				mu.Unlock()
			}
			next.ServeHTTP(w, r)
		})
	})
	ctx := mcpDraftAgentContext(t, server, fixture.source.ID, []string{"draft.create"})
	record.Store(true)
	session := mcpDraftTestSession(ctx, t)
	_, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: mcpserver.ToolDraftReply, Arguments: map[string]any{"message_id": fixture.parentID, "from": testutil.IMAPTestUsername, "body": "reply body"}})
	require.NoError(err)
	mu.Lock()
	defer mu.Unlock()
	require.GreaterOrEqual(len(headers), 3)
	for _, header := range headers {
		assert.NotEmpty(header.Get(apiprotocol.AgentTokenHeader))
		for _, key := range []string{"X-Api-Key", "Authorization", "Cookie", apiprotocol.DaemonRuntimeTokenHeader} {
			assert.Empty(header.Get(key), key)
		}
	}
}

func TestMCPDelegatedRejectsHTTPTransport(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	fixture := newDraftReplyFixture(t)
	var requests atomic.Int64
	server := mcpDraftTestDaemon(t, fixture.grantedAdapter(), func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1); next.ServeHTTP(w, r) })
	})
	ctx := mcpDraftAgentContext(t, server, fixture.source.ID, []string{"draft.create"})
	before := requests.Load()
	previousHTTP, previousServe := mcpHTTPAddr, serveMCPHTTPWithOptions
	mcpHTTPAddr = "127.0.0.1:0"
	called := false
	serveMCPHTTPWithOptions = func(context.Context, mcpserver.ServeOptions, mcpserver.HTTPOptions) error { called = true; return nil }
	t.Cleanup(func() { mcpHTTPAddr, serveMCPHTTPWithOptions = previousHTTP, previousServe })
	cmd := &cobra.Command{Use: "mcp"}
	cmd.SetContext(ctx)
	err := mcpCmd.RunE(cmd, nil)
	require.ErrorContains(err, "delegated MCP supports stdio only")
	assert.False(called)
	assert.Equal(before, requests.Load())
}

func TestMCPOwnerDraftGetMatchesCLI(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	fixture := newDraftReplyFixture(t)
	server := mcpDraftTestDaemon(t, fixture.grantedAdapter(), nil)
	ctx := withStoreResolverConfig(t, &config.Config{HomeDir: t.TempDir(), Data: config.DataConfig{DataDir: t.TempDir()}, Remote: config.RemoteConfig{URL: server.URL, APIKey: "owner-test-key", AllowInsecure: true}})
	session := mcpDraftTestSession(ctx, t)
	result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: mcpserver.ToolDraftReply, Arguments: map[string]any{"message_id": fixture.parentID, "from": testutil.IMAPTestUsername, "body": "reply body"}})
	require.NoError(err)
	require.False(result.IsError)
	structured, ok := result.StructuredContent.(map[string]any)
	require.True(ok)
	id, ok := structured["draft_id"].(string)
	require.True(ok)
	result, err = session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: mcpserver.ToolDraftGet, Arguments: map[string]any{"draft_id": id}})
	require.NoError(err)
	require.False(result.IsError)
	root := newTestRootCmd()
	root.AddCommand(newDraftGetCommand())
	root.SetArgs([]string{"draft-get", id, "--json"})
	var stdout bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&bytes.Buffer{})
	require.NoError(root.ExecuteContext(ctx))
	assert.JSONEq(stdout.String(), mcpDraftResultText(t, result))
}

func TestMCPDraftToolPropertiesMatchCommandFlags(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	fixture := newDraftReplyFixture(t)
	server := mcpDraftTestDaemon(t, fixture.grantedAdapter(), nil)
	ctx := withStoreResolverConfig(t, &config.Config{HomeDir: t.TempDir(), Remote: config.RemoteConfig{URL: server.URL, APIKey: "owner-test-key", AllowInsecure: true}})
	session := mcpDraftTestSession(ctx, t)
	listed, err := session.ListTools(t.Context(), nil)
	require.NoError(err)
	positionals := map[string]string{"draft_reply": "message_id", "draft_forward": "message_id", "draft_get": "draft_id", "draft_edit": "draft_id", "draft_delete": "draft_id", "draft_recover": "draft_id", "draft_send_as": "account"}
	var names []string
	for _, tool := range listed.Tools {
		if !strings.HasPrefix(tool.Name, "draft_") {
			continue
		}
		names = append(names, tool.Name)
		constructor := mcpDraftCommandConstructors[strings.ReplaceAll(tool.Name, "_", "-")]
		require.NotNil(constructor)
		var flags, properties []string
		constructor().Flags().VisitAll(func(flag *pflag.Flag) {
			if flag.Name != "json" {
				flags = append(flags, flag.Name)
			}
		})
		schema, ok := tool.InputSchema.(map[string]any)
		require.True(ok)
		inputProperties, ok := schema["properties"].(map[string]any)
		require.True(ok)
		for key := range inputProperties {
			if key != positionals[tool.Name] {
				properties = append(properties, strings.ReplaceAll(key, "_", "-"))
			}
		}
		sort.Strings(flags)
		sort.Strings(properties)
		assert.Equal(flags, properties, tool.Name)
	}
	assert.Equal([]string{"draft_compose", "draft_delete", "draft_edit", "draft_forward", "draft_get", "draft_recover", "draft_reply", "draft_send_as"}, names)
}

func TestMCPDraftCapabilityGate(t *testing.T) {
	previousCheck := remoteAPISchemaCheckEnabled
	remoteAPISchemaCheckEnabled = true
	t.Cleanup(func() { remoteAPISchemaCheckEnabled = previousCheck })
	for _, delegated := range []bool{false, true} {
		for _, version := range []string{"failed", "missing", "2.35.0", "3.0.0"} {
			name := "owner/" + version
			if delegated {
				name = "delegated/" + version
			}
			t.Run(name, func(t *testing.T) {
				assert := assert.New(t)
				require := require.New(t)
				fixture := newDraftReplyFixture(t)
				var probing atomic.Bool
				var healthCalls atomic.Int64
				server := mcpDraftTestDaemon(t, fixture.grantedAdapter(), func(next http.Handler) http.Handler {
					return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if probing.Load() && r.URL.Path == "/api/v1/health" && healthCalls.Add(1) > 1 {
							w.Header().Set("Content-Type", "application/json")
							if version == "failed" {
								w.WriteHeader(http.StatusServiceUnavailable)
								_, _ = w.Write([]byte(`{"error":"unavailable","message":"probe unavailable"}`))
								return
							}
							body := map[string]any{"status": "ok"}
							if version != "missing" {
								body["api_schema_version"] = version
							}
							_ = json.NewEncoder(w).Encode(body)
							return
						}
						next.ServeHTTP(w, r)
					})
				})
				var ctx context.Context
				if delegated {
					ctx = mcpDraftAgentContext(t, server, fixture.source.ID, []string{"draft.create"})
				} else {
					ctx = withStoreResolverConfig(t, &config.Config{HomeDir: t.TempDir(), Remote: config.RemoteConfig{URL: server.URL, APIKey: "owner-test-key", AllowInsecure: true}})
				}
				probing.Store(true)
				if delegated && version != "3.0.0" {
					// Delegated sessions offer nothing but draft tools, so they fail instead of serving an empty list.
					served := false
					previousServe := serveMCPStdioWithOptions
					serveMCPStdioWithOptions = func(context.Context, mcpserver.ServeOptions) error { served = true; return nil }
					t.Cleanup(func() { serveMCPStdioWithOptions = previousServe })
					cmd := &cobra.Command{Use: "mcp"}
					cmd.SetContext(ctx)
					err := mcpCmd.RunE(cmd, nil)
					if version == "failed" {
						require.ErrorContains(err, "check daemon compatibility")
						require.ErrorContains(err, "probe unavailable")
					} else {
						require.ErrorContains(err, "MCP draft tools require daemon API schema 3.0.0 or newer")
						require.ErrorContains(err, "upgrade the daemon")
					}
					assert.False(served)
					assert.Equal(int64(2), healthCalls.Load(), "startup compatibility and draft schema probes")
					return
				}
				session := mcpDraftTestSession(ctx, t)
				names := mcpDraftToolNames(t, session)
				if delegated && version == "3.0.0" {
					assert.NotContains(names, "search_metadata")
					assert.NotContains(names, "get_message")
				}
				var drafts []string
				for _, name := range names {
					if strings.HasPrefix(name, "draft_") {
						drafts = append(drafts, name)
					}
				}
				want := 0
				if version == "3.0.0" {
					want = 8
					if delegated {
						want = 6
					}
				}
				assert.Len(drafts, want)
				require.Equal(int64(2), healthCalls.Load(), "startup compatibility and capability probes")
			})
		}
	}
}
