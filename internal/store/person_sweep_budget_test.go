package store_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/peoplesweep"
	"go.kenn.io/msgvault/internal/store"
)

const testSweepUTCDate = "2026-08-23"

type personSweepBudgetFixture struct {
	store     *store.Store
	personID  int64
	runID     string
	attemptID string
}

func newPersonSweepBudgetFixture(t *testing.T, suffix string) personSweepBudgetFixture {
	t.Helper()
	journal := newPersonSweepJournalFixture(t, true, false)
	runID := "run-" + suffix
	attemptID := "attempt-" + suffix
	_, err := journal.store.StartPersonSweepRun(t.Context(), peoplesweep.StartRun{
		ID: runID, Kind: peoplesweep.RunManual, Mode: peoplesweep.RunIncremental,
		ProgramFingerprint: "program-fingerprint", CatalogFingerprint: "catalog-fingerprint",
		ProviderFingerprint: "provider-fingerprint", StartedAt: sweepBudgetNow(),
	})
	require.NoError(t, err)
	require.NoError(t, journal.store.StartPersonSweepAttempt(t.Context(), sweepStartAttempt(t,
		attemptID, runID, journal.alicePersonID, 1)))
	lease := sweepAttemptLease(personSweepBudgetFixture{personID: journal.alicePersonID})
	result, err := journal.store.DB().ExecContext(t.Context(), journal.store.Rebind(`
		UPDATE person_sweep_work SET lease_owner = ?, lease_until = ?, lease_fence = ?
		WHERE person_id = ?`), lease.WorkerID, lease.ExpiresAt, lease.Fence, lease.PersonID)
	require.NoError(t, err)
	changed, err := result.RowsAffected()
	require.NoError(t, err)
	require.Equal(t, int64(1), changed, "tracked person must have a person_sweep_work row")
	return personSweepBudgetFixture{store: journal.store, personID: journal.alicePersonID,
		runID: runID, attemptID: attemptID}
}

func sweepBudgetNow() time.Time {
	return time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
}

func sweepStartAttempt(t *testing.T, id, runID string, personID, fence int64) peoplesweep.StartAttempt {
	t.Helper()
	envelope := []peoplesweep.GenerationCursor{{
		Key: peoplesweep.CursorKey{PersonID: personID, SourceLane: peoplesweep.SourceConversationText,
			ProgramFingerprint: "program-fingerprint", CatalogFingerprint: "catalog-fingerprint"},
		Mode: peoplesweep.GenerationCursorOptimistic, CursorFrom: 10, CursorThrough: 20,
	}}
	encoded, err := json.Marshal(envelope)
	require.NoError(t, err)
	digest := sha256.Sum256(encoded)
	return peoplesweep.StartAttempt{ID: id, RunID: runID, PersonID: personID,
		LeaseFence: fence, Mode: peoplesweep.RunIncremental, CursorEnvelope: envelope,
		EnvelopeHash: hex.EncodeToString(digest[:]), StartedAt: sweepBudgetNow()}
}

func generousSweepBudget() peoplesweep.BudgetConfig {
	return peoplesweep.BudgetConfig{
		MaxRequestsPerPerson: 10, MaxInputTokensPerPerson: 10_000,
		MaxOutputTokensPerPerson: 10_000, MaxRequestsPerRun: 100,
		MaxInputTokensPerRun: 100_000, MaxOutputTokensPerRun: 100_000,
		MaxEstimatedCostMicroUSDPerRun: 100_000, MaxRequestsPerDay: 1_000,
		MaxInputTokensPerDay: 1_000_000, MaxOutputTokensPerDay: 1_000_000,
		MaxEstimatedCostMicroUSDPerDay:     1_000_000,
		InputCostMicroUSDPerMillionTokens:  1_000_000,
		OutputCostMicroUSDPerMillionTokens: 1_000_000,
	}
}

func sweepReservation(f personSweepBudgetFixture, ordinal int, input int64,
	profile string, budget peoplesweep.BudgetConfig,
) peoplesweep.BudgetReservationRequest {
	digest := sha256.Sum256([]byte(fmt.Sprintf("wire-%d-%s", ordinal, f.attemptID)))
	return peoplesweep.BudgetReservationRequest{
		RunID: f.runID, AttemptID: f.attemptID, BatchOrdinal: ordinal, CallOrdinal: 0,
		Purpose:  peoplesweep.ProviderCallPurposePrimary,
		PersonID: f.personID, ProviderFingerprint: profile, UTCDate: testSweepUTCDate,
		InputHash: hex.EncodeToString(digest[:]), ItemCount: 3,
		EstimatedRequests: 1, EstimatedInputTokens: input, EstimatedOutputTokens: 100,
		EstimatedCostMicroUSD: input + 100, Budget: budget,
	}
}

func sweepAttemptLease(f personSweepBudgetFixture) peoplesweep.Lease {
	return peoplesweep.Lease{PersonID: f.personID, WorkerID: "test-worker", Fence: 1,
		ExpiresAt: time.Now().UTC().Add(time.Hour)}
}

func TestPersonSweepBudgetRejectsConcurrentDailyOverrun(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	f := newPersonSweepBudgetFixture(t, "concurrent")
	_, err := f.store.StartPersonSweepRun(t.Context(), peoplesweep.StartRun{
		ID: "run-concurrent-two", Kind: peoplesweep.RunManual, Mode: peoplesweep.RunIncremental,
		ProgramFingerprint: "program-fingerprint", CatalogFingerprint: "catalog-fingerprint",
		ProviderFingerprint: "provider-fingerprint", StartedAt: sweepBudgetNow(),
	})
	requirements.NoError(err)
	requirements.NoError(f.store.StartPersonSweepAttempt(t.Context(), sweepStartAttempt(t,
		"attempt-concurrent-two", "run-concurrent-two", f.personID, 2)))
	budget := generousSweepBudget()
	budget.MaxInputTokensPerDay = 1_000

	start := make(chan struct{})
	results := make(chan error, 2)
	var ready sync.WaitGroup
	ready.Add(2)
	requests := []personSweepBudgetFixture{f, {
		store: f.store, personID: f.personID, runID: "run-concurrent-two",
		attemptID: "attempt-concurrent-two",
	}}
	for _, fixture := range requests {
		go func(fixture personSweepBudgetFixture) {
			ready.Done()
			<-start
			request := sweepReservation(fixture, 0, 800, "provider-fingerprint", budget)
			_, err := f.store.ReservePersonSweepBudget(t.Context(), request)
			results <- err
		}(fixture)
	}
	ready.Wait()
	close(start)

	var succeeded, exceeded int
	for range 2 {
		err := <-results
		switch {
		case err == nil:
			succeeded++
		case errors.Is(err, peoplesweep.ErrBudgetExceeded):
			exceeded++
		default:
			requirements.NoError(err)
		}
	}
	checks.Equal(1, succeeded)
	checks.Equal(1, exceeded)
}

