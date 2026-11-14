package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// personFactClaimCommitmentOriginCheck includes the separately consented commitment lane.
const personFactClaimCommitmentOriginCheck = `CHECK (origin IN ('extraction', 'enrichment', 'brief', 'commitment', 'system', 'invalid'))`

// personFactClaimCommitmentOriginConstraint is the constraint name both backends use.
// PostgreSQL auto-names an inline column check exactly this way, so an archive
// that predates the named constraint drops under the same name.
const personFactClaimCommitmentOriginConstraint = "person_fact_claims_origin_check"

// migratePersonFactClaimOriginCommitment widens person_fact_claims.origin so a claim
// the commitment sweep proposed can be recorded with its true origin. Only the
// check changes; every column, key, index, and row is preserved, including the
// claim's evidence links and the resolutions and decisions that reference it.
func (s *Store) migratePersonFactClaimOriginCommitment(ctx context.Context) error {
	if s.IsPostgreSQL() {
		return s.runMaintenance(ctx, func(ctx context.Context, tx *loggedTx) error {
			if err := validatePersonFactClaimCommitmentOriginRows(ctx, tx); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `ALTER TABLE person_fact_claims
				DROP CONSTRAINT IF EXISTS `+personFactClaimCommitmentOriginConstraint); err != nil {
				return fmt.Errorf("drop person fact claim origin constraint: %w", err)
			}
			if _, err := tx.ExecContext(ctx, `ALTER TABLE person_fact_claims
				ADD CONSTRAINT `+personFactClaimCommitmentOriginConstraint+` `+
				personFactClaimCommitmentOriginCheck); err != nil {
				return fmt.Errorf("create person fact claim origin constraint: %w", err)
			}
			return nil
		})
	}
	return s.migratePersonFactClaimOriginCommitmentSQLite(ctx)
}

// migratePersonFactClaimOriginCommitmentSQLite follows SQLite's documented procedure
// for an arbitrary schema change: rebuild the table inside a transaction on a
// connection whose foreign keys are disabled. Foreign keys must be off because
// person_fact_claim_evidence, person_fact_resolutions, and
// person_fact_decisions reference person_fact_claims with ON DELETE CASCADE and
// ON DELETE SET NULL, and DROP TABLE performs an implicit delete that would
// otherwise fire those actions and destroy the ledger. The pragma is
// connection-scoped and cannot change inside a transaction, so this runs on a
// dedicated pooled connection rather than through runMaintenance.
func (s *Store) migratePersonFactClaimOriginCommitmentSQLite(ctx context.Context) (err error) {
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
	if err := validatePersonFactClaimCommitmentOriginRows(ctx, &loggedTx{Tx: tx, rebind: s.Rebind}); err != nil {
		return err
	}
	for _, statement := range personFactClaimCommitmentOriginRebuildStatements() {
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

func personFactClaimCommitmentOriginRebuildStatements() []string {
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
			origin                TEXT NOT NULL CONSTRAINT ` + personFactClaimCommitmentOriginConstraint + `
			                          ` + personFactClaimCommitmentOriginCheck + `,
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

// validatePersonFactClaimCommitmentOriginRows refuses to widen the constraint over rows
// the widened vocabulary would still reject, so a corrupt ledger fails the
// upgrade loudly instead of at the next insert.
func validatePersonFactClaimCommitmentOriginRows(ctx context.Context, tx *loggedTx) error {
	var invalid int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM person_fact_claims
		WHERE origin NOT IN ('extraction', 'enrichment', 'brief', 'commitment', 'system', 'invalid')`,
	).Scan(&invalid); err != nil {
		return fmt.Errorf("validate person fact claim origins: %w", err)
	}
	if invalid != 0 {
		return fmt.Errorf("person fact ledger contains %d invalid claim origins", invalid)
	}
	return nil
}
