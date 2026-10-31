package x

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

type archiveCredentials struct {
	*importCredentials
	prepares atomic.Int64
}

func (c *archiveCredentials) Prepare(ctx context.Context) (CredentialSnapshot, error) {
	c.prepares.Add(1)
	return c.importCredentials.Prepare(ctx)
}

func TestXArchivePreviewMakesZeroRequests(t *testing.T) {
	st := testutil.NewTestStore(t)
	source := importTestSource(t, st, "1001")
	name := "example_owner"
	state, err := st.LoadXControlContext(t.Context(), source.ID)
	require.NoError(t, err)
	_, err = st.UpdateXControlContext(t.Context(), source.ID, state.Revision, store.XControlPatch{OwnerID: "1001", Username: &name})
	require.NoError(t, err)
	require.NoError(t, st.UpdateSourceDisplayName(source.ID, "Synthetic archive label"))
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(403) }))
	defer server.Close()
	credentials := &archiveCredentials{importCredentials: &importCredentials{snapshot: CredentialSnapshot{OwnerID: "1001", Generation: 1, GrantedScopes: []string{"tweet.read", "users.read"}}}}
	imp := NewImporter(st, NewClient(server.URL, server.Client()), importTestPolicy(t, "1001"), credentials)
	now := time.Date(2026, 10, 30, 9, 0, 0, 0, time.UTC)
	imp.now = func() time.Time { return now }
	after := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	summary, err := imp.Import(t.Context(), ImportOptions{SourceID: source.ID, OwnerID: "1001", After: after, Archive: true})
	assert.NoError(t, err)
	assert.Zero(t, calls.Load(), "preview must not call Me, search, counts, sampling or ordinary timelines")
	assert.Zero(t, credentials.prepares.Load(), "preview must not refresh credentials")
	require.NotNil(t, summary)
	assert.Equal(t, "preview", summary.Status)
	assert.Zero(t, summary.SyncRunID)
	require.NotNil(t, summary.Archive)
	assert.Equal(t, "from:example_owner -is:retweet", summary.Archive.OwnQuery)
	assert.Equal(t, "to:example_owner -is:retweet", summary.Archive.MentionsQuery)
	assert.Equal(t, after, summary.Archive.After)
	assert.Equal(t, now, summary.Archive.Before)
	assert.Equal(t, int64(1_500_000), summary.Archive.PageReservationMicros)
	assert.True(t, summary.Archive.TotalUnknown)
	current, err := st.LoadXControlContext(t.Context(), source.ID)
	require.NoError(t, err)
	assert.Empty(t, current.Jobs)
	assert.Empty(t, current.Reservations)
	assert.Nil(t, current.Own)
	assert.Nil(t, current.Mentions)
}

