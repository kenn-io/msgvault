package x

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

// Credentials are the already-tested lifecycle boundary. Pagination tests keep
// their verified snapshot fixed and exercise real HTTP, admission and Store.
type importCredentials struct{ snapshot CredentialSnapshot }

func (c *importCredentials) Prepare(ctx context.Context) (CredentialSnapshot, error) {
	return c.snapshot, ctx.Err()
}
func (c *importCredentials) Refresh(ctx context.Context, rejected int64) (CredentialSnapshot, error) {
	return c.snapshot, ctx.Err()
}
func (c *importCredentials) HTTPClient() *http.Client { return http.DefaultClient }

func importTestPolicy(t *testing.T) config.XAccountConfig {
	t.Helper()
	policy, err := config.NewDefaultConfig().XAccount("1001")
	require.NoError(t, err)
	policy.MonthlyBudgetMicros = 100_000_000
	return policy
}

func importTestSource(t *testing.T, st *store.Store) *store.Source {
	t.Helper()
	source, err := st.GetOrCreateSource("x", "1001")
	require.NoError(t, err)
	generation := int64(1)
	_, err = st.UpdateXControlContext(t.Context(), source.ID, 0, store.XControlPatch{OwnerID: "1001", Generation: &generation})
	require.NoError(t, err)
	return source
}

func wireOwnPost(id string, at time.Time) string {
	return fmt.Sprintf(`{"id":%q,"author_id":"1001","conversation_id":%q,"text":%q,"created_at":%q,"edit_history_tweet_ids":[%q]}`, id, id, "Synthetic post "+id, at.Format(time.RFC3339Nano), id)
}

func wireImportPage(w http.ResponseWriter, posts []string, next string) {
	w.Header().Set("Content-Type", "application/json")
	if len(posts) == 0 {
		_, _ = fmt.Fprintf(w, `{"data":[],"meta":{"result_count":0,"next_token":%q}}`, next)
		return
	}
	_, _ = fmt.Fprintf(w, `{"data":[%s],"includes":{"users":[{"id":"1001","name":"Example Owner","username":"example_owner","protected":false}]},"meta":{"result_count":%d,"next_token":%q}}`, strings.Join(posts, ","), len(posts), next)
}

func archivedXIDs(t *testing.T, st *store.Store, sourceID int64) []string {
	t.Helper()
	rows, err := st.DB().Query(st.Rebind(`SELECT source_message_id FROM messages WHERE source_id=? ORDER BY source_message_id`), sourceID)
	require.NoError(t, err)
	defer func() { assert.NoError(t, rows.Close()) }()
	var ids []string
	for rows.Next() {
		var id string
		require.NoError(t, rows.Scan(&id))
		ids = append(ids, id)
	}
	require.NoError(t, rows.Err())
	return ids
}

