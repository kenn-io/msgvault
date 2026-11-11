package store

import (
	"context"
	"fmt"
)

// migratePersonSweepUnknownDirectionScope reinstalls only the person-sweep
// triggers and functions. Their direct-chat roster fallback now tests
// ownership with IS TRUE, so a senderless message with unknown direction stays
// in scope instead of evaluating to NULL.
func (s *Store) migratePersonSweepUnknownDirectionScope(ctx context.Context) error {
	statements := personSweepSQLiteTriggerStatements()
	if s.IsPostgreSQL() {
		statements = personSweepPostgreSQLTriggerStatements()
	}
	return s.runMaintenance(ctx, func(ctx context.Context, tx *loggedTx) error {
		for _, statement := range statements {
			if _, err := tx.ExecContext(ctx, statement); err != nil {
				return fmt.Errorf("reinstall person sweep triggers: %w", err)
			}
		}
		return nil
	})
}
