package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

const (
	SyncStatusRunning   = "running"
	SyncStatusCompleted = "completed"
	SyncStatusFailed    = "failed"
)

// ErrSyncRunNotFound is returned by the sync-run getters (GetActiveSync,
// GetLatestCheckpointedSync, GetLastSuccessfulSync) when no matching run
// exists. Wrapped via fmt.Errorf so callers can use errors.Is to tell
// absence apart from real DB errors.
var ErrSyncRunNotFound = errors.New("sync run not found")

// ErrSourceImportItemNotFound is returned by GetSourceImportItem when no
// import-item row matches. Wrapped via fmt.Errorf for errors.Is checks.
var ErrSourceImportItemNotFound = errors.New("source import item not found")

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
	Scan(dest ...any) error
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

// requireNullTime extracts a non-NULL time.Time from a sql.NullTime, with
// a clear error mentioning the field name. Required timestamps
// (created_at, updated_at, started_at) violate a schema invariant if NULL,
// so this surfaces the violation rather than silently zero-valuing.
func requireNullTime(nt sql.NullTime, field string) (time.Time, error) {
	if !nt.Valid {
		return time.Time{}, fmt.Errorf("%s: required timestamp is NULL", field)
	}
	return nt.Time, nil
}

func scanSource(sc scanner) (*Source, error) {
	// Scan timestamps into sql.NullTime / time.Time. The pgx/v5 stdlib
	// driver decodes TIMESTAMP/TIMESTAMPTZ as time.Time at the driver
	// level and refuses to convert that to *string; go-sqlite3 also
	// accepts time.Time destinations and parses its stored formats
	// internally, so a single typed scan path works for both backends.
	// Required fields are scanned through sql.NullTime so a NULL value
	// (a schema invariant violation) is reported with field context
	// rather than the driver's opaque "unsupported Scan" error.
	var source Source
	var createdAt, updatedAt sql.NullTime
	err := sc.Scan(
		&source.ID, &source.SourceType, &source.Identifier, &source.DisplayName,
		&source.GoogleUserID, &source.LastSyncAt, &source.SyncCursor, &source.SyncConfig,
		&source.OAuthApp, &createdAt, &updatedAt,
	)
	if err != nil {
		return nil, err
	}
	source.CreatedAt, err = requireNullTime(createdAt, "created_at")
	if err != nil {
		return nil, fmt.Errorf("source %d: %w", source.ID, err)
	}
	source.UpdatedAt, err = requireNullTime(updatedAt, "updated_at")
	if err != nil {
		return nil, fmt.Errorf("source %d: %w", source.ID, err)
	}
	return &source, nil
}