func TestXInterruptedWindowDoesNotPublishNewestID(t *testing.T) {
	// A real close/reopen establishes restart durability, independently of an
	// importer object's memory. Other pagination cases use both Store backends.
	_testAssert := assert.New(t)
	_testRequire := require.New(t)

	dbPath := filepath.Join(t.TempDir(), "archive.db")
	st := testutil.NewSQLiteTestStoreAt(t, dbPath)
	source := importTestSource(t, st)
	now := time.Date(2026, 10, 30, 9, 0, 0, 0, time.UTC)
	var failSecond atomic.Bool
	failSecond.Store(true)
	var ownCalls atomic.Int64
	var activeStore atomic.Pointer[store.Store]
	activeStore.Store(st)
	var mu sync.Mutex
	var resumedBounds []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/mentions") {
			wireImportPage(w, nil, "")
			return
		}
		if !assert.Equal(t, "/users/1001/tweets", r.URL.Path) {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		ownCalls.Add(1)
		if r.URL.Query().Get("start_time") != "" {
			// The separate mutable overlap must not broaden this completed
			// live window or inject above-ceiling arrivals into its watermark.
			wireImportPage(w, nil, "")
			return
		}
		control, err := activeStore.Load().LoadXControlContext(r.Context(), source.ID)
		if !assert.NoError(t, err) || !assert.NotNil(t, control.Own) {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		assert.NotEmpty(t, control.Reservations, "actual HTTP must follow committed admission")
		assert.Equal(t, now, control.Own.Before)
		mu.Lock()
		resumedBounds = append(resumedBounds, r.URL.Query().Get("end_time"))
		mu.Unlock()
		switch r.URL.Query().Get("pagination_token") {
		case "":
			child := fmt.Sprintf(`{"id":"110","author_id":"1001","conversation_id":"109","text":"Synthetic child","created_at":%q,"edit_history_tweet_ids":["110"],"referenced_tweets":[{"type":"replied_to","id":"109"}]}`, now.Add(-time.Minute).Format(time.RFC3339Nano))
			wireImportPage(w, []string{child}, "page-two")
		case "page-two":
			assert.Equal(t, "page-two", control.Own.Token)
			assert.Empty(t, control.Own.Watermark)
			if failSecond.Load() {
				w.WriteHeader(http.StatusForbidden)
				_, _ = fmt.Fprint(w, `{"title":"Synthetic endpoint failure"}`)
				return
			}
			wireImportPage(w, []string{wireOwnPost("109", now.Add(-2*time.Minute))}, "page-three")
		case "page-three":
			wireImportPage(w, []string{wireOwnPost("108", now.Add(-3*time.Minute)), wireOwnPost("120", now.Add(time.Minute))}, "")
		default:
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	defer server.Close()
	credentials := &importCredentials{snapshot: CredentialSnapshot{OwnerID: "1001", Generation: 1, GrantedScopes: []string{"tweet.read", "users.read"}}}
	policy := importTestPolicy(t)
	first := NewImporter(st, NewClient(server.URL, server.Client()), policy, credentials)
	first.now = func() time.Time { return now }
	summary, err := first.Import(t.Context(), ImportOptions{SourceID: source.ID, OwnerID: "1001"})
	_testRequire.Error(err)
	_testRequire.NotNil(summary)
	_testAssert.False(summary.OwnComplete)
	control, err := st.LoadXControlContext(t.Context(), source.ID)
	_testRequire.NoError(err)
	_testRequire.NotNil(control.Own)
	_testAssert.Empty(control.Own.Watermark)
	_testAssert.Equal("page-two", control.Own.Token)
	_testAssert.Equal([]string{"110"}, archivedXIDs(t, st, source.ID))
	_testRequire.NoError(st.Close())
	st, err = store.Open(dbPath)
	_testRequire.NoError(err)
	activeStore.Store(st)
	t.Cleanup(func() { assert.NoError(t, st.Close()) })
	failSecond.Store(false)
	resumed := NewImporter(st, NewClient(server.URL, server.Client()), policy, credentials)
	resumed.now = func() time.Time { return now.Add(2 * time.Hour) }
	summary, err = resumed.Import(t.Context(), ImportOptions{SourceID: source.ID, OwnerID: "1001"})
	_testRequire.NoError(err)
	_testRequire.NotNil(summary)
	_testAssert.True(summary.OwnComplete)
	_testAssert.True(summary.MentionsComplete)
	_testAssert.Equal(int64(2), summary.MessagesAdded)
	_testAssert.Equal([]string{"108", "109", "110"}, archivedXIDs(t, st, source.ID))
	root, err := st.ResolveXPostContext(t.Context(), source.ID, "109")
	_testRequire.NoError(err)
	child, err := st.ResolveXPostContext(t.Context(), source.ID, "110")
	_testRequire.NoError(err)
	var parent sql.NullInt64
	_testRequire.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT reply_to_message_id FROM messages WHERE id=?`), child.MessageID).Scan(&parent))
	_testAssert.True(parent.Valid)
	_testAssert.Equal(root.MessageID, parent.Int64)
	control, err = st.LoadXControlContext(t.Context(), source.ID)
	_testRequire.NoError(err)
	_testAssert.Equal("110", control.Own.Watermark)
	_testAssert.True(control.Own.Complete)
	mu.Lock()
	for _, bound := range resumedBounds {
		_testAssert.Equal(now.Format(time.RFC3339Nano), bound)
	}
	mu.Unlock()
	_testAssert.GreaterOrEqual(ownCalls.Load(), int64(4))
}

func TestXImportRepairsParentsAfterInterruptedRun(t *testing.T) {
	st := testutil.NewTestStore(t)
	source := importTestSource(t, st)
	now := time.Date(2026, 10, 30, 9, 0, 0, 0, time.UTC)
	user := User{ID: "1001", Name: "Example Owner", Username: "example_owner", ProtectedKnown: true}
	posts := []Post{
		{ID: "110", AuthorID: "1001", ConversationID: "109", Text: "child", CreatedAt: now, EditHistory: []string{"110"}, References: []Reference{{Type: "replied_to", ID: "109"}}},
		{ID: "109", AuthorID: "1001", ConversationID: "109", Text: "parent", CreatedAt: now, EditHistory: []string{"109"}},
	}
	// Simulate interruption after both pages were persisted but before the final scan.
	for _, post := range posts {
		_, err := writePageWithSummary(t.Context(), st, source.ID, "1001", Page{Posts: []Post{post}, Users: []User{user}, FetchedAt: now}, now)
		require.NoError(t, err)
	}
	root, err := st.ResolveXPostContext(t.Context(), source.ID, "109")
	require.NoError(t, err)
	child, err := st.ResolveXPostContext(t.Context(), source.ID, "110")
	require.NoError(t, err)
	readParent := func() sql.NullInt64 {
		t.Helper()
		var parent sql.NullInt64
		require.NoError(t, st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT reply_to_message_id FROM messages WHERE id=?`), child.MessageID).Scan(&parent))
		return parent
	}
	assert.False(t, readParent().Valid)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer server.Close()
	credentials := &importCredentials{snapshot: CredentialSnapshot{OwnerID: "1001", Generation: 1, GrantedScopes: []string{"tweet.read", "users.read"}}}
	imp := NewImporter(st, NewClient(server.URL, server.Client()), importTestPolicy(t), credentials)
	imp.now = func() time.Time { return now }
	_, err = imp.Import(t.Context(), ImportOptions{SourceID: source.ID, OwnerID: "1001"})
	require.Error(t, err)
	parent := readParent()
	assert.True(t, parent.Valid)
	assert.Equal(t, root.MessageID, parent.Int64)
}

