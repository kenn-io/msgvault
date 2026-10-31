package cmd

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"fmt"
	"go.kenn.io/msgvault/internal/mcpdiscovery"
	"go.kenn.io/msgvault/internal/store"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/daemonclient"
	mcpserver "go.kenn.io/msgvault/internal/mcp"
)

func replyMCPSession(t *testing.T, f *replySurfaceFixture) *sdkmcp.ClientSession {
	t.Helper()
	daemon := httptest.NewServer(f.handler)
	t.Cleanup(daemon.Close)
	remote, err := daemonclient.New(daemonclient.Config{URL: daemon.URL, APIKey: f.cfg.Server.AuthenticationKey(), AllowInsecure: true})
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, remote.Close()) })
	state := invocationFromContext(testInvocationContext(t.Context(), f.cfg, invocationOptions{}))
	opts := daemonMCPServeOptions(t.Context(), remote, state)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	clientTransport, serverTransport := sdkmcp.NewInMemoryTransports()
	done := make(chan error, 1)
	go func() { done <- mcpserver.ServeTransport(ctx, opts, serverTransport) }()
	client := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "reply-surface-test", Version: "1"}, &sdkmcp.ClientOptions{MultiRoundTrip: &sdkmcp.MultiRoundTripOptions{Disabled: true}})
	session, err := client.Connect(ctx, clientTransport, nil)
	t.Cleanup(func() {
		if session != nil {
			_ = session.Close()
		}
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			assert.Fail(t, "MCP transport did not stop after cancellation")
		}
	})
	require.NoError(t, err)
	listed, err := session.ListTools(t.Context(), nil)
	require.NoError(t, err)
	names := []string{}
	for _, tool := range listed.Tools {
		names = append(names, tool.Name)
	}
	require.Contains(t, names, "send_reply", "actual daemonMCPServeOptions must install the health-gated sender")
	assert.NotContains(t, names, "send_x_reply")
	return session
}
func replyResultText(t *testing.T, result *sdkmcp.CallToolResult) string {
	t.Helper()
	var out strings.Builder
	for _, content := range result.Content {
		if text, ok := content.(*sdkmcp.TextContent); ok {
			out.WriteString(text.Text)
		}
	}
	return out.String()
}
func replyConfirmParams(args map[string]any, state string, accept bool) *sdkmcp.CallToolParams {
	return &sdkmcp.CallToolParams{Name: "send_reply", Arguments: args, RequestState: state, InputResponses: sdkmcp.InputResponseMap{"confirm": &sdkmcp.ElicitResult{Action: "accept", Content: map[string]any{"confirm": accept}}}}
}

