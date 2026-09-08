package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// personFactClaimOriginCheck is the widened claim-origin vocabulary. The person
// brief proposes attributes through the same resolver as extraction, so "brief"
// is a first-class origin rather than a second write path. Written once so the
// migration and the rebuilt table cannot drift.
const personFactClaimOriginCheck = `CHECK (origin IN ('extraction', 'enrichment', 'brief', 'system', 'invalid'))`

// personFactClaimOriginConstraint is the constraint name both backends use.
// PostgreSQL auto-names an inline column check exactly this way, so an archive
// that predates the named constraint drops under the same name.
const personFactClaimOriginConstraint = "person_fact_claims_origin_check"

// migratePersonFactClaimOriginBrief widens person_fact_claims.origin so a claim
// the person brief proposed can be recorded with its true origin. Only the
// check changes; every column, key, index, and row is preserved, including the
// claim's evidence links and the resolutions and decisions that reference it.
func (s *Store) migratePersonFactClaimOriginBrief(ctx context.Context) error {
	if s.IsPostgreSQL() {
		return s.runMaintenance(ctx, func(ctx context.Context, tx *loggedTx) error {
			if err := validatePersonFactClaimOriginRows(ctx, tx); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `ALTER TABLE person_fact_claims
				DROP CONSTRAINT IF EXISTS `+personFactClaimOriginConstraint); err != nil {
				return fmt.Errorf("drop person fact claim origin constraint: %w", err)
			}
			if _, err := tx.ExecContext(ctx, `ALTER TABLE person_fact_claims
				ADD CONSTRAINT `+personFactClaimOriginConstraint+` `+
				personFactClaimOriginCheck); err != nil {
				return fmt.Errorf("create person fact claim origin constraint: %w", err)
			}
			return nil
		})
	}
	return s.migratePersonFactClaimOriginBriefSQLite(ctx)
}

// migratePersonFactClaimOriginBriefSQLite follows SQLite's documented procedure
// for an arbitrary schema change: rebuild the table inside a transaction on a
// connection whose foreign keys are disabled. Foreign keys must be off because
// person_fact_claim_evidence, person_fact_resolutions, and
// person_fact_decisions reference person_fact_claims with ON DELETE CASCADE and
// ON DELETE SET NULL, and DROP TABLE performs an implicit delete that would
// otherwise fire those actions and destroy the ledger. The pragma is
// connection-scoped and cannot change inside a transaction, so this runs on a
// dedicated pooled connection rather than through runMaintenance.
func (s *Store) migratePersonFactClaimOriginBriefSQLite(ctx context.Context) (err error) {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("acquire connection to widen person fact claim origins: %w", err)
	}
	defer func() { _ = conn.Close() }()

	var definition string
	if err := conn.QueryRowContext(ctx, `SELECT sql FROM sqlite_master
		WHERE type = 'table' AND name = 'person_fact_claims'`).Scan(&definition); err != nil {
		return fmt.Errorf("inspect person fact claim origins: %w", err)
	}
	if strings.Contains(definition, "'brief'") {
		return nil
	}
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
	if err := validatePersonFactClaimOriginRows(ctx, &loggedTx{Tx: tx, rebind: s.Rebind}); err != nil {
		return err
	}
	for _, statement := range personFactClaimOriginRebuildStatements() {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("rebuild person fact claim origins: %w", err)
		}
	}
	violations, err := countSQLiteForeignKeyViolations(ctx, tx)
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

func personFactClaimOriginRebuildStatements() []string {
	return []string{
		`DROP TABLE IF EXISTS person_fact_claims_origin_v2`,
		`CREATE TABLE person_fact_claims_origin_v2 (
			id                    INTEGER PRIMARY KEY,
			person_id             INTEGER NOT NULL REFERENCES persons(id) ON DELETE CASCADE,
			generation_id         INTEGER NOT NULL REFERENCES person_fact_generations(id) ON DELETE CASCADE,
			claim_key             TEXT NOT NULL,
			target_kind           TEXT NOT NULL,
			target_key            TEXT NOT NULL,
			target_revision       TEXT NOT NULL,
			relation              TEXT NOT NULL CHECK (relation IN ('support', 'contradict', 'supersede', 'invalid')),
			submitted_value_json  TEXT NOT NULL,
			normalized_value_json JSON,
			value_fingerprint     TEXT,
			valid_from            DATETIME,
			valid_until           DATETIME,
			origin                TEXT NOT NULL CONSTRAINT ` + personFactClaimOriginConstraint + `
			                          ` + personFactClaimOriginCheck + `,
			confidence_json       JSON NOT NULL,
			rejection_action      TEXT CHECK (rejection_action IN (
			                          'applied', 'retained', 'superseded', 'invalid', 'identity-rejected',
			                          'policy-rejected', 'conflict-rejected', 'ambiguous-retained')),
			rejection_reason      TEXT CHECK (rejection_reason IN (
			                          'malformed-value', 'unsupported-target', 'stale-target-revision',
			                          'unaligned-evidence', 'identity-mismatch', 'sensitive-policy',
			                          'pin-retained', 'below-threshold', 'insufficient-margin',
			                          'competing-tie', 'explicit-contradiction', 'explicit-supersession',
			                          'organization-ambiguous', 'applied-projection', 'evidence-unsupported',
			                          'outside-validity')),
			rejection_detail      TEXT,
			created_at            DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			CHECK ((rejection_action IS NULL AND rejection_reason IS NULL AND rejection_detail IS NULL) OR
			       (rejection_action IS NOT NULL AND rejection_reason IS NOT NULL AND
			        rejection_detail IS NOT NULL AND rejection_detail <> '')),
			UNIQUE(person_id, claim_key),
			CHECK ((normalized_value_json IS NULL) = (value_fingerprint IS NULL)),
			CHECK (valid_from IS NULL OR valid_until IS NULL OR valid_until >= valid_from)
		)`,
		`INSERT INTO person_fact_claims_origin_v2 (
			id, person_id, generation_id, claim_key, target_kind, target_key, target_revision,
			relation, submitted_value_json, normalized_value_json, value_fingerprint,
			valid_from, valid_until, origin, confidence_json, rejection_action,
			rejection_reason, rejection_detail, created_at)
		 SELECT id, person_id, generation_id, claim_key, target_kind, target_key, target_revision,
			relation, submitted_value_json, normalized_value_json, value_fingerprint,
			valid_from, valid_until, origin, confidence_json, rejection_action,
			rejection_reason, rejection_detail, created_at
		 FROM person_fact_claims`,
		`DROP TABLE person_fact_claims`,
		`ALTER TABLE person_fact_claims_origin_v2 RENAME TO person_fact_claims`,
		`CREATE INDEX IF NOT EXISTS idx_person_fact_claims_person_target
			ON person_fact_claims(person_id, target_kind, target_key, id DESC)`,
	}
}

