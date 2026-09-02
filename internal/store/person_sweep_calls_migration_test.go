package store_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/peoplesweep"
)

func TestPersonSweepCallJournalMigrationPreservesLegacyRowsAndIsIdempotent(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newPersonSweepBudgetFixture(t, "calls-migration")
	primaryRequest := sweepReservation(f, 0, 250, "provider-fingerprint", generousSweepBudget())
	primary, err := f.store.ReservePersonSweepBudget(t.Context(), primaryRequest)
	require.NoError(err)
	require.NoError(f.store.MarkPersonSweepBudgetStarted(t.Context(), primary, sweepAttemptLease(f)))

	legacyColumns := `attempt_id, batch_ordinal, utc_day, reservation_id,
		budget_fingerprint, input_hash, item_count, status, provider_request_id,
		reserved_requests, reserved_input_tokens, reserved_output_tokens,
		reserved_cost_micro_usd, actual_requests, actual_input_tokens,
		actual_output_tokens, actual_cost_micro_usd, latency_milliseconds,
		failure_class, created_at, completed_at`
	if f.store.IsPostgreSQL() {
		_, err = f.store.DB().Exec(`CREATE TABLE person_sweep_batches_legacy AS
			SELECT ` + legacyColumns + ` FROM person_sweep_batches;
			DROP TABLE person_sweep_batches;
			ALTER TABLE person_sweep_batches_legacy RENAME TO person_sweep_batches;
			ALTER TABLE person_sweep_batches
				ADD PRIMARY KEY (attempt_id, batch_ordinal)`)
	} else {
		_, err = f.store.DB().Exec(`CREATE TABLE person_sweep_batches_legacy AS
			SELECT ` + legacyColumns + ` FROM person_sweep_batches;
			DROP TABLE person_sweep_batches;
			ALTER TABLE person_sweep_batches_legacy RENAME TO person_sweep_batches`)
	}
	require.NoError(err)
	_, err = f.store.DB().Exec(f.store.Rebind(
		`DELETE FROM applied_migrations WHERE name = ?`), "person_sweep_calls_v2")
	require.NoError(err)
	require.NoError(f.store.InitSchema())

	assert.Contains(liveTableColumns(t, f.store, "person_sweep_batches"), "call_ordinal")
	assert.Contains(liveTableColumns(t, f.store, "person_sweep_batches"), "purpose")
	var rowCount, repairCount int
	var callOrdinal int
	var purpose, reservationID string
	require.NoError(f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`
		SELECT COUNT(*), SUM(CASE WHEN purpose = 'repair' THEN 1 ELSE 0 END),
		       MAX(call_ordinal), MAX(purpose), MAX(reservation_id)
		FROM person_sweep_batches WHERE attempt_id = ?`), f.attemptID).Scan(
		&rowCount, &repairCount, &callOrdinal, &purpose, &reservationID))
	assert.Equal(1, rowCount)
	assert.Zero(repairCount, "migration must not synthesize a repair call")
	assert.Zero(callOrdinal)
	assert.Equal(peoplesweep.ProviderCallPurposePrimary, purpose)
	assert.Equal(primary.ID, reservationID)

	repairRequest := primaryRequest
	repairRequest.CallOrdinal = 1
	repairRequest.Purpose = peoplesweep.ProviderCallPurposeRepair
	repairRequest.InputHash = strings.Repeat("e", 64)
	_, err = f.store.ReservePersonSweepBudget(t.Context(), repairRequest)
	require.NoError(err, "the migrated primary key must admit one same-batch repair")
	_, err = f.store.DB().ExecContext(t.Context(), f.store.Rebind(`
		INSERT INTO person_sweep_batches
			(attempt_id, batch_ordinal, call_ordinal, purpose, utc_day,
			 reservation_id, budget_fingerprint, input_hash, item_count, status,
			 reserved_requests, reserved_input_tokens, reserved_output_tokens,
			 reserved_cost_micro_usd)
		VALUES (?, 1, 1, 'primary', ?, 'invalid-coordinate', ?, ?, 0, 'reserved', 1, 1, 1, 2)`),
		f.attemptID, testSweepUTCDate, "budget", strings.Repeat("f", 64))
	require.Error(err, "the migrated schema must reject mismatched call purpose")

	_, err = f.store.DB().Exec(f.store.Rebind(
		`DELETE FROM applied_migrations WHERE name = ?`), "person_sweep_calls_v2")
	require.NoError(err)
	require.NoError(f.store.InitSchema(), "the migration must be idempotent")
	var finalRows int
	require.NoError(f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`
		SELECT COUNT(*) FROM person_sweep_batches WHERE attempt_id = ?`),
		f.attemptID).Scan(&finalRows))
	assert.Equal(2, finalRows)
}
