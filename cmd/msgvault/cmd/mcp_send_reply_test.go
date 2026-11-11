package cmd

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/mcpdiscovery"
	"go.kenn.io/msgvault/internal/store"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/daemonclient"
	mcpserver "go.kenn.io/msgvault/internal/mcp"
)

func TestMCPSendReplyRequiresCurrentXDaemon(t *testing.T) {
	for _, version := range []string{"3.29.0", "3.34.0", "3.35.0", "5.0.0", "5.1.0"} {
		t.Run(version, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/health" {
					_, _ = fmt.Fprintf(w, `{"status":"ok","api_schema_version":%q}`, version)
					return
				}
				http.NotFound(w, r)
			}))
			t.Cleanup(server.Close)
			cfg := config.NewDefaultConfig()
			cfg.HomeDir = t.TempDir()
			cfg.Data.DataDir = cfg.HomeDir
			cfg.Remote = config.RemoteConfig{URL: server.URL, APIKey: "example-owner-key", AllowInsecure: true}
			session := mcpDraftTestSession(withStoreResolverConfig(t, cfg), t)
			listed, err := session.ListTools(t.Context(), nil)
			require.NoError(err)
			var names []string
			for _, tool := range listed.Tools {
				names = append(names, tool.Name)
			}
			if version == "5.1.0" {
				assert.Contains(names, "send_reply")
			} else {
				assert.NotContains(names, "send_reply")
			}
		})
	}
}

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
	_testAssert := assert.New(t)
	_testRequire := require.New(t)

	f := newReplySurfaceFixture(t)
	session := replyMCPSession(t, f)
	args := map[string]any{"message_id": float64(f.parent), "body": "Direct confirmation https://example.com"}
	pending, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "send_reply", Arguments: args})
	_testRequire.NoError(err)
	_testRequire.False(pending.IsError, replyResultText(t, pending))
	_testRequire.True(pending.NeedsInput())
	_testAssert.Zero(f.posts.Load())
	prompt, ok := pending.InputRequests["confirm"].(*sdkmcp.ElicitParams)
	_testRequire.True(ok)
	_testAssert.Contains(prompt.Message, args["body"])
	_testAssert.Contains(prompt.Message, "102")
	_testAssert.Contains(prompt.Message, "0.200000")
	drafts, err := f.st.ListChatDraftsContext(t.Context(), f.draft.ConversationID, func(string, string) error { return nil })
	_testRequire.NoError(err)
	_testRequire.Len(drafts, 2)
	var preparedID string
	for _, draft := range drafts {
		if draft.DraftID != f.draft.DraftID {
			preparedID = draft.DraftID
		}
	}
	_testRequire.NotEmpty(preparedID)
	_testAssert.Contains(prompt.Message, preparedID)
	accepted, err := session.CallTool(t.Context(), replyConfirmParams(args, pending.RequestState, true))
	_testRequire.NoError(err)
	_testRequire.False(accepted.IsError, replyResultText(t, accepted))
	_testAssert.Contains(replyResultText(t, accepted), "9001")
	_testAssert.Equal(int32(1), f.posts.Load())
	replay, err := session.CallTool(t.Context(), replyConfirmParams(args, pending.RequestState, true))
	_testRequire.NoError(err)
	_testAssert.True(replay.IsError)
	_testAssert.Equal(int32(1), f.posts.Load())
	remaining, err := f.st.ListChatDraftsContext(t.Context(), f.draft.ConversationID, func(string, string) error { return nil })
	_testRequire.NoError(err)
	_testAssert.Len(remaining, 1, "confirmation/replay must never recreate direct mode")
	f.cfg.X.Sends = nil
	_testRequire.NoError(os.RemoveAll(f.cfg.TokensDir()))
	receipt, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "send_reply", Arguments: map[string]any{"draft_id": preparedID, "revision": float64(1)}})
	_testRequire.NoError(err)
	_testAssert.False(receipt.IsError, replyResultText(t, receipt))
	_testAssert.False(receipt.NeedsInput())
	_testAssert.Contains(replyResultText(t, receipt), "9001")
	_testAssert.Equal(int32(1), f.posts.Load())
}
func TestMCPXCancelledOrChangedReviewDoesNotPost(t *testing.T) {
	for _, change := range []string{"cancel", "policy", "price", "edit", "arguments", "scope"} {
		t.Run(change, func(t *testing.T) {
			_testAssert := assert.New(t)
			_testRequire := require.New(t)

			f := newReplySurfaceFixture(t)
			session := replyMCPSession(t, f)
			args := map[string]any{"draft_id": f.draft.DraftID, "revision": float64(1)}
			pending, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "send_reply", Arguments: args})
			_testRequire.NoError(err)
			_testRequire.True(pending.NeedsInput(), replyResultText(t, pending))
			switch change {
			case "policy":
				f.cfg.X.Sends = nil
			case "price":
				p, err := f.cfg.XAccount("1001")
				_testRequire.NoError(err)
				p.CreateReservationMicros = 300000
				f.cfg.X.Accounts = append(f.cfg.X.Accounts, p)
			case "edit":
				_, err := f.st.UpdateChatDraftContext(t.Context(), f.draft.DraftID, 1, "different reviewed text")
				_testRequire.NoError(err)
			case "arguments":
				args = map[string]any{"draft_id": f.draft.DraftID, "revision": float64(2)}
			case "scope":
				_testRequire.NoError(os.RemoveAll(f.cfg.TokensDir()))
			}
			result, err := session.CallTool(t.Context(), replyConfirmParams(args, pending.RequestState, change != "cancel"))
			_testRequire.NoError(err)
			_testAssert.True(result.IsError, replyResultText(t, result))
			_testAssert.Zero(f.posts.Load())
			if change == "cancel" {
				replay, err := session.CallTool(t.Context(), replyConfirmParams(args, pending.RequestState, true))
				_testRequire.NoError(err)
				_testAssert.True(replay.IsError, "a cancelled challenge must be consumed")
				_testAssert.Zero(f.posts.Load(), "cancellation cannot later be replayed as approval")
			}
		})
	}
}

