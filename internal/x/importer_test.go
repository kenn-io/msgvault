package x

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
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

func importTestPolicy(t *testing.T, owner string) config.XAccountConfig {
	t.Helper()
	policy, err := config.NewDefaultConfig().XAccount(owner)
	require.NoError(t, err)
	policy.MonthlyBudgetMicros = 100_000_000
	return policy
}

func importTestSource(t *testing.T, st *store.Store, owner string) *store.Source {
	t.Helper()
	source, err := st.GetOrCreateSource("x", owner)
	require.NoError(t, err)
	generation := int64(1)
	_, err = st.UpdateXControlContext(t.Context(), source.ID, 0, store.XControlPatch{OwnerID: owner, Generation: &generation})
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
	defer rows.Close()
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
	dbPath := filepath.Join(t.TempDir(), "archive.db")
	st := testutil.NewSQLiteTestStoreAt(t, dbPath)
	source := importTestSource(t, st, "1001")
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
			wireImportPage(w, []string{wireOwnPost("110", now.Add(-time.Minute))}, "page-two")
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
	policy := importTestPolicy(t, "1001")
	first := NewImporter(st, NewClient(server.URL, server.Client()), policy, credentials)
	first.now = func() time.Time { return now }
	summary, err := first.Import(t.Context(), ImportOptions{SourceID: source.ID, OwnerID: "1001"})
	require.Error(t, err)
	require.NotNil(t, summary)
	assert.False(t, summary.OwnComplete)
	control, err := st.LoadXControlContext(t.Context(), source.ID)
	require.NoError(t, err)
	require.NotNil(t, control.Own)
	assert.Empty(t, control.Own.Watermark)
	assert.Equal(t, "page-two", control.Own.Token)
	assert.Equal(t, []string{"110"}, archivedXIDs(t, st, source.ID))
	require.NoError(t, st.Close())
	st, err = store.Open(dbPath)
	require.NoError(t, err)
	activeStore.Store(st)
	t.Cleanup(func() { assert.NoError(t, st.Close()) })
	failSecond.Store(false)
	resumed := NewImporter(st, NewClient(server.URL, server.Client()), policy, credentials)
	resumed.now = func() time.Time { return now.Add(2 * time.Hour) }
	summary, err = resumed.Import(t.Context(), ImportOptions{SourceID: source.ID, OwnerID: "1001"})
	require.NoError(t, err)
	require.NotNil(t, summary)
	assert.True(t, summary.OwnComplete)
	assert.True(t, summary.MentionsComplete)
	assert.Equal(t, int64(2), summary.MessagesAdded)
	assert.Equal(t, []string{"108", "109", "110"}, archivedXIDs(t, st, source.ID))
	control, err = st.LoadXControlContext(t.Context(), source.ID)
	require.NoError(t, err)
	assert.Equal(t, "110", control.Own.Watermark)
	assert.True(t, control.Own.Complete)
	mu.Lock()
	for _, bound := range resumedBounds {
		assert.Equal(t, now.Format(time.RFC3339Nano), bound)
	}
	mu.Unlock()
	assert.GreaterOrEqual(t, ownCalls.Load(), int64(4))
}

func TestXOwnCompletionSurvivesMentionsFailure(t *testing.T) {
	st := testutil.NewTestStore(t)
	source := importTestSource(t, st, "1001")
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
	imp := NewImporter(st, NewClient(server.URL, server.Client()), importTestPolicy(t, "1001"), credentials)
	imp.now = func() time.Time { return now }
	summary, err := imp.Import(t.Context(), ImportOptions{SourceID: source.ID, OwnerID: "1001"})
	require.Error(t, err)
	require.NotNil(t, summary)
	assert.True(t, summary.OwnComplete)
	assert.False(t, summary.MentionsComplete)
	control, err := st.LoadXControlContext(t.Context(), source.ID)
	require.NoError(t, err)
	require.NotNil(t, control.Own)
	assert.True(t, control.Own.Complete)
	assert.Equal(t, "110", control.Own.Watermark)
	assert.Equal(t, []string{"110"}, archivedXIDs(t, st, source.ID))
}

