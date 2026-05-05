package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ErrSourceNotFound is returned by GetSourceByID when no source row
// matches the given ID. Wrapped via fmt.Errorf("...: %w", ...) so
// callers can use errors.Is to distinguish absence from real DB
// errors.
var ErrSourceNotFound = errors.New("source not found")

// GetSourceByID returns the source with the given ID, or
// ErrSourceNotFound (wrapped) if no row matches.
func (s *Store) GetSourceByID(id int64) (*Source, error) {
	row := s.db.QueryRow(`
		SELECT id, source_type, identifier, display_name, google_user_id,
		       last_sync_at, sync_cursor, sync_config, oauth_app,
		       created_at, updated_at
		FROM sources
		WHERE id = ?
	`, id)

	source, err := scanSource(row)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("source %d: %w", id, ErrSourceNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("get source by id: %w", err)
	}
	return source, nil
}

// GetSourcesByIdentifier returns all sources matching an identifier,
// regardless of source_type. Use this when the identifier may be
// shared across source types (e.g., gmail + mbox import).
func (s *Store) GetSourcesByIdentifier(
	identifier string,
) ([]*Source, error) {
	rows, err := s.db.Query(`
		SELECT id, source_type, identifier, display_name,
		       google_user_id, last_sync_at, sync_cursor, sync_config,
		       oauth_app, created_at, updated_at
		FROM sources
		WHERE identifier = ?
		ORDER BY source_type
	`, identifier)
	if err != nil {
		return nil, fmt.Errorf("query sources: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var sources []*Source
	for rows.Next() {
		src, err := scanSource(rows)
		if err != nil {
			return nil, fmt.Errorf("scan source: %w", err)
		}
		sources = append(sources, src)
	}
	return sources, rows.Err()
}

// GetSourcesByIdentifierOrDisplayName returns all sources whose identifier or
// display_name matches the given value. This is the preferred single-query
// lookup when resolving a user-supplied email or identifier string.
func (s *Store) GetSourcesByIdentifierOrDisplayName(query string) ([]*Source, error) {
	rows, err := s.db.Query(`
		SELECT id, source_type, identifier, display_name,
		       google_user_id, last_sync_at, sync_cursor, sync_config,
		       oauth_app, created_at, updated_at
		FROM sources
		WHERE identifier = ? OR display_name = ?
		ORDER BY source_type
	`, query, query)
	if err != nil {
		return nil, fmt.Errorf("query sources: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var sources []*Source
	for rows.Next() {
		src, err := scanSource(rows)
		if err != nil {
			return nil, fmt.Errorf("scan source: %w", err)
		}
		sources = append(sources, src)
	}
	return sources, rows.Err()
}

// GetSourcesByDisplayName returns all sources with the given display name.
// Use this as a fallback when looking up IMAP sources by their human-readable
// email address rather than the full imaps:// identifier.
// Note: display_name is not constrained to be unique — callers receive all
// matching rows if more than one source shares the same name.
func (s *Store) GetSourcesByDisplayName(displayName string) ([]*Source, error) {
	rows, err := s.db.Query(`
		SELECT id, source_type, identifier, display_name,
		       google_user_id, last_sync_at, sync_cursor, sync_config,
		       oauth_app, created_at, updated_at
		FROM sources
		WHERE display_name = ?
		ORDER BY source_type
	`, displayName)
	if err != nil {
		return nil, fmt.Errorf("query sources by display name: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var sources []*Source
	for rows.Next() {
		src, err := scanSource(rows)
		if err != nil {
			return nil, fmt.Errorf("scan source: %w", err)
		}
		sources = append(sources, src)
	}
	return sources, rows.Err()
}

// RemoveSource deletes a source and all its associated data.
// FTS5 rows are cleaned up explicitly (no FK cascade for virtual tables).
// CASCADE handles conversations, messages, labels, attachments, sync state.
// Orphaned participants are left for a future `gc` command.
func (s *Store) RemoveSource(sourceID int64) error {
	return s.withTx(func(tx *loggedTx) error {
		return s.removeSourceExec(tx, sourceID)
	})
}

// RemoveSourceSerialized deletes a source while holding an exclusive write
// lock, and reports whether any sync was running at the moment the lock was
// acquired. Callers use hadActiveSync to gate follow-up operations (such as
// attachment file deletion) that must not race with a sync worker.
//
// Running the active-sync check and the cascade in the same transaction
// closes the race where a sync starts between a pre-check and RemoveSource:
// StartSync blocks on our exclusive lock, so either (a) it committed before
// us and we observe the running row, or (b) it has not yet started and will
// fail after we commit because the source is gone.
func (s *Store) RemoveSourceSerialized(
	ctx context.Context, sourceID int64,
) (hadActiveSync bool, err error) {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return false, fmt.Errorf("acquire connection: %w", err)
	}
	defer func() { _ = conn.Close() }()

	if _, err := conn.ExecContext(ctx, "BEGIN EXCLUSIVE"); err != nil {
		return false, fmt.Errorf("begin exclusive: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(ctx, "ROLLBACK")
		}
	}()

	var count int
	if err := conn.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sync_runs WHERE status = 'running'`,
	).Scan(&count); err != nil {
		return false, fmt.Errorf("check active syncs: %w", err)
	}
	hadActiveSync = count > 0

	if s.fts5Available {
		if _, err := conn.ExecContext(
			ctx, s.dialect.FTSDeleteSQL(), sourceID,
		); err != nil {
			return hadActiveSync, fmt.Errorf("delete FTS rows: %w", err)
		}
	}

	res, err := conn.ExecContext(
		ctx, `DELETE FROM sources WHERE id = ?`, sourceID,
	)
	if err != nil {
		return hadActiveSync, fmt.Errorf("delete source: %w", err)
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return hadActiveSync, fmt.Errorf("check rows affected: %w", err)
	}
	if rows == 0 {
		return hadActiveSync, fmt.Errorf("source %d not found", sourceID)
	}

	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return hadActiveSync, fmt.Errorf("commit: %w", err)
	}
	committed = true
	return hadActiveSync, nil
}

// removeSourceExec performs the FTS + sources DELETE on a generic executor
// (either a *loggedTx or *sql.Conn under a manual transaction).
func (s *Store) removeSourceExec(tx *loggedTx, sourceID int64) error {
	if s.fts5Available {
		if _, err := tx.Exec(s.dialect.FTSDeleteSQL(), sourceID); err != nil {
			return fmt.Errorf("delete FTS rows: %w", err)
		}
	}
	res, err := tx.Exec(`DELETE FROM sources WHERE id = ?`, sourceID)
	if err != nil {
		return fmt.Errorf("delete source: %w", err)
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("check rows affected: %w", err)
	}
	if rows == 0 {
		return fmt.Errorf("source %d not found", sourceID)
	}
	return nil
}
