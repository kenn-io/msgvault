package cmd

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/daemon"
	"go.kenn.io/msgvault/internal/agentgrant"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/apiprotocol"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	xapi "go.kenn.io/msgvault/internal/x"
)

type replySurfaceFixture struct {
	st           *store.Store
	cfg          *config.Config
	source       *store.Source
	parent       int64
	draft        store.ChatDraft
	adapter      *storeAPIAdapter
	provider     *httptest.Server
	handler      http.Handler
	posts        atomic.Int32
	postedText   atomic.Value
	postedTarget atomic.Value
}

func newReplySurfaceFixture(t *testing.T) *replySurfaceFixture {
	t.Helper()
	f := &replySurfaceFixture{st: testutil.NewTestStore(t), cfg: config.NewDefaultConfig()}
	f.cfg.HomeDir = t.TempDir()
	f.cfg.Data.DataDir = f.cfg.HomeDir
	f.cfg.Server.APIKey = "example-owner-key"
	f.cfg.Server.AgentAccess = true
	if f.st.IsPostgreSQL() {
		f.cfg.Data.DatabaseURL = os.Getenv("MSGVAULT_TEST_DB")
	}
	var err error
	f.source, err = f.st.GetOrCreateSource("x", "1001")
	require.NoError(t, err)
	now := time.Now().UTC().Add(-time.Hour)
	saved, err := f.st.PersistXPostContext(t.Context(), store.XPersistPostInput{SourceID: f.source.ID, OwnerID: "1001", PostID: "102", AuthorID: "1002", ConversationID: "100", EditHistory: []string{"100", "102"}, Text: "public parent", CreatedAt: now, FetchedAt: now, Availability: "public", Author: &store.XAuthorInput{UserID: "1002", Username: "example_author", FetchedAt: now}})
	require.NoError(t, err)
	f.parent = saved.MessageID
	parent, err := f.st.GetMessageContext(t.Context(), f.parent)
	require.NoError(t, err)
	f.draft, err = f.st.CreateChatDraftContext(t.Context(), parent.ConversationID, f.parent, "Thanks https://example.com", func(string, string) error { return nil })
	require.NoError(t, err)
	f.cfg.X.Sends = []config.XSendConfig{{UserID: "1001", Enabled: true}}
	manager := xapi.NewTokenManager(f.cfg.TokensDir())
	record := xapi.NewTokenRecord("1001", "example-client", xapi.PublicClient, "example-access", "example-refresh", "")
	record.GrantedScopes = []string{"tweet.read", "users.read", "tweet.write"}
	record.Expiry = time.Now().Add(time.Hour)
	require.NoError(t, manager.Save(t.Context(), record))
	record, err = manager.Load(t.Context(), "1001")
	require.NoError(t, err)
	_, err = f.st.UpdateXControlContext(t.Context(), f.source.ID, 0, store.XControlPatch{OwnerID: "1001", Generation: &record.Generation})
	require.NoError(t, err)
	f.provider = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "/2/tweets", r.URL.Path)
		assert.Equal(t, "Bearer example-access", r.Header.Get("Authorization"))
		f.posts.Add(1)
		var submitted struct {
			Text  string `json:"text"`
			Reply struct {
				ID string `json:"in_reply_to_tweet_id"`
			} `json:"reply"`
		}
		if err := json.UnmarshalRead(r.Body, &submitted); err != nil {
			assert.NoError(t, err)
			w.WriteHeader(400)
			return
		}
		f.postedText.Store(submitted.Text)
		f.postedTarget.Store(submitted.Reply.ID)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, `{"data":{"id":"9001"}}`)
	}))
	t.Cleanup(f.provider.Close)
	deps := xCommandDeps{apiBaseURL: f.provider.URL + "/2", httpClient: f.provider.Client()}
	f.adapter = &storeAPIAdapter{store: f.st, config: f.cfg, xDeps: &deps, logger: slog.New(slog.DiscardHandler)}
	f.handler = api.NewServerWithOptions(api.ServerOptions{Config: f.cfg, Store: f.adapter, Logger: slog.New(slog.DiscardHandler)}).Router()
	return f
}
func (f *replySurfaceFixture) request(t *testing.T, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	encoded, err := json.Marshal(body)
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(encoded))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+f.cfg.Server.AuthenticationKey())
	out := httptest.NewRecorder()
	f.handler.ServeHTTP(out, req)
	return out
}
func (f *replySurfaceFixture) args() []string {
	return []string{"send-reply", "--draft", f.draft.DraftID, "--revision", "1", "--json"}
}

