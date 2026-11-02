package cmd

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/vector"
	xapi "go.kenn.io/msgvault/internal/x"
)

// The credential lifecycle has its own real file/HTTP tests. Refresh tests keep
// this verified external contract fixed and use the real HTTP parser, admission,
// Store, cleanup coordinator and actual vector generations.
type xRefreshCredentials struct{ client *http.Client }

func (c xRefreshCredentials) Prepare(ctx context.Context) (xapi.CredentialSnapshot, error) {
	return xapi.CredentialSnapshot{OwnerID: "1001", Generation: 1, GrantedScopes: []string{"tweet.read", "users.read"}}, ctx.Err()
}
func (c xRefreshCredentials) Refresh(ctx context.Context, _ int64) (xapi.CredentialSnapshot, error) {
	return c.Prepare(ctx)
}
func (c xRefreshCredentials) HTTPClient() *http.Client { return c.client }

func xRefreshVectorCounts(t *testing.T, f *xCleanupFixture) map[vector.GenerationID]int64 {
	t.Helper()
	counts := make(map[vector.GenerationID]int64, len(f.generations))
	var total int64
	for _, gen := range f.generations {
		stats, err := f.backend.Stats(t.Context(), gen)
		require.NoError(t, err)
		counts[gen] = stats.EmbeddingCount
		total += stats.EmbeddingCount
	}
	// Retirement may already empty a generation. Uncertain reads must retain
	// every existing count, and the fixture must actually contain vectors.
	require.Positive(t, total)
	return counts
}

