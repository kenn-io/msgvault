package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
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
	requirements := require.New(t)
	owner, err := daemonclient.New(daemonclient.Config{URL: server.URL, APIKey: "owner-test-key", AllowInsecure: true})
	requirements.NoError(err)
	t.Cleanup(func() { _ = owner.Close() })
	grant, err := owner.IssueAgentToken(t.Context(), "MCP test agent", permissions, []int64{sourceID}, nil)
	requirements.NoError(err)
	tokenFile := filepath.Join(t.TempDir(), "agent.token")
	requirements.NoError(os.WriteFile(tokenFile, []byte(grant.Secret+"\n"), 0o600))
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
	requirements := require.New(t)
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	clientPipe, serverPipe := net.Pipe()
	clientTransport := &sdkmcp.IOTransport{Reader: clientPipe, Writer: clientPipe}
	serverTransport := &sdkmcp.IOTransport{Reader: serverPipe, Writer: serverPipe}
	// SDK writes to net.Pipe do not observe context cancellation. Closing both
	// endpoints also exposes a command that exits before connecting the server.
	stopClose := context.AfterFunc(ctx, func() {
		_ = clientPipe.Close()
		_ = serverPipe.Close()
	})
	t.Cleanup(func() {
		stopClose()
		_ = clientPipe.Close()
		_ = serverPipe.Close()
	})
	previousServe, previousHTTP := serveMCPStdioWithOptions, mcpHTTPAddr
	serveMCPStdioWithOptions = func(ctx context.Context, opts mcpserver.ServeOptions) error {
		return mcpserver.ServeTransport(ctx, opts, serverTransport)
	}
	mcpHTTPAddr = ""
	cmd := &cobra.Command{Use: "mcp"}
	cmd.SetContext(ctx)
	done := make(chan error, 1)
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		done <- mcpCmd.RunE(cmd, nil)
		cancel()
	}()
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
		case <-stopped:
		case <-time.After(5 * time.Second):
			requirements.Fail("MCP server did not stop after cancellation")
		}
		serveMCPStdioWithOptions, mcpHTTPAddr = previousServe, previousHTTP
	})
	requirements.NoError(err)
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
	requirements := require.New(t)
	requirements.NotEmpty(result.Content)
	text, ok := result.Content[0].(*sdkmcp.TextContent)
	requirements.True(ok)
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
		assertions := assert.New(t)
		requirements := require.New(t)
		session := mcpDraftTestSession(mcpDraftAgentContext(t, server, fixture.source.ID, []string{"draft.create"}), t)
		assertions.Equal([]string{"draft_compose", "draft_get", "draft_reply"}, mcpDraftToolNames(t, session))
		result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: mcpserver.ToolDraftReply, Arguments: map[string]any{"message_id": fixture.parentID, "from": testutil.IMAPTestUsername, "body": "reply body"}})
		requirements.NoError(err)
		assertions.False(result.IsError)
		structured, ok := result.StructuredContent.(map[string]any)
		requirements.True(ok)
		assertions.Equal("created", structured["status"])
		assertions.InDelta(fixture.source.ID, structured["source_id"], 0)
		assertions.Equal("Drafts", structured["mailbox"])
	})
	t.Run("not_permitted", func(t *testing.T) {
		assertions := assert.New(t)
		requirements := require.New(t)
		other, err := fixture.store.GetOrCreateSource("imap", "other@example.com")
		requirements.NoError(err)
		session := mcpDraftTestSession(mcpDraftAgentContext(t, server, other.ID, []string{"draft.create"}), t)
		before := providerCalls.Load()
		result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: mcpserver.ToolDraftReply, Arguments: map[string]any{"message_id": fixture.parentID, "from": testutil.IMAPTestUsername, "body": "reply body"}})
		requirements.NoError(err)
		assertions.True(result.IsError)
		requirements.NotEmpty(result.Content)
		text, ok := result.Content[0].(*sdkmcp.TextContent)
		requirements.True(ok)
		assertions.Equal("not_permitted", text.Text)
		t.Log(text.Text)
		assertions.Equal(before, providerCalls.Load())
	})
	t.Run("command is not allowed through the daemon CLI runner", func(t *testing.T) {
		assertions := assert.New(t)
		requirements := require.New(t)
		session := mcpDraftTestSession(mcpDraftAgentContext(t, server, fixture.source.ID, []string{"draft.delete"}), t)
		before := providerCalls.Load()
		result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: mcpserver.ToolDraftEdit, Arguments: map[string]any{"draft_id": "test-draft", "revision": 1, "body": "replacement"}})
		requirements.Error(err, "unadmitted tool must be absent from the MCP catalog")
		assertions.Nil(result)
		assertions.NotContains(mcpDraftToolNames(t, session), mcpserver.ToolDraftEdit)
		assertions.Equal(before, providerCalls.Load())
	})
}

