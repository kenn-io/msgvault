package store_test

import (
	"context"
	"encoding/json/v2"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestXControlCompletionPreservesReservation(t *testing.T) {
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("x", "100")
	require.NoError(t, err)
	state, err := st.LoadXControlContext(t.Context(), source.ID)
	require.NoError(t, err)
	reservations := []store.XReservation{{ID: "request-1", Kind: "timeline", Month: "2026-10", AmountMicros: 15000, State: "unknown"}}
	state, err = st.UpdateXControlContext(t.Context(), source.ID, state.Revision, store.XControlPatch{OwnerID: "100", Reservations: &reservations})
	require.NoError(t, err)
	stale := state.Revision
	generation := int64(2)
	state, err = st.UpdateXControlContext(t.Context(), source.ID, state.Revision, store.XControlPatch{OwnerID: "100", Generation: &generation})
	require.NoError(t, err)
	run, err := st.StartSync(source.ID, "x")
	require.NoError(t, err)
	scoped := st.ScopedToSync(source.ID, run)
	walk := store.XWalk{Endpoint: "own", Fingerprint: "own-window", Complete: true, MaxObservedID: "200"}
	err = scoped.CompleteXSyncContext(t.Context(), run, store.XControlPatch{OwnerID: "100", ExpectedRevision: stale, Own: &walk})
	require.ErrorIs(t, err, store.ErrXControlConflict)
	got, err := st.LoadXControlContext(t.Context(), source.ID)
	require.NoError(t, err)
	assert.Equal(t, reservations, got.Reservations)
	assert.Nil(t, got.Own)
	// A failed terminal write releases execution ownership and can be recovered.
	execution, err := st.AcquireSyncExecutionContext(t.Context(), source.ID)
	require.NoError(t, err)
	defer func() { require.NoError(t, execution.Release()) }()
	run, err = execution.StartSyncContext(t.Context(), "x", "")
	require.NoError(t, err)
	scoped = st.ScopedToSync(source.ID, run)
	require.NoError(t, scoped.UpdateSyncCheckpointContext(t.Context(), run, &store.Checkpoint{MessagesProcessed: 3, MessagesAdded: 2, MessagesUpdated: 1, PageToken: "diagnostic-only"}))
	require.NoError(t, scoped.CompleteXSyncContext(t.Context(), run, store.XControlPatch{OwnerID: "100", ExpectedRevision: got.Revision, Own: &walk}))
	got, err = st.LoadXControlContext(t.Context(), source.ID)
	require.NoError(t, err)
	assert.Equal(t, reservations, got.Reservations)
	assert.Equal(t, int64(2), got.Generation)
	require.NotNil(t, got.Own)
	assert.True(t, got.Own.Complete)
	var status, checkpoint string
	var processed, added, updated int64
	require.NoError(t, st.DB().QueryRow(st.Rebind("SELECT status,cursor_before,messages_processed,messages_added,messages_updated FROM sync_runs WHERE id=?"), run).Scan(&status, &checkpoint, &processed, &added, &updated))
	assert.Equal(t, "completed", status)
	assert.Equal(t, "diagnostic-only", checkpoint)
	assert.Equal(t, int64(3), processed)
	assert.Equal(t, int64(2), added)
	assert.Equal(t, int64(1), updated)
	require.ErrorIs(t, scoped.CompleteXSyncContext(t.Context(), run, store.XControlPatch{OwnerID: "100", ExpectedRevision: got.Revision}), store.ErrSyncRunSuperseded)
}

func TestXControlRejectsCorruptState(t *testing.T) {
	for _, raw := range []string{`{`, `null`, `{}`, `{"version":1,"owner_id":"100","revision":null}`, `{"version":1,"owner_id":"100","revision":0,"generation":null}`, `{"version":2,"owner_id":"100"}`, `{"version":1,"owner_id":"100","revision":0,"unrecognized":1}`, `{"version":1,"owner_id":"101","revision":0}`, `{"version":1,"owner_id":"100","revision":0,"reservations":[{"id":"r","month":"2026-10","amount_micros":-1,"state":"unknown"}]}`} {
		t.Run(raw, func(t *testing.T) {
			st := testutil.NewTestStore(t)
			source, err := st.GetOrCreateSource("x", "100")
			require.NoError(t, err)
			require.NoError(t, st.UpdateSourceSyncCursor(source.ID, raw))
			_, err = st.LoadXControlContext(t.Context(), source.ID)
			require.Error(t, err)
			_, err = st.UpdateXControlContext(t.Context(), source.ID, 0, store.XControlPatch{OwnerID: "100"})
			require.Error(t, err)
			got, err := st.GetSourceByID(source.ID)
			require.NoError(t, err)
			assert.Equal(t, raw, got.SyncCursor.String)
		})
	}
}

func TestXControlSourceRevisionAndCapacity(t *testing.T) {
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("x", "100")
	require.NoError(t, err)
	_, err = st.UpdateXControlContext(t.Context(), source.ID, 0, store.XControlPatch{OwnerID: "101"})
	require.Error(t, err)
	_, err = st.UpdateXControlContext(t.Context(), source.ID, 1, store.XControlPatch{OwnerID: "100"})
	require.ErrorIs(t, err, store.ErrXControlConflict)
	reservations := make([]store.XReservation, store.XMaxReservations+1)
	_, err = st.UpdateXControlContext(t.Context(), source.ID, 0, store.XControlPatch{OwnerID: "100", Reservations: &reservations})
	require.Error(t, err)
	other, err := st.GetOrCreateSource("gmail", "owner@example.com")
	require.NoError(t, err)
	_, err = st.LoadXControlContext(t.Context(), other.ID)
	require.Error(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = st.LoadXControlContext(ctx, source.ID)
	require.ErrorIs(t, err, context.Canceled)
	run, err := st.StartSync(source.ID, "x")
	require.NoError(t, err)
	scoped := st.ScopedToSync(source.ID, run)
	require.NoError(t, st.FailSync(run, "synthetic failure"))
	_, err = scoped.UpdateXControlContext(t.Context(), source.ID, 0, store.XControlPatch{OwnerID: "100"})
	require.ErrorIs(t, err, store.ErrSyncRunSuperseded)
	ctx, cancel = context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	execution, err := st.AcquireSyncExecutionContext(ctx, source.ID)
	require.NoError(t, err)
	require.NoError(t, execution.Release())
}

func TestXCompletionRollbackAndSourceScope(t *testing.T) {
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("x", "100")
	require.NoError(t, err)
	other, err := st.GetOrCreateSource("x", "101")
	require.NoError(t, err)
	state, err := st.UpdateXControlContext(t.Context(), source.ID, 0, store.XControlPatch{OwnerID: "100"})
	require.NoError(t, err)
	run, err := st.StartSync(source.ID, "x")
	require.NoError(t, err)
	scoped := st.ScopedToSync(source.ID, run)
	_, err = scoped.UpdateXControlContext(t.Context(), other.ID, 0, store.XControlPatch{OwnerID: "101"})
	require.Error(t, err)
	require.Error(t, st.CompleteXSyncContext(t.Context(), run, store.XControlPatch{OwnerID: "100", ExpectedRevision: state.Revision}))
	if st.IsPostgreSQL() {
		_, err = st.DB().Exec(`CREATE FUNCTION reject_x_completion() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.status='completed' THEN RAISE EXCEPTION 'synthetic completion failure'; END IF; RETURN NEW; END $$; CREATE TRIGGER reject_x_completion BEFORE UPDATE ON sync_runs FOR EACH ROW EXECUTE FUNCTION reject_x_completion()`)
	} else {
		_, err = st.DB().Exec(`CREATE TRIGGER reject_x_completion BEFORE UPDATE ON sync_runs WHEN NEW.status='completed' BEGIN SELECT RAISE(ABORT,'synthetic completion failure'); END`)
	}
	require.NoError(t, err)
	walk := store.XWalk{Endpoint: "own", Fingerprint: "synthetic-window", Complete: true}
	require.ErrorContains(t, scoped.CompleteXSyncContext(t.Context(), run, store.XControlPatch{OwnerID: "100", ExpectedRevision: state.Revision, Own: &walk}), "synthetic completion failure")
	got, err := st.LoadXControlContext(t.Context(), source.ID)
	require.NoError(t, err)
	assert.Equal(t, state.Revision, got.Revision)
	assert.Nil(t, got.Own)
	var status string
	require.NoError(t, st.DB().QueryRow(st.Rebind("SELECT status FROM sync_runs WHERE id=?"), run).Scan(&status))
	assert.Equal(t, "running", status)
	execution, err := st.AcquireSyncExecutionContext(t.Context(), source.ID)
	require.NoError(t, err)
	require.NoError(t, execution.Release())
	require.NoError(t, st.DB().QueryRow(st.Rebind("SELECT status FROM sync_runs WHERE id=?"), run).Scan(&status))
	assert.Equal(t, "failed", status)
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
			st := testutil.NewTestStore(t)
			source, err := st.GetOrCreateSource("x", "100")
			require.NoError(t, err)
			reservations := []store.XReservation{tc.value}
			_, err = st.UpdateXControlContext(t.Context(), source.ID, 0, store.XControlPatch{OwnerID: "100", Reservations: &reservations})
			if tc.invalid {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			got, err := st.LoadXControlContext(t.Context(), source.ID)
			require.NoError(t, err)
			assert.Equal(t, reservations, got.Reservations)
			var raw string
			require.NoError(t, st.DB().QueryRow(st.Rebind("SELECT sync_cursor FROM sources WHERE id=?"), source.ID).Scan(&raw))
			var encoded map[string]any
			require.NoError(t, json.Unmarshal([]byte(raw), &encoded))
			item := encoded["reservations"].([]any)[0].(map[string]any)
			assert.Equal(t, float64(tc.value.MaxPosts), item["max_posts"])
			assert.Equal(t, float64(tc.value.MaxUsers), item["max_users"])
			assert.Equal(t, float64(tc.value.PostPriceMicros), item["post_price_micros"])
			assert.Equal(t, float64(tc.value.UserPriceMicros), item["user_price_micros"])
		})
	}
}

