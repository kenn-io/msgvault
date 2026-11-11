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
	_testAssert := assert.New(t)
	_testRequire := require.New(t)

	st := testutil.NewTestStore(t)
	source := importTestSource(t, st)
	name := "example_owner"
	state, err := st.LoadXControlContext(t.Context(), source.ID)
	_testRequire.NoError(err)
	_, err = st.UpdateXControlContext(t.Context(), source.ID, state.Revision, store.XControlPatch{OwnerID: "1001", Username: &name})
	_testRequire.NoError(err)
	_testRequire.NoError(st.UpdateSourceDisplayName(source.ID, "Synthetic archive label"))
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(http.StatusForbidden) }))
	defer server.Close()
	credentials := &archiveCredentials{importCredentials: &importCredentials{snapshot: CredentialSnapshot{OwnerID: "1001", Generation: 1, GrantedScopes: []string{"tweet.read", "users.read"}}}}
	imp := NewImporter(st, NewClient(server.URL, server.Client()), importTestPolicy(t), credentials)
	now := time.Date(2026, 10, 30, 9, 0, 0, 0, time.UTC)
	imp.now = func() time.Time { return now }
	after := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	summary, err := imp.Import(t.Context(), ImportOptions{SourceID: source.ID, OwnerID: "1001", After: after, Archive: true})
	_testRequire.NoError(err)
	_testAssert.Zero(calls.Load(), "preview must not call Me, search, counts, sampling or ordinary timelines")
	_testAssert.Zero(credentials.prepares.Load(), "preview must not refresh credentials")
	_testRequire.NotNil(summary)
	_testAssert.Equal("preview", summary.Status)
	_testAssert.Zero(summary.SyncRunID)
	_testRequire.NotNil(summary.Archive)
	_testAssert.Equal("from:example_owner -is:retweet", summary.Archive.OwnQuery)
	_testAssert.Equal("to:example_owner -is:retweet", summary.Archive.MentionsQuery)
	_testAssert.Equal(after, summary.Archive.After)
	_testAssert.Equal(now, summary.Archive.Before)
	_testAssert.Equal(int64(1_500_000), summary.Archive.PageReservationMicros)
	_testAssert.True(summary.Archive.TotalUnknown)
	current, err := st.LoadXControlContext(t.Context(), source.ID)
	_testRequire.NoError(err)
	_testAssert.Empty(current.Jobs)
	_testAssert.Empty(current.Reservations)
	_testAssert.Nil(current.Own)
	_testAssert.Nil(current.Mentions)
}