func TestXArchiveResumeDoesNotRenewCap(t *testing.T) {
	st := testutil.NewTestStore(t)
	source := importTestSource(t, st, "1001")
	name := "example_owner"
	state, err := st.LoadXControlContext(t.Context(), source.ID)
	require.NoError(t, err)
	_, err = st.UpdateXControlContext(t.Context(), source.ID, state.Revision, store.XControlPatch{OwnerID: "1001", Username: &name})
	require.NoError(t, err)
	now := time.Date(2026, 10, 30, 9, 0, 0, 0, time.UTC)
	after := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	var reject atomic.Bool
	reject.Store(true)
	var calls, firstPages atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if !assert.Equal(t, "/tweets/search/all", r.URL.Path) {
			w.WriteHeader(403)
			return
		}
		control, readErr := st.LoadXControlContext(r.Context(), source.ID)
		if !assert.NoError(t, readErr) || !assert.Len(t, control.Jobs, 1) {
			w.WriteHeader(403)
			return
		}
		assert.Equal(t, int64(25_000_000), control.Jobs[0].CapMicros)
		assert.NotEmpty(t, control.Reservations)
		assert.Equal(t, now.Format(time.RFC3339), r.URL.Query().Get("end_time"))
		assert.Equal(t, after.Format(time.RFC3339), r.URL.Query().Get("start_time"))
		assert.Equal(t, "recency", r.URL.Query().Get("sort_order"))
		assert.Empty(t, r.URL.Query().Get("since_id"))
		assert.Empty(t, r.URL.Query().Get("until_id"))
		switch r.URL.Query().Get("query") {
		case "from:example_owner -is:retweet":
			if r.URL.Query().Get("next_token") == "" {
				firstPages.Add(1)
				wireImportPage(w, []string{wireOwnPost("102", now.Add(-time.Hour))}, "second")
				return
			}
			assert.Equal(t, "second", r.URL.Query().Get("next_token"))
			if reject.Load() {
				w.WriteHeader(403)
				return
			}
			wireImportPage(w, []string{wireOwnPost("101", now.Add(-2*time.Hour))}, "")
		case "to:example_owner -is:retweet":
			wireImportPage(w, nil, "")
		default:
			assert.Fail(t, "archive query changed from its persisted identity")
			w.WriteHeader(403)
		}
	}))
	defer server.Close()
	credentials := &importCredentials{snapshot: CredentialSnapshot{OwnerID: "1001", Generation: 1, GrantedScopes: []string{"tweet.read", "users.read"}}}
	policy := importTestPolicy(t, "1001")
	// This contract isolates cap persistence across month rollover. Maintenance
	// has a separate refresh-first regression; keep these public rows fresh for
	// the two-month clock advances rather than spending on unrelated lookups.
	policy.RefreshSweepInterval = 90 * 24 * time.Hour
	cap := int64(25_000_000)
	makeImporter := func(at time.Time) *Importer {
		imp := NewImporter(st, NewClient(server.URL, server.Client()), policy, credentials)
		imp.now = func() time.Time { return at }
		return imp
	}
	opts := ImportOptions{SourceID: source.ID, OwnerID: "1001", After: after, Archive: true, MaxSpendMicros: &cap}
	first, err := makeImporter(now).Import(t.Context(), opts)
	require.Error(t, err)
	require.NotNil(t, first)
	require.NotNil(t, first.Archive)
	require.NotEmpty(t, first.Archive.JobID)
	jobID := first.Archive.JobID
	assert.Equal(t, int64(15_000), first.Archive.SpentMicros)
	assert.Equal(t, int64(3_000_000), first.Archive.ReservedMicros)
	require.NotNil(t, first.Archive.RemainingMicros)
	assert.Equal(t, int64(21_985_000), *first.Archive.RemainingMicros)
	state, err = st.LoadXControlContext(t.Context(), source.ID)
	require.NoError(t, err)
	require.Len(t, state.Jobs, 1)
	assert.Equal(t, "second", state.Jobs[0].Own.Token)
	assert.False(t, state.Jobs[0].Own.Complete)
	assert.True(t, state.Jobs[0].Mentions.Complete)
	assert.Nil(t, state.Own)
	assert.Nil(t, state.Mentions)
	name = "renamed_owner"
	generation := int64(2)
	_, err = st.UpdateXControlContext(t.Context(), source.ID, state.Revision, store.XControlPatch{OwnerID: "1001", Username: &name, Generation: &generation})
	require.NoError(t, err)
	credentials.snapshot.Generation = 2
	reject.Store(false)
	resumed, err := makeImporter(now.AddDate(0, 1, 0)).Import(t.Context(), opts)
	require.NoError(t, err)
	require.NotNil(t, resumed.Archive)
	assert.Equal(t, jobID, resumed.Archive.JobID)
	assert.Equal(t, now, resumed.Archive.Before)
	assert.Equal(t, "example_owner", resumed.Archive.Username)
	assert.True(t, resumed.Archive.OwnComplete)
	assert.True(t, resumed.Archive.MentionsComplete)
	assert.Equal(t, int64(1), resumed.MessagesAdded)
	assert.Equal(t, int64(15_000), resumed.RunEstimatedMicros)
	assert.Equal(t, int64(15_000), resumed.EstimatedMicros)
	assert.Zero(t, resumed.ReservedMicros)
	assert.Equal(t, int64(30_000), resumed.Archive.SpentMicros)
	assert.Equal(t, int64(3_000_000), resumed.Archive.ReservedMicros)
	assert.Equal(t, []string{"101", "102"}, archivedXIDs(t, st, source.ID))
	beforeRepeat := calls.Load()
	repeated, err := makeImporter(now.AddDate(0, 2, 0)).Import(t.Context(), opts)
	require.NoError(t, err)
	assert.Equal(t, jobID, repeated.Archive.JobID)
	assert.Equal(t, beforeRepeat, calls.Load())
	assert.Equal(t, int64(1), firstPages.Load())
	state, err = st.LoadXControlContext(t.Context(), source.ID)
	require.NoError(t, err)
	require.Len(t, state.SpendBuckets, 2)
	assert.Equal(t, []store.XSpendBucket{{Month: "2026-10", SpentMicros: 15_000, ReservedMicros: 3_000_000}, {Month: "2026-11", SpentMicros: 15_000}}, state.SpendBuckets)
}