func TestXRefreshDistinguishesRemovalFromUnknown(t *testing.T) {
	for _, tc := range []struct {
		mode      string
		confirmed bool
	}{
		{"deleted", true}, {"protected", true}, {"withheld", true},
		{"unauthorized", false}, {"forbidden", false}, {"timeout", false}, {"missing", false},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			_testAssert := assert.New(t)
			_testRequire := require.New(t)

			f := newXCleanupFixture(t)
			initialVectors := xRefreshVectorCounts(t, f)
			generation := int64(1)
			_, err := f.st.UpdateXControlContext(t.Context(), f.source, 0, store.XControlPatch{OwnerID: "1001", Generation: &generation, InitializeInstance: true})
			_testRequire.NoError(err)
			var second int64
			at := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
			if tc.mode == "protected" {
				saved, err := f.st.PersistXPostContext(t.Context(), store.XPersistPostInput{
					SourceID: f.source, OwnerID: "1001", PostID: "200", AuthorID: "1002", ConversationID: "200",
					EditHistory: []string{"200"}, Text: "another post by the now protected author", CreatedAt: at, FetchedAt: at, Availability: "public",
				})
				_testRequire.NoError(err)
				second = saved.MessageID
			}
			var calls atomic.Int32
			started := make(chan struct{}, 1)
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				assert.Equal(t, http.MethodGet, r.Method)
				assert.Equal(t, "/tweets", r.URL.Path)
				assert.Equal(t, "102", r.URL.Query().Get("ids"))
				switch tc.mode {
				case "deleted":
					_, _ = fmt.Fprint(w, `{"errors":[{"resource_type":"tweet","resource_id":"102","type":"https://api.x.com/2/problems/resource-not-found"}],"meta":{"result_count":0}}`)
				case "unauthorized":
					w.WriteHeader(http.StatusUnauthorized)
				case "forbidden":
					w.WriteHeader(http.StatusForbidden)
				case "timeout":
					started <- struct{}{}
					<-r.Context().Done()
				case "missing":
					_, _ = fmt.Fprint(w, `{"data":[],"meta":{"result_count":0}}`)
				default:
					withheld := ""
					if tc.mode == "withheld" {
						withheld = `,"withheld":{"country_codes":["ZZ"]}`
					}
					_, _ = fmt.Fprintf(w, `{"data":[{"id":"102","author_id":"1002","conversation_id":"100","edit_history_tweet_ids":["100","102"],"created_at":%q,"text":"updated constellation content"%s}],"includes":{"users":[{"id":"1002","name":"Erased Label","username":"erased_handle","protected":%t}]},"meta":{"result_count":1}}`, at.Format(time.RFC3339Nano), withheld, tc.mode == "protected")
				}
			}))
			t.Cleanup(provider.Close)
			policy, err := f.cfg.XAccount("1001")
			_testRequire.NoError(err)
			imp := xapi.NewImporter(f.st, xapi.NewClient(provider.URL, provider.Client()), policy, xRefreshCredentials{provider.Client()})
			cleaner := NewXContentCleanup(f.st, f.cfg, f.backend)
			imp.SetContentCleanup(func(ctx context.Context, work store.XRemovalWork) error {
				err := cleaner.removeXContent(ctx, work)
				return err
			})
			ctx, cancel := context.WithCancelCause(t.Context())
			defer cancel(nil)
			type result struct {
				summary xapi.RefreshSummary
				err     error
			}
			finished := make(chan result, 1)
			go func() {
				summary, err := imp.Refresh(ctx, xapi.RefreshOptions{SourceID: f.source, OwnerID: "1001", BatchSize: 1})
				finished <- result{summary, err}
			}()
			var got result
			if tc.mode == "timeout" {
				// Trigger a deadline failure only after observable real provider
				// I/O begins. No runner-throughput or elapsed-time assertion.
				select {
				case <-started:
					cancel(context.DeadlineExceeded)
					got = <-finished
				case got = <-finished:
				}
			} else {
				got = <-finished
			}
			if tc.confirmed {
				_testRequire.NoError(got.err)
				_testAssert.True(got.summary.Complete)
				_testAssert.Zero(got.summary.Outstanding)
				_testAssert.False(got.summary.LastCompleteAt.IsZero())
			} else {
				_testRequire.Error(got.err)
				_testAssert.False(got.summary.Complete)
				_testAssert.Positive(got.summary.Outstanding)
				_testAssert.True(got.summary.LastCompleteAt.IsZero())
			}
			_testAssert.Positive(calls.Load())
			body, err := f.st.GetMessageBodyText(f.message)
			_testRequire.NoError(err)
			if tc.confirmed {
				_testAssert.Empty(body)
				_, err = f.st.GetMessageRaw(f.message)
				_testRequire.ErrorIs(err, sql.ErrNoRows)
				_, count, err := f.st.SearchMessages("constellation", 0, 10)
				_testRequire.NoError(err)
				_testAssert.Zero(count)
				if second != 0 {
					body, err := f.st.GetMessageBodyText(second)
					_testRequire.NoError(err)
					_testAssert.Empty(body, "a confirmed protected author invalidates all known chains")
				}
			} else {
				_testAssert.Equal("erased constellation secret", body)
				target, err := f.st.ResolveXPostContext(t.Context(), f.source, "102")
				_testRequire.NoError(err)
				_testAssert.Equal("unverified", target.Availability)
			}
			for _, gen := range f.generations {
				stats, err := f.backend.Stats(t.Context(), gen)
				_testRequire.NoError(err)
				if tc.confirmed {
					_testAssert.Zero(stats.EmbeddingCount)
				} else {
					_testAssert.Equal(initialVectors[gen], stats.EmbeddingCount)
				}
			}
			pending, err := f.st.XCleanupPendingContext(t.Context())
			_testRequire.NoError(err)
			_testAssert.False(pending)
			_testAssert.False(got.summary.PendingRemoval)
		})
	}
}