// Keep the test output parser exact; JSON remains one SendResult on stdout.
func TestXSendJSONOutputUsesExactIntegers(t *testing.T) {
	_testAssert := assert.New(t)
	_testRequire := require.New(t)

	f := newReplySurfaceFixture(t)
	out := f.request(t, "/api/v1/cli/run", map[string]any{"args": f.args()})
	_testRequire.Equal(200, out.Code)
	lines := strings.Split(strings.TrimSpace(out.Body.String()), "\n")
	var result map[string]any
	for _, line := range lines {
		var event struct {
			Type string `json:"type"`
			Data string `json:"data"`
		}
		_testRequire.NoError(json.Unmarshal([]byte(line), &event))
		if event.Type == "stdout" {
			_testRequire.NoError(json.Unmarshal([]byte(event.Data), &result))
		}
	}
	_testAssert.Equal("9001", result["remote_id"])
	_testAssert.InDelta(float64(200000), result["reserved_micros"], 0)
	_testAssert.NotContains(out.Body.String(), "example-access")
	_testAssert.NotContains(out.Body.String(), strconv.Quote(f.draft.Body))
}

func TestMCPXHTTPActualConfirmationAndReceipt(t *testing.T) {
	_testAssert := assert.New(t)
	_testRequire := require.New(t)

	f := newReplySurfaceFixture(t)
	daemon := httptest.NewServer(f.handler)
	t.Cleanup(daemon.Close)
	remote, err := daemonclient.New(daemonclient.Config{URL: daemon.URL, APIKey: f.cfg.Server.AuthenticationKey(), AllowInsecure: true})
	_testRequire.NoError(err)
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
	_testRequire.Eventually(func() bool {
		var err error
		endpoints, err = mcpdiscovery.List(directory)
		return err == nil && len(endpoints) == 1
	}, 30*time.Second, 20*time.Millisecond)
	client := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "reply-http-test", Version: "1"}, &sdkmcp.ClientOptions{MultiRoundTrip: &sdkmcp.MultiRoundTripOptions{Disabled: true}})
	session, err := client.Connect(t.Context(), &sdkmcp.StreamableClientTransport{Endpoint: endpoints[0].URL}, nil)
	_testRequire.NoError(err)
	t.Cleanup(func() { _ = session.Close() })
	listed, err := session.ListTools(t.Context(), nil)
	_testRequire.NoError(err)
	var found bool
	for _, tool := range listed.Tools {
		found = found || tool.Name == "send_reply"
	}
	_testRequire.True(found)
	args := map[string]any{"draft_id": f.draft.DraftID, "revision": float64(1)}
	pending, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "send_reply", Arguments: args})
	_testRequire.NoError(err)
	_testRequire.True(pending.NeedsInput(), replyResultText(t, pending))
	_testAssert.Zero(f.posts.Load())
	sent, err := session.CallTool(t.Context(), replyConfirmParams(args, pending.RequestState, true))
	_testRequire.NoError(err)
	_testAssert.False(sent.IsError, replyResultText(t, sent))
	_testAssert.Contains(replyResultText(t, sent), "9001")
	_testAssert.Equal(int32(1), f.posts.Load())
	replay, err := session.CallTool(t.Context(), replyConfirmParams(args, pending.RequestState, true))
	_testRequire.NoError(err)
	_testAssert.True(replay.IsError)
	_testAssert.Equal(int32(1), f.posts.Load())
	f.cfg.X.Sends = nil
	_testRequire.NoError(os.RemoveAll(f.cfg.TokensDir()))
	receipt, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "send_reply", Arguments: args})
	_testRequire.NoError(err)
	_testAssert.False(receipt.IsError, replyResultText(t, receipt))
	_testAssert.False(receipt.NeedsInput())
	_testAssert.Equal(int32(1), f.posts.Load())
}

