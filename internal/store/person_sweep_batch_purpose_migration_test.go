package store_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/peoplesweep"
	"go.kenn.io/msgvault/internal/store"
)

// legacyPersonSweepBatchPurposeCheck is the coupling constraint archives
// carried before the person brief added its own two purposes.
const legacyPersonSweepBatchPurposeCheck = `CHECK (
	(call_ordinal = 0 AND purpose = 'primary') OR
	(call_ordinal = 1 AND purpose = 'repair'))`

// installLegacyPersonSweepBatchPurpose puts the call journal back into its
// pre-brief shape, rows included, so the migration runs against an archive that
// already spent extraction calls.
func installLegacyPersonSweepBatchPurpose(t *testing.T, st *store.Store) {
	t.Helper()
	requirements := require.New(t)
	if st.IsPostgreSQL() {
		_, err := st.DB().ExecContext(t.Context(), `
			ALTER TABLE person_sweep_batches
				DROP CONSTRAINT IF EXISTS person_sweep_batches_call_coordinate_check;
			ALTER TABLE person_sweep_batches
				ADD CONSTRAINT person_sweep_batches_call_coordinate_check `+
			legacyPersonSweepBatchPurposeCheck)
		requirements.NoError(err)
		return
	}
	_, err := st.DB().ExecContext(t.Context(), `
		CREATE TABLE person_sweep_batches_legacy (
			attempt_id TEXT NOT NULL REFERENCES person_sweep_attempts(id) ON DELETE CASCADE,
			batch_ordinal INTEGER NOT NULL CHECK (batch_ordinal >= 0),
			call_ordinal INTEGER NOT NULL DEFAULT 0 CHECK (call_ordinal IN (0, 1)),
			purpose TEXT NOT NULL DEFAULT 'primary',
			utc_day TEXT NOT NULL,
			reservation_id TEXT NOT NULL,
			budget_fingerprint TEXT NOT NULL,
			input_hash TEXT NOT NULL,
			item_count INTEGER NOT NULL CHECK (item_count >= 0),
			status TEXT NOT NULL CHECK (status IN ('reserved', 'running', 'succeeded', 'failed', 'cancelled')),
			provider_request_id TEXT NOT NULL DEFAULT '',
			reserved_requests INTEGER NOT NULL CHECK (reserved_requests >= 0),
			reserved_input_tokens INTEGER NOT NULL CHECK (reserved_input_tokens >= 0),
			reserved_output_tokens INTEGER NOT NULL CHECK (reserved_output_tokens >= 0),
			reserved_cost_micro_usd INTEGER NOT NULL CHECK (reserved_cost_micro_usd >= 0),
			actual_requests INTEGER NOT NULL DEFAULT 0 CHECK (actual_requests >= 0),
			actual_input_tokens INTEGER NOT NULL DEFAULT 0 CHECK (actual_input_tokens >= 0),
			actual_output_tokens INTEGER NOT NULL DEFAULT 0 CHECK (actual_output_tokens >= 0),
			actual_cost_micro_usd INTEGER NOT NULL DEFAULT 0 CHECK (actual_cost_micro_usd >= 0),
			latency_milliseconds INTEGER NOT NULL DEFAULT 0 CHECK (latency_milliseconds >= 0),
			failure_class TEXT NOT NULL DEFAULT '' CHECK (failure_class IN (
				'', 'policy', 'budget', 'lease_lost', 'rate_limited', 'timeout',
				'provider_http', 'invalid_output', 'archive_gap', 'internal'
			)),
			created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
			completed_at TEXT,
			CONSTRAINT person_sweep_batches_call_coordinate_check `+
		legacyPersonSweepBatchPurposeCheck+`,
			PRIMARY KEY (attempt_id, batch_ordinal, call_ordinal)
		);
		INSERT INTO person_sweep_batches_legacy SELECT * FROM person_sweep_batches;
		DROP TABLE person_sweep_batches;
		ALTER TABLE person_sweep_batches_legacy RENAME TO person_sweep_batches`)
	requirements.NoError(err)
}

