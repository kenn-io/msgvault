package store_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/peoplesweep"
)

func TestFinalizePersonSweepFailureAccountsAfterLeaseLoss(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	journal := newPersonSweepJournalFixture(t, true, false)
	journal.insertMessage(t, "lease-loss", "email", journal.aliceID, sweepBudgetNow())
	reconcilePersonSweepFixture(t, journal, peoplesweep.SourceConversationText)
	first := claimPersonSweepFixture(t, journal.store, "worker-old")

	_, err := journal.store.StartPersonSweepRun(t.Context(), peoplesweep.StartRun{
		ID: "run-lease-loss", Kind: peoplesweep.RunScheduled, Mode: peoplesweep.RunIncremental,
		ProgramFingerprint: "program-fingerprint", CatalogFingerprint: "catalog-fingerprint",
		ProviderFingerprint: "provider-fingerprint", StartedAt: sweepBudgetNow(),
	})
	requirements.NoError(err)
	requirements.NoError(journal.store.StartPersonSweepAttempt(t.Context(), sweepStartAttempt(t,
		"attempt-lease-loss", "run-lease-loss", journal.alicePersonID, first.Fence)))
	f := personSweepBudgetFixture{store: journal.store, personID: journal.alicePersonID,
		runID: "run-lease-loss", attemptID: "attempt-lease-loss"}
	reservation, err := journal.store.ReservePersonSweepBudget(t.Context(),
		sweepReservation(f, 0, 250, "provider-fingerprint", generousSweepBudget()))
	requirements.NoError(err)
	requirements.NoError(journal.store.MarkPersonSweepBudgetStarted(t.Context(), reservation))

	_, err = journal.store.DB().ExecContext(t.Context(), journal.store.Rebind(`
		UPDATE person_sweep_work SET lease_until = ? WHERE person_id = ?`),
		time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC), first.PersonID)
	requirements.NoError(err)
	second := claimPersonSweepFixture(t, journal.store, "worker-new")
	cursorKey := personSweepConversationCursorKey(journal.alicePersonID)
	beforeCursor := loadPersonSweepCursor(t, journal.store, cursorKey)

	finalization := peoplesweep.FailureFinalization{Lease: *first,
		AttemptID: "attempt-lease-loss", Class: peoplesweep.FailureProviderHTTP,
		RetryAt: sweepBudgetNow().Add(time.Hour), Reservations: []peoplesweep.BudgetReservation{reservation},
		Completed: []peoplesweep.CompletedUsage{{BatchOrdinal: 0,
			ProviderRequestID: "safe-request-id", Usage: peoplesweep.TokenUsage{
				InputTokens: 300, OutputTokens: 150}, Latency: time.Second}},
		FinalizedAt: sweepBudgetNow().Add(time.Minute)}
	requirements.NoError(journal.store.FinalizePersonSweepFailure(t.Context(), finalization))
	requirements.NoError(journal.store.FinalizePersonSweepFailure(t.Context(), finalization))

	var owner string
	var fence int64
	requirements.NoError(journal.store.DB().QueryRowContext(t.Context(), journal.store.Rebind(`
		SELECT lease_owner, lease_fence FROM person_sweep_work WHERE person_id = ?`),
		journal.alicePersonID).Scan(&owner, &fence))
	checks.Equal(second.WorkerID, owner)
	checks.Equal(second.Fence, fence)
	checks.Equal(beforeCursor, loadPersonSweepCursor(t, journal.store, cursorKey))

	attempts, err := journal.store.ListPersonSweepAttempts(t.Context(), peoplesweep.AttemptFilter{
		RunID: "run-lease-loss", Limit: 10,
	})
	requirements.NoError(err)
	requirements.Len(attempts, 1)
	checks.Equal(peoplesweep.AttemptFailed, attempts[0].Status)
	checks.Equal(peoplesweep.Usage{Requests: 1, InputTokens: 250,
		OutputTokens: 100, EstimatedCostMicroUSD: 350}, attempts[0].Usage,
		"a reclaimed started request keeps its conservative reservation charge")
}

