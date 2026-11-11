package store

import (
	"context"
	"fmt"
	"strings"
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

// An agenda receipt outlives its promise. Ensure SQLite never reuses a
// retired c<ID> for another promise when the highest row is removed.
func (s *Store) migratePersonCommitmentAgendaIDs(ctx context.Context) error {
	if s.IsPostgreSQL() {
		return nil
	}
	return s.runMaintenance(ctx, func(ctx context.Context, tx *loggedTx) error {
		var definition string
		if err := tx.QueryRowContext(ctx, `SELECT sql FROM sqlite_master
			WHERE type = 'table' AND name = 'person_commitments'`).Scan(&definition); err != nil {
			return err
		}
		if strings.Contains(definition, "AUTOINCREMENT") {
			return nil
		}
		rebuild := strings.Replace(definition, "person_commitments", "person_commitments_new", 1)
		rebuild = strings.Replace(rebuild, "INTEGER PRIMARY KEY,", "INTEGER PRIMARY KEY AUTOINCREMENT,", 1)
		for _, statement := range []string{
			rebuild,
			`INSERT INTO person_commitments_new (` + commitmentColumns + `)
			  SELECT ` + commitmentColumns + ` FROM person_commitments`,
			`DROP TABLE person_commitments`,
			`ALTER TABLE person_commitments_new RENAME TO person_commitments`,
		} {
			if _, err := tx.ExecContext(ctx, statement); err != nil {
				return err
			}
		}
		return nil
	})
}
