package x

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestXFullObligationCheckpointRecoversUnidentifiedPage(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	source := importTestSource(t, st)
	now := time.Now().UTC().Truncate(time.Second)
	walk, err := prepareTimelineWalk("1001", "timeline", nil, time.Time{}, now)
	require.NoError(err)
	walk.Token, walk.SeenTokens, walk.UnresolvedPage = "retry-page", []string{""}, true
	// A prior run has completed cleanup for 900 identified results. One
	// identified result and an unidentified page error still need provider I/O.
	tx, err := st.DB().Begin()
	require.NoError(err)
	t.Cleanup(func() { _ = tx.Rollback() })
	stmt, err := tx.Prepare(st.Rebind(`INSERT INTO x_post_versions(source_id,post_id,canonical_id,version_position,availability,suppressed_at,cleanup_pending) VALUES(?,?,?,0,'deleted',?,0)`))
	require.NoError(err)
	for i := range 901 {
		id := strconv.Itoa(10000 + i)
		walk.UnresolvedIDs = append(walk.UnresolvedIDs, id)
		if i < 900 {
			_, err = stmt.Exec(source.ID, id, id, now)
			require.NoError(err)
		}
	}
	require.NoError(stmt.Close())
	require.NoError(tx.Commit())
	state, err := st.LoadXControlContext(t.Context(), source.ID)
	require.NoError(err)
	_, err = st.UpdateXControlContext(t.Context(), source.ID, state.Revision, store.XControlPatch{OwnerID: "1001", Own: walk})
	require.NoError(err)
	var lookups, pageCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/tweets" {
			lookups.Add(1)
			assert.Equal("10900", r.URL.Query().Get("ids"))
			current, err := st.LoadXControlContext(r.Context(), source.ID)
			if assert.NoError(err) {
				assert.True(current.Own.UnresolvedPage)
				assert.Equal("retry-page", current.Own.Token)
				assert.False(current.Own.Complete)
			}
			wireImportPage(w, []string{wireOwnPost("10900", now.Add(-time.Minute))}, "")
			return
		}
		if strings.HasSuffix(r.URL.Path, "/mentions") || r.URL.Query().Get("start_time") != "" {
			wireImportPage(w, nil, "")
			return
		}
		assert.Equal("retry-page", r.URL.Query().Get("pagination_token"))
		if pageCalls.Add(1) == 1 {
			_, _ = fmt.Fprint(w, `{"errors":[{"title":"Synthetic unidentified result"}],"meta":{"next_token":"unsafe-next"}}`)
			return
		}
		wireImportPage(w, []string{wireOwnPost("10901", now.Add(-time.Minute))}, "")
	}))
	defer server.Close()
	credentials := &importCredentials{snapshot: CredentialSnapshot{OwnerID: "1001", Generation: 1, GrantedScopes: []string{"tweet.read", "users.read"}}}
	makeImporter := func() *Importer {
		imp := NewImporter(st, NewClient(server.URL, server.Client()), importTestPolicy(t), credentials)
		imp.now = func() time.Time { return now }
		return imp
	}
	_, err = makeImporter().Import(t.Context(), ImportOptions{SourceID: source.ID, OwnerID: "1001"})
	require.ErrorIs(err, ErrUnresolvedPage)
	state, err = st.LoadXControlContext(t.Context(), source.ID)
	require.NoError(err)
	assert.Empty(state.Own.UnresolvedIDs)
	assert.True(state.Own.UnresolvedPage)
	assert.Equal("retry-page", state.Own.Token)
	assert.Empty(state.Own.Watermark)
	summary, err := makeImporter().Import(t.Context(), ImportOptions{SourceID: source.ID, OwnerID: "1001"})
	require.NoError(err)
	assert.True(summary.OwnComplete)
	assert.Equal("10901", summary.OwnWatermark)
	assert.Equal([]string{"10900", "10901"}, archivedXIDs(t, st, source.ID))
	assert.Equal(int32(1), lookups.Load())
	assert.Equal(int32(2), pageCalls.Load())
}