func TestPersonSweepHistoryContainsOnlySafeMetadata(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	f := newPersonSweepBudgetFixture(t, "safe-history")
	reservation, err := f.store.ReservePersonSweepBudget(t.Context(),
		sweepReservation(f, 0, 250, "provider-fingerprint", generousSweepBudget()))
	requirements.NoError(err)
	requirements.NoError(f.store.FinalizePersonSweepFailure(t.Context(), peoplesweep.FailureFinalization{
		Lease: sweepAttemptLease(f), AttemptID: f.attemptID, Class: peoplesweep.FailureInvalidOutput,
		RetryAt: sweepBudgetNow().Add(time.Hour), Reservations: []peoplesweep.BudgetReservation{reservation},
		Completed: []peoplesweep.CompletedUsage{{BatchOrdinal: 0,
			ProviderRequestID: "request-id-safe", Usage: peoplesweep.TokenUsage{
				InputTokens: 300, OutputTokens: 150}, Latency: 250 * time.Millisecond}},
		FinalizedAt: sweepBudgetNow().Add(time.Minute),
	}))
	_, err = f.store.DB().ExecContext(t.Context(), f.store.Rebind(`
		UPDATE person_sweep_attempts SET seed_count = 2, context_count = 3,
		       claim_count = 4, decision_count = 5, projected_write_count = 6
		WHERE id = ?`), f.attemptID)
	requirements.NoError(err)
	_, err = f.store.DB().ExecContext(t.Context(), f.store.Rebind(`
		UPDATE person_sweep_runs SET projected_write_count = 6 WHERE id = ?`), f.runID)
	requirements.NoError(err)
	requirements.NoError(f.store.FinishPersonSweepRun(t.Context(), f.runID,
		peoplesweep.RunFailed, sweepBudgetNow().Add(2*time.Minute)))

	runs, err := f.store.ListPersonSweepRuns(t.Context(), peoplesweep.RunFilter{
		PersonID: f.personID, Limit: 10,
	})
	requirements.NoError(err)
	requirements.Len(runs, 1)
	checks.Equal(peoplesweep.RunFailed, runs[0].Status)
	checks.Equal(1, runs[0].Attempts)
	checks.Equal(1, runs[0].Failures)
	checks.Equal(6, runs[0].ProjectedWrites)
	checks.Equal("program-fingerprint", runs[0].ProgramFingerprint)
	checks.Equal("catalog-fingerprint", runs[0].CatalogFingerprint)
	checks.Equal("provider-fingerprint", runs[0].ProviderFingerprint)
	checks.Equal(int64(300), runs[0].Usage.InputTokens)

	attempts, err := f.store.ListPersonSweepAttempts(t.Context(), peoplesweep.AttemptFilter{
		PersonID: f.personID, Limit: 10,
	})
	requirements.NoError(err)
	requirements.Len(attempts, 1)
	checks.Equal("request-id-safe", attempts[0].ProviderRequestID)
	checks.Equal(peoplesweep.FailureInvalidOutput, attempts[0].FailureClass)
	checks.Equal("program-fingerprint", attempts[0].ProgramFingerprint)
	checks.Equal("catalog-fingerprint", attempts[0].CatalogFingerprint)
	checks.Equal("provider-fingerprint", attempts[0].ProviderFingerprint)
	checks.Equal(2, attempts[0].SeedCount)
	checks.Equal(3, attempts[0].ContextCount)
	checks.Equal(4, attempts[0].ClaimCount)
	checks.Equal(5, attempts[0].DecisionCount)
	checks.Equal(6, attempts[0].ProjectedWrites)
	checks.NotEmpty(attempts[0].CursorEnvelope)

	wire, err := json.Marshal(struct { //nolint:musttag // Every anonymous wire field is tagged below.
		Runs     []peoplesweep.RunSummary     `json:"runs"`
		Attempts []peoplesweep.AttemptSummary `json:"attempts"`
	}{runs, attempts})
	requirements.NoError(err)
	for _, forbidden := range []string{
		"evidence", "excerpt", "response_body", "input_text", "credential", "api_key",
	} {
		checks.NotContains(strings.ToLower(string(wire)), forbidden)
	}
	checks.LessOrEqual(len(runs), 200)
	checks.LessOrEqual(len(attempts), 200)

	_, err = f.store.ListPersonSweepRuns(t.Context(), peoplesweep.RunFilter{Limit: 201})
	requirements.Error(err)
	_, err = f.store.ListPersonSweepAttempts(t.Context(), peoplesweep.AttemptFilter{Limit: 0})
	requirements.Error(err)
}

