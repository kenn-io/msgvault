package cmd

import (
	"bytes"
	"context"
	"encoding/json/v2"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/apiprotocol"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
	mcpserver "go.kenn.io/msgvault/internal/mcp"
	"go.kenn.io/msgvault/internal/store"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestMCPDraftArgumentsExerciseProductionParsers(t *testing.T) {
	descriptors := registeredMCPCommandDescriptors()
	descriptor := func(name string) apiprotocol.MCPCommandDescriptor {
		for _, d := range descriptors {
			if d.Name == name {
				return d
			}
		}
		require.FailNow(t, "missing production command", name)
		return apiprotocol.MCPCommandDescriptor{}
	}
	args, err := mcpDraftArguments("draft_reply", map[string]any{"message_id": int64(8), "body": "--from=intruder@example.com\ntext", "reply_all": true, "source_id": int64(3), "from": "sender@example.com"}, descriptor("draft-reply"))
	require.NoError(t, err)
	reply, err := parseDraftReplyArgs(args)
	require.NoError(t, err)
	assert.Equal(t, "--from=intruder@example.com\ntext", reply.Body)
	assert.Equal(t, "sender@example.com", reply.From)
	assert.True(t, reply.ReplyAll)
	assert.Equal(t, int64(3), reply.SourceID)
	args, err = mcpDraftArguments("draft_compose", map[string]any{"source_id": int64(3), "to": []string{"one@example.com", "two@example.com"}, "cc": []string{"cc@example.com"}, "body": "", "subject": "--account=other"}, descriptor("draft-compose"))
	require.NoError(t, err)
	compose, err := parseDraftComposeArgs(args)
	require.NoError(t, err)
	assert.Equal(t, []string{"one@example.com", "two@example.com"}, compose.To)
	assert.Equal(t, "--account=other", compose.Subject)
	assert.Empty(t, compose.Body)
	for _, name := range []string{"get_draft", "edit_draft", "delete_draft", "recover_draft"} {
		command := map[string]string{"get_draft": "draft-get", "edit_draft": "draft-edit", "delete_draft": "draft-delete", "recover_draft": "draft-recover"}[name]
		input := map[string]any{"draft_id": "draft_example"}
		if name != "get_draft" {
			input["revision"] = int64(2)
		}
		if name == "edit_draft" {
			input["body"] = ""
		}
		args, err := mcpDraftArguments(name, input, descriptor(command))
		require.NoError(t, err)
		intent, err := parseDraftLifecycleArgs(args)
		require.NoError(t, err)
		assert.Equal(t, "draft_example", intent.DraftID)
		if name != "get_draft" {
			assert.Equal(t, int64(2), intent.Revision)
		}
	}
	args, err = mcpDraftArguments("list_draft_send_as", map[string]any{"account": "sender@example.com"}, descriptor("draft-send-as"))
	require.NoError(t, err)
	account, jsonOutput, err := parseDraftSendAsArgs(args)
	require.NoError(t, err)
	assert.Equal(t, "sender@example.com", account)
	assert.True(t, jsonOutput)
	for _, input := range []map[string]any{
		{"draft_id": "--json", "revision": int64(1), "body": ""},
		{"draft_id": "draft_example", "revision": int64(0), "body": ""},
		{"draft_id": "draft_example", "revision": int64(1), "body": "", "argv": []string{"anything"}},
	} {
		_, err := mcpDraftArguments("edit_draft", input, descriptor("draft-edit"))
		assert.Error(t, err)
	}
	_, err = mcpDraftArguments("draft_compose", map[string]any{"conversation_id": int64(4), "body": "example"}, descriptor("draft-compose"))
	if slices.Contains(descriptor("draft-compose").Flags, "conversation") {
		assert.NoError(t, err)
	} else {
		assert.Error(t, err)
	}
}

func TestMCPDraftDiscoveryUsesActualAdmission(t *testing.T) {
	capabilities := &apiprotocol.MCPCapabilities{Version: 1, Commands: registeredMCPCommandDescriptors()}
	backend := newDaemonMCPOperations(nil, capabilities)
	assert.Contains(t, backend.capabilities(), "draft_reply")
	assert.Contains(t, backend.capabilities(), "get_draft")
	assert.Equal(t, slices.ContainsFunc(capabilities.Commands, func(d apiprotocol.MCPCommandDescriptor) bool { return d.Name == "draft-forward" }), slices.Contains(backend.capabilities(), "draft_forward"))
	assert.Equal(t, slices.ContainsFunc(capabilities.Commands, func(d apiprotocol.MCPCommandDescriptor) bool {
		return d.Name == "draft-get" && slices.Contains(d.Flags, "conversation")
	}), slices.Contains(backend.capabilities(), "list_conversation_drafts"))
	capabilities.Delegated = true
	backend = newDaemonMCPOperations(nil, capabilities)
	assert.Contains(t, backend.capabilities(), "draft_reply")
	assert.Contains(t, backend.capabilities(), "recover_draft")
	assert.Equal(t, agentDelegatedCapable(newDraftGetCommand()), slices.Contains(backend.capabilities(), "get_draft"))
	assert.NotContains(t, backend.capabilities(), "list_draft_send_as")
}

func TestMCPDraftResultsPreserveWithheldAndFailureReceipts(t *testing.T) {
	result, err := decodeMCPDraftResult("get_draft", &daemonclient.MCPCLIResult{Stdout: `{"status":"ok","draft_id":"draft_example","revision":2,"lifecycle":"active","source_id":3,"chat_id":"chat_example","content":null}`})
	require.NoError(t, err)
	require.False(t, result.IsError)
	output := operationOutput[map[string]any](t, result)
	assert.Contains(t, output, "content")
	assert.Nil(t, output["content"])
	result, err = decodeMCPDraftResult("edit_draft", &daemonclient.MCPCLIResult{Failed: true, ErrorCode: "remote_unknown", OperationMayHaveCompleted: true, Stderr: `{"status":"remote_unknown","draft_id":"draft_example","revision":2,"lifecycle":"active","source_id":3,"message_id":4,"receipt":{"mailbox":"Drafts","uid":6,"uidvalidity":7},"pending_operation":"edit","candidate_content":"candidate"}`})
	require.NoError(t, err)
	require.True(t, result.IsError)
	for _, data := range []string{`{"status":"ok"}`, `{"status":"ok","draft_id":"draft_example","revision":2,"source_id":3,"secret":"private"}`, `{}\n{}`, `private diagnostic`} {
		result, _ := decodeMCPDraftResult("get_draft", &daemonclient.MCPCLIResult{Stdout: data})
		require.True(t, result.IsError)
	}
	result, err = decodeMCPDraftResult("edit_draft", &daemonclient.MCPCLIResult{Failed: true, ErrorCode: "not_permitted", Stderr: "private diagnostic"})
	require.NoError(t, err)
	require.True(t, result.IsError)
	encoded, err := json.Marshal(result.Output)
	require.NoError(t, err)
	assert.NotContains(t, string(encoded), "private diagnostic")
}

func mcpDraftDaemonFixture(t *testing.T) (draftReplyFixture, *daemonMCPOperations, string, string) {
	t.Helper()
	fixture := newDraftReplyFixture(t)
	adapter := fixture.grantedAdapter()
	adapter.mcpCommands = registeredMCPCommandDescriptors()
	cfg := &config.Config{HomeDir: t.TempDir(), Server: config.ServerConfig{APIKey: "owner-test-key", AgentAccess: true}}
	server := httptest.NewServer(api.NewServerWithOptions(api.ServerOptions{Config: cfg, Store: adapter, Logger: slog.New(slog.DiscardHandler)}).Router())
	t.Cleanup(server.Close)
	data, err := json.Marshal(map[string]any{"label": "synthetic-agent", "permissions": []string{"draft.create", "draft.edit", "draft.delete"}, "source_ids": []int64{fixture.source.ID}})
	require.NoError(t, err)
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL+"/api/v1/agent-tokens", bytes.NewReader(data))
	require.NoError(t, err)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Api-Key", "owner-test-key")
	response, err := http.DefaultClient.Do(request)
	require.NoError(t, err)
	defer func() { _ = response.Body.Close() }()
	require.Equal(t, http.StatusCreated, response.StatusCode)
	var issued agentTokenIssueFixture
	require.NoError(t, json.UnmarshalRead(response.Body, &issued))
	tokenFile := filepath.Join(t.TempDir(), "grant.token")
	require.NoError(t, os.WriteFile(tokenFile, []byte(issued.Secret), 0o600))
	client, err := daemonclient.New(daemonclient.Config{URL: server.URL, APIKey: "owner-test-key", AllowInsecure: true})
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })
	caps, err := client.MCPCapabilities(t.Context())
	require.NoError(t, err)
	return fixture, newDaemonMCPOperations(client, caps), server.URL, tokenFile
}

