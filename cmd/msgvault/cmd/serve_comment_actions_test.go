package cmd

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/meta"
	"go.kenn.io/msgvault/internal/query"
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
	assertions := assert.New(t)
	requirements := require.New(t)
	a, id := commentAdapterFixture(t)
	t.Setenv("EXAMPLE_FACEBOOK_PAGE_TOKEN", "synthetic-page-token")
	calls, refreshes := 0, 0
	f := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		assertions.Equal("Bearer synthetic-page-token", r.Header.Get("Authorization"))
		if r.Method == http.MethodGet {
			assertions.Equal("/v26.0/me", r.URL.Path)
			_, _ = fmt.Fprint(w, `{"id":"100","name":"Example Page","category":"Community"}`)
			return
		}
		assertions.Equal("/v26.0/100_200_300", r.URL.Path)
		assertions.Empty(r.URL.RawQuery)
		_, _ = fmt.Fprint(w, `{"success":true}`)
	}))
	defer f.Close()
	destination, err := url.Parse(f.URL)
	requirements.NoError(err)
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
	requirements.NoError(err)
	assertions.Equal("succeeded", got.Receipt.State)
	assertions.Equal(2, calls)
	assertions.Equal(1, refreshes)
	page, err := a.ListCommentQueue(t.Context(), api.CommentQueueRequest{})
	requirements.NoError(err)
	requirements.Len(page.Items, 1)
	assertions.Equal("hidden", page.Items[0].Moderation)
	_, err = a.RunCommentAction(t.Context(), id, meta.ActionRequest{Action: meta.ActionUnhide}, nil)
	requirements.NoError(err)
	page, err = a.ListCommentQueue(t.Context(), api.CommentQueueRequest{})
	requirements.NoError(err)
	requirements.Len(page.Items, 1)
	assertions.Equal("visible", page.Items[0].Moderation)
	t.Setenv("EXAMPLE_FACEBOOK_PAGE_TOKEN", "")
	_, err = a.RunCommentAction(t.Context(), id, meta.ActionRequest{Action: meta.ActionUnhide}, nil)
	var failure *meta.ActionError
	requirements.ErrorAs(err, &failure)
	assertions.Equal("credential_unavailable", failure.Code)
	rows, err := a.ListCommentActions(t.Context(), id)
	requirements.NoError(err)
	requirements.Len(rows, 2)
	assertions.Equal(meta.ActionHide, rows[0].Action)
	assertions.Equal(meta.ActionUnhide, rows[1].Action)
	assertions.False(rows[0].CreatedAt.IsZero())
	assertions.False(rows[1].UpdatedAt.IsZero())
	assertions.Equal(4, calls)
}
func TestServeCommentActionNoFallbackProfile(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	a, id := commentAdapterFixture(t)
	a.config.FacebookComments[0].AppID = "8"
	_, err := a.RunCommentAction(t.Context(), id, meta.ActionRequest{Action: meta.ActionHide}, nil)
	var failure *meta.ActionError
	require.ErrorAs(err, &failure)
	assert.Equal("action_not_enabled", failure.Code)
	rows, err := a.ListCommentActions(t.Context(), id)
	require.NoError(err)
	assert.Empty(rows)
}
func TestServeCommentQueueAndTriage(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	a, id := commentAdapterFixture(t)
	page, err := a.ListCommentQueue(t.Context(), api.CommentQueueRequest{Limit: 1})
	require.NoError(err)
	require.Len(page.Items, 1)
	assert.Equal(id, page.NextBeforeID)
	assert.True(page.Items[0].Ad)
	assert.Equal("unknown", page.Items[0].Moderation)
	empty, err := a.ListCommentQueue(t.Context(), api.CommentQueueRequest{Limit: 1, BeforeID: id})
	require.NoError(err)
	assert.Empty(empty.Items)
	assert.Zero(empty.NextBeforeID)
	_, err = a.UpdateCommentTriage(t.Context(), id, "handled", "open", nil)
	require.NoError(err)
	_, err = a.UpdateCommentTriage(t.Context(), id, "skipped", "open", nil)
	var failure *meta.ActionError
	require.ErrorAs(err, &failure)
	assert.Equal("triage_conflict", failure.Code)
	page, err = a.ListCommentQueue(t.Context(), api.CommentQueueRequest{State: "handled", Source: "app:9:page:100"})
	require.NoError(err)
	require.Len(page.Items, 1)
	assert.Equal("handled", page.Items[0].Triage)
	page, err = a.ListCommentQueue(t.Context(), api.CommentQueueRequest{Source: "app:9:page:999"})
	require.NoError(err)
	assert.Empty(page.Items)
}