func TestXArchiveResumeDoesNotRenewCap(t *testing.T) {
	_testAssert := assert.New(t)
	_testRequire := require.New(t)

	st := testutil.NewTestStore(t)
	source := importTestSource(t, st)
	name := "example_owner"
	state, err := st.LoadXControlContext(t.Context(), source.ID)
	_testRequire.NoError(err)
	_, err = st.UpdateXControlContext(t.Context(), source.ID, state.Revision, store.XControlPatch{OwnerID: "1001", Username: &name})
	_testRequire.NoError(err)
	now := time.Date(2026, 10, 30, 9, 0, 0, 0, time.UTC)
	after := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	var reject atomic.Bool
	reject.Store(true)
	var calls, firstPages atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if !assert.Equal(t, "/tweets/search/all", r.URL.Path) {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		control, readErr := st.LoadXControlContext(r.Context(), source.ID)
		if !assert.NoError(t, readErr) || !assert.Len(t, control.Jobs, 1) {
			w.WriteHeader(http.StatusForbidden)
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
				w.WriteHeader(http.StatusForbidden)
				return
			}
			wireImportPage(w, []string{wireOwnPost("101", now.Add(-2*time.Hour))}, "")
		case "to:example_owner -is:retweet":
			wireImportPage(w, nil, "")
		default:
			assert.Fail(t, "archive query changed from its persisted identity")
			w.WriteHeader(http.StatusForbidden)
		}
	}))
	defer server.Close()
	credentials := &importCredentials{snapshot: CredentialSnapshot{OwnerID: "1001", Generation: 1, GrantedScopes: []string{"tweet.read", "users.read"}}}
	policy := importTestPolicy(t)
	// This contract isolates cap persistence across month rollover. Maintenance
	// has a separate refresh-first regression; keep these public rows fresh for
	// the two-month clock advances rather than spending on unrelated lookups.
	policy.RefreshSweepInterval = 90 * 24 * time.Hour
	maxSpend := int64(25_000_000)
	makeImporter := func(at time.Time) *Importer {
		imp := NewImporter(st, NewClient(server.URL, server.Client()), policy, credentials)
		imp.now = func() time.Time { return at }
		return imp
	}
	opts := ImportOptions{SourceID: source.ID, OwnerID: "1001", After: after, Archive: true, MaxSpendMicros: &maxSpend}
	first, err := makeImporter(now).Import(t.Context(), opts)
	_testRequire.Error(err)
	_testRequire.NotNil(first)
	_testRequire.NotNil(first.Archive)
	_testRequire.NotEmpty(first.Archive.JobID)
	jobID := first.Archive.JobID
	_testAssert.Equal(int64(15_000), first.Archive.SpentMicros)
	_testAssert.Equal(int64(3_000_000), first.Archive.ReservedMicros)
	_testRequire.NotNil(first.Archive.RemainingMicros)
	_testAssert.Equal(int64(21_985_000), *first.Archive.RemainingMicros)
	state, err = st.LoadXControlContext(t.Context(), source.ID)
	_testRequire.NoError(err)
	_testRequire.Len(state.Jobs, 1)
	_testAssert.Equal("second", state.Jobs[0].Own.Token)
	_testAssert.False(state.Jobs[0].Own.Complete)
	_testAssert.True(state.Jobs[0].Mentions.Complete)
	_testAssert.Nil(state.Own)
	_testAssert.Nil(state.Mentions)
	name = "renamed_owner"
	generation := int64(2)
	_, err = st.UpdateXControlContext(t.Context(), source.ID, state.Revision, store.XControlPatch{OwnerID: "1001", Username: &name, Generation: &generation})
	_testRequire.NoError(err)
	credentials.snapshot.Generation = 2
	reject.Store(false)
	resumed, err := makeImporter(now.AddDate(0, 1, 0)).Import(t.Context(), opts)
	_testRequire.NoError(err)
	_testRequire.NotNil(resumed.Archive)
	_testAssert.Equal(jobID, resumed.Archive.JobID)
	_testAssert.Equal(now, resumed.Archive.Before)
	_testAssert.Equal("example_owner", resumed.Archive.Username)
	_testAssert.True(resumed.Archive.OwnComplete)
	_testAssert.True(resumed.Archive.MentionsComplete)
	_testAssert.Equal(int64(1), resumed.MessagesAdded)
	_testAssert.Equal(int64(15_000), resumed.RunEstimatedMicros)
	_testAssert.Equal(int64(15_000), resumed.EstimatedMicros)
	_testAssert.Zero(resumed.ReservedMicros)
	_testAssert.Equal(int64(30_000), resumed.Archive.SpentMicros)
	_testAssert.Equal(int64(3_000_000), resumed.Archive.ReservedMicros)
	_testAssert.Equal([]string{"101", "102"}, archivedXIDs(t, st, source.ID))
	beforeRepeat := calls.Load()
	repeated, err := makeImporter(now.AddDate(0, 2, 0)).Import(t.Context(), opts)
	_testRequire.NoError(err)
	_testAssert.Equal(jobID, repeated.Archive.JobID)
	_testAssert.Equal(beforeRepeat, calls.Load())
	_testAssert.Equal(int64(1), firstPages.Load())
	state, err = st.LoadXControlContext(t.Context(), source.ID)
	_testRequire.NoError(err)
	_testRequire.Len(state.SpendBuckets, 2)
	_testAssert.Equal([]store.XSpendBucket{{Month: "2026-10", SpentMicros: 15_000, ReservedMicros: 3_000_000}, {Month: "2026-11", SpentMicros: 15_000}}, state.SpendBuckets)
}