func TestXPartialItemObligationsSurviveRestart(t *testing.T) {
	st := testutil.NewTestStore(t)
	source := importTestSource(t, st, "1001")
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
				_, _ = fmt.Fprintf(w, `{"data":[%s],"includes":{"users":[{"id":"1001","username":"example_owner","protected":false}]},"errors":[{"resource_id":"101","title":"Synthetic transient item"}],"meta":{"next_token":"second"}}`, wireOwnPost("102", now.Add(-time.Minute)))
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
	first := NewImporter(st, NewClient(server.URL, server.Client()), importTestPolicy(t, "1001"), credentials)
	first.now = func() time.Time { return now }
	_, err := first.Import(t.Context(), ImportOptions{SourceID: source.ID, OwnerID: "1001"})
	require.Error(t, err)
	control, err := st.LoadXControlContext(t.Context(), source.ID)
	require.NoError(t, err)
	require.NotNil(t, control.Own)
	assert.Empty(t, control.Own.Watermark)
	assert.Equal(t, []string{"101"}, control.Own.UnresolvedIDs)
	assert.False(t, control.Own.Complete)
	assert.Equal(t, int64(2), livePages.Load())
	assert.Equal(t, []string{"100", "102"}, archivedXIDs(t, st, source.ID))
	lookupFails.Store(false)
	restarted := NewImporter(st, NewClient(server.URL, server.Client()), importTestPolicy(t, "1001"), credentials)
	restarted.now = func() time.Time { return now.Add(time.Hour) }
	summary, err := restarted.Import(t.Context(), ImportOptions{SourceID: source.ID, OwnerID: "1001"})
	require.NoError(t, err)
	assert.True(t, summary.OwnComplete)
	assert.Equal(t, int64(1), summary.MessagesAdded)
	assert.Equal(t, []string{"100", "101", "102"}, archivedXIDs(t, st, source.ID))
	assert.Equal(t, int64(2), livePages.Load(), "persisted pages must not be replayed to repair one known item")
	assert.Equal(t, int64(2), lookups.Load())
	control, err = st.LoadXControlContext(t.Context(), source.ID)
	require.NoError(t, err)
	assert.Empty(t, control.Own.UnresolvedIDs)
	assert.Equal(t, "102", control.Own.Watermark)
}

func TestXExpiredTokenReplaysIdenticalFrozenWindow(t *testing.T) {
	st := testutil.NewTestStore(t)
	source := importTestSource(t, st, "1001")
	now := time.Date(2026, 10, 30, 9, 0, 0, 0, time.UTC)
	var firstCalls, expired atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/mentions") || r.URL.Query().Get("start_time") != "" {
			wireImportPage(w, nil, "")
			return
		}
		assert.Equal(t, now.Format(time.RFC3339), r.URL.Query().Get("end_time"))
		if r.URL.Query().Get("pagination_token") == "old-token" && expired.Add(1) == 1 {
			w.WriteHeader(400)
			fmt.Fprint(w, `{"title":"Invalid Request","errors":[{"parameters":{"pagination_token":["old-token"]},"message":"Invalid pagination token"}]}`)
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
	imp := NewImporter(st, NewClient(server.URL, server.Client()), importTestPolicy(t, "1001"), credentials)
	imp.now = func() time.Time { return now }
	summary, err := imp.Import(t.Context(), ImportOptions{SourceID: source.ID, OwnerID: "1001"})
	require.NoError(t, err)
	require.NotNil(t, summary)
	assert.Equal(t, int64(2), firstCalls.Load())
	assert.Equal(t, int64(2), summary.MessagesAdded)
	assert.Equal(t, []string{"101", "102"}, archivedXIDs(t, st, source.ID))
	assert.Equal(t, "102", summary.OwnWatermark)
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
			st := testutil.NewTestStore(t)
			source := importTestSource(t, st, "1001")
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/mentions") {
					wireImportPage(w, nil, "")
					return
				}
				fmt.Fprint(w, tc.body)
			}))
			defer server.Close()
			credentials := &importCredentials{snapshot: CredentialSnapshot{OwnerID: "1001", Generation: 1, GrantedScopes: []string{"tweet.read", "users.read"}}}
			_, err := NewImporter(st, NewClient(server.URL, server.Client()), importTestPolicy(t, "1001"), credentials).Import(t.Context(), ImportOptions{SourceID: source.ID, OwnerID: "1001"})
			require.Error(t, err)
			state, err := st.LoadXControlContext(t.Context(), source.ID)
			require.NoError(t, err)
			require.NotNil(t, state.Own)
			assert.False(t, state.Own.Complete)
			assert.Empty(t, state.Own.Token)
			assert.Empty(t, state.Own.Watermark)
			assert.Equal(t, tc.anonymous, state.Own.UnresolvedPage)
		})
	}
}

