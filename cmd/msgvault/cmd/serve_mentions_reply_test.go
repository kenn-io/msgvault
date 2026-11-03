package cmd

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/apiprotocol"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/meta"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func mentionActionAdapterFixture(t *testing.T) (*storeAPIAdapter, int64) {
	t.Helper()
	t.Setenv("EXAMPLE_MENTION_TOKEN", "")
	st := testutil.NewSQLiteTestStore(t)
	src, err := st.GetOrCreateSource(meta.MentionsSourceType, meta.InstagramSourceIdentifier("9", "100", "200"))
	require.NoError(t, err)
	conv, err := st.EnsureConversation(src.ID, "300", "Example mention")
	require.NoError(t, err)
	id, err := st.UpsertMessage(&store.Message{SourceID: src.ID, ConversationID: conv, SourceMessageID: "300", MessageType: meta.MentionsSourceType})
	require.NoError(t, err)
	require.NoError(t, st.SetMessageMetadata(id, sql.NullString{String: `{"version":1,"app_id":"9","page_id":"100","ig_user_id":"200","media_id":"300","caption_mention":true}`, Valid: true}))
	profile := config.InstagramCommentsSource{Identifier: "example", AppID: "9", PageID: "100", InstagramAccountID: "200", TokenEnv: "EXAMPLE_MENTION_TOKEN", Mentions: true, Enabled: true}
	return &storeAPIAdapter{store: st, config: &config.Config{Data: config.DataConfig{DataDir: t.TempDir()}, InstagramComments: []config.InstagramCommentsSource{profile}}}, id
}

func TestMentionReplyDelegatedRefused(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	a, id := mentionActionAdapterFixture(t)
	a.config.Server = config.ServerConfig{APIKey: "owner", AgentAccess: true}
	srv := api.NewServerWithOptions(api.ServerOptions{Config: a.config, Store: a, Logger: slog.New(slog.DiscardHandler)})
	t.Cleanup(func() { requirements.NoError(srv.Shutdown(context.Background())) })
	server := httptest.NewServer(srv.Router())
	t.Cleanup(server.Close)
	owner, err := daemonclient.New(daemonclient.Config{URL: server.URL, APIKey: "owner", AllowInsecure: true})
	requirements.NoError(err)
	t.Cleanup(func() { requirements.NoError(owner.Close()) })
	m, err := a.store.MetaCommentTargetContext(t.Context(), id)
	requirements.NoError(err)
	grant, err := owner.IssueAgentToken(t.Context(), "example reader", []string{"message.read"}, []int64{m.SourceID}, nil, time.Time{})
	requirements.NoError(err)
	// The owner reaches the real mention resolver; the delegated request must stop
	// at authorization before opening a provider client or writing a receipt.
	preview, err := owner.RunCommentAction(t.Context(), id, meta.ActionRequest{Action: meta.ActionPublicReply, Body: "Example reply", DryRun: true})
	requirements.NoError(err)
	assertions.Equal("instagram", preview.Disclosure.Provider)
	r := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/v1/messages/%d/comment-actions", id), strings.NewReader(`{"action":"public_reply","body":"Example reply","confirm":"`+preview.Confirm+`"}`))
	r.RemoteAddr = "127.0.0.1:4242"
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set(apiprotocol.AgentTokenHeader, grant.Secret)
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, r)
	assertions.Equal(http.StatusUnauthorized, w.Code, w.Body.String())
	rows, err := a.store.ListMetaCommentActionsContext(t.Context(), m.SourceID, m.CommentID)
	requirements.NoError(err)
	assertions.Empty(rows)
}

