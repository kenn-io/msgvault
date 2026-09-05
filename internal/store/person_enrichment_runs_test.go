package store_test

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/personenrichment"
	"go.kenn.io/msgvault/internal/store"
)

func TestPersonEnrichmentClaimLocksRunBeforeBindingWork(t *testing.T) {
	f := newEnrichmentWorkFixture(t)
	if !f.store.IsPostgreSQL() {
		t.Skip("PostgreSQL row-lock interleaving requires MSGVAULT_TEST_DB")
	}
	run := f.startRun(t, "claim-vs-complete")
	f.enqueue(t)

	claimLocked := make(chan struct{})
	releaseClaim := make(chan struct{})
	completeBeforeLock := make(chan struct{})
	var claimOnce, completeOnce sync.Once
	store.SetPersonEnrichmentRunBarrierForTest(f.store, func(phase string) {
		switch phase {
		case "claim_run_locked":
			claimOnce.Do(func() { close(claimLocked) })
			<-releaseClaim
		case "complete_before_run_lock":
			completeOnce.Do(func() { close(completeBeforeLock) })
		}
	})

	claimResult := make(chan *personenrichment.WorkLease, 1)
	claimErr := make(chan error, 1)
	go func() {
		lease, err := f.store.ClaimWork(t.Context(), personenrichment.ClaimOptions{
			RunID: run.ID, Owner: "claim-worker", ProviderName: f.profile.Name,
			Now: f.now, LeaseDuration: time.Minute,
		})
		claimResult <- lease
		claimErr <- err
	}()
	<-claimLocked

	completeErr := make(chan error, 1)
	go func() {
		completeErr <- f.store.CompleteRun(t.Context(), run.ID, personenrichment.RunCompletion{
			State: "succeeded", CompletedAt: f.now,
		})
	}()
	<-completeBeforeLock
	close(releaseClaim)

	require.NoError(t, <-claimErr)
	require.NotNil(t, <-claimResult)
	require.ErrorIs(t, <-completeErr, store.ErrRunNotTerminal)
}

func TestPersonEnrichmentClaimRetriesSQLiteSnapshotContention(t *testing.T) {
	require := require.New(t)
	f := newEnrichmentWorkFixture(t)
	if f.store.IsPostgreSQL() {
		t.Skip("SQLite snapshot contention requires the SQLite backend")
	}
	run := f.startRun(t, "claim-snapshot-contention")
	f.enqueue(t)

	claimRead := make(chan struct{})
	releaseClaim := make(chan struct{})
	var pauseOnce sync.Once
	store.SetPersonEnrichmentRunBarrierForTest(f.store, func(phase string) {
		if phase == "claim_run_locked" {
			pauseOnce.Do(func() {
				close(claimRead)
				<-releaseClaim
			})
		}
	})
	t.Cleanup(func() { store.SetPersonEnrichmentRunBarrierForTest(f.store, nil) })

	type claimOutcome struct {
		lease *personenrichment.WorkLease
		err   error
	}
	result := make(chan claimOutcome, 1)
	go func() {
		lease, err := f.store.ClaimWork(t.Context(), personenrichment.ClaimOptions{
			RunID: run.ID, Owner: "claim-worker", ProviderName: f.profile.Name,
			Now: f.now, LeaseDuration: time.Minute,
		})
		result <- claimOutcome{lease: lease, err: err}
	}()
	requireChannelSignal(t, claimRead, "claim did not establish its read snapshot")

	_, err := f.store.DB().ExecContext(t.Context(), `
		INSERT INTO archive_metadata (key, value)
		VALUES ('claim_snapshot_contention', 'committed')`)
	require.NoError(err)
	close(releaseClaim)

	claimed := <-result
	require.NoError(claimed.err)
	require.NotNil(claimed.lease)
}