func TestXRefreshRequiresActualCleanupAndResumesWithoutReadAccess(t *testing.T) {
	for _, mode := range []string{"missing callback", "failed callback"} {
		t.Run(mode, func(t *testing.T) {
			_testAssert := assert.New(t)
			_testRequire := require.New(t)

			f := newXCleanupFixture(t)
			initialVectors := xRefreshVectorCounts(t, f)
			generation := int64(1)
			_, err := f.st.UpdateXControlContext(t.Context(), f.source, 0, store.XControlPatch{OwnerID: "1001", Generation: &generation})
			_testRequire.NoError(err)
			var calls atomic.Int32
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				_, _ = fmt.Fprint(w, `{"errors":[{"resource_type":"tweet","resource_id":"102","type":"https://api.x.com/2/problems/resource-not-found"}],"meta":{"result_count":0}}`)
			}))
			t.Cleanup(provider.Close)
			policy, err := f.cfg.XAccount("1001")
			_testRequire.NoError(err)
			imp := xapi.NewImporter(f.st, xapi.NewClient(provider.URL, provider.Client()), policy, xRefreshCredentials{provider.Client()})
			injected := errors.New("synthetic derived cleanup failure")
			if mode == "failed callback" {
				imp.SetContentCleanup(func(context.Context, store.XRemovalWork) error { return injected })
			}
			summary, err := imp.Refresh(t.Context(), xapi.RefreshOptions{SourceID: f.source, OwnerID: "1001", BatchSize: 1})
			_testRequire.Error(err)
			if mode == "failed callback" {
				_testRequire.ErrorIs(err, injected)
			} else {
				_testRequire.ErrorIs(err, xapi.ErrContentCleanupRequired)
			}
			_testAssert.False(summary.Complete)
			_testAssert.True(summary.PendingRemoval)
			_testAssert.True(summary.LastCompleteAt.IsZero())
			body, err := f.st.GetMessageBodyText(f.message)
			_testRequire.NoError(err)
			_testAssert.Empty(body, "the source-scoped primary purge commits before external cleanup")
			for _, gen := range f.generations {
				stats, err := f.backend.Stats(t.Context(), gen)
				_testRequire.NoError(err)
				_testAssert.Equal(initialVectors[gen], stats.EmbeddingCount)
			}
			cleaner := NewXContentCleanup(f.st, f.cfg, f.backend)
			release, err := cleaner.AcquireRead(t.Context())
			if release != nil {
				release()
			}
			_testRequire.Error(err)
			before, err := f.st.LoadXControlContext(t.Context(), f.source)
			_testRequire.NoError(err)
			policy.MonthlyBudgetMicros = 0
			// No API or credential object exists for this local-only resume.
			resume := xapi.NewImporter(f.st, nil, policy, nil)
			resume.SetContentCleanup(func(ctx context.Context, work store.XRemovalWork) error {
				err := cleaner.removeXContent(ctx, work)
				return err
			})
			summary, err = resume.Refresh(t.Context(), xapi.RefreshOptions{SourceID: f.source, OwnerID: "1001", BatchSize: 1})
			_testRequire.NoError(err)
			_testAssert.True(summary.Complete)
			_testAssert.False(summary.PendingRemoval)
			_testAssert.Equal(int32(1), calls.Load())
			after, err := f.st.LoadXControlContext(t.Context(), f.source)
			_testRequire.NoError(err)
			_testAssert.Equal(before.SpendBuckets, after.SpendBuckets)
			_testAssert.Equal(before.Reservations, after.Reservations)
			for _, gen := range f.generations {
				stats, err := f.backend.Stats(t.Context(), gen)
				_testRequire.NoError(err)
				_testAssert.Zero(stats.EmbeddingCount)
			}
		})
	}
}