func TestPersonSweepBudgetEnforcesPersonRunAndDayDimensions(t *testing.T) {
	tests := []struct {
		name string
		cap  func(*peoplesweep.BudgetConfig)
		use  func(*peoplesweep.BudgetReservationRequest)
	}{
		{"person requests", func(b *peoplesweep.BudgetConfig) { b.MaxRequestsPerPerson = 1 }, func(r *peoplesweep.BudgetReservationRequest) { r.EstimatedRequests = 2 }},
		{"person input", func(b *peoplesweep.BudgetConfig) { b.MaxInputTokensPerPerson = 99 }, nil},
		{"person output", func(b *peoplesweep.BudgetConfig) { b.MaxOutputTokensPerPerson = 99 }, nil},
		{"run requests", func(b *peoplesweep.BudgetConfig) { b.MaxRequestsPerRun = 1 }, func(r *peoplesweep.BudgetReservationRequest) { r.EstimatedRequests = 2 }},
		{"run input", func(b *peoplesweep.BudgetConfig) { b.MaxInputTokensPerRun = 99 }, nil},
		{"run output", func(b *peoplesweep.BudgetConfig) { b.MaxOutputTokensPerRun = 99 }, nil},
		{"run cost", func(b *peoplesweep.BudgetConfig) { b.MaxEstimatedCostMicroUSDPerRun = 199 }, nil},
		{"day requests", func(b *peoplesweep.BudgetConfig) { b.MaxRequestsPerDay = 1 }, func(r *peoplesweep.BudgetReservationRequest) { r.EstimatedRequests = 2 }},
		{"day input", func(b *peoplesweep.BudgetConfig) { b.MaxInputTokensPerDay = 99 }, nil},
		{"day output", func(b *peoplesweep.BudgetConfig) { b.MaxOutputTokensPerDay = 99 }, nil},
		{"day cost", func(b *peoplesweep.BudgetConfig) { b.MaxEstimatedCostMicroUSDPerDay = 199 }, nil},
	}
	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			f := newPersonSweepBudgetFixture(t, fmt.Sprintf("dimension-%d", index))
			budget := generousSweepBudget()
			test.cap(&budget)
			request := sweepReservation(f, 0, 100, "provider-fingerprint", budget)
			if test.use != nil {
				test.use(&request)
			}
			_, err := f.store.ReservePersonSweepBudget(t.Context(), request)
			require.ErrorIs(t, err, peoplesweep.ErrBudgetExceeded)

			budget = generousSweepBudget()
			_, err = f.store.ReservePersonSweepBudget(t.Context(),
				sweepReservation(f, 0, 100, "provider-fingerprint", budget))
			require.NoError(t, err)
		})
	}
}

func TestPersonSweepBudgetAggregatesPersonAcrossAttemptsInRun(t *testing.T) {
	f := newPersonSweepBudgetFixture(t, "person-across-attempts")
	budget := generousSweepBudget()
	budget.MaxRequestsPerPerson = 1
	_, err := f.store.ReservePersonSweepBudget(t.Context(),
		sweepReservation(f, 0, 100, "provider-fingerprint", budget))
	require.NoError(t, err)

	second := f
	second.attemptID = "attempt-person-across-attempts-two"
	require.NoError(t, f.store.StartPersonSweepAttempt(t.Context(), sweepStartAttempt(t,
		second.attemptID, second.runID, second.personID, 2)))
	_, err = f.store.ReservePersonSweepBudget(t.Context(),
		sweepReservation(second, 0, 100, "provider-fingerprint", budget))
	require.ErrorIs(t, err, peoplesweep.ErrBudgetExceeded)
}

func TestPersonSweepBudgetReleaseBeforeNetwork(t *testing.T) {
	t.Run("never started restores every reservation", func(t *testing.T) {
		checks := assert.New(t)
		requirements := require.New(t)
		f := newPersonSweepBudgetFixture(t, "release")
		budget := generousSweepBudget()
		budget.MaxInputTokensPerPerson = 250
		budget.MaxInputTokensPerRun = 250
		budget.MaxInputTokensPerDay = 250
		request := sweepReservation(f, 0, 250, "provider-fingerprint", budget)
		reservation, err := f.store.ReservePersonSweepBudget(t.Context(), request)
		requirements.NoError(err)
		requirements.NoError(f.store.ReleasePersonSweepBudget(t.Context(), reservation))
		requirements.NoError(f.store.ReleasePersonSweepBudget(t.Context(), reservation))

		var status string
		var reservedRequests int
		var reservedInput, reservedOutput, reservedCost int64
		requirements.NoError(f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`
			SELECT status FROM person_sweep_batches
			WHERE attempt_id = ? AND batch_ordinal = ?`), f.attemptID, 0).Scan(&status))
		requirements.NoError(f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`
			SELECT reserved_requests, reserved_input_tokens, reserved_output_tokens,
		       reserved_cost_micro_usd FROM person_sweep_daily_usage WHERE utc_day = ?`),
			testSweepUTCDate).Scan(&reservedRequests, &reservedInput, &reservedOutput, &reservedCost))
		checks.Equal("cancelled", status)
		checks.Equal(0, reservedRequests)
		checks.Zero(reservedInput)
		checks.Zero(reservedOutput)
		checks.Zero(reservedCost)

		replacement := sweepReservation(f, 1, 250, "provider-fingerprint", budget)
		_, err = f.store.ReservePersonSweepBudget(t.Context(), replacement)
		requirements.NoError(err, "release must restore attempt, run, and day capacity")
	})

	t.Run("started work is charged instead of released", func(t *testing.T) {
		checks := assert.New(t)
		requirements := require.New(t)
		f := newPersonSweepBudgetFixture(t, "started")
		reservation, err := f.store.ReservePersonSweepBudget(t.Context(),
			sweepReservation(f, 0, 250, "provider-fingerprint", generousSweepBudget()))
		requirements.NoError(err)
		requirements.NoError(f.store.MarkPersonSweepBudgetStarted(t.Context(), reservation,
			sweepAttemptLease(f)))
		requirements.Error(f.store.ReleasePersonSweepBudget(t.Context(), reservation))
		requirements.NoError(f.store.FinalizePersonSweepFailure(t.Context(), peoplesweep.FailureFinalization{
			Lease: sweepAttemptLease(f), AttemptID: f.attemptID, Class: peoplesweep.FailureTimeout,
			RetryAt: sweepBudgetNow().Add(time.Hour), Reservations: []peoplesweep.BudgetReservation{reservation},
			FinalizedAt: sweepBudgetNow().Add(time.Minute),
		}))
		var status string
		var actualInput int64
		requirements.NoError(f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`
			SELECT status, actual_input_tokens FROM person_sweep_batches
			WHERE attempt_id = ? AND batch_ordinal = ?`), f.attemptID, 0).Scan(&status, &actualInput))
		checks.Equal("failed", status)
		checks.Equal(int64(250), actualInput)
	})
}

