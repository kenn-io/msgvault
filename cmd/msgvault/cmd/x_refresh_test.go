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
			f := newXCleanupFixture(t)
			initialVectors := xRefreshVectorCounts(t, f)
			generation := int64(1)
			_, err := f.st.UpdateXControlContext(t.Context(), f.source, 0, store.XControlPatch{OwnerID: "1001", Generation: &generation, InitializeInstance: true})
			require.NoError(t, err)
			var second int64
			at := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
			if tc.mode == "protected" {
				saved, err := f.st.PersistXPostContext(t.Context(), store.XPersistPostInput{
					SourceID: f.source, OwnerID: "1001", PostID: "200", AuthorID: "1002", ConversationID: "200",
					EditHistory: []string{"200"}, Text: "another post by the now protected author", CreatedAt: at, FetchedAt: at, Availability: "public",
				})
				require.NoError(t, err)
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
					_, _ = fmt.Fprint(w, `{"errors":[{"resource_id":"102","type":"https://api.x.com/2/problems/resource-not-found"}],"meta":{"result_count":0}}`)
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
			require.NoError(t, err)
			imp := xapi.NewImporter(f.st, xapi.NewClient(provider.URL, provider.Client()), policy, xRefreshCredentials{provider.Client()})
			cleaner := NewXContentCleanup(f.st, f.cfg, f.backend)
			imp.SetContentCleanup(func(ctx context.Context, work store.XRemovalWork) error {
				_, err := cleaner.removeXContent(ctx, work)
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
				require.NoError(t, got.err)
				assert.True(t, got.summary.Complete)
				assert.Zero(t, got.summary.Outstanding)
				assert.False(t, got.summary.LastCompleteAt.IsZero())
			} else {
				assert.Error(t, got.err)
				assert.False(t, got.summary.Complete)
				assert.Positive(t, got.summary.Outstanding)
				assert.True(t, got.summary.LastCompleteAt.IsZero())
			}
			assert.Positive(t, calls.Load())
			body, err := f.st.GetMessageBodyText(f.message)
			require.NoError(t, err)
			if tc.confirmed {
				assert.Empty(t, body)
				_, err = f.st.GetMessageRaw(f.message)
				assert.ErrorIs(t, err, sql.ErrNoRows)
				_, count, err := f.st.SearchMessages("constellation", 0, 10)
				require.NoError(t, err)
				assert.Zero(t, count)
				if second != 0 {
					body, err := f.st.GetMessageBodyText(second)
					require.NoError(t, err)
					assert.Empty(t, body, "a confirmed protected author invalidates all known chains")
				}
			} else {
				assert.Equal(t, "erased constellation secret", body)
				target, err := f.st.ResolveXPostContext(t.Context(), f.source, "102")
				require.NoError(t, err)
				assert.Equal(t, "unverified", target.Availability)
			}
			for _, gen := range f.generations {
				stats, err := f.backend.Stats(t.Context(), gen)
				require.NoError(t, err)
				if tc.confirmed {
					assert.Zero(t, stats.EmbeddingCount)
				} else {
					assert.Equal(t, initialVectors[gen], stats.EmbeddingCount)
				}
			}
			pending, err := f.st.XCleanupPendingContext(t.Context())
			require.NoError(t, err)
			assert.False(t, pending)
			assert.False(t, got.summary.PendingRemoval)
		})
	}
}

