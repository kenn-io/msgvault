package cmd

import (
	"bytes"
	"context"
	"encoding/json/v2"
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
)

func TestMCPDraftArgumentsExerciseProductionParsers(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	descriptors := registeredMCPCommandDescriptors()
	descriptor := func(name string) apiprotocol.MCPCommandDescriptor {
		for _, d := range descriptors {
			if d.Name == name {
				return d
			}
		}
		requirements.FailNow("missing production command", name)
		return apiprotocol.MCPCommandDescriptor{}
	}
	args, err := mcpDraftArguments("draft_reply", map[string]any{"message_id": int64(8), "body": "--from=intruder@example.com\ntext", "reply_all": true, "source_id": int64(3), "from": "sender@example.com"}, descriptor("draft-reply"))
	requirements.NoError(err)
	reply, err := parseDraftReplyArgs(args)
	requirements.NoError(err)
	assertions.Equal("--from=intruder@example.com\ntext", reply.Body)
	assertions.Equal("sender@example.com", reply.From)
	assertions.True(reply.ReplyAll)
	assertions.Equal(int64(3), reply.SourceID)
	args, err = mcpDraftArguments("draft_compose", map[string]any{"source_id": int64(3), "to": []string{"one@example.com", "two@example.com"}, "cc": []string{"cc@example.com"}, "body": "", "subject": "--account=other"}, descriptor("draft-compose"))
	requirements.NoError(err)
	compose, err := parseDraftComposeArgs(args)
	requirements.NoError(err)
	assertions.Equal([]string{"one@example.com", "two@example.com"}, compose.To)
	assertions.Equal("--account=other", compose.Subject)
	assertions.Empty(compose.Body)
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
		requirements.NoError(err)
		intent, err := parseDraftLifecycleArgs(args)
		requirements.NoError(err)
		assertions.Equal("draft_example", intent.DraftID)
		if name != "get_draft" {
			assertions.Equal(int64(2), intent.Revision)
		}
	}
	args, err = mcpDraftArguments("list_draft_send_as", map[string]any{"account": "sender@example.com"}, descriptor("draft-send-as"))
	requirements.NoError(err)
	account, jsonOutput, err := parseDraftSendAsArgs(args)
	requirements.NoError(err)
	assertions.Equal("sender@example.com", account)
	assertions.True(jsonOutput)
	for _, input := range []map[string]any{
		{"draft_id": "--json", "revision": int64(1), "body": ""},
		{"draft_id": "draft_example", "revision": int64(0), "body": ""},
		{"draft_id": "draft_example", "revision": int64(1), "body": "", "argv": []string{"anything"}},
	} {
		_, err := mcpDraftArguments("edit_draft", input, descriptor("draft-edit"))
		requirements.Error(err)
	}
	_, err = mcpDraftArguments("draft_compose", map[string]any{"conversation_id": int64(4), "body": "example"}, descriptor("draft-compose"))
	if slices.Contains(descriptor("draft-compose").Flags, "conversation") {
		requirements.NoError(err)
	} else {
		requirements.Error(err)
	}
}

func TestMCPDraftDiscoveryUsesActualAdmission(t *testing.T) {
	assertions := assert.New(t)
	capabilities := &apiprotocol.MCPCapabilities{Version: 1, Commands: registeredMCPCommandDescriptors()}
	backend := newDaemonMCPOperations(nil, capabilities)
	assertions.Contains(backend.capabilities(), "draft_reply")
	assertions.Contains(backend.capabilities(), "get_draft")
	assertions.Equal(slices.ContainsFunc(capabilities.Commands, func(d apiprotocol.MCPCommandDescriptor) bool { return d.Name == "draft-forward" }), slices.Contains(backend.capabilities(), "draft_forward"))
	assertions.Equal(slices.ContainsFunc(capabilities.Commands, func(d apiprotocol.MCPCommandDescriptor) bool {
		return d.Name == "draft-get" && slices.Contains(d.Flags, "conversation")
	}), slices.Contains(backend.capabilities(), "list_conversation_drafts"))
	capabilities.Delegated = true
	backend = newDaemonMCPOperations(nil, capabilities)
	assertions.Contains(backend.capabilities(), "draft_reply")
	assertions.Contains(backend.capabilities(), "recover_draft")
	assertions.Equal(agentDelegatedCapable(newDraftGetCommand()), slices.Contains(backend.capabilities(), "get_draft"))
	assertions.NotContains(backend.capabilities(), "list_draft_send_as")
}