func TestMCPXDirectConfirmationKeepsOnePreparedDraft(t *testing.T) {
	f := newReplySurfaceFixture(t)
	session := replyMCPSession(t, f)
	args := map[string]any{"message_id": float64(f.parent), "body": "Direct confirmation https://example.com"}
	pending, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "send_reply", Arguments: args})
	require.NoError(t, err)
	require.False(t, pending.IsError, replyResultText(t, pending))
	require.True(t, pending.NeedsInput())
	assert.Zero(t, f.posts.Load())
	prompt, ok := pending.InputRequests["confirm"].(*sdkmcp.ElicitParams)
	require.True(t, ok)
	assert.Contains(t, prompt.Message, args["body"])
	assert.Contains(t, prompt.Message, "102")
	assert.Contains(t, prompt.Message, "0.200000")
	drafts, err := f.st.ListChatDraftsContext(t.Context(), f.draft.ConversationID, func(string, string) error { return nil })
	require.NoError(t, err)
	require.Len(t, drafts, 2)
	var preparedID string
	for _, draft := range drafts {
		if draft.DraftID != f.draft.DraftID {
			preparedID = draft.DraftID
		}
	}
	require.NotEmpty(t, preparedID)
	assert.Contains(t, prompt.Message, preparedID)
	accepted, err := session.CallTool(t.Context(), replyConfirmParams(args, pending.RequestState, true))
	require.NoError(t, err)
	require.False(t, accepted.IsError, replyResultText(t, accepted))
	assert.Contains(t, replyResultText(t, accepted), "9001")
	assert.Equal(t, int32(1), f.posts.Load())
	replay, err := session.CallTool(t.Context(), replyConfirmParams(args, pending.RequestState, true))
	require.NoError(t, err)
	assert.True(t, replay.IsError)
	assert.Equal(t, int32(1), f.posts.Load())
	remaining, err := f.st.ListChatDraftsContext(t.Context(), f.draft.ConversationID, func(string, string) error { return nil })
	require.NoError(t, err)
	assert.Len(t, remaining, 1, "confirmation/replay must never recreate direct mode")
	f.cfg.X.Sends = nil
	require.NoError(t, os.RemoveAll(f.cfg.TokensDir()))
	receipt, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "send_reply", Arguments: map[string]any{"draft_id": preparedID, "revision": float64(1)}})
	require.NoError(t, err)
	assert.False(t, receipt.IsError, replyResultText(t, receipt))
	assert.False(t, receipt.NeedsInput())
	assert.Contains(t, replyResultText(t, receipt), "9001")
	assert.Equal(t, int32(1), f.posts.Load())
}
func TestMCPXCancelledOrChangedReviewDoesNotPost(t *testing.T) {
	for _, change := range []string{"cancel", "policy", "price", "edit", "arguments", "scope"} {
		t.Run(change, func(t *testing.T) {
			f := newReplySurfaceFixture(t)
			session := replyMCPSession(t, f)
			args := map[string]any{"draft_id": f.draft.DraftID, "revision": float64(1)}
			pending, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "send_reply", Arguments: args})
			require.NoError(t, err)
			require.True(t, pending.NeedsInput(), replyResultText(t, pending))
			switch change {
			case "policy":
				f.cfg.X.Sends = nil
			case "price":
				p, err := f.cfg.XAccount("1001")
				require.NoError(t, err)
				p.CreateReservationMicros = 300000
				f.cfg.X.Accounts = append(f.cfg.X.Accounts, p)
			case "edit":
				_, err := f.st.UpdateChatDraftContext(t.Context(), f.draft.DraftID, 1, "different reviewed text")
				require.NoError(t, err)
			case "arguments":
				args = map[string]any{"draft_id": f.draft.DraftID, "revision": float64(2)}
			case "scope":
				require.NoError(t, os.RemoveAll(f.cfg.TokensDir()))
			}
			result, err := session.CallTool(t.Context(), replyConfirmParams(args, pending.RequestState, change != "cancel"))
			require.NoError(t, err)
			assert.True(t, result.IsError, replyResultText(t, result))
			assert.Zero(t, f.posts.Load())
			if change == "cancel" {
				replay, err := session.CallTool(t.Context(), replyConfirmParams(args, pending.RequestState, true))
				require.NoError(t, err)
				assert.True(t, replay.IsError, "a cancelled challenge must be consumed")
				assert.Zero(t, f.posts.Load(), "cancellation cannot later be replayed as approval")
			}
		})
	}
}
func TestSendReplyRegisteredGenericCommand(t *testing.T) {
	command, _, err := rootCmd.Find([]string{"send-reply"})
	require.NoError(t, err)
	assert.Equal(t, "send-reply", command.Name())
	for _, name := range []string{"source-id", "to", "retry", "accept-url-cost"} {
		assert.Nil(t, command.Flags().Lookup(name))
	}
	assert.NotNil(t, command.Flags().Lookup("draft"))
	assert.NotNil(t, command.Flags().Lookup("revision"))
}

// Keep the test output parser exact; JSON remains one SendResult on stdout.
func TestXSendJSONOutputUsesExactIntegers(t *testing.T) {
	f := newReplySurfaceFixture(t)
	out := f.request(t, "/api/v1/cli/run", map[string]any{"args": f.args()})
	require.Equal(t, 200, out.Code)
	lines := strings.Split(strings.TrimSpace(out.Body.String()), "\n")
	var result map[string]any
	for _, line := range lines {
		var event struct {
			Type string `json:"type"`
			Data string `json:"data"`
		}
		require.NoError(t, json.Unmarshal([]byte(line), &event))
		if event.Type == "stdout" {
			require.NoError(t, json.Unmarshal([]byte(event.Data), &result))
		}
	}
	assert.Equal(t, "9001", result["remote_id"])
	assert.Equal(t, float64(200000), result["reserved_micros"])
	assert.NotContains(t, out.Body.String(), "example-access")
	assert.NotContains(t, out.Body.String(), strconv.Quote(f.draft.Body))
}

