package store

import (
	"context"
	"fmt"
)

func (s *Store) migratePersonSweepCallsV2(ctx context.Context) error {
	return s.runMaintenance(ctx, func(ctx context.Context, tx *loggedTx) error {
		if s.IsPostgreSQL() {
			return migratePersonSweepCallsV2PostgreSQL(ctx, tx)
		}
		return migratePersonSweepCallsV2SQLite(ctx, tx)
	})
}

func migratePersonSweepCallsV2SQLite(ctx context.Context, tx *loggedTx) error {
	type column struct {
		name string
		pk   int
	}
	rows, err := tx.QueryContext(ctx, `PRAGMA table_info(person_sweep_batches)`)
	if err != nil {
		return fmt.Errorf("inspect person sweep call journal: %w", err)
	}
	columns := make(map[string]column)
	for rows.Next() {
		var cid, notNull, pk int
		var name, dataType string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &dataType, &notNull, &defaultValue, &pk); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan person sweep call journal column: %w", err)
		}
		columns[name] = column{name: name, pk: pk}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("iterate person sweep call journal columns: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close person sweep call journal columns: %w", err)
	}
	if columns["attempt_id"].pk == 1 && columns["batch_ordinal"].pk == 2 &&
		columns["call_ordinal"].pk == 3 {
		return validatePersonSweepCallRows(ctx, tx)
	}

	callOrdinal, purpose := "0", "'primary'"
	if _, ok := columns["call_ordinal"]; ok {
		callOrdinal = "call_ordinal"
	}
	if _, ok := columns["purpose"]; ok {
		purpose = "purpose"
	}
	if err := validatePersonSweepCallRowsWithExpressions(ctx, tx, callOrdinal, purpose); err != nil {
		return err
	}
	statements := []string{
		`DROP TABLE IF EXISTS person_sweep_batches_calls_v2`,
		`CREATE TABLE person_sweep_batches_calls_v2 (
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
			CONSTRAINT person_sweep_batches_call_coordinate_check CHECK (
				(call_ordinal = 0 AND purpose = 'primary') OR
				(call_ordinal = 1 AND purpose = 'repair')
			),
			PRIMARY KEY (attempt_id, batch_ordinal, call_ordinal)
		)`,
		`INSERT INTO person_sweep_batches_calls_v2 (
			attempt_id, batch_ordinal, call_ordinal, purpose, utc_day, reservation_id,
			budget_fingerprint, input_hash, item_count, status, provider_request_id,
			reserved_requests, reserved_input_tokens, reserved_output_tokens,
			reserved_cost_micro_usd, actual_requests, actual_input_tokens,
			actual_output_tokens, actual_cost_micro_usd, latency_milliseconds,
			failure_class, created_at, completed_at)
		 SELECT attempt_id, batch_ordinal, ` + callOrdinal + `, ` + purpose + `, utc_day,
			reservation_id, budget_fingerprint, input_hash, item_count, status,
			provider_request_id, reserved_requests, reserved_input_tokens,
			reserved_output_tokens, reserved_cost_micro_usd, actual_requests,
			actual_input_tokens, actual_output_tokens, actual_cost_micro_usd,
			latency_milliseconds, failure_class, created_at, completed_at
		 FROM person_sweep_batches`,
		`DROP TABLE person_sweep_batches`,
		`ALTER TABLE person_sweep_batches_calls_v2 RENAME TO person_sweep_batches`,
	}
	for _, statement := range statements {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("rebuild person sweep call journal: %w", err)
		}
	}
	return nil
}

func migratePersonSweepCallsV2PostgreSQL(ctx context.Context, tx *loggedTx) error {
	for _, statement := range []string{
		`ALTER TABLE person_sweep_batches ADD COLUMN IF NOT EXISTS call_ordinal INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE person_sweep_batches ADD COLUMN IF NOT EXISTS purpose TEXT NOT NULL DEFAULT 'primary'`,
	} {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("add person sweep call journal coordinate: %w", err)
		}
	}
	if err := validatePersonSweepCallRows(ctx, tx); err != nil {
		return err
	}
	var primaryKey string
	if err := tx.QueryRowContext(ctx, `
		SELECT conname FROM pg_constraint
		WHERE conrelid = 'person_sweep_batches'::regclass AND contype = 'p'
	`).Scan(&primaryKey); err != nil {
		return fmt.Errorf("load person sweep call journal primary key: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`ALTER TABLE person_sweep_batches DROP CONSTRAINT `+quoteIdentifier(primaryKey)); err != nil {
		return fmt.Errorf("drop person sweep call journal primary key: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `ALTER TABLE person_sweep_batches
		ADD PRIMARY KEY (attempt_id, batch_ordinal, call_ordinal)`); err != nil {
		return fmt.Errorf("create person sweep call journal primary key: %w", err)
	}
	var coordinateCheck bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS (
		SELECT 1 FROM pg_constraint
		WHERE conrelid = 'person_sweep_batches'::regclass
		  AND conname = 'person_sweep_batches_call_coordinate_check'
	)`).Scan(&coordinateCheck); err != nil {
		return fmt.Errorf("inspect person sweep call journal constraint: %w", err)
	}
	if !coordinateCheck {
		if _, err := tx.ExecContext(ctx, `ALTER TABLE person_sweep_batches
			ADD CONSTRAINT person_sweep_batches_call_coordinate_check CHECK (
				(call_ordinal = 0 AND purpose = 'primary') OR
				(call_ordinal = 1 AND purpose = 'repair'))`); err != nil {
			return fmt.Errorf("create person sweep call journal constraint: %w", err)
		}
	}
	return nil
}

func validatePersonSweepCallRows(ctx context.Context, tx *loggedTx) error {
	return validatePersonSweepCallRowsWithExpressions(ctx, tx, "call_ordinal", "purpose")
}

func validatePersonSweepCallRowsWithExpressions(
	ctx context.Context, tx *loggedTx, callOrdinal, purpose string,
) error {
	var invalid int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM person_sweep_batches WHERE NOT ((`+
		callOrdinal+` = 0 AND `+purpose+` = 'primary') OR (`+callOrdinal+` = 1 AND `+
		purpose+` = 'repair'))`).Scan(&invalid); err != nil {
		return fmt.Errorf("validate person sweep call journal rows: %w", err)
	}
	if invalid != 0 {
		return fmt.Errorf("person sweep call journal contains %d invalid coordinates", invalid)
	}
	return nil
}