func TestMCPDraftResultsPreserveWithheldAndFailureReceipts(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	result, err := decodeMCPDraftResult("get_draft", &daemonclient.MCPCLIResult{Stdout: `{"status":"ok","draft_id":"draft_example","revision":2,"lifecycle":"active","source_id":3,"chat_id":"chat_example","content":null}`})
	requirements.NoError(err)
	requirements.False(result.IsError)
	output := operationOutput[map[string]any](t, result)
	assertions.Contains(output, "content")
	assertions.Nil(output["content"])
	result, err = decodeMCPDraftResult("edit_draft", &daemonclient.MCPCLIResult{Failed: true, ErrorCode: "remote_unknown", OperationMayHaveCompleted: true, Stderr: `{"status":"remote_unknown","draft_id":"draft_example","revision":2,"lifecycle":"active","source_id":3,"message_id":4,"receipt":{"mailbox":"Drafts","uid":6,"uidvalidity":7},"pending_operation":"edit","candidate_content":"candidate"}`})
	requirements.NoError(err)
	requirements.True(result.IsError)
	for _, data := range []string{`{"status":"ok"}`, `{"status":"ok","draft_id":"draft_example","revision":2,"source_id":3,"secret":"private"}`, `{}\n{}`, `private diagnostic`} {
		result, _ := decodeMCPDraftResult("get_draft", &daemonclient.MCPCLIResult{Stdout: data})
		requirements.True(result.IsError)
	}
	result, err = decodeMCPDraftResult("edit_draft", &daemonclient.MCPCLIResult{Failed: true, ErrorCode: "not_permitted", Stderr: "private diagnostic"})
	requirements.NoError(err)
	requirements.True(result.IsError)
	encoded, err := json.Marshal(result.Output)
	requirements.NoError(err)
	assertions.NotContains(string(encoded), "private diagnostic")
}

func mcpDraftDaemonFixture(t *testing.T) (draftReplyFixture, *daemonMCPOperations, string, string) {
	t.Helper()
	requirements := require.New(t)
	fixture := newDraftReplyFixture(t)
	adapter := fixture.grantedAdapter()
	adapter.mcpCommands = registeredMCPCommandDescriptors()
	cfg := &config.Config{HomeDir: t.TempDir(), Server: config.ServerConfig{APIKey: "owner-test-key", AgentAccess: true}}
	server := httptest.NewServer(api.NewServerWithOptions(api.ServerOptions{Config: cfg, Store: adapter, Logger: slog.New(slog.DiscardHandler)}).Router())
	t.Cleanup(server.Close)
	data, err := json.Marshal(map[string]any{"label": "synthetic-agent", "permissions": []string{"draft.create", "draft.edit", "draft.delete"}, "source_ids": []int64{fixture.source.ID}})
	requirements.NoError(err)
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL+"/api/v1/agent-tokens", bytes.NewReader(data))
	requirements.NoError(err)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Api-Key", "owner-test-key")
	response, err := http.DefaultClient.Do(request)
	requirements.NoError(err)
	defer func() { _ = response.Body.Close() }()
	requirements.Equal(http.StatusCreated, response.StatusCode)
	var issued agentTokenIssueFixture
	requirements.NoError(json.UnmarshalRead(response.Body, &issued))
	tokenFile := filepath.Join(t.TempDir(), "grant.token")
	requirements.NoError(os.WriteFile(tokenFile, []byte(issued.Secret), 0o600))
	client, err := daemonclient.New(daemonclient.Config{URL: server.URL, APIKey: "owner-test-key", AllowInsecure: true})
	requirements.NoError(err)
	t.Cleanup(func() { _ = client.Close() })
	caps, err := client.MCPCapabilities(t.Context())
	requirements.NoError(err)
	return fixture, newDaemonMCPOperations(client, caps), server.URL, tokenFile
}

