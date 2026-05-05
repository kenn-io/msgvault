// Package store provides database access for msgvault.
package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/mattn/go-sqlite3"
)

//go:embed schema.sql schema_sqlite.sql schema_pg.sql
var schemaFS embed.FS

// Store provides database operations for msgvault.
//
// The db field wraps a *sql.DB with a thin logging adapter that
// emits slog records for every Query / Exec / QueryRow call.
// Because loggedDB embeds *sql.DB and overrides the instrumented
// methods, existing store code that does s.db.Query(...) compiles
// unchanged and automatically routes through the logger.
type Store struct {
	db            *loggedDB
	dbPath        string
	dialect       Dialect
	readOnly      bool // Opened via OpenReadOnly; skips WAL checkpoint on close
	fts5Available bool // Whether FTS5 is available for full-text search
	closeCleanup  func()
}

const defaultSQLiteParams = "?_journal_mode=WAL&_busy_timeout=30000&_synchronous=NORMAL&_foreign_keys=ON"

// isSQLiteError checks if err is a sqlite3.Error with a message containing substr.
// This is more robust than strings.Contains on err.Error() because it first
// type-asserts to the specific driver error type using errors.As.
// Handles both value (sqlite3.Error) and pointer (*sqlite3.Error) forms.
//
// SQLiteDialect's error predicates are thin wrappers around this helper; it also
// services subset.go (which has not been migrated to Dialect).
func isSQLiteError(err error, substr string) bool {
	var sqliteErr sqlite3.Error
	if errors.As(err, &sqliteErr) {
		return strings.Contains(sqliteErr.Error(), substr)
	}
	var sqliteErrPtr *sqlite3.Error
	if errors.As(err, &sqliteErrPtr) && sqliteErrPtr != nil {
		return strings.Contains(sqliteErrPtr.Error(), substr)
	}
	return false
}

// isPostgresURL returns true if the path looks like a PostgreSQL connection URL.
func isPostgresURL(dbPath string) bool {
	return strings.HasPrefix(dbPath, "postgresql://") || strings.HasPrefix(dbPath, "postgres://")
}

// Open opens or creates the database at the given path.
// If dbPath is a postgres:// or postgresql:// URL, opens a PostgreSQL connection.
// Otherwise, opens a SQLite database at the file path.
func Open(dbPath string) (*Store, error) {
	if isPostgresURL(dbPath) {
		return openPostgres(dbPath)
	}
	return openSQLite(dbPath)
}

// openSQLite opens a SQLite database at the given file path.
func openSQLite(dbPath string) (*Store, error) {
	// Ensure directory exists (skip for in-memory databases)
	if dbPath != ":memory:" && !strings.Contains(dbPath, ":memory:") {
		dir := filepath.Dir(dbPath)
		if err := os.MkdirAll(dir, 0755); err != nil {
			return nil, fmt.Errorf("create db directory: %w", err)
		}
	}

	dsn := dbPath + defaultSQLiteParams
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}

	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}

	// SQLite with WAL supports one writer + multiple readers.
	// Allow enough connections for concurrent reads (TUI async
	// queries, FTS backfill) while SQLite handles write serialization.
	// Exception: :memory: databases are per-connection, so multiple
	// connections would create separate databases.
	if dbPath == ":memory:" || strings.Contains(dbPath, ":memory:") {
		db.SetMaxOpenConns(1)
	} else {
		db.SetMaxOpenConns(4)
	}

	dialect := &SQLiteDialect{}
	if err := dialect.InitConn(db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("init connection: %w", err)
	}

	return &Store{
		db:      newLoggedDB(db, dialect.Rebind),
		dbPath:  dbPath,
		dialect: dialect,
	}, nil
}

// openPostgres opens a PostgreSQL database using the given connection URL.
func openPostgres(dbURL string) (*Store, error) {
	db, cleanup, err := openPostgresDB(dbURL, false)
	if err != nil {
		return nil, err
	}

	if err := db.Ping(); err != nil {
		_ = db.Close()
		cleanup()
		return nil, fmt.Errorf("ping PostgreSQL: %w", err)
	}

	// PostgreSQL supports full concurrency — use a larger pool than SQLite.
	db.SetMaxOpenConns(25)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(5 * time.Minute)

	dialect := &PostgreSQLDialect{}
	if err := dialect.InitConn(db); err != nil {
		_ = db.Close()
		cleanup()
		return nil, fmt.Errorf("init PostgreSQL connection: %w", err)
	}

	return &Store{
		db:           newLoggedDB(db, dialect.Rebind),
		dbPath:       dbURL,
		dialect:      dialect,
		closeCleanup: cleanup,
	}, nil
}

