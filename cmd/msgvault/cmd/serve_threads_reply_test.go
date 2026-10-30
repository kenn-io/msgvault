package cmd

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/agentgrant"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/threads"
)

type threadsSendDaemonFixture struct {
	adapter                           *storeAPIAdapter
	provider                          *threadsConnectFixture
	deps                              threadsCommandDeps
	cfg                               *config.Config
	source                            *store.Source
	draft                             store.ChatDraft
	root                              int64
	opens, creates, publishes, quotas atomic.Int32
	mode                              string
	status                            string
	beforePublish                     func()
}

func newThreadsSendDaemonFixture(t *testing.T, disk ...bool) *threadsSendDaemonFixture {
	t.Helper()
	dir := t.TempDir()
	var st *store.Store
	if len(disk) > 0 && disk[0] {
		st = testutil.NewSQLiteTestStoreAt(t, filepath.Join(dir, "msgvault.db"))
	} else {
		st = testutil.NewTestStore(t)
	}
	provider := newThreadsConnectFixture(t, "20")
	provider.scopes = append(provider.scopes, "threads_content_publish", "threads_manage_replies")
	cfg := &config.Config{HomeDir: dir, Data: config.DataConfig{DataDir: dir}, Server: config.ServerConfig{APIKey: "synthetic-owner-key", AgentAccess: true}}
	deps := provider.deps(st, cfg.TokensDir())
	_, err := executeAddThreadsFixture(t, deps, "123", true)
	require.NoError(t, err)
	source, err := st.GetSourceByTypeAndIdentifier("threads", "threads:123:20")
	require.NoError(t, err)
	cfg.Threads.Sends = []config.ThreadsSendConfig{{SourceID: source.ID}}
	root, err := threads.NewWriter(st, source.ID, source.Identifier).Apply(t.Context(), threads.Media{ID: "100", Timestamp: provider.clock().Add(-time.Hour), Text: new("Owned synthetic post")}, threads.ItemOrigin{Kind: "own_posts"})
	require.NoError(t, err)
	target, err := st.GetMessageRelationTarget(root)
	require.NoError(t, err)
	draft, err := st.CreateChatDraftContext(t.Context(), target.ConversationID, root, "reviewed daemon reply", func(string, string) error { return nil })
	require.NoError(t, err)
	f := &threadsSendDaemonFixture{provider: provider, deps: deps, cfg: cfg, source: source, draft: draft, root: root, status: "FINISHED"}
	f.deps.newAPI = func(record threads.TokenRecord, budget *threads.RequestBudget) (threads.API, error) {
		f.opens.Add(1)
		return threads.NewClient(threads.ClientOptions{BaseURL: provider.server.URL + "/v1.0", HTTPClient: provider.server.Client(), AccessToken: record.AccessToken(), Budget: budget})
	}
	provider.readHandler = func(w http.ResponseWriter, r *http.Request) bool {
		switch r.URL.Path {
		case "/v1.0/me/threads_publishing_limit":
			f.quotas.Add(1)
			_, _ = fmt.Fprint(w, `{"data":[{"reply_quota_usage":0,"reply_config":{"quota_total":1000,"quota_duration":86400}}]}`)
		case "/v1.0/me/threads":
			f.creates.Add(1)
			require.NoError(t, r.ParseForm())
			assert.Equal(t, f.draft.Body, r.PostForm.Get("text"))
			assert.Equal(t, f.draft.ReplyToSourceMessageID, r.PostForm.Get("reply_to_id"))
			_, _ = fmt.Fprint(w, `{"id":"400"}`)
		case "/v1.0/400":
			_, _ = fmt.Fprintf(w, `{"id":"400","status":%q}`, f.status)
		case "/v1.0/20/threads_publish":
			f.publishes.Add(1)
			if f.beforePublish != nil {
				f.beforePublish()
			}
			if f.mode == "drop" {
				conn, _, err := http.NewResponseController(w).Hijack()
				require.NoError(t, err)
				require.NoError(t, conn.Close())
			} else {
				_, _ = fmt.Fprint(w, `{"id":"500"}`)
			}
		case "/v1.0/500":
			_, _ = fmt.Fprintf(w, `{"id":"500","timestamp":%q,"text":%q,"root_post":{"id":"100"},"replied_to":{"id":"100"},"is_reply":true,"is_reply_owned_by_me":true}`, provider.now.Format(time.RFC3339Nano), f.draft.Body)
		default:
			return false
		}
		return true
	}
	f.adapter = &storeAPIAdapter{store: st, config: cfg, threadsReplyDeps: &f.deps}
	return f
}
func (f *threadsSendDaemonFixture) args() []string {
	return []string{"send-reply", "--draft=" + f.draft.DraftID, "--revision=1", "--json"}
}
func (f *threadsSendDaemonFixture) run(ctx context.Context, args []string) (threads.SendResult, string, error) {
	var out strings.Builder
	err := f.adapter.runCLISendReply(ctx, api.CLIRunRequest{Args: args}, func(event api.CLIRunEvent) error {
		if event.Type == "stdout" {
			out.WriteString(event.Data)
		}
		return nil
	})
	var result threads.SendResult
	if out.Len() > 0 {
		err = errors.Join(err, json.Unmarshal([]byte(out.String()), &result))
	}
	return result, out.String(), err
}

