package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

const personSweepAttemptModeCheck = `CHECK (mode IN ('incremental', 'backstop', 'derive'))`
const personSweepAttemptDeriveFailureClassCheck = `CHECK (derive_failure_class IN (
        '', 'policy', 'budget', 'lease_lost', 'rate_limited', 'timeout',
        'provider_http', 'invalid_output', 'archive_gap', 'internal'
    ))`
const personSweepBatchPurposeCheckV3 = `CHECK (
        (call_ordinal = 0 AND purpose IN ('primary', 'brief', 'observation', 'derive_deductive', 'derive_inductive')) OR
        (call_ordinal = 1 AND purpose IN ('repair', 'brief_repair', 'observation_repair', 'derive_repair'))
    )`

// migratePersonSweepDeriveAccountingV1 widens the accounting vocabulary without
// enabling any new sweep program. Both table changes commit together.
func (s *Store) migratePersonSweepDeriveAccountingV1(ctx context.Context) (err error) {
	if s.IsPostgreSQL() {
		return s.runMaintenance(ctx, func(ctx context.Context, tx *loggedTx) error {
			return s.migratePersonSweepDeriveAccountingTx(ctx, tx)
		})
	}
	// DROP TABLE invokes cascades even during a rebuild. Disable enforcement on
	// one pinned connection before beginning the transaction, then check all
	// inbound references before committing and restore enforcement before reuse.
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("acquire sweep accounting migration connection: %w", err)
	}
	defer func() { _ = conn.Close() }()
	if _, err = conn.ExecContext(ctx, `PRAGMA foreign_keys = OFF`); err != nil {
		return fmt.Errorf("suspend sweep accounting foreign keys: %w", err)
	}
	defer func() {
		if _, restoreErr := conn.ExecContext(context.WithoutCancel(ctx), `PRAGMA foreign_keys = ON`); restoreErr != nil {
			err = errors.Join(err, fmt.Errorf("restore sweep accounting foreign keys: %w", restoreErr))
		}
	}()
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin sweep accounting migration: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := s.migratePersonSweepDeriveAccountingTx(ctx, &loggedTx{Tx: tx, rebind: s.Rebind}); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit sweep accounting migration: %w", err)
	}
	return nil
}