func TestPersonEnrichmentScheduledRunLifecycle(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newEnrichmentWorkFixture(t)
	run := f.startRun(t, "2026-08-22T12:00:00Z")
	assert.Equal("running", run.State)
	f.enqueue(t)
	lease := f.claim(t, run.ID, "worker")
	require.NoError(f.store.ReleaseWork(t.Context(), lease.Token, personenrichment.WorkRelease{
		Outcome: "suppressed", PersonRevision: f.person.Revision,
		PayloadHash: strings.Repeat("a", 64), RequestHash: strings.Repeat("b", 64),
	}))
	require.NoError(f.store.CompleteRun(t.Context(), run.ID, personenrichment.RunCompletion{
		State: "succeeded", CompletedAt: f.now,
	}))

	got, err := f.store.GetPersonEnrichmentRunContext(t.Context(), run.ID)
	require.NoError(err)
	assert.Equal("succeeded", got.State)
	assert.Equal(int64(1), got.RequestedCount)
	assert.Equal(int64(1), got.StartedCount)
	assert.Equal(int64(1), got.SuppressedCount)
	assert.NotNil(got.CompletedAt)
}

func TestPersonEnrichmentManualRunIdempotency(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newEnrichmentWorkFixture(t)
	input := personenrichment.RunStart{Kind: "manual", RequestedBy: "manual-key-1", RequestedAt: f.now}
	first, created, err := f.store.StartRun(t.Context(), input)
	require.NoError(err)
	assert.True(created)
	second, created, err := f.store.StartRun(t.Context(), input)
	require.NoError(err)
	assert.False(created)
	assert.Equal(first.ID, second.ID)
	assert.Equal(first.RequestedAt, second.RequestedAt)
}

func TestPersonEnrichmentRunTransitionsQueuedOccurrenceToRunning(t *testing.T) {
	require := require.New(t)
	f := newEnrichmentWorkFixture(t)
	_, err := f.store.DB().ExecContext(t.Context(), f.store.Rebind(`
		INSERT INTO person_enrichment_runs (kind, requested_by, requested_at, state)
		VALUES ('scheduled', 'queued-occurrence', ?, 'queued')`), f.now)
	require.NoError(err)

	run, created, err := f.store.StartRun(t.Context(), personenrichment.RunStart{
		Kind: "scheduled", RequestedBy: "queued-occurrence", RequestedAt: f.now,
	})
	require.NoError(err)
	assert.False(t, created)
	assert.Equal(t, "running", run.State)
}

func TestQueuedPersonEnrichmentClaimsOldestStartedRunAtomically(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newEnrichmentWorkFixture(t)
	started := f.now.Add(-time.Hour)
	ids := make([]int64, 0, 3)
	for index, offset := range []time.Duration{time.Minute, 0, 0} {
		var id int64
		require.NoError(f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`
			INSERT INTO person_enrichment_runs
				(kind, requested_by, requested_at, started_at, state)
			VALUES ('scheduled', ?, ?, ?, 'queued') RETURNING id`),
			"queued-"+string(rune('a'+index)), started.Add(offset), started.Add(offset)).Scan(&id))
		ids = append(ids, id)
	}

	queued, err := f.store.ListQueuedPersonEnrichmentRunsContext(t.Context(), 10)
	require.NoError(err)
	require.Len(queued, 3)
	assert.Equal([]int64{ids[1], ids[2], ids[0]}, []int64{queued[0].ID, queued[1].ID, queued[2].ID})

	claimed, ok, err := f.store.ClaimQueuedPersonEnrichmentRunContext(t.Context(), queued[0].ID)
	require.NoError(err)
	require.True(ok)
	assert.Equal("running", claimed.State)
	_, ok, err = f.store.ClaimQueuedPersonEnrichmentRunContext(t.Context(), queued[0].ID)
	require.NoError(err)
	assert.False(ok, "the state predicate must permit one claimant")
}