func TestMCPXHTTPActualConfirmationAndReceipt(t *testing.T) {
	f := newReplySurfaceFixture(t)
	daemon := httptest.NewServer(f.handler)
	t.Cleanup(daemon.Close)
	remote, err := daemonclient.New(daemonclient.Config{URL: daemon.URL, APIKey: f.cfg.Server.AuthenticationKey(), AllowInsecure: true})
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, remote.Close()) })
	opts := daemonMCPServeOptions(t.Context(), remote, invocationFromContext(testInvocationContext(t.Context(), f.cfg, invocationOptions{})))
	ctx, cancel := context.WithCancel(t.Context())
	directory := filepath.Join(t.TempDir(), "mcp")
	done := make(chan error, 1)
	go func() {
		done <- mcpserver.ServeHTTPWithOptions(ctx, opts, mcpserver.HTTPOptions{Addr: "127.0.0.1:0", DiscoveryDirectory: directory, AllowWrites: true})
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			assert.Fail(t, "MCP HTTP listener did not stop")
		}
	})
	var endpoints []mcpdiscovery.Endpoint
	require.Eventually(t, func() bool {
		var err error
		endpoints, err = mcpdiscovery.List(directory)
		return err == nil && len(endpoints) == 1
	}, 30*time.Second, 20*time.Millisecond)
	client := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "reply-http-test", Version: "1"}, &sdkmcp.ClientOptions{MultiRoundTrip: &sdkmcp.MultiRoundTripOptions{Disabled: true}})
	session, err := client.Connect(t.Context(), &sdkmcp.StreamableClientTransport{Endpoint: endpoints[0].URL}, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = session.Close() })
	listed, err := session.ListTools(t.Context(), nil)
	require.NoError(t, err)
	var found bool
	for _, tool := range listed.Tools {
		found = found || tool.Name == "send_reply"
	}
	require.True(t, found)
	args := map[string]any{"draft_id": f.draft.DraftID, "revision": float64(1)}
	pending, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "send_reply", Arguments: args})
	require.NoError(t, err)
	require.True(t, pending.NeedsInput(), replyResultText(t, pending))
	assert.Zero(t, f.posts.Load())
	sent, err := session.CallTool(t.Context(), replyConfirmParams(args, pending.RequestState, true))
	require.NoError(t, err)
	assert.False(t, sent.IsError, replyResultText(t, sent))
	assert.Contains(t, replyResultText(t, sent), "9001")
	assert.Equal(t, int32(1), f.posts.Load())
	replay, err := session.CallTool(t.Context(), replyConfirmParams(args, pending.RequestState, true))
	require.NoError(t, err)
	assert.True(t, replay.IsError)
	assert.Equal(t, int32(1), f.posts.Load())
	f.cfg.X.Sends = nil
	require.NoError(t, os.RemoveAll(f.cfg.TokensDir()))
	receipt, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "send_reply", Arguments: args})
	require.NoError(t, err)
	assert.False(t, receipt.IsError, replyResultText(t, receipt))
	assert.False(t, receipt.NeedsInput())
	assert.Equal(t, int32(1), f.posts.Load())
}