func TestPersonSweepBudgetStartedRequiresLiveFencedLease(t *testing.T) {
	requireSweepBatchUntouchedByLostLease := func(t *testing.T, f personSweepBudgetFixture) {
		t.Helper()
		checks := assert.New(t)
		var batchStatus, attemptStatus string
		require.NoError(t, f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`
			SELECT status FROM person_sweep_batches
			WHERE attempt_id = ? AND batch_ordinal = 0 AND call_ordinal = 0`),
			f.attemptID).Scan(&batchStatus))
		require.NoError(t, f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`
			SELECT status FROM person_sweep_attempts WHERE id = ?`),
			f.attemptID).Scan(&attemptStatus))
		checks.Equal("reserved", batchStatus)
		checks.Equal("running", attemptStatus)
		var reservedRequests, actualRequests int
		require.NoError(t, f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`
			SELECT reserved_requests, actual_requests FROM person_sweep_daily_usage
			WHERE utc_day = ?`), testSweepUTCDate).Scan(&reservedRequests, &actualRequests))
		checks.Equal(1, reservedRequests)
		checks.Zero(actualRequests)
	}
	execWork := func(t *testing.T, f personSweepBudgetFixture, query string, args ...any) {
		t.Helper()
		_, err := f.store.DB().ExecContext(t.Context(), f.store.Rebind(query), args...)
		require.NoError(t, err)
	}
	tests := []struct {
		name   string
		mutate func(t *testing.T, f personSweepBudgetFixture, lease peoplesweep.Lease)
	}{
		{name: "stale owner", mutate: func(t *testing.T, f personSweepBudgetFixture,
			lease peoplesweep.Lease,
		) {
			t.Helper()
			execWork(t, f, `UPDATE person_sweep_work SET lease_owner = 'successor-worker'
				WHERE person_id = ?`, lease.PersonID)
		}},
		{name: "stale fence", mutate: func(t *testing.T, f personSweepBudgetFixture,
			lease peoplesweep.Lease,
		) {
			t.Helper()
			execWork(t, f, `UPDATE person_sweep_work SET lease_fence = lease_fence + 1
				WHERE person_id = ?`, lease.PersonID)
		}},
		{name: "expired lease", mutate: func(t *testing.T, f personSweepBudgetFixture,
			lease peoplesweep.Lease,
		) {
			t.Helper()
			execWork(t, f, `UPDATE person_sweep_work SET lease_until = ? WHERE person_id = ?`,
				time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC), lease.PersonID)
		}},
		{name: "reclaimed successor", mutate: func(t *testing.T, f personSweepBudgetFixture,
			lease peoplesweep.Lease,
		) {
			t.Helper()
			execWork(t, f, `UPDATE person_sweep_work SET lease_owner = 'successor-worker',
				lease_fence = lease_fence + 1, lease_until = ? WHERE person_id = ?`,
				time.Now().UTC().Add(time.Hour), lease.PersonID)
		}},
		{name: "missing work row", mutate: func(t *testing.T, f personSweepBudgetFixture,
			lease peoplesweep.Lease,
		) {
			t.Helper()
			execWork(t, f, `DELETE FROM person_sweep_work WHERE person_id = ?`, lease.PersonID)
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			requirements := require.New(t)
			f := newPersonSweepBudgetFixture(t, "lease-"+strings.ReplaceAll(test.name, " ", "-"))
			reservation, err := f.store.ReservePersonSweepBudget(t.Context(),
				sweepReservation(f, 0, 100, "provider-fingerprint", generousSweepBudget()))
			requirements.NoError(err)
			lease := sweepAttemptLease(f)
			test.mutate(t, f, lease)

			err = f.store.MarkPersonSweepBudgetStarted(t.Context(), reservation, lease)
			requirements.ErrorIs(err, peoplesweep.ErrLeaseLost,
				"a stale worker must not start reserved provider calls")
			requireSweepBatchUntouchedByLostLease(t, f)
		})
	}

	t.Run("lease bound to another person", func(t *testing.T) {
		requirements := require.New(t)
		f := newPersonSweepBudgetFixture(t, "lease-other-person")
		reservation, err := f.store.ReservePersonSweepBudget(t.Context(),
			sweepReservation(f, 0, 100, "provider-fingerprint", generousSweepBudget()))
		requirements.NoError(err)
		lease := sweepAttemptLease(f)
		lease.PersonID++
		err = f.store.MarkPersonSweepBudgetStarted(t.Context(), reservation, lease)
		requirements.ErrorIs(err, peoplesweep.ErrLeaseLost)
	})

	t.Run("committed reclaim converges to the typed lease-lost error", func(t *testing.T) {
		checks := assert.New(t)
		requirements := require.New(t)
		f := newPersonSweepBudgetFixture(t, "lease-reclaim-convergence")
		reservation, err := f.store.ReservePersonSweepBudget(t.Context(),
			sweepReservation(f, 0, 100, "provider-fingerprint", generousSweepBudget()))
		requirements.NoError(err)
		lease := sweepAttemptLease(f)
		_, err = f.store.DB().ExecContext(t.Context(), f.store.Rebind(`
			UPDATE person_sweep_work SET lease_until = ? WHERE person_id = ?`),
			time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC), lease.PersonID)
		requirements.NoError(err)

		// The successor's claim reclaims the expired lease and finalizes the
		// abandoned attempt, which is exactly the durable state a deadlock-aborted
		// mark observes on its contention retry once the reclaim commits.
		reclaimed, claimErr := f.store.ClaimPersonSweep(t.Context(), peoplesweep.ClaimRequest{
			WorkerID: "successor-worker", LeaseDuration: time.Minute,
		})
		requirements.NoError(claimErr)
		requirements.NotNil(reclaimed)
		requirements.Equal(f.personID, reclaimed.PersonID)

		err = f.store.MarkPersonSweepBudgetStarted(t.Context(), reservation, lease)
		requirements.ErrorIs(err, peoplesweep.ErrLeaseLost,
			"a mark that lost its lease to a committed reclaim must report it typed")
		var batchStatus, attemptStatus string
		requirements.NoError(f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`
			SELECT status FROM person_sweep_batches
			WHERE attempt_id = ? AND batch_ordinal = 0 AND call_ordinal = 0`),
			f.attemptID).Scan(&batchStatus))
		requirements.NoError(f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`
			SELECT status FROM person_sweep_attempts WHERE id = ?`),
			f.attemptID).Scan(&attemptStatus))
		checks.Equal("cancelled", batchStatus)
		checks.Equal("failed", attemptStatus)
	})

	t.Run("live lease marks the batch running", func(t *testing.T) {
		checks := assert.New(t)
		requirements := require.New(t)
		f := newPersonSweepBudgetFixture(t, "lease-live")
		reservation, err := f.store.ReservePersonSweepBudget(t.Context(),
			sweepReservation(f, 0, 100, "provider-fingerprint", generousSweepBudget()))
		requirements.NoError(err)
		requirements.NoError(f.store.MarkPersonSweepBudgetStarted(t.Context(),
			reservation, sweepAttemptLease(f)))
		var status string
		requirements.NoError(f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`
			SELECT status FROM person_sweep_batches
			WHERE attempt_id = ? AND batch_ordinal = 0 AND call_ordinal = 0`),
			f.attemptID).Scan(&status))
		checks.Equal("running", status)
	})

	t.Run("running replay still requires a current live lease", func(t *testing.T) {
		checks := assert.New(t)
		requirements := require.New(t)
		f := newPersonSweepBudgetFixture(t, "lease-running-replay")
		reservation, err := f.store.ReservePersonSweepBudget(t.Context(),
			sweepReservation(f, 0, 100, "provider-fingerprint", generousSweepBudget()))
		requirements.NoError(err)
		lease := sweepAttemptLease(f)
		requirements.NoError(f.store.MarkPersonSweepBudgetStarted(t.Context(), reservation, lease))
		_, err = f.store.DB().ExecContext(t.Context(), f.store.Rebind(`
			UPDATE person_sweep_work SET lease_until = ? WHERE person_id = ?`),
			time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC), lease.PersonID)
		requirements.NoError(err)

		err = f.store.MarkPersonSweepBudgetStarted(t.Context(), reservation, lease)
		requirements.ErrorIs(err, peoplesweep.ErrLeaseLost,
			"the idempotent running replay must still require a live lease")
		var status string
		requirements.NoError(f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`
			SELECT status FROM person_sweep_batches
			WHERE attempt_id = ? AND batch_ordinal = 0 AND call_ordinal = 0`),
			f.attemptID).Scan(&status))
		checks.Equal("running", status)
	})
}

