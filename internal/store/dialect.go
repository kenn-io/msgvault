package store

import (
	"context"
	"database/sql"
	"time"
)

// CurrentFTSIndexingVersion identifies the PostgreSQL search_fts field-weight
// layout. Version 2 assigns subject=A, sender=B, recipients=C, and body=D so
// exact body-only queries can restrict tsquery matches to weight D.
const CurrentFTSIndexingVersion = 2

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

// WatermarkBounds is the pair of instants the content-change feed reads before
// every page. They answer two different questions and must not be conflated.
//
// Now is the database server's clock. It is what a consumer needs for overlap
// arithmetic against a watermark, and it is published as the page's
// server_time.
//
// CommitBound is the instant strictly below which every content_changed_at
// stamp made by a write the bound can see is already COMMITTED, and it is the
// page's upper bound. The distinction is the whole point: both backends stamp
// the watermark when the statement runs and publish the row when its
// transaction commits, so a row can carry a stamp the clock has already passed
// and still be invisible. Paging up to the clock parks the consumer's cursor
// above such a row, which then fails both arms of the keyset lower bound
// forever — measured at 40 of 40 tombstones lost from one PostgreSQL deletion
// run. Paging up to the oldest write that could still commit cannot: every
// uncommitted stamp is at or above that instant. The one write this cannot
// cover is a PostgreSQL prepared transaction, which holds its locks with no
// owning session and so exposes no start time to be the oldest. That residual
// and the feed's other exceptions are written up in docs/api-server.md; this
// comment names only the exception that bears on CommitBound and deliberately
// does not restate the list, because a list kept in two places drifts.
//
// CommitBound is never after Now, and while a write transaction is in flight it
// lags Now. That lag is the feature's cost: while a connection sits idle inside
// a transaction, the feed stops advancing. It is published (complete_through)
// so a stalled feed does not look like a caught-up one.
//
// The lag is the oldest in-flight write transaction's own age on PostgreSQL,
// which can read xact_start. SQLite cannot: its bound is the last instant the
// database was caught with its write lock free, so the lag there is the age of
// that proof, and a writer that started a moment ago inherits however long it
// had been since a probe last succeeded — including any stretch in which
// nothing read the bound at all. The two are not interchangeable when reading
// the number; see each dialect's ReadWatermarkBounds.
//
// A ZERO CommitBound is not an instant and no lag can be derived from it: it
// means no bound has been established at all, so nothing is known to have
// committed and the page is empty by construction. Only the SQLite dialect
// produces it, and only until its first successful probe (see
// SQLiteDialect.ReadWatermarkBounds). A dialect that cannot establish a bound
// it can TRUST returns an error instead, which is a different thing — see
// PostgreSQLDialect.visibilityFloor.
type WatermarkBounds struct {
	Now         time.Time
	CommitBound time.Time
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

	// ContentChangedNow returns the SQL expression that stamps
	// content_changed_at. Sub-second resolution on both backends, but not the
	// same resolution: SQLite's strftime('%f') stops at milliseconds, while
	// PostgreSQL's clock_timestamp() is microsecond. Either beats whole
	// seconds, which make same-instant collisions common enough to matter for
	// the (content_changed_at, id) change-feed cursor; neither guarantees
	// distinctness, which is why the cursor carries the id tiebreak. SQLite
	// must also emit exactly one textual format, since its comparison is
	// lexical.
	ContentChangedNow() string

	// TimestampParam converts a Go time to the parameter form that compares
	// correctly against a timestamp column on this backend. SQLite stores text
	// and the driver serialises time.Time with a "+00:00" suffix, so an
	// unformatted bind sorts BELOW an equal stored value and silently drops
	// every row sharing the cursor's instant. PostgreSQL compares natively.
	TimestampParam(t time.Time) any

	// ReadWatermarkBounds reads the database clock and the content-change
	// feed's commit bound — see WatermarkBounds for what each one means and
	// why the feed needs both.
	//
	// Each backend establishes the bound differently because each has a
	// different way to observe writes that are stamped but not yet committed:
	// PostgreSQL exposes every backend's transaction start in
	// pg_stat_activity, while SQLite exposes nothing and instead serialises
	// writers, so acquiring the write lock is itself proof that no write is in
	// flight. Both are documented on their implementations.
	//
	// Called once per page, on the pooled handle, outside any transaction the
	// caller holds.
	ReadWatermarkBounds(ctx context.Context, db *sql.DB) (WatermarkBounds, error)

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

	// InvalidateFTSForMessage removes or marks stale one message's search
	// document before its canonical body changes. This prevents a failed
	// best-effort reindex from leaving an old body searchable as an exact hit.
	InvalidateFTSForMessage(q querier, messageID int64) error

	// FTSBackfillBatchSQL returns the SQL to populate the search index for a range of message IDs.
	// Uses two ? placeholders for the ID range: WHERE m.id >= ? AND m.id < ?
	FTSBackfillBatchSQL() string

	// FTSAvailable reports whether full-text search is available for this database.
	// For SQLite this probes the FTS5 virtual table; for PostgreSQL it checks
	// that the tsvector column exists.
	//
	// A probe that fails reports "unavailable" with a nil error — that is the
	// answer for a binary built without FTS5, and it is the whole point of
	// probing rather than asking the schema. The error is reserved for ctx: a
	// cancelled probe has no answer at all, and reporting one as `false` would
	// turn an operator's SIGINT into a store that silently believes search is
	// gone. Implementations must bind the probe to ctx and must not reach around
	// it to a contextless handle.
	FTSAvailable(ctx context.Context, db *sql.DB) (bool, error)

	// FTSNeedsBackfill reports whether the FTS index needs to be populated.
	FTSNeedsBackfill(db *sql.DB) bool

	// FTSNeedsBackfillQuick is a cheap approximation of FTSNeedsBackfill for
	// hot paths: it must never take longer than a few index lookups. True
	// means the index is visibly behind (a backfill is certainly needed);
	// false is not authoritative — on SQLite the MAX(rowid)-vs-MAX(id)
	// comparison misses interior holes that only the full anti-join finds.
	FTSNeedsBackfillQuick(ctx context.Context, db *sql.DB) bool

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
	// PostgreSQL: DROP INDEX + full-table search_fts = NULL + recreate GIN.
	//
	// Takes a querier (not *sql.DB) so RebuildFTS can run it on the
	// maintenance transaction whose statement_timeout has been disabled — the
	// PG path includes a full-table tsvector clear (same cost as FTSClearSQL)
	// plus a GIN rebuild over a populated table, both of which can exceed the
	// pool-wide 30s timeout on a large archive (finding S1).
	FTSRebuildSchema(ctx context.Context, q contextQuerier) error

	// EnsureFTSIndex idempotently creates any FTS index that must be created
	// AFTER LegacyColumnMigrations have added the FTS column. SQLite is a
	// no-op (its messages_fts virtual table is created via SchemaFTS). For
	// PostgreSQL it creates the GIN index on messages.search_fts; this lives
	// here, not in schema_pg.sql, because a legacy PG database missing the
	// search_fts column would fail the schema-file Exec on the index before
	// the ADD COLUMN migration could run. Called by InitSchema after
	// LegacyColumnMigrations. [cr2-10]
	//
	// Takes a querier (not *sql.DB) so InitSchema can run it on the
	// maintenance transaction whose statement_timeout has been disabled —
	// the GIN build over a populated messages table can exceed the pool-wide
	// 30s timeout on a large archive (finding S1). As with EnsureTriggers,
	// InitSchema passes one BOUND to its context, so an implementation
	// inherits cancellation from every statement it runs through q and must
	// not reach around it to a contextless handle: with no statement_timeout
	// left on the transaction, an index build blocked on a table lock would
	// otherwise ignore SIGINT and SIGTERM indefinitely.
	EnsureFTSIndex(q querier) error

	// ValidateMessageWatermarks checks cheap, backend-specific invariants that
	// must hold on every open even when the versioned trigger migration is
	// already applied.
	ValidateMessageWatermarks(q querier) error

	// EnsureTriggers idempotently creates the database-maintained triggers on
	// both message watermarks: last_modified, which bumps on any change to a
	// message or its body row, and content_changed_at, the change feed's
	// watermark, which bumps only when tracked content actually changes.
	// Called by InitSchema after LegacyColumnMigrations (which add both
	// columns on legacy DBs), so both are guaranteed present.
	//
	// Both dialects DROP and CREATE rather than create-if-absent, so a change
	// to the tracked-column list reaches an existing archive. On SQLite that
	// includes trg_messages_last_modified, whose `UPDATE OF` scope has to be
	// built from the live column list and so cannot be static SQL; only the
	// message_bodies last_modified pair still rides schema.sql. On PostgreSQL
	// it covers every trigger, because CREATE TRIGGER is not idempotent
	// before PG14.
	//
	// Takes a querier (not *sql.DB) so InitSchema can run it on the
	// maintenance transaction (consistent with EnsureFTSIndex). InitSchema
	// passes one BOUND to its context, so an implementation inherits
	// cancellation from every statement it runs through q and must not reach
	// around it to a contextless handle: the maintenance transaction has no
	// statement_timeout, so a DDL statement blocked on a table lock would
	// otherwise ignore SIGINT and SIGTERM indefinitely.
	EnsureTriggers(q querier) error

	// EnsureActivityProjectionTriggers repairs the durable activity queue
	// triggers independently of the message watermark trigger migration.
	EnsureActivityProjectionTriggers(q querier) error

	// LegacyColumnMigrations returns ALTER TABLE ADD COLUMN statements to
	// bring older databases up to date with schema columns added over time.
	// Both dialects return the same logical list, translated to the
	// dialect's column-type spellings. Statements are idempotent
	// (`IF NOT EXISTS` on PG; IsDuplicateColumnError silences re-runs on
	// SQLite). Fresh installs see no-op ALTERs because the columns are
	// already present in schema.sql / schema_pg.sql.
	LegacyColumnMigrations() []ColumnMigration

	// DatabaseSize returns the logical allocated size of the database in
	// bytes. SQLite multiplies the main database's page_count by page_size
	// (excluding WAL/SHM sidecar overhead); PostgreSQL queries
	// pg_database_size(). In-memory SQLite databases report 0.
	DatabaseSize(ctx context.Context, db *sql.DB, dbPath string) (int64, error)

	// Connection lifecycle

	// InitConn performs driver-specific connection initialization, called
	// after opening a connection. Both backends are currently no-ops:
	// SQLite PRAGMAs are set via DSN parameters, and PostgreSQL
	// per-connection settings (statement_timeout, hnsw.ef_search, and
	// search_path when present) are applied via pgx RuntimeParams / DSN
	// parameters at open time — a SET on a pooled *sql.DB would not
	// deterministically reach every pooled connection.
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

	// IsSerializationFailureError reports whether err is the backend's refusal
	// to serialize a lock or write against a row that a concurrent
	// transaction changed and committed after this transaction took its
	// snapshot (PostgreSQL: SQLSTATE 40001 serialization_failure, raised by
	// SELECT ... FOR UPDATE under REPEATABLE READ). It is not "retry later"
	// like IsBusyError: nothing was lost to contention, the read set simply
	// went stale, and the caller is expected to translate it into whatever
	// stale-input error it already reports. SQLite serializes writers, so its
	// implementation always returns false.
	IsSerializationFailureError(err error) bool

	// IsFTSValueTooLargeError returns true if err indicates an FTS value
	// exceeded a hard backend limit (PostgreSQL: SQLSTATE 54000
	// program_limit_exceeded, "string is too long for tsvector"). This is the
	// ONLY error for which the FTS backfill is allowed to skip the offending
	// row and continue; every other error must abort so a systemic failure
	// (dead connection, etc.) is not silently masked. SQLite's FTS5 has no
	// such limit, so the SQLite impl always returns false.
	IsFTSValueTooLargeError(err error) bool

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

	// JSONIsDistinctExpr returns a null-safe comparison between a JSON column
	// and one bound JSON value. PostgreSQL compares parsed JSONB values rather
	// than their differently formatted text renderings; SQLite compares its
	// stored JSON text directly.
	JSONIsDistinctExpr(col string) string

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

	// RowWriterLockSQL returns a statement that takes the database writer
	// lock on one row of table, keyed by id through a single ? placeholder,
	// or "" when SelectForUpdate is the right tool. A read-then-write
	// transaction runs it before its first read.
	// SQLite: a self-assign UPDATE (`UPDATE t SET col = col WHERE id = ?`).
	// A deferred transaction that reads first and asks for the writer lock
	// later loses SQLITE_BUSY_SNAPSHOT outright to any writer that committed
	// meanwhile, without the busy handler being consulted; taking the lock
	// up front makes it wait for its turn instead. The self-assign changes
	// no value.
	// PostgreSQL: "" — the row lock comes from SelectForUpdate. A self-assign
	// UPDATE there would write a new row version, so two REPEATABLE READ
	// transactions locking the same row would fail each other with a
	// serialization error even though neither changed anything.
	RowWriterLockSQL(table, column string) string

	// MaintenanceTimeoutResetSQL returns a statement that disables any
	// per-statement execution timeout for the remainder of the current
	// transaction, or "" if the backend has no such timeout.
	//
	// PostgreSQL: "SET LOCAL statement_timeout = 0". The pool-wide 30s
	// statement_timeout (postgresConnConfig) would otherwise cancel
	// maintenance operations whose cost scales with archive size — cascade
	// source deletes, FTS clear/backfill rewrites, GIN index builds, the
	// attachment-dedup unique-index migration, and dedup cascade deletes —
	// with SQLSTATE 57014 on a large archive. SET LOCAL applies only to the
	// enclosing transaction and auto-resets at COMMIT/ROLLBACK, so it can
	// never leak the GUC to another pooled connection (unlike a bare session
	// SET). Callers MUST run this inside an explicit transaction.
	//
	// SQLite: "" (no statement_timeout concept). Store.runMaintenance skips
	// the statement when this is empty, preserving SQLite behavior exactly.
	MaintenanceTimeoutResetSQL() string
}
