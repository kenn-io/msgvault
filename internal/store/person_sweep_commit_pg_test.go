package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/peoplesweep"
)

func TestPersonSweepPostgreSQLApplyConsentLinearizesWithRevoke(t *testing.T) {
	t.Run("apply locks active grant before revoke", func(t *testing.T) {
		requirements := require.New(t)
		f := newPersonSweepApplyFixture(t, "pg-consent-apply-first", true)
		if !f.store.IsPostgreSQL() {
			t.Skip("PostgreSQL-only consent row-lock regression")
		}
		entered, release := make(chan struct{}), make(chan struct{})
		var releaseOnce sync.Once
		t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
		ctx := withPersonSweepApplyFailpoint(t.Context(), func(stage string) error {
			if stage == "consent" {
				close(entered)
				<-release
			}
			return nil
		})
		applyResult := make(chan error, 1)
		go func() {
			_, err := f.store.ApplyPersonSweep(ctx, f.request)
			applyResult <- err
		}()
		select {
		case <-entered:
		case err := <-applyResult:
			requirements.Failf("apply exited before consent fence", "error: %v", err)
		case <-time.After(5 * time.Second):
			requirements.Fail("apply did not reach consent fence")
		}
		revokeResult := make(chan error, 1)
		go func() {
			changed, err := f.store.RevokePersonInferenceConsent(
				t.Context(), f.request.Generation.Policy.ProviderPolicyFingerprint, "reviewer-test")
			if err == nil && !changed {
				err = errors.New("expected active consent to be revoked")
			}
			revokeResult <- err
		}()
		select {
		case err := <-revokeResult:
			requirements.Failf("revoke passed locked consent", "error: %v", err)
		case <-time.After(200 * time.Millisecond):
		}
		releaseOnce.Do(func() { close(release) })
		requirements.NoError(<-applyResult)
		requirements.NoError(<-revokeResult)
	})

	t.Run("committed revoke rejects waiting apply", func(t *testing.T) {
		requirements := require.New(t)
		f := newPersonSweepApplyFixture(t, "pg-consent-revoke-first", true)
		if !f.store.IsPostgreSQL() {
			t.Skip("PostgreSQL-only consent row-lock regression")
		}
		tx, err := f.store.DB().BeginTx(t.Context(), nil)
		requirements.NoError(err)
		t.Cleanup(func() { _ = tx.Rollback() })
		_, err = tx.ExecContext(t.Context(), f.store.Rebind(`
			UPDATE person_inference_consents SET revoked_by = ?, revoked_at = CURRENT_TIMESTAMP
			WHERE profile_fingerprint = ? AND revoked_at IS NULL`), "reviewer-test",
			f.request.Generation.Policy.ProviderPolicyFingerprint)
		requirements.NoError(err)
		applyResult := make(chan error, 1)
		go func() {
			_, applyErr := f.store.ApplyPersonSweep(t.Context(), f.request)
			applyResult <- applyErr
		}()
		select {
		case applyErr := <-applyResult:
			requirements.Failf("apply passed uncommitted revoke", "error: %v", applyErr)
		case <-time.After(200 * time.Millisecond):
		}
		requirements.NoError(tx.Commit())
		applyErr := <-applyResult
		requirements.ErrorIs(applyErr, peoplesweep.ErrPersonSweepConsentRevoked)
		assert.Zero(t, personFactProjectionRowCount(t, f.store, "person_fact_generations"))
	})
}