func scanSyncRun(sc scanner) (*SyncRun, error) {
	// Scan timestamps into typed columns — see scanSource for the
	// dialect-portability rationale.
	var run SyncRun
	var startedAt sql.NullTime
	err := sc.Scan(
		&run.ID, &run.SourceID, &startedAt, &run.CompletedAt, &run.Status,
		&run.MessagesProcessed, &run.MessagesAdded, &run.MessagesUpdated, &run.ErrorsCount,
		&run.ErrorMessage, &run.CursorBefore, &run.CursorAfter,
	)
	if err != nil {
		return nil, err
	}
	run.StartedAt, err = requireNullTime(startedAt, "started_at")
	if err != nil {
		return nil, fmt.Errorf("sync_run %d: %w", run.ID, err)
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

type SourceImportItem struct {
	ID              int64
	SourceID        int64
	Provider        string
	ProviderID      string
	Name            string
	Checksum        string
	Size            int64
	ModifiedAt      sql.NullTime
	ImportedAt      sql.NullTime
	Status          string
	RecordsImported int
	ErrorMessage    sql.NullString
}

// StartSync creates a new sync run record and returns its ID. The
// supersede UPDATE and the INSERT run inside a writer-locked
// transaction so concurrent StartSync calls cannot both find no
// running rows, both INSERT, and leave two 'running' rows alive.
// SQLite uses BEGIN IMMEDIATE; PostgreSQL takes a row lock on the
// source via SELECT ... FOR UPDATE before doing the read-modify-write
// on sync_runs.
func (s *Store) StartSync(sourceID int64, syncType string) (int64, error) {
	ctx := context.Background()
	const maxAttempts = 5
	for range maxAttempts {
		id, err := s.startSyncOnce(ctx, sourceID)
		if err == nil {
			return id, nil
		}
		if !s.dialect.IsBusyError(err) {
			return 0, err
		}
	}
	return 0, fmt.Errorf("start sync: gave up after %d retries on busy", maxAttempts)
}

func (s *Store) startSyncOnce(ctx context.Context, sourceID int64) (retID int64, retErr error) {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return 0, fmt.Errorf("acquire connection: %w", err)
	}
	defer func() { _ = conn.Close() }()

	if _, err := conn.ExecContext(ctx, s.dialect.BeginWriteSQL()); err != nil {
		return 0, fmt.Errorf("begin write tx: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(ctx, "ROLLBACK")
		}
	}()

	rebind := s.dialect.Rebind
	now := s.dialect.Now()

	// Serialize against concurrent StartSync for the same source.
	// SQLite already serializes writers under BEGIN IMMEDIATE; PG
	// needs an explicit row lock on the source so the read snapshot
	// for the UPDATE below cannot miss a concurrently committed
	// running run.
	var lockedID int64
	if err := conn.QueryRowContext(ctx,
		rebind(`SELECT id FROM sources WHERE id = ?`+s.dialect.SelectForUpdate()),
		sourceID,
	).Scan(&lockedID); err != nil {
		return 0, fmt.Errorf("lock source row: %w", err)
	}

	if _, err := conn.ExecContext(ctx,
		rebind(fmt.Sprintf(`
			UPDATE sync_runs
			SET status = 'failed',
			    error_message = 'superseded by new sync',
			    completed_at = %s
			WHERE source_id = ? AND status = 'running'
		`, now)),
		sourceID,
	); err != nil {
		return 0, fmt.Errorf("mark old syncs failed: %w", err)
	}

	var syncRunID int64
	if err := conn.QueryRowContext(ctx,
		rebind(fmt.Sprintf(`
			INSERT INTO sync_runs (source_id, started_at, status, messages_processed, messages_added, messages_updated, errors_count)
			VALUES (?, %s, 'running', 0, 0, 0, 0)
			RETURNING id
		`, now)),
		sourceID,
	).Scan(&syncRunID); err != nil {
		return 0, fmt.Errorf("insert sync_run: %w", err)
	}

	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return 0, fmt.Errorf("commit: %w", err)
	}
	committed = true
	return syncRunID, nil
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
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("active sync for source %d: %w", sourceID, ErrSyncRunNotFound)
	}
	return run, err
}

// GetLatestCheckpointedSync returns the most recent sync run for a source if
// (and only if) that latest run is running or failed and has a non-empty
// cursor_before. A completed run after a failed one means the failed run's
// checkpoint is stale: re-importing must re-scan all threads, so we return
// no row in that case.
func (s *Store) GetLatestCheckpointedSync(sourceID int64) (*SyncRun, error) {
	row := s.db.QueryRow(`
		SELECT id, source_id, started_at, completed_at, status,
		       messages_processed, messages_added, messages_updated, errors_count,
		       error_message, cursor_before, cursor_after
		FROM sync_runs
		WHERE source_id = ?
		  AND id = (SELECT MAX(id) FROM sync_runs WHERE source_id = ?)
		  AND status IN ('running', 'failed')
		  AND cursor_before IS NOT NULL AND cursor_before != ''
	`, sourceID, sourceID)

	run, err := scanSyncRun(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("latest checkpointed sync for source %d: %w", sourceID, ErrSyncRunNotFound)
	}
	return run, err
}

func (s *Store) UpsertSourceImportItem(item SourceImportItem) error {
	_, err := s.db.Exec(fmt.Sprintf(`
		INSERT INTO source_import_items (
			source_id, provider, provider_id, name, checksum, size, modified_at,
			imported_at, status, records_imported, error_message, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, %s, %s)
		ON CONFLICT(source_id, provider, provider_id) DO UPDATE SET
			name = excluded.name,
			checksum = excluded.checksum,
			size = excluded.size,
			modified_at = excluded.modified_at,
			imported_at = excluded.imported_at,
			status = excluded.status,
			records_imported = excluded.records_imported,
			error_message = excluded.error_message,
			updated_at = %s
	`, s.dialect.Now(), s.dialect.Now(), s.dialect.Now()),
		item.SourceID, item.Provider, item.ProviderID, item.Name, item.Checksum,
		item.Size, item.ModifiedAt, item.ImportedAt, item.Status,
		item.RecordsImported, item.ErrorMessage)
	if err != nil {
		return fmt.Errorf("upsert source import item: %w", err)
	}
	return nil
}

