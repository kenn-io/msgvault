package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// Widen the provider vocabulary without changing existing profiles or their
// consent fingerprints. SQLite requires a table rebuild with inbound foreign
// keys temporarily disabled on the dedicated migration connection.
func (s *Store) migrateContextEnrichment(ctx context.Context) (err error) {
	if s.IsPostgreSQL() {
		return s.runMaintenance(ctx, func(ctx context.Context, tx *loggedTx) error {
			_, err := tx.ExecContext(ctx, `ALTER TABLE person_enrichment_profiles
    DROP CONSTRAINT person_enrichment_profiles_provider_kind_check,
    ADD CONSTRAINT person_enrichment_profiles_provider_kind_check
    CHECK (provider_kind IN ('exa', 'sixtyfour', 'context'))`)
			return err
		})
	}
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	var definition string
	if err := conn.QueryRowContext(ctx, `SELECT sql FROM sqlite_master WHERE type='table' AND name='person_enrichment_profiles'`).Scan(&definition); err != nil {
		return err
	}
	if strings.Contains(definition, "'context'") {
		return nil
	}
	if _, err := conn.ExecContext(ctx, `PRAGMA foreign_keys = OFF`); err != nil {
		return err
	}
	defer func() {
		_, restoreErr := conn.ExecContext(context.WithoutCancel(ctx), `PRAGMA foreign_keys = ON`)
		err = errors.Join(err, restoreErr)
	}()
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	statements := []string{
		`CREATE TABLE person_enrichment_profiles_context (
   fingerprint TEXT PRIMARY KEY, provider_name TEXT NOT NULL,
   provider_kind TEXT NOT NULL CHECK (provider_kind IN ('exa', 'sixtyfour', 'context')),
   provider_namespace TEXT NOT NULL, endpoint TEXT NOT NULL, api_key_env TEXT NOT NULL,
   policy_json JSON NOT NULL, created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
  )`,
		`INSERT INTO person_enrichment_profiles_context SELECT * FROM person_enrichment_profiles`,
		`DROP TABLE person_enrichment_profiles`,
		`ALTER TABLE person_enrichment_profiles_context RENAME TO person_enrichment_profiles`,
	}
	for _, statement := range statements {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("widen enrichment provider kinds: %w", err)
		}
	}
	var violations int
	if err := tx.QueryRowContext(ctx, `SELECT
 (SELECT COUNT(*) FROM pragma_foreign_key_check('person_enrichment_manual_run_targets') WHERE parent='person_enrichment_profiles') +
 (SELECT COUNT(*) FROM pragma_foreign_key_check('person_enrichment_work') WHERE parent='person_enrichment_profiles') +
 (SELECT COUNT(*) FROM pragma_foreign_key_check('person_enrichment_attempts') WHERE parent='person_enrichment_profiles') +
 (SELECT COUNT(*) FROM pragma_foreign_key_check('person_enrichment_person_day_counters') WHERE parent='person_enrichment_profiles') +
 (SELECT COUNT(*) FROM pragma_foreign_key_check('person_enrichment_day_counters') WHERE parent='person_enrichment_profiles') +
 (SELECT COUNT(*) FROM pragma_foreign_key_check('person_enrichment_profile_accounting') WHERE parent='person_enrichment_profiles')`).Scan(&violations); err != nil {
		return err
	}
	if violations != 0 {
		return fmt.Errorf("enrichment profile rebuild left %d dangling references", violations)
	}
	return tx.Commit()
}