func TestXArchiveMinimumPagesRespectRaisedLifetimeAndMonthlyCaps(t *testing.T) {
	for _, limiter := range []string{"job", "month"} {
		t.Run(limiter, func(t *testing.T) {
			st := testutil.NewTestStore(t)
			source := importTestSource(t, st, "1001")
			state, err := st.LoadXControlContext(t.Context(), source.ID)
			require.NoError(t, err)
			name := "example_owner"
			_, err = st.UpdateXControlContext(t.Context(), source.ID, state.Revision, store.XControlPatch{OwnerID: "1001", Username: &name})
			require.NoError(t, err)
			var calls atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				assert.Equal(t, "/tweets/search/all", r.URL.Path)
				assert.Equal(t, "10", r.URL.Query().Get("max_results"))
				next := ""
				if strings.HasPrefix(r.URL.Query().Get("query"), "from:") && r.URL.Query().Get("next_token") == "" {
					next = "second"
				}
				wireImportPage(w, nil, next)
			}))
			defer server.Close()
			credentials := &importCredentials{snapshot: CredentialSnapshot{OwnerID: "1001", Generation: 1, GrantedScopes: []string{"tweet.read", "users.read"}}}
			policy := importTestPolicy(t, "1001")
			cap := int64(25_000_000)
			if limiter == "job" {
				cap = 150_000
			} else {
				policy.MonthlyBudgetMicros = 150_000
			}
			options := ImportOptions{SourceID: source.ID, OwnerID: "1001", After: time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC), Archive: true, MaxSpendMicros: &cap}
			run := func() (*ImportSummary, error) {
				return NewImporter(st, NewClient(server.URL, server.Client()), policy, credentials).Import(t.Context(), options)
			}
			summary, err := run()
			require.ErrorIs(t, err, store.ErrXBudgetExceeded)
			assert.Equal(t, int64(1), calls.Load())
			require.NotNil(t, summary.Archive)
			id := summary.Archive.JobID
			_, err = run()
			require.ErrorIs(t, err, store.ErrXBudgetExceeded)
			assert.Equal(t, int64(1), calls.Load(), "repeating a cap must not renew allowance")
			if limiter == "job" {
				cap = 300_000
			} else {
				policy.MonthlyBudgetMicros = 300_000
			}
			summary, err = run()
			require.ErrorIs(t, err, store.ErrXBudgetExceeded)
			assert.Equal(t, id, summary.Archive.JobID)
			assert.Equal(t, int64(2), calls.Load())
			if limiter == "job" {
				cap = 450_000
			} else {
				policy.MonthlyBudgetMicros = 450_000
			}
			summary, err = run()
			require.NoError(t, err)
			assert.Equal(t, id, summary.Archive.JobID)
			assert.Equal(t, int64(3), calls.Load())
			assert.Equal(t, int64(450_000), summary.Archive.ReservedMicros)
			assert.Zero(t, summary.Archive.SpentMicros)
		})
	}
}