// OpenReadOnly opens an existing database in read-only mode. Suitable for
// query-only workloads (MCP server) where multiple processes access the
// same database concurrently. Does not create the database, run migrations,
// or checkpoint WAL on close.
func OpenReadOnly(dbPath string) (*Store, error) {
	if isPostgresURL(dbPath) {
		return openPostgresReadOnly(dbPath)
	}

	if _, err := os.Stat(dbPath); err != nil {
		return nil, fmt.Errorf(
			"database not found: %s "+
				"(run 'msgvault init-db' first)", dbPath,
		)
	}

	// Use _query_only instead of mode=ro. WAL-mode databases may need
	// to create or update -wal/-shm sidecar files on open, which fails
	// under SQLITE_OPEN_READONLY. _query_only opens normally (so SQLite
	// can manage sidecars) but rejects all write SQL at the query layer.
	dsn := dbPath + "?_query_only=true&_busy_timeout=5000"
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, fmt.Errorf("open database (read-only): %w", err)
	}

	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}

	db.SetMaxOpenConns(4)

	dialect := &SQLiteDialect{}
	if err := dialect.InitConn(db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("init connection: %w", err)
	}

	s := &Store{
		db:       newLoggedDB(db, dialect.Rebind),
		dbPath:   dbPath,
		dialect:  dialect,
		readOnly: true,
	}

	s.fts5Available = dialect.FTSAvailable(db)

	return s, nil
}

// openPostgresReadOnly opens a PostgreSQL database in read-only mode.
//
// Read-only enforcement uses pgx's RuntimeParams so that
// default_transaction_read_only=on is sent in the startup packet of every
// connection in the pool, not just the first one. Setting it via
// `db.Exec("SET ...")` on a pooled *sql.DB only affects whichever connection
// happened to serve the Exec — subsequent operations on a different pooled
// connection would run as writable.
func openPostgresReadOnly(dbURL string) (*Store, error) {
	db, cleanup, err := openPostgresDB(dbURL, true)
	if err != nil {
		return nil, err
	}

	if err := db.Ping(); err != nil {
		_ = db.Close()
		cleanup()
		return nil, fmt.Errorf("ping PostgreSQL: %w", err)
	}

	db.SetMaxOpenConns(25)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(5 * time.Minute)

	dialect := &PostgreSQLDialect{}
	if err := dialect.InitConn(db); err != nil {
		_ = db.Close()
		cleanup()
		return nil, fmt.Errorf("init PostgreSQL connection: %w", err)
	}

	s := &Store{
		db:           newLoggedDB(db, dialect.Rebind),
		dbPath:       dbURL,
		dialect:      dialect,
		readOnly:     true,
		closeCleanup: cleanup,
	}

	s.fts5Available = dialect.FTSAvailable(db)

	return s, nil
}

func postgresConnConfig(dbURL string, readOnly bool) (*pgx.ConnConfig, error) {
	connConfig, err := pgx.ParseConfig(dbURL)
	if err != nil {
		return nil, fmt.Errorf("parse PostgreSQL URL: %w", err)
	}
	if connConfig.RuntimeParams == nil {
		connConfig.RuntimeParams = map[string]string{}
	}
	connConfig.RuntimeParams["statement_timeout"] = "30s"
	if readOnly {
		connConfig.RuntimeParams["default_transaction_read_only"] = "on"
	}
	return connConfig, nil
}

func openPostgresDB(dbURL string, readOnly bool) (*sql.DB, func(), error) {
	connConfig, err := postgresConnConfig(dbURL, readOnly)
	if err != nil {
		return nil, nil, err
	}

	dsn := stdlib.RegisterConnConfig(connConfig)
	cleanup := func() { stdlib.UnregisterConnConfig(dsn) }
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("open PostgreSQL: %w", err)
	}
	return db, cleanup, nil
}

// Close checkpoints the WAL (unless read-only) and closes the database.
func (s *Store) Close() error {
	if !s.readOnly {
		// Checkpoint WAL before closing to fold it back into the main
		// database. This prevents WAL accumulation across sessions and
		// reduces the risk of corruption from stale WAL entries.
		_ = s.CheckpointWAL()
	}
	err := s.db.Close()
	if s.closeCleanup != nil {
		s.closeCleanup()
		s.closeCleanup = nil
	}
	return err
}