func TestSendReplyUnsupportedSourceExplainsType(t *testing.T) {
	require := require.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("slack", "TEXAMPLE:UEXAMPLE")
	require.NoError(err)
	conversationID, err := st.EnsureConversationWithType(source.ID, "CEXAMPLE", "channel", "Synthetic")
	require.NoError(err)
	messageID, err := st.UpsertMessage(&store.Message{
		SourceID: source.ID, ConversationID: conversationID,
		SourceMessageID: "message-example", MessageType: "chat",
	})
	require.NoError(err)
	adapter := &storeAPIAdapter{store: st}
	err = adapter.runCLISendReply(t.Context(), api.CLIRunRequest{
		Args: []string{"send-reply", fmt.Sprint(messageID), "--body=Thanks!"},
	}, nil)
	require.Error(err)
	assert.ErrorContains(t, err, "replies are not supported for slack sources")
}

func TestThreadsSendAdmission(t *testing.T) {
	for _, kind := range []string{"optout", "wrong-source", "revision", "parent", "scopes", "delegated"} {
		t.Run(kind, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			f := newThreadsSendDaemonFixture(t)
			args := f.args()
			switch kind {
			case "optout":
				f.cfg.Threads.Sends = nil
			case "wrong-source":
				f.cfg.Threads.Sends = []config.ThreadsSendConfig{{SourceID: f.source.ID + 100}}
			case "revision":
				args = []string{"send-reply", "--draft=" + f.draft.DraftID, "--revision=2", "--json"}
			case "parent":
				require.NoError(f.adapter.store.MarkMessageDeletedContext(t.Context(), f.source.ID, "100"))
			case "scopes":
				manager := f.deps.tokenManager()
				record, err := manager.Load("123", "20")
				require.NoError(err)
				record.Scopes = []string{"threads_basic", "threads_read_replies"}
				require.NoError(manager.WithSourceLock(t.Context(), f.source.Identifier, func() error { return manager.Save(record) }))
				f.provider.mu.Lock()
				f.provider.now = f.provider.now.Add(8 * 24 * time.Hour)
				f.provider.mu.Unlock()
			case "delegated":
				f.adapter.store = nil
				err := f.adapter.runCLISendReply(t.Context(), api.CLIRunRequest{Args: args, Grant: &agentgrant.Grant{Permissions: []agentgrant.Permission{agentgrant.PermissionDraftCreate}}}, nil)
				require.Error(err)
				assert.Equal("not_permitted", err.Error())
				assert.Zero(f.opens.Load())
				return
			}
			f.provider.mu.Lock()
			before := f.provider.attempts
			f.provider.mu.Unlock()
			_, out, err := f.run(t.Context(), args)
			require.Error(err)
			assert.NotContains(out, f.draft.Body)
			assert.Zero(f.opens.Load())
			assert.Zero(f.creates.Load())
			assert.Zero(f.publishes.Load())
			f.provider.mu.Lock()
			assert.Equal(before, f.provider.attempts)
			f.provider.mu.Unlock()
		})
	}
}
func TestThreadsSendDaemonHTTPAndCache(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newThreadsSendDaemonFixture(t)
	cacheRuns := 0
	f.adapter.draftCacheRefresh = func(ctx context.Context, _ string) error {
		cacheRuns++
		return f.deps.tokenManager().WithSourceLock(ctx, f.source.Identifier, func() error {
			execution, err := f.adapter.store.AcquireSyncExecutionContext(ctx, f.source.ID)
			if err != nil {
				return err
			}
			return execution.Release()
		})
	}
	server := httptest.NewServer(api.NewServerWithOptions(api.ServerOptions{Config: f.cfg, Store: f.adapter, Logger: slog.New(slog.DiscardHandler), OperationGate: api.NewSerialOperationGate()}).Router())
	defer server.Close()
	client, err := daemonclient.New(daemonclient.Config{URL: server.URL, APIKey: f.cfg.Server.APIKey, AllowInsecure: true})
	require.NoError(err)
	var out strings.Builder
	err = client.RunCLICommand(t.Context(), daemonclient.CLIRunRequest{Args: f.args()}, func(stream, data string) error {
		if stream == "stdout" {
			out.WriteString(data)
		}
		return nil
	})
	require.NoError(err)
	var result threads.SendResult
	require.NoError(json.Unmarshal([]byte(out.String()), &result))
	assert.True(result.ArchiveComplete)
	assert.Equal("sent", result.RemoteStatus)
	assert.Equal("complete", result.LocalStatus)
	assert.Equal(1, cacheRuns)
	assert.Equal(int32(1), f.publishes.Load())
	// Replaying a consumed draft remains local after publication opt-out and
	// expired credentials; it still retries derived-cache publication.
	f.cfg.Threads.Sends = nil
	require.NoError(f.deps.tokenManager().Delete("123", "20"))
	out.Reset()
	err = client.RunCLICommand(t.Context(), daemonclient.CLIRunRequest{Args: f.args()}, func(stream, data string) error {
		if stream == "stdout" {
			out.WriteString(data)
		}
		return nil
	})
	require.NoError(err)
	require.NoError(json.Unmarshal([]byte(out.String()), &result))
	assert.True(result.ArchiveComplete)
	assert.Equal(2, cacheRuns)
	assert.Equal(int32(1), f.publishes.Load())
}