func TestMCPDraftRoundTripUsesRealIMAPProducer(t *testing.T) {
	fixture, backend, _, _ := mcpDraftDaemonFixture(t)
	result, err := backend.ExecuteOperation(t.Context(), "draft_reply", map[string]any{"message_id": fixture.parentID, "source_id": fixture.source.ID, "body": "--from=other@example.com\nreply"})
	require.NoError(t, err)
	created := operationOutput[draftReplyOutput](t, result)
	require.Equal(t, "created", created.Status)
	require.NotEmpty(t, created.DraftID)
	_, raw := fetchDraftMailboxMessage(t, fixture.config, store.IMAPDraftReceipt{SourceID: created.SourceID, Mailbox: created.Mailbox, UID: created.UID, UIDValidity: created.UIDValidity})
	assert.Contains(t, string(raw), "From: <alice@example.com>")
	result, err = backend.ExecuteOperation(t.Context(), "get_draft", map[string]any{"draft_id": created.DraftID})
	require.NoError(t, err)
	current := operationOutput[draftLifecycleOutput](t, result)
	assert.Contains(t, current.Content, "--from=other@example.com")
	result, err = backend.ExecuteOperation(t.Context(), "edit_draft", map[string]any{"draft_id": created.DraftID, "revision": created.Revision, "body": ""})
	require.NoError(t, err)
	edited := operationOutput[draftLifecycleOutput](t, result)
	assert.Equal(t, created.Revision+1, edited.Revision)
	result, err = backend.ExecuteOperation(t.Context(), "delete_draft", map[string]any{"draft_id": created.DraftID, "revision": created.Revision})
	require.NoError(t, err)
	require.True(t, result.IsError)
	result, err = backend.ExecuteOperation(t.Context(), "delete_draft", map[string]any{"draft_id": created.DraftID, "revision": edited.Revision})
	require.NoError(t, err)
	deleted := operationOutput[draftLifecycleOutput](t, result)
	assert.Equal(t, "discarded", deleted.Lifecycle)
}