func (s *Store) migratePersonSweepDeriveAccountingTx(ctx context.Context, tx *loggedTx) error {
	if err := validatePersonSweepDeriveAccountingRows(ctx, tx); err != nil {
		return err
	}
	if s.IsPostgreSQL() {
		if _, err := tx.ExecContext(ctx, `ALTER TABLE person_sweep_attempts
   ADD COLUMN IF NOT EXISTS derive_failure_class TEXT NOT NULL DEFAULT '',
   ADD COLUMN IF NOT EXISTS memory_epoch TEXT NOT NULL DEFAULT '',
   ADD COLUMN IF NOT EXISTS memory_sha256 TEXT NOT NULL DEFAULT '',
   ADD COLUMN IF NOT EXISTS work_list_through BIGINT NOT NULL DEFAULT 0 CHECK (work_list_through >= 0),
   ADD COLUMN IF NOT EXISTS notes_applied INTEGER NOT NULL DEFAULT 0 CHECK (notes_applied >= 0),
   ADD COLUMN IF NOT EXISTS notes_rejected INTEGER NOT NULL DEFAULT 0 CHECK (notes_rejected >= 0)`); err != nil {
			return fmt.Errorf("add sweep derive accounting columns: %w", err)
		}
		for _, c := range []struct{ name, check string }{
			{"person_sweep_attempts_mode_check", personSweepAttemptModeCheck},
			{"person_sweep_attempts_derive_failure_class_check", personSweepAttemptDeriveFailureClassCheck},
		} {
			if _, err := tx.ExecContext(ctx, `ALTER TABLE person_sweep_attempts DROP CONSTRAINT IF EXISTS `+c.name); err != nil {
				return fmt.Errorf("drop sweep attempt constraint %s: %w", c.name, err)
			}
			if _, err := tx.ExecContext(ctx, `ALTER TABLE person_sweep_attempts ADD CONSTRAINT `+c.name+` `+c.check); err != nil {
				return fmt.Errorf("add sweep attempt constraint %s: %w", c.name, err)
			}
		}
	} else if err := rebuildPersonSweepAccountingTable(ctx, tx, "person_sweep_attempts", "'derive'", personSweepDeriveAttemptRebuildStatements()); err != nil {
		return err
	}
	if err := s.personSweepDeriveAccountingStage("attempts"); err != nil {
		return err
	}
	if s.IsPostgreSQL() {
		if _, err := tx.ExecContext(ctx, `ALTER TABLE person_sweep_batches DROP CONSTRAINT IF EXISTS person_sweep_batches_call_coordinate_check`); err != nil {
			return fmt.Errorf("drop sweep batch coordinate constraint: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `ALTER TABLE person_sweep_batches ADD CONSTRAINT person_sweep_batches_call_coordinate_check `+personSweepBatchPurposeCheckV3); err != nil {
			return fmt.Errorf("add sweep batch coordinate constraint: %w", err)
		}
	} else if err := rebuildPersonSweepAccountingTable(ctx, tx, "person_sweep_batches", "'derive_repair'", personSweepDeriveBatchRebuildStatements()); err != nil {
		return err
	}
	if err := s.personSweepDeriveAccountingStage("batches"); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS idx_person_sweep_batches_day_purpose ON person_sweep_batches(utc_day,purpose)`); err != nil {
		return fmt.Errorf("index sweep calls by day and purpose: %w", err)
	}
	if err := s.personSweepDeriveAccountingStage("index"); err != nil {
		return err
	}
	if !s.IsPostgreSQL() {
		return checkPersonSweepAccountingReferences(ctx, tx)
	}
	return nil
}

func (s *Store) personSweepDeriveAccountingStage(stage string) error {
	if s.personSweepDeriveAccountingHook != nil {
		return s.personSweepDeriveAccountingHook(stage)
	}
	return nil
}

func validatePersonSweepDeriveAccountingRows(ctx context.Context, tx *loggedTx) error {
	for _, q := range []string{
		`SELECT COUNT(*) FROM person_sweep_attempts WHERE NOT (mode IN ('incremental','backstop','derive'))`,
		`SELECT COUNT(*) FROM person_sweep_batches WHERE NOT (
   (call_ordinal=0 AND purpose IN ('primary','brief','observation','derive_deductive','derive_inductive')) OR
   (call_ordinal=1 AND purpose IN ('repair','brief_repair','observation_repair','derive_repair')))`,
	} {
		var invalid int
		if err := tx.QueryRowContext(ctx, q).Scan(&invalid); err != nil {
			return fmt.Errorf("validate sweep derive accounting rows: %w", err)
		}
		if invalid != 0 {
			return fmt.Errorf("sweep derive accounting contains %d invalid rows", invalid)
		}
	}
	return nil
}

func rebuildPersonSweepAccountingTable(ctx context.Context, tx *loggedTx, table, marker string, statements []string) error {
	var definition string
	if err := tx.QueryRowContext(ctx, `SELECT sql FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&definition); err != nil {
		return fmt.Errorf("inspect %s: %w", table, err)
	}
	if strings.Contains(definition, marker) {
		return nil
	}
	for _, statement := range statements {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("rebuild %s: %w", table, err)
		}
	}
	return nil
}

// Scope validation to the rebuilt attempts and every table referencing it.
// Discovering inbound tables also covers future derived accounting children.
func checkPersonSweepAccountingReferences(ctx context.Context, tx *loggedTx) error {
	var violations int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_foreign_key_check('person_sweep_attempts')`).Scan(&violations); err != nil {
		return fmt.Errorf("check sweep attempt references: %w", err)
	}
	rows, err := tx.QueryContext(ctx, `SELECT name FROM sqlite_master AS tables WHERE type='table'
  AND EXISTS (SELECT 1 FROM pragma_foreign_key_list(tables.name) AS fk WHERE fk."table"='person_sweep_attempts')`)
	if err != nil {
		return fmt.Errorf("find sweep attempt child tables: %w", err)
	}
	var tables []string
	for rows.Next() {
		var table string
		if err := rows.Scan(&table); err != nil {
			_ = rows.Close()
			return fmt.Errorf("read sweep attempt child table: %w", err)
		}
		tables = append(tables, table)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("iterate sweep attempt child tables: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close sweep attempt child tables: %w", err)
	}
	for _, table := range tables {
		var count int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_foreign_key_check(?) WHERE parent='person_sweep_attempts'`, table).Scan(&count); err != nil {
			return fmt.Errorf("check %s sweep attempt references: %w", table, err)
		}
		violations += count
	}
	if violations != 0 {
		return fmt.Errorf("sweep accounting rebuild left %d dangling references", violations)
	}
	return nil
}