func TestXArchiveMinimumPagesRespectRaisedLifetimeAndMonthlyCaps(t *testing.T) {
	for _, limiter := range []string{"job", "month"} {
		t.Run(limiter, func(t *testing.T) {
			_testAssert := assert.New(t)
			_testRequire := require.New(t)

			st := testutil.NewTestStore(t)
			source := importTestSource(t, st)
			state, err := st.LoadXControlContext(t.Context(), source.ID)
			_testRequire.NoError(err)
			name := "example_owner"
			_, err = st.UpdateXControlContext(t.Context(), source.ID, state.Revision, store.XControlPatch{OwnerID: "1001", Username: &name})
			_testRequire.NoError(err)
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
			policy := importTestPolicy(t)
			maxSpend := int64(25_000_000)
			if limiter == "job" {
				maxSpend = 150_000
			} else {
				policy.MonthlyBudgetMicros = 150_000
			}
			options := ImportOptions{SourceID: source.ID, OwnerID: "1001", After: time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC), Archive: true, MaxSpendMicros: &maxSpend}
			run := func() (*ImportSummary, error) {
				return NewImporter(st, NewClient(server.URL, server.Client()), policy, credentials).Import(t.Context(), options)
			}
			summary, err := run()
			_testRequire.ErrorIs(err, store.ErrXBudgetExceeded)
			_testAssert.Equal(int64(1), calls.Load())
			_testRequire.NotNil(summary.Archive)
			id := summary.Archive.JobID
			_, err = run()
			_testRequire.ErrorIs(err, store.ErrXBudgetExceeded)
			_testAssert.Equal(int64(1), calls.Load(), "repeating a cap must not renew allowance")
			if limiter == "job" {
				maxSpend = 300_000
			} else {
				policy.MonthlyBudgetMicros = 300_000
			}
			summary, err = run()
			_testRequire.ErrorIs(err, store.ErrXBudgetExceeded)
			_testAssert.Equal(id, summary.Archive.JobID)
			_testAssert.Equal(int64(2), calls.Load())
			if limiter == "job" {
				maxSpend = 450_000
			} else {
				policy.MonthlyBudgetMicros = 450_000
			}
			summary, err = run()
			_testRequire.NoError(err)
			_testAssert.Equal(id, summary.Archive.JobID)
			_testAssert.Equal(int64(3), calls.Load())
			_testAssert.Equal(int64(450_000), summary.Archive.ReservedMicros)
			_testAssert.Zero(summary.Archive.SpentMicros)
		})
	}
}

func TestXArchiveTokenReplayKeepsStoredWindow(t *testing.T) {
	_testAssert := assert.New(t)
	_testRequire := require.New(t)

	st := testutil.NewTestStore(t)
	source := importTestSource(t, st)
	state, err := st.LoadXControlContext(t.Context(), source.ID)
	_testRequire.NoError(err)
	name := "example_owner"
	_, err = st.UpdateXControlContext(t.Context(), source.ID, state.Revision, store.XControlPatch{OwnerID: "1001", Username: &name})
	_testRequire.NoError(err)
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
			w.WriteHeader(http.StatusBadRequest)
			_, _ = fmt.Fprint(w, `{"title":"Invalid Request","errors":[{"parameters":{"next_token":["old"]}}]}`)
			return
		}
		wireImportPage(w, []string{wireOwnPost("101", now.Add(-2*time.Hour))}, "")
	}))
	defer server.Close()
	credentials := &importCredentials{snapshot: CredentialSnapshot{OwnerID: "1001", Generation: 1, GrantedScopes: []string{"tweet.read", "users.read"}}}
	imp := NewImporter(st, NewClient(server.URL, server.Client()), importTestPolicy(t), credentials)
	imp.now = func() time.Time { return now }
	maxSpend := int64(25_000_000)
	summary, err := imp.Import(t.Context(), ImportOptions{SourceID: source.ID, OwnerID: "1001", After: now.AddDate(-1, 0, 0), Archive: true, MaxSpendMicros: &maxSpend})
	_testRequire.NoError(err)
	_testAssert.Equal(int64(2), starts.Load())
	_testAssert.Equal([]string{"101", "102"}, archivedXIDs(t, st, source.ID))
	_testAssert.Equal(int64(2), summary.MessagesAdded)
	_testAssert.True(summary.Archive.OwnComplete)
	_testAssert.Equal(int64(3_000_000), summary.Archive.ReservedMicros)
}

func TestXArchivePreviewWorksWhileReadsAreDisabled(t *testing.T) {
	_testAssert := assert.New(t)
	_testRequire := require.New(t)

	st := testutil.NewTestStore(t)
	source := importTestSource(t, st)
	state, err := st.LoadXControlContext(t.Context(), source.ID)
	_testRequire.NoError(err)
	username, credential, model := "example_owner", "invalid_grant", store.XModelErrorResourceShape
	_, err = st.UpdateXControlContext(t.Context(), source.ID, state.Revision, store.XControlPatch{OwnerID: "1001", Username: &username, CredentialError: &credential, ModelError: &model})
	_testRequire.NoError(err)
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(http.StatusForbidden) }))
	defer server.Close()
	credentials := &archiveCredentials{importCredentials: &importCredentials{snapshot: CredentialSnapshot{OwnerID: "1001", Generation: 1, GrantedScopes: []string{"tweet.read", "users.read"}}}}
	policy := importTestPolicy(t)
	policy.MonthlyBudgetMicros = 0
	summary, err := NewImporter(st, NewClient(server.URL, server.Client()), policy, credentials).Import(t.Context(), ImportOptions{SourceID: source.ID, OwnerID: "1001", After: time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC), Archive: true})
	_testRequire.NoError(err)
	_testAssert.Equal("preview", summary.Status)
	_testAssert.Zero(calls.Load())
	_testAssert.Zero(credentials.prepares.Load())
	_testAssert.Equal(credential, summary.CredentialError)
	_testAssert.Equal(model, summary.ModelError)
}