func TestXSendDispatchRequiresOwner(t *testing.T) {
	t.Run("owner receipt survives revoked token and opt out", func(t *testing.T) {
		f := newReplySurfaceFixture(t)
		out := f.request(t, "/api/v1/cli/run", map[string]any{"args": f.args()})
		require.Equal(t, 200, out.Code, out.Body.String())
		assert.Contains(t, out.Body.String(), "9001")
		assert.Contains(t, out.Body.String(), "sent")
		assert.Equal(t, int32(1), f.posts.Load())
		f.cfg.X.Sends = nil
		require.NoError(t, os.RemoveAll(f.cfg.TokensDir()))
		retry := f.request(t, "/api/v1/cli/run", map[string]any{"args": f.args()})
		require.Equal(t, 200, retry.Code, retry.Body.String())
		assert.Contains(t, retry.Body.String(), "9001")
		assert.Equal(t, int32(1), f.posts.Load())
		prep := f.request(t, "/api/v1/x/replies/prepare", map[string]any{"draft_id": f.draft.DraftID, "revision": 1})
		require.Equal(t, 200, prep.Code, prep.Body.String())
		assert.Contains(t, prep.Body.String(), `"mode":"receipt"`)
		assert.Contains(t, prep.Body.String(), "9001")
	})
	for _, which := range []string{"disabled", "wrong owner", "scope"} {
		t.Run(which, func(t *testing.T) {
			f := newReplySurfaceFixture(t)
			switch which {
			case "disabled":
				f.cfg.X.Sends = nil
			case "wrong owner":
				f.cfg.X.Sends = []config.XSendConfig{{UserID: "2001", Enabled: true}}
			case "scope":
				manager := xapi.NewTokenManager(f.cfg.TokensDir())
				r, err := manager.Load(t.Context(), "1001")
				require.NoError(t, err)
				r.GrantedScopes = []string{"tweet.read", "users.read"}
				require.NoError(t, manager.Save(t.Context(), r))
			}
			out := f.request(t, "/api/v1/cli/run", map[string]any{"args": f.args()})
			assert.Contains(t, out.Body.String(), "error")
			assert.Zero(t, f.posts.Load())
		})
	}
	t.Run("adapter refuses grant before archive", func(t *testing.T) {
		adapter := &storeAPIAdapter{}
		err := adapter.runCLICommandWithRunner(t.Context(), api.CLIRunRequest{Args: []string{"send-reply", "--draft=chat-draft-example", "--revision=1"}, Grant: &agentgrant.Grant{}}, func(api.CLIRunEvent) error { return nil }, func(context.Context, []string, map[string]string, string, func(string, string) error) error {
			return errors.New("unexpected subprocess")
		})
		require.EqualError(t, err, "not_permitted")
	})
}

func TestXSendPrepareIsOwnerOnlyAndStrict(t *testing.T) {
	f := newReplySurfaceFixture(t)
	for _, extra := range []string{"owner", "source_id", "text", "price", "adoptPost"} {
		body := map[string]any{"draft_id": f.draft.DraftID, "revision": 1, extra: true}
		out := f.request(t, "/api/v1/x/replies/prepare", body)
		assert.Equal(t, 400, out.Code, out.Body.String())
	}
	out := f.request(t, "/api/v1/x/replies/prepare", map[string]any{"draft_id": f.draft.DraftID, "revision": 1})
	require.Equal(t, 200, out.Code, out.Body.String())
	assert.Contains(t, out.Body.String(), f.draft.Body)
	assert.Contains(t, out.Body.String(), `"reserved_micros":200000`)
	assert.Zero(t, f.posts.Load())
}