func TestXControlConcurrentRevision(t *testing.T) {
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("x", "100")
	require.NoError(t, err)
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
			require.ErrorIs(t, err, store.ErrXControlConflict)
			conflicts++
		}
	}
	assert.Equal(t, 1, successes)
	assert.Equal(t, 1, conflicts)
	state, err := st.LoadXControlContext(t.Context(), source.ID)
	require.NoError(t, err)
	assert.Equal(t, int64(1), state.Revision)
}

func TestXArchiveJobPreparationKeepsLifetimeIdentity(t *testing.T) {
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("x", "1001")
	require.NoError(t, err)
	username := "example_owner"
	generation := int64(1)
	_, err = st.UpdateXControlContext(t.Context(), source.ID, 0, store.XControlPatch{OwnerID: "1001", Username: &username, Generation: &generation})
	require.NoError(t, err)
	after := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	before := time.Date(2026, 10, 30, 9, 0, 0, 0, time.UTC)
	cap := int64(25_000_000)
	spec := store.XArchiveJobSpec{OwnerID: "1001", After: after, Before: before, CapMicros: &cap}
	job, err := st.PrepareXArchiveJobContext(t.Context(), source.ID, spec)
	require.NoError(t, err)
	require.NotEmpty(t, job.ID)
	require.NotNil(t, job.Own)
	require.NotNil(t, job.Mentions)
	assert.Equal(t, "example_owner", job.Username)
	assert.Equal(t, "from:example_owner -is:retweet", job.Own.Query)
	assert.Equal(t, "to:example_owner -is:retweet", job.Mentions.Query)
	assert.Equal(t, after, job.After)
	assert.Equal(t, before, job.Before)
	cost := store.XRequestCost{Kind: store.XRequestSearch, OwnerID: "1001", Generation: 1, GrantedScopes: []string{"tweet.read", "users.read"}, MonthlyBudgetMicros: 100_000_000, RequestedResults: 100, PostPriceMicros: 5000, UserPriceMicros: 10000, JobID: job.ID, JobFingerprint: job.Fingerprint, Query: job.Own.Query, After: after, Before: before, At: before}
	known, err := st.ReserveXRequestContext(t.Context(), source.ID, cost)
	require.NoError(t, err)
	require.NoError(t, st.SettleXRequestContext(t.Context(), source.ID, known.ID, store.XResourceUsage{Posts: 1, Users: 1, Persisted: true, Complete: true}))
	cost.Query = job.Mentions.Query
	_, err = st.ReserveXRequestContext(t.Context(), source.ID, cost)
	require.NoError(t, err)
	state, err := st.LoadXControlContext(t.Context(), source.ID)
	require.NoError(t, err)
	username = "renamed_owner"
	generation = 2
	_, err = st.UpdateXControlContext(t.Context(), source.ID, state.Revision, store.XControlPatch{OwnerID: "1001", Username: &username, Generation: &generation})
	require.NoError(t, err)
	spec.Before = before.AddDate(0, 1, 0)
	resumed, err := st.PrepareXArchiveJobContext(t.Context(), source.ID, spec)
	require.NoError(t, err)
	assert.Equal(t, job.ID, resumed.ID)
	assert.Equal(t, job.Fingerprint, resumed.Fingerprint)
	assert.Equal(t, job.Before, resumed.Before)
	assert.Equal(t, job.Username, resumed.Username)
	assert.Equal(t, job.Own.Query, resumed.Own.Query)
	assert.Equal(t, int64(15_000), resumed.SpentMicros)
	assert.Equal(t, int64(1_500_000), resumed.ReservedMicros)
	cap = 30_000_000
	raised, err := st.PrepareXArchiveJobContext(t.Context(), source.ID, spec)
	require.NoError(t, err)
	assert.Equal(t, job.ID, raised.ID)
	assert.Equal(t, cap, raised.CapMicros)
	assert.Equal(t, resumed.SpentMicros, raised.SpentMicros)
	assert.Equal(t, resumed.ReservedMicros, raised.ReservedMicros)
	state, err = st.LoadXControlContext(t.Context(), source.ID)
	require.NoError(t, err)
	require.Len(t, state.Jobs, 1)
	completed := state.Jobs[0]
	completed.Complete = true
	completed.Own.Complete = true
	completed.Mentions.Complete = true
	jobs := []store.XArchiveJob{completed}
	_, err = st.UpdateXControlContext(t.Context(), source.ID, state.Revision, store.XControlPatch{OwnerID: "1001", Jobs: &jobs})
	require.NoError(t, err)
	repeated, err := st.PrepareXArchiveJobContext(t.Context(), source.ID, spec)
	require.NoError(t, err)
	assert.True(t, repeated.Complete)
	assert.Equal(t, job.ID, repeated.ID)
	assert.Equal(t, before, repeated.Before)
	lower := int64(20_000_000)
	spec.JobID = job.ID
	spec.CapMicros = &lower
	_, err = st.PrepareXArchiveJobContext(t.Context(), source.ID, spec)
	require.Error(t, err)
	spec.CapMicros = &cap
	spec.JobID = ""
	spec.NewJob = true
	fresh, err := st.PrepareXArchiveJobContext(t.Context(), source.ID, spec)
	require.NoError(t, err)
	assert.NotEqual(t, job.ID, fresh.ID)
	assert.Equal(t, "renamed_owner", fresh.Username)
	assert.Equal(t, spec.Before, fresh.Before)
	spec.NewJob = false
	spec.After = after.AddDate(-1, 0, 0)
	other, err := st.PrepareXArchiveJobContext(t.Context(), source.ID, spec)
	require.NoError(t, err)
	assert.NotEqual(t, fresh.ID, other.ID)
	assert.NotEqual(t, job.ID, other.ID)
}