// CheckpointWAL forces a WAL checkpoint, folding the WAL back into the main
// database file. Uses TRUNCATE mode which also resets the WAL file to zero
// bytes. Returns nil on success; callers may log but should not fail on error.
// No-op for non-SQLite backends.
func (s *Store) CheckpointWAL() error {
	return s.dialect.CheckpointWAL(s.db.DB)
}

// DB returns the underlying *sql.DB for consumers that need to
// pass the raw handle elsewhere (e.g. the DuckDB engine's
// sqlite_scan wrapper). The wrapper's structured-logging
// behaviour is bypassed for those consumers — they're operating
// at a different abstraction layer.
func (s *Store) DB() *sql.DB {
	return s.db.DB
}

// WithExclusiveLock executes fn while holding an exclusive write lock on the
// database. In WAL mode this blocks concurrent writers (e.g. StartSync) while
// allowing reads (e.g. IsAttachmentPathReferenced) to proceed. Use this to
// serialize destructive file operations against concurrent sync attachment
// ingestion. The context controls both lock acquisition and the lifetime of
// the underlying connection; cancelling it aborts a pending BEGIN EXCLUSIVE
// and rolls back any held transaction.
func (s *Store) WithExclusiveLock(ctx context.Context, fn func() error) error {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("acquire connection: %w", err)
	}
	defer func() { _ = conn.Close() }()

	if _, err := conn.ExecContext(ctx, "BEGIN EXCLUSIVE"); err != nil {
		return fmt.Errorf("begin exclusive: %w", err)
	}

	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(ctx, "ROLLBACK")
		}
	}()

	if err := fn(); err != nil {
		return err
	}

	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return fmt.Errorf("commit exclusive: %w", err)
	}
	committed = true
	return nil
}

// withTx executes fn within a database transaction. If fn returns an error,
// the transaction is rolled back; otherwise it is committed. The callback
// receives *loggedTx so every statement inside the transaction goes through
// the dialect's Rebind automatically.
func (s *Store) withTx(fn func(tx *loggedTx) error) error {
	start := time.Now()
	slog.Debug("sql tx begin")
	tx, err := s.db.Begin()
	if err != nil {
		slog.Warn("sql tx begin failed", "error", err.Error())
		return fmt.Errorf("begin tx: %w", err)
	}
	if err := fn(tx); err != nil {
		if rbErr := tx.Rollback(); rbErr != nil {
			slog.Warn("sql tx rollback failed",
				"error", rbErr.Error(),
				"fn_error", err.Error(),
				"duration_ms", time.Since(start).Milliseconds())
		} else {
			slog.Info("sql tx rollback",
				"reason", err.Error(),
				"duration_ms", time.Since(start).Milliseconds())
		}
		return err
	}
	if err := tx.Commit(); err != nil {
		slog.Warn("sql tx commit failed",
			"error", err.Error(),
			"duration_ms", time.Since(start).Milliseconds())
		return err
	}
	ms := time.Since(start).Milliseconds()
	if slowMs := sqlLogSlowMs.Load(); slowMs > 0 && ms >= slowMs {
		slog.Warn("sql tx slow", "duration_ms", ms)
	} else {
		slog.Debug("sql tx commit", "duration_ms", ms)
	}
	return nil
}

// queryInChunks executes a parameterized IN-query in chunks to stay within
// SQLite's parameter limit. queryTemplate must contain a single %s placeholder
// for the comma-separated "?" list. The prefix args are prepended before each
// chunk's args (e.g., a source_id filter).
// chunkQuerier abstracts the subset of *loggedDB that queryInChunks
// and execInChunks actually use. The Query path returns *loggedRows
// so streaming-query timing reflects scan-close, not just prepare.
type chunkQuerier interface {
	Query(query string, args ...any) (*loggedRows, error)
	Exec(query string, args ...any) (sql.Result, error)
}

func queryInChunks[T any](db chunkQuerier, ids []T, prefixArgs []interface{}, queryTemplate string, fn func(*loggedRows) error) error {
	const chunkSize = 500
	for i := 0; i < len(ids); i += chunkSize {
		end := i + chunkSize
		if end > len(ids) {
			end = len(ids)
		}
		chunk := ids[i:end]

		placeholders := make([]string, len(chunk))
		args := make([]interface{}, 0, len(prefixArgs)+len(chunk))
		args = append(args, prefixArgs...)
		for j, id := range chunk {
			placeholders[j] = "?"
			args = append(args, id)
		}

		query := fmt.Sprintf(queryTemplate, strings.Join(placeholders, ","))
		rows, err := db.Query(query, args...)
		if err != nil {
			return err
		}

		for rows.Next() {
			if err := fn(rows); err != nil {
				_ = rows.Close()
				return err
			}
		}
		_ = rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
	}
	return nil
}