func TestPersonSweepOperationalStatusSelectsLatestSafeFailure(t *testing.T) {
	for _, test := range []struct {
		name        string
		attemptTime string
		workTime    string
		want        peoplesweep.FailureClass
	}{
		{
			name: "newer pre-attempt work failure", attemptTime: "2026-08-23T12:00:00Z",
			workTime: "2026-08-23T12:01:00Z", want: peoplesweep.FailureTimeout,
		},
		{
			name: "newer attempt failure", attemptTime: "2026-08-23T12:01:00Z",
			workTime: "2026-08-23T12:00:00Z", want: peoplesweep.FailureInvalidOutput,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			checks, must := assert.New(t), require.New(t)
			f := newPersonSweepBudgetFixture(t, "latest-failure-"+strings.ReplaceAll(test.name, " ", "-"))
			_, err := f.store.DB().ExecContext(t.Context(), f.store.Rebind(`
				UPDATE person_sweep_attempts
				SET status = 'failed', failure_class = 'invalid_output', completed_at = ?
				WHERE id = ?`), test.attemptTime, f.attemptID)
			must.NoError(err)
			_, err = f.store.DB().ExecContext(t.Context(), f.store.Rebind(`
				UPDATE person_sweep_work
				SET last_failure_class = 'timeout', updated_at = ?
				WHERE person_id = ?`), test.workTime, f.personID)
			must.NoError(err)

			status, err := f.store.PersonSweepOperationalStatus(t.Context())
			must.NoError(err)
			checks.Equal(test.want, status.LastFailure)
		})
	}
}

func TestFinalizePersonSweepFailureRejectsAttemptLeaseMismatch(t *testing.T) {
	for _, mutate := range []struct {
		name string
		fn   func(*peoplesweep.Lease)
	}{
		{"person", func(lease *peoplesweep.Lease) { lease.PersonID++ }},
		{"fence", func(lease *peoplesweep.Lease) { lease.Fence++ }},
	} {
		t.Run(mutate.name, func(t *testing.T) {
			checks := assert.New(t)
			requirements := require.New(t)
			f := newPersonSweepBudgetFixture(t, "lease-mismatch-"+mutate.name)
			reservation, err := f.store.ReservePersonSweepBudget(t.Context(),
				sweepReservation(f, 0, 250, "provider-fingerprint", generousSweepBudget()))
			requirements.NoError(err)
			lease := sweepAttemptLease(f)
			mutate.fn(&lease)
			err = f.store.FinalizePersonSweepFailure(t.Context(), peoplesweep.FailureFinalization{
				Lease: lease, AttemptID: f.attemptID, Class: peoplesweep.FailureTimeout,
				RetryAt:      sweepBudgetNow().Add(time.Hour),
				Reservations: []peoplesweep.BudgetReservation{reservation},
				Completed: []peoplesweep.CompletedUsage{{BatchOrdinal: 0,
					Usage: peoplesweep.TokenUsage{InputTokens: 300, OutputTokens: 150}}},
				FinalizedAt: sweepBudgetNow().Add(time.Minute),
			})
			requirements.Error(err)

			var batchStatus, attemptStatus string
			requirements.NoError(f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`
				SELECT status FROM person_sweep_batches WHERE attempt_id = ? AND batch_ordinal = 0`),
				f.attemptID).Scan(&batchStatus))
			requirements.NoError(f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`
				SELECT status FROM person_sweep_attempts WHERE id = ?`), f.attemptID).Scan(&attemptStatus))
			checks.Equal("reserved", batchStatus)
			checks.Equal("running", attemptStatus)
		})
	}
}