func TestXDelayedCrossSourceImportCannotOutrankProtectedObservation(t *testing.T) {
	_testAssert := assert.New(t)
	_testRequire := require.New(t)

	st := testutil.NewTestStore(t)
	protectedSource, err := st.GetOrCreateSource("x", "1001")
	_testRequire.NoError(err)
	protectedAt := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)

	publicOwner := "2001"
	publicSource, err := st.GetOrCreateSource("x", publicOwner)
	_testRequire.NoError(err)
	generation := int64(1)
	_, err = st.UpdateXControlContext(t.Context(), publicSource.ID, 0, store.XControlPatch{OwnerID: publicOwner, Generation: &generation})
	_testRequire.NoError(err)
	publicStart := protectedAt.Add(-time.Minute)
	var publicClock atomic.Int64
	publicClock.Store(publicStart.UnixNano())
	requestStarted := make(chan struct{})
	releaseResponse := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseResponse) }) }
	var once sync.Once
	publicServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/mentions") {
			wireImportPage(w, nil, "")
			return
		}
		if r.URL.Path != "/users/"+publicOwner+"/tweets" {
			http.NotFound(w, r)
			return
		}
		once.Do(func() { close(requestStarted) })
		select {
		case <-releaseResponse:
		case <-r.Context().Done():
			return
		}
		_, _ = fmt.Fprint(w, `{"data":[{"id":"7100","author_id":"3000","conversation_id":"7100","text":"stale delayed public copy","created_at":"2026-10-29T12:00:00Z","edit_history_tweet_ids":["7100"]}],"includes":{"users":[{"id":"3000","name":"Example Author","username":"example_author","protected":false}]},"meta":{"result_count":1}}`)
	}))
	defer func() {
		release()
		publicServer.Close()
	}()
	policy, err := config.NewDefaultConfig().XAccount(publicOwner)
	_testRequire.NoError(err)
	policy.MonthlyBudgetMicros = 100_000_000
	publicImporter := NewImporter(st, NewClient(publicServer.URL, publicServer.Client()), policy, &importCredentials{
		snapshot: CredentialSnapshot{OwnerID: publicOwner, Generation: 1, GrantedScopes: []string{"tweet.read", "users.read"}},
	})
	publicImporter.now = func() time.Time { return time.Unix(0, publicClock.Load()).UTC() }
	publicDone := make(chan error, 1)
	go func() {
		_, importErr := publicImporter.Import(t.Context(), ImportOptions{SourceID: publicSource.ID, OwnerID: publicOwner})
		publicDone <- importErr
	}()
	select {
	case <-requestStarted:
	case importErr := <-publicDone:
		_testRequire.NoError(importErr)
		_testRequire.FailNow("public import completed without issuing its timeline request")
	case <-time.After(10 * time.Second):
		release()
		_testRequire.FailNow("public timeline request did not start")
	}

	// The public response stays in flight while a second source records the
	// protected profile, then arrives with a later wall-clock completion time.
	_, err = RecordAuthor(t.Context(), st, protectedSource.ID, User{
		ID: "3000", Name: "Example Private Author", Username: "private_example",
		Protected: true, ProtectedKnown: true,
	}, protectedAt)
	_testRequire.NoError(err)
	publicClock.Store(protectedAt.Add(time.Minute).UnixNano())
	release()
	select {
	case importErr := <-publicDone:
		_testRequire.ErrorIs(importErr, ErrContentCleanupRequired)
	case <-time.After(10 * time.Second):
		_testRequire.FailNow("public import did not finish after releasing its response")
	}
	target, err := st.ResolveXPostContext(t.Context(), publicSource.ID, "7100")
	_testRequire.NoError(err)
	_testAssert.Equal("removed", target.Availability, "a response completing after protection cannot outrank a request that started before it")
	_testAssert.Zero(target.MessageID)
	var messages int
	_testRequire.NoError(st.DB().QueryRow("SELECT count(*) FROM messages").Scan(&messages))
	_testAssert.Zero(messages, "stale public text must not enter the archive")
	pending, err := st.XCleanupPendingContext(t.Context())
	_testRequire.NoError(err)
	_testAssert.True(pending, "the owner must complete durable cleanup before reads resume")
}