func TestServeCommentTriageRefreshesCachedLabels(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	a, id := commentAdapterFixture(t)
	var sequence int
	var name, dbPath string
	requirements.NoError(a.store.DB().QueryRow("PRAGMA database_list").Scan(&sequence, &name, &dbPath))
	analyticsDir := filepath.Join(t.TempDir(), "analytics")
	_, err := buildCache(dbPath, analyticsDir, true)
	requirements.NoError(err)
	labels := func() map[string]int64 {
		engine, err := query.NewDuckDBEngine(analyticsDir, "", nil)
		requirements.NoError(err)
		defer func() { requirements.NoError(engine.Close()) }()
		options := query.DefaultAggregateOptions()
		options.SearchQuery = "message_type:facebook_comments"
		rows, err := engine.Aggregate(t.Context(), query.ViewLabels, options)
		requirements.NoError(err)
		result := map[string]int64{}
		for _, row := range rows {
			result[row.Key] = row.Count
		}
		return result
	}
	assertions.Zero(labels()["Handled"])
	refreshes := 0
	a.draftCacheRefresh = func(ctx context.Context, source string) error {
		refreshes++
		assertions.Equal("app:9:page:100", source)
		_, err := buildCache(dbPath, analyticsDir, true)
		return err
	}
	_, err = a.UpdateCommentTriage(t.Context(), id, "handled", "open", nil)
	requirements.NoError(err)
	assertions.Equal(map[string]int64{"Handled": 1}, labels())
	assertions.Equal(1, refreshes)
	_, err = a.UpdateCommentTriage(t.Context(), id, "skipped", "open", nil)
	actionFailure := new(meta.ActionError)
	requirements.ErrorAs(err, &actionFailure)
	assertions.Equal("triage_conflict", actionFailure.Code)
	assertions.Equal(1, refreshes)
	_, err = a.UpdateCommentTriage(t.Context(), id, "open", "handled", nil)
	requirements.NoError(err)
	assertions.Zero(labels()["Handled"])
	assertions.Equal(2, refreshes)
}

func TestServeCommentActionsRetainMergedProvenance(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	a, id := commentAdapterFixture(t)
	m, err := a.store.MetaCommentTargetContext(t.Context(), id)
	require.NoError(err)
	row, _, err := a.store.ClaimMetaCommentActionContext(t.Context(), store.MetaCommentActionClaim{ActionKey: "history", SourceID: m.SourceID, SourceIdentifier: "app:9:page:100", MessageID: id, TargetCommentID: m.CommentID, Action: "hide"})
	require.NoError(err)
	row, err = a.store.MarkMetaCommentActionDispatchedContext(t.Context(), row.ID, row.RowRevision)
	require.NoError(err)
	row, err = a.store.FinishMetaCommentActionContext(t.Context(), row.ID, row.RowRevision, store.MetaCommentActionOutcome{State: "succeeded"})
	require.NoError(err)
	into, err := a.store.GetOrCreateSource(meta.SourceType, "app:8:page:200")
	require.NoError(err)
	_, err = a.store.MergeSourcesContext(t.Context(), store.MergeSourcesRequest{FromSourceID: m.SourceID, IntoSourceID: into.ID})
	require.NoError(err)
	rows, err := a.ListCommentActions(t.Context(), id)
	require.NoError(err)
	require.Len(rows, 1)
	assert.Equal(row.ID, rows[0].ID)
	assert.Equal("succeeded", rows[0].State)
}