func TestPersonSweepBatchPurposeMigrationPreservesExtractionRowsAndAdmitsBriefCalls(t *testing.T) {
	requirements := require.New(t)
	checks := assert.New(t)
	f := newPersonSweepBudgetFixture(t, "batch-purpose-migration")
	primaryRequest := sweepReservation(f, 0, 250, "provider-fingerprint", generousSweepBudget())
	primary, err := f.store.ReservePersonSweepBudget(t.Context(), primaryRequest)
	requirements.NoError(err)
	requirements.NoError(f.store.MarkPersonSweepBudgetStarted(t.Context(), primary, sweepAttemptLease(f)))
	repairRequest := primaryRequest
	repairRequest.CallOrdinal = 1
	repairRequest.Purpose = peoplesweep.ProviderCallPurposeRepair
	repairRequest.InputHash = strings.Repeat("d", 64)
	_, err = f.store.ReservePersonSweepBudget(t.Context(), repairRequest)
	requirements.NoError(err)

	installLegacyPersonSweepBatchPurpose(t, f.store)
	_, err = f.store.DB().ExecContext(t.Context(), f.store.Rebind(
		`INSERT INTO person_sweep_batches
			(attempt_id, batch_ordinal, call_ordinal, purpose, utc_day, reservation_id,
			 budget_fingerprint, input_hash, item_count, status,
			 reserved_requests, reserved_input_tokens, reserved_output_tokens,
			 reserved_cost_micro_usd)
		 VALUES (?, 1, 0, 'brief', ?, 'brief-before-migration', 'budget', ?, 0, 'reserved', 1, 1, 1, 2)`),
		f.attemptID, testSweepUTCDate, strings.Repeat("a", 64))
	requirements.Error(err, "the legacy constraint must reject a brief call")

	_, err = f.store.DB().ExecContext(t.Context(), f.store.Rebind(
		`DELETE FROM applied_migrations WHERE name = ?`), "person_sweep_batch_purpose_v2")
	requirements.NoError(err)
	requirements.NoError(f.store.InitSchema())

	var rowCount, repairCount int
	requirements.NoError(f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`
		SELECT COUNT(*), SUM(CASE WHEN purpose = 'repair' THEN 1 ELSE 0 END)
		FROM person_sweep_batches WHERE attempt_id = ?`), f.attemptID).Scan(&rowCount, &repairCount))
	checks.Equal(2, rowCount, "the migration must preserve every extraction call")
	checks.Equal(1, repairCount)

	_, err = f.store.DB().ExecContext(t.Context(), f.store.Rebind(
		`INSERT INTO person_sweep_batches
			(attempt_id, batch_ordinal, call_ordinal, purpose, utc_day, reservation_id,
			 budget_fingerprint, input_hash, item_count, status,
			 reserved_requests, reserved_input_tokens, reserved_output_tokens,
			 reserved_cost_micro_usd)
		 VALUES (?, 1, 0, 'brief', ?, 'brief-after-migration', 'budget', ?, 0, 'reserved', 1, 1, 1, 2)`),
		f.attemptID, testSweepUTCDate, strings.Repeat("a", 64))
	requirements.NoError(err, "the migrated constraint must admit a brief call")
	_, err = f.store.DB().ExecContext(t.Context(), f.store.Rebind(
		`INSERT INTO person_sweep_batches
			(attempt_id, batch_ordinal, call_ordinal, purpose, utc_day, reservation_id,
			 budget_fingerprint, input_hash, item_count, status,
			 reserved_requests, reserved_input_tokens, reserved_output_tokens,
			 reserved_cost_micro_usd)
		 VALUES (?, 2, 0, 'brief_repair', ?, 'misplaced', 'budget', ?, 0, 'reserved', 1, 1, 1, 2)`),
		f.attemptID, testSweepUTCDate, strings.Repeat("b", 64))
	requirements.Error(err, "a brief repair still belongs on call ordinal 1")

	_, err = f.store.DB().ExecContext(t.Context(), f.store.Rebind(
		`DELETE FROM applied_migrations WHERE name = ?`), "person_sweep_batch_purpose_v2")
	requirements.NoError(err)
	requirements.NoError(f.store.InitSchema(), "the migration must be idempotent")
	var finalRows int
	requirements.NoError(f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`
		SELECT COUNT(*) FROM person_sweep_batches WHERE attempt_id = ?`),
		f.attemptID).Scan(&finalRows))
	checks.Equal(3, finalRows)
}