func TestXArchivePartialItemRepairUsesSameJob(t *testing.T) {
	_testAssert := assert.New(t)
	_testRequire := require.New(t)

	st := testutil.NewTestStore(t)
	source := importTestSource(t, st)
	state, err := st.LoadXControlContext(t.Context(), source.ID)
	_testRequire.NoError(err)
	username := "example_owner"
	_, err = st.UpdateXControlContext(t.Context(), source.ID, state.Revision, store.XControlPatch{OwnerID: "1001", Username: &username})
	_testRequire.NoError(err)
	now := time.Now().UTC().Truncate(time.Second)
	var lookupsFail atomic.Bool
	lookupsFail.Store(true)
	var pages, lookups atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		state, readErr := st.LoadXControlContext(r.Context(), source.ID)
		if !assert.NoError(t, readErr) || !assert.Len(t, state.Jobs, 1) {
			w.WriteHeader(http.StatusForbidden)
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
				w.WriteHeader(http.StatusForbidden)
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
			_, _ = fmt.Fprintf(w, `{"data":[%s],"includes":{"users":[{"id":"1001","username":"example_owner","protected":false}]},"errors":[{"resource_type":"tweet","resource_id":"101","title":"Synthetic transient item"}],"meta":{"next_token":"second"}}`, wireOwnPost("102", now.Add(-time.Minute)))
			return
		}
		wireImportPage(w, []string{wireOwnPost("100", now.Add(-3*time.Minute))}, "")
	}))
	defer server.Close()
	credentials := &importCredentials{snapshot: CredentialSnapshot{OwnerID: "1001", Generation: 1, GrantedScopes: []string{"tweet.read", "users.read"}}}
	policy := importTestPolicy(t)
	maxSpend := int64(25_000_000)
	options := ImportOptions{SourceID: source.ID, OwnerID: "1001", After: now.AddDate(-1, 0, 0), Archive: true, MaxSpendMicros: &maxSpend}
	run := func() (*ImportSummary, error) {
		return NewImporter(st, NewClient(server.URL, server.Client()), policy, credentials).Import(t.Context(), options)
	}
	first, err := run()
	_testRequire.Error(err)
	_testRequire.NotNil(first.Archive)
	jobID := first.Archive.JobID
	state, err = st.LoadXControlContext(t.Context(), source.ID)
	_testRequire.NoError(err)
	_testAssert.Equal([]string{"101"}, state.Jobs[0].Own.UnresolvedIDs)
	_testAssert.False(state.Jobs[0].Own.Complete)
	_testAssert.Equal(int64(2), pages.Load())
	lookupsFail.Store(false)
	resumed, err := run()
	_testRequire.NoError(err)
	_testAssert.Equal(jobID, resumed.Archive.JobID)
	_testAssert.True(resumed.Archive.OwnComplete)
	_testAssert.True(resumed.Archive.MentionsComplete)
	_testAssert.Equal(int64(1), resumed.MessagesAdded)
	_testAssert.Equal(int64(2), pages.Load())
	_testAssert.Equal(int64(2), lookups.Load())
	_testAssert.Equal([]string{"100", "101", "102"}, archivedXIDs(t, st, source.ID))
}

func TestXArchiveOwnQueryKeepsNumericOwnerBoundary(t *testing.T) {
	_testAssert := assert.New(t)
	_testRequire := require.New(t)

	st := testutil.NewTestStore(t)
	source := importTestSource(t, st)
	state, err := st.LoadXControlContext(t.Context(), source.ID)
	_testRequire.NoError(err)
	username := "example_owner"
	_, err = st.UpdateXControlContext(t.Context(), source.ID, state.Revision, store.XControlPatch{OwnerID: "1001", Username: &username})
	_testRequire.NoError(err)
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
	maxSpend := int64(25_000_000)
	summary, err := NewImporter(st, NewClient(server.URL, server.Client()), importTestPolicy(t), credentials).Import(t.Context(), ImportOptions{SourceID: source.ID, OwnerID: "1001", After: now.AddDate(-1, 0, 0), Archive: true, MaxSpendMicros: &maxSpend})
	_testRequire.NoError(err)
	_testAssert.Equal([]string{"101"}, archivedXIDs(t, st, source.ID))
	_testAssert.Equal(int64(1), summary.MessagesAdded)
	_testAssert.Equal(int64(20_000), summary.Archive.SpentMicros, "all returned posts and users remain billable, including scope exclusions")
	state, err = st.LoadXControlContext(t.Context(), source.ID)
	_testRequire.NoError(err)
	_testAssert.Equal("101", state.Jobs[0].Own.Watermark)
}