func TestXPageBoundariesReplayWithoutSkipping(t *testing.T) {
	for _, boundary := range []string{"message", "settlement", "checkpoint"} {
		t.Run(boundary, func(t *testing.T) {
			st := testutil.NewSQLiteTestStoreAt(t, filepath.Join(t.TempDir(), "archive.db"))
			source := importTestSource(t, st, "1001")
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
			require.NoError(t, err)
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
				imp := NewImporter(st, NewClient(server.URL, server.Client()), importTestPolicy(t, "1001"), credentials)
				imp.now = func() time.Time { return now }
				return imp
			}
			_, err = makeImporter().Import(t.Context(), ImportOptions{SourceID: source.ID, OwnerID: "1001"})
			require.Error(t, err)
			state, err := st.LoadXControlContext(t.Context(), source.ID)
			require.NoError(t, err)
			assert.Empty(t, state.Own.Watermark)
			assert.False(t, state.Own.Complete)
			_, err = st.DB().Exec(`DROP TRIGGER x_fail`)
			require.NoError(t, err)
			summary, err := makeImporter().Import(t.Context(), ImportOptions{SourceID: source.ID, OwnerID: "1001"})
			require.NoError(t, err)
			assert.True(t, summary.OwnComplete)
			assert.Equal(t, "102", summary.OwnWatermark)
			assert.Equal(t, []string{"100", "101", "102"}, archivedXIDs(t, st, source.ID))
		})
	}
}

func TestXEmptyTokenPageContinuesAndRepeatedTokenStops(t *testing.T) {
	for _, repeat := range []bool{false, true} {
		t.Run(fmt.Sprint(repeat), func(t *testing.T) {
			st := testutil.NewTestStore(t)
			source := importTestSource(t, st, "1001")
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
			imp := NewImporter(st, NewClient(server.URL, server.Client()), importTestPolicy(t, "1001"), credentials)
			imp.now = func() time.Time { return now }
			summary, err := imp.Import(t.Context(), ImportOptions{SourceID: source.ID, OwnerID: "1001"})
			if repeat {
				require.ErrorIs(t, err, ErrRepeatedPageToken)
				assert.Empty(t, summary.OwnWatermark)
			} else {
				require.NoError(t, err)
				assert.Equal(t, "101", summary.OwnWatermark)
			}
			assert.Equal(t, int64(2), calls.Load())
			assert.Equal(t, []string{"101"}, archivedXIDs(t, st, source.ID))
		})
	}
}
func TestXCancellationPreservesCommittedPageAndExcludesConcurrentRun(t *testing.T) {
	st := testutil.NewTestStore(t)
	source := importTestSource(t, st, "1001")
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
		imp := NewImporter(st, NewClient(server.URL, server.Client()), importTestPolicy(t, "1001"), credentials)
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
		require.FailNow(t, "second page did not start")
	}
	before := calls.Load()
	_, err := makeImporter().Import(t.Context(), ImportOptions{SourceID: source.ID, OwnerID: "1001"})
	require.ErrorIs(t, err, store.ErrSyncAlreadyActive)
	assert.Equal(t, before, calls.Load())
	cancel()
	select {
	case outcome := <-done:
		require.ErrorIs(t, outcome.err, context.Canceled)
		assert.Equal(t, "interrupted", outcome.summary.Code)
	case <-time.After(10 * time.Second):
		require.FailNow(t, "cancelled importer did not stop")
	}
	state, err := st.LoadXControlContext(t.Context(), source.ID)
	require.NoError(t, err)
	assert.Equal(t, "second", state.Own.Token)
	assert.Empty(t, state.Own.Watermark)
	assert.Equal(t, []string{"102"}, archivedXIDs(t, st, source.ID))
	interrupted.Store(false)
	summary, err := makeImporter().Import(t.Context(), ImportOptions{SourceID: source.ID, OwnerID: "1001"})
	require.NoError(t, err)
	assert.Equal(t, "102", summary.OwnWatermark)
	assert.Equal(t, []string{"101", "102"}, archivedXIDs(t, st, source.ID))
}