func TestPersonEnrichmentRecoveryQueuesValidatedPendingRun(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newEnrichmentWorkFixture(t)
	run := f.startRun(t, "restart-pending")
	f.enqueue(t)
	lease := f.claim(t, run.ID, "stopped-daemon")
	attempt, created, err := f.store.BeginAttempt(t.Context(), lease.Token, testAttemptStart(&f, run.ID, "a"))
	require.NoError(err)
	require.True(created)
	require.NoError(f.store.AuthorizeAttemptDispatch(t.Context(), attempt.Token))
	require.NoError(f.store.RecordProviderStarted(t.Context(), attempt.Token, personenrichment.Attempt{
		State: personenrichment.AttemptPending, JobID: "restart-job", StartedAt: f.now,
		AdapterVersion: "adapter-v1", SchemaVersion: "schema-v1",
		ProgramFingerprint: strings.Repeat("b", 64),
	}))
	require.NoError(f.store.SchedulePoll(t.Context(), attempt.Token, personenrichment.Result{
		State: personenrichment.ResultPending, JobID: "restart-job", PollAfter: time.Minute,
		AdapterVersion: "adapter-v1", SchemaVersion: "schema-v1",
	}))
	before, err := f.store.GetPersonEnrichmentRunContext(t.Context(), run.ID)
	require.NoError(err)
	require.NotNil(before.StartedAt)

	recovered, err := f.store.RecoverPersonEnrichmentRunsContext(t.Context(), f.now.Add(time.Minute))
	require.NoError(err)
	assert.Equal(int64(1), recovered)
	recovered, err = f.store.RecoverPersonEnrichmentRunsContext(t.Context(), f.now.Add(2*time.Minute))
	require.NoError(err)
	assert.Zero(recovered)
	after, err := f.store.GetPersonEnrichmentRunContext(t.Context(), run.ID)
	require.NoError(err)
	assert.Equal("queued", after.State)
	require.NotNil(after.StartedAt)
	assert.Equal(before.StartedAt.UTC(), after.StartedAt.UTC())
}

func TestPersonEnrichmentRecoveryTerminalizesAttemptlessRunFromNormalOutcomes(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newEnrichmentWorkFixture(t)
	run := f.startRun(t, "restart-invalid")
	recoveredAt := f.now.Add(time.Minute)

	recovered, err := f.store.RecoverPersonEnrichmentRunsContext(t.Context(), recoveredAt)
	require.NoError(err)
	assert.Equal(int64(1), recovered)
	got, err := f.store.GetPersonEnrichmentRunContext(t.Context(), run.ID)
	require.NoError(err)
	assert.Equal("succeeded", got.State)
	require.NotNil(got.CompletedAt)
	assert.Equal(recoveredAt, got.CompletedAt.UTC())
	require.NotNil(got.FailureClass)
	assert.Equal("uncertain_start", *got.FailureClass)
	require.NotNil(got.SafeError)
	assert.Equal("daemon_restarted", *got.SafeError)
}

func TestPersonEnrichmentRecoveryPreservesClaimedWorkBeforeAttempt(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newEnrichmentWorkFixture(t)
	run := f.startRun(t, "restart-before-attempt")
	f.enqueue(t)
	lease := f.claim(t, run.ID, "stopped-daemon")
	recoveredAt := f.now.Add(time.Second)

	_, err := f.store.RecoverPersonEnrichmentRunsContext(t.Context(), recoveredAt)
	require.NoError(err)
	got, err := f.store.GetPersonEnrichmentRunContext(t.Context(), run.ID)
	require.NoError(err)
	assert.Equal("failed", got.State, "claimed work is not a successful empty run")
	assert.Zero(got.StartedCount, "recovery must not invent a provider attempt")

	nextRun := f.startRun(t, "restart-retry-unstarted-work")
	reclaimed := f.claim(t, nextRun.ID, "restarted-daemon")
	assert.Equal(lease.PersonID, reclaimed.PersonID)
	assert.Equal(lease.Trigger, reclaimed.Trigger)
	assert.Nil(reclaimed.ActiveAttempt)
	_, created, err := f.store.BeginAttempt(t.Context(), reclaimed.Token, testAttemptStart(&f, nextRun.ID, "a"))
	require.NoError(err)
	assert.True(created, "the preserved work must still be processable")
}

