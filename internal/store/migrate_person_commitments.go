package store

import (
	"context"
	"fmt"
)

func (s *Store) migratePersonCommitmentFailure(ctx context.Context) error {
	return s.runMaintenance(ctx, func(ctx context.Context, tx *loggedTx) error {
		if s.IsPostgreSQL() {
			_, err := tx.ExecContext(ctx, `ALTER TABLE person_sweep_attempts ADD COLUMN IF NOT EXISTS commitment_failure_class TEXT NOT NULL DEFAULT ''`)
			return err
		}
		present, err := sqliteColumnPresent(ctx, tx, "person_sweep_attempts", "commitment_failure_class")
		if err != nil || present {
			return err
		}
		_, err = tx.ExecContext(ctx, `ALTER TABLE person_sweep_attempts ADD COLUMN commitment_failure_class TEXT NOT NULL DEFAULT ''`)
		if err != nil {
			return fmt.Errorf("add commitment failure class: %w", err)
		}
		return nil
	})
}
