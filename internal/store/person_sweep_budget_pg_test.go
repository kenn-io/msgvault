package store_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/peoplesweep"
)

// TestPersonSweepPostgreSQLMarkStartedSurvivesReclaimLockCycle regresses the
// lock-cycle deadlock between MarkPersonSweepBudgetStarted and
// ClaimPersonSweep's reclaim. The mark locks attempt -> run -> daily usage ->
// batch -> work, while the reclaim locks work -> batch -> daily usage ->
// attempt, so on PostgreSQL the two transactions can abort each other with
// 40P01. The mark must retry from a fresh transaction and converge either to
// success or to the typed peoplesweep.ErrLeaseLost.
func TestPersonSweepPostgreSQLMarkStartedSurvivesReclaimLockCycle(t *testing.T) {
	t.Run("deadlock victim marks the batch running from a fresh transaction", func(t *testing.T) {
		checks := assert.New(t)
		requirements := require.New(t)
		f := newPersonSweepBudgetFixture(t, "pg-deadlock-retry")
		if !f.store.IsPostgreSQL() {
			t.Skip("PostgreSQL-only mark/reclaim deadlock regression")
		}
		reservation, err := f.store.ReservePersonSweepBudget(t.Context(),
			sweepReservation(f, 0, 100, "provider-fingerprint", generousSweepBudget()))
		requirements.NoError(err)

		writeErr := forcePersonSweepReclaimLockCycle(t.Context(), t, f, func() error {
			return f.store.MarkPersonSweepBudgetStarted(t.Context(), reservation,
				sweepAttemptLease(f))
		})
		requirements.NoError(writeErr,
			"the mark must retry the 40P01 deadlock instead of surfacing it")

		var status string
		requirements.NoError(f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`
			SELECT status FROM person_sweep_batches
			WHERE attempt_id = ? AND batch_ordinal = 0 AND call_ordinal = 0`),
			f.attemptID).Scan(&status))
		checks.Equal("running", status)
	})

	t.Run("reclaim that wins the cycle converges to the typed lease-lost error", func(t *testing.T) {
		checks := assert.New(t)
		requirements := require.New(t)
		f := newPersonSweepBudgetFixture(t, "pg-deadlock-reclaim")
		if !f.store.IsPostgreSQL() {
			t.Skip("PostgreSQL-only mark/reclaim deadlock regression")
		}
		reservation, err := f.store.ReservePersonSweepBudget(t.Context(),
			sweepReservation(f, 0, 100, "provider-fingerprint", generousSweepBudget()))
		requirements.NoError(err)
		staleLease := sweepAttemptLease(f)
		// Expire the lease so a concurrent claim reclaims this attempt while
		// the stale worker is still marking its reserved batch started.
		_, err = f.store.DB().ExecContext(t.Context(), f.store.Rebind(`
			UPDATE person_sweep_work SET lease_until = ? WHERE person_id = ?`),
			time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC), staleLease.PersonID)
		requirements.NoError(err)

		// Park the real reclaim inside ClaimPersonSweep between its work-row
		// lock and its attempt/batch/daily finalization: a BEFORE UPDATE
		// trigger on the first abandoned-batch update waits on an advisory
		// lock this test holds.
		const advisoryKey int64 = 725043
		holder, err := f.store.DB().Conn(t.Context())
		requirements.NoError(err)
		claimStarted := false
		markStarted := false
		claimFinished := make(chan struct{})
		markFinished := make(chan struct{})
		t.Cleanup(func() {
			_, _ = holder.ExecContext(context.Background(),
				`SELECT pg_advisory_unlock($1)`, advisoryKey)
			if claimStarted {
				waitForPostgreSQLTestGoroutine(t, claimFinished)
			}
			if markStarted {
				waitForPostgreSQLTestGoroutine(t, markFinished)
			}
			_, _ = f.store.DB().ExecContext(context.Background(),
				`DROP TRIGGER IF EXISTS park_reclaimed_batch_update ON person_sweep_batches`)
			_, _ = f.store.DB().ExecContext(context.Background(),
				`DROP FUNCTION IF EXISTS park_reclaimed_batch_update()`)
			_ = holder.Close()
		})
		_, err = holder.ExecContext(t.Context(), `SELECT pg_advisory_lock($1)`, advisoryKey)
		requirements.NoError(err)
		_, err = f.store.DB().ExecContext(t.Context(), fmt.Sprintf(`
			CREATE FUNCTION park_reclaimed_batch_update() RETURNS trigger AS $$
			BEGIN
				-- Keep the reclaim from choosing itself as the victim, including
				-- if the mark retries and forms another cycle. This transaction's
				-- detector is deferred beyond the test's bounded wait; the mark's
				-- detector still breaks each cycle normally.
				PERFORM set_config('deadlock_timeout', '1h', true);
				PERFORM pg_advisory_xact_lock(%d);
				RETURN NEW;
			END;
			$$ LANGUAGE plpgsql`, advisoryKey))
		requirements.NoError(err)
		_, err = f.store.DB().ExecContext(t.Context(), `
			CREATE TRIGGER park_reclaimed_batch_update
			BEFORE UPDATE ON person_sweep_batches
			FOR EACH ROW EXECUTE FUNCTION park_reclaimed_batch_update()`)
		requirements.NoError(err)

		waitingBefore := postgreSQLWaitingLockCount(t, f.store)
		claimDone := make(chan *peoplesweep.Lease, 1)
		claimErrs := make(chan error, 1)
		claimStarted = true
		go func() {
			defer close(claimFinished)
			lease, claimErr := f.store.ClaimPersonSweep(t.Context(), peoplesweep.ClaimRequest{
				WorkerID: "reclaim-worker", LeaseDuration: time.Minute,
			})
			claimDone <- lease
			claimErrs <- claimErr
		}()
		requirements.Eventually(func() bool {
			return postgreSQLWaitingLockCount(t, f.store) >= waitingBefore+1
		}, 5*time.Second, 10*time.Millisecond,
			"the reclaim did not park on the batch update trigger")

		markDone := make(chan error, 1)
		markStarted = true
		go func() {
			defer close(markFinished)
			markDone <- f.store.MarkPersonSweepBudgetStarted(t.Context(),
				reservation, staleLease)
		}()
		requirements.Eventually(func() bool {
			return postgreSQLWaitingLockCount(t, f.store) >= waitingBefore+2
		}, 5*time.Second, 10*time.Millisecond,
			"the mark did not park behind the reclaim's batch row lock")
		_, err = holder.ExecContext(t.Context(), `SELECT pg_advisory_unlock($1)`, advisoryKey)
		requirements.NoError(err)

		var markErr error
		select {
		case markErr = <-markDone:
		case <-time.After(15 * time.Second):
			requirements.FailNow("the mark did not finish after the deadlock detector")
		}
		claimErr := <-claimErrs
		reclaimed := <-claimDone
		requirements.NoError(claimErr, "the reclaim must win the lock cycle")
		requirements.NotNil(reclaimed)
		checks.Equal(f.personID, reclaimed.PersonID)
		requirements.ErrorIs(markErr, peoplesweep.ErrLeaseLost,
			"the retried mark must converge to the typed error after the reclaim commits")

		var attemptStatus, attemptClass, batchStatus, workOwner string
		var workFence int64
		requirements.NoError(f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`
			SELECT status, failure_class FROM person_sweep_attempts WHERE id = ?`),
			f.attemptID).Scan(&attemptStatus, &attemptClass))
		requirements.NoError(f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`
			SELECT status FROM person_sweep_batches
			WHERE attempt_id = ? AND batch_ordinal = 0 AND call_ordinal = 0`),
			f.attemptID).Scan(&batchStatus))
		requirements.NoError(f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`
			SELECT lease_owner, lease_fence FROM person_sweep_work WHERE person_id = ?`),
			f.personID).Scan(&workOwner, &workFence))
		checks.Equal("failed", attemptStatus)
		checks.Equal(string(peoplesweep.FailureLeaseLost), attemptClass)
		checks.Equal("cancelled", batchStatus,
			"the stale worker must not start a batch the reclaim cancelled")
		checks.Equal("reclaim-worker", workOwner)
		checks.Equal(int64(2), workFence)
	})
}