func personSweepDeriveAttemptRebuildStatements() []string {
	return []string{
		`CREATE TABLE person_sweep_attempts_derive_v1 (
    id                          TEXT PRIMARY KEY,
    run_id                      TEXT NOT NULL REFERENCES person_sweep_runs(id) ON DELETE CASCADE,
    person_id                   INTEGER NOT NULL REFERENCES persons(id) ON DELETE CASCADE,
    lease_fence                 INTEGER NOT NULL CHECK (lease_fence >= 0),
    mode                        TEXT NOT NULL ` + personSweepAttemptModeCheck + `,
    status                      TEXT NOT NULL CHECK (status IN ('running', 'succeeded', 'failed', 'cancelled')),
    failure_class               TEXT NOT NULL DEFAULT '' CHECK (failure_class IN (
        '', 'policy', 'budget', 'lease_lost', 'rate_limited', 'timeout',
        'provider_http', 'invalid_output', 'archive_gap', 'internal'
    )),
    brief_failure_class         TEXT NOT NULL DEFAULT '' CHECK (brief_failure_class IN (
        '', 'policy', 'budget', 'lease_lost', 'rate_limited', 'timeout',
        'provider_http', 'invalid_output', 'archive_gap', 'internal'
    )),
    derive_failure_class        TEXT NOT NULL DEFAULT '' ` + personSweepAttemptDeriveFailureClassCheck + `,
    memory_epoch                TEXT NOT NULL DEFAULT '',
    memory_sha256               TEXT NOT NULL DEFAULT '',
    work_list_through           INTEGER NOT NULL DEFAULT 0 CHECK (work_list_through >= 0),
    notes_applied               INTEGER NOT NULL DEFAULT 0 CHECK (notes_applied >= 0),
    notes_rejected              INTEGER NOT NULL DEFAULT 0 CHECK (notes_rejected >= 0),
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
)`,
		`INSERT INTO person_sweep_attempts_derive_v1 (id, run_id, person_id, lease_fence, mode, status, failure_class, brief_failure_class, cursor_envelope_json, envelope_hash, program_fingerprint, catalog_fingerprint, provider_fingerprint, generation_id, generation_key, seed_count, context_count, claim_count, decision_count, projected_write_count, request_count, provider_request_id, input_tokens, output_tokens, estimated_cost_micro_usd, latency_milliseconds, retry_at, started_at, completed_at) SELECT id, run_id, person_id, lease_fence, mode, status, failure_class, brief_failure_class, cursor_envelope_json, envelope_hash, program_fingerprint, catalog_fingerprint, provider_fingerprint, generation_id, generation_key, seed_count, context_count, claim_count, decision_count, projected_write_count, request_count, provider_request_id, input_tokens, output_tokens, estimated_cost_micro_usd, latency_milliseconds, retry_at, started_at, completed_at FROM person_sweep_attempts`,
		`DROP TABLE person_sweep_attempts`,
		`ALTER TABLE person_sweep_attempts_derive_v1 RENAME TO person_sweep_attempts`,
		`CREATE INDEX IF NOT EXISTS idx_person_sweep_attempts_person_started
    ON person_sweep_attempts(person_id, started_at DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_person_sweep_attempts_run
    ON person_sweep_attempts(run_id, id)`,
		`CREATE INDEX IF NOT EXISTS idx_person_sweep_attempts_operations_failure
    ON person_sweep_attempts(
        run_id,
        COALESCE(completed_at, started_at) DESC,
        id COLLATE BINARY DESC
    ) WHERE failure_class <> ''`,
		`CREATE INDEX IF NOT EXISTS idx_person_sweep_attempts_generation
    ON person_sweep_attempts(generation_id)`,
	}
}

func personSweepDeriveBatchRebuildStatements() []string {
	return []string{
		`CREATE TABLE person_sweep_batches_derive_v1 (
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
    CONSTRAINT person_sweep_batches_call_coordinate_check ` + personSweepBatchPurposeCheckV3 + `,
    PRIMARY KEY (attempt_id, batch_ordinal, call_ordinal)
)`,
		`INSERT INTO person_sweep_batches_derive_v1 (attempt_id, batch_ordinal, call_ordinal, purpose, utc_day, reservation_id, budget_fingerprint, input_hash, item_count, status, provider_request_id, reserved_requests, reserved_input_tokens, reserved_output_tokens, reserved_cost_micro_usd, actual_requests, actual_input_tokens, actual_output_tokens, actual_cost_micro_usd, latency_milliseconds, failure_class, created_at, completed_at) SELECT attempt_id, batch_ordinal, call_ordinal, purpose, utc_day, reservation_id, budget_fingerprint, input_hash, item_count, status, provider_request_id, reserved_requests, reserved_input_tokens, reserved_output_tokens, reserved_cost_micro_usd, actual_requests, actual_input_tokens, actual_output_tokens, actual_cost_micro_usd, latency_milliseconds, failure_class, created_at, completed_at FROM person_sweep_batches`,
		`DROP TABLE person_sweep_batches`,
		`ALTER TABLE person_sweep_batches_derive_v1 RENAME TO person_sweep_batches`,
	}
}