func TestFinalizePersonSweepFailureRequiresCurrentLease(t *testing.T) {
	execWork := func(t *testing.T, f personSweepBudgetFixture, query string, args ...any) {
		t.Helper()
		_, err := f.store.DB().ExecContext(t.Context(), f.store.Rebind(query), args...)
		require.NoError(t, err)
	}
	requireRunningAttemptUntouched := func(t *testing.T, f personSweepBudgetFixture) {
		t.Helper()
		var batchStatus, attemptStatus, requestID string
		require.NoError(t, f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`
			SELECT status, provider_request_id FROM person_sweep_batches
			WHERE attempt_id = ? AND batch_ordinal = 0 AND call_ordinal = 0`),
			f.attemptID).Scan(&batchStatus, &requestID))
		require.NoError(t, f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`
			SELECT status FROM person_sweep_attempts WHERE id = ?`),
			f.attemptID).Scan(&attemptStatus))
		assert.Equal(t, "running", batchStatus,
			"a stale worker must not terminalize its running batch")
		assert.Equal(t, "running", attemptStatus,
			"a stale worker must not fail the still-running attempt")
		assert.Empty(t, requestID)
		var reservedRequests, actualRequests int
		require.NoError(t, f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`
			SELECT reserved_requests, actual_requests FROM person_sweep_daily_usage
			WHERE utc_day = ?`), testSweepUTCDate).Scan(&reservedRequests, &actualRequests))
		assert.Equal(t, 1, reservedRequests)
		assert.Zero(t, actualRequests, "a stale worker must not account actual usage")
	}
	staleFinalization := func(f personSweepBudgetFixture, lease peoplesweep.Lease,
		reservation peoplesweep.BudgetReservation) peoplesweep.FailureFinalization {
		return peoplesweep.FailureFinalization{Lease: lease, AttemptID: f.attemptID,
			Class: peoplesweep.FailureProviderHTTP, RetryAt: sweepBudgetNow().Add(time.Hour),
			Reservations: []peoplesweep.BudgetReservation{reservation},
			Completed: []peoplesweep.CompletedUsage{{BatchOrdinal: 0, CallOrdinal: 0,
				Purpose: peoplesweep.ProviderCallPurposePrimary, UsageKnown: true,
				ProviderRequestID: "safe-request-id", Usage: peoplesweep.TokenUsage{
					InputTokens: 300, OutputTokens: 150}, Latency: time.Second}},
			FinalizedAt: sweepBudgetNow().Add(time.Minute)}
	}
	tests := []struct {
		name   string
		mutate func(t *testing.T, f personSweepBudgetFixture, lease peoplesweep.Lease)
	}{
		{name: "stale owner", mutate: func(t *testing.T, f personSweepBudgetFixture,
			lease peoplesweep.Lease,
		) {
			t.Helper()
			execWork(t, f, `UPDATE person_sweep_work SET lease_owner = 'successor-worker'
				WHERE person_id = ?`, lease.PersonID)
		}},
		{name: "stale fence", mutate: func(t *testing.T, f personSweepBudgetFixture,
			lease peoplesweep.Lease,
		) {
			t.Helper()
			execWork(t, f, `UPDATE person_sweep_work SET lease_fence = lease_fence + 1
				WHERE person_id = ?`, lease.PersonID)
		}},
		{name: "expired lease", mutate: func(t *testing.T, f personSweepBudgetFixture,
			lease peoplesweep.Lease,
		) {
			t.Helper()
			execWork(t, f, `UPDATE person_sweep_work SET lease_until = ? WHERE person_id = ?`,
				time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC), lease.PersonID)
		}},
		{name: "missing work row", mutate: func(t *testing.T, f personSweepBudgetFixture,
			lease peoplesweep.Lease,
		) {
			t.Helper()
			execWork(t, f, `DELETE FROM person_sweep_work WHERE person_id = ?`, lease.PersonID)
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			f := newPersonSweepBudgetFixture(t, "finalize-lease-"+strings.ReplaceAll(test.name, " ", "-"))
			reservation, err := f.store.ReservePersonSweepBudget(t.Context(),
				sweepReservation(f, 0, 100, "provider-fingerprint", generousSweepBudget()))
			require.NoError(t, err)
			lease := sweepAttemptLease(f)
			require.NoError(t, f.store.MarkPersonSweepBudgetStarted(t.Context(), reservation, lease))
			test.mutate(t, f, lease)

			err = f.store.FinalizePersonSweepFailure(t.Context(), staleFinalization(f, lease, reservation))
			require.ErrorIs(t, err, peoplesweep.ErrLeaseLost,
				"a stale worker must not finalize failure without a current lease")
			requireRunningAttemptUntouched(t, f)
		})
	}

	t.Run("expired lease converges through successor reclaim", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		f := newPersonSweepBudgetFixture(t, "finalize-lease-reclaim")
		reservation, err := f.store.ReservePersonSweepBudget(t.Context(),
			sweepReservation(f, 0, 100, "provider-fingerprint", generousSweepBudget()))
		require.NoError(err)
		lease := sweepAttemptLease(f)
		require.NoError(f.store.MarkPersonSweepBudgetStarted(t.Context(), reservation, lease))
		execWork(t, f, `UPDATE person_sweep_work SET lease_until = ? WHERE person_id = ?`,
			time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC), lease.PersonID)

		err = f.store.FinalizePersonSweepFailure(t.Context(), staleFinalization(f, lease, reservation))
		require.ErrorIs(err, peoplesweep.ErrLeaseLost)

		// The successor's claim reclaims the expired lease and finalizes the
		// abandoned attempt, so the fenced-out stale worker leaves no stranded
		// durable state behind.
		reclaimed, claimErr := f.store.ClaimPersonSweep(t.Context(), peoplesweep.ClaimRequest{
			WorkerID: "successor-worker", LeaseDuration: time.Minute,
		})
		require.NoError(claimErr)
		require.NotNil(reclaimed)
		require.Equal(f.personID, reclaimed.PersonID)

		var attemptStatus, failureClass, batchStatus string
		require.NoError(f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`
			SELECT status, failure_class FROM person_sweep_attempts WHERE id = ?`),
			f.attemptID).Scan(&attemptStatus, &failureClass))
		require.NoError(f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`
			SELECT status FROM person_sweep_batches
			WHERE attempt_id = ? AND batch_ordinal = 0 AND call_ordinal = 0`),
			f.attemptID).Scan(&batchStatus))
		assert.Equal(string(peoplesweep.AttemptFailed), attemptStatus)
		assert.Equal(string(peoplesweep.FailureLeaseLost), failureClass)
		assert.Equal("failed", batchStatus)
		var reservedRequests, actualRequests int
		require.NoError(f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`
			SELECT reserved_requests, actual_requests FROM person_sweep_daily_usage
			WHERE utc_day = ?`), testSweepUTCDate).Scan(&reservedRequests, &actualRequests))
		assert.Zero(reservedRequests)
		assert.Equal(1, actualRequests,
			"the reclaim conservatively charges the started reservation")

		replay := staleFinalization(f, lease, reservation)
		replay.Class = peoplesweep.FailureLeaseLost
		replay.Completed = nil
		require.NoError(f.store.FinalizePersonSweepFailure(t.Context(), replay),
			"an exact lease-lost replay of the reclaimed attempt stays a no-op")
	})

	t.Run("finalized replay survives the released lease", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		f := newPersonSweepBudgetFixture(t, "finalize-lease-replay")
		reservation, err := f.store.ReservePersonSweepBudget(t.Context(),
			sweepReservation(f, 0, 100, "provider-fingerprint", generousSweepBudget()))
		require.NoError(err)
		lease := sweepAttemptLease(f)
		finalization := peoplesweep.FailureFinalization{Lease: lease, AttemptID: f.attemptID,
			Class: peoplesweep.FailureTimeout, RetryAt: sweepBudgetNow().Add(time.Hour),
			Reservations: []peoplesweep.BudgetReservation{reservation},
			FinalizedAt:  sweepBudgetNow().Add(time.Minute)}
		require.NoError(f.store.FinalizePersonSweepFailure(t.Context(), finalization))

		var owner string
		var leaseUntil any
		require.NoError(f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`
			SELECT lease_owner, lease_until FROM person_sweep_work WHERE person_id = ?`),
			f.personID).Scan(&owner, &leaseUntil))
		assert.Empty(owner, "the original finalize released the lease")
		assert.Nil(leaseUntil)

		require.NoError(f.store.FinalizePersonSweepFailure(t.Context(), finalization),
			"an idempotent replay must not require lease currency")
	})
}

