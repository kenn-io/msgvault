package store

import (
	"context"
	"database/sql"
)

// FTSDoc is the set of fields the dialect needs to upsert a message into
// the full-text search index.
type FTSDoc struct {
	MessageID int64
	Subject   string
	Body      string
	FromAddr  string
	ToAddrs   string
	CcAddrs   string
}

// ColumnMigration is a single ALTER TABLE ADD COLUMN statement used by
// SQLiteDialect.LegacyColumnMigrations to evolve older SQLite databases.
type ColumnMigration struct {
	SQL  string // full ALTER TABLE ... ADD COLUMN statement
	Desc string // short label for error messages
}

// Dialect abstracts database-specific SQL generation and behavior.
// Implementations exist for SQLite (default) and PostgreSQL (opt-in).
type Dialect interface {
	// DriverName returns the database/sql driver name ("sqlite3" or "pgx").
	DriverName() string

	// Rebind converts a query with ? placeholders to the appropriate format
	// for the database driver. No-op for SQLite; converts to $1, $2, ... for PostgreSQL.
	Rebind(query string) string

	// Now returns the SQL expression for the current timestamp.
	// SQLite: "datetime('now')"  PostgreSQL: "NOW()"
	Now() string

	// InsertOrIgnore rewrites a complete INSERT statement to silently ignore conflicts.
	// SQLite: INSERT OR IGNORE INTO ...  PostgreSQL: INSERT INTO ... ON CONFLICT DO NOTHING
	// The input sql must be a complete statement in SQLite form
	// (starting with "INSERT OR IGNORE INTO"). For chunked inserts that
	// build the VALUES list incrementally, use InsertOrIgnorePrefix +
	// InsertOrIgnoreSuffix instead.
	InsertOrIgnore(sql string) string

	// InsertOrIgnorePrefix rewrites the prefix portion of a chunked
	// INSERT OR IGNORE whose VALUES tuples are appended separately.
	// The input must be a SQLite-form prefix ending in "VALUES ".
	// SQLite returns the prefix unchanged (OR IGNORE stays); PostgreSQL
	// strips "OR IGNORE" so conflict handling can come from the suffix.
	// Always pair this with InsertOrIgnoreSuffix at the end of the statement.
	InsertOrIgnorePrefix(sql string) string

	// InsertOrIgnoreSuffix returns a SQL suffix to append after VALUES for
	// conflict-ignoring inserts built incrementally (e.g., by insertInChunks).
	// SQLite: "" (OR IGNORE is in the prefix)
	// PostgreSQL: " ON CONFLICT DO NOTHING"
	InsertOrIgnoreSuffix() string

	// Full-text search

	// FTSUpsert inserts or updates the search index for a single message.
	// The dialect owns both the SQL and the argument shape, so SQLite's
	// FTS5 rowid duplication stays out of the caller and PostgreSQL is
	// free to use a column-update on messages.
	FTSUpsert(q querier, doc FTSDoc) error

	// FTSSearchClause returns SQL fragments for full-text search using ?
	// placeholders. Returns: join clause, where clause, order-by clause,
	// and the number of times the caller must re-bind the search term to
	// satisfy ? placeholders that appear in orderBy (SQLite: 0, because
	// "rank" is an implicit FTS5 column; PostgreSQL: 1 for ts_rank).
	// Callers compose these with their own SQL and must run Rebind on the
	// final query before execution.
	FTSSearchClause() (join, where, orderBy string, orderArgCount int)

	// FTSDeleteSQL returns the SQL to remove FTS entries for messages belonging to
	// a given source. Takes one parameter: source_id.
	FTSDeleteSQL() string

	// FTSBackfillBatchSQL returns the SQL to populate the search index for a range of message IDs.
	// Uses two ? placeholders for the ID range: WHERE m.id >= ? AND m.id < ?
	FTSBackfillBatchSQL() string

	// FTSAvailable reports whether full-text search is available for this database.
	// For SQLite this probes the FTS5 virtual table; for PostgreSQL it checks
	// that the tsvector column exists.
	FTSAvailable(db *sql.DB) bool

	// FTSNeedsBackfill reports whether the FTS index needs to be populated.
	FTSNeedsBackfill(db *sql.DB) bool

	// FTSClearSQL returns the SQL to clear all FTS data before a full backfill.
	FTSClearSQL() string

	// SchemaFTS returns the embedded filename containing FTS DDL to execute during
	// schema initialization. Returns "" if no separate FTS schema file is needed
	// (e.g., PostgreSQL includes tsvector in its main schema).
	SchemaFTS() string

	// FTSRebuildSchema tears down and recreates the FTS infrastructure from
	// scratch — the caller is expected to follow up with a full backfill.
	// Used to recover from malformed FTS shadow-table state that in-place
	// rebuild operations (e.g., SQLite's rebuild pragma) cannot clear.
	// SQLite: DROP TABLE IF EXISTS messages_fts + re-execute schema_sqlite.sql.
	// PostgreSQL: TODO (REINDEX / recompute tsvector column).
	FTSRebuildSchema(db *sql.DB) error

	// LegacyColumnMigrations returns ALTER TABLE ADD COLUMN statements to
	// bring older databases up to date with schema columns added over time.
	// Both dialects return the same logical list, translated to the
	// dialect's column-type spellings. Statements are idempotent
	// (`IF NOT EXISTS` on PG; IsDuplicateColumnError silences re-runs on
	// SQLite). Fresh installs see no-op ALTERs because the columns are
	// already present in schema.sql / schema_pg.sql.
	LegacyColumnMigrations() []ColumnMigration

	// DatabaseSize returns the on-disk or logical size of the database in
	// bytes. For SQLite: file size at dbPath. For PostgreSQL: queries
	// pg_database_size(). Returns 0 if the size cannot be determined;
	// an error only for genuine failures (not missing files).
	DatabaseSize(db *sql.DB, dbPath string) (int64, error)

	// Connection lifecycle

	// InitConn performs driver-specific connection initialization.
	// Called after opening a connection. For SQLite: no-op (PRAGMAs are set via
	// DSN parameters). For PostgreSQL: SET search_path, statement_timeout, etc.
	InitConn(db *sql.DB) error

	// SchemaFiles returns the filenames of embedded schema files to execute during InitSchema.
	SchemaFiles() []string

	// CheckpointWAL checkpoints the WAL (SQLite) or is a no-op (PostgreSQL).
	CheckpointWAL(db *sql.DB) error

	// Schema migration

	// SchemaStaleCheck returns the SQL to check whether migrations are needed.
	SchemaStaleCheck() string

	// IsDuplicateColumnError returns true if the error indicates an ALTER TABLE
	// ADD COLUMN failed because the column already exists.
	IsDuplicateColumnError(err error) bool

	// Error handling

	// IsConflictError returns true if the error indicates a unique constraint violation.
	IsConflictError(err error) bool

	// IsNoSuchTableError returns true if the error indicates a missing table.
	IsNoSuchTableError(err error) bool

	// IsNoSuchModuleError returns true if the error indicates a missing module
	// (e.g., FTS5 not compiled in for SQLite). Always false for PostgreSQL.
	IsNoSuchModuleError(err error) bool

	// IsReturningError returns true if the error indicates RETURNING is not supported.
	// This handles SQLite < 3.35 which doesn't support RETURNING.
	// Always false for PostgreSQL (which always supports RETURNING).
	IsReturningError(err error) bool

	// IsBusyError returns true if the error indicates the database is held
	// by another connection, either busy (SQLITE_BUSY) or locked
	// (SQLITE_LOCKED). Used to surface actionable errors from maintenance
	// commands that need exclusive access.
	IsBusyError(err error) bool

	// BoolTrueExpr returns a SQL boolean expression that evaluates to true
	// when col holds a "true" value. SQLite stores booleans as 0/1 INTEGER
	// (emit "col = 1"); PostgreSQL has a real BOOLEAN type and rejects
	// integer comparisons against it, so the bare column name is correct.
	BoolTrueExpr(col string) string

	// BuildFTSArg formats a slice of user-supplied search terms into the
	// single string argument that FTSSearchClause's WHERE fragment binds
	// against the dialect's FTS function. Both dialects emit prefix-match
	// arguments and drop terms that contain no usable tokens:
	//   SQLite:     `"term"*` per term, space-joined (FTS5 reads space as
	//               implicit AND).
	//   PostgreSQL: `term:*` per term, joined by " & " (to_tsquery).
	// Shapes match the query package's equivalent helpers so API search
	// and engine deep-search return the same hits for the same input.
	// Returns "" when every term reduces to nothing usable — the caller
	// must substitute a FALSE predicate instead of dispatching the
	// dialect's FTS WHERE clause (an empty argument errors at both
	// to_tsquery and the FTS5 MATCH parser).
	BuildFTSArg(terms []string) string

	// JSONBindExpr returns the SQL fragment to use in place of a bare ?
	// when binding a Go string (or []byte) to a JSON column. SQLite has
	// no JSON type and stores JSON as plain TEXT, so the placeholder
	// stays bare. PostgreSQL's JSONB column does not implicitly cast
	// from text; without ?::JSONB the bind raises
	// "column is of type jsonb but expression is of type text".
	JSONBindExpr() string

	// BeginExclusive opens a transaction on conn that blocks concurrent
	// writers to the tables sync code touches (sync_runs in particular,
	// so StartSync's INSERT cannot run until COMMIT/ROLLBACK). Readers
	// may proceed.
	// SQLite: a single "BEGIN EXCLUSIVE" statement (WAL mode allows
	// concurrent reads while blocking writers).
	// PostgreSQL: "BEGIN" followed by LOCK TABLE sync_runs IN EXCLUSIVE
	// MODE, which conflicts with the ROW EXCLUSIVE lock INSERT acquires
	// but does not block ACCESS SHARE (reads).
	BeginExclusive(ctx context.Context, conn *sql.Conn) error

	// BeginWriteSQL returns the SQL to begin a transaction that
	// immediately acquires the write lock, so a read-modify-write under
	// concurrency cannot lose updates to a snapshot race.
	// SQLite: "BEGIN IMMEDIATE" (reserves the writer slot at BEGIN).
	// PostgreSQL: "BEGIN" — pair with SelectForUpdate to row-lock the
	// modified row inside the transaction.
	BeginWriteSQL() string

	// SelectForUpdate returns the row-lock clause to append to a SELECT
	// inside BeginWriteSQL transactions. PostgreSQL needs " FOR UPDATE"
	// to lock the matched row; SQLite already serializes writers under
	// BEGIN IMMEDIATE and returns "".
	SelectForUpdate() string
}
