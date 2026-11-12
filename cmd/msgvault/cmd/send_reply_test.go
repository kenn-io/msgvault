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
	"path/filepath"
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
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		f.postedText.Store(submitted.Text)
		f.postedTarget.Store(submitted.Reply.ID)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = fmt.Fprint(w, `{"data":{"id":"9001"}}`)
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
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, path, bytes.NewReader(encoded))
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
		_testAssert := assert.New(t)
		_testRequire := require.New(t)

		f := newReplySurfaceFixture(t)
		out := f.request(t, "/api/v1/cli/run", map[string]any{"args": f.args()})
		_testRequire.Equal(200, out.Code, out.Body.String())
		_testAssert.Contains(out.Body.String(), "9001")
		_testAssert.Contains(out.Body.String(), "sent")
		_testAssert.Equal(int32(1), f.posts.Load())
		f.cfg.X.Sends = nil
		_testRequire.NoError(os.RemoveAll(f.cfg.TokensDir()))
		retry := f.request(t, "/api/v1/cli/run", map[string]any{"args": f.args()})
		_testRequire.Equal(200, retry.Code, retry.Body.String())
		_testAssert.Contains(retry.Body.String(), "9001")
		_testAssert.Equal(int32(1), f.posts.Load())
		prep := f.request(t, "/api/v1/x/replies/prepare", map[string]any{"draft_id": f.draft.DraftID, "revision": 1})
		_testRequire.Equal(200, prep.Code, prep.Body.String())
		_testAssert.Contains(prep.Body.String(), `"mode":"receipt"`)
		_testAssert.Contains(prep.Body.String(), "9001")
	})
	for _, which := range []string{"disabled", "wrong owner", "scope"} {
		t.Run(which, func(t *testing.T) {
			_testAssert := assert.New(t)
			_testRequire := require.New(t)

			f := newReplySurfaceFixture(t)
			switch which {
			case "disabled":
				f.cfg.X.Sends = nil
			case "wrong owner":
				f.cfg.X.Sends = []config.XSendConfig{{UserID: "2001", Enabled: true}}
			case "scope":
				manager := xapi.NewTokenManager(f.cfg.TokensDir())
				r, err := manager.Load(t.Context(), "1001")
				_testRequire.NoError(err)
				r.GrantedScopes = []string{"tweet.read", "users.read"}
				_testRequire.NoError(manager.Save(t.Context(), r))
			}
			out := f.request(t, "/api/v1/cli/run", map[string]any{"args": f.args()})
			_testAssert.Contains(out.Body.String(), "error")
			_testAssert.Zero(f.posts.Load())
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
	_testAssert := assert.New(t)

	f := newReplySurfaceFixture(t)
	for _, extra := range []string{"owner", "source_id", "text", "price", "adoptPost"} {
		body := map[string]any{"draft_id": f.draft.DraftID, "revision": 1, extra: true}
		out := f.request(t, "/api/v1/x/replies/prepare", body)
		_testAssert.Equal(400, out.Code, out.Body.String())
	}
	out := f.request(t, "/api/v1/x/replies/prepare", map[string]any{"draft_id": f.draft.DraftID, "revision": 1})
	require.Equal(t, 200, out.Code, out.Body.String())
	_testAssert.Contains(out.Body.String(), f.draft.Body)
	_testAssert.Contains(out.Body.String(), `"reserved_micros":200000`)
	_testAssert.Zero(f.posts.Load())
}

func TestXSendDirectOutputFailureRetainsDraftBeforePost(t *testing.T) {
	_testAssert := assert.New(t)
	_testRequire := require.New(t)

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
	_testRequire.Error(err)
	_testRequire.NotEmpty(handle.DraftID)
	_testAssert.Equal(int64(1), handle.Revision)
	_testAssert.Zero(f.posts.Load())
	draft, err := f.st.GetChatDraftContext(t.Context(), handle.DraftID)
	_testRequire.NoError(err)
	_testAssert.Equal("direct reply", draft.Body)
	args := []string{"send-reply", "--draft", handle.DraftID, "--revision=1", "--json"}
	out := f.request(t, "/api/v1/cli/run", map[string]any{"args": args})
	_testRequire.Equal(200, out.Code, out.Body.String())
	_testAssert.Contains(out.Body.String(), "9001")
	_testAssert.Equal(int32(1), f.posts.Load())
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

func TestXSubsetReplyRequiresOwnerOptInAndCanSendBeforeSync(t *testing.T) {
	_testAssert := assert.New(t)
	_testRequire := require.New(t)

	ctx := t.Context()
	sourcePath := filepath.Join(t.TempDir(), "source.db")
	source := testutil.NewSQLiteTestStoreAt(t, sourcePath)
	src, err := source.GetOrCreateSource("x", "1001")
	_testRequire.NoError(err)

	cfg := config.NewDefaultConfig()
	cfg.HomeDir = t.TempDir()
	cfg.Server.APIKey = "example-owner-key"
	record := xapi.NewTokenRecord("1001", "example-client", xapi.PublicClient, "example-access", "example-refresh", "")
	record.GrantedScopes = []string{"tweet.read", "users.read", "tweet.write"}
	record.Expiry = time.Now().Add(time.Hour)

	postAt := time.Now().UTC().Add(-time.Hour)
	saved, err := source.PersistXPostContext(ctx, store.XPersistPostInput{
		SourceID: src.ID, OwnerID: "1001", PostID: "102", AuthorID: "1002",
		ConversationID: "100", EditHistory: []string{"100", "102"},
		Text: "synthetic selected parent", CreatedAt: postAt,
		FetchedAt: postAt.Add(time.Minute), Availability: "public",
		Raw: []byte(`{"post":{"id":"102"}}`),
		Author: &store.XAuthorInput{
			SourceID: src.ID, UserID: "1002", Username: "example_author",
			DisplayName: "Example Author", FetchedAt: postAt.Add(time.Minute),
		},
	})
	_testRequire.NoError(err)
	generation := record.Generation
	_, err = source.UpdateXControlContext(ctx, src.ID, 0, store.XControlPatch{
		OwnerID: "1001", Generation: &generation,
	})
	_testRequire.NoError(err)
	_testRequire.NoError(source.Close())

	destinationDir := filepath.Join(t.TempDir(), "subset")
	_, err = store.CopySubset(t.Context(), sourcePath, destinationDir, 1, false)
	_testRequire.NoError(err)
	subset, err := store.Open(filepath.Join(destinationDir, "msgvault.db"))
	_testRequire.NoError(err)
	t.Cleanup(func() { _testRequire.NoError(subset.Close()) })
	cfg.Data.DataDir = destinationDir
	tokens := xapi.NewTokenManager(cfg.TokensDir())
	_testRequire.NoError(tokens.Save(ctx, record))

	state, err := subset.LoadXControlContext(ctx, src.ID)
	_testRequire.NoError(err)
	_testAssert.Equal("1001", state.OwnerID)
	_testAssert.Equal(generation, state.Generation)
	_testAssert.Nil(state.Own, "the copied source has not completed a fresh X sync")
	target, err := subset.ResolveXPostContext(ctx, src.ID, "100")
	_testRequire.NoError(err)
	_testAssert.Equal(saved.MessageID, target.MessageID)
	_testAssert.Equal("102", target.CurrentID)
	_testAssert.Equal("public", target.Availability)

	var calls atomic.Int32
	var postedTarget atomic.Value
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_testAssert.Equal(http.MethodPost, r.Method)
		_testAssert.Equal("/2/tweets", r.URL.Path)
		_testAssert.Equal("Bearer example-access", r.Header.Get("Authorization"))
		var submitted struct {
			Reply struct {
				ID string `json:"in_reply_to_tweet_id"`
			} `json:"reply"`
		}
		if err := json.UnmarshalRead(r.Body, &submitted); err != nil {
			_testAssert.NoError(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		postedTarget.Store(submitted.Reply.ID)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = fmt.Fprint(w, `{"data":{"id":"9001"}}`)
	}))
	t.Cleanup(provider.Close)
	deps := xCommandDeps{apiBaseURL: provider.URL + "/2", httpClient: provider.Client()}
	adapter := &storeAPIAdapter{store: subset, config: cfg, xDeps: &deps, logger: slog.New(slog.DiscardHandler)}
	run := func(args []string) error {
		return adapter.runCLICommandWithRunner(ctx, api.CLIRunRequest{Args: args}, func(api.CLIRunEvent) error { return nil }, func(context.Context, []string, map[string]string, string, func(string, string) error) error {
			return errors.New("unexpected subprocess")
		})
	}

	err = run([]string{"send-reply", strconv.FormatInt(saved.MessageID, 10), "--body", "A synthetic reply", "--json"})
	_testRequire.ErrorContains(err, "send_disabled")
	_testAssert.Zero(calls.Load(), "a copied owner identity does not bypass x.sends")
	drafts, err := subset.ListChatDraftsContext(ctx, target.ConversationID, func(string, string) error { return nil })
	_testRequire.NoError(err)
	_testRequire.Len(drafts, 1, "direct reply retains its managed draft when opt-in is disabled")

	cfg.X.Sends = []config.XSendConfig{{UserID: "1001", Enabled: true}}
	prepared, err := adapter.PrepareReply(ctx, api.ReplyPrepareRequest{
		DraftID: drafts[0].DraftID, Revision: drafts[0].Revision,
	}, nil)
	_testRequire.NoError(err)
	_testAssert.Equal("new_send", prepared.Mode)
	_testAssert.Equal("102", prepared.TargetPostID)
	err = run([]string{"send-reply", "--draft", drafts[0].DraftID, "--revision", strconv.FormatInt(drafts[0].Revision, 10), "--json"})
	_testRequire.NoError(err)
	_testAssert.Equal(int32(1), calls.Load(), "the local provider receives only the synthetic tweet write, with no sync or read request")
	_testAssert.Equal("102", postedTarget.Load())
	after, err := subset.LoadXControlContext(ctx, src.ID)
	_testRequire.NoError(err)
	_testAssert.Nil(after.Own, "the reply succeeded without running sync-x")
}

func TestXSendDirectActualResponseLossRecoversSameDraft(t *testing.T) {
	_testAssert := assert.New(t)
	_testRequire := require.New(t)

	f := newReplySurfaceFixture(t)
	var enabled atomic.Bool
	enabled.Store(true)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.handler.ServeHTTP(&replyOutcomeLossWriter{ResponseWriter: w, enabled: &enabled}, r)
	}))
	t.Cleanup(server.Close)
	client, err := daemonclient.New(daemonclient.Config{URL: server.URL, APIKey: f.cfg.Server.AuthenticationKey(), AllowInsecure: true})
	_testRequire.NoError(err)
	t.Cleanup(func() { assert.NoError(t, client.Close()) })
	var prepared xapi.SendResult
	err = client.RunCLICommand(t.Context(), daemonclient.CLIRunRequest{Args: []string{"send-reply", strconv.FormatInt(f.parent, 10), "--body=Direct lost response", "--json"}}, func(stream, data string) error {
		if stream == cliStreamStderr {
			return json.Unmarshal([]byte(data), &prepared)
		}
		return nil
	})
	_testRequire.Error(err)
	_testRequire.NotEmpty(prepared.DraftID)
	_testAssert.Equal(int64(1), prepared.Revision)
	_testAssert.Equal(int32(1), f.posts.Load())
	receipt, err := f.st.GetXReplyReceiptContext(t.Context(), prepared.DraftID)
	_testRequire.NoError(err)
	_testAssert.Equal("sent", receipt.State)
	f.cfg.X.Sends = nil
	_testRequire.NoError(os.RemoveAll(f.cfg.TokensDir()))
	var final strings.Builder
	_testRequire.NoError(client.RunCLICommand(t.Context(), daemonclient.CLIRunRequest{Args: []string{"send-reply", "--draft", prepared.DraftID, "--revision=1", "--json"}}, func(stream, data string) error {
		if stream == cliStreamStdout {
			final.WriteString(data)
		}
		return nil
	}))
	_testAssert.Contains(final.String(), "9001")
	_testAssert.Equal(int32(1), f.posts.Load())
	drafts, err := f.st.ListChatDraftsContext(t.Context(), f.draft.ConversationID, func(string, string) error { return nil })
	_testRequire.NoError(err)
	_testAssert.Len(drafts, 1, "no implicit retry may recreate direct mode")
}
func TestXSendReusedLocalSourceIDDoesNotTransferOptIn(t *testing.T) {
	_testAssert := assert.New(t)
	_testRequire := require.New(t)

	f := newReplySurfaceFixture(t)
	oldID := f.source.ID
	_testRequire.NoError(f.st.RemoveSource(oldID))
	other, err := f.st.GetOrCreateSource("x", "2001")
	_testRequire.NoError(err)
	if !f.st.IsPostgreSQL() {
		_testAssert.Equal(oldID, other.ID, "the real SQLite reuse boundary must be exercised")
	}
	now := time.Now().UTC()
	saved, err := f.st.PersistXPostContext(t.Context(), store.XPersistPostInput{SourceID: other.ID, OwnerID: "2001", PostID: "202", AuthorID: "2002", ConversationID: "200", EditHistory: []string{"202"}, Text: "other public parent", CreatedAt: now, FetchedAt: now, Availability: "public", Author: &store.XAuthorInput{UserID: "2002", Username: "other_example", FetchedAt: now}})
	_testRequire.NoError(err)
	target, err := f.st.ResolveXPostContext(t.Context(), other.ID, "202")
	_testRequire.NoError(err)
	draft, err := f.st.CreateChatDraftContext(t.Context(), target.ConversationID, saved.MessageID, "different owner", func(string, string) error { return nil })
	_testRequire.NoError(err)
	out := f.request(t, "/api/v1/cli/run", map[string]any{"args": []string{"send-reply", "--draft", draft.DraftID, "--revision=1", "--json"}})
	_testAssert.Contains(out.Body.String(), "send_disabled")
	_testAssert.Zero(f.posts.Load())
	_, err = f.st.GetXReplyReceiptContext(t.Context(), draft.DraftID)
	_testAssert.ErrorIs(err, store.ErrXReplyReceiptNotFound)
}
func TestXSendHTTPRejectsDelegationAndUnsafeTransport(t *testing.T) {
	_testAssert := assert.New(t)
	_testRequire := require.New(t)

	f := newReplySurfaceFixture(t)
	for _, req := range []api.CLIRunRequest{{Args: f.args(), Cwd: "/tmp"}, {Args: f.args(), Env: map[string]string{"X_TOKEN": "synthetic-secret"}}, {Args: append(f.args(), "--token=synthetic-secret")}, {Args: append(f.args(), "--source-id=1")}, {Args: append(f.args(), "extra")}} {
		out := f.request(t, "/api/v1/cli/run", req)
		_testAssert.Equal(400, out.Code)
		_testAssert.NotContains(out.Body.String(), "synthetic-secret")
	}
	issued := f.request(t, "/api/v1/agent-tokens", map[string]any{"label": "synthetic-reply-agent", "permissions": []string{"draft.create", "draft.edit", "draft.delete"}, "source_ids": []int64{f.source.ID}})
	_testRequire.Equal(201, issued.Code, issued.Body.String())
	var grant struct {
		Secret string `json:"secret"`
	}
	_testRequire.NoError(json.Unmarshal(issued.Body.Bytes(), &grant))
	_testRequire.NotEmpty(grant.Secret)
	for _, path := range []string{"/api/v1/cli/run", "/api/v1/x/replies/prepare"} {
		var body any = map[string]any{"args": f.args()}
		if strings.HasSuffix(path, "prepare") {
			body = map[string]any{"draft_id": f.draft.DraftID, "revision": 1}
		}
		encoded, err := json.Marshal(body)
		_testRequire.NoError(err)
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, path, bytes.NewReader(encoded))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(apiprotocol.AgentTokenHeader, grant.Secret)
		out := httptest.NewRecorder()
		f.handler.ServeHTTP(out, req)
		_testAssert.GreaterOrEqual(out.Code, 400, out.Body.String())
	}
	_testAssert.Zero(f.posts.Load())
	_, err := f.st.GetXReplyReceiptContext(t.Context(), f.draft.DraftID)
	_testAssert.ErrorIs(err, store.ErrXReplyReceiptNotFound)
}