func TestMCPDelegatedNeverSendsOwnerCredential(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
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
	requirements.NoError(err)
	mu.Lock()
	defer mu.Unlock()
	requirements.GreaterOrEqual(len(headers), 3)
	for _, header := range headers {
		assertions.NotEmpty(header.Get(apiprotocol.AgentTokenHeader))
		for _, key := range []string{"X-Api-Key", "Authorization", "Cookie", apiprotocol.DaemonRuntimeTokenHeader} {
			assertions.Empty(header.Get(key), key)
		}
	}
}

func TestMCPDelegatedRejectsHTTPTransport(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
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
	requirements.ErrorContains(err, "--http is not available in agent-delegated mode")
	assertions.False(called)
	assertions.Equal(before, requests.Load())
	t.Log("--http is not available in agent-delegated mode; zero daemon requests")
}

func TestMCPOwnerDraftGetMatchesCLI(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	fixture := newDraftReplyFixture(t)
	server := mcpDraftTestDaemon(t, fixture.grantedAdapter(), nil)
	ctx := withStoreResolverConfig(t, &config.Config{HomeDir: t.TempDir(), Data: config.DataConfig{DataDir: t.TempDir()}, Remote: config.RemoteConfig{URL: server.URL, APIKey: "owner-test-key", AllowInsecure: true}})
	session := mcpDraftTestSession(ctx, t)
	result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: mcpserver.ToolDraftReply, Arguments: map[string]any{"message_id": fixture.parentID, "from": testutil.IMAPTestUsername, "body": "reply body"}})
	requirements.NoError(err)
	requirements.False(result.IsError)
	structured, ok := result.StructuredContent.(map[string]any)
	requirements.True(ok)
	id, ok := structured["draft_id"].(string)
	requirements.True(ok)
	result, err = session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: mcpserver.ToolDraftGet, Arguments: map[string]any{"draft_id": id}})
	requirements.NoError(err)
	requirements.False(result.IsError)
	root := newTestRootCmd()
	root.AddCommand(newDraftGetCommand())
	root.SetArgs([]string{"draft-get", id, "--json"})
	var stdout bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&bytes.Buffer{})
	requirements.NoError(root.ExecuteContext(ctx))
	assertions.JSONEq(stdout.String(), mcpDraftResultText(t, result))
}

func TestMCPDraftToolPropertiesMatchCommandFlags(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	fixture := newDraftReplyFixture(t)
	server := mcpDraftTestDaemon(t, fixture.grantedAdapter(), nil)
	ctx := withStoreResolverConfig(t, &config.Config{HomeDir: t.TempDir(), Remote: config.RemoteConfig{URL: server.URL, APIKey: "owner-test-key", AllowInsecure: true}})
	session := mcpDraftTestSession(ctx, t)
	listed, err := session.ListTools(t.Context(), nil)
	requirements.NoError(err)
	positionals := map[string]string{"draft_reply": "message_id", "draft_forward": "message_id", "draft_get": "draft_id", "draft_edit": "draft_id", "draft_delete": "draft_id", "draft_recover": "draft_id", "draft_send_as": "account"}
	var names []string
	for _, tool := range listed.Tools {
		if !strings.HasPrefix(tool.Name, "draft_") {
			continue
		}
		names = append(names, tool.Name)
		constructor := mcpDraftCommandConstructors[strings.ReplaceAll(tool.Name, "_", "-")]
		requirements.NotNil(constructor)
		var flags, properties []string
		constructor().Flags().VisitAll(func(flag *pflag.Flag) {
			if flag.Name != "json" {
				flags = append(flags, flag.Name)
			}
		})
		schema, ok := tool.InputSchema.(map[string]any)
		requirements.True(ok)
		inputProperties, ok := schema["properties"].(map[string]any)
		requirements.True(ok)
		for key := range inputProperties {
			if key != positionals[tool.Name] {
				properties = append(properties, strings.ReplaceAll(key, "_", "-"))
			}
		}
		sort.Strings(flags)
		sort.Strings(properties)
		assertions.Equal(flags, properties, tool.Name)
	}
	assertions.Equal([]string{"draft_compose", "draft_delete", "draft_edit", "draft_forward", "draft_get", "draft_recover", "draft_reply", "draft_send_as"}, names)
}