func TestXExpandedAuthorErrorCannotRemoveCollidingPost(t *testing.T) {
	for _, endpoint := range []string{"timeline", "search", "lookup"} {
		t.Run(endpoint, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			st := testutil.NewTestStore(t)
			source, err := st.GetOrCreateSource("x", "1001")
			require.NoError(err)
			at := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
			saved, err := st.PersistXPostContext(t.Context(), store.XPersistPostInput{
				SourceID: source.ID, OwnerID: "1001", PostID: "1002", AuthorID: "1003", ConversationID: "1002",
				EditHistory: []string{"1002"}, Text: "unrelated retained post", CreatedAt: at, FetchedAt: at,
				Availability: "public", Author: &store.XAuthorInput{UserID: "1003", Username: "example_author", FetchedAt: at},
			})
			require.NoError(err)
			client, _ := servePage(t, `{"data":[{"id":"700","author_id":"1002","conversation_id":"700","text":"unverified reply","created_at":"2026-10-30T12:00:00Z"}],"errors":[{"resource_type":"user","resource_id":"1002","type":"https://api.x.com/2/problems/resource-not-found"}]}`)
			requests := map[string]func(context.Context) (Page, error){
				"timeline": func(ctx context.Context) (Page, error) {
					return client.Timeline(ctx, TimelineRequest{OwnerID: "1001", MaxResults: 5})
				},
				"search": func(ctx context.Context) (Page, error) {
					return client.SearchAll(ctx, SearchRequest{Query: "from:example_author", MaxResults: 10})
				},
				"lookup": func(ctx context.Context) (Page, error) { return client.Lookup(ctx, []string{"700"}) },
			}
			page, err := requests[endpoint](t.Context())
			require.NoError(err)
			require.Len(page.Errors, 1)
			assert.False(page.Errors[0].Unavailable, "an author expansion failure is not post deletion evidence")
			require.NoError(queueUnavailablePageResults(t.Context(), st, source.ID, page))
			pending, err := st.XCleanupPendingContext(t.Context())
			require.NoError(err)
			assert.False(pending)
			body, err := st.GetMessageBodyText(t.Context(), saved.MessageID)
			require.NoError(err)
			assert.Equal("unrelated retained post", body)
			walk, err := prepareTimelineWalk("1001", "mentions", nil, time.Time{}, at.Add(time.Hour))
			require.NoError(err)
			next, err := advanceWalk(walk, page)
			require.NoError(err)
			assert.Empty(next.UnresolvedIDs, "author IDs must not become missing-post obligations")
			assert.Equal(int64(1), next.ObservedResults, "expansion errors do not count toward post endpoint reach")
			assert.True(next.Complete)
		})
	}
}

func TestXDeletionRequiresExplicitPostResource(t *testing.T) {
	for _, resource := range []string{"tweet", "post", "user", "list", ""} {
		t.Run("resource="+resource, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			client, _ := servePage(t, fmt.Sprintf(`{"errors":[{"resource_type":%q,"resource_id":"1002","type":"https://api.x.com/2/problems/resource-not-found"}]}`, resource))
			page, err := client.Lookup(t.Context(), []string{"1002"})
			require.NoError(err)
			require.Len(page.Errors, 1)
			assert.Equal(resource == "tweet" || resource == "post", page.Errors[0].Unavailable)
		})
	}
}

func TestXDelayedProtectedPagePreservesNewerPublicEvidence(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("x", "1001")
	require.NoError(err)
	other, err := st.GetOrCreateSource("x", "1003")
	require.NoError(err)
	at := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	post := Post{ID: "100", AuthorID: "1002", ConversationID: "100", Text: "newer public content", CreatedAt: at, EditHistory: []string{"100"}}
	public := User{ID: "1002", Username: "example_author", ProtectedKnown: true}
	require.NoError(WritePage(t.Context(), st, other.ID, "1003", Page{Posts: []Post{post}, Users: []User{public}, FetchedAt: at.Add(time.Hour)}, at))
	policy, err := config.NewDefaultConfig().XAccount("1001")
	require.NoError(err)
	imp := NewImporter(st, nil, policy, nil)
	page := Page{Posts: []Post{post}, Users: []User{{ID: "1002", Protected: true, ProtectedKnown: true}}, FetchedAt: at}
	require.NoError(imp.persistPage(t.Context(), &ImportSummary{SourceID: source.ID}, "1001", &store.XWalk{}, page))
	pending, err := st.XCleanupPendingContext(t.Context())
	require.NoError(err)
	assert.False(pending, "a delayed protected response must not suppress newer explicit public evidence")
	target, err := st.ResolveXPostContext(t.Context(), other.ID, "100")
	require.NoError(err)
	assert.Equal("public", target.Availability)
}