func TestPersonSweepBudgetReconcilesActualUsage(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	f := newPersonSweepBudgetFixture(t, "reconcile")
	reservation, err := f.store.ReservePersonSweepBudget(t.Context(),
		sweepReservation(f, 0, 250, "provider-fingerprint", generousSweepBudget()))
	requirements.NoError(err)
	finalization := peoplesweep.FailureFinalization{Lease: sweepAttemptLease(f), AttemptID: f.attemptID,
		Class: peoplesweep.FailureProviderHTTP, RetryAt: sweepBudgetNow().Add(time.Hour),
		Reservations: []peoplesweep.BudgetReservation{reservation},
		Completed: []peoplesweep.CompletedUsage{{BatchOrdinal: 0, CallOrdinal: 0,
			Purpose: peoplesweep.ProviderCallPurposePrimary, UsageKnown: true,
			ProviderRequestID: "safe-request-id", Usage: peoplesweep.TokenUsage{
				InputTokens: 300, OutputTokens: 150}, Latency: 2 * time.Second}},
		FinalizedAt: sweepBudgetNow().Add(time.Minute)}
	requirements.NoError(f.store.FinalizePersonSweepFailure(t.Context(), finalization))
	requirements.NoError(f.store.FinalizePersonSweepFailure(t.Context(), finalization))
	mismatchedReplay := finalization
	mismatchedReplay.Class = peoplesweep.FailureBudget
	requirements.Error(f.store.FinalizePersonSweepFailure(t.Context(), mismatchedReplay),
		"a terminal attempt must reject replay under a different failure class")

	var reservedRequests, actualRequests int
	var reservedInput, actualInput, actualOutput, actualCost int64
	requirements.NoError(f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`
		SELECT reserved_requests, actual_requests, reserved_input_tokens,
		       actual_input_tokens, actual_output_tokens, actual_cost_micro_usd
		FROM person_sweep_daily_usage WHERE utc_day = ?`), testSweepUTCDate).Scan(
		&reservedRequests, &actualRequests, &reservedInput, &actualInput, &actualOutput, &actualCost))
	checks.Zero(reservedRequests)
	checks.Zero(reservedInput)
	checks.Equal(1, actualRequests)
	checks.Equal(int64(300), actualInput)
	checks.Equal(int64(150), actualOutput)
	checks.Equal(int64(450), actualCost)
}

func TestPersonSweepBudgetJournalsPrimaryAndRepairCalls(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newPersonSweepBudgetFixture(t, "call-ordinals")
	primaryRequest := sweepReservation(f, 0, 250, "provider-fingerprint", generousSweepBudget())
	primaryRequest.CallOrdinal = 0
	primaryRequest.Purpose = peoplesweep.ProviderCallPurposePrimary
	primary, err := f.store.ReservePersonSweepBudget(t.Context(), primaryRequest)
	require.NoError(err)
	require.NoError(f.store.MarkPersonSweepBudgetStarted(t.Context(), primary, sweepAttemptLease(f)))

	repairRequest := primaryRequest
	repairRequest.CallOrdinal = 1
	repairRequest.Purpose = peoplesweep.ProviderCallPurposeRepair
	repairRequest.InputHash = strings.Repeat("d", 64)
	repair, err := f.store.ReservePersonSweepBudget(t.Context(), repairRequest)
	require.NoError(err)
	assert.NotEqual(primary.ID, repair.ID)

	rows, err := f.store.DB().QueryContext(t.Context(), f.store.Rebind(`
		SELECT batch_ordinal, call_ordinal, purpose
		FROM person_sweep_batches WHERE attempt_id = ?
		ORDER BY batch_ordinal, call_ordinal`), f.attemptID)
	require.NoError(err)
	defer func() { require.NoError(rows.Close()) }()
	var got []peoplesweep.ProviderCallCoordinate
	for rows.Next() {
		var coordinate peoplesweep.ProviderCallCoordinate
		require.NoError(rows.Scan(&coordinate.BatchOrdinal, &coordinate.CallOrdinal,
			&coordinate.Purpose))
		got = append(got, coordinate)
	}
	require.NoError(rows.Err())
	assert.Equal([]peoplesweep.ProviderCallCoordinate{
		{BatchOrdinal: 0, CallOrdinal: 0, Purpose: peoplesweep.ProviderCallPurposePrimary},
		{BatchOrdinal: 0, CallOrdinal: 1, Purpose: peoplesweep.ProviderCallPurposeRepair},
	}, got)
}

