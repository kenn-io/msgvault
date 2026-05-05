package store

import (
	"database/sql"
	"fmt"
	"log/slog"
	"time"
)

const (
	SyncStatusRunning   = "running"
	SyncStatusCompleted = "completed"
	SyncStatusFailed    = "failed"
)

// dbTimeLayouts lists formats used by SQLite/go-sqlite3 for timestamp storage.
// This matches the full set from SQLiteTimestampFormats in mattn/go-sqlite3,
// plus RFC3339/RFC3339Nano as fallbacks for maximum compatibility.
// The order matters: more specific formats (with fractional seconds/timezones) come first.
var dbTimeLayouts = []string{
	// Formats from mattn/go-sqlite3 SQLiteTimestampFormats
	"2006-01-02 15:04:05.999999999-07:00", // space-separated with fractional seconds and TZ
	"2006-01-02T15:04:05.999999999-07:00", // T-separated with fractional seconds and TZ
	"2006-01-02 15:04:05.999999999",       // space-separated with fractional seconds
	"2006-01-02T15:04:05.999999999",       // T-separated with fractional seconds
	"2006-01-02 15:04:05",                 // SQLite datetime('now') format
	"2006-01-02T15:04:05",                 // T-separated basic
	"2006-01-02 15:04",                    // space-separated without seconds
	"2006-01-02T15:04",                    // T-separated without seconds
	"2006-01-02",                          // date only
	// Additional fallback formats
	time.RFC3339,     // go-sqlite3 DATETIME column format (e.g., "2006-01-02T15:04:05Z")
	time.RFC3339Nano, // RFC3339 with nanoseconds (e.g., "2006-01-02T15:04:05.999999999Z07:00")
}

// scanner is satisfied by both *sql.Row and *sql.Rows.
type scanner interface {
	Scan(dest ...interface{}) error
}

// parseDBTime attempts to parse a timestamp string using known SQLite/go-sqlite3 formats.
func parseDBTime(s string) (time.Time, error) {
	for _, layout := range dbTimeLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("unrecognized timestamp format %q", s)
}

func parseNullTime(ns sql.NullString) (sql.NullTime, error) {
	if !ns.Valid {
		return sql.NullTime{}, nil
	}
	t, err := parseDBTime(ns.String)
	if err != nil {
		return sql.NullTime{}, err
	}
	return sql.NullTime{Time: t, Valid: true}, nil
}

// parseRequiredTime parses a timestamp that must not be NULL.
// Use this for required fields like created_at/updated_at.
func parseRequiredTime(ns sql.NullString, field string) (time.Time, error) {
	if !ns.Valid {
		return time.Time{}, fmt.Errorf("%s: required timestamp is NULL", field)
	}
	t, err := parseDBTime(ns.String)
	if err != nil {
		return time.Time{}, fmt.Errorf("%s: %w", field, err)
	}
	return t, nil
}

func scanSource(sc scanner) (*Source, error) {
	var source Source
	var lastSyncAt, createdAt, updatedAt sql.NullString

	err := sc.Scan(
		&source.ID, &source.SourceType, &source.Identifier, &source.DisplayName,
		&source.GoogleUserID, &lastSyncAt, &source.SyncCursor, &source.SyncConfig,
		&source.OAuthApp, &createdAt, &updatedAt,
	)
	if err != nil {
		return nil, err
	}

	source.LastSyncAt, err = parseNullTime(lastSyncAt)
	if err != nil {
		return nil, fmt.Errorf("source %d: last_sync_at: %w", source.ID, err)
	}
	source.CreatedAt, err = parseRequiredTime(createdAt, "created_at")
	if err != nil {
		return nil, fmt.Errorf("source %d: %w", source.ID, err)
	}
	source.UpdatedAt, err = parseRequiredTime(updatedAt, "updated_at")
	if err != nil {
		return nil, fmt.Errorf("source %d: %w", source.ID, err)
	}

	return &source, nil
}