// chunkInsert describes a multi-row INSERT for insertInChunks.
// Prefix is everything up to "VALUES ", suffix is anything after the values
// (e.g. " ON CONFLICT DO NOTHING" for PostgreSQL). ValuesPerRow counts the
// parameters in one row's tuple (used to stay under the driver's parameter
// limit).
type chunkInsert struct {
	totalRows    int
	valuesPerRow int
	prefix       string
	suffix       string
}

// insertInChunks executes a multi-value INSERT in chunks to stay within SQLite's
// parameter limit (999). valueBuilder generates the VALUES placeholders and
// args for each chunk of row indices. Rebinding to the dialect's placeholder
// form happens inside tx.Exec (loggedTx wraps the dialect's Rebind).
func insertInChunks(tx *loggedTx, c chunkInsert, valueBuilder func(start, end int) ([]string, []interface{})) error {
	// SQLite default SQLITE_MAX_VARIABLE_NUMBER is 999
	// Leave some margin for safety
	const maxParams = 900
	chunkSize := maxParams / c.valuesPerRow
	if chunkSize < 1 {
		chunkSize = 1
	}

	for i := 0; i < c.totalRows; i += chunkSize {
		end := i + chunkSize
		if end > c.totalRows {
			end = c.totalRows
		}

		values, args := valueBuilder(i, end)
		query := c.prefix + strings.Join(values, ",") + c.suffix
		if _, err := tx.Exec(query, args...); err != nil {
			return err
		}
	}
	return nil
}

// execInChunks executes a parameterized DELETE/UPDATE with an IN-clause in chunks
// to stay within SQLite's parameter limit. queryTemplate must contain a single %s
// placeholder for the comma-separated "?" list. The prefix args are prepended before
// each chunk's args (e.g., a message_id filter).
func execInChunks[T any](db chunkQuerier, ids []T, prefixArgs []interface{}, queryTemplate string) error {
	const chunkSize = 500
	for i := 0; i < len(ids); i += chunkSize {
		end := i + chunkSize
		if end > len(ids) {
			end = len(ids)
		}
		chunk := ids[i:end]

		placeholders := make([]string, len(chunk))
		args := make([]interface{}, 0, len(prefixArgs)+len(chunk))
		args = append(args, prefixArgs...)
		for j, id := range chunk {
			placeholders[j] = "?"
			args = append(args, id)
		}

		query := fmt.Sprintf(queryTemplate, strings.Join(placeholders, ","))
		if _, err := db.Exec(query, args...); err != nil {
			return err
		}
	}
	return nil
}

// Rebind converts a query with ? placeholders to the appropriate format
// for the current database driver. No-op for SQLite; converts to $1, $2, ...
// for PostgreSQL.
func (s *Store) Rebind(query string) string {
	return s.dialect.Rebind(query)
}

// FTS5Available returns whether FTS5 full-text search is available.
func (s *Store) FTS5Available() bool {
	return s.fts5Available
}

// IsBusyError reports whether err indicates another process holds the
// database (SQLITE_BUSY or SQLITE_LOCKED). Callers running maintenance
// operations that need exclusive access can use this to produce a
// user-actionable "stop other processes and retry" message.
func (s *Store) IsBusyError(err error) bool {
	return s.dialect.IsBusyError(err)
}

// SchemaStale checks whether the database schema is missing columns
// added by recent migrations. Returns (stale, column, err). Only
// reports stale when the query succeeds and the column is absent;
// query errors are returned separately so callers don't misdiagnose
// corruption or permission problems as outdated schema.
func (s *Store) SchemaStale() (bool, string, error) {
	var count int
	err := s.db.QueryRow(s.dialect.SchemaStaleCheck()).Scan(&count)
	if err != nil {
		return false, "", fmt.Errorf("check schema version: %w", err)
	}
	if count == 0 {
		return true, "conversations.conversation_type", nil
	}
	return false, "", nil
}