func TestXRefreshRequiresActualCleanupAndResumesWithoutReadAccess(t *testing.T) {
	for _, mode := range []string{"missing callback", "failed callback"} {
		t.Run(mode, func(t *testing.T) {
			f := newXCleanupFixture(t)
			initialVectors := xRefreshVectorCounts(t, f)
			generation := int64(1)
			_, err := f.st.UpdateXControlContext(t.Context(), f.source, 0, store.XControlPatch{OwnerID: "1001", Generation: &generation})
			require.NoError(t, err)
			var calls atomic.Int32
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				_, _ = fmt.Fprint(w, `{"errors":[{"resource_id":"102","type":"https://api.x.com/2/problems/resource-not-found"}],"meta":{"result_count":0}}`)
			}))
			t.Cleanup(provider.Close)
			policy, err := f.cfg.XAccount("1001")
			require.NoError(t, err)
			imp := xapi.NewImporter(f.st, xapi.NewClient(provider.URL, provider.Client()), policy, xRefreshCredentials{provider.Client()})
			injected := errors.New("synthetic derived cleanup failure")
			if mode == "failed callback" {
				imp.SetContentCleanup(func(context.Context, store.XRemovalWork) error { return injected })
			}
			summary, err := imp.Refresh(t.Context(), xapi.RefreshOptions{SourceID: f.source, OwnerID: "1001", BatchSize: 1})
			require.Error(t, err)
			if mode == "failed callback" {
				assert.ErrorIs(t, err, injected)
			} else {
				assert.ErrorIs(t, err, xapi.ErrContentCleanupRequired)
			}
			assert.False(t, summary.Complete)
			assert.True(t, summary.PendingRemoval)
			assert.True(t, summary.LastCompleteAt.IsZero())
			body, err := f.st.GetMessageBodyText(f.message)
			require.NoError(t, err)
			assert.Empty(t, body, "the source-scoped primary purge commits before external cleanup")
			for _, gen := range f.generations {
				stats, err := f.backend.Stats(t.Context(), gen)
				require.NoError(t, err)
				assert.Equal(t, initialVectors[gen], stats.EmbeddingCount)
			}
			cleaner := NewXContentCleanup(f.st, f.cfg, f.backend)
			release, err := cleaner.AcquireRead(t.Context())
			if release != nil {
				release()
			}
			require.Error(t, err)
			before, err := f.st.LoadXControlContext(t.Context(), f.source)
			require.NoError(t, err)
			policy.MonthlyBudgetMicros = 0
			// No API or credential object exists for this local-only resume.
			resume := xapi.NewImporter(f.st, nil, policy, nil)
			resume.SetContentCleanup(func(ctx context.Context, work store.XRemovalWork) error {
				_, err := cleaner.removeXContent(ctx, work)
				return err
			})
			summary, err = resume.Refresh(t.Context(), xapi.RefreshOptions{SourceID: f.source, OwnerID: "1001", BatchSize: 1})
			require.NoError(t, err)
			assert.True(t, summary.Complete)
			assert.False(t, summary.PendingRemoval)
			assert.Equal(t, int32(1), calls.Load())
			after, err := f.st.LoadXControlContext(t.Context(), f.source)
			require.NoError(t, err)
			assert.Equal(t, before.SpendBuckets, after.SpendBuckets)
			assert.Equal(t, before.Reservations, after.Reservations)
			for _, gen := range f.generations {
				stats, err := f.backend.Stats(t.Context(), gen)
				require.NoError(t, err)
				assert.Zero(t, stats.EmbeddingCount)
			}
		})
	}
}

func TestXRefreshFollowsLatestEditWithSeparateAdmission(t *testing.T) {
	for _, stopped := range []bool{false, true} {
		t.Run(fmt.Sprint("budget stop ", stopped), func(t *testing.T) {
			f := newXCleanupFixture(t)
			generation := int64(1)
			_, err := f.st.UpdateXControlContext(t.Context(), f.source, 0, store.XControlPatch{OwnerID: "1001", Generation: &generation})
			require.NoError(t, err)
			var mu sync.Mutex
			var requested []string
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				id := r.URL.Query().Get("ids")
				mu.Lock()
				requested = append(requested, id)
				mu.Unlock()
				assert.Contains(t, []string{"102", "104"}, id)
				_, _ = fmt.Fprintf(w, `{"data":[{"id":%q,"author_id":"1002","conversation_id":"100","edit_history_tweet_ids":["100","102","104"],"created_at":"2026-10-29T12:00:00Z","text":%q}],"includes":{"users":[{"id":"1002","username":"example_author","protected":false}]},"meta":{"result_count":1}}`, id, "current version "+id)
			}))
			t.Cleanup(provider.Close)
			policy, err := f.cfg.XAccount("1001")
			require.NoError(t, err)
			if stopped {
				policy.MonthlyBudgetMicros = 15_000 // Exactly one post+expanded-user lookup.
			}
			imp := xapi.NewImporter(f.st, xapi.NewClient(provider.URL, provider.Client()), policy, xRefreshCredentials{provider.Client()})
			summary, err := imp.Refresh(t.Context(), xapi.RefreshOptions{SourceID: f.source, OwnerID: "1001", BatchSize: 1})
			mu.Lock()
			requests := append([]string{}, requested...)
			mu.Unlock()
			target, targetErr := f.st.ResolveXPostContext(t.Context(), f.source, "100")
			require.NoError(t, targetErr)
			assert.Equal(t, "104", target.LatestKnownID)
			if stopped {
				require.ErrorIs(t, err, store.ErrXBudgetExceeded)
				assert.Equal(t, []string{"102"}, requests)
				assert.False(t, summary.Complete)
				assert.Equal(t, int64(1), summary.Outstanding)
				assert.True(t, summary.LastCompleteAt.IsZero())
				assert.Equal(t, "unverified", target.Availability)
				assert.Equal(t, "102", target.CurrentID)
			} else {
				require.NoError(t, err)
				assert.Equal(t, []string{"102", "104"}, requests)
				assert.True(t, summary.Complete)
				assert.Equal(t, "104", target.CurrentID)
				assert.Equal(t, "public", target.Availability)
				body, err := f.st.GetMessageBodyText(f.message)
				require.NoError(t, err)
				assert.Equal(t, "current version 104", body)
			}
		})
	}
}