func TestXRefreshFollowsLatestEditWithSeparateAdmission(t *testing.T) {
	for _, stopped := range []bool{false, true} {
		t.Run(fmt.Sprint("budget stop ", stopped), func(t *testing.T) {
			_testAssert := assert.New(t)
			_testRequire := require.New(t)

			f := newXCleanupFixture(t)
			generation := int64(1)
			_, err := f.st.UpdateXControlContext(t.Context(), f.source, 0, store.XControlPatch{OwnerID: "1001", Generation: &generation})
			_testRequire.NoError(err)
			var mu sync.Mutex
			var requested []string
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				id := r.URL.Query().Get("ids")
				mu.Lock()
				requested = append(requested, id)
				mu.Unlock()
				assert.Contains(t, []string{"102", "104"}, id)
				// #nosec G705 -- The test client decodes this synthetic response as JSON; it is not rendered as HTML.
				// #nosec G705 -- The test client decodes this synthetic response as JSON; it is not rendered as HTML.
				_, _ = fmt.Fprintf(w, `{"data":[{"id":%q,"author_id":"1002","conversation_id":"100","edit_history_tweet_ids":["100","102","104"],"created_at":"2026-10-29T12:00:00Z","text":%q}],"includes":{"users":[{"id":"1002","username":"example_author","protected":false}]},"meta":{"result_count":1}}`, id, "current version "+id)
			}))
			t.Cleanup(provider.Close)
			policy, err := f.cfg.XAccount("1001")
			_testRequire.NoError(err)
			if stopped {
				policy.MonthlyBudgetMicros = 15_000 // Exactly one post+expanded-user lookup.
			}
			imp := xapi.NewImporter(f.st, xapi.NewClient(provider.URL, provider.Client()), policy, xRefreshCredentials{provider.Client()})
			summary, err := imp.Refresh(t.Context(), xapi.RefreshOptions{SourceID: f.source, OwnerID: "1001", BatchSize: 1})
			mu.Lock()
			requests := append([]string{}, requested...)
			mu.Unlock()
			target, targetErr := f.st.ResolveXPostContext(t.Context(), f.source, "100")
			_testRequire.NoError(targetErr)
			_testAssert.Equal("104", target.LatestKnownID)
			if stopped {
				_testRequire.ErrorIs(err, store.ErrXBudgetExceeded)
				_testAssert.Equal([]string{"102"}, requests)
				_testAssert.False(summary.Complete)
				_testAssert.Equal(int64(1), summary.Outstanding)
				_testAssert.True(summary.LastCompleteAt.IsZero())
				_testAssert.Equal("unverified", target.Availability)
				_testAssert.Equal("102", target.CurrentID)
			} else {
				_testRequire.NoError(err)
				_testAssert.Equal([]string{"102", "104"}, requests)
				_testAssert.True(summary.Complete)
				_testAssert.Equal("104", target.CurrentID)
				_testAssert.Equal("public", target.Availability)
				body, err := f.st.GetMessageBodyText(f.message)
				_testRequire.NoError(err)
				_testAssert.Equal("current version 104", body)
			}
		})
	}
}