func TestMentionReplyConfiguredProfileAndReceipts(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	a, id := mentionActionAdapterFixture(t)
	a.config.Server.APIKey = "owner"
	srv := api.NewServerWithOptions(api.ServerOptions{Config: a.config, Store: a, Logger: slog.New(slog.DiscardHandler)})
	t.Cleanup(func() { requirements.NoError(srv.Shutdown(context.Background())) })
	server := httptest.NewServer(srv.Router())
	t.Cleanup(server.Close)
	owner, err := daemonclient.New(daemonclient.Config{URL: server.URL, APIKey: "owner", AllowInsecure: true})
	requirements.NoError(err)
	t.Cleanup(func() { requirements.NoError(owner.Close()) })
	var calls atomic.Int32
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "Bearer synthetic-mention-token", r.Header.Get("Authorization"))
		if r.Method == http.MethodGet {
			switch r.URL.Path {
			case "/v26.0/me":
				_, _ = fmt.Fprint(w, `{"id":"100","name":"Example Page","category":"Example"}`)
			case "/v26.0/100":
				_, _ = fmt.Fprint(w, `{"id":"100","instagram_business_account":{"id":"200","username":"example_account"}}`)
			default:
				assert.Fail(t, "unexpected verification path", r.URL.Path)
				w.WriteHeader(http.StatusNotFound)
			}
			return
		}
		calls.Add(1)
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "/v26.0/200/mentions", r.URL.Path)
		assert.Empty(t, r.URL.RawQuery)
		if !assert.NoError(t, r.ParseForm()) {
			return
		}
		assert.Equal(t, url.Values{"media_id": {"300"}, "message": {"Example public reply"}}, r.PostForm)
		_, _ = fmt.Fprint(w, `{"id":"500"}`)
	}))
	t.Cleanup(fixture.Close)
	destination, err := url.Parse(fixture.URL)
	requirements.NoError(err)
	original := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = original })
	http.DefaultTransport = testTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host == server.Listener.Addr().String() {
			return original.RoundTrip(r)
		}
		if r.URL.Host != "graph.facebook.com" {
			return nil, fmt.Errorf("unexpected fixture host %q", r.URL.Host)
		}
		clone := r.Clone(r.Context())
		clone.URL.Scheme, clone.URL.Host = destination.Scheme, destination.Host
		return original.RoundTrip(clone)
	})
	request := meta.ActionRequest{Action: meta.ActionPublicReply, Body: "Example public reply", DryRun: true}
	preview, err := owner.RunCommentAction(t.Context(), id, request)
	requirements.NoError(err)
	assertions.Equal("instagram", preview.Disclosure.Provider)
	assertions.Zero(calls.Load())
	request.DryRun, request.Confirm = false, preview.Confirm
	profile := &a.config.InstagramComments[0]
	for _, denied := range []string{"action", "mentions", "disabled", "identity"} {
		t.Run(denied, func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			saved := *profile
			t.Cleanup(func() { *profile = saved })
			profile.Actions = []string{"public_reply"}
			switch denied {
			case "action":
				profile.Actions = nil
			case "mentions":
				profile.Mentions = false
			case "disabled":
				profile.Enabled = false
			case "identity":
				profile.PageID = "101"
			}
			_, err := owner.RunCommentAction(t.Context(), id, request)
			var failure *meta.ActionError
			requirements.ErrorAs(err, &failure)
			assertions.Equal("action_not_enabled", failure.Code)
			assertions.Zero(calls.Load())
		})
	}
	profile.Actions = []string{"public_reply"}
	t.Setenv("EXAMPLE_MENTION_TOKEN", "synthetic-mention-token")
	result, err := owner.RunCommentAction(t.Context(), id, request)
	requirements.NoError(err)
	requirements.NotNil(result.Receipt)
	assertions.Equal("succeeded", result.Receipt.State)
	assertions.True(result.Receipt.ArchiveComplete)
	assertions.Equal(int32(1), calls.Load())
	sent, err := a.store.MetaCommentTargetContext(t.Context(), result.Receipt.ReplyMessageID)
	requirements.NoError(err)
	assertions.True(sent.IsFromMe)
	assertions.Equal(id, sent.ParentMessageID)
	assertions.Equal("500", sent.CommentID)
	t.Setenv("EXAMPLE_MENTION_TOKEN", "")
	replay, err := owner.RunCommentAction(t.Context(), id, request)
	requirements.NoError(err)
	assertions.Equal(result.Receipt.ID, replay.Receipt.ID)
	assertions.Equal(result.Receipt.ReplyMessageID, replay.Receipt.ReplyMessageID)
	assertions.Equal(int32(1), calls.Load())
	profile.Mentions = false
	rows, err := owner.ListCommentActions(t.Context(), id)
	requirements.NoError(err)
	requirements.Len(rows, 1)
	assertions.Equal("500", rows[0].RemoteID)
	assertions.True(rows[0].ArchiveComplete)
}