func TestXCollectionCleanupResolvesTerminalItemsWithoutPaidReplay(t *testing.T) {
	for _, mode := range []string{"deleted", "protected", "withheld"} {
		t.Run(mode, func(t *testing.T) {
			f := newXCleanupFixture(t)
			generation := int64(1)
			_, err := f.st.UpdateXControlContext(t.Context(), f.source, 0, store.XControlPatch{OwnerID: "1001", Generation: &generation, InitializeInstance: true})
			require.NoError(t, err)
			at := time.Now().UTC().Add(-2 * time.Hour)
			_, err = f.st.PersistXPostContext(t.Context(), store.XPersistPostInput{SourceID: f.source, OwnerID: "1001", PostID: "102", AuthorID: "1002", ConversationID: "100", EditHistory: []string{"100", "102"}, Text: "erased constellation secret", CreatedAt: at, FetchedAt: time.Now().UTC(), Availability: "public", Author: &store.XAuthorInput{UserID: "1002", FetchedAt: time.Now().UTC()}})
			require.NoError(t, err)
			var lookups, mentions atomic.Int32
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/tweets" {
					lookups.Add(1)
					_, _ = fmt.Fprint(w, `{"errors":[{"resource_id":"102","type":"https://api.x.com/2/problems/resource-not-found"}]}`)
					return
				}
				if r.URL.Path != "/users/1001/mentions" || r.URL.Query().Get("start_time") != "" {
					_, _ = fmt.Fprint(w, `{"data":[]}`)
					return
				}
				mentions.Add(1)
				if mode == "deleted" {
					_, _ = fmt.Fprint(w, `{"errors":[{"resource_id":"102","type":"https://api.x.com/2/problems/resource-not-found"}]}`)
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
			require.NoError(t, err)
			policy.MonthlyBudgetMicros = 100000000
			cleaner := NewXContentCleanup(f.st, f.cfg, f.backend)
			cleaner.afterStore = func() error { return errors.New("synthetic derived cleanup interruption") }
			imp := xapi.NewImporter(f.st, xapi.NewClient(provider.URL, provider.Client()), policy, xRefreshCredentials{provider.Client()})
			imp.SetContentCleanup(func(ctx context.Context, work store.XRemovalWork) error {
				_, err := cleaner.removeXContent(ctx, work)
				return err
			})
			_, err = imp.Import(t.Context(), xapi.ImportOptions{SourceID: f.source, OwnerID: "1001"})
			require.Error(t, err)
			state, err := f.st.LoadXControlContext(t.Context(), f.source)
			require.NoError(t, err)
			require.NotNil(t, state.Mentions)
			assert.Equal(t, []string{"102"}, state.Mentions.UnresolvedIDs, "page obligations must survive cleanup interruption")
			assert.False(t, state.Mentions.Complete)
			assert.Zero(t, lookups.Load(), "known terminal page result needs no lookup")
			pending, err := f.st.XCleanupPendingContext(t.Context())
			require.NoError(t, err)
			assert.True(t, pending)
			cleaner.afterStore = nil
			_, err = imp.Import(t.Context(), xapi.ImportOptions{SourceID: f.source, OwnerID: "1001"})
			require.NoError(t, err)
			state, err = f.st.LoadXControlContext(t.Context(), f.source)
			require.NoError(t, err)
			assert.True(t, state.Mentions.Complete)
			assert.Empty(t, state.Mentions.UnresolvedIDs)
			assert.Equal(t, int32(1), mentions.Load(), "committed page must not be replayed")
			assert.Zero(t, lookups.Load(), "completed local cleanup resolves terminal obligation without paid retry")
			body, err := f.st.GetMessageBodyText(f.message)
			require.NoError(t, err)
			assert.Empty(t, body)
			for _, gen := range f.generations {
				stats, err := f.backend.Stats(t.Context(), gen)
				require.NoError(t, err)
				assert.Zero(t, stats.EmbeddingCount)
			}
		})
	}
}

