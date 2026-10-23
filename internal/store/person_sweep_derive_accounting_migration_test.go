package store_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/peoplesweep"
	"go.kenn.io/msgvault/internal/store"
)

const deriveAccountingMigration = "person_sweep_derive_accounting_v1"

func installLegacyPersonSweepAccounting(t *testing.T, st *store.Store) {
	t.Helper()
	if st.IsPostgreSQL() {
		_, err := st.DB().ExecContext(t.Context(), `ALTER TABLE person_sweep_attempts
    DROP COLUMN IF EXISTS derive_failure_class, DROP COLUMN IF EXISTS memory_epoch,
    DROP COLUMN IF EXISTS memory_sha256, DROP COLUMN IF EXISTS work_list_through,
    DROP COLUMN IF EXISTS notes_applied, DROP COLUMN IF EXISTS notes_rejected;
   ALTER TABLE person_sweep_attempts DROP CONSTRAINT person_sweep_attempts_mode_check;
   ALTER TABLE person_sweep_attempts ADD CONSTRAINT person_sweep_attempts_mode_check CHECK (mode IN ('incremental', 'backstop'));
   ALTER TABLE person_sweep_batches DROP CONSTRAINT person_sweep_batches_call_coordinate_check;
   ALTER TABLE person_sweep_batches ADD CONSTRAINT person_sweep_batches_call_coordinate_check CHECK (
    (call_ordinal = 0 AND purpose IN ('primary', 'brief')) OR
    (call_ordinal = 1 AND purpose IN ('repair', 'brief_repair')));
   DROP INDEX IF EXISTS idx_person_sweep_batches_day_purpose`)
		require.NoError(t, err)
		return
	}
	conn, err := st.DB().Conn(t.Context())
	require.NoError(t, err)
	defer func() { require.NoError(t, conn.Close()) }()
	_, err = conn.ExecContext(t.Context(), `PRAGMA foreign_keys = OFF`)
	require.NoError(t, err)
	defer func() {
		_, err := conn.ExecContext(context.WithoutCancel(t.Context()), `PRAGMA foreign_keys = ON`)
		require.NoError(t, err)
	}()
	tx, err := conn.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	_, err = tx.ExecContext(t.Context(), `CREATE TABLE person_sweep_attempts_legacy (
    id                          TEXT PRIMARY KEY,
    run_id                      TEXT NOT NULL REFERENCES person_sweep_runs(id) ON DELETE CASCADE,
    person_id                   INTEGER NOT NULL REFERENCES persons(id) ON DELETE CASCADE,
    lease_fence                 INTEGER NOT NULL CHECK (lease_fence >= 0),
    mode                        TEXT NOT NULL CHECK (mode IN ('incremental', 'backstop')),
    status                      TEXT NOT NULL CHECK (status IN ('running', 'succeeded', 'failed', 'cancelled')),
    failure_class               TEXT NOT NULL DEFAULT '' CHECK (failure_class IN (
        '', 'policy', 'budget', 'lease_lost', 'rate_limited', 'timeout',
        'provider_http', 'invalid_output', 'archive_gap', 'internal'
    )),
    brief_failure_class         TEXT NOT NULL DEFAULT '' CHECK (brief_failure_class IN (
        '', 'policy', 'budget', 'lease_lost', 'rate_limited', 'timeout',
        'provider_http', 'invalid_output', 'archive_gap', 'internal'
    )),
    cursor_envelope_json        TEXT NOT NULL,
    envelope_hash               TEXT NOT NULL,
    program_fingerprint         TEXT NOT NULL,
    catalog_fingerprint         TEXT NOT NULL,
    provider_fingerprint        TEXT NOT NULL,
    generation_id               INTEGER REFERENCES person_fact_generations(id) ON DELETE SET NULL,
    generation_key              TEXT NOT NULL DEFAULT '',
    seed_count                   INTEGER NOT NULL DEFAULT 0 CHECK (seed_count >= 0),
    context_count                INTEGER NOT NULL DEFAULT 0 CHECK (context_count >= 0),
    claim_count                  INTEGER NOT NULL DEFAULT 0 CHECK (claim_count >= 0),
    decision_count               INTEGER NOT NULL DEFAULT 0 CHECK (decision_count >= 0),
    projected_write_count       INTEGER NOT NULL DEFAULT 0 CHECK (projected_write_count >= 0),
    request_count                INTEGER NOT NULL DEFAULT 0 CHECK (request_count >= 0),
    provider_request_id         TEXT NOT NULL DEFAULT '',
    input_tokens                INTEGER NOT NULL DEFAULT 0 CHECK (input_tokens >= 0),
    output_tokens               INTEGER NOT NULL DEFAULT 0 CHECK (output_tokens >= 0),
    estimated_cost_micro_usd    INTEGER NOT NULL DEFAULT 0 CHECK (estimated_cost_micro_usd >= 0),
    latency_milliseconds        INTEGER NOT NULL DEFAULT 0 CHECK (latency_milliseconds >= 0),
    retry_at                    TEXT,
    started_at                  TEXT NOT NULL,
    completed_at                TEXT
);

INSERT INTO person_sweep_attempts_legacy (id, run_id, person_id, lease_fence, mode, status, failure_class, brief_failure_class, cursor_envelope_json, envelope_hash, program_fingerprint, catalog_fingerprint, provider_fingerprint, generation_id, generation_key, seed_count, context_count, claim_count, decision_count, projected_write_count, request_count, provider_request_id, input_tokens, output_tokens, estimated_cost_micro_usd, latency_milliseconds, retry_at, started_at, completed_at) SELECT id, run_id, person_id, lease_fence, mode, status, failure_class, brief_failure_class, cursor_envelope_json, envelope_hash, program_fingerprint, catalog_fingerprint, provider_fingerprint, generation_id, generation_key, seed_count, context_count, claim_count, decision_count, projected_write_count, request_count, provider_request_id, input_tokens, output_tokens, estimated_cost_micro_usd, latency_milliseconds, retry_at, started_at, completed_at FROM person_sweep_attempts;
DROP TABLE person_sweep_attempts;
ALTER TABLE person_sweep_attempts_legacy RENAME TO person_sweep_attempts;
CREATE INDEX IF NOT EXISTS idx_person_sweep_attempts_person_started
    ON person_sweep_attempts(person_id, started_at DESC);
CREATE INDEX IF NOT EXISTS idx_person_sweep_attempts_run
    ON person_sweep_attempts(run_id, id);
CREATE INDEX IF NOT EXISTS idx_person_sweep_attempts_operations_failure
    ON person_sweep_attempts(
        run_id,
        COALESCE(completed_at, started_at) DESC,
        id COLLATE BINARY DESC
    ) WHERE failure_class <> '';
CREATE INDEX IF NOT EXISTS idx_person_sweep_attempts_generation
    ON person_sweep_attempts(generation_id);

CREATE TABLE person_sweep_batches_legacy (
    attempt_id                  TEXT NOT NULL REFERENCES person_sweep_attempts(id) ON DELETE CASCADE,
    batch_ordinal               INTEGER NOT NULL CHECK (batch_ordinal >= 0),
    call_ordinal                INTEGER NOT NULL DEFAULT 0 CHECK (call_ordinal IN (0, 1)),
    purpose                     TEXT NOT NULL DEFAULT 'primary',
    utc_day                     TEXT NOT NULL,
    reservation_id              TEXT NOT NULL,
    budget_fingerprint          TEXT NOT NULL,
    input_hash                  TEXT NOT NULL,
    item_count                  INTEGER NOT NULL CHECK (item_count >= 0),
    status                      TEXT NOT NULL CHECK (status IN ('reserved', 'running', 'succeeded', 'failed', 'cancelled')),
    provider_request_id         TEXT NOT NULL DEFAULT '',
    reserved_requests           INTEGER NOT NULL CHECK (reserved_requests >= 0),
    reserved_input_tokens       INTEGER NOT NULL CHECK (reserved_input_tokens >= 0),
    reserved_output_tokens      INTEGER NOT NULL CHECK (reserved_output_tokens >= 0),
    reserved_cost_micro_usd     INTEGER NOT NULL CHECK (reserved_cost_micro_usd >= 0),
    actual_requests             INTEGER NOT NULL DEFAULT 0 CHECK (actual_requests >= 0),
    actual_input_tokens         INTEGER NOT NULL DEFAULT 0 CHECK (actual_input_tokens >= 0),
    actual_output_tokens        INTEGER NOT NULL DEFAULT 0 CHECK (actual_output_tokens >= 0),
    actual_cost_micro_usd       INTEGER NOT NULL DEFAULT 0 CHECK (actual_cost_micro_usd >= 0),
    latency_milliseconds        INTEGER NOT NULL DEFAULT 0 CHECK (latency_milliseconds >= 0),
    failure_class               TEXT NOT NULL DEFAULT '' CHECK (failure_class IN (
        '', 'policy', 'budget', 'lease_lost', 'rate_limited', 'timeout',
        'provider_http', 'invalid_output', 'archive_gap', 'internal'
    )),
    created_at                  TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
    completed_at                TEXT,
    CONSTRAINT person_sweep_batches_call_coordinate_check CHECK (
        (call_ordinal = 0 AND purpose IN ('primary', 'brief')) OR
        (call_ordinal = 1 AND purpose IN ('repair', 'brief_repair'))
    ),
    PRIMARY KEY (attempt_id, batch_ordinal, call_ordinal)
);

INSERT INTO person_sweep_batches_legacy (attempt_id, batch_ordinal, call_ordinal, purpose, utc_day, reservation_id, budget_fingerprint, input_hash, item_count, status, provider_request_id, reserved_requests, reserved_input_tokens, reserved_output_tokens, reserved_cost_micro_usd, actual_requests, actual_input_tokens, actual_output_tokens, actual_cost_micro_usd, latency_milliseconds, failure_class, created_at, completed_at) SELECT attempt_id, batch_ordinal, call_ordinal, purpose, utc_day, reservation_id, budget_fingerprint, input_hash, item_count, status, provider_request_id, reserved_requests, reserved_input_tokens, reserved_output_tokens, reserved_cost_micro_usd, actual_requests, actual_input_tokens, actual_output_tokens, actual_cost_micro_usd, latency_milliseconds, failure_class, created_at, completed_at FROM person_sweep_batches;
DROP TABLE person_sweep_batches;
ALTER TABLE person_sweep_batches_legacy RENAME TO person_sweep_batches;
DROP INDEX IF EXISTS idx_person_sweep_batches_day_purpose;`)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
}