func (s *Store) GetSourceImportItem(sourceID int64, provider, providerID string) (*SourceImportItem, error) {
	var item SourceImportItem
	err := s.db.QueryRow(`
		SELECT id, source_id, provider, provider_id, name, checksum, size,
		       modified_at, imported_at, status, records_imported, error_message
		FROM source_import_items
		WHERE source_id = ? AND provider = ? AND provider_id = ?
	`, sourceID, provider, providerID).Scan(
		&item.ID, &item.SourceID, &item.Provider, &item.ProviderID, &item.Name,
		&item.Checksum, &item.Size, &item.ModifiedAt, &item.ImportedAt,
		&item.Status, &item.RecordsImported, &item.ErrorMessage,
	)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("source import item %s/%s for source %d: %w", provider, providerID, sourceID, ErrSourceImportItemNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("get source import item: %w", err)
	}
	return &item, nil
}

func (s *Store) ListImportedSourceItemChecksums(sourceID int64, provider string) (map[string]string, error) {
	rows, err := s.db.Query(`
		SELECT provider_id, checksum
		FROM source_import_items
		WHERE source_id = ? AND provider = ? AND status = 'imported'
	`, sourceID, provider)
	if err != nil {
		return nil, fmt.Errorf("list imported source import items: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]string{}
	for rows.Next() {
		var providerID, checksum string
		if err := rows.Scan(&providerID, &checksum); err != nil {
			return nil, fmt.Errorf("scan source import item checksum: %w", err)
		}
		out[providerID] = checksum
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate source import item checksums: %w", err)
	}
	return out, nil
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
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("last successful sync for source %d: %w", sourceID, ErrSyncRunNotFound)
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
// Concurrent first-inserts converge via INSERT ... ON CONFLICT DO UPDATE
// RETURNING: the no-op SET fires RETURNING on both the insert and
// conflict path so the second caller receives the existing row's
// fields instead of a unique-violation error.
func (s *Store) GetOrCreateSource(sourceType, identifier string) (*Source, error) {
	now := s.dialect.Now()
	row := s.db.QueryRow(fmt.Sprintf(`
		INSERT INTO sources (source_type, identifier, created_at, updated_at)
		VALUES (?, ?, %s, %s)
		ON CONFLICT (source_type, identifier) DO UPDATE
		SET identifier = sources.identifier
		RETURNING id, source_type, identifier, display_name, google_user_id,
		          last_sync_at, sync_cursor, sync_config, oauth_app,
		          created_at, updated_at
	`, now, now), sourceType, identifier)

	source, err := scanSource(row)
	if err != nil {
		return nil, fmt.Errorf("upsert source: %w", err)
	}

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
		s.dialect.InsertOrIgnore(
			`INSERT OR IGNORE INTO collection_sources (collection_id, source_id)
			 SELECT id, ? FROM collections WHERE name = ?`,
		),
		source.ID, DefaultCollectionName,
	); err != nil {
		slog.Warn("failed to add source to default collection (self-heals on next InitSchema)",
			"source_id", source.ID,
			"identifier", identifier,
			"error", err,
		)
	}

	return source, nil
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
// The sync_config column is JSONB on PG; the dialect supplies the
// appropriate placeholder cast (?::JSONB on PG, bare ? on SQLite).
func (s *Store) UpdateSourceSyncConfig(sourceID int64, configJSON string) error {
	_, err := s.db.Exec(fmt.Sprintf(`
		UPDATE sources
		SET sync_config = %s, updated_at = %s
		WHERE id = ?
	`, s.dialect.JSONBindExpr(), s.dialect.Now()), configJSON, sourceID)
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
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("source %q: %w", identifier, ErrSourceNotFound)
	}
	if err != nil {
		return nil, err
	}

	return source, nil
}
