package cmd

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/jobctx"
	"go.kenn.io/msgvault/internal/persontiers"
	"go.kenn.io/msgvault/internal/scheduler"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func tierScheduledFixture(t *testing.T) (*storetest.Fixture, peopleTierScheduledRunner) {
	t.Helper()
	f := storetest.New(t)
	cfg := persontiers.DefaultConfig()
	cfg.Enabled = true
	cfg.MinimumTier = 2
	return f, peopleTierScheduledRunner{store: f.Store, config: cfg, timezone: "UTC", maxDirect: 25, gate: api.NewSerialOperationGate(), now: func() time.Time { return time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC) }, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

func TestPeopleTierSchedulerRegistersOnlyWhenEnabled(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(fmt.Sprint(enabled), func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			f, runner := tierScheduledFixture(t)
			cfg := config.NewDefaultConfig()
			cfg.People.Tiers = runner.config
			cfg.People.Tiers.Enabled = enabled
			sched := scheduler.New(nil)
			require.NoError(registerPeopleTierJob(sched, f.Store, cfg, runner.gate, runner.logger))
			assert.Equal(enabled, sched.IsJobScheduled(peopleTierJobName))
			if enabled {
				require.Len(sched.JobStatus(), 1)
				assert.Equal("45 1 * * *", sched.JobStatus()[0].Schedule)
			}
		})
	}
	_, runner := tierScheduledFixture(t)
	job := newPeopleTierScheduledJob(runner)
	assert.True(t, job.Preemptible)
	assert.Zero(t, job.MaxRuntime, "the scheduler must not spend the work budget waiting for operation admission")
}

func TestPeopleTierSchedulerRecordsThenAppliesWithoutProjecting(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, runner := tierScheduledFixture(t)
	person, _, err := f.Store.CreatePersonFromParticipant(f.EnsureParticipant("scheduled@example.com", "Scheduled", "example.com"))
	require.NoError(err)
	// A pending archive message makes readiness false. Tier evaluation must not
	// consume the projection queue to make its own evidence ready.
	f.CreateMessage("unprojected-tier-message")
	ctx := jobctx.WithProgress(t.Context())
	require.ErrorIs(runner.Run(ctx), scheduler.ErrDeferUntilNextTrigger)
	assert.True(jobctx.HasProgress(ctx))
	var computed, shadow int
	require.NoError(f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(`SELECT computed_tier,shadow_tier FROM person_tier_observations WHERE person_id=?`), person.ID).Scan(&computed, &shadow))
	assert.Equal(2, computed)
	assert.Equal(2, shadow)
	for _, table := range []string{"person_tier_state", "person_tracking", "person_brief_enrollments", "activity_events"} {
		var n int
		require.NoError(f.Store.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM `+table).Scan(&n))
		assert.Zero(n, table)
	}
	pending, err := f.Store.ListActivityProjectionQueueContext(t.Context(), 1)
	require.NoError(err)
	require.NotEmpty(pending)
	events, err := f.Store.PersonTierHistoryContext(t.Context(), person.ID, 20, 0)
	require.NoError(err)
	require.Len(events, 1)
	assert.Equal("preview", events[0].Mode)
	run, err := f.Store.PersonTierRunContext(t.Context(), "scheduled")
	require.NoError(err)
	assert.Nil(run)
	runner.config.Apply = true
	require.ErrorIs(runner.Run(jobctx.WithProgress(t.Context())), scheduler.ErrDeferUntilNextTrigger)
	tracking, err := f.Store.GetPersonTrackingContext(t.Context(), person.ID)
	require.NoError(err)
	assert.True(tracking.Tracked)
	assert.Equal("auto", tracking.Provenance)
	assert.Equal("people-tiers", tracking.Actor)
	_, _, held := runner.gate.Holder()
	assert.False(held, "the completed page releases operation admission")
}

func TestPeopleTierSchedulerResumesFrozenPageAfterCancellation(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, runner := tierScheduledFixture(t)
	// Saved unbound profiles exercise the configured floor and production ID
	// pagination without importing any external identities.
	values := make([]string, 501)
	args := make([]any, 0, 1002)
	for i := range values {
		values[i] = "(?,?)"
		args = append(args, fmt.Sprintf("synthetic-scheduled-%d", i), fmt.Sprintf("Synthetic %d", i))
	}
	_, err := f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`INSERT INTO persons(vcard_uid,display_name) VALUES `+strings.Join(values, ",")), args...)
	require.NoError(err)
	for _, mode := range []string{"preview", "apply"} {
		_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`INSERT INTO person_tier_events(person_id,from_tier,to_tier,reason,mode,lanes_json,signals_json,rule,rule_version,actor,run_id,created_at) SELECT id,3,2,'promotion',?,'{}','{}','minimum_tier',1,'synthetic',?,? FROM persons ORDER BY id LIMIT 1`), mode, "old-"+mode, time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC))
		require.NoError(err)
	}
	ctx := jobctx.WithProgress(t.Context())
	require.ErrorIs(runner.Run(ctx), scheduler.ErrReschedule)
	assert.True(jobctx.HasProgress(ctx))
	var retained int
	require.NoError(f.Store.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM person_tier_events WHERE run_id IN ('old-preview','old-apply')`).Scan(&retained))
	assert.Equal(2, retained, "retention waits for the final page")
	frozen, err := f.Store.PersonTierRunContext(t.Context(), "scheduled")
	require.NoError(err)
	require.NotNil(frozen)
	assert.NotEmpty(frozen.Cursor)
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	require.ErrorIs(runner.Run(canceled), context.Canceled)
	afterCancel, err := f.Store.PersonTierRunContext(t.Context(), "scheduled")
	require.NoError(err)
	require.NotNil(afterCancel)
	assert.Equal(frozen.RunID, afterCancel.RunID)
	assert.Equal(frozen.Cursor, afterCancel.Cursor)
	// A new callback after restart sees changed operational and semantic values,
	// but must finish the saved record-only snapshot before adopting either.
	runner.config.Apply = true
	runner.config.MinimumTier = 3
	runner.now = func() time.Time { return time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC) }
	require.ErrorIs(runner.Run(jobctx.WithProgress(t.Context())), scheduler.ErrDeferUntilNextTrigger)
	var observations, tracking int
	require.NoError(f.Store.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM person_tier_observations WHERE computed_tier=2 AND as_of_local_date='2026-10-17'`).Scan(&observations))
	require.NoError(f.Store.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM person_tracking`).Scan(&tracking))
	assert.Equal(501, observations)
	assert.Zero(tracking)
	var oldPreview, oldApplied int
	require.NoError(f.Store.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM person_tier_events WHERE run_id='old-preview'`).Scan(&oldPreview))
	require.NoError(f.Store.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM person_tier_events WHERE run_id='old-apply'`).Scan(&oldApplied))
	assert.Zero(oldPreview)
	assert.Equal(1, oldApplied, "applied audit survives preview retention")
	run, err := f.Store.PersonTierRunContext(t.Context(), "scheduled")
	require.NoError(err)
	assert.Nil(run)
}