func TestPersonEnrichmentRecoveryReconcilesUncertainStartCostOnce(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	profile := enrichmentBudgetProfile(t, 10, 1_200, 1_200, 1_200)
	st, claims := newBudgetClaims(t, profile)
	start := claims[0].start
	start.HardCostCap = true
	start.GuaranteedMaxCost = personenrichment.Cost{Currency: "USD", AmountMicros: 600}
	attempt, created, err := st.BeginAttempt(t.Context(), claims[0].token, start)
	require.NoError(err)
	require.True(created)
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE person_enrichment_attempts SET
		state = 'pending', provider_request_id = ?, provider_job_id = ?,
		adapter_version = 'adapter-v1', schema_version = 'schema-v1',
		program_fingerprint = ?, provider_started_at = ? WHERE id = ?`),
		"private-recovery-request", "private-recovery-job", strings.Repeat("b", 64),
		time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC), attempt.ID)
	require.NoError(err)
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE person_enrichment_work
		SET active_attempt_id = NULL WHERE person_id = ? AND profile_fingerprint = ?`),
		start.PersonID, start.ProfileFingerprint)
	require.NoError(err, "break the native resume pointer while retaining the private provider binding")

	before, err := st.GetPersonEnrichmentRunCountersContext(t.Context(), start.RunID)
	require.NoError(err)
	assert.Equal(int64(600), before.CostReservedUSDMicros)
	assert.Zero(before.CostChargedUSDMicros)

	recoveredAt := time.Date(2026, 8, 22, 12, 5, 0, 0, time.UTC)
	recovered, err := st.RecoverPersonEnrichmentRunsContext(t.Context(), recoveredAt)
	require.NoError(err)
	assert.Equal(int64(1), recovered)
	after, err := st.GetPersonEnrichmentRunCountersContext(t.Context(), start.RunID)
	require.NoError(err)
	assert.Zero(after.CostReservedUSDMicros)
	assert.Equal(int64(600), after.CostChargedUSDMicros)
	day := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC).Format("2006-01-02")
	var personReserved, personCharged, dayReserved, dayCharged int64
	require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`
		SELECT cost_reserved_usd_micros, cost_charged_usd_micros
		FROM person_enrichment_person_day_counters
		WHERE person_id = ? AND profile_fingerprint = ? AND utc_day = ?`),
		start.PersonID, start.ProfileFingerprint, day).Scan(&personReserved, &personCharged))
	require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`
		SELECT cost_reserved_usd_micros, cost_charged_usd_micros
		FROM person_enrichment_day_counters
		WHERE profile_fingerprint = ? AND utc_day = ?`),
		start.ProfileFingerprint, day).Scan(&dayReserved, &dayCharged))
	assert.Zero(personReserved)
	assert.Equal(int64(600), personCharged)
	assert.Zero(dayReserved)
	assert.Equal(int64(600), dayCharged)
	stored, err := st.GetPersonEnrichmentAttemptContext(t.Context(), attempt.ID)
	require.NoError(err)
	assert.Zero(stored.ReservedCostUSDMicros)
	require.NotNil(stored.ProviderRequestID)
	require.NotNil(stored.ProviderJobID)
	assert.Equal("private-recovery-request", *stored.ProviderRequestID)
	assert.Equal("private-recovery-job", *stored.ProviderJobID)

	recovered, err = st.RecoverPersonEnrichmentRunsContext(t.Context(), recoveredAt.Add(time.Minute))
	require.NoError(err)
	assert.Zero(recovered)
	idempotent, err := st.GetPersonEnrichmentRunCountersContext(t.Context(), start.RunID)
	require.NoError(err)
	assert.Equal(after, idempotent)

	nextRun, _, err := st.StartRun(t.Context(), personenrichment.RunStart{
		Kind: "manual", RequestedBy: "post-recovery-budget", RequestedAt: recoveredAt,
	})
	require.NoError(err)
	lease, err := st.ClaimWork(t.Context(), personenrichment.ClaimOptions{
		RunID: nextRun.ID, Owner: "post-recovery-worker", ProviderName: profile.Name,
		Now: recoveredAt, LeaseDuration: time.Minute,
	})
	require.NoError(err)
	require.NotNil(lease)
	person, err := st.GetPersonContext(t.Context(), lease.PersonID)
	require.NoError(err)
	_, created, err = st.BeginAttempt(t.Context(), lease.Token, personenrichment.AttemptStart{
		RunID: nextRun.ID, PersonID: lease.PersonID, ProfileFingerprint: profile.Fingerprint,
		PayloadHash: strings.Repeat("e", 64), RequestHash: strings.Repeat("f", 64),
		PersonRevision: person.Revision, Trigger: lease.Trigger, HardCostCap: true,
		GuaranteedMaxCost: personenrichment.Cost{Currency: "USD", AmountMicros: 600},
	})
	require.NoError(err, "a reconciled reservation must not remain counted against same-day capacity")
	assert.True(created)
}