func TestMCPXAdoptionConfirmsExactRemoteIDWithoutPost(t *testing.T) {
	f := newReplySurfaceFixture(t)
	var gets atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost {
			f.posts.Add(1)
			w.WriteHeader(503)
			fmt.Fprint(w, `{"title":"Synthetic uncertainty"}`)
			return
		}
		assert.Equal(t, http.MethodGet, r.Method)
		assert.Equal(t, "9001", r.URL.Query().Get("ids"))
		gets.Add(1)
		fmt.Fprintf(w, `{"data":[{"id":"9001","author_id":"1001","conversation_id":"100","text":%q,"note_tweet":{"text":%q},"created_at":"2026-10-31T06:00:00Z","edit_history_tweet_ids":["9001"],"referenced_tweets":[{"type":"replied_to","id":"102"}]}],"includes":{"users":[{"id":"1001","username":"example_owner","protected":false}]}}`, f.draft.Body, f.draft.Body)
	}))
	t.Cleanup(provider.Close)
	f.adapter.xDeps.apiBaseURL = provider.URL + "/2"
	f.adapter.xDeps.httpClient = provider.Client()
	initial := f.request(t, "/api/v1/cli/run", map[string]any{"args": f.args()})
	assert.Contains(t, initial.Body.String(), "remote_unknown")
	require.Equal(t, int32(1), f.posts.Load())
	f.cfg.X.Sends = nil
	session := replyMCPSession(t, f)
	args := map[string]any{"draft_id": f.draft.DraftID, "revision": float64(1), "adopt_post": "9001"}
	pending, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "send_reply", Arguments: args})
	require.NoError(t, err)
	require.True(t, pending.NeedsInput(), replyResultText(t, pending))
	prompt, ok := pending.InputRequests["confirm"].(*sdkmcp.ElicitParams)
	require.True(t, ok)
	assert.Contains(t, prompt.Message, "9001")
	assert.Contains(t, prompt.Message, "102")
	assert.Contains(t, prompt.Message, "sends nothing")
	assert.Equal(t, int32(1), f.posts.Load())
	assert.Equal(t, int32(1), gets.Load())
	result, err := session.CallTool(t.Context(), replyConfirmParams(args, pending.RequestState, true))
	require.NoError(t, err)
	assert.False(t, result.IsError, replyResultText(t, result))
	assert.Contains(t, replyResultText(t, result), "owner_confirmed")
	assert.Contains(t, replyResultText(t, result), "9001")
	assert.Equal(t, int32(1), f.posts.Load())
	assert.Equal(t, int32(3), gets.Load(), "each independently verified lookup is admitted")
}
func TestMCPXUnsupportedSourceDoesNotCreateProviderDraft(t *testing.T) {
	f := newReplySurfaceFixture(t)
	source, err := f.st.GetOrCreateSource("imap", "reader@example.com")
	require.NoError(t, err)
	now := time.Now().UTC()
	parent, err := f.st.PersistMessageContext(t.Context(), &store.MessagePersistData{Message: &store.Message{SourceID: source.ID, SourceMessageID: "example-parent", MessageType: "email", ArchivedAt: now}, Conversation: &store.ConversationPersistData{SourceConversationID: "example-thread", ConversationType: "email_thread", Title: "Example"}, BodyText: sql.NullString{String: "Example archived mail", Valid: true}})
	require.NoError(t, err)
	session := replyMCPSession(t, f)
	result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "send_reply", Arguments: map[string]any{"message_id": float64(parent), "body": "Must not create a mail draft"}})
	require.NoError(t, err)
	assert.True(t, result.IsError)
	assert.Contains(t, replyResultText(t, result), "replies are not supported for imap sources")
	assert.False(t, result.NeedsInput())
	assert.Zero(t, f.posts.Load())
	out := f.request(t, "/api/v1/cli/run", map[string]any{"args": []string{"send-reply", fmt.Sprint(parent), "--body=Unsupported mail"}})
	assert.Contains(t, out.Body.String(), "replies are not supported for imap sources")
	assert.Zero(t, f.posts.Load())
}

func TestMCPXUnsupportedDaemonDoesNotInstallSendBackend(t *testing.T) {
	f := newReplySurfaceFixture(t)
	for _, version := range []string{"", "3.28.0"} {
		t.Run(version, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/health" {
					w.Header().Set("Content-Type", "application/json")
					fmt.Fprintf(w, `{"status":"ok","api_schema_version":%q}`, version)
					return
				}
				f.handler.ServeHTTP(w, r)
			}))
			t.Cleanup(server.Close)
			remote, err := daemonclient.New(daemonclient.Config{URL: server.URL, APIKey: f.cfg.Server.AuthenticationKey(), AllowInsecure: true})
			require.NoError(t, err)
			t.Cleanup(func() { assert.NoError(t, remote.Close()) })
			opts := daemonMCPServeOptions(t.Context(), remote, invocationFromContext(testInvocationContext(t.Context(), f.cfg, invocationOptions{})))
			assert.Nil(t, opts.Replies)
		})
	}
	assert.Zero(t, f.posts.Load())
}