func TestXCollectionCleanupResolvesTerminalItemsWithoutPaidReplay(t *testing.T) {
	for _, mode := range []string{"deleted", "protected", "withheld"} {
		t.Run(mode, func(t *testing.T) {
			_testAssert := assert.New(t)
			_testRequire := require.New(t)

			f := newXCleanupFixture(t)
			generation := int64(1)
			_, err := f.st.UpdateXControlContext(t.Context(), f.source, 0, store.XControlPatch{OwnerID: "1001", Generation: &generation, InitializeInstance: true})
			_testRequire.NoError(err)
			at := time.Now().UTC().Add(-2 * time.Hour)
			_, err = f.st.PersistXPostContext(t.Context(), store.XPersistPostInput{SourceID: f.source, OwnerID: "1001", PostID: "102", AuthorID: "1002", ConversationID: "100", EditHistory: []string{"100", "102"}, Text: "erased constellation secret", CreatedAt: at, FetchedAt: time.Now().UTC(), Availability: "public", Author: &store.XAuthorInput{UserID: "1002", FetchedAt: time.Now().UTC()}})
			_testRequire.NoError(err)
			var lookups, mentions atomic.Int32
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/tweets" {
					lookups.Add(1)
					_, _ = fmt.Fprint(w, `{"errors":[{"resource_type":"tweet","resource_id":"102","type":"https://api.x.com/2/problems/resource-not-found"}]}`)
					return
				}
				if r.URL.Path != "/users/1001/mentions" || r.URL.Query().Get("start_time") != "" {
					_, _ = fmt.Fprint(w, `{"data":[]}`)
					return
				}
				mentions.Add(1)
				if mode == "deleted" {
					_, _ = fmt.Fprint(w, `{"errors":[{"resource_type":"tweet","resource_id":"102","type":"https://api.x.com/2/problems/resource-not-found"}]}`)
					return
				}
				withheld := ""
				if mode == "withheld" {
					withheld = `,"withheld":{"country_codes":["ZZ"]}`
				}
				_, _ = fmt.Fprintf(w, `{"data":[{"id":"102","author_id":"1002","conversation_id":"100","edit_history_tweet_ids":["100","102"],"created_at":%q,"text":"synthetic content"%s}],"includes":{"users":[{"id":"1002","username":"example_author","protected":%t}]}}`, at.Format(time.RFC3339Nano), withheld, mode == "protected")
			}))
			defer provider.Close()
			policy, err := f.cfg.XAccount("1001")
			_testRequire.NoError(err)
			policy.MonthlyBudgetMicros = 100000000
			cleaner := NewXContentCleanup(f.st, f.cfg, f.backend)
			cleaner.afterStore = func() error { return errors.New("synthetic derived cleanup interruption") }
			imp := xapi.NewImporter(f.st, xapi.NewClient(provider.URL, provider.Client()), policy, xRefreshCredentials{provider.Client()})
			imp.SetContentCleanup(func(ctx context.Context, work store.XRemovalWork) error {
				err := cleaner.removeXContent(ctx, work)
				return err
			})
			_, err = imp.Import(t.Context(), xapi.ImportOptions{SourceID: f.source, OwnerID: "1001"})
			_testRequire.Error(err)
			state, err := f.st.LoadXControlContext(t.Context(), f.source)
			_testRequire.NoError(err)
			_testRequire.NotNil(state.Mentions)
			_testAssert.Equal([]string{"102"}, state.Mentions.UnresolvedIDs, "page obligations must survive cleanup interruption")
			_testAssert.False(state.Mentions.Complete)
			_testAssert.Zero(lookups.Load(), "known terminal page result needs no lookup")
			pending, err := f.st.XCleanupPendingContext(t.Context())
			_testRequire.NoError(err)
			_testAssert.True(pending)
			cleaner.afterStore = nil
			_, err = imp.Import(t.Context(), xapi.ImportOptions{SourceID: f.source, OwnerID: "1001"})
			_testRequire.NoError(err)
			state, err = f.st.LoadXControlContext(t.Context(), f.source)
			_testRequire.NoError(err)
			_testAssert.True(state.Mentions.Complete)
			_testAssert.Empty(state.Mentions.UnresolvedIDs)
			_testAssert.Equal(int32(1), mentions.Load(), "committed page must not be replayed")
			_testAssert.Zero(lookups.Load(), "completed local cleanup resolves terminal obligation without paid retry")
			body, err := f.st.GetMessageBodyText(f.message)
			_testRequire.NoError(err)
			_testAssert.Empty(body)
			for _, gen := range f.generations {
				stats, err := f.backend.Stats(t.Context(), gen)
				_testRequire.NoError(err)
				_testAssert.Zero(stats.EmbeddingCount)
			}
		})
	}
}

