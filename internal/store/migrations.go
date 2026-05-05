package store

import (
	"fmt"
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