func seedDeriveAccountingCalls(t *testing.T) personSweepBudgetFixture {
	t.Helper()
	f := newPersonSweepBudgetFixture(t, "derive-accounting")
	request := sweepReservation(f, 0, 250, "provider-fingerprint", generousSweepBudget())
	primary, err := f.store.ReservePersonSweepBudget(t.Context(), request)
	require.NoError(t, err)
	require.NoError(t, f.store.MarkPersonSweepBudgetStarted(t.Context(), primary, sweepAttemptLease(f)))
	request.CallOrdinal = 1
	request.Purpose = peoplesweep.ProviderCallPurposeRepair
	request.InputHash = strings.Repeat("d", 64)
	_, err = f.store.ReservePersonSweepBudget(t.Context(), request)
	require.NoError(t, err)
	_, err = f.store.ReservePersonSweepBudget(t.Context(), sweepReservation(f, 1, 250, "provider-fingerprint", generousSweepBudget()))
	require.NoError(t, err)
	return f
}

func forgetDeriveAccountingMigration(t *testing.T, st *store.Store) {
	t.Helper()
	_, err := st.DB().ExecContext(t.Context(), st.Rebind(`DELETE FROM applied_migrations WHERE name = ?`), deriveAccountingMigration)
	require.NoError(t, err)
}