func TestMCPDraftRoundTripUsesRealIMAPProducer(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	fixture, backend, _, _ := mcpDraftDaemonFixture(t)
	result, err := backend.ExecuteOperation(t.Context(), "draft_reply", map[string]any{"message_id": fixture.parentID, "source_id": fixture.source.ID, "body": "--from=other@example.com\nreply"})
	requirements.NoError(err)
	created := operationOutput[draftReplyOutput](t, result)
	requirements.Equal("created", created.Status)
	requirements.NotEmpty(created.DraftID)
	_, raw := fetchDraftMailboxMessage(t, fixture.config, store.IMAPDraftReceipt{SourceID: created.SourceID, Mailbox: created.Mailbox, UID: created.UID, UIDValidity: created.UIDValidity})
	assertions.Contains(string(raw), "From: <alice@example.com>")
	result, err = backend.ExecuteOperation(t.Context(), "get_draft", map[string]any{"draft_id": created.DraftID})
	requirements.NoError(err)
	current := operationOutput[draftLifecycleOutput](t, result)
	assertions.Contains(current.Content, "--from=other@example.com")
	result, err = backend.ExecuteOperation(t.Context(), "edit_draft", map[string]any{"draft_id": created.DraftID, "revision": created.Revision, "body": ""})
	requirements.NoError(err)
	edited := operationOutput[draftLifecycleOutput](t, result)
	assertions.Equal(created.Revision+1, edited.Revision)
	result, err = backend.ExecuteOperation(t.Context(), "delete_draft", map[string]any{"draft_id": created.DraftID, "revision": created.Revision})
	requirements.NoError(err)
	requirements.True(result.IsError)
	result, err = backend.ExecuteOperation(t.Context(), "delete_draft", map[string]any{"draft_id": created.DraftID, "revision": edited.Revision})
	requirements.NoError(err)
	deleted := operationOutput[draftLifecycleOutput](t, result)
	assertions.Equal("discarded", deleted.Lifecycle)
}

func TestMCPDelegatedAdmissionSkipsOwnerConfiguration(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	ctx := testInvocationContext(t.Context(), nil, invocationOptions{agentURL: "https://daemon.example.com", agentTokenFile: "/grant.token", agentURLChanged: true, agentTokenChanged: true})
	command := &cobra.Command{Use: "mcp"}
	command.SetContext(ctx)
	requirements.NoError(rootCmd.PersistentPreRunE(command, nil))
	assertions.Nil(invocationFromCommand(command).cfg)
}

func TestMCPDelegatedHandshakeUsesOnlyAgentSession(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	_, _, url, tokenFile := mcpDraftDaemonFixture(t)
	ctx := testInvocationContext(t.Context(), nil, invocationOptions{agentURL: url, agentTokenFile: tokenFile, agentURLChanged: true, agentTokenChanged: true, agentAllowInsecure: true})
	client, err := openMCPAgentDelegatedStore(ctx, invocationFromContext(ctx))
	requirements.NoError(err)
	t.Cleanup(func() { _ = client.Close() })
	assertions.True(client.UsesDelegatedAuthentication())
	caps, err := client.MCPCapabilities(ctx)
	requirements.NoError(err)
	assertions.True(caps.Delegated)
	opts := daemonMCPServeOptions(ctx, client, nil)
	assertions.True(opts.DelegatedOnly)
	assertions.Contains(opts.OperationCapabilities, "draft_reply")
	assertions.Equal(agentDelegatedCapable(newDraftGetCommand()), slices.Contains(opts.OperationCapabilities, "get_draft"))
}