func TestXArchivePreviewPreparationIsReadOnly(t *testing.T) {
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("x", "1001")
	require.NoError(t, err)
	username := "example_owner"
	before, err := st.UpdateXControlContext(t.Context(), source.ID, 0, store.XControlPatch{OwnerID: "1001", Username: &username})
	require.NoError(t, err)
	upper := time.Date(2026, 10, 30, 9, 0, 0, 0, time.UTC)
	preview, err := st.PrepareXArchiveJobContext(t.Context(), source.ID, store.XArchiveJobSpec{OwnerID: "1001", After: upper.AddDate(-1, 0, 0), Before: upper, Preview: true})
	require.NoError(t, err)
	require.NotNil(t, preview.Own)
	require.NotNil(t, preview.Mentions)
	assert.Empty(t, preview.ID)
	assert.Equal(t, "from:example_owner -is:retweet", preview.Own.Query)
	after, err := st.LoadXControlContext(t.Context(), source.ID)
	require.NoError(t, err)
	assert.Equal(t, before, after)
}

func TestXArchiveSavedRequestsFailClosed(t *testing.T) {
	for _, corruption := range []string{"query", "window", "fingerprint", "completion"} {
		t.Run(corruption, func(t *testing.T) {
			st := testutil.NewTestStore(t)
			source, err := st.GetOrCreateSource("x", "1001")
			require.NoError(t, err)
			username := "example_owner"
			_, err = st.UpdateXControlContext(t.Context(), source.ID, 0, store.XControlPatch{OwnerID: "1001", Username: &username})
			require.NoError(t, err)
			cap := int64(25_000_000)
			upper := time.Date(2026, 10, 30, 9, 0, 0, 0, time.UTC)
			spec := store.XArchiveJobSpec{OwnerID: "1001", After: upper.AddDate(-1, 0, 0), Before: upper, CapMicros: &cap}
			job, err := st.PrepareXArchiveJobContext(t.Context(), source.ID, spec)
			require.NoError(t, err)
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
			require.NoError(t, err)
			jobs := []store.XArchiveJob{job}
			before, err := st.UpdateXControlContext(t.Context(), source.ID, state.Revision, store.XControlPatch{OwnerID: "1001", Jobs: &jobs})
			require.NoError(t, err)
			_, err = st.PrepareXArchiveJobContext(t.Context(), source.ID, spec)
			assert.ErrorIs(t, err, store.ErrXArchiveScope)
			after, err := st.LoadXControlContext(t.Context(), source.ID)
			require.NoError(t, err)
			assert.Equal(t, before, after)
		})
	}
}
