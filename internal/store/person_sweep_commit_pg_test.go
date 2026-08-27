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
			BatchOrdinal: 0, PersonID: f.personID,
			ProviderFingerprint: f.request.Generation.Policy.ProviderPolicyFingerprint,
			UTCDate:             "2026-08-23", InputHash: "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd",
			ItemCount: 1, EstimatedRequests: 1, EstimatedInputTokens: 3,
			EstimatedOutputTokens: 2, EstimatedCostMicroUSD: 5, Budget: personSweepApplyBudget()})
	requirements.NoError(err)
	requirements.NoError(f.store.MarkPersonSweepBudgetStarted(t.Context(), successorReservation))
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
	errs := make(chan error, 2)
	go func() {
		<-start
		errs <- f.store.FinalizePersonSweepFailure(ctx, peoplesweep.FailureFinalization{
			Lease: f.lease, AttemptID: f.attemptID, Class: peoplesweep.FailureTimeout,
			RetryAt:      personFactLedgerNow.Add(time.Hour),
			Reservations: []peoplesweep.BudgetReservation{f.reservation},
			Completed: []peoplesweep.CompletedUsage{{BatchOrdinal: 0,
				ProviderRequestID: "request-old-finalizer",
				Usage:             peoplesweep.TokenUsage{InputTokens: 2, OutputTokens: 1}, Latency: time.Second}},
			FinalizedAt: personFactLedgerNow.Add(time.Minute),
		})
	}()
	go func() {
		<-start
		_, applyErr := f.store.ApplyPersonSweep(ctx, successorRequest)
		errs <- applyErr
	}()
	close(start)
	for range 2 {
		requirements.NoError(<-errs)
	}
}
