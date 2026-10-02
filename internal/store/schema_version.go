package store

import (
	"context"
	"fmt"
	"strconv"
)

// SchemaVersion is the main archive schema contract. Increase it whenever the
// archive schema or a required migration changes; TestSchemaVersionContract
// catches most missed bumps. API, cache and optional vector backend versions are
// independent. Zero identifies pre-contract archives.
const SchemaVersion = 1

// schemaVersionMarkerKey holds the PostgreSQL copy of SchemaVersion, which
// SQLite keeps in PRAGMA user_version.
const schemaVersionMarkerKey = "schema_version"

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
		value, ok, err := s.GetArchiveMarker(ctx, schemaVersionMarkerKey)
		if err != nil || !ok {
			return 0, err
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
	if s.IsPostgreSQL() {
		return s.SetArchiveMarker(ctx, schemaVersionMarkerKey, strconv.Itoa(SchemaVersion))
	}
	if _, err := s.db.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", SchemaVersion)); err != nil {
		return fmt.Errorf("publish archive schema version: %w", err)
	}
	return nil
}