func TestXSendDirectOutputFailureRetainsDraftBeforePost(t *testing.T) {
	f := newReplySurfaceFixture(t)
	var handle struct {
		DraftID  string `json:"draft_id"`
		Revision int64  `json:"revision"`
	}
	err := f.adapter.runCLICommandWithRunner(t.Context(), api.CLIRunRequest{Args: []string{"send-reply", strconv.FormatInt(f.parent, 10), "--body", "direct reply", "--json"}}, func(e api.CLIRunEvent) error {
		if strings.Contains(e.Data, "draft_id") {
			require.NoError(t, json.Unmarshal([]byte(e.Data), &handle))
			return io.ErrClosedPipe
		}
		return nil
	}, func(context.Context, []string, map[string]string, string, func(string, string) error) error {
		return errors.New("unexpected subprocess")
	})
	require.Error(t, err)
	require.NotEmpty(t, handle.DraftID)
	assert.Equal(t, int64(1), handle.Revision)
	assert.Zero(t, f.posts.Load())
	draft, err := f.st.GetChatDraftContext(t.Context(), handle.DraftID)
	require.NoError(t, err)
	assert.Equal(t, "direct reply", draft.Body)
	args := []string{"send-reply", "--draft", handle.DraftID, "--revision=1", "--json"}
	out := f.request(t, "/api/v1/cli/run", map[string]any{"args": args})
	require.Equal(t, 200, out.Code, out.Body.String())
	assert.Contains(t, out.Body.String(), "9001")
	assert.Equal(t, int32(1), f.posts.Load())
}

// This writer loses the actual CLI response after the provider accepted it.
// Prepared stderr reaches the real client first; the failure is never retried.
type replyOutcomeLossWriter struct {
	http.ResponseWriter
	enabled *atomic.Bool
	failed  bool
}