func TestPersonEnrichmentRecoveryUsesNormalOutcomeDerivation(t *testing.T) {
	seedAttempt := func(t *testing.T, f enrichmentWorkFixture, runID int64, suffix, state, failure string) {
		t.Helper()
		var failureValue any
		if failure != "" {
			failureValue = failure
		}
		_, err := f.store.DB().ExecContext(t.Context(), f.store.Rebind(`
			INSERT INTO person_enrichment_attempts
				(run_id, person_id, profile_fingerprint, trigger_kind, trigger_generation,
				 person_revision, payload_hash, request_hash, state, lease_fence,
				 hard_cost_cap_enforced, reserved_cost_usd_micros, failure_class, completed_at)
			VALUES (?, ?, ?, 'tracked', 'revision:1', ?, ?, ?, ?, 0, FALSE, 0, ?, ?)`),
			runID, f.person.ID, f.profile.Fingerprint, f.person.Revision,
			strings.Repeat(suffix, 64), strings.Repeat(strings.ToUpper(suffix), 64),
			state, failureValue, f.now)
		require.NoError(t, err)
	}

	t.Run("policy only succeeds without failed count", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		f := newEnrichmentWorkFixture(t)
		run := f.startRun(t, "recovery-policy-only")
		seedAttempt(t, f, run.ID, "a", "terminal", "policy")
		recovered, err := f.store.RecoverPersonEnrichmentRunsContext(t.Context(), f.now.Add(time.Minute))
		require.NoError(err)
		assert.Equal(int64(1), recovered)
		got, err := f.store.GetPersonEnrichmentRunContext(t.Context(), run.ID)
		require.NoError(err)
		assert.Equal("succeeded", got.State)
		assert.Equal(int64(1), got.RequestedCount)
		assert.Equal(int64(1), got.StartedCount)
		assert.Zero(got.FailedCount)
	})

	t.Run("mixed policy useful and failure is partial", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		f := newEnrichmentWorkFixture(t)
		run := f.startRun(t, "recovery-mixed-outcomes")
		seedAttempt(t, f, run.ID, "b", "terminal", "policy")
		seedAttempt(t, f, run.ID, "c", "succeeded", "")
		seedAttempt(t, f, run.ID, "d", "terminal", "terminal")
		recovered, err := f.store.RecoverPersonEnrichmentRunsContext(t.Context(), f.now.Add(time.Minute))
		require.NoError(err)
		assert.Equal(int64(1), recovered)
		got, err := f.store.GetPersonEnrichmentRunContext(t.Context(), run.ID)
		require.NoError(err)
		assert.Equal("partial", got.State)
		assert.Equal(int64(3), got.RequestedCount)
		assert.Equal(int64(3), got.StartedCount)
		assert.Equal(int64(1), got.SucceededCount)
		assert.Equal(int64(1), got.FailedCount)
		assert.Zero(got.SuppressedCount)
		assert.Zero(got.IdentityRejectedCount)
	})
}

func TestPersonEnrichmentRunCannotCompleteWithFutureRetry(t *testing.T) {
	testPersonEnrichmentRunCannotCompleteWithDeferredAttempt(t, "retry")
}

func TestPersonEnrichmentRunCannotCompleteWithPendingPoll(t *testing.T) {
	testPersonEnrichmentRunCannotCompleteWithDeferredAttempt(t, "poll")
}