func TestThreadsSendCacheFailurePreservesRemoteSuccess(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newThreadsSendDaemonFixture(t)
	f.adapter.draftCacheRefresh = func(context.Context, string) error { return errors.New("synthetic cache failure") }
	result, _, err := f.run(t.Context(), f.args())
	require.Error(err)
	assert.Equal("sent", result.RemoteStatus)
	assert.Equal("cache_pending", result.LocalStatus)
	assert.Equal("500", result.MediaID)
	assert.True(result.ArchiveComplete)
	f.adapter.draftCacheRefresh = func(context.Context, string) error { return nil }
	require.NoError(f.deps.tokenManager().Delete("123", "20"))
	result, _, err = f.run(t.Context(), f.args())
	require.NoError(err)
	assert.Equal("complete", result.LocalStatus)
	assert.Equal(int32(1), f.publishes.Load())
}
func TestThreadsSendStructuredIncompleteOutcomes(t *testing.T) {
	for _, mode := range []string{"drop", "processing", "expired", "quota"} {
		t.Run(mode, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			f := newThreadsSendDaemonFixture(t)
			switch mode {
			case "drop":
				f.mode = "drop"
			case "processing":
				f.status = "IN_PROGRESS"
			case "expired":
				f.status = "EXPIRED"
			case "quota":
				original := f.provider.readHandler
				f.provider.readHandler = func(w http.ResponseWriter, r *http.Request) bool {
					if r.URL.Path == "/v1.0/me/threads_publishing_limit" {
						_, _ = fmt.Fprint(w, `{"data":[{"reply_quota_usage":1000,"reply_config":{"quota_total":1000,"quota_duration":86400}}]}`)
						return true
					}
					return original(w, r)
				}
			}
			result, _, err := f.run(t.Context(), f.args())
			require.Error(err)
			assert.NotEmpty(result.RemoteStatus)
			assert.NotEmpty(result.LocalStatus)
			switch mode {
			case "drop":
				assert.Equal("remote_unknown", result.Status)
				assert.Equal("unknown", result.RemoteStatus)
				before := f.opens.Load()
				again, _, retryErr := f.run(t.Context(), f.args())
				require.Error(retryErr)
				assert.Equal("remote_unknown", again.Status)
				assert.Equal(before, f.opens.Load())
				assert.Equal(int32(1), f.publishes.Load())
			case "processing":
				assert.Equal("container_saved", result.Status)
				assert.Zero(f.publishes.Load())
			case "expired":
				assert.Equal("send_failed", result.Status)
				assert.Zero(f.publishes.Load())
			case "quota":
				assert.Equal("rate_limited", result.Status)
				assert.Zero(f.creates.Load())
				assert.Zero(f.publishes.Load())
			}
		})
	}
}
func TestThreadsRemovalReauthSendRace(t *testing.T) {
	t.Run("queued send observes removal", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		f := newThreadsSendDaemonFixture(t)
		manager := f.deps.tokenManager()
		entered := make(chan struct{})
		var once atomic.Bool
		f.deps.tokenManager = func() *threads.TokenManager {
			if once.CompareAndSwap(false, true) {
				close(entered)
			}
			return manager
		}
		done := make(chan error, 1)
		require.NoError(manager.WithSourceLock(t.Context(), f.source.Identifier, func() error {
			go func() { _, _, err := f.run(t.Context(), f.args()); done <- err }()
			select {
			case <-entered:
			case <-t.Context().Done():
				return t.Context().Err()
			}
			_, _, err := f.adapter.store.RemoveSourceSerialized(t.Context(), f.source.ID)
			if err != nil {
				return err
			}
			return manager.Delete("123", "20")
		}))
		require.Error(<-done)
		assert.Zero(f.opens.Load())
		assert.Zero(f.publishes.Load())
	})
	t.Run("removal during derived cache publication", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		f := newThreadsSendDaemonFixture(t, true)
		f.adapter.draftCacheRefresh = func(ctx context.Context, _ string) error {
			// This is the real removal command, including lifecycle and analytics locks.
			// Waiting for it here proves the send has released both source leases.
			root := newTestRootCmd()
			root.SetContext(testInvocationContext(ctx, f.cfg, invocationOptions{}))
			root.AddCommand(newRemoveAccountLocalTestCmd())
			root.SetArgs([]string{"remove-account", "--source-id", fmt.Sprint(f.source.ID), "--yes"})
			return root.Execute()
		}
		result, _, err := f.run(t.Context(), f.args())
		require.NoError(err)
		assert.Equal("sent", result.RemoteStatus)
		_, err = f.adapter.store.GetSourceByID(f.source.ID)
		require.ErrorIs(err, store.ErrSourceNotFound)
		_, err = f.adapter.store.GetThreadsReplySendContext(t.Context(), f.draft.DraftID)
		require.ErrorIs(err, store.ErrThreadsReplyNotFound)
		_, err = f.deps.tokenManager().Load("123", "20")
		require.ErrorIs(err, threads.ErrTokenNotFound)
		before := f.opens.Load()
		_, _, err = f.run(t.Context(), f.args())
		require.Error(err)
		assert.Equal(before, f.opens.Load())
		assert.Equal(int32(1), f.publishes.Load())
	})
}

