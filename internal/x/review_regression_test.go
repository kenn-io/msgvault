package x

import (
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

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