func TestPersonSweepBudgetConcurrentRepairReservationReplaysOneCall(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newPersonSweepBudgetFixture(t, "concurrent-repair")
	primaryRequest := sweepReservation(f, 0, 250, "provider-fingerprint", generousSweepBudget())
	primary, err := f.store.ReservePersonSweepBudget(t.Context(), primaryRequest)
	require.NoError(err)
	require.NoError(f.store.MarkPersonSweepBudgetStarted(t.Context(), primary, sweepAttemptLease(f)))
	repairRequest := primaryRequest
	repairRequest.CallOrdinal = 1
	repairRequest.Purpose = peoplesweep.ProviderCallPurposeRepair
	repairRequest.InputHash = strings.Repeat("d", 64)

	start := make(chan struct{})
	results := make(chan peoplesweep.BudgetReservation, 2)
	errs := make(chan error, 2)
	var ready sync.WaitGroup
	ready.Add(2)
	for range 2 {
		go func() {
			ready.Done()
			<-start
			reservation, reserveErr := f.store.ReservePersonSweepBudget(t.Context(), repairRequest)
			results <- reservation
			errs <- reserveErr
		}()
	}
	ready.Wait()
	close(start)
	first, second := <-results, <-results
	require.NoError(<-errs)
	require.NoError(<-errs)
	assert.Equal(first.ID, second.ID)
	var rows int
	require.NoError(f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`
		SELECT COUNT(*) FROM person_sweep_batches WHERE attempt_id = ?`),
		f.attemptID).Scan(&rows))
	assert.Equal(2, rows)
}

func TestPersonSweepBudgetRejectsInvalidCallCoordinates(t *testing.T) {
	tests := []struct {
		name        string
		callOrdinal int
		purpose     string
	}{
		{name: "primary purpose mismatch", callOrdinal: 0, purpose: peoplesweep.ProviderCallPurposeRepair},
		{name: "repair purpose mismatch", callOrdinal: 1, purpose: peoplesweep.ProviderCallPurposePrimary},
		{name: "third call", callOrdinal: 2, purpose: peoplesweep.ProviderCallPurposeRepair},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			f := newPersonSweepBudgetFixture(t, "invalid-call-"+strings.ReplaceAll(test.name, " ", "-"))
			request := sweepReservation(f, 0, 250, "provider-fingerprint", generousSweepBudget())
			request.CallOrdinal = test.callOrdinal
			request.Purpose = test.purpose
			_, err := f.store.ReservePersonSweepBudget(t.Context(), request)
			require.Error(t, err)
		})
	}
}

func TestPersonSweepBudgetRejectsCallGaps(t *testing.T) {
	t.Run("repair without primary", func(t *testing.T) {
		f := newPersonSweepBudgetFixture(t, "repair-without-primary")
		request := sweepReservation(f, 0, 250, "provider-fingerprint", generousSweepBudget())
		request.CallOrdinal = 1
		request.Purpose = peoplesweep.ProviderCallPurposeRepair
		_, err := f.store.ReservePersonSweepBudget(t.Context(), request)
		require.Error(t, err)
	})
	t.Run("repair before primary starts", func(t *testing.T) {
		f := newPersonSweepBudgetFixture(t, "repair-before-primary")
		primaryRequest := sweepReservation(f, 0, 250, "provider-fingerprint", generousSweepBudget())
		_, err := f.store.ReservePersonSweepBudget(t.Context(), primaryRequest)
		require.NoError(t, err)
		repairRequest := primaryRequest
		repairRequest.CallOrdinal = 1
		repairRequest.Purpose = peoplesweep.ProviderCallPurposeRepair
		repairRequest.InputHash = strings.Repeat("c", 64)
		_, err = f.store.ReservePersonSweepBudget(t.Context(), repairRequest)
		require.Error(t, err)
	})
	t.Run("primary batch gap", func(t *testing.T) {
		f := newPersonSweepBudgetFixture(t, "primary-gap")
		request := sweepReservation(f, 1, 250, "provider-fingerprint", generousSweepBudget())
		_, err := f.store.ReservePersonSweepBudget(t.Context(), request)
		require.Error(t, err)
	})
}

func TestPersonSweepBudgetMissingUsageChargesFullReservation(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newPersonSweepBudgetFixture(t, "missing-usage")
	request := sweepReservation(f, 0, 250, "provider-fingerprint", generousSweepBudget())
	request.CallOrdinal = 0
	request.Purpose = peoplesweep.ProviderCallPurposePrimary
	reservation, err := f.store.ReservePersonSweepBudget(t.Context(), request)
	require.NoError(err)
	require.NoError(f.store.MarkPersonSweepBudgetStarted(t.Context(), reservation, sweepAttemptLease(f)))
	require.NoError(f.store.FinalizePersonSweepFailure(t.Context(), peoplesweep.FailureFinalization{
		Lease: sweepAttemptLease(f), AttemptID: f.attemptID,
		Class: peoplesweep.FailureInvalidOutput, RetryAt: sweepBudgetNow().Add(time.Hour),
		Reservations: []peoplesweep.BudgetReservation{reservation},
		Completed: []peoplesweep.CompletedUsage{{BatchOrdinal: 0, CallOrdinal: 0,
			Purpose:           peoplesweep.ProviderCallPurposePrimary,
			ProviderRequestID: "request-without-usage", UsageKnown: false}},
		FinalizedAt: sweepBudgetNow().Add(time.Minute),
	}))

	var requests int
	var inputTokens, outputTokens, cost int64
	require.NoError(f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`
		SELECT actual_requests, actual_input_tokens, actual_output_tokens, actual_cost_micro_usd
		FROM person_sweep_batches
		WHERE attempt_id = ? AND batch_ordinal = 0 AND call_ordinal = 0`),
		f.attemptID).Scan(&requests, &inputTokens, &outputTokens, &cost))
	assert.Equal(request.EstimatedRequests, requests)
	assert.Equal(request.EstimatedInputTokens, inputTokens)
	assert.Equal(request.EstimatedOutputTokens, outputTokens)
	assert.Equal(request.EstimatedCostMicroUSD, cost)
}

func TestPersonSweepBudgetDoesNotTrustProviderUnderreporting(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	f := newPersonSweepBudgetFixture(t, "underreport")
	reservation, err := f.store.ReservePersonSweepBudget(t.Context(),
		sweepReservation(f, 0, 250, "provider-fingerprint", generousSweepBudget()))
	requirements.NoError(err)
	requirements.NoError(f.store.FinalizePersonSweepFailure(t.Context(), peoplesweep.FailureFinalization{
		Lease: sweepAttemptLease(f), AttemptID: f.attemptID, Class: peoplesweep.FailureTimeout,
		RetryAt: sweepBudgetNow().Add(time.Hour), Reservations: []peoplesweep.BudgetReservation{reservation},
		Completed: []peoplesweep.CompletedUsage{{BatchOrdinal: 0, CallOrdinal: 0,
			Purpose: peoplesweep.ProviderCallPurposePrimary, UsageKnown: true,
			Usage: peoplesweep.TokenUsage{InputTokens: 1, OutputTokens: 1}}},
		FinalizedAt: sweepBudgetNow().Add(time.Minute),
	}))

	var actualInput, actualOutput, actualCost int64
	requirements.NoError(f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`
		SELECT actual_input_tokens, actual_output_tokens, actual_cost_micro_usd
		FROM person_sweep_batches WHERE attempt_id = ? AND batch_ordinal = ?`),
		f.attemptID, 0).Scan(&actualInput, &actualOutput, &actualCost))
	checks.Equal(int64(250), actualInput)
	checks.Equal(int64(100), actualOutput)
	checks.Equal(int64(350), actualCost)
}