func TestXArchiveTokenReplayKeepsStoredWindow(t *testing.T) {
	st := testutil.NewTestStore(t)
	source := importTestSource(t, st, "1001")
	state, err := st.LoadXControlContext(t.Context(), source.ID)
	require.NoError(t, err)
	name := "example_owner"
	_, err = st.UpdateXControlContext(t.Context(), source.ID, state.Revision, store.XControlPatch{OwnerID: "1001", Username: &name})
	require.NoError(t, err)
	now := time.Date(2026, 10, 30, 9, 0, 0, 0, time.UTC)
	var starts, rejections atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/tweets/search/all", r.URL.Path)
		assert.Equal(t, now.Format(time.RFC3339), r.URL.Query().Get("end_time"))
		if strings.HasPrefix(r.URL.Query().Get("query"), "to:") {
			wireImportPage(w, nil, "")
			return
		}
		if r.URL.Query().Get("next_token") == "" {
			starts.Add(1)
			wireImportPage(w, []string{wireOwnPost("102", now.Add(-time.Hour))}, "old")
			return
		}
		if rejections.Add(1) == 1 {
			w.WriteHeader(400)
			fmt.Fprint(w, `{"title":"Invalid Request","errors":[{"parameters":{"next_token":["old"]}}]}`)
			return
		}
		wireImportPage(w, []string{wireOwnPost("101", now.Add(-2*time.Hour))}, "")
	}))
	defer server.Close()
	credentials := &importCredentials{snapshot: CredentialSnapshot{OwnerID: "1001", Generation: 1, GrantedScopes: []string{"tweet.read", "users.read"}}}
	imp := NewImporter(st, NewClient(server.URL, server.Client()), importTestPolicy(t, "1001"), credentials)
	imp.now = func() time.Time { return now }
	cap := int64(25_000_000)
	summary, err := imp.Import(t.Context(), ImportOptions{SourceID: source.ID, OwnerID: "1001", After: now.AddDate(-1, 0, 0), Archive: true, MaxSpendMicros: &cap})
	require.NoError(t, err)
	assert.Equal(t, int64(2), starts.Load())
	assert.Equal(t, []string{"101", "102"}, archivedXIDs(t, st, source.ID))
	assert.Equal(t, int64(2), summary.MessagesAdded)
	assert.True(t, summary.Archive.OwnComplete)
	assert.Equal(t, int64(3_000_000), summary.Archive.ReservedMicros)
}

func TestXArchivePreviewWorksWhileReadsAreDisabled(t *testing.T) {
	st := testutil.NewTestStore(t)
	source := importTestSource(t, st, "1001")
	state, err := st.LoadXControlContext(t.Context(), source.ID)
	require.NoError(t, err)
	username, credential, model := "example_owner", "invalid_grant", store.XModelErrorResourceShape
	_, err = st.UpdateXControlContext(t.Context(), source.ID, state.Revision, store.XControlPatch{OwnerID: "1001", Username: &username, CredentialError: &credential, ModelError: &model})
	require.NoError(t, err)
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(403) }))
	defer server.Close()
	credentials := &archiveCredentials{importCredentials: &importCredentials{snapshot: CredentialSnapshot{OwnerID: "1001", Generation: 1, GrantedScopes: []string{"tweet.read", "users.read"}}}}
	policy := importTestPolicy(t, "1001")
	policy.MonthlyBudgetMicros = 0
	summary, err := NewImporter(st, NewClient(server.URL, server.Client()), policy, credentials).Import(t.Context(), ImportOptions{SourceID: source.ID, OwnerID: "1001", After: time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC), Archive: true})
	require.NoError(t, err)
	assert.Equal(t, "preview", summary.Status)
	assert.Zero(t, calls.Load())
	assert.Zero(t, credentials.prepares.Load())
	assert.Equal(t, credential, summary.CredentialError)
	assert.Equal(t, model, summary.ModelError)
}

