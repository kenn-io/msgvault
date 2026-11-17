package store

import (
	"context"
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
	return s.migratePersonFactClaimOriginSQLite(ctx, personFactClaimCommitmentOriginCheck, "'commitment'")
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
	for _, statement := range personSweepBatchRebuildStatements(personSweepBatchCommitmentPurposeCheck) {
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