func TestFinalizePersonSweepFailureRequiresFullBatchCoverage(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	f := newPersonSweepBudgetFixture(t, "full-coverage")
	first, err := f.store.ReservePersonSweepBudget(t.Context(),
		sweepReservation(f, 0, 100, "provider-fingerprint", generousSweepBudget()))
	requirements.NoError(err)
	second, err := f.store.ReservePersonSweepBudget(t.Context(),
		sweepReservation(f, 1, 100, "provider-fingerprint", generousSweepBudget()))
	requirements.NoError(err)

	err = f.store.FinalizePersonSweepFailure(t.Context(), peoplesweep.FailureFinalization{
		Lease: sweepAttemptLease(f), AttemptID: f.attemptID, Class: peoplesweep.FailureTimeout,
		RetryAt:      sweepBudgetNow().Add(time.Hour),
		Reservations: []peoplesweep.BudgetReservation{first},
		FinalizedAt:  sweepBudgetNow().Add(time.Minute),
	})
	requirements.Error(err)

	var reservedRequests int
	requirements.NoError(f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`
		SELECT reserved_requests FROM person_sweep_daily_usage WHERE utc_day = ?`),
		testSweepUTCDate).Scan(&reservedRequests))
	checks.Equal(2, reservedRequests)
	for _, ordinal := range []int{0, 1} {
		var status string
		requirements.NoError(f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`
			SELECT status FROM person_sweep_batches WHERE attempt_id = ? AND batch_ordinal = ?`),
			f.attemptID, ordinal).Scan(&status))
		checks.Equal("reserved", status)
	}

	requirements.NoError(f.store.FinalizePersonSweepFailure(t.Context(), peoplesweep.FailureFinalization{
		Lease: sweepAttemptLease(f), AttemptID: f.attemptID, Class: peoplesweep.FailureTimeout,
		RetryAt:      sweepBudgetNow().Add(time.Hour),
		Reservations: []peoplesweep.BudgetReservation{first, second},
		FinalizedAt:  sweepBudgetNow().Add(time.Minute),
	}))
}

func TestPersonSweepHistoryRejectsUnsafeProviderRequestID(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	f := newPersonSweepBudgetFixture(t, "unsafe-request-id")
	reservation, err := f.store.ReservePersonSweepBudget(t.Context(),
		sweepReservation(f, 0, 100, "provider-fingerprint", generousSweepBudget()))
	requirements.NoError(err)
	err = f.store.FinalizePersonSweepFailure(t.Context(), peoplesweep.FailureFinalization{
		Lease: sweepAttemptLease(f), AttemptID: f.attemptID,
		Class: peoplesweep.FailureInvalidOutput, RetryAt: sweepBudgetNow().Add(time.Hour),
		Reservations: []peoplesweep.BudgetReservation{reservation},
		Completed: []peoplesweep.CompletedUsage{{BatchOrdinal: 0,
			ProviderRequestID: "unsafe\narchive-derived-value",
			Usage:             peoplesweep.TokenUsage{InputTokens: 100, OutputTokens: 100}}},
		FinalizedAt: sweepBudgetNow().Add(time.Minute),
	})
	requirements.Error(err)

	var requestID, status string
	requirements.NoError(f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`
		SELECT provider_request_id, status FROM person_sweep_batches
		WHERE attempt_id = ? AND batch_ordinal = 0`), f.attemptID).Scan(&requestID, &status))
	checks.Empty(requestID)
	checks.Equal("reserved", status)
}

func TestPersonSweepHistoryAttemptGenerationIdentity(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	f := newPersonSweepBudgetFixture(t, "generation")
	attempts, err := f.store.ListPersonSweepAttempts(t.Context(), peoplesweep.AttemptFilter{
		RunID: f.runID, Limit: 10,
	})
	requirements.NoError(err)
	requirements.Len(attempts, 1)
	checks.Nil(attempts[0].GenerationID)
	checks.Empty(attempts[0].GenerationKey)

	var generationID int64
	err = f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`
		INSERT INTO person_fact_generations
			(person_id, generation_key, source_cursors_json, program_id, program_version,
			 program_fingerprint, catalog_fingerprint, provider, provider_version,
			 model, model_version, provider_policy_fingerprint, resolved_at)
		VALUES (?, ?, '[]', 'program', 'v1', ?, 'catalog', 'provider', 'v1',
		        'model', 'v1', 'policy', ?)
		RETURNING id`), f.personID, "generation-key", strings.Repeat("a", 64),
		sweepBudgetNow()).Scan(&generationID)
	requirements.NoError(err)
	_, err = f.store.DB().ExecContext(t.Context(), f.store.Rebind(`
		UPDATE person_sweep_attempts
		SET status = 'succeeded', generation_id = ?, generation_key = ?, completed_at = ?
		WHERE id = ?`), generationID, "generation-key", sweepBudgetNow(), f.attemptID)
	requirements.NoError(err)

	attempts, err = f.store.ListPersonSweepAttempts(t.Context(), peoplesweep.AttemptFilter{
		RunID: f.runID, Limit: 10,
	})
	requirements.NoError(err)
	requirements.Len(attempts, 1)
	requirements.NotNil(attempts[0].GenerationID)
	checks.Equal(generationID, *attempts[0].GenerationID)
	checks.Equal("generation-key", attempts[0].GenerationKey)

	_, err = f.store.DB().ExecContext(t.Context(), f.store.Rebind(`
		UPDATE person_sweep_attempts SET generation_id = ? WHERE id = ?`),
		generationID+9999, f.attemptID)
	requirements.Error(err)

	var indexCount int
	if f.store.IsPostgreSQL() {
		err = f.store.DB().QueryRowContext(t.Context(), `
			SELECT COUNT(*) FROM pg_indexes
			WHERE schemaname = current_schema()
			  AND indexname = 'idx_person_sweep_attempts_generation'
			  AND indexdef LIKE '%(generation_id)%'`).Scan(&indexCount)
	} else {
		err = f.store.DB().QueryRowContext(t.Context(), `
			SELECT COUNT(*) FROM sqlite_master
			WHERE type = 'index' AND name = 'idx_person_sweep_attempts_generation'
			  AND sql LIKE '%(generation_id)%'`).Scan(&indexCount)
	}
	requirements.NoError(err)
	checks.Equal(1, indexCount)
}