func checkDeriveAccounting(t *testing.T, f personSweepBudgetFixture) {
	t.Helper()
	st := f.store
	rows, err := st.DB().QueryContext(t.Context(), st.Rebind(`SELECT status FROM person_sweep_batches WHERE attempt_id = ? ORDER BY batch_ordinal,call_ordinal`), f.attemptID)
	require.NoError(t, err)
	defer func() { require.NoError(t, rows.Close()) }()
	var statuses []string
	for rows.Next() {
		var status string
		require.NoError(t, rows.Scan(&status))
		statuses = append(statuses, status)
	}
	require.NoError(t, rows.Err())
	assert.Equal(t, []string{"running", "reserved", "reserved"}, statuses)
	var status, failure, epoch, hash string
	var through, applied, rejected int
	require.NoError(t, st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT status,derive_failure_class,memory_epoch,memory_sha256,work_list_through,notes_applied,notes_rejected FROM person_sweep_attempts WHERE id = ?`), f.attemptID).Scan(&status, &failure, &epoch, &hash, &through, &applied, &rejected))
	assert.Equal(t, "running", status)
	assert.Empty(t, failure)
	assert.Empty(t, epoch)
	assert.Empty(t, hash)
	assert.Zero(t, through)
	assert.Zero(t, applied)
	assert.Zero(t, rejected)
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE person_sweep_attempts SET mode='derive' WHERE id=?`), f.attemptID)
	require.NoError(t, err)
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE person_sweep_attempts SET mode='card' WHERE id=?`), f.attemptID)
	require.Error(t, err)
	for i, tc := range []struct {
		purpose string
		call    int
		valid   bool
	}{
		{"observation", 0, true}, {"derive_deductive", 0, true}, {"derive_inductive", 0, true},
		{"observation_repair", 1, true}, {"derive_repair", 1, true}, {"derive_repair", 0, false}, {"card", 0, false},
	} {
		_, err := st.DB().ExecContext(t.Context(), st.Rebind(`INSERT INTO person_sweep_batches
   (attempt_id,batch_ordinal,call_ordinal,purpose,utc_day,reservation_id,budget_fingerprint,input_hash,item_count,status,reserved_requests,reserved_input_tokens,reserved_output_tokens,reserved_cost_micro_usd)
   VALUES (?,?,?,?,?,'test','budget','hash',0,'reserved',1,1,1,1)`), f.attemptID, i+2, tc.call, tc.purpose, testSweepUTCDate)
		if tc.valid {
			require.NoError(t, err, tc.purpose)
		} else {
			require.Error(t, err, tc.purpose)
		}
	}
	var count int
	query := `SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name='idx_person_sweep_batches_day_purpose'`
	if st.IsPostgreSQL() {
		query = `SELECT COUNT(*) FROM pg_indexes WHERE schemaname=current_schema() AND indexname='idx_person_sweep_batches_day_purpose'`
	}
	require.NoError(t, st.DB().QueryRowContext(t.Context(), query).Scan(&count))
	assert.Equal(t, 1, count)
	version, err := st.SchemaVersionContext(t.Context())
	require.NoError(t, err)
	assert.Equal(t, store.SchemaVersion, version)
	if !st.IsPostgreSQL() {
		require.NoError(t, st.DB().QueryRowContext(t.Context(), `PRAGMA foreign_keys`).Scan(&count))
		assert.Equal(t, 1, count)
	}
	forgetDeriveAccountingMigration(t, st)
	require.NoError(t, st.InitSchema())
	require.NoError(t, st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT COUNT(*) FROM person_sweep_batches WHERE attempt_id=?`), f.attemptID).Scan(&count))
	assert.Equal(t, 8, count)
}