func TestXRefreshedCredentialGenerationSurvivesDeniedAdmission(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	var tokenCalls, resourceCalls atomic.Int32
	f := newBudgetFixture(t, func(f *budgetFixture, w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/token" {
			resourceCalls.Add(1)
			http.Error(w, "unexpected resource request", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if tokenCalls.Add(1) == 1 {
			_, _ = fmt.Fprint(w, `{"access_token":"rotated-example","refresh_token":"rotated-refresh","expires_in":7200,"token_type":"bearer","scope":"tweet.read users.read offline.access"}`)
			return
		}
		w.WriteHeader(http.StatusBadRequest)
		_, _ = fmt.Fprint(w, `{"error":"invalid_grant"}`)
	})
	now := time.Now().UTC()
	manager := NewTokenManager(tokenTempDir(t), TokenManagerOptions{TokenURL: f.server.URL + "/token", HTTPClient: f.server.Client(), Now: func() time.Time { return now }})
	record := NewTokenRecord("100", "example-client", PublicClient, "initial-example", "initial-refresh", "")
	record.GrantedScopes = []string{"tweet.read", "users.read", "offline.access"}
	record.Expiry = now.Add(-time.Hour)
	require.NoError(manager.Save(t.Context(), record))
	credentials := manager.Credentials("100", f.server.Client())
	f.api.credentials = credentials
	f.api.api = NewClient(f.server.URL, credentials.HTTPClient())
	f.api.policy.MonthlyBudgetMicros = 1
	_, err := f.api.Timeline(t.Context(), TimelineRequest{OwnerID: "100", MaxResults: 5})
	require.ErrorIs(err, store.ErrXBudgetExceeded)
	refreshed, err := manager.Load(t.Context(), "100")
	require.NoError(err)
	assert.Equal(int64(2), refreshed.Generation)
	assert.Equal(refreshed.Generation, f.state(t).Generation)
	now = now.Add(24 * time.Hour)
	_, err = f.api.Timeline(t.Context(), TimelineRequest{OwnerID: "100", MaxResults: 5})
	require.Error(err)
	assert.Equal("invalid_grant", f.state(t).CredentialError)
	assert.Zero(resourceCalls.Load(), "denied admission must not call the provider resource")
	assert.Equal(int32(2), tokenCalls.Load())
}

func FuzzXProtectedAuthorObservationOrder(f *testing.F) {
	f.Add(int64(-3600))
	f.Add(int64(0))
	f.Add(int64(3600))
	f.Fuzz(func(t *testing.T, offset int64) {
		assert := assert.New(t)
		require := require.New(t)
		st := testutil.NewTestStore(t)
		observed, err := st.GetOrCreateSource("x", "1001")
		require.NoError(err)
		copied, err := st.GetOrCreateSource("x", "1003")
		require.NoError(err)
		at := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
		post := Post{ID: "100", AuthorID: "1002", ConversationID: "100", Text: "public observation", CreatedAt: at.Add(-48 * time.Hour), EditHistory: []string{"100"}}
		user := User{ID: "1002", Username: "example_author", ProtectedKnown: true}
		require.NoError(WritePage(t.Context(), st, copied.ID, "1003", Page{Posts: []Post{post}, Users: []User{user}, FetchedAt: at}, at))
		policy, err := config.NewDefaultConfig().XAccount("1001")
		require.NoError(err)
		imp := NewImporter(st, nil, policy, nil)
		offset %= 86400
		page := Page{Posts: []Post{post}, Users: []User{{ID: "1002", Protected: true, ProtectedKnown: true}}, FetchedAt: at.Add(time.Duration(offset) * time.Second)}
		require.NoError(imp.persistPage(t.Context(), &ImportSummary{SourceID: observed.ID}, "1001", &store.XWalk{}, page))
		pending, err := st.XCleanupPendingContext(t.Context())
		require.NoError(err)
		assert.Equal(offset >= 0, pending, "original observation time decides protection precedence")
	})
}