func (w *replyOutcomeLossWriter) Write(data []byte) (int, error) {
	if w.failed {
		return 0, io.ErrClosedPipe
	}
	if bytes.Contains(data, []byte(`"type":"stdout"`)) && w.enabled.CompareAndSwap(true, false) {
		w.failed = true
		return 0, io.ErrClosedPipe
	}
	return w.ResponseWriter.Write(data)
}
func (w *replyOutcomeLossWriter) Flush() {
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}
func TestXSendDirectActualResponseLossRecoversSameDraft(t *testing.T) {
	f := newReplySurfaceFixture(t)
	var enabled atomic.Bool
	enabled.Store(true)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.handler.ServeHTTP(&replyOutcomeLossWriter{ResponseWriter: w, enabled: &enabled}, r)
	}))
	t.Cleanup(server.Close)
	client, err := daemonclient.New(daemonclient.Config{URL: server.URL, APIKey: f.cfg.Server.AuthenticationKey(), AllowInsecure: true})
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, client.Close()) })
	var prepared xapi.SendResult
	err = client.RunCLICommand(t.Context(), daemonclient.CLIRunRequest{Args: []string{"send-reply", fmt.Sprint(f.parent), "--body=Direct lost response", "--json"}}, func(stream, data string) error {
		if stream == cliStreamStderr {
			return json.Unmarshal([]byte(data), &prepared)
		}
		return nil
	})
	require.Error(t, err)
	require.NotEmpty(t, prepared.DraftID)
	assert.Equal(t, int64(1), prepared.Revision)
	assert.Equal(t, int32(1), f.posts.Load())
	receipt, err := f.st.GetXReplyReceiptContext(t.Context(), prepared.DraftID)
	require.NoError(t, err)
	assert.Equal(t, "sent", receipt.State)
	f.cfg.X.Sends = nil
	require.NoError(t, os.RemoveAll(f.cfg.TokensDir()))
	var final strings.Builder
	require.NoError(t, client.RunCLICommand(t.Context(), daemonclient.CLIRunRequest{Args: []string{"send-reply", "--draft", prepared.DraftID, "--revision=1", "--json"}}, func(stream, data string) error {
		if stream == cliStreamStdout {
			final.WriteString(data)
		}
		return nil
	}))
	assert.Contains(t, final.String(), "9001")
	assert.Equal(t, int32(1), f.posts.Load())
	drafts, err := f.st.ListChatDraftsContext(t.Context(), f.draft.ConversationID, func(string, string) error { return nil })
	require.NoError(t, err)
	assert.Len(t, drafts, 1, "no implicit retry may recreate direct mode")
}
func TestXSendReusedLocalSourceIDDoesNotTransferOptIn(t *testing.T) {
	f := newReplySurfaceFixture(t)
	oldID := f.source.ID
	require.NoError(t, f.st.RemoveSource(oldID))
	other, err := f.st.GetOrCreateSource("x", "2001")
	require.NoError(t, err)
	if !f.st.IsPostgreSQL() {
		assert.Equal(t, oldID, other.ID, "the real SQLite reuse boundary must be exercised")
	}
	now := time.Now().UTC()
	saved, err := f.st.PersistXPostContext(t.Context(), store.XPersistPostInput{SourceID: other.ID, OwnerID: "2001", PostID: "202", AuthorID: "2002", ConversationID: "200", EditHistory: []string{"202"}, Text: "other public parent", CreatedAt: now, FetchedAt: now, Availability: "public", Author: &store.XAuthorInput{UserID: "2002", Username: "other_example", FetchedAt: now}})
	require.NoError(t, err)
	target, err := f.st.ResolveXPostContext(t.Context(), other.ID, "202")
	require.NoError(t, err)
	draft, err := f.st.CreateChatDraftContext(t.Context(), target.ConversationID, saved.MessageID, "different owner", func(string, string) error { return nil })
	require.NoError(t, err)
	out := f.request(t, "/api/v1/cli/run", map[string]any{"args": []string{"send-reply", "--draft", draft.DraftID, "--revision=1", "--json"}})
	assert.Contains(t, out.Body.String(), "send_disabled")
	assert.Zero(t, f.posts.Load())
	_, err = f.st.GetXReplyReceiptContext(t.Context(), draft.DraftID)
	assert.ErrorIs(t, err, store.ErrXReplyReceiptNotFound)
}
func TestXSendHTTPRejectsDelegationAndUnsafeTransport(t *testing.T) {
	f := newReplySurfaceFixture(t)
	for _, req := range []api.CLIRunRequest{{Args: f.args(), Cwd: "/tmp"}, {Args: f.args(), Env: map[string]string{"X_TOKEN": "synthetic-secret"}}, {Args: append(f.args(), "--token=synthetic-secret")}, {Args: append(f.args(), "--source-id=1")}, {Args: append(f.args(), "extra")}} {
		out := f.request(t, "/api/v1/cli/run", req)
		assert.Equal(t, 400, out.Code)
		assert.NotContains(t, out.Body.String(), "synthetic-secret")
	}
	issued := f.request(t, "/api/v1/agent-tokens", map[string]any{"label": "synthetic-reply-agent", "permissions": []string{"draft.create", "draft.edit", "draft.delete"}, "source_ids": []int64{f.source.ID}})
	require.Equal(t, 201, issued.Code, issued.Body.String())
	var grant struct {
		Secret string `json:"secret"`
	}
	require.NoError(t, json.Unmarshal(issued.Body.Bytes(), &grant))
	require.NotEmpty(t, grant.Secret)
	for _, path := range []string{"/api/v1/cli/run", "/api/v1/x/replies/prepare"} {
		var body any = map[string]any{"args": f.args()}
		if strings.HasSuffix(path, "prepare") {
			body = map[string]any{"draft_id": f.draft.DraftID, "revision": 1}
		}
		encoded, err := json.Marshal(body)
		require.NoError(t, err)
		req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(encoded))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(apiprotocol.AgentTokenHeader, grant.Secret)
		out := httptest.NewRecorder()
		f.handler.ServeHTTP(out, req)
		assert.GreaterOrEqual(t, out.Code, 400, out.Body.String())
	}
	assert.Zero(t, f.posts.Load())
	_, err := f.st.GetXReplyReceiptContext(t.Context(), f.draft.DraftID)
	assert.ErrorIs(t, err, store.ErrXReplyReceiptNotFound)
}

