package x

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestXRefreshStatusReadsStoredCoverageWithoutCredentials(t *testing.T) {
	_testAssert := assert.New(t)
	_testRequire := require.New(t)

	st := testutil.NewTestStore(t)
	source := importTestSource(t, st)
	at := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	_, err := st.PersistXPostContext(t.Context(), store.XPersistPostInput{
		SourceID: source.ID, OwnerID: "1001", PostID: "100", AuthorID: "1002", ConversationID: "100",
		EditHistory: []string{"100"}, Text: "synthetic stored coverage", CreatedAt: at, FetchedAt: at, Availability: "public", Author: &store.XAuthorInput{UserID: "1002", FetchedAt: at},
	})
	_testRequire.NoError(err)
	state, err := st.LoadXControlContext(t.Context(), source.ID)
	_testRequire.NoError(err)
	credential, model := "invalid_grant", store.XModelErrorResourceShape
	refresh := store.XRefreshState{LastCompleteAt: at}
	_, err = st.UpdateXControlContext(t.Context(), source.ID, state.Revision, store.XControlPatch{OwnerID: "1001", CredentialError: &credential, ModelError: &model, Refresh: &refresh})
	_testRequire.NoError(err)
	before, err := st.LoadXControlContext(t.Context(), source.ID)
	_testRequire.NoError(err)
	// The reader takes no API, credential or importer. Its clock is explicit.
	status, err := ReadRefreshStatus(t.Context(), st, source.ID, at.Add(48*time.Hour), 24*time.Hour)
	_testRequire.NoError(err)
	_testAssert.Equal(int64(1), status.Outstanding)
	_testAssert.Equal(at, status.OldestUncheckedAt.UTC())
	_testAssert.Equal(at, status.LastCompleteAt.UTC())
	_testAssert.True(status.Overdue)
	_testAssert.False(status.PendingRemoval)
	fresh, err := ReadRefreshStatus(t.Context(), st, source.ID, at.Add(time.Hour), 24*time.Hour)
	_testRequire.NoError(err)
	_testAssert.Zero(fresh.Outstanding)
	_testAssert.False(fresh.Overdue)
	after, err := st.LoadXControlContext(t.Context(), source.ID)
	_testRequire.NoError(err)
	_testAssert.Equal(before, after, "status must not mutate holds, spending, history or refresh state")
}

func TestXImportRefreshesOldMetadataBeforeCollection(t *testing.T) {
	_testAssert := assert.New(t)
	_testRequire := require.New(t)

	st := testutil.NewTestStore(t)
	source := importTestSource(t, st)
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	old := now.Add(-48 * time.Hour)
	_, err := st.PersistXPostContext(t.Context(), store.XPersistPostInput{SourceID: source.ID, OwnerID: "1001", PostID: "100", AuthorID: "1001", ConversationID: "100", EditHistory: []string{"100"}, Text: "old synthetic content", CreatedAt: old, FetchedAt: old, Availability: "public", Author: &store.XAuthorInput{UserID: "1001", FetchedAt: old}})
	_testRequire.NoError(err)
	var calls []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.URL.Path)
		if r.URL.Path == "/tweets" {
			wireImportPage(w, []string{wireOwnPost("100", old)}, "")
			return
		}
		wireImportPage(w, nil, "")
	}))
	defer server.Close()
	creds := &importCredentials{snapshot: CredentialSnapshot{OwnerID: "1001", Generation: 1, GrantedScopes: []string{"tweet.read", "users.read"}}}
	imp := NewImporter(st, NewClient(server.URL, server.Client()), importTestPolicy(t), creds)
	imp.now = func() time.Time { return now }
	_, err = imp.Import(t.Context(), ImportOptions{SourceID: source.ID, OwnerID: "1001"})
	_testRequire.NoError(err)
	_testRequire.NotEmpty(calls)
	_testAssert.Equal("/tweets", calls[0], "all-age refresh precedes collection")
	state, err := st.LoadXControlContext(t.Context(), source.ID)
	_testRequire.NoError(err)
	_testAssert.Equal(now, state.Refresh.LastCompleteAt.UTC())
	calls = nil
	_, err = imp.Import(t.Context(), ImportOptions{SourceID: source.ID, OwnerID: "1001"})
	_testRequire.NoError(err)
	_testAssert.NotContains(calls, "/tweets", "fresh metadata does not consume another lookup")
}