// validatePersonFactClaimOriginRows refuses to widen the constraint over rows
// the widened vocabulary would still reject, so a corrupt ledger fails the
// upgrade loudly instead of at the next insert.
func validatePersonFactClaimOriginRows(ctx context.Context, tx *loggedTx) error {
	var invalid int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM person_fact_claims
		WHERE origin NOT IN ('extraction', 'enrichment', 'brief', 'system', 'invalid')`,
	).Scan(&invalid); err != nil {
		return fmt.Errorf("validate person fact claim origins: %w", err)
	}
	if invalid != 0 {
		return fmt.Errorf("person fact ledger contains %d invalid claim origins", invalid)
	}
	return nil
}

// migratePersonSweepAttemptBriefFailure adds the attempt column that records
// why a brief call did not produce a version while the attempt itself
// succeeded. Adding a column preserves every row on both backends, and IF NOT
// EXISTS (PostgreSQL) or a column probe (SQLite) makes it idempotent.
func (s *Store) migratePersonSweepAttemptBriefFailure(ctx context.Context) error {
	return s.runMaintenance(ctx, func(ctx context.Context, tx *loggedTx) error {
		if s.IsPostgreSQL() {
			_, err := tx.ExecContext(ctx, `ALTER TABLE person_sweep_attempts
				ADD COLUMN IF NOT EXISTS brief_failure_class TEXT NOT NULL DEFAULT ''`)
			if err != nil {
				return fmt.Errorf("add person sweep attempt brief failure class: %w", err)
			}
			if _, err := tx.ExecContext(ctx, `ALTER TABLE person_sweep_attempts
				DROP CONSTRAINT IF EXISTS person_sweep_attempts_brief_failure_class_check`); err != nil {
				return fmt.Errorf("drop person sweep attempt brief failure constraint: %w", err)
			}
			if _, err := tx.ExecContext(ctx, `ALTER TABLE person_sweep_attempts
				ADD CONSTRAINT person_sweep_attempts_brief_failure_class_check `+
				personSweepBriefFailureClassCheck); err != nil {
				return fmt.Errorf("create person sweep attempt brief failure constraint: %w", err)
			}
			return nil
		}
		present, err := sqliteColumnPresent(ctx, tx, "person_sweep_attempts", "brief_failure_class")
		if err != nil {
			return err
		}
		if present {
			return nil
		}
		if _, err := tx.ExecContext(ctx, `ALTER TABLE person_sweep_attempts
			ADD COLUMN brief_failure_class TEXT NOT NULL DEFAULT '' `+
			personSweepBriefFailureClassCheck); err != nil {
			return fmt.Errorf("add person sweep attempt brief failure class: %w", err)
		}
		return nil
	})
}

// personSweepBriefFailureClassCheck is the brief column's enum, identical to
// the attempt's own failure_class vocabulary.
const personSweepBriefFailureClassCheck = `CHECK (brief_failure_class IN (
		'', 'policy', 'budget', 'lease_lost', 'rate_limited', 'timeout',
		'provider_http', 'invalid_output', 'archive_gap', 'internal'
	))`

func sqliteColumnPresent(ctx context.Context, tx *loggedTx, table, column string) (bool, error) {
	rows, err := tx.QueryContext(ctx, `SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		return false, fmt.Errorf("inspect %s columns: %w", table, err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return false, fmt.Errorf("scan %s column: %w", table, err)
		}
		if name == column {
			return true, nil
		}
	}
	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("iterate %s columns: %w", table, err)
	}
	return false, nil
}

// countSQLiteForeignKeyViolations reports how many references the rebuilt
// schema leaves dangling, so a rebuild that lost a row fails the upgrade rather
// than committing a corrupt ledger.
func countSQLiteForeignKeyViolations(ctx context.Context, tx *sql.Tx) (int, error) {
	rows, err := tx.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		return 0, fmt.Errorf("check person fact claim references: %w", err)
	}
	defer func() { _ = rows.Close() }()
	violations := 0
	for rows.Next() {
		violations++
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("check person fact claim references: %w", err)
	}
	return violations, nil
}