// Changing the configured credential must not change the identity that an
// archived target and its confirmation disclosed. Recovery remains offline.
func TestServeCommentActionVerifiesPageBeforeDispatch(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	a, id := commentAdapterFixture(t)
	t.Setenv("EXAMPLE_FACEBOOK_PAGE_TOKEN", "synthetic-page-token")
	pageID := "101"
	reads, writes := 0, 0
	original := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = original })
	http.DefaultTransport = testTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "graph.facebook.com" {
			return nil, errors.New("unexpected fixture host")
		}
		body := `{"success":false}`
		if r.Method == http.MethodGet {
			reads++
			assertions.Equal("/v26.0/me", r.URL.Path)
			body = fmt.Sprintf(`{"id":%q,"name":"Example Page","category":"Community"}`, pageID)
		} else {
			writes++
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
	})
	_, err := a.RunCommentAction(t.Context(), id, meta.ActionRequest{Action: meta.ActionHide, DryRun: true}, nil)
	requirements.NoError(err)
	assertions.Zero(reads)
	assertions.Zero(writes)
	_, err = a.RunCommentAction(t.Context(), id, meta.ActionRequest{Action: meta.ActionHide}, nil)
	var failure *meta.ActionError
	requirements.ErrorAs(err, &failure)
	assertions.Equal("credential_unavailable", failure.Code)
	assertions.Equal(1, reads)
	assertions.Zero(writes)
	rows, err := a.ListCommentActions(t.Context(), id)
	requirements.NoError(err)
	assertions.Empty(rows)
	pageID = "100"
	_, err = a.RunCommentAction(t.Context(), id, meta.ActionRequest{Action: meta.ActionHide}, nil)
	requirements.ErrorAs(err, &failure)
	assertions.Equal("outcome_unknown", failure.Code)
	assertions.Equal(2, reads)
	assertions.Equal(1, writes)
	pageID = "101"
	resolved, err := a.RunCommentAction(t.Context(), id, meta.ActionRequest{Action: meta.ActionHide, ResolveUnknown: "sent"}, nil)
	requirements.NoError(err)
	assertions.Equal("succeeded", resolved.Receipt.State)
	assertions.Equal(2, reads)
	assertions.Equal(1, writes)
}