func testPersonEnrichmentRunCannotCompleteWithDeferredAttempt(t *testing.T, kind string) {
	t.Helper()
	require := require.New(t)
	assert := assert.New(t)
	f := newEnrichmentWorkFixture(t)
	run := f.startRun(t, "future-"+kind)
	f.enqueue(t)
	lease := f.claim(t, run.ID, "worker-a")
	attempt, _, err := f.store.BeginAttempt(t.Context(), lease.Token, testAttemptStart(&f, run.ID, "a"))
	require.NoError(err)
	next := f.now.Add(time.Hour)
	if kind == "retry" {
		err = f.store.ScheduleRetry(t.Context(), attempt.Token, personenrichment.RetryUpdate{
			Failure:      personenrichment.SafeFailure{Class: personenrichment.FailureTransient, Message: "safe"},
			NextActionAt: next,
		})
	} else {
		require.NoError(f.store.AuthorizeAttemptDispatch(t.Context(), attempt.Token))
		err = f.store.RecordProviderStarted(t.Context(), attempt.Token, personenrichment.Attempt{
			State: personenrichment.AttemptPending, RequestID: "request", JobID: "job",
			StartedAt:      f.now,
			AdapterVersion: "adapter-v1", SchemaVersion: "schema-v1",
			ProgramFingerprint: strings.Repeat("b", 64),
		})
		require.NoError(err)
		err = f.store.SchedulePoll(t.Context(), attempt.Token, personenrichment.Result{
			State: personenrichment.ResultPending, RequestID: "request", JobID: "job",
			PollAfter: time.Hour, AdapterVersion: "adapter-v1", SchemaVersion: "schema-v1",
		})
	}
	require.NoError(err)

	err = f.store.CompleteRun(t.Context(), run.ID, personenrichment.RunCompletion{State: "succeeded", CompletedAt: f.now})
	require.ErrorIs(err, store.ErrRunNotTerminal)
	got, err := f.store.GetPersonEnrichmentRunContext(t.Context(), run.ID)
	require.NoError(err)
	assert.Equal("running", got.State)

	f.setNow(next)
	reclaimed := f.claim(t, run.ID, "worker-b")
	require.NotNil(reclaimed.ActiveAttempt)
	require.NoError(f.store.MarkTerminal(t.Context(), reclaimed.Token, personenrichment.SafeFailure{
		Class: personenrichment.FailureTerminal, Message: "finished safely",
	}))
	require.NoError(f.store.CompleteRun(t.Context(), run.ID, personenrichment.RunCompletion{
		State: "failed", CompletedAt: f.now,
	}))
}

func TestPersonEnrichmentRunningRunPaginationIncludesDeferredRuns(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newEnrichmentWorkFixture(t)
	var want []int64
	for i := range 3 {
		run, _, err := f.store.StartRun(t.Context(), personenrichment.RunStart{
			Kind: "manual", RequestedBy: "page-" + string(rune('a'+i)), RequestedAt: f.now,
		})
		require.NoError(err)
		want = append(want, run.ID)
	}
	first, err := f.store.ListRunningRuns(t.Context(), personenrichment.RunningRunFilter{Limit: 2})
	require.NoError(err)
	require.Len(first, 2)
	second, err := f.store.ListRunningRuns(t.Context(), personenrichment.RunningRunFilter{
		AfterRequestedAt: first[1].RequestedAt, AfterID: first[1].ID, Limit: 2,
	})
	require.NoError(err)
	require.Len(second, 1)
	assert.Equal(want, []int64{first[0].ID, first[1].ID, second[0].ID})
	_, err = f.store.ListRunningRuns(t.Context(), personenrichment.RunningRunFilter{Limit: 0})
	require.Error(err)
}