func TestThreadsQuotaInvalidatesRevokedGeneration(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newThreadsSendDaemonFixture(t)
	f.cfg.Threads.Sends = nil // Quota inspection does not enable publication.
	f.provider.readHandler = func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != "/v1.0/me/threads_publishing_limit" {
			return false
		}
		f.quotas.Add(1)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = fmt.Fprint(w, `{"error":{"code":190,"message":"synthetic revoked token"}}`)
		return true
	}
	args := []string{"send-reply", fmt.Sprint(f.root), "--quota", "--json"}
	_, _, err := f.run(t.Context(), args)
	require.Error(err)
	saved, err := f.deps.tokenManager().Load("123", "20")
	require.NoError(err)
	assert.True(saved.Invalidated)
	_, _, err = f.run(t.Context(), args)
	require.Error(err)
	assert.Equal(int32(1), f.quotas.Load())
	assert.Zero(f.creates.Load())
	assert.Zero(f.publishes.Load())
}

func TestThreadsSendDelegatedHTTPDeniedBeforeCredentials(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newThreadsSendDaemonFixture(t)
	server := httptest.NewServer(api.NewServerWithOptions(api.ServerOptions{Config: f.cfg, Store: f.adapter, Logger: slog.New(slog.DiscardHandler)}).Router())
	defer server.Close()
	owner, err := daemonclient.New(daemonclient.Config{URL: server.URL, APIKey: f.cfg.Server.APIKey, AllowInsecure: true})
	require.NoError(err)
	grant, err := owner.IssueAgentToken(t.Context(), "Synthetic draft agent", []string{"draft.create"}, []int64{f.source.ID}, nil, time.Time{})
	require.NoError(err)
	delegated, err := daemonclient.New(daemonclient.Config{URL: server.URL, APIKey: grant.Secret, AllowInsecure: true})
	require.NoError(err)
	var out strings.Builder
	err = delegated.RunCLICommand(t.Context(), daemonclient.CLIRunRequest{Args: f.args()}, func(_, data string) error { out.WriteString(data); return nil })
	require.Error(err)
	assert.NotContains(out.String(), f.draft.Body)
	assert.Zero(f.opens.Load())
	assert.Zero(f.creates.Load())
	assert.Zero(f.publishes.Load())
}