func TestMCPDelegatedAdmissionSkipsOwnerConfiguration(t *testing.T) {
	ctx := testInvocationContext(t.Context(), nil, invocationOptions{agentURL: "https://daemon.example.com", agentTokenFile: "/grant.token", agentURLChanged: true, agentTokenChanged: true})
	command := &cobra.Command{Use: "mcp"}
	command.SetContext(ctx)
	require.NoError(t, rootCmd.PersistentPreRunE(command, nil))
	assert.Nil(t, invocationFromCommand(command).cfg)
}

func TestMCPDelegatedHandshakeUsesOnlyAgentSession(t *testing.T) {
	_, _, url, tokenFile := mcpDraftDaemonFixture(t)
	ctx := testInvocationContext(t.Context(), nil, invocationOptions{agentURL: url, agentTokenFile: tokenFile, agentURLChanged: true, agentTokenChanged: true, agentAllowInsecure: true})
	client, err := openMCPAgentDelegatedStore(ctx, invocationFromContext(ctx))
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })
	assert.True(t, client.UsesDelegatedAuthentication())
	caps, err := client.MCPCapabilities(ctx)
	require.NoError(t, err)
	assert.True(t, caps.Delegated)
	opts := daemonMCPServeOptions(ctx, client, nil)
	assert.True(t, opts.DelegatedOnly)
	assert.Contains(t, opts.OperationCapabilities, "draft_reply")
	assert.Equal(t, agentDelegatedCapable(newDraftGetCommand()), slices.Contains(opts.OperationCapabilities, "get_draft"))
}

func TestMCPDelegatedHTTPRefusesBeforeReadingToken(t *testing.T) {
	ctx := testInvocationContext(t.Context(), nil, invocationOptions{agentURL: "https://daemon.example.com", agentTokenFile: "/missing-grant.token", agentURLChanged: true, agentTokenChanged: true})
	savedHTTP := mcpHTTPAddr
	mcpHTTPAddr = "127.0.0.1:0"
	t.Cleanup(func() { mcpHTTPAddr = savedHTTP })
	command := &cobra.Command{Use: "mcp"}
	command.SetContext(ctx)
	require.ErrorContains(t, mcpCmd.RunE(command, nil), "stdio only")
}

