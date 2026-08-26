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
		suppressPerson(t, f, run.ID, "derive-succeeded-worker", "d")
		requirements.NoError(f.store.CompleteRun(t.Context(), run.ID, personenrichment.RunCompletion{State: "", CompletedAt: f.now}))
		got, err := f.store.GetPersonEnrichmentRunContext(t.Context(), run.ID)
		requirements.NoError(err)
		checks.Equal("succeeded", got.State)
		checks.Equal(int64(1), got.SuppressedCount)
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