func TestXRefreshInvalidatesOnlyBudgetSelectedIDsBeforeIO(t *testing.T) {
	_testAssert := assert.New(t)
	_testRequire := require.New(t)

	st := testutil.NewTestStore(t)
	source := importTestSource(t, st)
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	for _, id := range []string{"100", "101"} {
		_, err := st.PersistXPostContext(t.Context(), store.XPersistPostInput{SourceID: source.ID, OwnerID: "1001", PostID: id, AuthorID: "1002", ConversationID: id, EditHistory: []string{id}, Text: "retained content", CreatedAt: now.Add(-48 * time.Hour), FetchedAt: now.Add(-48 * time.Hour), Availability: "public", Author: &store.XAuthorInput{UserID: "1002", FetchedAt: now.Add(-48 * time.Hour)}})
		_testRequire.NoError(err)
	}
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		assert.Equal(t, "100", r.URL.Query().Get("ids"))
		for _, id := range []string{"100", "101"} {
			target, err := st.ResolveXPostContext(t.Context(), source.ID, id)
			if !assert.NoError(t, err) {
				return
			}
			want := "public"
			if id == "100" {
				want = "unverified"
			}
			assert.Equal(t, want, target.Availability, "metadata must be durable before provider I/O")
		}
		w.WriteHeader(http.StatusForbidden)
		_, _ = fmt.Fprint(w, `{"title":"unavailable"}`)
	}))
	defer server.Close()
	policy := importTestPolicy(t)
	policy.MonthlyBudgetMicros = 15000
	creds := &importCredentials{snapshot: CredentialSnapshot{OwnerID: "1001", Generation: 1, GrantedScopes: []string{"tweet.read", "users.read"}}}
	imp := NewImporter(st, NewClient(server.URL, server.Client()), policy, creds)
	imp.now = func() time.Time { return now }
	summary, err := imp.Refresh(t.Context(), RefreshOptions{SourceID: source.ID, OwnerID: "1001", BatchSize: 100})
	_testRequire.Error(err)
	_testAssert.Equal(int64(1), calls.Load())
	_testAssert.Equal(int64(2), summary.Outstanding)
	_testAssert.True(summary.Overdue, "a stopped sweep reports the same overdue age as read-only status")
	_testAssert.True(summary.LastCompleteAt.IsZero())
	state, err := st.LoadXControlContext(t.Context(), source.ID)
	_testRequire.NoError(err)
	_testAssert.Equal(now, state.Refresh.SweepStartedAt.UTC())
	_testAssert.Empty(state.Refresh.Cursor)
}