func TestXArchivePartialItemRepairUsesSameJob(t *testing.T) {
	st := testutil.NewTestStore(t)
	source := importTestSource(t, st, "1001")
	state, err := st.LoadXControlContext(t.Context(), source.ID)
	require.NoError(t, err)
	username := "example_owner"
	_, err = st.UpdateXControlContext(t.Context(), source.ID, state.Revision, store.XControlPatch{OwnerID: "1001", Username: &username})
	require.NoError(t, err)
	now := time.Now().UTC().Truncate(time.Second)
	var lookupsFail atomic.Bool
	lookupsFail.Store(true)
	var pages, lookups atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		state, readErr := st.LoadXControlContext(r.Context(), source.ID)
		if !assert.NoError(t, readErr) || !assert.Len(t, state.Jobs, 1) {
			w.WriteHeader(403)
			return
		}
		var newest store.XReservation
		for _, reservation := range state.Reservations {
			newest = reservation
		}
		assert.Equal(t, state.Jobs[0].ID, newest.JobID)
		if r.URL.Path == "/tweets" {
			lookups.Add(1)
			assert.Equal(t, "101", r.URL.Query().Get("ids"))
			if lookupsFail.Load() {
				w.WriteHeader(403)
				return
			}
			wireImportPage(w, []string{wireOwnPost("101", now.Add(-2*time.Minute))}, "")
			return
		}
		assert.Equal(t, "/tweets/search/all", r.URL.Path)
		if strings.HasPrefix(r.URL.Query().Get("query"), "to:") {
			wireImportPage(w, nil, "")
			return
		}
		pages.Add(1)
		if r.URL.Query().Get("next_token") == "" {
			fmt.Fprintf(w, `{"data":[%s],"includes":{"users":[{"id":"1001","username":"example_owner","protected":false}]},"errors":[{"resource_id":"101","title":"Synthetic transient item"}],"meta":{"next_token":"second"}}`, wireOwnPost("102", now.Add(-time.Minute)))
			return
		}
		wireImportPage(w, []string{wireOwnPost("100", now.Add(-3*time.Minute))}, "")
	}))
	defer server.Close()
	credentials := &importCredentials{snapshot: CredentialSnapshot{OwnerID: "1001", Generation: 1, GrantedScopes: []string{"tweet.read", "users.read"}}}
	policy := importTestPolicy(t, "1001")
	cap := int64(25_000_000)
	options := ImportOptions{SourceID: source.ID, OwnerID: "1001", After: now.AddDate(-1, 0, 0), Archive: true, MaxSpendMicros: &cap}
	run := func() (*ImportSummary, error) {
		return NewImporter(st, NewClient(server.URL, server.Client()), policy, credentials).Import(t.Context(), options)
	}
	first, err := run()
	require.Error(t, err)
	require.NotNil(t, first.Archive)
	jobID := first.Archive.JobID
	state, err = st.LoadXControlContext(t.Context(), source.ID)
	require.NoError(t, err)
	assert.Equal(t, []string{"101"}, state.Jobs[0].Own.UnresolvedIDs)
	assert.False(t, state.Jobs[0].Own.Complete)
	assert.Equal(t, int64(2), pages.Load())
	lookupsFail.Store(false)
	resumed, err := run()
	require.NoError(t, err)
	assert.Equal(t, jobID, resumed.Archive.JobID)
	assert.True(t, resumed.Archive.OwnComplete)
	assert.True(t, resumed.Archive.MentionsComplete)
	assert.Equal(t, int64(1), resumed.MessagesAdded)
	assert.Equal(t, int64(2), pages.Load())
	assert.Equal(t, int64(2), lookups.Load())
	assert.Equal(t, []string{"100", "101", "102"}, archivedXIDs(t, st, source.ID))
}

func TestXArchiveOwnQueryKeepsNumericOwnerBoundary(t *testing.T) {
	st := testutil.NewTestStore(t)
	source := importTestSource(t, st, "1001")
	state, err := st.LoadXControlContext(t.Context(), source.ID)
	require.NoError(t, err)
	username := "example_owner"
	_, err = st.UpdateXControlContext(t.Context(), source.ID, state.Revision, store.XControlPatch{OwnerID: "1001", Username: &username})
	require.NoError(t, err)
	now := time.Now().UTC().Truncate(time.Second)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Query().Get("query"), "to:") {
			wireImportPage(w, nil, "")
			return
		}
		foreign := strings.Replace(wireOwnPost("102", now.Add(-time.Minute)), `"author_id":"1001"`, `"author_id":"1002"`, 1)
		wireImportPage(w, []string{wireOwnPost("101", now.Add(-2*time.Minute)), foreign}, "")
	}))
	defer server.Close()
	credentials := &importCredentials{snapshot: CredentialSnapshot{OwnerID: "1001", Generation: 1, GrantedScopes: []string{"tweet.read", "users.read"}}}
	cap := int64(25_000_000)
	summary, err := NewImporter(st, NewClient(server.URL, server.Client()), importTestPolicy(t, "1001"), credentials).Import(t.Context(), ImportOptions{SourceID: source.ID, OwnerID: "1001", After: now.AddDate(-1, 0, 0), Archive: true, MaxSpendMicros: &cap})
	require.NoError(t, err)
	assert.Equal(t, []string{"101"}, archivedXIDs(t, st, source.ID))
	assert.Equal(t, int64(1), summary.MessagesAdded)
	assert.Equal(t, int64(20_000), summary.Archive.SpentMicros, "all returned posts and users remain billable, including scope exclusions")
	state, err = st.LoadXControlContext(t.Context(), source.ID)
	require.NoError(t, err)
	assert.Equal(t, "101", state.Jobs[0].Own.Watermark)
}