func TestMCPDelegatedHandshakeRejectsOwnerSessionWithoutHealthFallback(t *testing.T) {
	fixture := api.NewServerWithOptions(api.ServerOptions{Config: &config.Config{}, Logger: slog.New(slog.DiscardHandler)})
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		assert.Empty(t, r.Header.Get("X-Api-Key"))
		assert.Equal(t, "synthetic-grant", r.Header.Get("X-Msgvault-Agent-Token"))
		fixture.Router().ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	tokenFile := filepath.Join(t.TempDir(), "grant.token")
	require.NoError(t, os.WriteFile(tokenFile, []byte("synthetic-grant"), 0o600))
	ctx := testInvocationContext(t.Context(), nil, invocationOptions{agentURL: server.URL, agentTokenFile: tokenFile, agentURLChanged: true, agentTokenChanged: true, agentAllowInsecure: true})
	client, err := openMCPAgentDelegatedStore(ctx, invocationFromContext(ctx))
	require.Error(t, err)
	assert.Nil(t, client)
	assert.Equal(t, []string{"/api/session"}, paths)
}

// The property uses the production reply parser as its oracle. Valid text must
// round-trip; invalid UTF-8 and NUL must be refused before any command is sent.
func FuzzMCPDraftReplyBodyIsolation(f *testing.F) {
	f.Add("--from=other@example.com")
	f.Add("")
	f.Add("line\n--json=false")
	f.Add("a\x00b")
	f.Add(string([]byte{0xff}))
	descriptor := apiprotocol.MCPCommandDescriptor{Name: "draft-reply", Flags: []string{"body", "json", "from", "all", "account", "source-id"}}
	f.Fuzz(func(t *testing.T, body string) {
		args, err := mcpDraftArguments("draft_reply", map[string]any{"message_id": int64(3), "source_id": int64(2), "body": body}, descriptor)
		if !utf8.ValidString(body) || strings.ContainsRune(body, 0) {
			assert.Error(t, err)
			return
		}
		require.NoError(t, err)
		intent, err := parseDraftReplyArgs(args)
		require.NoError(t, err)
		assert.Equal(t, body, intent.Body)
		assert.Equal(t, int64(2), intent.SourceID)
		assert.Empty(t, intent.From)
		assert.False(t, intent.ReplyAll)
	})
}

func TestMCPDraftOptionalForwardUsesActualRegistration(t *testing.T) {
	fixture, backend, _, _ := mcpDraftDaemonFixture(t)
	if !slices.Contains(backend.capabilities(), "draft_forward") {
		result, err := backend.ExecuteOperation(t.Context(), "draft_forward", nil)
		require.NoError(t, err)
		assert.True(t, result.IsError)
		return
	}
	result, err := backend.ExecuteOperation(t.Context(), "draft_forward", map[string]any{"message_id": fixture.parentID, "source_id": fixture.source.ID, "body": "forward note", "to": []string{"recipient@example.com"}})
	require.NoError(t, err)
	created := operationOutput[draftReplyOutput](t, result)
	assert.Equal(t, "created", created.Status)
	_, raw := fetchDraftMailboxMessage(t, fixture.config, store.IMAPDraftReceipt{SourceID: created.SourceID, Mailbox: created.Mailbox, UID: created.UID, UIDValidity: created.UIDValidity})
	assert.Contains(t, string(raw), "recipient@example.com")
}

func TestMCPDraftOptionalConversationUsesActualProducer(t *testing.T) {
	fixture, backend, _, _ := mcpDraftDaemonFixture(t)
	if !backend.SupportsConversationDrafts() {
		return
	}
	source, err := fixture.store.GetOrCreateSource("slack", "workspace_example")
	require.NoError(t, err)
	conversation, err := fixture.store.EnsureConversationWithType(source.ID, "channel_example", "channel", "Synthetic chat")
	require.NoError(t, err)
	result, err := backend.ExecuteOperation(t.Context(), "draft_compose", map[string]any{"conversation_id": conversation, "body": "local text"})
	require.NoError(t, err)
	output := operationOutput[map[string]any](t, result)
	assert.Equal(t, "msgvault", output["location"])
	assert.Equal(t, "local text", output["body"])
	result, err = backend.ExecuteOperation(t.Context(), "list_conversation_drafts", map[string]any{"conversation_id": conversation})
	require.NoError(t, err)
	listed := operationOutput[struct {
		Drafts []map[string]any `json:"drafts"`
	}](t, result)
	require.Len(t, listed.Drafts, 1)
	assert.Equal(t, output["draft_id"], listed.Drafts[0]["draft_id"])
}