func TestXOwnCompletionSurvivesMentionsFailure(t *testing.T) {
	_testAssert := assert.New(t)
	_testRequire := require.New(t)

	st := testutil.NewTestStore(t)
	source := importTestSource(t, st)
	now := time.Date(2026, 10, 30, 9, 0, 0, 0, time.UTC)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/mentions") {
			w.WriteHeader(http.StatusForbidden)
			_, _ = fmt.Fprint(w, `{"title":"Synthetic endpoint permission"}`)
			return
		}
		wireImportPage(w, []string{wireOwnPost("110", now.Add(-time.Minute))}, "")
	}))
	defer server.Close()
	credentials := &importCredentials{snapshot: CredentialSnapshot{OwnerID: "1001", Generation: 1, GrantedScopes: []string{"tweet.read", "users.read"}}}
	imp := NewImporter(st, NewClient(server.URL, server.Client()), importTestPolicy(t), credentials)
	imp.now = func() time.Time { return now }
	summary, err := imp.Import(t.Context(), ImportOptions{SourceID: source.ID, OwnerID: "1001"})
	_testRequire.Error(err)
	_testRequire.NotNil(summary)
	_testAssert.True(summary.OwnComplete)
	_testAssert.False(summary.MentionsComplete)
	control, err := st.LoadXControlContext(t.Context(), source.ID)
	_testRequire.NoError(err)
	_testRequire.NotNil(control.Own)
	_testAssert.True(control.Own.Complete)
	_testAssert.Equal("110", control.Own.Watermark)
	_testAssert.Equal([]string{"110"}, archivedXIDs(t, st, source.ID))
}

func TestXPartialItemObligationsSurviveRestart(t *testing.T) {
	_testAssert := assert.New(t)
	_testRequire := require.New(t)

	st := testutil.NewTestStore(t)
	source := importTestSource(t, st)
	now := time.Date(2026, 10, 30, 9, 0, 0, 0, time.UTC)
	var lookupFails atomic.Bool
	lookupFails.Store(true)
	var livePages, lookups atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/users/1001/mentions":
			wireImportPage(w, nil, "")
		case "/tweets":
			lookups.Add(1)
			assert.Equal(t, "101", r.URL.Query().Get("ids"))
			if lookupFails.Load() {
				w.WriteHeader(http.StatusForbidden)
				_, _ = fmt.Fprint(w, `{"title":"Synthetic lookup unavailable"}`)
				return
			}
			wireImportPage(w, []string{wireOwnPost("101", now.Add(-2*time.Minute))}, "")
		case "/users/1001/tweets":
			if r.URL.Query().Get("start_time") != "" {
				wireImportPage(w, nil, "")
				return
			}
			livePages.Add(1)
			if r.URL.Query().Get("pagination_token") == "" {
				_, _ = fmt.Fprintf(w, `{"data":[%s],"includes":{"users":[{"id":"1001","username":"example_owner","protected":false}]},"errors":[{"resource_type":"tweet","resource_id":"101","title":"Synthetic transient item"}],"meta":{"next_token":"second"}}`, wireOwnPost("102", now.Add(-time.Minute)))
				return
			}
			assert.Equal(t, "second", r.URL.Query().Get("pagination_token"))
			wireImportPage(w, []string{wireOwnPost("100", now.Add(-3*time.Minute))}, "")
		default:
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	defer server.Close()
	credentials := &importCredentials{snapshot: CredentialSnapshot{OwnerID: "1001", Generation: 1, GrantedScopes: []string{"tweet.read", "users.read"}}}
	first := NewImporter(st, NewClient(server.URL, server.Client()), importTestPolicy(t), credentials)
	first.now = func() time.Time { return now }
	_, err := first.Import(t.Context(), ImportOptions{SourceID: source.ID, OwnerID: "1001"})
	_testRequire.Error(err)
	control, err := st.LoadXControlContext(t.Context(), source.ID)
	_testRequire.NoError(err)
	_testRequire.NotNil(control.Own)
	_testAssert.Empty(control.Own.Watermark)
	_testAssert.Equal([]string{"101"}, control.Own.UnresolvedIDs)
	_testAssert.False(control.Own.Complete)
	_testAssert.Equal(int64(2), livePages.Load())
	_testAssert.Equal([]string{"100", "102"}, archivedXIDs(t, st, source.ID))
	lookupFails.Store(false)
	restarted := NewImporter(st, NewClient(server.URL, server.Client()), importTestPolicy(t), credentials)
	restarted.now = func() time.Time { return now.Add(time.Hour) }
	summary, err := restarted.Import(t.Context(), ImportOptions{SourceID: source.ID, OwnerID: "1001"})
	_testRequire.NoError(err)
	_testAssert.True(summary.OwnComplete)
	_testAssert.Equal(int64(1), summary.MessagesAdded)
	_testAssert.Equal([]string{"100", "101", "102"}, archivedXIDs(t, st, source.ID))
	_testAssert.Equal(int64(2), livePages.Load(), "persisted pages must not be replayed to repair one known item")
	_testAssert.Equal(int64(2), lookups.Load())
	control, err = st.LoadXControlContext(t.Context(), source.ID)
	_testRequire.NoError(err)
	_testAssert.Empty(control.Own.UnresolvedIDs)
	_testAssert.Equal("102", control.Own.Watermark)
}