// InitSchema initializes the database schema.
// This creates all tables if they don't exist.
func (s *Store) InitSchema() error {
	// Load and execute schema files provided by the dialect.
	for _, filename := range s.dialect.SchemaFiles() {
		schema, err := schemaFS.ReadFile(filename)
		if err != nil {
			return fmt.Errorf("read %s: %w", filename, err)
		}
		if _, err := s.db.Exec(string(schema)); err != nil {
			return fmt.Errorf("execute %s: %w", filename, err)
		}
	}

	// Migrations: add columns for databases created before these features.
	// The dialect determines whether a "duplicate column" error is benign.
	for _, m := range []struct {
		sql  string
		desc string
	}{
		{`ALTER TABLE sources ADD COLUMN sync_config JSON`, "sync_config"},
		{`ALTER TABLE messages ADD COLUMN rfc822_message_id TEXT`, "rfc822_message_id"},
		{`ALTER TABLE sources ADD COLUMN oauth_app TEXT`, "oauth_app"},
		{`ALTER TABLE participants ADD COLUMN phone_number TEXT`, "phone_number"},
		{`ALTER TABLE participants ADD COLUMN canonical_id TEXT`, "canonical_id"},
		{`ALTER TABLE messages ADD COLUMN sender_id INTEGER REFERENCES participants(id)`, "sender_id"},
		{`ALTER TABLE messages ADD COLUMN message_type TEXT NOT NULL DEFAULT 'email'`, "message_type"},
		{`ALTER TABLE messages ADD COLUMN attachment_count INTEGER DEFAULT 0`, "attachment_count"},
		{`ALTER TABLE messages ADD COLUMN deleted_from_source_at DATETIME`, "deleted_from_source_at"},
		{`ALTER TABLE messages ADD COLUMN deleted_at DATETIME`, "deleted_at"},
		{`ALTER TABLE messages ADD COLUMN delete_batch_id TEXT`, "delete_batch_id"},
		{`ALTER TABLE conversations ADD COLUMN title TEXT`, "title"},
		{`ALTER TABLE conversations ADD COLUMN conversation_type TEXT NOT NULL DEFAULT 'email_thread'`, "conversation_type"},
	} {
		if _, err := s.db.Exec(m.sql); err != nil {
			if !s.dialect.IsDuplicateColumnError(err) {
				return fmt.Errorf("migrate schema (%s): %w", m.desc, err)
			}
		}
	}

	// Load the optional FTS schema, if the dialect keeps one separate.
	// PostgreSQL returns "" here because its tsvector lives in the main schema.
	if ftsFile := s.dialect.SchemaFTS(); ftsFile != "" {
		ftsSchema, err := schemaFS.ReadFile(ftsFile)
		if err != nil {
			return fmt.Errorf("read %s: %w", ftsFile, err)
		}
		if _, err := s.db.Exec(string(ftsSchema)); err != nil {
			if !s.dialect.IsNoSuchModuleError(err) {
				return fmt.Errorf("init FTS schema: %w", err)
			}
			// Module not compiled in; availability stays false. Fall
			// through so the rest of schema init still runs.
		}
	}

	// Probe availability through the dialect so it works uniformly for
	// backends that carry FTS inside their main schema.
	s.fts5Available = s.dialect.FTSAvailable(s.db.DB)

	// Ensure the default "All" collection exists and contains every source.
	if err := s.EnsureDefaultCollection(); err != nil {
		return fmt.Errorf("ensure default collection: %w", err)
	}

	return nil
}

// NeedsFTSBackfill reports whether the FTS index needs to be populated.
func (s *Store) NeedsFTSBackfill() bool {
	if !s.fts5Available {
		return false
	}
	return s.dialect.FTSNeedsBackfill(s.db.DB)
}

// Stats holds database statistics.
type Stats struct {
	MessageCount    int64
	ThreadCount     int64
	AttachmentCount int64
	LabelCount      int64
	SourceCount     int64
	DatabaseSize    int64
}

// GetStats returns statistics about the database.
// Delegates to GetStatsForScope with no scope filter (global counts).
func (s *Store) GetStats() (*Stats, error) {
	return s.GetStatsForScope(nil)
}