func TestXRevokedGenerationDoesNotReadProvider(t *testing.T) {
	st := testutil.NewTestStore(t)
	source := importTestSource(t, st, "1001")
	generation := int64(2)
	state, err := st.LoadXControlContext(t.Context(), source.ID)
	require.NoError(t, err)
	_, err = st.UpdateXControlContext(t.Context(), source.ID, state.Revision, store.XControlPatch{OwnerID: "1001", Generation: &generation})
	require.NoError(t, err)
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); wireImportPage(w, nil, "") }))
	defer server.Close()
	credentials := &importCredentials{snapshot: CredentialSnapshot{OwnerID: "1001", Generation: 1, GrantedScopes: []string{"tweet.read", "users.read"}}}
	summary, err := NewImporter(st, NewClient(server.URL, server.Client()), importTestPolicy(t, "1001"), credentials).Import(t.Context(), ImportOptions{SourceID: source.ID, OwnerID: "1001"})
	require.Error(t, err)
	assert.Equal(t, "credential_unavailable", summary.Code)
	assert.Zero(t, calls.Load())
}

func TestXCreditsExhaustionStopsOtherEndpoint(t *testing.T) {
	st := testutil.NewTestStore(t)
	source := importTestSource(t, st, "1001")
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(402)
		fmt.Fprint(w, `{"title":"Synthetic credits exhausted"}`)
	}))
	defer server.Close()
	credentials := &importCredentials{snapshot: CredentialSnapshot{OwnerID: "1001", Generation: 1, GrantedScopes: []string{"tweet.read", "users.read"}}}
	summary, err := NewImporter(st, NewClient(server.URL, server.Client()), importTestPolicy(t, "1001"), credentials).Import(t.Context(), ImportOptions{SourceID: source.ID, OwnerID: "1001"})
	require.Error(t, err)
	assert.Equal(t, "credits_exhausted", summary.Code)
	assert.Equal(t, int64(1), calls.Load())
	assert.False(t, summary.OwnComplete)
	assert.False(t, summary.MentionsComplete)
}

func TestXImportReportsRunAndMonthlyEstimates(t *testing.T) {
	st := testutil.NewTestStore(t)
	source := importTestSource(t, st, "1001")
	policy := importTestPolicy(t, "1001")
	cost := store.XRequestCost{Kind: store.XRequestLookup, OwnerID: "1001", Generation: 1, GrantedScopes: []string{"tweet.read", "users.read"}, MonthlyBudgetMicros: policy.MonthlyBudgetMicros, RequestedResults: 1, PostPriceMicros: 5000, UserPriceMicros: 10000, At: time.Now().UTC()}
	_, err := st.ReserveXRequestContext(t.Context(), source.ID, cost)
	require.NoError(t, err)
	known, err := st.ReserveXRequestContext(t.Context(), source.ID, cost)
	require.NoError(t, err)
	require.NoError(t, st.SettleXRequestContext(t.Context(), source.ID, known.ID, store.XResourceUsage{Posts: 1, Users: 1, Persisted: true, Complete: true}))
	now := time.Now().UTC().Truncate(time.Second)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/mentions") {
			w.WriteHeader(403)
			fmt.Fprint(w, `{"title":"Synthetic endpoint rejection"}`)
			return
		}
		wireImportPage(w, []string{wireOwnPost("101", now.Add(-time.Minute))}, "")
	}))
	defer server.Close()
	credentials := &importCredentials{snapshot: CredentialSnapshot{OwnerID: "1001", Generation: 1, GrantedScopes: []string{"tweet.read", "users.read"}}}
	summary, err := NewImporter(st, NewClient(server.URL, server.Client()), policy, credentials).Import(t.Context(), ImportOptions{SourceID: source.ID, OwnerID: "1001"})
	require.Error(t, err)
	require.NotNil(t, summary)
	encoded, err := json.Marshal(summary)
	require.NoError(t, err)
	var result struct {
		RunEstimated   int64 `json:"run_estimated_micros"`
		RunReserved    int64 `json:"run_reserved_micros"`
		MonthEstimated int64 `json:"month_estimated_micros"`
		MonthReserved  int64 `json:"month_reserved_micros"`
	}
	require.NoError(t, json.Unmarshal(encoded, &result))
	assert.Equal(t, int64(15_000), result.RunEstimated)
	assert.Equal(t, int64(1_500_000), result.RunReserved)
	assert.Equal(t, int64(30_000), result.MonthEstimated)
	assert.Equal(t, int64(1_515_000), result.MonthReserved)
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
			spent, reserved, err := runCostDelta(tc.before, tc.after)
			if tc.invalid {
				assert.ErrorIs(t, err, errRunCostEstimate)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.spent, spent)
			assert.Equal(t, tc.reserved, reserved)
		})
	}
}