func TestXRefreshReopensFrozenSweepWithoutSkippingOrRepeatingCheckedIDs(t *testing.T) {
	_testAssert := assert.New(t)
	_testRequire := require.New(t)

	path := filepath.Join(t.TempDir(), "archive.db")
	st := testutil.NewSQLiteTestStoreAt(t, path)
	source := importTestSource(t, st)
	now := time.Now().UTC().Add(-time.Hour)
	old := now.Add(-48 * time.Hour)
	for _, id := range []string{"100", "200"} {
		_, err := st.PersistXPostContext(t.Context(), store.XPersistPostInput{SourceID: source.ID, OwnerID: "1001", PostID: id, AuthorID: "1001", ConversationID: id, EditHistory: []string{id}, Text: "old synthetic content", CreatedAt: old, FetchedAt: old, Availability: "public", Author: &store.XAuthorInput{UserID: "1001", FetchedAt: old}})
		_testRequire.NoError(err)
	}
	var fail atomic.Bool
	fail.Store(true)
	var first, second atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.URL.Query().Get("ids")
		if id == "100" {
			first.Add(1)
		} else {
			assert.Equal(t, "200", id)
			second.Add(1)
		}
		if id == "200" && fail.Load() {
			w.WriteHeader(http.StatusForbidden)
			_, _ = fmt.Fprint(w, `{"title":"synthetic interruption"}`)
			return
		}
		wireImportPage(w, []string{wireOwnPost(id, old)}, "")
	}))
	defer provider.Close()
	creds := &importCredentials{snapshot: CredentialSnapshot{OwnerID: "1001", Generation: 1, GrantedScopes: []string{"tweet.read", "users.read"}}}
	imp := NewImporter(st, NewClient(provider.URL, provider.Client()), importTestPolicy(t), creds)
	imp.now = func() time.Time { return now }
	partial, err := imp.Refresh(t.Context(), RefreshOptions{SourceID: source.ID, OwnerID: "1001", BatchSize: 1})
	_testRequire.Error(err)
	_testAssert.Equal(int64(1), partial.Outstanding)
	state, err := st.LoadXControlContext(t.Context(), source.ID)
	_testRequire.NoError(err)
	_testAssert.Equal("100", state.Refresh.Cursor)
	_testAssert.Equal(now, state.Refresh.SweepStartedAt.UTC())
	_testAssert.True(state.Refresh.LastCompleteAt.IsZero())
	_testRequire.NoError(st.Close())
	reopened, err := store.Open(path)
	_testRequire.NoError(err)
	t.Cleanup(func() { assert.NoError(t, reopened.Close()) })
	persisted, err := reopened.LoadXControlContext(t.Context(), source.ID)
	_testRequire.NoError(err)
	_testAssert.Equal(state, persisted)
	fail.Store(false)
	resumed := NewImporter(reopened, NewClient(provider.URL, provider.Client()), importTestPolicy(t), creds)
	resumed.now = func() time.Time { return now.Add(time.Hour) }
	complete, err := resumed.Refresh(t.Context(), RefreshOptions{SourceID: source.ID, OwnerID: "1001", BatchSize: 1})
	_testRequire.NoError(err)
	_testAssert.True(complete.Complete)
	_testAssert.Zero(complete.Outstanding)
	_testAssert.Equal(int32(1), first.Load(), "already-checked ID must not be replayed")
	_testAssert.Equal(int32(2), second.Load(), "the unresolved ID remains authoritative across reopen")
	final, err := reopened.LoadXControlContext(t.Context(), source.ID)
	_testRequire.NoError(err)
	_testAssert.Empty(final.Refresh.Cursor)
	_testAssert.True(final.Refresh.SweepStartedAt.IsZero())
	_testAssert.Equal(now.Add(time.Hour), final.Refresh.LastCompleteAt.UTC())
}

func TestXRefreshCompletionRequiresDurableCheckpoint(t *testing.T) {
	_testAssert := assert.New(t)
	_testRequire := require.New(t)

	st := testutil.NewSQLiteTestStoreAt(t, filepath.Join(t.TempDir(), "archive.db"))
	source := importTestSource(t, st)
	// Fail the actual Store checkpoint publication, after the empty sweep has
	// done all of its local work. No provider or replacement Store is involved.
	_, err := st.DB().Exec(`CREATE TRIGGER reject_x_refresh_completion BEFORE UPDATE OF sync_cursor ON sources WHEN json_type(NEW.sync_cursor,'$.refresh.last_complete_at') IS NOT NULL BEGIN SELECT RAISE(ABORT,'synthetic refresh checkpoint failure'); END`)
	_testRequire.NoError(err)
	policy := importTestPolicy(t)
	policy.MonthlyBudgetMicros = 0
	imp := NewImporter(st, nil, policy, nil)
	result, err := imp.Refresh(t.Context(), RefreshOptions{SourceID: source.ID, OwnerID: "1001"})
	_testRequire.ErrorContains(err, "synthetic refresh checkpoint failure")
	_testAssert.False(result.Complete, "a rejected durable checkpoint cannot report a complete sweep")
	_testAssert.True(result.LastCompleteAt.IsZero())
	state, err := st.LoadXControlContext(t.Context(), source.ID)
	_testRequire.NoError(err)
	_testAssert.True(state.Refresh.LastCompleteAt.IsZero())
	_testAssert.False(state.Refresh.SweepStartedAt.IsZero())
	_, err = st.DB().Exec(`DROP TRIGGER reject_x_refresh_completion`)
	_testRequire.NoError(err)
	result, err = imp.Refresh(t.Context(), RefreshOptions{SourceID: source.ID, OwnerID: "1001"})
	_testRequire.NoError(err)
	_testAssert.True(result.Complete)
	state, err = st.LoadXControlContext(t.Context(), source.ID)
	_testRequire.NoError(err)
	_testAssert.Equal(state.Refresh.LastCompleteAt, result.LastCompleteAt)
	_testAssert.False(result.LastCompleteAt.IsZero())
}