func TestMCPXAdoptionConfirmsExactRemoteIDWithoutPost(t *testing.T) {
	_testAssert := assert.New(t)
	_testRequire := require.New(t)

	f := newReplySurfaceFixture(t)
	var gets atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost {
			f.posts.Add(1)
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = fmt.Fprint(w, `{"title":"Synthetic uncertainty"}`)
			return
		}
		assert.Equal(t, http.MethodGet, r.Method)
		assert.Equal(t, "9001", r.URL.Query().Get("ids"))
		gets.Add(1)
		_, _ = fmt.Fprintf(w, `{"data":[{"id":"9001","author_id":"1001","conversation_id":"100","text":%q,"note_tweet":{"text":%q},"created_at":"2026-10-31T06:00:00Z","edit_history_tweet_ids":["9001"],"referenced_tweets":[{"type":"replied_to","id":"102"}]}],"includes":{"users":[{"id":"1001","username":"example_owner","protected":false}]}}`, f.draft.Body, f.draft.Body)
	}))
	t.Cleanup(provider.Close)
	f.adapter.xDeps.apiBaseURL = provider.URL + "/2"
	f.adapter.xDeps.httpClient = provider.Client()
	initial := f.request(t, "/api/v1/cli/run", map[string]any{"args": f.args()})
	_testAssert.Contains(initial.Body.String(), "remote_unknown")
	_testRequire.Equal(int32(1), f.posts.Load())
	f.cfg.X.Sends = nil
	session := replyMCPSession(t, f)
	args := map[string]any{"draft_id": f.draft.DraftID, "revision": float64(1), "adopt_post": "9001"}
	pending, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "send_reply", Arguments: args})
	_testRequire.NoError(err)
	_testRequire.True(pending.NeedsInput(), replyResultText(t, pending))
	prompt, ok := pending.InputRequests["confirm"].(*sdkmcp.ElicitParams)
	_testRequire.True(ok)
	_testAssert.Contains(prompt.Message, "9001")
	_testAssert.Contains(prompt.Message, "102")
	_testAssert.Contains(prompt.Message, "sends nothing")
	_testAssert.Equal(int32(1), f.posts.Load())
	_testAssert.Equal(int32(1), gets.Load())
	result, err := session.CallTool(t.Context(), replyConfirmParams(args, pending.RequestState, true))
	_testRequire.NoError(err)
	_testAssert.False(result.IsError, replyResultText(t, result))
	_testAssert.Contains(replyResultText(t, result), "owner_confirmed")
	_testAssert.Contains(replyResultText(t, result), "9001")
	_testAssert.Equal(int32(1), f.posts.Load())
	_testAssert.Equal(int32(3), gets.Load(), "each independently verified lookup is admitted")
}
func TestMCPXUnsupportedSourceDoesNotCreateProviderDraft(t *testing.T) {
	_testAssert := assert.New(t)
	_testRequire := require.New(t)

	f := newReplySurfaceFixture(t)
	source, err := f.st.GetOrCreateSource("imap", "reader@example.com")
	_testRequire.NoError(err)
	now := time.Now().UTC()
	parent, err := f.st.PersistMessageContext(t.Context(), &store.MessagePersistData{Message: &store.Message{SourceID: source.ID, SourceMessageID: "example-parent", MessageType: "email", ArchivedAt: now}, Conversation: &store.ConversationPersistData{SourceConversationID: "example-thread", ConversationType: "email_thread", Title: "Example"}, BodyText: sql.NullString{String: "Example archived mail", Valid: true}})
	_testRequire.NoError(err)
	session := replyMCPSession(t, f)
	result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "send_reply", Arguments: map[string]any{"message_id": float64(parent), "body": "Must not create a mail draft"}})
	_testRequire.NoError(err)
	_testAssert.True(result.IsError)
	_testAssert.Contains(replyResultText(t, result), "replies are not supported for imap sources")
	_testAssert.False(result.NeedsInput())
	_testAssert.Zero(f.posts.Load())
	out := f.request(t, "/api/v1/cli/run", map[string]any{"args": []string{"send-reply", strconv.FormatInt(parent, 10), "--body=Unsupported mail"}})
	_testAssert.Contains(out.Body.String(), "replies are not supported for imap sources")
	_testAssert.Zero(f.posts.Load())
}

func TestMCPXUnsupportedDaemonDoesNotInstallSendBackend(t *testing.T) {
	f := newReplySurfaceFixture(t)
	for _, version := range []string{"", "3.28.0"} {
		t.Run(version, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/health" {
					w.Header().Set("Content-Type", "application/json")
					_, _ = fmt.Fprintf(w, `{"status":"ok","api_schema_version":%q}`, version)
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