func TestXCollectionOwnerResumesForeignCopyAfterReleasingExecution(t *testing.T) {
	f := newXCleanupFixture(t)
	now := time.Now().UTC()
	manager := xapi.NewTokenManager(f.cfg.TokensDir())
	require.NoError(t, manager.Save(t.Context(), xTestRecord("collection-access")))
	record, err := manager.Load(t.Context(), "1001")
	require.NoError(t, err)
	_, err = f.st.UpdateXControlContext(t.Context(), f.source, 0, store.XControlPatch{OwnerID: "1001", Generation: &record.Generation, InitializeInstance: true})
	require.NoError(t, err)
	_, err = f.st.PersistXPostContext(t.Context(), store.XPersistPostInput{SourceID: f.source, OwnerID: "1001", PostID: "102", AuthorID: "1002", ConversationID: "100", EditHistory: []string{"100", "102"}, Text: "erased constellation secret", CreatedAt: now.Add(-2 * time.Hour), FetchedAt: now, Availability: "public", Author: &store.XAuthorInput{UserID: "1002", FetchedAt: now}})
	require.NoError(t, err)
	other, err := f.st.GetOrCreateSource("x", "2001")
	require.NoError(t, err)
	copy, err := f.st.PersistXPostContext(t.Context(), store.XPersistPostInput{SourceID: other.ID, OwnerID: "2001", PostID: "102", AuthorID: "1002", ConversationID: "100", EditHistory: []string{"100", "102"}, Text: "erased constellation secret", CreatedAt: now.Add(-2 * time.Hour), FetchedAt: now, Availability: "public", Author: &store.XAuthorInput{UserID: "1002", FetchedAt: now}})
	require.NoError(t, err)
	gen, err := f.backend.CreateGeneration(t.Context(), "foreign-copy-local", 3, f.cfg.Vector.GenerationFingerprint())
	require.NoError(t, err)
	f.generations = append(f.generations, gen)
	require.NoError(t, f.backend.Upsert(t.Context(), gen, []vector.Chunk{{MessageID: copy.MessageID, Vector: []float32{1, 0, 0}}}))
	var lookups, mentions atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/tweets" {
			lookups.Add(1)
		}
		if r.URL.Path == "/users/1001/mentions" && r.URL.Query().Get("start_time") == "" {
			mentions.Add(1)
			fmt.Fprint(w, `{"errors":[{"resource_id":"102","type":"https://api.x.com/2/problems/resource-not-found"}]}`)
			return
		}
		fmt.Fprint(w, `{"data":[]}`)
	}))
	defer provider.Close()
	policy, err := f.cfg.XAccount("1001")
	require.NoError(t, err)
	policy.MonthlyBudgetMicros = 100000000
	f.cfg.X.Accounts = []config.XAccountConfig{policy}
	cleaner := NewXContentCleanup(f.st, f.cfg, f.backend)
	deps := xCommandDeps{apiBaseURL: provider.URL, httpClient: provider.Client(), cleanup: cleaner}
	source, err := f.st.GetSourceByIDContext(t.Context(), f.source)
	require.NoError(t, err)
	_, err = importXSourceWithDeps(t.Context(), f.st, f.cfg, source, xapi.ImportOptions{}, deps)
	require.ErrorIs(t, err, store.ErrXRemovalNeedsOwner, "the callback defers the connected source group")
	pending, err := f.st.XCleanupPendingContext(t.Context())
	require.NoError(t, err)
	assert.False(t, pending, "owner Resume must run after releasing Import execution")
	for _, message := range []int64{f.message, copy.MessageID} {
		body, err := f.st.GetMessageBodyText(message)
		require.NoError(t, err)
		assert.Empty(t, body)
	}
	for _, gen := range f.generations {
		stats, err := f.backend.Stats(t.Context(), gen)
		require.NoError(t, err)
		assert.Zero(t, stats.EmbeddingCount)
	}
	_, err = importXSourceWithDeps(t.Context(), f.st, f.cfg, source, xapi.ImportOptions{}, deps)
	require.NoError(t, err)
	assert.Equal(t, int32(1), mentions.Load())
	assert.Zero(t, lookups.Load())
}