func TestPersonSweepBudgetJournalsBriefCallsAfterExtraction(t *testing.T) {
	requirements := require.New(t)
	checks := assert.New(t)
	f := newPersonSweepBudgetFixture(t, "brief-calls")
	primaryRequest := sweepReservation(f, 0, 250, "provider-fingerprint", generousSweepBudget())
	primary, err := f.store.ReservePersonSweepBudget(t.Context(), primaryRequest)
	requirements.NoError(err)
	requirements.NoError(f.store.MarkPersonSweepBudgetStarted(t.Context(), primary, sweepAttemptLease(f)))

	briefRequest := sweepReservation(f, 1, 250, "provider-fingerprint", generousSweepBudget())
	briefRequest.Purpose = peoplesweep.ProviderCallPurposeBrief
	briefRequest.InputHash = strings.Repeat("1", 64)
	brief, err := f.store.ReservePersonSweepBudget(t.Context(), briefRequest)
	requirements.NoError(err)
	requirements.NoError(f.store.MarkPersonSweepBudgetStarted(t.Context(), brief, sweepAttemptLease(f)))

	briefRepair := briefRequest
	briefRepair.CallOrdinal = 1
	briefRepair.Purpose = peoplesweep.ProviderCallPurposeBriefRepair
	briefRepair.InputHash = strings.Repeat("2", 64)
	_, err = f.store.ReservePersonSweepBudget(t.Context(), briefRepair)
	requirements.NoError(err)

	rows, err := f.store.DB().QueryContext(t.Context(), f.store.Rebind(`
		SELECT batch_ordinal, call_ordinal, purpose
		FROM person_sweep_batches WHERE attempt_id = ?
		ORDER BY batch_ordinal, call_ordinal`), f.attemptID)
	requirements.NoError(err)
	defer func() { requirements.NoError(rows.Close()) }()
	var got []peoplesweep.ProviderCallCoordinate
	for rows.Next() {
		var coordinate peoplesweep.ProviderCallCoordinate
		requirements.NoError(rows.Scan(&coordinate.BatchOrdinal, &coordinate.CallOrdinal,
			&coordinate.Purpose))
		got = append(got, coordinate)
	}
	requirements.NoError(rows.Err())
	checks.Equal([]peoplesweep.ProviderCallCoordinate{
		{BatchOrdinal: 0, CallOrdinal: 0, Purpose: peoplesweep.ProviderCallPurposePrimary},
		{BatchOrdinal: 1, CallOrdinal: 0, Purpose: peoplesweep.ProviderCallPurposeBrief},
		{BatchOrdinal: 1, CallOrdinal: 1, Purpose: peoplesweep.ProviderCallPurposeBriefRepair},
	}, got)
}

func TestPersonSweepBudgetRejectsMisplacedBriefCalls(t *testing.T) {
	t.Run("brief repair paired with an extraction primary", func(t *testing.T) {
		f := newPersonSweepBudgetFixture(t, "brief-repair-on-primary")
		primaryRequest := sweepReservation(f, 0, 250, "provider-fingerprint", generousSweepBudget())
		primary, err := f.store.ReservePersonSweepBudget(t.Context(), primaryRequest)
		require.NoError(t, err)
		require.NoError(t, f.store.MarkPersonSweepBudgetStarted(
			t.Context(), primary, sweepAttemptLease(f)))
		mismatched := primaryRequest
		mismatched.CallOrdinal = 1
		mismatched.Purpose = peoplesweep.ProviderCallPurposeBriefRepair
		mismatched.InputHash = strings.Repeat("3", 64)
		_, err = f.store.ReservePersonSweepBudget(t.Context(), mismatched)
		require.Error(t, err)
	})
	t.Run("extraction batch after a brief batch", func(t *testing.T) {
		f := newPersonSweepBudgetFixture(t, "primary-after-brief")
		primaryRequest := sweepReservation(f, 0, 250, "provider-fingerprint", generousSweepBudget())
		_, err := f.store.ReservePersonSweepBudget(t.Context(), primaryRequest)
		require.NoError(t, err)
		briefRequest := sweepReservation(f, 1, 250, "provider-fingerprint", generousSweepBudget())
		briefRequest.Purpose = peoplesweep.ProviderCallPurposeBrief
		briefRequest.InputHash = strings.Repeat("4", 64)
		_, err = f.store.ReservePersonSweepBudget(t.Context(), briefRequest)
		require.NoError(t, err)
		late := sweepReservation(f, 2, 250, "provider-fingerprint", generousSweepBudget())
		late.InputHash = strings.Repeat("5", 64)
		_, err = f.store.ReservePersonSweepBudget(t.Context(), late)
		require.Error(t, err, "a brief batch closes the attempt's ordinal sequence")
	})
	t.Run("brief on the repair call ordinal", func(t *testing.T) {
		f := newPersonSweepBudgetFixture(t, "brief-on-repair-ordinal")
		request := sweepReservation(f, 0, 250, "provider-fingerprint", generousSweepBudget())
		request.CallOrdinal = 1
		request.Purpose = peoplesweep.ProviderCallPurposeBrief
		_, err := f.store.ReservePersonSweepBudget(t.Context(), request)
		require.Error(t, err)
	})
}