func TestMCPDelegatedHTTPRefusesBeforeReadingToken(t *testing.T) {
	requirements := require.New(t)
	ctx := testInvocationContext(t.Context(), nil, invocationOptions{agentURL: "https://daemon.example.com", agentTokenFile: "/missing-grant.token", agentURLChanged: true, agentTokenChanged: true})
	savedHTTP := mcpHTTPAddr
	mcpHTTPAddr = "127.0.0.1:0"
	t.Cleanup(func() { mcpHTTPAddr = savedHTTP })
	command := &cobra.Command{Use: "mcp"}
	command.SetContext(ctx)
	requirements.ErrorContains(mcpCmd.RunE(command, nil), "stdio only")
}

func TestMCPDelegatedHandshakeRejectsOwnerSessionWithoutHealthFallback(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	fixture := api.NewServerWithOptions(api.ServerOptions{Config: &config.Config{}, Logger: slog.New(slog.DiscardHandler)})
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		assertions.Empty(r.Header.Get("X-Api-Key"))
		assertions.Equal("synthetic-grant", r.Header.Get("X-Msgvault-Agent-Token"))
		fixture.Router().ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	tokenFile := filepath.Join(t.TempDir(), "grant.token")
	requirements.NoError(os.WriteFile(tokenFile, []byte("synthetic-grant"), 0o600))
	ctx := testInvocationContext(t.Context(), nil, invocationOptions{agentURL: server.URL, agentTokenFile: tokenFile, agentURLChanged: true, agentTokenChanged: true, agentAllowInsecure: true})
	client, err := openMCPAgentDelegatedStore(ctx, invocationFromContext(ctx))
	requirements.Error(err)
	assertions.Nil(client)
	assertions.Equal([]string{"/api/session"}, paths)
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
	requirements := require.New(t)
	assertions := assert.New(t)
	fixture, backend, _, _ := mcpDraftDaemonFixture(t)
	if !slices.Contains(backend.capabilities(), "draft_forward") {
		result, err := backend.ExecuteOperation(t.Context(), "draft_forward", nil)
		requirements.NoError(err)
		assertions.True(result.IsError)
		return
	}
	result, err := backend.ExecuteOperation(t.Context(), "draft_forward", map[string]any{"message_id": fixture.parentID, "source_id": fixture.source.ID, "body": "forward note", "to": []string{"recipient@example.com"}})
	requirements.NoError(err)
	created := operationOutput[draftReplyOutput](t, result)
	assertions.Equal("created", created.Status)
	_, raw := fetchDraftMailboxMessage(t, fixture.config, store.IMAPDraftReceipt{SourceID: created.SourceID, Mailbox: created.Mailbox, UID: created.UID, UIDValidity: created.UIDValidity})
	assertions.Contains(string(raw), "recipient@example.com")
}

func TestMCPDraftOptionalConversationUsesActualProducer(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	fixture, backend, _, _ := mcpDraftDaemonFixture(t)
	if !backend.SupportsConversationDrafts() {
		return
	}
	source, err := fixture.store.GetOrCreateSource("slack", "workspace_example")
	requirements.NoError(err)
	conversation, err := fixture.store.EnsureConversationWithType(source.ID, "channel_example", "channel", "Synthetic chat")
	requirements.NoError(err)
	result, err := backend.ExecuteOperation(t.Context(), "draft_compose", map[string]any{"conversation_id": conversation, "body": "local text"})
	requirements.NoError(err)
	output := operationOutput[map[string]any](t, result)
	assertions.Equal("msgvault", output["location"])
	assertions.Equal("local text", output["body"])
	result, err = backend.ExecuteOperation(t.Context(), "list_conversation_drafts", map[string]any{"conversation_id": conversation})
	requirements.NoError(err)
	listed := operationOutput[struct {
		Drafts []map[string]any `json:"drafts"`
	}](t, result)
	requirements.Len(listed.Drafts, 1)
	assertions.Equal(output["draft_id"], listed.Drafts[0]["draft_id"])
}

