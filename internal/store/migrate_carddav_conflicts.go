package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

const cardDAVConflictPendingInvariant = `pending_operation IS NULL OR
	(status = 'unresolved' AND local_tombstone AND remote_etag IS NOT NULL AND
	 connection_generation IS NOT NULL AND book_sync_revision IS NOT NULL AND
	 previous_mapping_revision IS NOT NULL AND pending_started_at IS NOT NULL)`

// ensureCardDAVConflictPendingInvariant finishes the legacy conflict-table
// upgrade after the dialect's ADD COLUMN statements. PostgreSQL can add the
// table constraint in place. SQLite cannot, so an old table is rebuilt in one
// transaction with all rows and the two public indexes restored before commit.
func (s *Store) ensureCardDAVConflictPendingInvariant(ctx context.Context) error {
	if s.IsPostgreSQL() {
		var present bool
		if err := s.db.QueryRowContext(ctx, `SELECT EXISTS (
			SELECT 1 FROM pg_constraint
			WHERE conrelid = 'carddav_conflicts'::regclass
			  AND contype = 'c'
			  AND pg_get_constraintdef(oid) ILIKE '%pending_started_at IS NOT NULL%'
		)`).Scan(&present); err != nil {
			return fmt.Errorf("inspect PostgreSQL pending constraint: %w", err)
		}
		if present {
			return nil
		}
		if _, err := s.db.ExecContext(ctx, `DO $migration$
			BEGIN
				ALTER TABLE carddav_conflicts
					ADD CONSTRAINT carddav_conflicts_pending_invariant CHECK (`+
			cardDAVConflictPendingInvariant+`);
			EXCEPTION WHEN duplicate_object THEN NULL;
			END;
		$migration$;`); err != nil {
			return fmt.Errorf("add PostgreSQL pending constraint: %w", err)
		}
		return nil
	}

	var tableSQL sql.NullString
	if err := s.db.QueryRowContext(ctx, `SELECT sql FROM sqlite_master
		WHERE type = 'table' AND name = 'carddav_conflicts'`).Scan(&tableSQL); err != nil {
		return fmt.Errorf("inspect SQLite conflict table: %w", err)
	}
	if tableSQL.Valid && strings.Contains(strings.ToLower(tableSQL.String),
		"pending_started_at is not null") {
		return nil
	}

	return s.withTxContext(ctx, func(tx *loggedTx) error {
		if _, err := tx.ExecContext(ctx, `DROP TABLE IF EXISTS carddav_conflicts_pending_upgrade`); err != nil {
			return fmt.Errorf("clear stale SQLite conflict upgrade table: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `CREATE TABLE carddav_conflicts_pending_upgrade (
			id                         INTEGER PRIMARY KEY AUTOINCREMENT,
			address_book_id            INTEGER NOT NULL REFERENCES carddav_address_books(id) ON DELETE CASCADE,
			href                       TEXT NOT NULL,
			base_local_hash            TEXT NOT NULL,
			local_hash                 TEXT NOT NULL,
			base_remote_hash           TEXT NOT NULL,
			base_remote_etag           TEXT NOT NULL,
			remote_etag                TEXT,
			mapping_revision           INTEGER NOT NULL CHECK (mapping_revision > 0),
			local_body                 BLOB,
			remote_body                BLOB,
			local_tombstone            BOOLEAN NOT NULL DEFAULT FALSE,
			remote_tombstone           BOOLEAN NOT NULL DEFAULT FALSE,
			pending_operation          TEXT CHECK (pending_operation IN ('delete')),
			connection_generation      INTEGER,
			book_sync_revision         INTEGER,
			previous_mapping_revision  INTEGER,
			pending_started_at         DATETIME,
			status                     TEXT NOT NULL DEFAULT 'unresolved' CHECK (status IN ('unresolved', 'resolved')),
			resolution                 TEXT CHECK (resolution IN ('keep_local', 'keep_remote')),
			resolved_at                DATETIME,
			created_at                 DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at                 DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			CHECK (local_tombstone OR local_body IS NOT NULL),
			CHECK (remote_tombstone OR remote_body IS NOT NULL),
			CONSTRAINT carddav_conflicts_pending_invariant CHECK (`+
			cardDAVConflictPendingInvariant+`),
			CHECK ((status = 'unresolved' AND resolution IS NULL AND resolved_at IS NULL) OR
			       (status = 'resolved' AND resolution IS NOT NULL AND resolved_at IS NOT NULL))
		)`); err != nil {
			return fmt.Errorf("create SQLite conflict upgrade table: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO carddav_conflicts_pending_upgrade (
			id, address_book_id, href, base_local_hash, local_hash, base_remote_hash,
			base_remote_etag, remote_etag, mapping_revision, local_body, remote_body,
			local_tombstone, remote_tombstone, pending_operation, connection_generation,
			book_sync_revision, previous_mapping_revision, pending_started_at, status,
			resolution, resolved_at, created_at, updated_at
		) SELECT
			id, address_book_id, href, base_local_hash, local_hash, base_remote_hash,
			base_remote_etag, remote_etag, mapping_revision, local_body, remote_body,
			local_tombstone, remote_tombstone, pending_operation, connection_generation,
			book_sync_revision, previous_mapping_revision, pending_started_at, status,
			resolution, resolved_at, created_at, updated_at
		FROM carddav_conflicts`); err != nil {
			return fmt.Errorf("copy SQLite conflict rows: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `DROP TABLE carddav_conflicts`); err != nil {
			return fmt.Errorf("replace SQLite conflict table: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `ALTER TABLE carddav_conflicts_pending_upgrade
			RENAME TO carddav_conflicts`); err != nil {
			return fmt.Errorf("rename SQLite conflict table: %w", err)
		}
		for _, statement := range []string{
			`CREATE UNIQUE INDEX idx_carddav_one_unresolved_conflict
			 ON carddav_conflicts(address_book_id, href) WHERE status = 'unresolved'`,
			`CREATE INDEX idx_carddav_conflicts_resolved_at
			 ON carddav_conflicts(status, resolved_at)`,
		} {
			if _, err := tx.ExecContext(ctx, statement); err != nil {
				return fmt.Errorf("restore SQLite conflict index: %w", err)
			}
		}
		return nil
	})
}