func TestXExpiredTokenReplaysIdenticalFrozenWindow(t *testing.T) {
	_testAssert := assert.New(t)
	_testRequire := require.New(t)

	st := testutil.NewTestStore(t)
	source := importTestSource(t, st)
	now := time.Date(2026, 10, 30, 9, 0, 0, 0, time.UTC)
	var firstCalls, expired atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/mentions") || r.URL.Query().Get("start_time") != "" {
			wireImportPage(w, nil, "")
			return
		}
		assert.Equal(t, now.Format(time.RFC3339), r.URL.Query().Get("end_time"))
		if r.URL.Query().Get("pagination_token") == "old-token" && expired.Add(1) == 1 {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = fmt.Fprint(w, `{"title":"Invalid Request","errors":[{"parameters":{"pagination_token":["old-token"]},"message":"Invalid pagination token"}]}`)
			return
		}
		if r.URL.Query().Get("pagination_token") == "" {
			firstCalls.Add(1)
			wireImportPage(w, []string{wireOwnPost("102", now.Add(-time.Minute))}, "old-token")
			return
		}
		wireImportPage(w, []string{wireOwnPost("101", now.Add(-2*time.Minute))}, "")
	}))
	defer server.Close()
	credentials := &importCredentials{snapshot: CredentialSnapshot{OwnerID: "1001", Generation: 1, GrantedScopes: []string{"tweet.read", "users.read"}}}
	imp := NewImporter(st, NewClient(server.URL, server.Client()), importTestPolicy(t), credentials)
	imp.now = func() time.Time { return now }
	summary, err := imp.Import(t.Context(), ImportOptions{SourceID: source.ID, OwnerID: "1001"})
	_testRequire.NoError(err)
	_testRequire.NotNil(summary)
	_testAssert.Equal(int64(2), firstCalls.Load())
	_testAssert.Equal(int64(2), summary.MessagesAdded)
	_testAssert.Equal([]string{"101", "102"}, archivedXIDs(t, st, source.ID))
	_testAssert.Equal("102", summary.OwnWatermark)
}

