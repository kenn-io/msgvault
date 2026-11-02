package cmd

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/meta"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func commentAdapterFixture(t *testing.T) (*storeAPIAdapter, int64) {
	t.Helper()
	st := testutil.NewSQLiteTestStore(t)
	src, err := st.GetOrCreateSource(meta.SourceType, "app:9:page:100")
	require.NoError(t, err)
	conv, err := st.EnsureConversation(src.ID, "100_200", "Example post")
	require.NoError(t, err)
	root, err := st.UpsertMessage(&store.Message{SourceID: src.ID, ConversationID: conv, SourceMessageID: "100_200", MessageType: meta.SourceType})
	require.NoError(t, err)
	id, err := st.UpsertMessage(&store.Message{SourceID: src.ID, ConversationID: conv, SourceMessageID: "100_200_300", MessageType: meta.SourceType, ReplyToMessageID: sql.NullInt64{Int64: root, Valid: true}, SentAt: sql.NullTime{Time: time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC), Valid: true}})
	require.NoError(t, err)
	require.NoError(t, st.SetMessageMetadataContext(t.Context(), id, sql.NullString{String: `{"version":1,"app_id":"9","page_id":"100","post_id":"100_200","parent_id":"100_200","ad":true}`, Valid: true}))
	require.NoError(t, st.UpsertMessageRawWithFormat(id, []byte(`{"id":"100_200_300","from":{"id":"300","name":"Example Author"}}`), "facebook-comment-json"))
	profile := facebookProfile()
	profile.Actions = []string{"hide", "delete"}
	return &storeAPIAdapter{store: st, config: &config.Config{Data: config.DataConfig{DataDir: t.TempDir()}, FacebookComments: []config.FacebookCommentsSource{profile}}}, id
}
func TestServeCommentActionUsesProfileTokenEnv(t *testing.T) { //nolint:paralleltest // intercepts the Graph transport
	a, id := commentAdapterFixture(t)
	t.Setenv("EXAMPLE_FACEBOOK_PAGE_TOKEN", "synthetic-page-token")
	calls, refreshes := 0, 0
	f := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		assert.Equal(t, "Bearer synthetic-page-token", r.Header.Get("Authorization"))
		assert.Equal(t, "/v26.0/100_200_300", r.URL.Path)
		assert.Empty(t, r.URL.RawQuery)
		_, _ = fmt.Fprint(w, `{"success":true}`)
	}))
	defer f.Close()
	destination, err := url.Parse(f.URL)
	require.NoError(t, err)
	original := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = original })
	http.DefaultTransport = testTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "graph.facebook.com" {
			return nil, errors.New("unexpected fixture host")
		}
		clone := r.Clone(r.Context())
		uri := *r.URL
		uri.Host = destination.Host
		uri.Scheme = destination.Scheme
		clone.URL = &uri
		return original.RoundTrip(clone)
	})
	a.draftCacheRefresh = func(context.Context, string) error { refreshes++; return nil }
	got, err := a.RunCommentAction(t.Context(), id, meta.ActionRequest{Action: meta.ActionHide}, nil)
	require.NoError(t, err)
	assert.Equal(t, "succeeded", got.Receipt.State)
	assert.Equal(t, 1, calls)
	assert.Equal(t, 1, refreshes)
	page, err := a.ListCommentQueue(t.Context(), api.CommentQueueRequest{})
	require.NoError(t, err)
	require.Len(t, page.Items, 1)
	assert.Equal(t, "hidden", page.Items[0].Moderation)
	_, err = a.RunCommentAction(t.Context(), id, meta.ActionRequest{Action: meta.ActionUnhide}, nil)
	require.NoError(t, err)
	page, err = a.ListCommentQueue(t.Context(), api.CommentQueueRequest{})
	require.NoError(t, err)
	require.Len(t, page.Items, 1)
	assert.Equal(t, "visible", page.Items[0].Moderation)
	t.Setenv("EXAMPLE_FACEBOOK_PAGE_TOKEN", "")
	_, err = a.RunCommentAction(t.Context(), id, meta.ActionRequest{Action: meta.ActionUnhide}, nil)
	var failure *meta.ActionError
	require.ErrorAs(t, err, &failure)
	assert.Equal(t, "credential_unavailable", failure.Code)
	rows, err := a.ListCommentActions(t.Context(), id)
	require.NoError(t, err)
	require.Len(t, rows, 2)
	assert.Equal(t, meta.ActionHide, rows[0].Action)
	assert.Equal(t, meta.ActionUnhide, rows[1].Action)
	assert.False(t, rows[0].CreatedAt.IsZero())
	assert.False(t, rows[1].UpdatedAt.IsZero())
	assert.Equal(t, 2, calls)
}
func TestServeCommentActionNoFallbackProfile(t *testing.T) {
	a, id := commentAdapterFixture(t)
	a.config.FacebookComments[0].AppID = "8"
	_, err := a.RunCommentAction(t.Context(), id, meta.ActionRequest{Action: meta.ActionHide}, nil)
	var failure *meta.ActionError
	require.ErrorAs(t, err, &failure)
	assert.Equal(t, "action_not_enabled", failure.Code)
	rows, err := a.ListCommentActions(t.Context(), id)
	require.NoError(t, err)
	assert.Empty(t, rows)
}
func TestServeCommentQueueAndTriage(t *testing.T) {
	a, id := commentAdapterFixture(t)
	page, err := a.ListCommentQueue(t.Context(), api.CommentQueueRequest{Limit: 1})
	require.NoError(t, err)
	require.Len(t, page.Items, 1)
	assert.Equal(t, id, page.NextBeforeID)
	assert.True(t, page.Items[0].Ad)
	assert.Equal(t, "unknown", page.Items[0].Moderation)
	empty, err := a.ListCommentQueue(t.Context(), api.CommentQueueRequest{Limit: 1, BeforeID: id})
	require.NoError(t, err)
	assert.Empty(t, empty.Items)
	assert.Zero(t, empty.NextBeforeID)
	_, err = a.UpdateCommentTriage(t.Context(), id, "handled", "open", nil)
	require.NoError(t, err)
	_, err = a.UpdateCommentTriage(t.Context(), id, "skipped", "open", nil)
	var failure *meta.ActionError
	require.ErrorAs(t, err, &failure)
	assert.Equal(t, "triage_conflict", failure.Code)
	page, err = a.ListCommentQueue(t.Context(), api.CommentQueueRequest{State: "handled", Source: "app:9:page:100"})
	require.NoError(t, err)
	require.Len(t, page.Items, 1)
	assert.Equal(t, "handled", page.Items[0].Triage)
	page, err = a.ListCommentQueue(t.Context(), api.CommentQueueRequest{Source: "app:9:page:999"})
	require.NoError(t, err)
	assert.Empty(t, page.Items)
}
