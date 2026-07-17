package store

import (
	"fmt"
)

// One-time data migrations run by InitSchema and gated on the
// applied_migrations ledger. Without the gate their no-op verification
// re-runs on every daemon start and scales with archive size (the
// last_modified backfill alone is a full messages-table scan — seconds of
// startup on a large archive).
const (
	migrationAttachmentsContentHashUnique = "attachments_content_hash_unique_index"
	migrationMessagesLastModifiedBackfill = "messages_last_modified_backfill"
)

// IsMigrationApplied reports whether the named one-time data migration
// has already run.
func (s *Store) IsMigrationApplied(name string) (bool, error) {
	var count int
	err := s.db.QueryRow(
		`SELECT COUNT(*) FROM applied_migrations WHERE name = ?`, name,
	).Scan(&count)
	if err != nil {
		return false, fmt.Errorf("check migration %q: %w", name, err)
	}
	return count > 0, nil
}

// MarkMigrationApplied records that a migration has run. Idempotent.
func (s *Store) MarkMigrationApplied(name string) error {
	_, err := s.db.Exec(
		s.dialect.InsertOrIgnore(`INSERT OR IGNORE INTO applied_migrations (name) VALUES (?)`),
		name,
	)
	if err != nil {
		return fmt.Errorf("mark migration %q applied: %w", name, err)
	}
	return nil
}
