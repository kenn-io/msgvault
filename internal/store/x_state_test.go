package store_test

import (
	"context"
	"encoding/json/v2"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestXControlCompletionPreservesReservation(t *testing.T) {
	_testAssert := assert.New(t)
	_testRequire := require.New(t)

	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("x", "100")
	_testRequire.NoError(err)
	state, err := st.LoadXControlContext(t.Context(), source.ID)
	_testRequire.NoError(err)
	reservations := []store.XReservation{{ID: "request-1", Kind: "timeline", Month: "2026-10", AmountMicros: 15000, State: "unknown"}}
	state, err = st.UpdateXControlContext(t.Context(), source.ID, state.Revision, store.XControlPatch{OwnerID: "100", Reservations: &reservations})
	_testRequire.NoError(err)
	stale := state.Revision
	generation := int64(2)
	_, err = st.UpdateXControlContext(t.Context(), source.ID, state.Revision, store.XControlPatch{OwnerID: "100", Generation: &generation})
	_testRequire.NoError(err)
	run, err := st.StartSync(source.ID, "x")
	_testRequire.NoError(err)
	scoped := st.ScopedToSync(source.ID, run)
	walk := store.XWalk{Endpoint: "own", Fingerprint: "own-window", Complete: true, MaxObservedID: "200"}
	err = scoped.CompleteXSyncContext(t.Context(), run, store.XControlPatch{OwnerID: "100", ExpectedRevision: stale, Own: &walk})
	_testRequire.ErrorIs(err, store.ErrXControlConflict)
	got, err := st.LoadXControlContext(t.Context(), source.ID)
	_testRequire.NoError(err)
	_testAssert.Equal(reservations, got.Reservations)
	_testAssert.Nil(got.Own)
	// A failed terminal write releases execution ownership and can be recovered.
	execution, err := st.AcquireSyncExecutionContext(t.Context(), source.ID)
	_testRequire.NoError(err)
	defer func() { require.NoError(t, execution.Release()) }()
	run, err = execution.StartSyncContext(t.Context(), "x", "")
	_testRequire.NoError(err)
	scoped = st.ScopedToSync(source.ID, run)
	_testRequire.NoError(scoped.UpdateSyncCheckpointContext(t.Context(), run, &store.Checkpoint{MessagesProcessed: 3, MessagesAdded: 2, MessagesUpdated: 1, PageToken: "diagnostic-only"}))
	_testRequire.NoError(scoped.CompleteXSyncContext(t.Context(), run, store.XControlPatch{OwnerID: "100", ExpectedRevision: got.Revision, Own: &walk}))
	got, err = st.LoadXControlContext(t.Context(), source.ID)
	_testRequire.NoError(err)
	_testAssert.Equal(reservations, got.Reservations)
	_testAssert.Equal(int64(2), got.Generation)
	_testRequire.NotNil(got.Own)
	_testAssert.True(got.Own.Complete)
	var status, checkpoint string
	var processed, added, updated int64
	_testRequire.NoError(st.DB().QueryRow(st.Rebind("SELECT status,cursor_before,messages_processed,messages_added,messages_updated FROM sync_runs WHERE id=?"), run).Scan(&status, &checkpoint, &processed, &added, &updated))
	_testAssert.Equal("completed", status)
	_testAssert.Equal("diagnostic-only", checkpoint)
	_testAssert.Equal(int64(3), processed)
	_testAssert.Equal(int64(2), added)
	_testAssert.Equal(int64(1), updated)
	_testRequire.ErrorIs(scoped.CompleteXSyncContext(t.Context(), run, store.XControlPatch{OwnerID: "100", ExpectedRevision: got.Revision}), store.ErrSyncRunSuperseded)
}

func TestXControlRejectsCorruptState(t *testing.T) {
	for _, raw := range []string{`{`, `null`, `{}`, `{"version":1,"owner_id":"100","revision":null}`, `{"version":1,"owner_id":"100","revision":0,"generation":null}`, `{"version":2,"owner_id":"100"}`, `{"version":1,"owner_id":"100","revision":0,"unrecognized":1}`, `{"version":1,"owner_id":"101","revision":0}`, `{"version":1,"owner_id":"100","revision":0,"reservations":[{"id":"r","month":"2026-10","amount_micros":-1,"state":"unknown"}]}`} {
		t.Run(raw, func(t *testing.T) {
			_testRequire := require.New(t)

			st := testutil.NewTestStore(t)
			source, err := st.GetOrCreateSource("x", "100")
			_testRequire.NoError(err)
			_testRequire.NoError(st.UpdateSourceSyncCursor(source.ID, raw))
			_, err = st.LoadXControlContext(t.Context(), source.ID)
			_testRequire.Error(err)
			_, err = st.UpdateXControlContext(t.Context(), source.ID, 0, store.XControlPatch{OwnerID: "100"})
			_testRequire.Error(err)
			got, err := st.GetSourceByID(source.ID)
			_testRequire.NoError(err)
			assert.Equal(t, raw, got.SyncCursor.String)
		})
	}
}

func TestXControlSourceRevisionAndCapacity(t *testing.T) {
	_testRequire := require.New(t)

	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("x", "100")
	_testRequire.NoError(err)
	_, err = st.UpdateXControlContext(t.Context(), source.ID, 0, store.XControlPatch{OwnerID: "101"})
	_testRequire.Error(err)
	_, err = st.UpdateXControlContext(t.Context(), source.ID, 1, store.XControlPatch{OwnerID: "100"})
	_testRequire.ErrorIs(err, store.ErrXControlConflict)
	reservations := make([]store.XReservation, store.XMaxReservations+1)
	for i := range reservations {
		reservations[i] = store.XReservation{
			ID: "capacity-" + strconv.Itoa(i), Kind: "timeline", Month: "2026-10",
			AmountMicros: 1, State: "unknown",
		}
	}
	_, err = st.UpdateXControlContext(t.Context(), source.ID, 0, store.XControlPatch{OwnerID: "100", Reservations: &reservations})
	_testRequire.Error(err)
	other, err := st.GetOrCreateSource("gmail", "owner@example.com")
	_testRequire.NoError(err)
	_, err = st.LoadXControlContext(t.Context(), other.ID)
	_testRequire.Error(err)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = st.LoadXControlContext(ctx, source.ID)
	_testRequire.ErrorIs(err, context.Canceled)
	run, err := st.StartSync(source.ID, "x")
	_testRequire.NoError(err)
	scoped := st.ScopedToSync(source.ID, run)
	_testRequire.NoError(st.FailSync(run, "synthetic failure"))
	_, err = scoped.UpdateXControlContext(t.Context(), source.ID, 0, store.XControlPatch{OwnerID: "100"})
	_testRequire.ErrorIs(err, store.ErrSyncRunSuperseded)
	ctx, cancel = context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	execution, err := st.AcquireSyncExecutionContext(ctx, source.ID)
	_testRequire.NoError(err)
	_testRequire.NoError(execution.Release())
}

func TestXCompletionRollbackAndSourceScope(t *testing.T) {
	_testAssert := assert.New(t)
	_testRequire := require.New(t)

	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("x", "100")
	_testRequire.NoError(err)
	other, err := st.GetOrCreateSource("x", "101")
	_testRequire.NoError(err)
	state, err := st.UpdateXControlContext(t.Context(), source.ID, 0, store.XControlPatch{OwnerID: "100"})
	_testRequire.NoError(err)
	run, err := st.StartSync(source.ID, "x")
	_testRequire.NoError(err)
	scoped := st.ScopedToSync(source.ID, run)
	_, err = scoped.UpdateXControlContext(t.Context(), other.ID, 0, store.XControlPatch{OwnerID: "101"})
	_testRequire.Error(err)
	_testRequire.Error(st.CompleteXSyncContext(t.Context(), run, store.XControlPatch{OwnerID: "100", ExpectedRevision: state.Revision}))
	if st.IsPostgreSQL() {
		_, err = st.DB().Exec(`CREATE FUNCTION reject_x_completion() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.status='completed' THEN RAISE EXCEPTION 'synthetic completion failure'; END IF; RETURN NEW; END $$; CREATE TRIGGER reject_x_completion BEFORE UPDATE ON sync_runs FOR EACH ROW EXECUTE FUNCTION reject_x_completion()`)
	} else {
		_, err = st.DB().Exec(`CREATE TRIGGER reject_x_completion BEFORE UPDATE ON sync_runs WHEN NEW.status='completed' BEGIN SELECT RAISE(ABORT,'synthetic completion failure'); END`)
	}
	_testRequire.NoError(err)
	walk := store.XWalk{Endpoint: "own", Fingerprint: "synthetic-window", Complete: true}
	_testRequire.ErrorContains(scoped.CompleteXSyncContext(t.Context(), run, store.XControlPatch{OwnerID: "100", ExpectedRevision: state.Revision, Own: &walk}), "synthetic completion failure")
	got, err := st.LoadXControlContext(t.Context(), source.ID)
	_testRequire.NoError(err)
	_testAssert.Equal(state.Revision, got.Revision)
	_testAssert.Nil(got.Own)
	var status string
	_testRequire.NoError(st.DB().QueryRow(st.Rebind("SELECT status FROM sync_runs WHERE id=?"), run).Scan(&status))
	_testAssert.Equal("running", status)
	execution, err := st.AcquireSyncExecutionContext(t.Context(), source.ID)
	_testRequire.NoError(err)
	_testRequire.NoError(execution.Release())
	_testRequire.NoError(st.DB().QueryRow(st.Rebind("SELECT status FROM sync_runs WHERE id=?"), run).Scan(&status))
	_testAssert.Equal("failed", status)
}

func TestXControlReservationShapeAndRates(t *testing.T) {
	for _, tc := range []struct {
		name    string
		value   store.XReservation
		invalid bool
	}{
		{name: "read", value: store.XReservation{ID: "read-1", Kind: "timeline", Month: "2026-10", AmountMicros: 75000, MaxPosts: 5, MaxUsers: 5, PostPriceMicros: 5000, UserPriceMicros: 10000, State: "unknown"}},
		{name: "zero-shape", value: store.XReservation{ID: "create-1", Kind: "create", Month: "2026-10", AmountMicros: 200000, State: "pending"}},
		{name: "negative-post-count", value: store.XReservation{ID: "read-1", Kind: "timeline", Month: "2026-10", State: "unknown", MaxPosts: -1}, invalid: true},
		{name: "negative-user-count", value: store.XReservation{ID: "read-1", Kind: "timeline", Month: "2026-10", State: "unknown", MaxUsers: -1}, invalid: true},
		{name: "negative-post-price", value: store.XReservation{ID: "read-1", Kind: "timeline", Month: "2026-10", State: "unknown", PostPriceMicros: -1}, invalid: true},
		{name: "negative-user-price", value: store.XReservation{ID: "read-1", Kind: "timeline", Month: "2026-10", State: "unknown", UserPriceMicros: -1}, invalid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_testAssert := assert.New(t)
			_testRequire := require.New(t)

			st := testutil.NewTestStore(t)
			source, err := st.GetOrCreateSource("x", "100")
			_testRequire.NoError(err)
			reservations := []store.XReservation{tc.value}
			_, err = st.UpdateXControlContext(t.Context(), source.ID, 0, store.XControlPatch{OwnerID: "100", Reservations: &reservations})
			if tc.invalid {
				_testRequire.Error(err)
				return
			}
			_testRequire.NoError(err)
			got, err := st.LoadXControlContext(t.Context(), source.ID)
			_testRequire.NoError(err)
			_testAssert.Equal(reservations, got.Reservations)
			var raw string
			_testRequire.NoError(st.DB().QueryRow(st.Rebind("SELECT sync_cursor FROM sources WHERE id=?"), source.ID).Scan(&raw))
			var encoded map[string]any
			_testRequire.NoError(json.Unmarshal([]byte(raw), &encoded))
			reservationValues, ok := encoded["reservations"].([]any)
			_testRequire.True(ok)
			item, ok := reservationValues[0].(map[string]any)
			_testRequire.True(ok)
			_testAssert.InDelta(float64(tc.value.MaxPosts), item["max_posts"], 0)
			_testAssert.InDelta(float64(tc.value.MaxUsers), item["max_users"], 0)
			_testAssert.InDelta(float64(tc.value.PostPriceMicros), item["post_price_micros"], 0)
			_testAssert.InDelta(float64(tc.value.UserPriceMicros), item["user_price_micros"], 0)
		})
	}
}