func scanSyncRun(sc scanner) (*SyncRun, error) {
	var run SyncRun
	var startedAt string
	var completedAt sql.NullString

	err := sc.Scan(
		&run.ID, &run.SourceID, &startedAt, &completedAt, &run.Status,
		&run.MessagesProcessed, &run.MessagesAdded, &run.MessagesUpdated, &run.ErrorsCount,
		&run.ErrorMessage, &run.CursorBefore, &run.CursorAfter,
	)
	if err != nil {
		return nil, err
	}

	run.StartedAt, err = parseDBTime(startedAt)
	if err != nil {
		return nil, fmt.Errorf("sync_run %d: parse started_at %q: %w", run.ID, startedAt, err)
	}
	run.CompletedAt, err = parseNullTime(completedAt)
	if err != nil {
		return nil, fmt.Errorf("sync_run %d: completed_at: %w", run.ID, err)
	}

	return &run, nil
}

// SyncRun represents a sync operation in progress or completed.
type SyncRun struct {
	ID                int64
	SourceID          int64
	StartedAt         time.Time
	CompletedAt       sql.NullTime
	Status            string // SyncStatusRunning, SyncStatusCompleted, SyncStatusFailed
	MessagesProcessed int64
	MessagesAdded     int64
	MessagesUpdated   int64
	ErrorsCount       int64
	ErrorMessage      sql.NullString
	CursorBefore      sql.NullString // Page token for resumption
	CursorAfter       sql.NullString // Final history ID
}

// Checkpoint represents sync progress for resumption.
type Checkpoint struct {
	PageToken         string
	MessagesProcessed int64
	MessagesAdded     int64
	MessagesUpdated   int64
	ErrorsCount       int64
}

// StartSync creates a new sync run record and returns its ID.
func (s *Store) StartSync(sourceID int64, syncType string) (int64, error) {
	now := s.dialect.Now()

	// Mark any existing running syncs as failed
	_, err := s.db.Exec(fmt.Sprintf(`
		UPDATE sync_runs
		SET status = 'failed',
		    error_message = 'superseded by new sync',
		    completed_at = %s
		WHERE source_id = ? AND status = 'running'
	`, now), sourceID)
	if err != nil {
		return 0, fmt.Errorf("mark old syncs failed: %w", err)
	}

	// Create new sync run
	result, err := s.db.Exec(fmt.Sprintf(`
		INSERT INTO sync_runs (source_id, started_at, status, messages_processed, messages_added, messages_updated, errors_count)
		VALUES (?, %s, 'running', 0, 0, 0, 0)
	`, now), sourceID)
	if err != nil {
		return 0, fmt.Errorf("insert sync_run: %w", err)
	}

	return result.LastInsertId()
}

// UpdateSyncCheckpoint saves progress for resumption.
func (s *Store) UpdateSyncCheckpoint(syncID int64, cp *Checkpoint) error {
	_, err := s.db.Exec(`
		UPDATE sync_runs
		SET cursor_before = ?,
		    messages_processed = ?,
		    messages_added = ?,
		    messages_updated = ?,
		    errors_count = ?
		WHERE id = ?
	`, cp.PageToken, cp.MessagesProcessed, cp.MessagesAdded, cp.MessagesUpdated, cp.ErrorsCount, syncID)
	return err
}

// CompleteSync marks a sync as successfully completed.
func (s *Store) CompleteSync(syncID int64, finalHistoryID string) error {
	_, err := s.db.Exec(fmt.Sprintf(`
		UPDATE sync_runs
		SET status = 'completed',
		    completed_at = %s,
		    cursor_after = ?
		WHERE id = ?
	`, s.dialect.Now()), finalHistoryID, syncID)
	return err
}