func TestPersonSweepBudgetAggregatesRunAcrossAttempts(t *testing.T) {
	requirements := require.New(t)
	f := newPersonSweepBudgetFixture(t, "attempt-run")
	requirements.NoError(f.store.StartPersonSweepAttempt(t.Context(), sweepStartAttempt(t,
		"attempt-run-two", f.runID, f.personID, 2)))
	budget := generousSweepBudget()
	budget.MaxInputTokensPerPerson = 2_000
	budget.MaxInputTokensPerRun = 1_500

	_, err := f.store.ReservePersonSweepBudget(t.Context(),
		sweepReservation(f, 0, 800, "provider-fingerprint", budget))
	requirements.NoError(err)
	second := sweepReservation(f, 0, 600, "provider-fingerprint", budget)
	second.AttemptID = "attempt-run-two"
	_, err = f.store.ReservePersonSweepBudget(t.Context(), second)
	requirements.NoError(err)

	second.BatchOrdinal = 1
	digest := sha256.Sum256([]byte("input-hash-run-over"))
	second.InputHash = hex.EncodeToString(digest[:])
	second.EstimatedInputTokens = 200
	second.EstimatedCostMicroUSD = 300
	_, err = f.store.ReservePersonSweepBudget(t.Context(), second)
	requirements.ErrorIs(err, peoplesweep.ErrBudgetExceeded)
}

func TestPersonSweepDailyBudgetAggregatesAcrossProfiles(t *testing.T) {
	requirements := require.New(t)
	f := newPersonSweepBudgetFixture(t, "profiles")
	budget := generousSweepBudget()
	budget.MaxInputTokensPerDay = 1_000
	_, err := f.store.ReservePersonSweepBudget(t.Context(),
		sweepReservation(f, 0, 600, "provider-fingerprint", budget))
	requirements.NoError(err)

	_, err = f.store.StartPersonSweepRun(t.Context(), peoplesweep.StartRun{
		ID: "run-profile-two", Kind: peoplesweep.RunManual, Mode: peoplesweep.RunIncremental,
		ProgramFingerprint: "program-fingerprint", CatalogFingerprint: "catalog-fingerprint",
		ProviderFingerprint: "provider-fingerprint-b", StartedAt: sweepBudgetNow(),
	})
	requirements.NoError(err)
	requirements.NoError(f.store.StartPersonSweepAttempt(t.Context(), sweepStartAttempt(t,
		"attempt-profile-two", "run-profile-two", f.personID, 2)))
	secondFixture := f
	secondFixture.runID = "run-profile-two"
	secondFixture.attemptID = "attempt-profile-two"
	second := sweepReservation(secondFixture, 0, 600, "provider-fingerprint-b", budget)
	_, err = f.store.ReservePersonSweepBudget(t.Context(), second)
	requirements.ErrorIs(err, peoplesweep.ErrBudgetExceeded)
}

func TestPersonSweepBudgetAuthenticatesBoundedWireMetadata(t *testing.T) {
	t.Run("canonical SHA-256", func(t *testing.T) {
		f := newPersonSweepBudgetFixture(t, "invalid-hash")
		request := sweepReservation(f, 0, 100, "provider-fingerprint", generousSweepBudget())
		request.InputHash = "not-a-sha256"
		_, err := f.store.ReservePersonSweepBudget(t.Context(), request)
		require.Error(t, err)
	})

	t.Run("bounded item count", func(t *testing.T) {
		f := newPersonSweepBudgetFixture(t, "item-bound")
		request := sweepReservation(f, 0, 100, "provider-fingerprint", generousSweepBudget())
		request.ItemCount = int(int64(math.MaxInt32) + 1)
		_, err := f.store.ReservePersonSweepBudget(t.Context(), request)
		require.Error(t, err)
	})

	t.Run("release coordinates are immutable", func(t *testing.T) {
		requirements := require.New(t)
		f := newPersonSweepBudgetFixture(t, "immutable-release")
		reservation, err := f.store.ReservePersonSweepBudget(t.Context(),
			sweepReservation(f, 0, 100, "provider-fingerprint", generousSweepBudget()))
		requirements.NoError(err)
		reservation.Request.UTCDate = "2026-08-24"
		requirements.Error(f.store.ReleasePersonSweepBudget(t.Context(), reservation))

		var status string
		requirements.NoError(f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`
			SELECT status FROM person_sweep_batches
			WHERE attempt_id = ? AND batch_ordinal = ?`), f.attemptID, 0).Scan(&status))
		assert.Equal(t, "reserved", status)
	})

	t.Run("started replay is idempotent and terminal work is rejected", func(t *testing.T) {
		requirements := require.New(t)
		f := newPersonSweepBudgetFixture(t, "started-replay")
		reservation, err := f.store.ReservePersonSweepBudget(t.Context(),
			sweepReservation(f, 0, 100, "provider-fingerprint", generousSweepBudget()))
		requirements.NoError(err)
		requirements.NoError(f.store.MarkPersonSweepBudgetStarted(t.Context(), reservation,
			sweepAttemptLease(f)))
		requirements.NoError(f.store.MarkPersonSweepBudgetStarted(t.Context(), reservation,
			sweepAttemptLease(f)))

		mismatch := reservation
		mismatch.Request.InputHash = strings.Repeat("f", 64)
		requirements.Error(f.store.MarkPersonSweepBudgetStarted(t.Context(), mismatch, sweepAttemptLease(f)))

		requirements.NoError(f.store.FinalizePersonSweepFailure(t.Context(), peoplesweep.FailureFinalization{
			Lease: sweepAttemptLease(f), AttemptID: f.attemptID, Class: peoplesweep.FailureTimeout,
			RetryAt:      sweepBudgetNow().Add(time.Hour),
			Reservations: []peoplesweep.BudgetReservation{reservation},
			FinalizedAt:  sweepBudgetNow().Add(time.Minute),
		}))
		requirements.Error(f.store.MarkPersonSweepBudgetStarted(t.Context(), reservation, sweepAttemptLease(f)))
	})
}