func TestCommentQueueIncludesInstagram(t *testing.T) { //nolint:paralleltest // intercepts the fixture Graph transport
	assertions := assert.New(t)
	requirements := require.New(t)
	a, _ := commentAdapterFixture(t)
	src, err := a.store.GetOrCreateSource(meta.InstagramSourceType, "app:9:page:100:instagram:200")
	requirements.NoError(err)
	conv, err := a.store.EnsureConversation(src.ID, "300", "Example media")
	requirements.NoError(err)
	root, err := a.store.UpsertMessage(&store.Message{SourceID: src.ID, ConversationID: conv, SourceMessageID: "300", MessageType: meta.InstagramSourceType})
	requirements.NoError(err)
	id, err := a.store.UpsertMessage(&store.Message{SourceID: src.ID, ConversationID: conv, SourceMessageID: "400", MessageType: meta.InstagramSourceType, ReplyToMessageID: sql.NullInt64{Int64: root, Valid: true}})
	requirements.NoError(err)
	requirements.NoError(a.store.SetMessageMetadataContext(t.Context(), id, sql.NullString{String: `{"version":1,"app_id":"9","page_id":"100","instagram_account_id":"200","media_id":"300","parent_id":"300","organic":true}`, Valid: true}))
	requirements.NoError(a.store.UpsertMessageRawWithFormat(id, []byte(`{"id":"400","from":{"id":"600","username":"example_author"}}`), "instagram-comment-json"))
	a.config.InstagramComments = []config.InstagramCommentsSource{{Identifier: "example-account", AppID: "9", PageID: "100", InstagramAccountID: "200", TokenEnv: "EXAMPLE_INSTAGRAM_TOKEN", Enabled: true, Actions: []string{"public_reply"}}}
	page, err := a.ListCommentQueue(t.Context(), api.CommentQueueRequest{})
	requirements.NoError(err)
	requirements.Len(page.Items, 2)
	selected, err := a.ListCommentQueue(t.Context(), api.CommentQueueRequest{Source: src.Identifier})
	requirements.NoError(err)
	requirements.Len(selected.Items, 1)
	assertions.Equal("instagram", selected.Items[0].Provider)
	t.Setenv("EXAMPLE_INSTAGRAM_TOKEN", "synthetic-instagram-token")
	calls := 0
	wrongPage, wrongAccount := true, false
	f := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		assertions.Equal("Bearer synthetic-instagram-token", r.Header.Get("Authorization"))
		if r.URL.Path == "/v26.0/me" {
			pageID := "100"
			if wrongPage {
				pageID = "101"
			}
			_, _ = fmt.Fprintf(w, `{"id":%q,"name":"Example Page","category":"Community"}`, pageID)
			return
		}
		if r.URL.Path == "/v26.0/100" {
			accountID := "200"
			if wrongAccount {
				accountID = "201"
			}
			_, _ = fmt.Fprintf(w, `{"id":"100","instagram_business_account":{"id":%q,"username":"example_account"}}`, accountID)
			return
		}
		assertions.Equal("/v26.0/400/replies", r.URL.Path)
		_, _ = fmt.Fprint(w, `{"id":"501"}`)
	}))
	defer f.Close()
	destination, err := url.Parse(f.URL)
	requirements.NoError(err)
	original := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = original })
	http.DefaultTransport = testTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "graph.facebook.com" {
			return nil, errors.New("unexpected fixture host")
		}
		clone := r.Clone(r.Context())
		uri := *r.URL
		uri.Host, uri.Scheme = destination.Host, destination.Scheme
		clone.URL = &uri
		return original.RoundTrip(clone)
	})
	preview, err := a.RunCommentAction(t.Context(), id, meta.ActionRequest{Action: meta.ActionPublicReply, Body: "reply", DryRun: true}, nil)
	requirements.NoError(err)
	assertions.Zero(calls)
	var failure *meta.ActionError
	_, err = a.RunCommentAction(t.Context(), id, meta.ActionRequest{Action: meta.ActionPublicReply, Body: "reply", Confirm: preview.Confirm}, nil)
	requirements.ErrorAs(err, &failure)
	assertions.Equal("credential_unavailable", failure.Code)
	assertions.Equal(1, calls)
	wrongPage, wrongAccount = false, true
	_, err = a.RunCommentAction(t.Context(), id, meta.ActionRequest{Action: meta.ActionPublicReply, Body: "reply", Confirm: preview.Confirm}, nil)
	requirements.ErrorAs(err, &failure)
	assertions.Equal("credential_unavailable", failure.Code)
	assertions.Equal(3, calls)
	wrongAccount = false
	got, err := a.RunCommentAction(t.Context(), id, meta.ActionRequest{Action: meta.ActionPublicReply, Body: "reply", Confirm: preview.Confirm}, nil)
	requirements.NoError(err)
	assertions.Equal("501", got.Receipt.RemoteID)
	assertions.Equal(6, calls)
	rows, err := a.ListCommentActions(t.Context(), id)
	requirements.NoError(err)
	requirements.Len(rows, 1)
	a.config.InstagramComments[0].InstagramAccountID = "201"
	recovered, err := a.RunCommentAction(t.Context(), id, meta.ActionRequest{Action: meta.ActionPublicReply, Body: "reply", Confirm: preview.Confirm}, nil)
	requirements.NoError(err, "an existing receipt remains available after the profile changes")
	requirements.NotNil(recovered.Receipt)
	assertions.Equal(got.Receipt.ID, recovered.Receipt.ID)
	assertions.Equal("succeeded", recovered.Receipt.State)
	fresh, err := a.RunCommentAction(t.Context(), id, meta.ActionRequest{Action: meta.ActionPublicReply, Body: "another reply", DryRun: true}, nil)
	requirements.NoError(err)
	_, err = a.RunCommentAction(t.Context(), id, meta.ActionRequest{Action: meta.ActionPublicReply, Body: "another reply", Confirm: fresh.Confirm}, nil)
	requirements.ErrorAs(err, &failure)
	assertions.Equal("action_not_enabled", failure.Code)
	assertions.Equal(6, calls, "neither local recovery nor a disabled fresh write contacts the provider")
}