func TestPersonSweepPostgreSQLReclaimedFinalizerAndSuccessorApplyUseOneLockOrder(t *testing.T) {
	requirements := require.New(t)
	f := newPersonSweepApplyFixture(t, "pg-reclaimed-finalizer", true)
	if !f.store.IsPostgreSQL() {
		t.Skip("PostgreSQL-only reclaimed-finalizer/successor-apply lock-order regression")
	}

	successorRun, successorAttempt := "run-pg-successor", "attempt-pg-successor"
	_, err := f.store.StartPersonSweepRun(t.Context(), peoplesweep.StartRun{
		ID: successorRun, Kind: peoplesweep.RunManual, Mode: peoplesweep.RunIncremental,
		ProgramFingerprint:  f.request.Generation.ProgramFingerprint,
		CatalogFingerprint:  f.request.Generation.CatalogFingerprint,
		ProviderFingerprint: f.request.Generation.Policy.ProviderPolicyFingerprint,
		StartedAt:           personFactLedgerNow,
	})
	requirements.NoError(err)
	envelopeJSON, err := json.Marshal(f.request.CursorEnvelope)
	requirements.NoError(err)
	envelopeDigest := sha256.Sum256(envelopeJSON)
	requirements.NoError(f.store.StartPersonSweepAttempt(t.Context(), peoplesweep.StartAttempt{
		ID: successorAttempt, RunID: successorRun, PersonID: f.personID, LeaseFence: 2,
		Mode: peoplesweep.RunIncremental, CursorEnvelope: f.request.CursorEnvelope,
		EnvelopeHash: hex.EncodeToString(envelopeDigest[:]), StartedAt: personFactLedgerNow,
	}))
	successorLease := peoplesweep.Lease{PersonID: f.personID, WorkerID: "worker-pg-successor",
		Fence: 2, ExpiresAt: time.Now().UTC().Add(time.Hour)}
	_, err = f.store.db.ExecContext(t.Context(), `UPDATE person_sweep_work SET
		lease_owner = ?, lease_fence = ?, lease_until = ? WHERE person_id = ?`,
		successorLease.WorkerID, successorLease.Fence,
		f.store.dialect.TimestampParam(successorLease.ExpiresAt), f.personID)
	requirements.NoError(err)
	successorReservation, err := f.store.ReservePersonSweepBudget(t.Context(),
		peoplesweep.BudgetReservationRequest{RunID: successorRun, AttemptID: successorAttempt,
			BatchOrdinal: 0, CallOrdinal: 0, Purpose: peoplesweep.ProviderCallPurposePrimary,
			PersonID:            f.personID,
			ProviderFingerprint: f.request.Generation.Policy.ProviderPolicyFingerprint,
			UTCDate:             "2026-08-23", InputHash: "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd",
			ItemCount: 1, EstimatedRequests: 1, EstimatedInputTokens: 3,
			EstimatedOutputTokens: 2, EstimatedCostMicroUSD: 5, Budget: personSweepApplyBudget()})
	requirements.NoError(err)
	requirements.NoError(f.store.MarkPersonSweepBudgetStarted(t.Context(), successorReservation,
		successorLease))
	successorRequest := f.request
	successorRequest.RunID = successorRun
	successorRequest.AttemptID = successorAttempt
	successorRequest.Lease = successorLease
	successorRequest.Batches = append([]peoplesweep.CompletedBatch(nil), f.request.Batches...)
	successorRequest.Batches[0].ReservationID = successorReservation.ID
	successorRequest.Batches[0].InputHash = successorReservation.Request.InputHash

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	start := make(chan struct{})
	finalizeErrs, applyErrs := make(chan error, 1), make(chan error, 1)
	go func() {
		<-start
		finalizeErrs <- f.store.FinalizePersonSweepFailure(ctx, peoplesweep.FailureFinalization{
			Lease: f.lease, AttemptID: f.attemptID, Class: peoplesweep.FailureTimeout,
			RetryAt:      personFactLedgerNow.Add(time.Hour),
			Reservations: []peoplesweep.BudgetReservation{f.reservation},
			Completed: []peoplesweep.CompletedUsage{{BatchOrdinal: 0, CallOrdinal: 0,
				Purpose: peoplesweep.ProviderCallPurposePrimary, UsageKnown: true,
				ProviderRequestID: "request-old-finalizer",
				Usage:             peoplesweep.TokenUsage{InputTokens: 2, OutputTokens: 1}, Latency: time.Second}},
			FinalizedAt: personFactLedgerNow.Add(time.Minute),
		})
	}()
	go func() {
		<-start
		_, applyErr := f.store.ApplyPersonSweep(ctx, successorRequest)
		applyErrs <- applyErr
	}()
	close(start)
	// Both calls must return inside the shared deadline: the stale finalizer
	// and the successor apply lock daily usage, batches, and the work row in
	// one order, so a reclaim racing apply cannot deadlock the pair.
	applyErr := <-applyErrs
	finalizeErr := <-finalizeErrs
	requirements.NoError(applyErr)
	// The reclaimed lease no longer authorizes failure finalization: the
	// successor's reclaim owns terminalizing the stale attempt, so the stale
	// finalizer is fenced off with the typed lease-lost error before it can
	// terminalize batches or account usage.
	requirements.ErrorIs(finalizeErr, peoplesweep.ErrLeaseLost)

	assert := assert.New(t)
	// Durable state converges on the successor alone: the fenced finalizer
	// leaves the stale attempt, its batch, and its reservation untouched.
	var staleAttemptStatus, successorAttemptStatus peoplesweep.AttemptStatus
	requirements.NoError(f.store.db.QueryRow(`SELECT status FROM person_sweep_attempts
		WHERE id = ?`, f.attemptID).Scan(&staleAttemptStatus))
	requirements.NoError(f.store.db.QueryRow(`SELECT status FROM person_sweep_attempts
		WHERE id = ?`, successorAttempt).Scan(&successorAttemptStatus))
	assert.Equal(peoplesweep.AttemptRunning, staleAttemptStatus)
	assert.Equal(peoplesweep.AttemptSucceeded, successorAttemptStatus)
	var staleBatchStatus, staleBatchRequestID string
	requirements.NoError(f.store.db.QueryRow(`SELECT status, provider_request_id
		FROM person_sweep_batches WHERE attempt_id = ?`, f.attemptID).
		Scan(&staleBatchStatus, &staleBatchRequestID))
	assert.Equal(personSweepBatchStatusRunning, staleBatchStatus)
	assert.Empty(staleBatchRequestID)
	var successorBatchStatus string
	requirements.NoError(f.store.db.QueryRow(`SELECT status FROM person_sweep_batches
		WHERE attempt_id = ?`, successorAttempt).Scan(&successorBatchStatus))
	assert.Equal("succeeded", successorBatchStatus)
	// Daily usage holds the stale attempt's unreleased reservation plus the
	// successor's reconciled actual usage — the fenced finalizer neither
	// cancels its reservation nor double-accounts its completed call.
	var reserved, actual peoplesweep.Usage
	requirements.NoError(f.store.db.QueryRow(`SELECT reserved_requests,
		reserved_input_tokens, reserved_output_tokens, reserved_cost_micro_usd,
		actual_requests, actual_input_tokens, actual_output_tokens,
		actual_cost_micro_usd FROM person_sweep_daily_usage WHERE utc_day = ?`,
		"2026-08-23").Scan(&reserved.Requests, &reserved.InputTokens,
		&reserved.OutputTokens, &reserved.EstimatedCostMicroUSD, &actual.Requests,
		&actual.InputTokens, &actual.OutputTokens, &actual.EstimatedCostMicroUSD))
	assert.Equal(peoplesweep.Usage{Requests: 1, InputTokens: 3,
		OutputTokens: 2, EstimatedCostMicroUSD: 5}, reserved)
	assert.Equal(peoplesweep.Usage{Requests: 1, InputTokens: 3,
		OutputTokens: 2, EstimatedCostMicroUSD: 5}, actual)
	// The successor apply finished the lease lifecycle: no work row survives.
	var workRows int
	requirements.NoError(f.store.db.QueryRow(`SELECT COUNT(*) FROM person_sweep_work
		WHERE person_id = ?`, f.personID).Scan(&workRows))
	assert.Zero(workRows)
}
