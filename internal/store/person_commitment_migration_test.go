package store_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/peoplesweep"
	"go.kenn.io/msgvault/internal/store"
)

// The same production upgrade runs on SQLite and the PostgreSQL CI backend.
// Restore the v43 ledger vocabularies and retain an existing attempt, claim,
// and evidence link across initialization.
func TestCommitmentMigrationUpgradesVersion43(t *testing.T) {
	r, a, ctx := require.New(t), assert.New(t), t.Context()
	f := newPersonSweepBudgetFixture(t, "commitment-v43")
	st := f.store
	seedPersonFactClaimLedger(t, st, f.personID, "before-commitment")
	installPersonFactClaimOrigin(t, st, `CHECK (origin IN ('extraction', 'enrichment', 'brief', 'system', 'invalid'))`)
	installPersonSweepBatchPurpose(t, st, `CHECK ((call_ordinal = 0 AND purpose IN ('primary', 'brief')) OR (call_ordinal = 1 AND purpose IN ('repair', 'brief_repair')))`)
	for _, query := range []string{
		`DELETE FROM applied_migrations WHERE name = 'person_commitments_v1'`,
		`UPDATE archive_metadata SET value = '43' WHERE key = 'schema_version'`,
	} {
		_, err := st.DB().ExecContext(ctx, query)
		r.NoError(err)
	}
	version, err := st.SchemaVersionContext(ctx)
	r.NoError(err)
	a.Equal(43, version)
	for range 2 {
		r.NoError(st.InitSchemaContext(ctx))
		version, err = st.SchemaVersionContext(ctx)
		r.NoError(err)
		a.Equal(store.SchemaVersion, version)
		var claims, links, attempts int
		r.NoError(st.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM person_fact_claims`).Scan(&claims))
		r.NoError(st.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM person_fact_claim_evidence`).Scan(&links))
		r.NoError(st.DB().QueryRowContext(ctx, st.Rebind(`SELECT COUNT(*) FROM person_sweep_attempts WHERE id = ?`), f.attemptID).Scan(&attempts))
		a.Equal(1, claims)
		a.Equal(1, links)
		a.Equal(1, attempts)
	}
	_, err = st.DB().ExecContext(ctx, st.Rebind(`INSERT INTO person_fact_claims
 (person_id,generation_id,claim_key,target_kind,target_key,target_revision,relation,submitted_value_json,origin,confidence_json)
 SELECT ?,MIN(id),'upgraded-commitment','commitment','promise','rev-1','support','"Send report"','commitment','{}'
 FROM person_fact_generations WHERE person_id = ?`), f.personID, f.personID)
	r.NoError(err)
	for ordinal, purpose := range []string{peoplesweep.ProviderCallPurposeCommitment, peoplesweep.ProviderCallPurposeCommitmentRepair} {
		request := sweepReservation(f, 0, 250, "provider-fingerprint", generousSweepBudget())
		request.CallOrdinal, request.Purpose = ordinal, purpose
		reservation, reserveErr := st.ReservePersonSweepBudget(ctx, request)
		r.NoError(reserveErr)
		if ordinal == 0 {
			r.NoError(st.MarkPersonSweepBudgetStarted(ctx, reservation, sweepAttemptLease(f)))
		}
	}
}