func TestPersonSweepRunRejectsTerminalizationWithActiveAccounting(t *testing.T) {
	t.Run("running attempt", func(t *testing.T) {
		f := newPersonSweepBudgetFixture(t, "finish-running-attempt")
		err := f.store.FinishPersonSweepRun(t.Context(), f.runID,
			peoplesweep.RunFailed, sweepBudgetNow().Add(time.Minute))
		require.Error(t, err)
		assertPersonSweepRunStillRunning(t, f)
	})

	t.Run("nonterminal batch", func(t *testing.T) {
		f := newPersonSweepBudgetFixture(t, "finish-reserved-batch")
		_, err := f.store.ReservePersonSweepBudget(t.Context(),
			sweepReservation(f, 0, 100, "provider-fingerprint", generousSweepBudget()))
		require.NoError(t, err)
		_, err = f.store.DB().ExecContext(t.Context(), f.store.Rebind(`
			UPDATE person_sweep_attempts SET status = 'cancelled', completed_at = ? WHERE id = ?`),
			sweepBudgetNow(), f.attemptID)
		require.NoError(t, err)

		err = f.store.FinishPersonSweepRun(t.Context(), f.runID,
			peoplesweep.RunFailed, sweepBudgetNow().Add(time.Minute))
		require.Error(t, err)
		assertPersonSweepRunStillRunning(t, f)
	})
}

func TestPersonSweepRunTerminalReplayPreservesCompletedAt(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	journal := newPersonSweepJournalFixture(t, true, false)
	_, err := journal.store.StartPersonSweepRun(t.Context(), peoplesweep.StartRun{
		ID: "run-terminal-replay", Kind: peoplesweep.RunManual, Mode: peoplesweep.RunIncremental,
		ProgramFingerprint: "program-fingerprint", CatalogFingerprint: "catalog-fingerprint",
		ProviderFingerprint: "provider-fingerprint", StartedAt: sweepBudgetNow(),
	})
	requirements.NoError(err)
	completedAt := sweepBudgetNow().Add(time.Minute)
	requirements.NoError(journal.store.FinishPersonSweepRun(t.Context(), "run-terminal-replay",
		peoplesweep.RunFailed, completedAt))
	requirements.NoError(journal.store.FinishPersonSweepRun(t.Context(), "run-terminal-replay",
		peoplesweep.RunFailed, completedAt))
	requirements.Error(journal.store.FinishPersonSweepRun(t.Context(), "run-terminal-replay",
		peoplesweep.RunFailed, completedAt.Add(time.Second)))
	requirements.Error(journal.store.FinishPersonSweepRun(t.Context(), "run-terminal-replay",
		peoplesweep.RunPartial, completedAt))

	runs, err := journal.store.ListPersonSweepRuns(t.Context(), peoplesweep.RunFilter{Limit: 10})
	requirements.NoError(err)
	requirements.Len(runs, 1)
	requirements.NotNil(runs[0].CompletedAt)
	checks.Equal(completedAt, *runs[0].CompletedAt)
	checks.Equal(peoplesweep.RunFailed, runs[0].Status)
}

func assertPersonSweepRunStillRunning(t *testing.T, f personSweepBudgetFixture) {
	t.Helper()
	var status string
	var completedAt any
	require.NoError(t, f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`
		SELECT status, completed_at FROM person_sweep_runs WHERE id = ?`), f.runID).Scan(
		&status, &completedAt))
	assert.Equal(t, "running", status)
	assert.Nil(t, completedAt)
}