func TestXCollectionOwnerResumesForeignCopyAfterReleasingExecution(t *testing.T) {
	_testAssert := assert.New(t)
	_testRequire := require.New(t)

	f := newXCleanupFixture(t)
	now := time.Now().UTC()
	manager := xapi.NewTokenManager(f.cfg.TokensDir())
	_testRequire.NoError(manager.Save(t.Context(), xTestRecord("collection-access")))
	record, err := manager.Load(t.Context(), "1001")
	_testRequire.NoError(err)
	_, err = f.st.UpdateXControlContext(t.Context(), f.source, 0, store.XControlPatch{OwnerID: "1001", Generation: &record.Generation, InitializeInstance: true})
	_testRequire.NoError(err)
	_, err = f.st.PersistXPostContext(t.Context(), store.XPersistPostInput{SourceID: f.source, OwnerID: "1001", PostID: "102", AuthorID: "1002", ConversationID: "100", EditHistory: []string{"100", "102"}, Text: "erased constellation secret", CreatedAt: now.Add(-2 * time.Hour), FetchedAt: now, Availability: "public", Author: &store.XAuthorInput{UserID: "1002", FetchedAt: now}})
	_testRequire.NoError(err)
	other, err := f.st.GetOrCreateSource("x", "2001")
	_testRequire.NoError(err)
	foreignCopy, err := f.st.PersistXPostContext(t.Context(), store.XPersistPostInput{SourceID: other.ID, OwnerID: "2001", PostID: "102", AuthorID: "1002", ConversationID: "100", EditHistory: []string{"100", "102"}, Text: "erased constellation secret", CreatedAt: now.Add(-2 * time.Hour), FetchedAt: now, Availability: "public", Author: &store.XAuthorInput{UserID: "1002", FetchedAt: now}})
	_testRequire.NoError(err)
	gen, err := f.backend.CreateGeneration(t.Context(), "foreign-copy-local", 3, f.cfg.Vector.GenerationFingerprint())
	_testRequire.NoError(err)
	f.generations = append(f.generations, gen)
	_testRequire.NoError(f.backend.Upsert(t.Context(), gen, []vector.Chunk{{MessageID: foreignCopy.MessageID, Vector: []float32{1, 0, 0}}}))
	var lookups, mentions atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/tweets" {
			lookups.Add(1)
		}
		if r.URL.Path == "/users/1001/mentions" && r.URL.Query().Get("start_time") == "" {
			mentions.Add(1)
			_, _ = fmt.Fprint(w, `{"errors":[{"resource_type":"tweet","resource_id":"102","type":"https://api.x.com/2/problems/resource-not-found"}]}`)
			return
		}
		_, _ = fmt.Fprint(w, `{"data":[]}`)
	}))
	defer provider.Close()
	policy, err := f.cfg.XAccount("1001")
	_testRequire.NoError(err)
	policy.MonthlyBudgetMicros = 100000000
	f.cfg.X.Accounts = []config.XAccountConfig{policy}
	cleaner := NewXContentCleanup(f.st, f.cfg, f.backend)
	deps := xCommandDeps{apiBaseURL: provider.URL, httpClient: provider.Client(), cleanup: cleaner}
	source, err := f.st.GetSourceByIDContext(t.Context(), f.source)
	_testRequire.NoError(err)
	_, err = importXSourceWithDeps(t.Context(), f.st, f.cfg, source, xapi.ImportOptions{}, deps)
	_testRequire.ErrorIs(err, store.ErrXRemovalNeedsOwner, "the callback defers the connected source group")
	pending, err := f.st.XCleanupPendingContext(t.Context())
	_testRequire.NoError(err)
	_testAssert.False(pending, "owner Resume must run after releasing Import execution")
	for _, message := range []int64{f.message, foreignCopy.MessageID} {
		body, err := f.st.GetMessageBodyText(message)
		_testRequire.NoError(err)
		_testAssert.Empty(body)
	}
	for _, gen := range f.generations {
		stats, err := f.backend.Stats(t.Context(), gen)
		_testRequire.NoError(err)
		_testAssert.Zero(stats.EmbeddingCount)
	}
	_, err = importXSourceWithDeps(t.Context(), f.st, f.cfg, source, xapi.ImportOptions{}, deps)
	_testRequire.NoError(err)
	_testAssert.Equal(int32(1), mentions.Load())
	_testAssert.Zero(lookups.Load())
}
