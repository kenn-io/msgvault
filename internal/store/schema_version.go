package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
)

// SchemaVersion identifies a completed InitSchemaContext. Increment it for any
// main archive schema change, including indexes, or required data migration.
// Runtime-only consumers require an exact match and never run migrations.
// TestSchemaVersionContract catches most missed bumps. API, cache and optional
// vector backend versions are independent.
const SchemaVersion = 3

// SchemaVersionContext reads the stored completion marker without migrating.
// A missing marker identifies a legacy archive and returns zero.
func (s *Store) SchemaVersionContext(ctx context.Context) (int, error) {
	var exists bool
	query := "SELECT EXISTS (SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = 'archive_metadata')"
	if s.IsPostgreSQL() {
		query = "SELECT to_regclass('archive_metadata') IS NOT NULL"
	}
	if err := s.db.QueryRowContext(ctx, query).Scan(&exists); err != nil {
		return 0, err
	}
	if !exists {
		return 0, nil
	}
	var value string
	err := s.db.QueryRowContext(ctx, "SELECT value FROM archive_metadata WHERE key = 'schema_version'").Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	version, err := strconv.Atoi(value)
	if err != nil || version <= 0 {
		return 0, fmt.Errorf("invalid archive schema version %q", value)
	}
	return version, nil
}

// ValidateSchemaContext performs read-only readiness checks. Only setup runs
// InitSchemaContext; callers with a runtime role must never try to repair here.
func (s *Store) ValidateSchemaContext(ctx context.Context) error {
	version, err := s.SchemaVersionContext(ctx)
	if err != nil {
		return err
	}
	if version != SchemaVersion {
		return fmt.Errorf("archive schema version %d does not match %d; run setup with the matching release", version, SchemaVersion)
	}
	uid, err := s.ArchiveUIDContext(ctx)
	if err != nil {
		return err
	}
	if uid == "" {
		return ErrArchiveIdentityCorrupt
	}
	return nil
}
