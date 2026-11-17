package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

const personFactClaimCommitmentOriginCheck = `CHECK (origin IN ('extraction', 'enrichment', 'brief', 'commitment', 'system', 'invalid'))`
const personSweepBatchCommitmentPurposeCheck = `CHECK (
 (call_ordinal = 0 AND purpose IN ('primary', 'brief', 'commitment')) OR
 (call_ordinal = 1 AND purpose IN ('repair', 'brief_repair', 'commitment_repair'))
)`

func (s *Store) migratePersonCommitments(ctx context.Context) error {
	if err := s.migrateCommitmentClaimOrigin(ctx); err != nil {
		return err
	}
	return s.runMaintenance(ctx, func(ctx context.Context, tx *loggedTx) error {
		if s.IsPostgreSQL() {
			return migrateCommitmentBatchPurposePostgreSQL(ctx, tx)
		}
		return migrateCommitmentBatchPurposeSQLite(ctx, tx)
	})
}

func (s *Store) migrateCommitmentClaimOrigin(ctx context.Context) error {
	if s.IsPostgreSQL() {
		return s.runMaintenance(ctx, func(ctx context.Context, tx *loggedTx) error {
			if _, err := tx.ExecContext(ctx, `ALTER TABLE person_fact_claims
				DROP CONSTRAINT IF EXISTS `+personFactClaimOriginConstraint); err != nil {
				return fmt.Errorf("drop person fact claim origin constraint: %w", err)
			}
			if _, err := tx.ExecContext(ctx, `ALTER TABLE person_fact_claims
				ADD CONSTRAINT `+personFactClaimOriginConstraint+` `+
				personFactClaimCommitmentOriginCheck); err != nil {
				return fmt.Errorf("create person fact claim origin constraint: %w", err)
			}
			return nil
		})
	}
	return s.migrateCommitmentClaimOriginSQLite(ctx)
}

// migrateCommitmentClaimOriginSQLite follows SQLite's documented procedure
// for an arbitrary schema change: rebuild the table inside a transaction on a
// connection whose foreign keys are disabled. Foreign keys must be off because
// person_fact_claim_evidence, person_fact_resolutions, and
// person_fact_decisions reference person_fact_claims with ON DELETE CASCADE and
// ON DELETE SET NULL, and DROP TABLE performs an implicit delete that would
// otherwise fire those actions and destroy the ledger. The pragma is
// connection-scoped and cannot change inside a transaction, so this runs on a
// dedicated pooled connection rather than through runMaintenance.
func (s *Store) migrateCommitmentClaimOriginSQLite(ctx context.Context) (err error) {
	var definition string
	if err := s.db.QueryRowContext(ctx, `SELECT sql FROM sqlite_master
		WHERE type = 'table' AND name = 'person_fact_claims'`).Scan(&definition); err != nil {
		return fmt.Errorf("inspect person fact claim origins: %w", err)
	}
	if strings.Contains(definition, "'commitment'") {
		return nil
	}
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("acquire connection to widen person fact claim origins: %w", err)
	}
	defer func() { _ = conn.Close() }()

	if _, err := conn.ExecContext(ctx, `PRAGMA foreign_keys = OFF`); err != nil {
		return fmt.Errorf("suspend foreign keys to widen person fact claim origins: %w", err)
	}
	// Restore enforcement before the connection returns to the pool, whatever
	// happens below. A pooled connection is reused, so a leaked pragma would
	// silently disable foreign keys for unrelated later work; failing to
	// restore it is reported rather than swallowed.
	defer func() {
		if _, restoreErr := conn.ExecContext(context.WithoutCancel(ctx),
			`PRAGMA foreign_keys = ON`); restoreErr != nil {
			err = errors.Join(err, fmt.Errorf(
				"restore foreign keys after person fact claim rebuild: %w", restoreErr))
		}
	}()

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin person fact claim origin rebuild: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	for _, statement := range personFactClaimOriginRebuildStatements() {
		statement = strings.ReplaceAll(statement, personFactClaimOriginCheck, personFactClaimCommitmentOriginCheck)
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("rebuild person fact claim origins: %w", err)
		}
	}
	violations, err := countPersonFactClaimForeignKeyViolations(ctx, tx)
	if err != nil {
		return err
	}
	if violations != 0 {
		return fmt.Errorf("person fact claim rebuild left %d dangling references", violations)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit person fact claim origin rebuild: %w", err)
	}
	committed = true
	return nil
}

func migrateCommitmentBatchPurposeSQLite(ctx context.Context, tx *loggedTx) error {
	var definition string
	if err := tx.QueryRowContext(ctx, `SELECT sql FROM sqlite_master
		WHERE type = 'table' AND name = 'person_sweep_batches'`).Scan(&definition); err != nil {
		return fmt.Errorf("inspect person sweep call journal purposes: %w", err)
	}
	if strings.Contains(definition, "'commitment_repair'") {
		return nil
	}
	statements := []string{
		`DROP TABLE IF EXISTS person_sweep_batches_purpose_v2`,
		`CREATE TABLE person_sweep_batches_purpose_v2 (
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
			CONSTRAINT person_sweep_batches_call_coordinate_check ` + personSweepBatchCommitmentPurposeCheck + `,
			PRIMARY KEY (attempt_id, batch_ordinal, call_ordinal)
		)`,
		`INSERT INTO person_sweep_batches_purpose_v2 (
			attempt_id, batch_ordinal, call_ordinal, purpose, utc_day, reservation_id,
			budget_fingerprint, input_hash, item_count, status, provider_request_id,
			reserved_requests, reserved_input_tokens, reserved_output_tokens,
			reserved_cost_micro_usd, actual_requests, actual_input_tokens,
			actual_output_tokens, actual_cost_micro_usd, latency_milliseconds,
			failure_class, created_at, completed_at)
		 SELECT attempt_id, batch_ordinal, call_ordinal, purpose, utc_day, reservation_id,
			budget_fingerprint, input_hash, item_count, status, provider_request_id,
			reserved_requests, reserved_input_tokens, reserved_output_tokens,
			reserved_cost_micro_usd, actual_requests, actual_input_tokens,
			actual_output_tokens, actual_cost_micro_usd, latency_milliseconds,
			failure_class, created_at, completed_at
		 FROM person_sweep_batches`,
		`DROP TABLE person_sweep_batches`,
		`ALTER TABLE person_sweep_batches_purpose_v2 RENAME TO person_sweep_batches`,
	}
	for _, statement := range statements {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("rebuild person sweep call journal purposes: %w", err)
		}
	}
	return nil
}

func migrateCommitmentBatchPurposePostgreSQL(ctx context.Context, tx *loggedTx) error {
	if _, err := tx.ExecContext(ctx, `ALTER TABLE person_sweep_batches
		DROP CONSTRAINT IF EXISTS person_sweep_batches_call_coordinate_check`); err != nil {
		return fmt.Errorf("drop person sweep call journal purpose constraint: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `ALTER TABLE person_sweep_batches
		ADD CONSTRAINT person_sweep_batches_call_coordinate_check `+
		personSweepBatchCommitmentPurposeCheck); err != nil {
		return fmt.Errorf("create person sweep call journal purpose constraint: %w", err)
	}
	return nil
}
