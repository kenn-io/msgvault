package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
)

// SchemaVersion is the main archive schema contract. Increase it whenever the
// archive schema or a required migration changes. API, cache and optional vector
// backend versions are independent. Zero identifies pre-contract archives.
const SchemaVersion = 1

// SchemaVersionContext reads the stored completion marker without migrating.
// The marker certifies required InitSchemaContext work, not physical integrity.
func (s *Store) SchemaVersionContext(ctx context.Context) (int, error) {
	var version int
	if s.IsPostgreSQL() {
		var exists bool
		if err := s.db.QueryRowContext(ctx, `SELECT to_regclass('archive_metadata') IS NOT NULL`).Scan(&exists); err != nil {
			return 0, fmt.Errorf("inspect archive schema version metadata: %w", err)
		}
		if !exists {
			return 0, nil
		}
		var value string
		err := s.db.QueryRowContext(ctx, `SELECT value FROM archive_metadata WHERE key = 'schema_version'`).Scan(&value)
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		if err != nil {
			return 0, fmt.Errorf("read archive schema version: %w", err)
		}
		version, err = strconv.Atoi(value)
		if err != nil {
			return 0, fmt.Errorf("invalid archive schema version %q: %w", value, err)
		}
	} else {
		if err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
			return 0, fmt.Errorf("read archive schema version: %w", err)
		}
	}
	if version < 0 {
		return 0, fmt.Errorf("invalid negative archive schema version: %d", version)
	}
	return version, nil
}

func (s *Store) stampSchemaVersionContext(ctx context.Context) error {
	var err error
	if s.IsPostgreSQL() {
		_, err = s.db.ExecContext(ctx, s.Rebind(`INSERT INTO archive_metadata (key, value) VALUES ('schema_version', ?)
   ON CONFLICT (key) DO UPDATE SET value = excluded.value`), strconv.Itoa(SchemaVersion))
	} else {
		_, err = s.db.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", SchemaVersion))
	}
	if err != nil {
		return fmt.Errorf("publish archive schema version: %w", err)
	}
	return nil
}