func TestMCPDelegatedBuiltBinaryHasNoOwnerArchive(t *testing.T) {
	binary := os.Getenv("MSGVAULT_MCP_TEST_BINARY")
	if binary == "" {
		t.Skip("artifact check: set MSGVAULT_MCP_TEST_BINARY to a make build binary")
	}
	_, _, url, tokenFile := mcpDraftDaemonFixture(t)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	for _, home := range []string{filepath.Join(t.TempDir(), "absent", "home"), "/proc/msgvault-mcp-unwritable"} {
		command := exec.CommandContext(ctx, binary, "mcp", "--agent-url="+url, "--agent-token-file="+tokenFile, "--agent-allow-insecure", "--allow-draft-writes")
		command.Env = append(os.Environ(), "MSGVAULT_HOME="+home)
		var stderr bytes.Buffer
		command.Stderr = &stderr
		client := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "synthetic-client", Version: "1"}, nil)
		session, err := client.Connect(ctx, &sdkmcp.CommandTransport{Command: command}, nil)
		require.NoError(t, err, stderr.String())
		assert.Nil(t, session.InitializeResult().Capabilities.Resources)
		tools, err := session.ListTools(ctx, nil)
		require.NoError(t, err)
		var names []string
		for _, tool := range tools.Tools {
			names = append(names, tool.Name)
		}
		assert.Contains(t, names, "draft_reply")
		assert.Contains(t, names, "draft_compose")
		assert.Contains(t, names, "recover_draft")
		assert.NotContains(t, names, "get_message")
		assert.NotContains(t, names, "search_metadata")
		assert.NotContains(t, names, "list_source_status")
		assert.NotContains(t, names, "list_draft_send_as")
		_, err = session.ReadResource(ctx, &sdkmcp.ReadResourceParams{URI: "msgvault://stats"})
		assert.Error(t, err)
		require.NoError(t, session.Close())
		_, err = os.Stat(home)
		assert.True(t, os.IsNotExist(err), "delegated launch must not create its configured home")
	}
}

func TestMCPDraftResultRejectsUnidentifiedProducerShape(t *testing.T) {
	for _, data := range []string{
		`{"status":"ok","draft_id":"draft_example","revision":2,"source_id":3}`,
		`{"status":"ok","draft_id":"draft_example","revision":2,"source_id":3,"chat_id":"chat_example"}`,
		`{"status":"ok","draft_id":"draft_example","revision":2,"source_id":3,"lifecycle":"active","message_id":4,"receipt":{}}`,
	} {
		result, _ := decodeMCPDraftResult("get_draft", &daemonclient.MCPCLIResult{Stdout: data})
		assert.True(t, result.IsError)
	}
}

func TestMCPDraftSDKConfirmsBeforeRealProviderWrite(t *testing.T) {
	fixture, backend, _, _ := mcpDraftDaemonFixture(t)
	approved := false
	approvals := 0
	session := operationMCPSession(t, backend, []mcpserver.OperationFamily{mcpserver.OperationFamilyDrafts}, &sdkmcp.ClientOptions{ElicitationHandler: func(_ context.Context, request *sdkmcp.ElicitRequest) (*sdkmcp.ElicitResult, error) {
		approvals++
		assert.Contains(t, request.Params.Message, "never retries or sends")
		if approved {
			return &sdkmcp.ElicitResult{Action: "accept", Content: map[string]any{"confirm": true}}, nil
		}
		return &sdkmcp.ElicitResult{Action: "decline"}, nil
	}})
	args := map[string]any{"message_id": fixture.parentID, "source_id": fixture.source.ID, "body": "SDK approved reply"}
	declined, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "draft_reply", Arguments: args})
	require.NoError(t, err)
	assert.True(t, declined.IsError)
	assert.Empty(t, *fixture.refreshed)
	approved = true
	result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "draft_reply", Arguments: args})
	require.NoError(t, err)
	require.False(t, result.IsError)
	data, err := json.Marshal(result.StructuredContent)
	require.NoError(t, err)
	var output draftReplyOutput
	require.NoError(t, json.Unmarshal(data, &output))
	assert.Equal(t, "created", output.Status)
	assert.Len(t, *fixture.refreshed, 1)
	assert.Equal(t, 2, approvals)
}