func TestXControlConcurrentRevision(t *testing.T) {
	_testAssert := assert.New(t)
	_testRequire := require.New(t)

	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("x", "100")
	_testRequire.NoError(err)
	start := make(chan struct{})
	results := make(chan error, 2)
	for _, generation := range []int64{1, 2} {
		go func() {
			<-start
			_, err := st.UpdateXControlContext(t.Context(), source.ID, 0, store.XControlPatch{OwnerID: "100", Generation: &generation})
			results <- err
		}()
	}
	close(start)
	errorsSeen := []error{<-results, <-results}
	successes, conflicts := 0, 0
	for _, err := range errorsSeen {
		if err == nil {
			successes++
		} else {
			_testRequire.ErrorIs(err, store.ErrXControlConflict)
			conflicts++
		}
	}
	_testAssert.Equal(1, successes)
	_testAssert.Equal(1, conflicts)
	state, err := st.LoadXControlContext(t.Context(), source.ID)
	_testRequire.NoError(err)
	_testAssert.Equal(int64(1), state.Revision)
}

func TestXArchiveJobPreparationKeepsLifetimeIdentity(t *testing.T) {
	_testAssert := assert.New(t)
	_testRequire := require.New(t)

	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("x", "1001")
	_testRequire.NoError(err)
	username := "example_owner"
	generation := int64(1)
	_, err = st.UpdateXControlContext(t.Context(), source.ID, 0, store.XControlPatch{OwnerID: "1001", Username: &username, Generation: &generation})
	_testRequire.NoError(err)
	after := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	before := time.Date(2026, 10, 30, 9, 0, 0, 0, time.UTC)
	maxSpend := int64(25_000_000)
	spec := store.XArchiveJobSpec{OwnerID: "1001", After: after, Before: before, CapMicros: &maxSpend}
	job, err := st.PrepareXArchiveJobContext(t.Context(), source.ID, spec)
	_testRequire.NoError(err)
	_testRequire.NotEmpty(job.ID)
	_testRequire.NotNil(job.Own)
	_testRequire.NotNil(job.Mentions)
	_testAssert.Equal("example_owner", job.Username)
	_testAssert.Equal("from:example_owner -is:retweet", job.Own.Query)
	_testAssert.Equal("to:example_owner -is:retweet", job.Mentions.Query)
	_testAssert.Equal(after, job.After)
	_testAssert.Equal(before, job.Before)
	cost := store.XRequestCost{Kind: store.XRequestSearch, OwnerID: "1001", Generation: 1, GrantedScopes: []string{"tweet.read", "users.read"}, MonthlyBudgetMicros: 100_000_000, RequestedResults: 100, PostPriceMicros: 5000, UserPriceMicros: 10000, JobID: job.ID, JobFingerprint: job.Fingerprint, Query: job.Own.Query, After: after, Before: before, At: before}
	known, err := st.ReserveXRequestContext(t.Context(), source.ID, cost)
	_testRequire.NoError(err)
	_testRequire.NoError(st.SettleXRequestContext(t.Context(), source.ID, known.ID, store.XResourceUsage{Posts: 1, Users: 1, Persisted: true, Complete: true}))
	cost.Query = job.Mentions.Query
	_, err = st.ReserveXRequestContext(t.Context(), source.ID, cost)
	_testRequire.NoError(err)
	state, err := st.LoadXControlContext(t.Context(), source.ID)
	_testRequire.NoError(err)
	username = "renamed_owner"
	generation = 2
	_, err = st.UpdateXControlContext(t.Context(), source.ID, state.Revision, store.XControlPatch{OwnerID: "1001", Username: &username, Generation: &generation})
	_testRequire.NoError(err)
	spec.Before = before.AddDate(0, 1, 0)
	resumed, err := st.PrepareXArchiveJobContext(t.Context(), source.ID, spec)
	_testRequire.NoError(err)
	_testAssert.Equal(job.ID, resumed.ID)
	_testAssert.Equal(job.Fingerprint, resumed.Fingerprint)
	_testAssert.Equal(job.Before, resumed.Before)
	_testAssert.Equal(job.Username, resumed.Username)
	_testAssert.Equal(job.Own.Query, resumed.Own.Query)
	_testAssert.Equal(int64(15_000), resumed.SpentMicros)
	_testAssert.Equal(int64(1_500_000), resumed.ReservedMicros)
	maxSpend = 30_000_000
	raised, err := st.PrepareXArchiveJobContext(t.Context(), source.ID, spec)
	_testRequire.NoError(err)
	_testAssert.Equal(job.ID, raised.ID)
	_testAssert.Equal(maxSpend, raised.CapMicros)
	_testAssert.Equal(resumed.SpentMicros, raised.SpentMicros)
	_testAssert.Equal(resumed.ReservedMicros, raised.ReservedMicros)
	state, err = st.LoadXControlContext(t.Context(), source.ID)
	_testRequire.NoError(err)
	_testRequire.Len(state.Jobs, 1)
	completed := state.Jobs[0]
	completed.Complete = true
	completed.Own.Complete = true
	completed.Mentions.Complete = true
	jobs := []store.XArchiveJob{completed}
	_, err = st.UpdateXControlContext(t.Context(), source.ID, state.Revision, store.XControlPatch{OwnerID: "1001", Jobs: &jobs})
	_testRequire.NoError(err)
	repeated, err := st.PrepareXArchiveJobContext(t.Context(), source.ID, spec)
	_testRequire.NoError(err)
	_testAssert.True(repeated.Complete)
	_testAssert.Equal(job.ID, repeated.ID)
	_testAssert.Equal(before, repeated.Before)
	lower := int64(20_000_000)
	spec.JobID = job.ID
	spec.CapMicros = &lower
	_, err = st.PrepareXArchiveJobContext(t.Context(), source.ID, spec)
	_testRequire.Error(err)
	spec.CapMicros = &maxSpend
	spec.JobID = ""
	spec.NewJob = true
	fresh, err := st.PrepareXArchiveJobContext(t.Context(), source.ID, spec)
	_testRequire.NoError(err)
	_testAssert.NotEqual(job.ID, fresh.ID)
	_testAssert.Equal("renamed_owner", fresh.Username)
	_testAssert.Equal(spec.Before, fresh.Before)
	spec.NewJob = false
	spec.After = after.AddDate(-1, 0, 0)
	other, err := st.PrepareXArchiveJobContext(t.Context(), source.ID, spec)
	_testRequire.NoError(err)
	_testAssert.NotEqual(fresh.ID, other.ID)
	_testAssert.NotEqual(job.ID, other.ID)
}