func TestPersonEnrichmentCompleteRunDerivesTruthfulState(t *testing.T) {
	terminalPerson := func(t *testing.T, f enrichmentWorkFixture, runID int64, owner, hashByte string) {
		t.Helper()
		lease := f.claim(t, runID, owner)
		start := testAttemptStart(&f, runID, hashByte)
		attempt, created, err := f.store.BeginAttempt(t.Context(), lease.Token, start)
		require.NoError(t, err)
		require.True(t, created)
		require.NoError(t, f.store.MarkTerminal(t.Context(), attempt.Token, personenrichment.SafeFailure{
			Class: personenrichment.FailureTerminal, Message: "synthetic terminal failure",
		}))
	}
	suppressPerson := func(t *testing.T, f enrichmentWorkFixture, runID int64, owner, hashByte string) {
		t.Helper()
		lease := f.claim(t, runID, owner)
		require.NoError(t, f.store.ReleaseWork(t.Context(), lease.Token, personenrichment.WorkRelease{
			Outcome: "suppressed", PersonRevision: f.person.Revision,
			PayloadHash: strings.Repeat(hashByte, 64), RequestHash: strings.Repeat(hashByte, 64),
		}))
	}
	policyPerson := func(t *testing.T, f enrichmentWorkFixture, runID int64, owner, hashByte string) {
		t.Helper()
		lease := f.claim(t, runID, owner)
		require.NoError(t, f.store.ReleaseWork(t.Context(), lease.Token, personenrichment.WorkRelease{
			Outcome: "policy", PersonRevision: f.person.Revision,
			PayloadHash: strings.Repeat(hashByte, 64), RequestHash: strings.Repeat(hashByte, 64),
		}))
	}
	extraTrackedPerson := func(t *testing.T, f *enrichmentWorkFixture) {
		t.Helper()
		participantID, err := f.store.EnsureParticipant(
			"derive-state@example.test", "Derive State", "example.test")
		require.NoError(t, err)
		person, _, err := f.store.CreatePersonFromParticipantContext(t.Context(), participantID)
		require.NoError(t, err)
		require.NoError(t, f.store.PutPersonEnrichmentWorkContext(t.Context(), store.PersonEnrichmentWorkInput{
			PersonID: person.ID, ProfileFingerprint: f.profile.Fingerprint,
			Trigger: personenrichment.Trigger{Kind: personenrichment.TriggerTracked, Generation: "revision:1"},
			DueAt:   f.now,
		}))
	}

	t.Run("all started attempts failed", func(t *testing.T) {
		checks := assert.New(t)
		requirements := require.New(t)
		f := newEnrichmentWorkFixture(t)
		run := f.startRun(t, "derive-failed")
		f.enqueue(t)
		terminalPerson(t, f, run.ID, "derive-failed-worker", "c")
		requirements.NoError(f.store.CompleteRun(t.Context(), run.ID, personenrichment.RunCompletion{State: "", CompletedAt: f.now}))
		got, err := f.store.GetPersonEnrichmentRunContext(t.Context(), run.ID)
		requirements.NoError(err)
		checks.Equal("failed", got.State)
		checks.Equal(int64(1), got.FailedCount)
	})

	t.Run("policy outcomes without failures", func(t *testing.T) {
		checks := assert.New(t)
		requirements := require.New(t)
		f := newEnrichmentWorkFixture(t)
		run := f.startRun(t, "derive-succeeded")
		f.enqueue(t)
		policyPerson(t, f, run.ID, "derive-succeeded-worker", "d")
		requirements.NoError(f.store.CompleteRun(t.Context(), run.ID, personenrichment.RunCompletion{State: "", CompletedAt: f.now}))
		got, err := f.store.GetPersonEnrichmentRunContext(t.Context(), run.ID)
		requirements.NoError(err)
		checks.Equal("succeeded", got.State)
		checks.Zero(got.FailedCount)
	})

	t.Run("mixed outcomes", func(t *testing.T) {
		checks := assert.New(t)
		requirements := require.New(t)
		f := newEnrichmentWorkFixture(t)
		run := f.startRun(t, "derive-partial")
		f.enqueue(t)
		terminalPerson(t, f, run.ID, "derive-partial-worker-a", "e")
		extraTrackedPerson(t, &f)
		suppressPerson(t, f, run.ID, "derive-partial-worker-b", "f")
		requirements.NoError(f.store.CompleteRun(t.Context(), run.ID, personenrichment.RunCompletion{State: "", CompletedAt: f.now}))
		got, err := f.store.GetPersonEnrichmentRunContext(t.Context(), run.ID)
		requirements.NoError(err)
		checks.Equal("partial", got.State)
		checks.Equal(int64(1), got.FailedCount)
		checks.Equal(int64(1), got.SuppressedCount)
	})
}