func TestDaemonMCPDraftRunnerRejectsFlagShapedPositional(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests.Add(1) }))
	t.Cleanup(server.Close)
	client, err := daemonclient.New(daemonclient.Config{URL: server.URL, AllowInsecure: true})
	requirements.NoError(err)
	t.Cleanup(func() { _ = client.Close() })
	_, err = (daemonMCPDraftRunner{client: client}).RunDraftCommand(t.Context(), mcpserver.DraftCommandRequest{Command: "draft-get", Positional: "--conversation=7"})
	_, typed := errors.AsType[*mcpserver.DraftCommandError](err)
	assertions.True(typed)
	assertions.Equal(int64(0), requests.Load())
	t.Log("--conversation=7 rejected before any daemon request")
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
				assertions := assert.New(t)
				requirements := require.New(t)
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
						requirements.ErrorContains(err, "check daemon compatibility")
						requirements.ErrorContains(err, "probe unavailable")
					} else {
						requirements.ErrorContains(err, "MCP draft tools require daemon API schema 3.0.0 or newer")
						requirements.ErrorContains(err, "upgrade the daemon")
					}
					assertions.False(served)
					assertions.Equal(int64(2), healthCalls.Load(), "startup compatibility and draft schema probes")
					return
				}
				session := mcpDraftTestSession(ctx, t)
				var drafts []string
				for _, name := range mcpDraftToolNames(t, session) {
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
				assertions.Len(drafts, want)
				requirements.Equal(int64(2), healthCalls.Load(), "startup compatibility and capability probes")
			})
		}
	}
}

func TestMCPDelegatedCalendarToolsRequireCalendarSchema(t *testing.T) {
	fixture := newDraftReplyFixture(t)
	server := mcpDraftTestDaemon(t, fixture.grantedAdapter(), func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/v1/health" {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"status":"ok","api_schema_version":"3.0.0"}`))
				return
			}
			next.ServeHTTP(w, r)
		})
	})
	session := mcpDraftTestSession(mcpDraftAgentContext(t, server, fixture.source.ID, []string{"draft.create"}), t)
	assert.Equal(t, []string{"draft_compose", "draft_delete", "draft_edit", "draft_get", "draft_recover", "draft_reply"}, mcpDraftToolNames(t, session))
}

func TestMCPProductionDraftCommandDiscovery(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	fixture := newDraftReplyFixture(t)
	server := mcpDraftTestDaemon(t, fixture.grantedAdapter(), nil)
	owner, err := daemonclient.New(daemonclient.Config{URL: server.URL, APIKey: "owner-test-key", AllowInsecure: true})
	requirements.NoError(err)
	t.Cleanup(func() { _ = owner.Close() })
	discovery, err := owner.MCPCapabilities(t.Context())
	requirements.NoError(err)
	var names []string
	for _, command := range discovery.Commands {
		names = append(names, command.Name)
		if command.Name == api.CLIRunDraftForwardCommand || command.Name == api.CLIRunDraftSendAsCommand {
			assertions.False(command.Delegated)
		}
		constructor := mcpDraftCommandConstructors[command.Name]
		requirements.NotNil(constructor)
		var flags []string
		constructor().Flags().VisitAll(func(flag *pflag.Flag) { flags = append(flags, flag.Name) })
		sort.Strings(flags)
		assertions.Equal(flags, command.Flags, command.Name)
	}
	assertions.Equal(mcpDraftCommands(false), names)
	grant, err := owner.IssueAgentToken(t.Context(), "command discovery test", []string{"draft.create"}, []int64{fixture.source.ID}, nil)
	requirements.NoError(err)
	agent, err := daemonclient.New(daemonclient.Config{URL: server.URL, AgentToken: grant.Secret, AllowInsecure: true})
	requirements.NoError(err)
	t.Cleanup(func() { _ = agent.Close() })
	discovery, err = agent.MCPCapabilities(t.Context())
	requirements.NoError(err)
	assertions.True(discovery.Delegated)
	names = nil
	for _, command := range discovery.Commands {
		names = append(names, command.Name)
		assertions.True(command.Delegated)
	}
	assertions.Equal([]string{"draft-compose", "draft-get", "draft-reply"}, names)
}

