package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// personFactClaimCommitmentOriginCheck includes the separately consented commitment lane.
const personFactClaimCommitmentOriginCheck = `CHECK (origin IN ('extraction', 'enrichment', 'brief', 'commitment', 'system', 'invalid'))`

// migratePersonFactClaimOriginCommitment widens person_fact_claims.origin so a claim
// the commitment sweep proposed can be recorded with its true origin. Only the
// check changes; every column, key, index, and row is preserved, including the
// claim's evidence links and the resolutions and decisions that reference it.
func (s *Store) migratePersonFactClaimOriginCommitment(ctx context.Context) error {
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