// forcePersonSweepReclaimLockCycle closes the lock cycle a ClaimPersonSweep
// reclaim forms against a concurrent mark. A blocker transaction takes the
// person_sweep_work row first, exactly like the claim's fenced lease transfer;
// once the mark parks behind that row while holding the attempt row, the
// blocker asks for the attempt row, so PostgreSQL's deadlock detector has to
// abort one side. The mark is parked well before the blocker closes the cycle,
// so its deadlock timer expires first and it, not the blocker, is the victim.
// The blocker releases cleanly; the mark's own outcome is returned for the
// caller to judge.
func forcePersonSweepReclaimLockCycle(
	ctx context.Context, t *testing.T, f personSweepBudgetFixture, mark func() error,
) error {
	t.Helper()
	requirements := require.New(t)
	blocker, err := f.store.DB().BeginTx(ctx, nil)
	requirements.NoError(err)
	t.Cleanup(func() { _ = blocker.Rollback() })
	_, err = blocker.ExecContext(ctx, `SET LOCAL deadlock_timeout = '1h'`)
	requirements.NoError(err)
	var lockedPerson int64
	requirements.NoError(blocker.QueryRowContext(ctx,
		`SELECT person_id FROM person_sweep_work WHERE person_id = $1 FOR UPDATE`,
		f.personID).Scan(&lockedPerson))
	requirements.Equal(f.personID, lockedPerson)

	markDone := make(chan error, 1)
	go func() { markDone <- mark() }()
	requirements.Eventually(func() bool {
		return postgreSQLWaitingLockCount(t, f.store) >= 1
	}, 5*time.Second, 10*time.Millisecond, "the mark did not park behind the work row lock")

	blockerDone := make(chan error, 1)
	go func() {
		var lockedAttempt string
		lockErr := blocker.QueryRowContext(ctx,
			`SELECT id FROM person_sweep_attempts WHERE id = $1 FOR UPDATE`,
			f.attemptID).Scan(&lockedAttempt)
		if lockErr == nil {
			lockErr = blocker.Commit()
		} else {
			_ = blocker.Rollback()
		}
		blockerDone <- lockErr
	}()

	var markErr error
	select {
	case markErr = <-markDone:
	case <-ctx.Done():
		requirements.FailNow("the mark did not finish after the deadlock detector", ctx.Err())
	}
	select {
	case err := <-blockerDone:
		requirements.NoError(err, "the blocker must release its row locks")
	case <-ctx.Done():
		requirements.FailNow("the blocker did not finish after the deadlock detector", ctx.Err())
	}
	return markErr
}