func TestXMalformedAndAnonymousPagesKeepRequestUnresolved(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		anonymous  bool
	}{
		{"malformed", `{"data":[{"id":"101","text":"missing required author and time"}]}`, false},
		{"anonymous", `{"data":[],"errors":[{"title":"Synthetic transient unidentified result"}],"meta":{"next_token":"unsafe-next"}}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_testAssert := assert.New(t)
			_testRequire := require.New(t)

			st := testutil.NewTestStore(t)
			source := importTestSource(t, st)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/mentions") {
					wireImportPage(w, nil, "")
					return
				}
				_, _ = fmt.Fprint(w, tc.body)
			}))
			defer server.Close()
			credentials := &importCredentials{snapshot: CredentialSnapshot{OwnerID: "1001", Generation: 1, GrantedScopes: []string{"tweet.read", "users.read"}}}
			_, err := NewImporter(st, NewClient(server.URL, server.Client()), importTestPolicy(t), credentials).Import(t.Context(), ImportOptions{SourceID: source.ID, OwnerID: "1001"})
			_testRequire.Error(err)
			state, err := st.LoadXControlContext(t.Context(), source.ID)
			_testRequire.NoError(err)
			_testRequire.NotNil(state.Own)
			_testAssert.False(state.Own.Complete)
			_testAssert.Empty(state.Own.Token)
			_testAssert.Empty(state.Own.Watermark)
			_testAssert.Equal(tc.anonymous, state.Own.UnresolvedPage)
		})
	}
}

func TestXPageBoundariesReplayWithoutSkipping(t *testing.T) {
	for _, boundary := range []string{"message", "settlement", "checkpoint"} {
		t.Run(boundary, func(t *testing.T) {
			_testAssert := assert.New(t)
			_testRequire := require.New(t)

			st := testutil.NewSQLiteTestStoreAt(t, filepath.Join(t.TempDir(), "archive.db"))
			source := importTestSource(t, st)
			now := time.Now().UTC().Truncate(time.Second)
			trigger := ""
			switch boundary {
			case "message":
				trigger = `CREATE TRIGGER x_fail BEFORE INSERT ON message_bodies WHEN EXISTS(SELECT 1 FROM messages WHERE id=NEW.message_id AND source_message_id='101') BEGIN SELECT RAISE(ABORT,'synthetic message failure'); END`
			case "settlement":
				trigger = `CREATE TRIGGER x_fail BEFORE UPDATE OF sync_cursor ON sources WHEN EXISTS(SELECT 1 FROM messages WHERE source_id=NEW.id) AND json_extract(NEW.sync_cursor,'$.spend_buckets[0].spent_micros')>0 BEGIN SELECT RAISE(ABORT,'synthetic settlement failure'); END`
			case "checkpoint":
				trigger = `CREATE TRIGGER x_fail BEFORE UPDATE OF cursor_before ON sync_runs WHEN EXISTS(SELECT 1 FROM messages WHERE source_id=NEW.source_id) AND NEW.status='running' BEGIN SELECT RAISE(ABORT,'synthetic checkpoint failure'); END`
			}
			_, err := st.DB().Exec(trigger)
			_testRequire.NoError(err)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/mentions") || r.URL.Query().Get("start_time") != "" {
					wireImportPage(w, nil, "")
					return
				}
				if r.URL.Query().Get("pagination_token") == "second" {
					wireImportPage(w, []string{wireOwnPost("100", now.Add(-3*time.Minute))}, "")
					return
				}
				wireImportPage(w, []string{wireOwnPost("102", now.Add(-time.Minute)), wireOwnPost("101", now.Add(-2*time.Minute))}, "second")
			}))
			defer server.Close()
			credentials := &importCredentials{snapshot: CredentialSnapshot{OwnerID: "1001", Generation: 1, GrantedScopes: []string{"tweet.read", "users.read"}}}
			makeImporter := func() *Importer {
				imp := NewImporter(st, NewClient(server.URL, server.Client()), importTestPolicy(t), credentials)
				imp.now = func() time.Time { return now }
				return imp
			}
			_, err = makeImporter().Import(t.Context(), ImportOptions{SourceID: source.ID, OwnerID: "1001"})
			_testRequire.Error(err)
			state, err := st.LoadXControlContext(t.Context(), source.ID)
			_testRequire.NoError(err)
			_testAssert.Empty(state.Own.Watermark)
			_testAssert.False(state.Own.Complete)
			_, err = st.DB().Exec(`DROP TRIGGER x_fail`)
			_testRequire.NoError(err)
			summary, err := makeImporter().Import(t.Context(), ImportOptions{SourceID: source.ID, OwnerID: "1001"})
			_testRequire.NoError(err)
			_testAssert.True(summary.OwnComplete)
			_testAssert.Equal("102", summary.OwnWatermark)
			_testAssert.Equal([]string{"100", "101", "102"}, archivedXIDs(t, st, source.ID))
		})
	}
}

func TestXEmptyTokenPageContinuesAndRepeatedTokenStops(t *testing.T) {
	for _, repeat := range []bool{false, true} {
		t.Run(strconv.FormatBool(repeat), func(t *testing.T) {
			_testAssert := assert.New(t)
			_testRequire := require.New(t)

			st := testutil.NewTestStore(t)
			source := importTestSource(t, st)
			now := time.Now().UTC().Truncate(time.Second)
			var calls atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/mentions") || r.URL.Query().Get("start_time") != "" {
					wireImportPage(w, nil, "")
					return
				}
				calls.Add(1)
				if r.URL.Query().Get("pagination_token") == "" {
					wireImportPage(w, nil, "second")
					return
				}
				next := ""
				if repeat {
					next = "second"
				}
				wireImportPage(w, []string{wireOwnPost("101", now.Add(-time.Minute))}, next)
			}))
			defer server.Close()
			credentials := &importCredentials{snapshot: CredentialSnapshot{OwnerID: "1001", Generation: 1, GrantedScopes: []string{"tweet.read", "users.read"}}}
			imp := NewImporter(st, NewClient(server.URL, server.Client()), importTestPolicy(t), credentials)
			imp.now = func() time.Time { return now }
			summary, err := imp.Import(t.Context(), ImportOptions{SourceID: source.ID, OwnerID: "1001"})
			if repeat {
				_testRequire.ErrorIs(err, ErrRepeatedPageToken)
				_testAssert.Empty(summary.OwnWatermark)
			} else {
				_testRequire.NoError(err)
				_testAssert.Equal("101", summary.OwnWatermark)
			}
			_testAssert.Equal(int64(2), calls.Load())
			_testAssert.Equal([]string{"101"}, archivedXIDs(t, st, source.ID))
		})
	}
}
func TestXCancellationPreservesCommittedPageAndExcludesConcurrentRun(t *testing.T) {
	_testAssert := assert.New(t)
	_testRequire := require.New(t)

	st := testutil.NewTestStore(t)
	source := importTestSource(t, st)
	now := time.Now().UTC().Truncate(time.Second)
	entered := make(chan struct{})
	var interrupted atomic.Bool
	interrupted.Store(true)
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if strings.HasSuffix(r.URL.Path, "/mentions") || r.URL.Query().Get("start_time") != "" {
			wireImportPage(w, nil, "")
			return
		}
		if r.URL.Query().Get("pagination_token") == "second" {
			if interrupted.Load() {
				close(entered)
				<-r.Context().Done()
				return
			}
			wireImportPage(w, []string{wireOwnPost("101", now.Add(-2*time.Minute))}, "")
			return
		}
		wireImportPage(w, []string{wireOwnPost("102", now.Add(-time.Minute))}, "second")
	}))
	defer server.Close()
	credentials := &importCredentials{snapshot: CredentialSnapshot{OwnerID: "1001", Generation: 1, GrantedScopes: []string{"tweet.read", "users.read"}}}
	makeImporter := func() *Importer {
		imp := NewImporter(st, NewClient(server.URL, server.Client()), importTestPolicy(t), credentials)
		imp.now = func() time.Time { return now }
		return imp
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	type result struct {
		summary *ImportSummary
		err     error
	}
	done := make(chan result, 1)
	go func() {
		summary, err := makeImporter().Import(ctx, ImportOptions{SourceID: source.ID, OwnerID: "1001"})
		done <- result{summary, err}
	}()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		_testRequire.FailNow("second page did not start")
	}
	before := calls.Load()
	_, err := makeImporter().Import(t.Context(), ImportOptions{SourceID: source.ID, OwnerID: "1001"})
	_testRequire.ErrorIs(err, store.ErrSyncAlreadyActive)
	_testAssert.Equal(before, calls.Load())
	cancel()
	select {
	case outcome := <-done:
		_testRequire.ErrorIs(outcome.err, context.Canceled)
		_testAssert.Equal("interrupted", outcome.summary.Code)
	case <-time.After(10 * time.Second):
		_testRequire.FailNow("cancelled importer did not stop")
	}
	state, err := st.LoadXControlContext(t.Context(), source.ID)
	_testRequire.NoError(err)
	_testAssert.Equal("second", state.Own.Token)
	_testAssert.Empty(state.Own.Watermark)
	_testAssert.Equal([]string{"102"}, archivedXIDs(t, st, source.ID))
	interrupted.Store(false)
	summary, err := makeImporter().Import(t.Context(), ImportOptions{SourceID: source.ID, OwnerID: "1001"})
	_testRequire.NoError(err)
	_testAssert.Equal("102", summary.OwnWatermark)
	_testAssert.Equal([]string{"101", "102"}, archivedXIDs(t, st, source.ID))
}

func TestXRevokedGenerationDoesNotReadProvider(t *testing.T) {
	_testAssert := assert.New(t)
	_testRequire := require.New(t)

	st := testutil.NewTestStore(t)
	source := importTestSource(t, st)
	generation := int64(2)
	state, err := st.LoadXControlContext(t.Context(), source.ID)
	_testRequire.NoError(err)
	_, err = st.UpdateXControlContext(t.Context(), source.ID, state.Revision, store.XControlPatch{OwnerID: "1001", Generation: &generation})
	_testRequire.NoError(err)
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); wireImportPage(w, nil, "") }))
	defer server.Close()
	credentials := &importCredentials{snapshot: CredentialSnapshot{OwnerID: "1001", Generation: 1, GrantedScopes: []string{"tweet.read", "users.read"}}}
	summary, err := NewImporter(st, NewClient(server.URL, server.Client()), importTestPolicy(t), credentials).Import(t.Context(), ImportOptions{SourceID: source.ID, OwnerID: "1001"})
	_testRequire.Error(err)
	_testAssert.Equal("credential_unavailable", summary.Code)
	_testAssert.Zero(calls.Load())
}

func TestXCreditsExhaustionStopsOtherEndpoint(t *testing.T) {
	_testAssert := assert.New(t)

	st := testutil.NewTestStore(t)
	source := importTestSource(t, st)
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusPaymentRequired)
		_, _ = fmt.Fprint(w, `{"title":"Synthetic credits exhausted"}`)
	}))
	defer server.Close()
	credentials := &importCredentials{snapshot: CredentialSnapshot{OwnerID: "1001", Generation: 1, GrantedScopes: []string{"tweet.read", "users.read"}}}
	summary, err := NewImporter(st, NewClient(server.URL, server.Client()), importTestPolicy(t), credentials).Import(t.Context(), ImportOptions{SourceID: source.ID, OwnerID: "1001"})
	require.Error(t, err)
	_testAssert.Equal("credits_exhausted", summary.Code)
	_testAssert.Equal(int64(1), calls.Load())
	_testAssert.False(summary.OwnComplete)
	_testAssert.False(summary.MentionsComplete)
}

func TestXImportReportsRunAndMonthlyEstimates(t *testing.T) {
	_testAssert := assert.New(t)
	_testRequire := require.New(t)

	st := testutil.NewTestStore(t)
	source := importTestSource(t, st)
	policy := importTestPolicy(t)
	cost := store.XRequestCost{Kind: store.XRequestLookup, OwnerID: "1001", Generation: 1, GrantedScopes: []string{"tweet.read", "users.read"}, MonthlyBudgetMicros: policy.MonthlyBudgetMicros, RequestedResults: 1, PostPriceMicros: 5000, UserPriceMicros: 10000, At: time.Now().UTC()}
	_, err := st.ReserveXRequestContext(t.Context(), source.ID, cost)
	_testRequire.NoError(err)
	known, err := st.ReserveXRequestContext(t.Context(), source.ID, cost)
	_testRequire.NoError(err)
	_testRequire.NoError(st.SettleXRequestContext(t.Context(), source.ID, known.ID, store.XResourceUsage{Posts: 1, Users: 1, Persisted: true, Complete: true}))
	now := time.Now().UTC().Truncate(time.Second)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/mentions") {
			w.WriteHeader(http.StatusForbidden)
			_, _ = fmt.Fprint(w, `{"title":"Synthetic endpoint rejection"}`)
			return
		}
		wireImportPage(w, []string{wireOwnPost("101", now.Add(-time.Minute))}, "")
	}))
	defer server.Close()
	credentials := &importCredentials{snapshot: CredentialSnapshot{OwnerID: "1001", Generation: 1, GrantedScopes: []string{"tweet.read", "users.read"}}}
	summary, err := NewImporter(st, NewClient(server.URL, server.Client()), policy, credentials).Import(t.Context(), ImportOptions{SourceID: source.ID, OwnerID: "1001"})
	_testRequire.Error(err)
	_testRequire.NotNil(summary)
	encoded, err := json.Marshal(summary)
	_testRequire.NoError(err)
	var result struct {
		RunEstimated   int64 `json:"run_estimated_micros"`
		RunReserved    int64 `json:"run_reserved_micros"`
		MonthEstimated int64 `json:"month_estimated_micros"`
		MonthReserved  int64 `json:"month_reserved_micros"`
	}
	_testRequire.NoError(json.Unmarshal(encoded, &result))
	_testAssert.Equal(int64(15_000), result.RunEstimated)
	_testAssert.Equal(int64(1_500_000), result.RunReserved)
	_testAssert.Equal(int64(30_000), result.MonthEstimated)
	_testAssert.Equal(int64(1_515_000), result.MonthReserved)
}

func TestXRunCostDeltasPreserveMonthsAndRejectLostAuthority(t *testing.T) {
	for _, tc := range []struct {
		name            string
		before, after   []store.XSpendBucket
		spent, reserved int64
		invalid         bool
	}{
		{name: "month rollover", before: []store.XSpendBucket{{Month: "2026-10", SpentMicros: 100, ReservedMicros: 200}}, after: []store.XSpendBucket{{Month: "2026-10", SpentMicros: 150, ReservedMicros: 250}, {Month: "2026-11", SpentMicros: 70, ReservedMicros: 80}}, spent: 120, reserved: 130},
		{name: "large historical amount", before: []store.XSpendBucket{{Month: "2026-10", SpentMicros: math.MaxInt64 - 10}}, after: []store.XSpendBucket{{Month: "2026-10", SpentMicros: math.MaxInt64 - 1}}, spent: 9},
		{name: "sum overflow", after: []store.XSpendBucket{{Month: "2026-10", SpentMicros: math.MaxInt64}, {Month: "2026-11", SpentMicros: 1}}, invalid: true},
		{name: "regressed amount", before: []store.XSpendBucket{{Month: "2026-10", SpentMicros: 3}}, after: []store.XSpendBucket{{Month: "2026-10", SpentMicros: 2}}, invalid: true},
		{name: "lost month", before: []store.XSpendBucket{{Month: "2026-10", ReservedMicros: 1}}, invalid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_testAssert := assert.New(t)

			spent, reserved, err := runCostDelta(tc.before, tc.after)
			if tc.invalid {
				_testAssert.ErrorIs(err, errRunCostEstimate)
				return
			}
			require.NoError(t, err)
			_testAssert.Equal(tc.spent, spent)
			_testAssert.Equal(tc.reserved, reserved)
		})
	}
}
