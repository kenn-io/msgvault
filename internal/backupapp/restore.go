package backupapp

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"go.kenn.io/kit/backup"
	"go.kenn.io/msgvault/internal/sqliteutil"
)

// InvalidateRestoredDocumentVectors removes document-vector authority because vectors.db
// is deliberately excluded from snapshots. Consent and historical provider
// usage remain valid archive records; the next vector run rebuilds the derived
// generation from normalized document evidence.
func InvalidateRestoredDocumentVectors(ctx context.Context, target backup.RestorePublicationTarget) (err error) {
	if target.DBPath == "" {
		return errors.New("backupapp: restored database path is required")
	}
	db, err := sql.Open(sqliteutil.DriverName(), target.DBPath)
	if err != nil {
		return fmt.Errorf("backupapp: open restored database: %w", err)
	}
	db.SetMaxOpenConns(1)
	defer func() {
		if closeErr := db.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("backupapp: close restored database: %w", closeErr))
		}
	}()
	for _, statement := range []string{
		`PRAGMA busy_timeout = 30000`,
		`PRAGMA foreign_keys = ON`,
		`PRAGMA journal_mode = DELETE`,
	} {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("backupapp: configure restored database: %w", err)
		}
	}

	var vectorAuthorityExists bool
	if err := db.QueryRowContext(ctx, `SELECT EXISTS (
		SELECT 1 FROM sqlite_master
		WHERE type = 'table' AND name = 'document_vector_generations'
	)`).Scan(&vectorAuthorityExists); err != nil {
		return fmt.Errorf("backupapp: inspect restored document vector authority: %w", err)
	}
	if !vectorAuthorityExists {
		return nil
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("backupapp: begin restored document vector invalidation: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, statement := range []string{
		`DELETE FROM document_vector_publications`,
		`DELETE FROM document_vector_build_progress`,
		`DELETE FROM document_vector_generations`,
	} {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("backupapp: invalidate restored document vectors: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("backupapp: commit restored document vector invalidation: %w", err)
	}
	return nil
}