func TestMCPDelegatedBuiltBinaryHasNoOwnerArchive(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	binary := os.Getenv("MSGVAULT_MCP_TEST_BINARY")
	if binary == "" {
		t.Skip("artifact check: set MSGVAULT_MCP_TEST_BINARY to a make build binary")
	}
	_, _, url, tokenFile := mcpDraftDaemonFixture(t)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	for _, home := range []string{filepath.Join(t.TempDir(), "absent", "home"), "/proc/msgvault-mcp-unwritable"} {
		//nolint:gosec // The operator selects a locally built msgvault binary for this production artifact test.
		command := exec.CommandContext(ctx, binary, "mcp", "--agent-url="+url, "--agent-token-file="+tokenFile, "--agent-allow-insecure", "--allow-draft-writes")
		command.Env = append(os.Environ(), "MSGVAULT_HOME="+home)
		var stderr bytes.Buffer
		command.Stderr = &stderr
		client := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "synthetic-client", Version: "1"}, nil)
		session, err := client.Connect(ctx, &sdkmcp.CommandTransport{Command: command}, nil)
		requirements.NoError(err, stderr.String())
		assertions.Nil(session.InitializeResult().Capabilities.Resources)
		tools, err := session.ListTools(ctx, nil)
		requirements.NoError(err)
		var names []string
		for _, tool := range tools.Tools {
			names = append(names, tool.Name)
		}
		assertions.Contains(names, "draft_reply")
		assertions.Contains(names, "draft_compose")
		assertions.Contains(names, "recover_draft")
		assertions.NotContains(names, "get_message")
		assertions.NotContains(names, "search_metadata")
		assertions.NotContains(names, "list_source_status")
		assertions.NotContains(names, "list_draft_send_as")
		_, err = session.ReadResource(ctx, &sdkmcp.ReadResourceParams{URI: "msgvault://stats"})
		requirements.Error(err)
		requirements.NoError(session.Close())
		_, err = os.Stat(home)
		assertions.True(os.IsNotExist(err), "delegated launch must not create its configured home")
	}
}

func TestMCPDraftResultRejectsUnidentifiedProducerShape(t *testing.T) {
	assertions := assert.New(t)
	for _, data := range []string{
		`{"status":"ok","draft_id":"draft_example","revision":2,"source_id":3}`,
		`{"status":"ok","draft_id":"draft_example","revision":2,"source_id":3,"chat_id":"chat_example"}`,
		`{"status":"ok","draft_id":"draft_example","revision":2,"source_id":3,"lifecycle":"active","message_id":4,"receipt":{}}`,
	} {
		result, _ := decodeMCPDraftResult("get_draft", &daemonclient.MCPCLIResult{Stdout: data})
		assertions.True(result.IsError)
	}
}

func TestMCPDraftSDKConfirmsBeforeRealProviderWrite(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	fixture, backend, _, _ := mcpDraftDaemonFixture(t)
	approved := false
	approvals := 0
	session := operationMCPSession(t, backend, []mcpserver.OperationFamily{mcpserver.OperationFamilyDrafts}, &sdkmcp.ClientOptions{ElicitationHandler: func(_ context.Context, request *sdkmcp.ElicitRequest) (*sdkmcp.ElicitResult, error) {
		approvals++
		assertions.Contains(request.Params.Message, "never retries or sends")
		if approved {
			return &sdkmcp.ElicitResult{Action: "accept", Content: map[string]any{"confirm": true}}, nil
		}
		return &sdkmcp.ElicitResult{Action: "decline"}, nil
	}})
	args := map[string]any{"message_id": fixture.parentID, "source_id": fixture.source.ID, "body": "SDK approved reply"}
	declined, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "draft_reply", Arguments: args})
	requirements.NoError(err)
	assertions.True(declined.IsError)
	assertions.Empty(*fixture.refreshed)
	approved = true
	result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "draft_reply", Arguments: args})
	requirements.NoError(err)
	requirements.False(result.IsError)
	data, err := json.Marshal(result.StructuredContent)
	requirements.NoError(err)
	var output draftReplyOutput
	requirements.NoError(json.Unmarshal(data, &output))
	assertions.Equal("created", output.Status)
	assertions.Len(*fixture.refreshed, 1)
	assertions.Equal(2, approvals)
}