func TestXArchivePreviewPreparationIsReadOnly(t *testing.T) {
	_testAssert := assert.New(t)
	_testRequire := require.New(t)

	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("x", "1001")
	_testRequire.NoError(err)
	username := "example_owner"
	before, err := st.UpdateXControlContext(t.Context(), source.ID, 0, store.XControlPatch{OwnerID: "1001", Username: &username})
	_testRequire.NoError(err)
	upper := time.Date(2026, 10, 30, 9, 0, 0, 0, time.UTC)
	preview, err := st.PrepareXArchiveJobContext(t.Context(), source.ID, store.XArchiveJobSpec{OwnerID: "1001", After: upper.AddDate(-1, 0, 0), Before: upper, Preview: true})
	_testRequire.NoError(err)
	_testRequire.NotNil(preview.Own)
	_testRequire.NotNil(preview.Mentions)
	_testAssert.Empty(preview.ID)
	_testAssert.Equal("from:example_owner -is:retweet", preview.Own.Query)
	after, err := st.LoadXControlContext(t.Context(), source.ID)
	_testRequire.NoError(err)
	_testAssert.Equal(before, after)
}

func TestXArchiveSavedRequestsFailClosed(t *testing.T) {
	for _, corruption := range []string{"query", "window", "fingerprint", "completion"} {
		t.Run(corruption, func(t *testing.T) {
			_testAssert := assert.New(t)
			_testRequire := require.New(t)

			st := testutil.NewTestStore(t)
			source, err := st.GetOrCreateSource("x", "1001")
			_testRequire.NoError(err)
			username := "example_owner"
			_, err = st.UpdateXControlContext(t.Context(), source.ID, 0, store.XControlPatch{OwnerID: "1001", Username: &username})
			_testRequire.NoError(err)
			maxSpend := int64(25_000_000)
			upper := time.Date(2026, 10, 30, 9, 0, 0, 0, time.UTC)
			spec := store.XArchiveJobSpec{OwnerID: "1001", After: upper.AddDate(-1, 0, 0), Before: upper, CapMicros: &maxSpend}
			job, err := st.PrepareXArchiveJobContext(t.Context(), source.ID, spec)
			_testRequire.NoError(err)
			switch corruption {
			case "query":
				job.Own.Query = "from:someone_else -is:retweet"
			case "window":
				job.Own.Before = job.Own.Before.Add(-time.Second)
			case "fingerprint":
				job.Fingerprint = "different"
			case "completion":
				job.Complete = true
			}
			state, err := st.LoadXControlContext(t.Context(), source.ID)
			_testRequire.NoError(err)
			jobs := []store.XArchiveJob{job}
			before, err := st.UpdateXControlContext(t.Context(), source.ID, state.Revision, store.XControlPatch{OwnerID: "1001", Jobs: &jobs})
			_testRequire.NoError(err)
			_, err = st.PrepareXArchiveJobContext(t.Context(), source.ID, spec)
			_testRequire.ErrorIs(err, store.ErrXArchiveScope)
			after, err := st.LoadXControlContext(t.Context(), source.ID)
			_testRequire.NoError(err)
			_testAssert.Equal(before, after)
		})
	}
}