func TestMCPDelegatedDiscoveryPreservesAdmittedUnion(t *testing.T) {
	fixture := newDraftReplyFixture(t)
	server := mcpDraftTestDaemon(t, fixture.grantedAdapter(), nil)
	session := mcpDraftTestSession(mcpDraftAgentContext(t, server, fixture.source.ID, []string{"draft.create", "inbox.read", "inbox.archive", "calendar.read"}), t)
	names := mcpDraftToolNames(t, session)
	for _, name := range []string{"inbox_candidates", "inbox_get_state", "inbox_get_capabilities", "inbox_list_folders", "inbox_archive", "inbox_unarchive", "inbox_receipt_get", "inbox_reconcile", "draft_compose", "draft_get", "draft_reply", "calendar_freebusy", "calendar_conflicts"} {
		assert.Contains(t, names, name)
	}
	for _, name := range []string{"inbox_set_read", "inbox_set_unread", "inbox_move", "inbox_tags", "inbox_create_folder", "draft_edit", "draft_delete", "draft_recover", "draft_forward", "draft_send_as", mcpserver.ToolSearchMessages} {
		assert.NotContains(t, names, name)
	}
}
func TestMCPOwnerDiscoveryWiresInboxAndExistingLanes(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	fixture := newDraftReplyFixture(t)
	server := mcpDraftTestDaemon(t, fixture.grantedAdapter(), nil)
	client, err := daemonclient.New(daemonclient.Config{URL: server.URL, APIKey: "owner-test-key", AllowInsecure: true})
	requirements.NoError(err)
	t.Cleanup(func() { _ = client.Close() })
	ctx := withStoreResolverConfig(t, &config.Config{HomeDir: t.TempDir(), Remote: config.RemoteConfig{URL: server.URL, APIKey: "owner-test-key", AllowInsecure: true}})
	opts := daemonMCPServeOptions(ctx, client, invocationFromContext(ctx))
	requirements.NotNil(opts.Inbox)
	assertions.NotNil(opts.InboxCandidates)
	assertions.Len(opts.InboxOperations, 12)
	assertions.False(opts.SuppressMessageTagWrites)
	assertions.NotNil(opts.MessageTags)
	assertions.NotNil(opts.Calendar)
	assertions.Equal(mcpDraftCommands(false), opts.DraftCommands)
}

func TestMCPDelegatedNewDaemonRequiresDiscovery(t *testing.T) {
	fixture := newDraftReplyFixture(t)
	server := mcpDraftTestDaemon(t, fixture.grantedAdapter(), func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/v1/mcp/capabilities" {
				http.NotFound(w, r)
				return
			}
			next.ServeHTTP(w, r)
		})
	})
	ctx := mcpDraftAgentContext(t, server, fixture.source.ID, []string{"draft.create"})
	command := &cobra.Command{Use: "mcp"}
	command.SetContext(ctx)
	previous := mcpHTTPAddr
	mcpHTTPAddr = ""
	t.Cleanup(func() { mcpHTTPAddr = previous })
	require.ErrorContains(t, runDelegatedMCP(command), "MCP operation discovery unavailable")
}
func TestMCPDiscoveryRejectsIncompleteInboxContract(t *testing.T) {
	assertions := assert.New(t)

	fixture := newDraftReplyFixture(t)
	server := mcpDraftTestDaemon(t, fixture.grantedAdapter(), func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/api/v1/mcp/capabilities" {
				next.ServeHTTP(w, r)
				return
			}
			recorded := httptest.NewRecorder()
			next.ServeHTTP(recorded, r)
			var descriptor apiprotocol.MCPCapabilities
			if !assert.NoError(t, json.Unmarshal(recorded.Body.Bytes(), &descriptor)) {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			for i, route := range descriptor.Routes {
				if route.OperationID == "controlInbox" {
					descriptor.Routes[i].RequestProperties = []string{"operation", "target"}
				}
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(descriptor)
		})
	})
	ctx := withStoreResolverConfig(t, &config.Config{HomeDir: t.TempDir(), Remote: config.RemoteConfig{URL: server.URL, APIKey: "owner-test-key", AllowInsecure: true}})
	client, err := daemonclient.New(daemonclient.Config{URL: server.URL, APIKey: "owner-test-key", AllowInsecure: true})
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })
	opts := daemonMCPServeOptions(ctx, client, invocationFromContext(ctx))
	assertions.Nil(opts.Inbox)
	assertions.Empty(opts.InboxOperations)
	assertions.NotNil(opts.Calendar)
	assertions.Equal(mcpDraftCommands(false), opts.DraftCommands)
}
func TestMCPAgentOptionsBuilderPreservesDiscoveryUnion(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	fixture := newDraftReplyFixture(t)
	server := mcpDraftTestDaemon(t, fixture.grantedAdapter(), nil)
	ctx := mcpDraftAgentContext(t, server, fixture.source.ID, []string{"draft.create", "inbox.read", "calendar.read"})
	client, _, err := OpenHTTPStore(ctx)
	requirements.NoError(err)
	t.Cleanup(func() { _ = client.Close() })
	opts := daemonMCPServeOptions(ctx, client, invocationFromContext(ctx))
	requirements.NotNil(opts.Inbox)
	assertions.Len(opts.InboxOperations, 5)
	assertions.NotNil(opts.Calendar)
	assertions.Equal([]string{"draft-compose", "draft-get", "draft-reply"}, opts.DraftCommands)
	assertions.Nil(opts.Engine)
	assertions.True(opts.CalendarOnly)
}
