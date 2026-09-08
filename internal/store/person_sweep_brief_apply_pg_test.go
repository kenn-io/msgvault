package store

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/peoplesweep"
)

// TestPostgreSQLPersonFactClaimOriginConstraintStaysClosedAfterWidening pins
// the PostgreSQL half of the origin migration: the constraint is dropped and
// recreated by name, so an archive that carried the auto-named column check
// ends up with exactly one named constraint that still rejects an unknown
// origin.
func TestPostgreSQLPersonFactClaimOriginConstraintStaysClosedAfterWidening(t *testing.T) {
	skipUnlessPostgresInternal(t)
	checks := assert.New(t)
	requirements := require.New(t)
	f := newPersonSweepApplyFixture(t, "pg-origin-constraint", true)

	var constraints int
	requirements.NoError(f.store.db.QueryRowContext(t.Context(), `
		SELECT COUNT(*) FROM pg_constraint
		WHERE conname = 'person_fact_claims_origin_check'
		  AND conrelid = 'person_fact_claims'::regclass`).Scan(&constraints))
	checks.Equal(1, constraints, "the widened check keeps one named constraint")

	_, err := f.store.db.ExecContext(t.Context(), f.store.Rebind(`
		INSERT INTO person_fact_claims
			(person_id, generation_id, claim_key, target_kind, target_key, target_revision,
			 relation, submitted_value_json, origin, confidence_json)
		SELECT ?, MIN(id), 'pg-unknown-origin', 'attribute', 'city', 'rev-1', 'support',
		       '"Riverton"', 'not-an-origin', '{}'
		FROM person_fact_generations WHERE person_id = ?`), f.personID, f.personID)
	requirements.Error(err, "the widened vocabulary is still closed")
}

// TestPostgreSQLEnsurePersonSweepWorkPublishesOneRowUnderConcurrency pins the
// upsert's row-level behavior on the backend where two publishers can reach the
// same person_sweep_work row at once. SQLite serializes writers, so only
// PostgreSQL can show that the ON CONFLICT path, not a read-then-write race,
// is what keeps the row single.
func TestPostgreSQLEnsurePersonSweepWorkPublishesOneRowUnderConcurrency(t *testing.T) {
	skipUnlessPostgresInternal(t)
	checks := assert.New(t)
	requirements := require.New(t)
	f := newPersonSweepApplyFixture(t, "pg-ensure-work", true)
	_, err := f.store.db.ExecContext(t.Context(), f.store.Rebind(
		`DELETE FROM person_sweep_work WHERE person_id = ?`), f.personID)
	requirements.NoError(err)

	start := make(chan struct{})
	errs := make(chan error, 2)
	var ready sync.WaitGroup
	ready.Add(2)
	for range 2 {
		go func() {
			ready.Done()
			<-start
			_, publishErr := f.store.EnsurePersonSweepWork(t.Context(), f.personID, true)
			errs <- publishErr
		}()
	}
	ready.Wait()
	close(start)
	requirements.NoError(<-errs)
	requirements.NoError(<-errs)

	var rows int
	requirements.NoError(f.store.db.QueryRowContext(t.Context(), f.store.Rebind(
		`SELECT COUNT(*) FROM person_sweep_work WHERE person_id = ?`), f.personID).Scan(&rows))
	checks.Equal(1, rows, "concurrent publication converges on one work row")
}

// TestPostgreSQLApplyPersonSweepReconcilesAFailedBriefReservation pins the
// brief's failure accounting on PostgreSQL, where the batch row is locked with
// FOR UPDATE inside the apply transaction.
func TestPostgreSQLApplyPersonSweepReconcilesAFailedBriefReservation(t *testing.T) {
	skipUnlessPostgresInternal(t)
	checks := assert.New(t)
	requirements := require.New(t)
	f := newPersonSweepApplyFixture(t, "pg-brief-failure", true)
	_ = reserveBriefCall(t, f, strings.Repeat("9", 64))
	f.request.BriefFailureClass = peoplesweep.FailureProviderHTTP
	f.request.BriefRetryAt = f.request.CompletedAt.Add(time.Hour)

	_, err := f.store.ApplyPersonSweep(t.Context(), f.request)
	requirements.NoError(err)

	var status, failureClass string
	requirements.NoError(f.store.db.QueryRowContext(t.Context(), f.store.Rebind(`
		SELECT status, failure_class FROM person_sweep_batches
		WHERE attempt_id = ? AND batch_ordinal = 1 AND call_ordinal = 0`),
		f.attemptID).Scan(&status, &failureClass))
	checks.Equal("failed", status)
	checks.Equal(string(peoplesweep.FailureProviderHTTP), failureClass)

	var attemptBriefClass string
	requirements.NoError(f.store.db.QueryRowContext(t.Context(), f.store.Rebind(
		`SELECT brief_failure_class FROM person_sweep_attempts WHERE id = ?`),
		f.attemptID).Scan(&attemptBriefClass))
	checks.Equal(string(peoplesweep.FailureProviderHTTP), attemptBriefClass)
}