func TestPersonSweepBudgetRejectsReservationPolicyReplay(t *testing.T) {
	for _, mutate := range []struct {
		name string
		fn   func(*peoplesweep.BudgetReservation)
	}{
		{"cap", func(r *peoplesweep.BudgetReservation) {
			r.Request.Budget.MaxInputTokensPerDay++
		}},
		{"price", func(r *peoplesweep.BudgetReservation) {
			r.Request.Budget.InputCostMicroUSDPerMillionTokens++
		}},
	} {
		t.Run(mutate.name, func(t *testing.T) {
			checks := assert.New(t)
			requirements := require.New(t)
			f := newPersonSweepBudgetFixture(t, "policy-replay-"+mutate.name)
			reservation, err := f.store.ReservePersonSweepBudget(t.Context(),
				sweepReservation(f, 0, 250, "provider-fingerprint", generousSweepBudget()))
			requirements.NoError(err)
			var durableReservationID, durableBudgetFingerprint string
			requirements.NoError(f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`
				SELECT reservation_id, budget_fingerprint FROM person_sweep_batches
				WHERE attempt_id = ? AND batch_ordinal = 0`), f.attemptID).Scan(
				&durableReservationID, &durableBudgetFingerprint))
			checks.Equal(reservation.ID, durableReservationID)
			checks.Len(durableBudgetFingerprint, 64)
			mutate.fn(&reservation)
			err = f.store.FinalizePersonSweepFailure(t.Context(), peoplesweep.FailureFinalization{
				Lease: sweepAttemptLease(f), AttemptID: f.attemptID,
				Class: peoplesweep.FailureProviderHTTP, RetryAt: sweepBudgetNow().Add(time.Hour),
				Reservations: []peoplesweep.BudgetReservation{reservation},
				Completed: []peoplesweep.CompletedUsage{{BatchOrdinal: 0, CallOrdinal: 0,
					Purpose: peoplesweep.ProviderCallPurposePrimary, UsageKnown: true,
					Usage: peoplesweep.TokenUsage{InputTokens: 500, OutputTokens: 200}}},
				FinalizedAt: sweepBudgetNow().Add(time.Minute),
			})
			requirements.Error(err)

			var status string
			var actualCost int64
			requirements.NoError(f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`
				SELECT status, actual_cost_micro_usd FROM person_sweep_batches
				WHERE attempt_id = ? AND batch_ordinal = ?`), f.attemptID, 0).Scan(&status, &actualCost))
			checks.Equal("reserved", status)
			checks.Zero(actualCost)
		})
	}
}

func TestPersonSweepBudgetReleaseFinalizeLockOrder(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	f := newPersonSweepBudgetFixture(t, "lock-order")
	requirements.NoError(f.store.StartPersonSweepAttempt(t.Context(), sweepStartAttempt(t,
		"attempt-lock-order-two", f.runID, f.personID, 1)))
	first, err := f.store.ReservePersonSweepBudget(t.Context(),
		sweepReservation(f, 0, 100, "provider-fingerprint", generousSweepBudget()))
	requirements.NoError(err)
	secondFixture := f
	secondFixture.attemptID = "attempt-lock-order-two"
	second, err := f.store.ReservePersonSweepBudget(t.Context(),
		sweepReservation(secondFixture, 0, 100, "provider-fingerprint", generousSweepBudget()))
	requirements.NoError(err)

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	start := make(chan struct{})
	errs := make(chan error, 2)
	go func() {
		<-start
		errs <- f.store.ReleasePersonSweepBudget(ctx, first)
	}()
	go func() {
		<-start
		errs <- f.store.FinalizePersonSweepFailure(ctx, peoplesweep.FailureFinalization{
			Lease: sweepAttemptLease(secondFixture), AttemptID: secondFixture.attemptID,
			Class: peoplesweep.FailureTimeout, RetryAt: sweepBudgetNow().Add(time.Hour),
			Reservations: []peoplesweep.BudgetReservation{second},
			Completed: []peoplesweep.CompletedUsage{{BatchOrdinal: 0, CallOrdinal: 0,
				Purpose: peoplesweep.ProviderCallPurposePrimary, UsageKnown: true,
				ProviderRequestID: "safe-request-id", Usage: peoplesweep.TokenUsage{
					InputTokens: 100, OutputTokens: 100}}},
			FinalizedAt: sweepBudgetNow().Add(time.Minute),
		})
	}()
	close(start)
	for range 2 {
		requirements.NoError(<-errs)
	}

	var reservedRequests, actualRequests int
	requirements.NoError(f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`
		SELECT reserved_requests, actual_requests FROM person_sweep_daily_usage
		WHERE utc_day = ?`), testSweepUTCDate).Scan(&reservedRequests, &actualRequests))
	checks.Zero(reservedRequests)
	checks.Equal(1, actualRequests)
}

func TestPersonSweepBudgetRejectsTerminalRun(t *testing.T) {
	t.Run("reserve", func(t *testing.T) {
		f := newPersonSweepBudgetFixture(t, "terminal-reserve")
		_, err := f.store.DB().ExecContext(t.Context(), f.store.Rebind(`
			UPDATE person_sweep_runs SET status = 'failed', completed_at = ? WHERE id = ?`),
			sweepBudgetNow(), f.runID)
		require.NoError(t, err)
		_, err = f.store.ReservePersonSweepBudget(t.Context(),
			sweepReservation(f, 0, 100, "provider-fingerprint", generousSweepBudget()))
		require.Error(t, err)
	})

	t.Run("mark started", func(t *testing.T) {
		f := newPersonSweepBudgetFixture(t, "terminal-start")
		reservation, err := f.store.ReservePersonSweepBudget(t.Context(),
			sweepReservation(f, 0, 100, "provider-fingerprint", generousSweepBudget()))
		require.NoError(t, err)
		_, err = f.store.DB().ExecContext(t.Context(), f.store.Rebind(`
			UPDATE person_sweep_runs SET status = 'failed', completed_at = ? WHERE id = ?`),
			sweepBudgetNow(), f.runID)
		require.NoError(t, err)
		require.Error(t, f.store.MarkPersonSweepBudgetStarted(t.Context(), reservation, sweepAttemptLease(f)))
	})
}

func TestPersonSweepRunFinishAccountingRaceIsSerialized(t *testing.T) {
	for _, operation := range []string{"reserve", "mark"} {
		t.Run(operation, func(t *testing.T) {
			f := newPersonSweepBudgetFixture(t, "finish-race-"+operation)
			request := sweepReservation(f, 0, 100, "provider-fingerprint", generousSweepBudget())
			var reservation peoplesweep.BudgetReservation
			if operation == "mark" {
				var err error
				reservation, err = f.store.ReservePersonSweepBudget(t.Context(), request)
				require.NoError(t, err)
			}

			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			start := make(chan struct{})
			finishErr := make(chan error, 1)
			accountErr := make(chan error, 1)
			go func() {
				<-start
				finishErr <- f.store.FinishPersonSweepRun(ctx, f.runID,
					peoplesweep.RunFailed, sweepBudgetNow().Add(time.Minute))
			}()
			go func() {
				<-start
				if operation == "reserve" {
					_, err := f.store.ReservePersonSweepBudget(ctx, request)
					accountErr <- err
					return
				}
				accountErr <- f.store.MarkPersonSweepBudgetStarted(ctx, reservation, sweepAttemptLease(f))
			}()
			close(start)
			require.Error(t, <-finishErr)
			require.NoError(t, <-accountErr)
			assertPersonSweepRunStillRunning(t, f)
		})
	}
}