// GetStatsForScope returns statistics scoped to the given source IDs.
// When sourceIDs is nil or empty, returns global counts.
// All message-derived counts (threads, attachments, labels) exclude
// dedup-hidden and source-deleted messages via LiveMessagesWhere.
// DatabaseSize is always the global file size — it cannot be decomposed per source.
func (s *Store) GetStatsForScope(sourceIDs []int64) (*Stats, error) {
	stats := &Stats{}

	var queries []struct {
		query string
		args  []any
		dest  *int64
	}

	if len(sourceIDs) == 0 {
		// Unscoped: global catalog counts, matching pre-slice-3 semantics.
		// All message-linked counts apply LiveMessagesWhere so dedup-hidden
		// and source-deleted rows aren't reported as live rows.
		queries = []struct {
			query string
			args  []any
			dest  *int64
		}{
			{
				"SELECT COUNT(*) FROM messages WHERE " + LiveMessagesWhere("", true),
				nil,
				&stats.MessageCount,
			},
			{
				"SELECT COUNT(*) FROM conversations WHERE EXISTS (" +
					"SELECT 1 FROM messages m WHERE m.conversation_id = conversations.id AND " + LiveMessagesWhere("m", true) +
					")",
				nil,
				&stats.ThreadCount,
			},
			{
				"SELECT COUNT(*) FROM attachments a WHERE EXISTS (" +
					"SELECT 1 FROM messages m WHERE m.id = a.message_id AND " + LiveMessagesWhere("m", true) +
					")",
				nil,
				&stats.AttachmentCount,
			},
			{
				"SELECT COUNT(*) FROM labels l WHERE EXISTS (" +
					"SELECT 1 FROM message_labels ml JOIN messages m ON m.id = ml.message_id WHERE ml.label_id = l.id AND " + LiveMessagesWhere("m", true) +
					")",
				nil,
				&stats.LabelCount,
			},
			{
				"SELECT COUNT(*) FROM sources",
				nil,
				&stats.SourceCount,
			},
		}
	} else {
		// Build the IN (?, ?, ...) placeholder list. TrimSuffix is panic-safe
		// for any len(sourceIDs); the outer guard already routes empty slices
		// to the unscoped branch, but this avoids a negative slice index if
		// the guard is ever refactored.
		placeholders := strings.TrimSuffix(strings.Repeat("?,", len(sourceIDs)), ",")

		inClause := "source_id IN (" + placeholders + ")"
		args := make([]any, len(sourceIDs))
		for i, id := range sourceIDs {
			args[i] = id
		}
		cloneArgs := func() []any {
			out := make([]any, len(args))
			copy(out, args)
			return out
		}

		queries = []struct {
			query string
			args  []any
			dest  *int64
		}{
			{
				"SELECT COUNT(*) FROM messages WHERE " + LiveMessagesWhere("", true) + " AND " + inClause,
				cloneArgs(),
				&stats.MessageCount,
			},
			{
				"SELECT COUNT(DISTINCT conversation_id) FROM messages WHERE " + LiveMessagesWhere("", true) + " AND " + inClause,
				cloneArgs(),
				&stats.ThreadCount,
			},
			{
				"SELECT COUNT(*) FROM attachments a WHERE EXISTS (" +
					"SELECT 1 FROM messages m WHERE m.id = a.message_id AND " + LiveMessagesWhere("m", true) +
					" AND m." + inClause + ")",
				cloneArgs(),
				&stats.AttachmentCount,
			},
			{
				"SELECT COUNT(DISTINCT ml.label_id) FROM message_labels ml " +
					"JOIN messages m ON m.id = ml.message_id WHERE " + LiveMessagesWhere("m", true) +
					" AND m." + inClause,
				cloneArgs(),
				&stats.LabelCount,
			},
		}
		// SourceCount reflects the scope: how many distinct accounts are
		// represented. Dedupe defensively in case a caller passes a
		// slice with repeats.
		seen := make(map[int64]struct{}, len(sourceIDs))
		for _, id := range sourceIDs {
			seen[id] = struct{}{}
		}
		stats.SourceCount = int64(len(seen))
	}

	for _, q := range queries {
		var row *sql.Row
		if len(q.args) > 0 {
			row = s.db.QueryRow(q.query, q.args...)
		} else {
			row = s.db.QueryRow(q.query)
		}
		if err := row.Scan(q.dest); err != nil {
			if s.dialect.IsNoSuchTableError(err) {
				continue
			}
			return nil, fmt.Errorf("get stats %q: %w", q.query, err)
		}
	}

	// DatabaseSize is always the global file size; scoped stats cannot decompose it.
	if info, err := os.Stat(s.dbPath); err == nil {
		stats.DatabaseSize = info.Size()
	}

	return stats, nil
}