// FailSync marks a sync as failed with an error message.
func (s *Store) FailSync(syncID int64, errMsg string) error {
	_, err := s.db.Exec(fmt.Sprintf(`
		UPDATE sync_runs
		SET status = 'failed',
		    completed_at = %s,
		    error_message = ?
		WHERE id = ?
	`, s.dialect.Now()), errMsg, syncID)
	return err
}

// GetActiveSync returns the most recent running sync for a source, if any.
func (s *Store) GetActiveSync(sourceID int64) (*SyncRun, error) {
	row := s.db.QueryRow(`
		SELECT id, source_id, started_at, completed_at, status,
		       messages_processed, messages_added, messages_updated, errors_count,
		       error_message, cursor_before, cursor_after
		FROM sync_runs
		WHERE source_id = ? AND status = 'running'
		ORDER BY started_at DESC
		LIMIT 1
	`, sourceID)

	run, err := scanSyncRun(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return run, err
}

// HasAnyActiveSync returns true if any source currently has a running sync.
// Use this as a safety gate before performing destructive file operations that
// could race with concurrent attachment ingestion.
func (s *Store) HasAnyActiveSync() (bool, error) {
	var count int
	err := s.db.QueryRow(
		`SELECT COUNT(*) FROM sync_runs WHERE status = 'running'`,
	).Scan(&count)
	if err != nil {
		return true, err // fail safe
	}
	return count > 0, nil
}

// GetLastSuccessfulSync returns the most recent successful sync for a source.
func (s *Store) GetLastSuccessfulSync(sourceID int64) (*SyncRun, error) {
	row := s.db.QueryRow(`
		SELECT id, source_id, started_at, completed_at, status,
		       messages_processed, messages_added, messages_updated, errors_count,
		       error_message, cursor_before, cursor_after
		FROM sync_runs
		WHERE source_id = ? AND status = 'completed'
		ORDER BY completed_at DESC
		LIMIT 1
	`, sourceID)

	run, err := scanSyncRun(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return run, err
}

// Source represents a Gmail account or other message source.
type Source struct {
	ID           int64
	SourceType   string // "gmail" or "imap"
	Identifier   string // email address or IMAP identifier URL
	DisplayName  sql.NullString
	GoogleUserID sql.NullString
	LastSyncAt   sql.NullTime
	SyncCursor   sql.NullString // historyId for Gmail
	SyncConfig   sql.NullString // JSON config for IMAP sources
	OAuthApp     sql.NullString // named OAuth app binding (NULL = default)
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// GetOrCreateSource gets or creates a source by type and identifier.
func (s *Store) GetOrCreateSource(sourceType, identifier string) (*Source, error) {
	// Try to get existing
	row := s.db.QueryRow(`
		SELECT id, source_type, identifier, display_name, google_user_id,
		       last_sync_at, sync_cursor, sync_config, oauth_app,
		       created_at, updated_at
		FROM sources
		WHERE source_type = ? AND identifier = ?
	`, sourceType, identifier)

	source, err := scanSource(row)
	if err == nil {
		return source, nil
	}
	if err != sql.ErrNoRows {
		return nil, err
	}

	// Create new
	now := s.dialect.Now()
	result, err := s.db.Exec(fmt.Sprintf(`
		INSERT INTO sources (source_type, identifier, created_at, updated_at)
		VALUES (?, ?, %s, %s)
	`, now, now), sourceType, identifier)
	if err != nil {
		return nil, fmt.Errorf("insert source: %w", err)
	}

	newSource := &Source{
		SourceType: sourceType,
		Identifier: identifier,
		CreatedAt:  time.Now(),
		UpdatedAt:  time.Now(),
	}
	newSource.ID, _ = result.LastInsertId()

	// Add to the default "All" collection if it exists.
	//
	// This runs as a separate Exec rather than inside a transaction
	// with the source insert. If this Exec fails, the source row is
	// committed but the All membership is missing — and the next
	// EnsureDefaultCollection call (which runs in InitSchema on every
	// process launch) re-adds every source not yet linked. Self-heals
	// on next CLI invocation; until then collection-scoped reads of
	// All would miss this source. Acceptable for a single-user tool;
	// a future refactor can fold this into a withTx.
	if _, err := s.db.Exec(
		`INSERT OR IGNORE INTO collection_sources (collection_id, source_id)
		 SELECT id, ? FROM collections WHERE name = ?`,
		newSource.ID, DefaultCollectionName,
	); err != nil {
		slog.Warn("failed to add source to default collection (self-heals on next InitSchema)",
			"source_id", newSource.ID,
			"identifier", identifier,
			"error", err,
		)
	}

	return newSource, nil
}

// UpdateSourceSyncCursor updates the sync cursor (historyId) for a source.
func (s *Store) UpdateSourceSyncCursor(sourceID int64, cursor string) error {
	now := s.dialect.Now()
	_, err := s.db.Exec(fmt.Sprintf(`
		UPDATE sources
		SET sync_cursor = ?, last_sync_at = %s, updated_at = %s
		WHERE id = ?
	`, now, now), cursor, sourceID)
	return err
}

// ListSources returns all sources, optionally filtered by source type.
// Pass an empty string to return all sources.
func (s *Store) ListSources(sourceType string) ([]*Source, error) {
	var rows *loggedRows
	var err error

	if sourceType != "" {
		rows, err = s.db.Query(`
			SELECT id, source_type, identifier, display_name, google_user_id,
			       last_sync_at, sync_cursor, sync_config, oauth_app,
			       created_at, updated_at
			FROM sources
			WHERE source_type = ?
			ORDER BY identifier
		`, sourceType)
	} else {
		rows, err = s.db.Query(`
			SELECT id, source_type, identifier, display_name, google_user_id,
			       last_sync_at, sync_cursor, sync_config, oauth_app,
			       created_at, updated_at
			FROM sources
			ORDER BY identifier
		`)
	}
	if err != nil {
		return nil, fmt.Errorf("list sources: %w", err)
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
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate sources: %w", err)
	}

	return sources, nil
}

// UpdateSourceDisplayName updates the display name for a source.
func (s *Store) UpdateSourceDisplayName(sourceID int64, displayName string) error {
	_, err := s.db.Exec(fmt.Sprintf(`
		UPDATE sources
		SET display_name = ?, updated_at = %s
		WHERE id = ?
	`, s.dialect.Now()), displayName, sourceID)
	return err
}

// UpdateSourceSyncConfig updates the JSON sync configuration for an IMAP source.
func (s *Store) UpdateSourceSyncConfig(sourceID int64, configJSON string) error {
	_, err := s.db.Exec(fmt.Sprintf(`
		UPDATE sources
		SET sync_config = ?, updated_at = %s
		WHERE id = ?
	`, s.dialect.Now()), configJSON, sourceID)
	return err
}

// UpdateSourceIdentifier updates the identifier column for an existing source.
// Used by add-o365 to fix up the IMAP host when re-authorizing an account
// whose host classification changed (e.g. personal vs org scope correction).
func (s *Store) UpdateSourceIdentifier(sourceID int64, identifier string) error {
	_, err := s.db.Exec(fmt.Sprintf(`
		UPDATE sources
		SET identifier = ?, updated_at = %s
		WHERE id = ?
	`, s.dialect.Now()), identifier, sourceID)
	return err
}

// GetSourceByIdentifier returns a source by its identifier (email address).
func (s *Store) GetSourceByIdentifier(identifier string) (*Source, error) {
	row := s.db.QueryRow(`
		SELECT id, source_type, identifier, display_name, google_user_id,
		       last_sync_at, sync_cursor, sync_config, oauth_app,
		       created_at, updated_at
		FROM sources
		WHERE identifier = ?
	`, identifier)

	source, err := scanSource(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	return source, nil
}