func TestPersonSweepDeriveAccountingMigrationPreservesCalls(t *testing.T) {
	f := seedDeriveAccountingCalls(t)
	installLegacyPersonSweepAccounting(t, f.store)
	forgetDeriveAccountingMigration(t, f.store)
	require.NoError(t, f.store.InitSchema())
	checkDeriveAccounting(t, f)
}

func TestPersonSweepDeriveAccountingMigrationResumesAfterInterruption(t *testing.T) {
	for _, stage := range []string{"attempts", "batches", "index"} {
		t.Run(stage, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			f := seedDeriveAccountingCalls(t)
			installLegacyPersonSweepAccounting(t, f.store)
			forgetDeriveAccountingMigration(t, f.store)
			failure := errors.New("migration interrupted")
			resetHook := f.store.SetPersonSweepDeriveAccountingHookForTest(func(got string) error {
				if got == stage {
					return failure
				}
				return nil
			})
			require.ErrorIs(f.store.InitSchema(), failure)
			var count int
			require.NoError(f.store.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM applied_migrations WHERE name='person_sweep_derive_accounting_v1'`).Scan(&count))
			assert.Zero(count)
			_, err := f.store.DB().ExecContext(t.Context(), `UPDATE person_sweep_attempts SET mode='derive'`)
			require.Error(err)
			require.NoError(f.store.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM person_sweep_batches`).Scan(&count))
			assert.Equal(3, count)
			resetHook()
			require.NoError(f.store.InitSchema())
			checkDeriveAccounting(t, f)
		})
	}
}

func TestPersonSweepDeriveAccountingMigrationFreshArchiveIsNoop(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := seedDeriveAccountingCalls(t)
	var count int
	require.NoError(f.store.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM applied_migrations WHERE name='person_sweep_derive_accounting_v1'`).Scan(&count))
	assert.Equal(1, count)
	checkDeriveAccounting(t, f)
}

// Retaining inbound references catches accidental cascade deletes during rebuilds.
func TestPersonSweepDeriveAccountingMigrationPreservesReferences(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := seedDeriveAccountingCalls(t)
	_, err := f.store.DB().ExecContext(t.Context(), `CREATE TABLE sweep_accounting_reference (attempt_id TEXT NOT NULL REFERENCES person_sweep_attempts(id) ON DELETE CASCADE)`)
	require.NoError(err)
	_, err = f.store.DB().ExecContext(t.Context(), f.store.Rebind(`INSERT INTO sweep_accounting_reference VALUES (?)`), f.attemptID)
	require.NoError(err)
	installLegacyPersonSweepAccounting(t, f.store)
	forgetDeriveAccountingMigration(t, f.store)
	require.NoError(f.store.InitSchema())
	var id string
	require.NoError(f.store.DB().QueryRowContext(t.Context(), `SELECT attempt_id FROM sweep_accounting_reference`).Scan(&id))
	assert.Equal(f.attemptID, id)
}