func TestPeopleTierSchedulerDefersSupersededRunUntilNextTrigger(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, runner := tierScheduledFixture(t)
	_, _, err := f.Store.CreatePersonFromParticipant(f.EnsureParticipant("superseded@example.com", "Superseded", "example.com"))
	require.NoError(err)
	request := store.PersonTierPageRequest{Config: runner.config, Timezone: "UTC", MaxDirect: 25, AsOf: runner.now()}
	old, err := f.Store.StartPersonTierRunContext(t.Context(), request, "scheduled", "record_only")
	require.NoError(err)
	_, err = f.Store.StartPersonTierRunContext(t.Context(), request, "manual", "apply")
	require.NoError(err)
	require.ErrorIs(runner.Run(jobctx.WithProgress(t.Context())), scheduler.ErrDeferUntilNextTrigger)
	run, err := f.Store.PersonTierRunContext(t.Context(), "scheduled")
	require.NoError(err)
	assert.Nil(run)
	var observations int
	require.NoError(f.Store.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM person_tier_observations`).Scan(&observations))
	assert.Zero(observations)
	require.ErrorIs(runner.Run(jobctx.WithProgress(t.Context())), scheduler.ErrDeferUntilNextTrigger)
	history, err := f.Store.PersonTierHistoryContext(t.Context(), 0, 20, 0)
	require.NoError(err)
	require.Len(history, 1)
	assert.NotEqual(old.RunID, history[0].RunID)
}

func TestPeopleTierSchedulerContinuesFailuresAndAuditsThirdRun(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, runner := tierScheduledFixture(t)
	runner.config.Apply = true
	broken, _, err := f.Store.CreatePersonFromParticipant(f.EnsureParticipant("broken@example.com", "Broken", "example.com"))
	require.NoError(err)
	healthy, _, err := f.Store.CreatePersonFromParticipant(f.EnsureParticipant("healthy@example.com", "Healthy", "example.com"))
	require.NoError(err)
	if f.Store.IsPostgreSQL() {
		_, err = f.Store.DB().ExecContext(t.Context(), fmt.Sprintf(`CREATE FUNCTION reject_scheduled_tier_grant() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.person_id=%d THEN RAISE EXCEPTION 'synthetic grant failure'; END IF; RETURN NEW; END $$`, broken.ID))
		require.NoError(err)
		_, err = f.Store.DB().ExecContext(t.Context(), `CREATE TRIGGER reject_scheduled_tier_grant BEFORE INSERT ON person_tracking FOR EACH ROW EXECUTE FUNCTION reject_scheduled_tier_grant()`)
	} else {
		_, err = f.Store.DB().ExecContext(t.Context(), fmt.Sprintf(`CREATE TRIGGER reject_scheduled_tier_grant BEFORE INSERT ON person_tracking WHEN NEW.person_id=%d BEGIN SELECT RAISE(ABORT,'synthetic grant failure'); END`, broken.ID))
	}
	require.NoError(err)
	for range 3 {
		require.ErrorIs(runner.Run(jobctx.WithProgress(t.Context())), scheduler.ErrDeferUntilNextTrigger)
	}
	tracking, err := f.Store.GetPersonTrackingContext(t.Context(), healthy.ID)
	require.NoError(err)
	assert.True(tracking.Tracked)
	events, err := f.Store.PersonTierHistoryContext(t.Context(), broken.ID, 20, 0)
	require.NoError(err)
	require.Len(events, 1)
	assert.Equal("apply_failed", events[0].Reason)
}

func TestPeopleTierSchedulerDoesNotRestartResetPendingFollowup(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, runner := tierScheduledFixture(t)
	runner.config.Apply = true
	person, _, err := f.Store.CreatePersonFromParticipant(f.EnsureParticipant("reset-pending@example.com", "Reset pending", "example.com"))
	require.NoError(err)
	request := store.PersonTierPageRequest{Config: runner.config, Timezone: "UTC", MaxDirect: 25, AsOf: runner.now()}
	old, err := f.Store.StartPersonTierRunContext(t.Context(), request, "scheduled", "apply")
	require.NoError(err)
	_, err = f.Store.ResetPersonTiersContext(t.Context(), "operator")
	require.NoError(err)
	// A queued follow-up must process the stale marker, rather than interpret
	// its disappearance as permission to begin another automatic apply.
	ctx := jobctx.WithProgress(t.Context())
	require.ErrorIs(runner.Run(ctx), scheduler.ErrDeferUntilNextTrigger)
	assert.False(jobctx.HasProgress(ctx))
	tracking, err := f.Store.GetPersonTrackingContext(t.Context(), person.ID)
	require.NoError(err)
	assert.False(tracking.Tracked)
	run, err := f.Store.PersonTierRunContext(t.Context(), "scheduled")
	require.NoError(err)
	assert.Nil(run)
	// A later independent invocation represents the next cron trigger.
	require.ErrorIs(runner.Run(jobctx.WithProgress(t.Context())), scheduler.ErrDeferUntilNextTrigger)
	tracking, err = f.Store.GetPersonTrackingContext(t.Context(), person.ID)
	require.NoError(err)
	assert.True(tracking.Tracked)
	events, err := f.Store.PersonTierHistoryContext(t.Context(), person.ID, 20, 0)
	require.NoError(err)
	require.Len(events, 1)
	assert.NotEqual(old.RunID, events[0].RunID)
}

type tierObservedAdmissionGate struct {
	*api.SerialOperationGate
	admission chan struct{}
}

func (g *tierObservedAdmissionGate) BeginLabeledWorkContext(ctx context.Context, label string) (func(), bool) {
	close(g.admission)
	return g.SerialOperationGate.BeginLabeledWorkContext(ctx, label)
}

func TestPeopleTierSchedulerAdmissionWaitCancelsOnShutdown(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	_, runner := tierScheduledFixture(t)
	baseGate := api.NewSerialOperationGate()
	release, ok := baseGate.BeginLabeledWorkContext(t.Context(), "Synthetic active operation")
	require.True(ok)
	defer release()
	gate := &tierObservedAdmissionGate{SerialOperationGate: baseGate, admission: make(chan struct{})}
	runner.gate = gate
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- runner.Run(ctx) }()
	select {
	case <-gate.admission:
	case <-ctx.Done():
		require.NoError(ctx.Err())
	}
	cancel()
	select {
	case err := <-result:
		require.ErrorIs(err, context.Canceled)
	case <-time.After(30 * time.Second):
		require.FailNow("admission wait did not stop when the daemon context was cancelled")
	}
	_, _, held := baseGate.Holder()
	assert.True(held, "cancelling the waiting job does not release the active operation")
}

func TestPeopleTierSchedulerBudgetQueuesCommittedProgress(t *testing.T) {
	runCtx := jobctx.WithProgress(t.Context())
	budgetCtx, cancel := context.WithCancelCause(runCtx)
	cancel(jobctx.ErrRunBudgetExceeded)
	jobctx.RecordProgress(runCtx)

	err := personTierScheduledRunResult(budgetCtx, runCtx, context.Canceled)
	require.Equal(t, scheduler.ErrReschedule, err)
}

func TestPeopleTierSchedulerBudgetWithoutProgressWaitsForNextTrigger(t *testing.T) {
	runCtx := jobctx.WithProgress(t.Context())
	budgetCtx, cancel := context.WithCancelCause(runCtx)
	cancel(jobctx.ErrRunBudgetExceeded)

	err := personTierScheduledRunResult(budgetCtx, runCtx, context.Canceled)
	require.ErrorIs(t, err, jobctx.ErrRunBudgetExceeded)
	assert.False(t, err == scheduler.ErrReschedule)
}

// Pause only after the real gate grants admission, so an actual API waiter can
// queue before the runner starts the next page. Store operations remain real.
type tierAdmissionPauseGate struct {
	*api.SerialOperationGate
	admitted chan struct{}
	proceed  chan struct{}
}

func (g *tierAdmissionPauseGate) BeginLabeledWorkContext(ctx context.Context, label string) (func(), bool) {
	release, ok := g.SerialOperationGate.BeginLabeledWorkContext(ctx, label)
	if !ok {
		return release, false
	}
	close(g.admitted)
	select {
	case <-g.proceed:
		return release, true
	case <-ctx.Done():
		release()
		return func() {}, false
	}
}

func TestPeopleTierSchedulerYieldsToRealRequestWaiter(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, runner := tierScheduledFixture(t)
	_, _, err := f.Store.CreatePersonFromParticipant(f.EnsureParticipant("waiting-request@example.com", "Waiting request", "example.com"))
	require.NoError(err)
	request := store.PersonTierPageRequest{Config: runner.config, Timezone: "UTC", MaxDirect: 25, AsOf: runner.now()}
	frozen, err := f.Store.StartPersonTierRunContext(t.Context(), request, "scheduled", "record_only")
	require.NoError(err)
	gate := &tierAdmissionPauseGate{SerialOperationGate: api.NewSerialOperationGate(), admitted: make(chan struct{}), proceed: make(chan struct{})}
	runner.gate = gate
	// This timeout bounds observing admission and release; elapsed duration is
	// not a correctness condition for the runner or Store work.
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	progress := jobctx.WithProgress(ctx)
	result := make(chan error, 1)
	go func() { result <- runner.Run(progress) }()
	select {
	case <-gate.admitted:
	case <-ctx.Done():
		require.NoError(ctx.Err())
	}
	acquired := make(chan bool, 1)
	go func() {
		release, ok := gate.BeginRequestWorkContext(ctx, "Synthetic interactive request")
		if ok {
			release()
		}
		acquired <- ok
	}()
	require.Eventually(gate.HasRequestWaiters, 30*time.Second, 10*time.Millisecond, "the real interactive request queues behind the held gate")
	close(gate.proceed)
	select {
	case err := <-result:
		require.ErrorIs(err, scheduler.ErrReschedule)
	case <-ctx.Done():
		require.NoError(ctx.Err())
	}
	select {
	case ok := <-acquired:
		assert.True(ok)
	case <-ctx.Done():
		require.NoError(ctx.Err())
	}
	assert.False(jobctx.HasProgress(progress))
	remaining, err := f.Store.PersonTierRunContext(t.Context(), "scheduled")
	require.NoError(err)
	require.NotNil(remaining)
	assert.Equal(frozen.RunID, remaining.RunID)
	assert.Equal(frozen.Cursor, remaining.Cursor)
	var observations int
	require.NoError(f.Store.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM person_tier_observations`).Scan(&observations))
	assert.Zero(observations)
	_, _, held := gate.Holder()
	assert.False(held)
}

func TestPeopleTierYieldContextObservesLateRealRequestWaiter(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	gate := api.NewSerialOperationGate()
	parent, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	release, ok := gate.BeginLabeledWorkContext(parent, "Synthetic tier page")
	require.True(ok)
	defer release()
	ctx, stop := personTierYieldContext(parent, gate)
	defer stop()
	require.NoError(ctx.Err(), "the page starts before any request is waiting")
	acquired := make(chan bool, 1)
	go func() {
		done, ok := gate.BeginRequestWorkContext(parent, "Synthetic late request")
		if ok {
			done()
		}
		acquired <- ok
	}()
	select {
	case <-ctx.Done():
	case <-parent.Done():
		require.NoError(parent.Err())
	}
	assert.ErrorIs(context.Cause(ctx), jobctx.ErrYieldedToWaiter)
	assert.NoError(parent.Err(), "only the page context yields")
	stop()
	release()
	select {
	case ok := <-acquired:
		assert.True(ok)
	case <-parent.Done():
		require.NoError(parent.Err())
	}
}