func TestSendReplyCobraUsesSelectedDaemon(t *testing.T) {
	for _, mode := range []string{"draft", "direct", "old daemon"} {
		t.Run(mode, func(t *testing.T) {
			f := newReplySurfaceFixture(t)
			mux := http.NewServeMux()
			mux.Handle("/api/ping", daemon.NewPingHandler(daemon.PingHandlerOptions{Service: daemonService, Version: Version}))
			mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if mode == "old daemon" && r.URL.Path == "/api/v1/health" {
					w.Header().Set("Content-Type", "application/json")
					fmt.Fprint(w, `{"status":"ok","api_schema_version":"3.28.0"}`)
					return
				}
				f.handler.ServeHTTP(w, r)
			}))
			server := httptest.NewServer(mux)
			t.Cleanup(server.Close)
			cfg := config.NewDefaultConfig()
			cfg.HomeDir = t.TempDir()
			cfg.Data.DataDir = cfg.HomeDir
			cfg.Remote.URL = server.URL
			cfg.Remote.AllowInsecure = true
			cfg.Remote.APIKey = f.cfg.Server.AuthenticationKey()
			root := &cobra.Command{Use: "msgvault"}
			root.AddCommand(newSendReplyCommand())
			root.SetContext(testInvocationContext(t.Context(), cfg, invocationOptions{}))
			silenceUsageInRunE(root)
			args := []string{"send-reply", "--draft", f.draft.DraftID, "--revision=1", "--json"}
			text := f.draft.Body
			if mode == "direct" {
				text = "Thanks " + strings.Repeat("🙂", 130) + " https://example.com"
				args = []string{"send-reply", fmt.Sprint(f.parent), "--body", text, "--json"}
			}
			var out, stderr bytes.Buffer
			root.SetOut(&out)
			root.SetErr(&stderr)
			root.SetArgs(args)
			err := root.Execute()
			if mode == "old daemon" {
				require.ErrorContains(t, err, "x_reply_send")
				assert.Zero(t, f.posts.Load())
			} else {
				require.NoError(t, err, stderr.String())
				var result xapi.SendResult
				require.NoError(t, json.Unmarshal(out.Bytes(), &result))
				assert.Equal(t, "sent", result.Status)
				assert.Equal(t, "9001", result.RemoteID)
				assert.Equal(t, int64(200000), result.ReservedMicros)
				assert.Equal(t, int32(1), f.posts.Load())
				assert.Equal(t, text, f.postedText.Load())
				assert.Equal(t, "102", f.postedTarget.Load())
				if mode == "direct" {
					var prepared xapi.SendResult
					require.NoError(t, json.Unmarshal(stderr.Bytes(), &prepared))
					assert.Equal(t, result.DraftID, prepared.DraftID)
					assert.Equal(t, result.Revision, prepared.Revision)
				}
			}
			path, err := cfg.DatabasePath()
			require.NoError(t, err)
			assert.NoFileExists(t, path, "the CLI must not open a secondary archive")
		})
	}
}