func TestThreadsSendCLIAndMCPDaemonTransports(t *testing.T) {
	t.Run("CLI preserves uncertain outcome", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		f := newThreadsSendDaemonFixture(t)
		f.mode = "drop"
		server := httptest.NewServer(api.NewServerWithOptions(api.ServerOptions{Config: f.cfg, Store: f.adapter, Logger: slog.New(slog.DiscardHandler), OperationGate: api.NewSerialOperationGate()}).Router())
		defer server.Close()
		configPath := filepath.Join(t.TempDir(), "config.toml")
		require.NoError(os.WriteFile(configPath, []byte(fmt.Sprintf("[remote]\nurl = %q\napi_key = %q\nallow_insecure = true\n", server.URL, f.cfg.Server.APIKey)), 0600))
		root := newRootCommand()
		root.AddCommand(newSendReplyCommand())
		root.SetArgs(append([]string{"--config", configPath}, f.args()...))
		var out strings.Builder
		root.SetOut(&out)
		root.SetErr(io.Discard)
		// The production root suppresses runtime usage, keeping JSON stdout clean.
		require.Error(executeRootContext(t.Context(), root))
		var result threads.SendResult
		require.NoError(json.Unmarshal([]byte(out.String()), &result))
		assert.Equal("remote_unknown", result.Status)
		assert.Equal("unknown", result.RemoteStatus)
		assert.Equal(int32(1), f.publishes.Load())
	})
	t.Run("owner MCP confirmation reaches daemon", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		f := newThreadsSendDaemonFixture(t)
		server := mcpDraftTestDaemon(t, f.adapter, nil)
		cfg := &config.Config{HomeDir: t.TempDir(), Remote: config.RemoteConfig{URL: server.URL, APIKey: "owner-test-key", AllowInsecure: true}}
		session := mcpDraftTestSession(withStoreResolverConfig(t, cfg), t)
		args := map[string]any{"draft_id": f.draft.DraftID, "revision": 1}
		pending, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "send_reply", Arguments: args})
		require.NoError(err)
		require.True(pending.NeedsInput())
		assert.Zero(f.publishes.Load())
		result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "send_reply", Arguments: args, RequestState: pending.RequestState, InputResponses: sdkmcp.InputResponseMap{"confirm": &sdkmcp.ElicitResult{Action: "accept", Content: map[string]any{"confirm": true}}}})
		require.NoError(err)
		assert.False(result.IsError)
		structured, ok := result.StructuredContent.(map[string]any)
		require.True(ok)
		assert.Equal("sent", structured["remote_status"])
		assert.Equal(int32(1), f.publishes.Load())
	})
}

func TestThreadsSendClientDisconnectPreservesReceipt(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newThreadsSendDaemonFixture(t)
	server := httptest.NewServer(api.NewServerWithOptions(api.ServerOptions{Config: f.cfg, Store: f.adapter, Logger: slog.New(slog.DiscardHandler), OperationGate: api.NewSerialOperationGate()}).Router())
	defer server.Close()
	client, err := daemonclient.New(daemonclient.Config{URL: server.URL, APIKey: f.cfg.Server.APIKey, AllowInsecure: true})
	require.NoError(err)
	disconnect := errors.New("synthetic result consumer disconnected")
	err = client.RunCLICommand(t.Context(), daemonclient.CLIRunRequest{Args: f.args()}, func(stream, _ string) error {
		if stream == "stdout" {
			return disconnect
		}
		return nil
	})
	require.ErrorIs(err, disconnect)
	receipt, err := f.adapter.store.GetThreadsReplySendContext(t.Context(), f.draft.DraftID)
	require.NoError(err)
	assert.Equal("500", receipt.MediaID)
	assert.True(receipt.ArchiveComplete)
	require.NoError(f.deps.tokenManager().Delete("123", "20"))
	f.cfg.Threads.Sends = nil
	var out strings.Builder
	require.NoError(client.RunCLICommand(t.Context(), daemonclient.CLIRunRequest{Args: f.args()}, func(stream, data string) error {
		if stream == "stdout" {
			out.WriteString(data)
		}
		return nil
	}))
	var result threads.SendResult
	require.NoError(json.Unmarshal([]byte(out.String()), &result))
	assert.Equal("complete", result.Status)
	assert.Equal(int32(1), f.publishes.Load())
}