func TestSendReplyCobraUsesSelectedDaemon(t *testing.T) {
	for _, mode := range []string{"draft", "direct", "old daemon"} {
		t.Run(mode, func(t *testing.T) {
			_testAssert := assert.New(t)
			_testRequire := require.New(t)

			f := newReplySurfaceFixture(t)
			mux := http.NewServeMux()
			mux.Handle("/api/ping", daemon.NewPingHandler(daemon.PingHandlerOptions{Service: daemonService, Version: Version}))
			mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if mode == "old daemon" && r.URL.Path == "/api/v1/health" {
					w.Header().Set("Content-Type", "application/json")
					_, _ = fmt.Fprint(w, `{"status":"ok","api_schema_version":"3.28.0"}`)
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
				args = []string{"send-reply", strconv.FormatInt(f.parent, 10), "--body", text, "--json"}
			}
			var out, stderr bytes.Buffer
			root.SetOut(&out)
			root.SetErr(&stderr)
			root.SetArgs(args)
			err := root.Execute()
			if mode == "old daemon" {
				_testRequire.ErrorContains(err, "x_reply_send")
				_testAssert.Zero(f.posts.Load())
			} else {
				_testRequire.NoError(err, stderr.String())
				var result xapi.SendResult
				_testRequire.NoError(json.Unmarshal(out.Bytes(), &result))
				_testAssert.Equal("sent", result.Status)
				_testAssert.Equal("9001", result.RemoteID)
				_testAssert.Equal(int64(200000), result.ReservedMicros)
				_testAssert.Equal(int32(1), f.posts.Load())
				_testAssert.Equal(text, f.postedText.Load())
				_testAssert.Equal("102", f.postedTarget.Load())
				if mode == "direct" {
					var prepared xapi.SendResult
					_testRequire.NoError(json.Unmarshal(stderr.Bytes(), &prepared))
					_testAssert.Equal(result.DraftID, prepared.DraftID)
					_testAssert.Equal(result.Revision, prepared.Revision)
				}
			}
			path, err := cfg.DatabasePath()
			_testRequire.NoError(err)
			_testAssert.NoFileExists(path, "the CLI must not open a secondary archive")
		})
	}
}
