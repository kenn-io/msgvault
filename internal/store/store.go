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
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/mattn/go-sqlite3"
	"go.kenn.io/msgvault/internal/sqliteutil"
)

//go:embed schema.sql schema_sqlite.sql schema_pg.sql
var schemaFS embed.FS

// HNSWEfSearch is the per-connection value applied to pgvector's
// hnsw.ef_search GUC (via RuntimeParams in postgresConnConfig). It must
// be >= the largest inner ANN LIMIT the vector backend issues so the
// HNSW index does not throttle the over-fetch below k. The fused ANN
// path's inner LIMIT is (KPerSignal+1)*fusedANNChunksPerMessage ≈ 808 at
// the default KPerSignal=100; 1000 covers that worst case with headroom
// while keeping per-query latency bounded. The candidate-widening loop in
// the pgvector backend can grow the inner LIMIT beyond this for
// pathological multi-chunk corpora; in that regime recall is best-effort.
const HNSWEfSearch = 1000

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

// synchronous=FULL + fullfsync=true protects WAL writes against OS/power crashes
// (NORMAL only protects against process crashes). msgvault is commonly run as a
// laptop daemon (`msgvault serve`) where sleep/wake, forced reboots, and OOM kills
// give many opportunities to leave a torn page on disk; the write volume is tiny
// so the durability cost is negligible. fullfsync is macOS-only (F_FULLFSYNC
// fcntl) and a no-op on other platforms.
const defaultSQLiteParams = "?_journal_mode=WAL&_busy_timeout=30000&_synchronous=FULL&_fullfsync=true&_foreign_keys=ON"

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

// IsPostgresURL returns true if the path looks like a PostgreSQL connection URL.
// Exported so cmd-side helpers can decide whether to skip SQLite-only code
// paths (e.g., the Parquet analytics cache) without first opening a Store.
func IsPostgresURL(dbPath string) bool {
	return strings.HasPrefix(dbPath, "postgresql://") || strings.HasPrefix(dbPath, "postgres://")
}

// testSQLiteParams configures SQLite for ephemeral test databases: WAL mode
// for concurrency parity with production, but synchronous=OFF (no fsync per
// commit). Test DBs live in t.TempDir() and are discarded at test exit, so
// durability against OS crashes is irrelevant — and on slow-fsync platforms
// like Windows CI runners, the production FULL setting can push bulk-import
// tests past their timing tripwires.
const testSQLiteParams = "?_journal_mode=WAL&_busy_timeout=30000&_synchronous=OFF&_foreign_keys=ON"

// Open opens or creates the database at the given path.
// If dbPath is a postgres:// or postgresql:// URL, opens a PostgreSQL connection.
// Otherwise, opens a SQLite database at the file path.
func Open(dbPath string) (*Store, error) {
	if IsPostgresURL(dbPath) {
		return openPostgres(dbPath)
	}
	return openSQLite(dbPath, defaultSQLiteParams)
}

// OpenForTest opens or creates a database tuned for test use: ephemeral,
// fast, with durability disabled. PostgreSQL URLs go through the normal
// connection path (durability is a server-side concern there).
//
// Not for production use — a process crash mid-test can leave a corrupt
// database, which is fine because tests recreate it from scratch.
func OpenForTest(dbPath string) (*Store, error) {
	if IsPostgresURL(dbPath) {
		return openPostgres(dbPath)
	}
	return openSQLite(dbPath, testSQLiteParams)
}

// openSQLite opens a SQLite database at the given file path with the
// supplied DSN parameters appended.
func openSQLite(dbPath, params string) (*Store, error) {
	// Ensure directory exists (skip for in-memory databases)
	if dbPath != ":memory:" && !strings.Contains(dbPath, ":memory:") {
		dir := filepath.Dir(dbPath)
		if err := os.MkdirAll(dir, 0755); err != nil {
			return nil, fmt.Errorf("create db directory: %w", err)
		}
	}

	dsn := dbPath + params
	db, err := sql.Open(sqliteutil.DriverName(), dsn)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}

	if err := db.PingContext(context.Background()); err != nil {
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

	if err := db.PingContext(context.Background()); err != nil {
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
	if IsPostgresURL(dbPath) {
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
	db, err := sql.Open(sqliteutil.DriverName(), dsn)
	if err != nil {
		return nil, fmt.Errorf("open database (read-only): %w", err)
	}

	if err := db.PingContext(context.Background()); err != nil {
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

	if err := db.PingContext(context.Background()); err != nil {
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
	// Raise pgvector's HNSW ef_search so the vector backend's over-fetch
	// (inner ORDER BY <=> LIMIT) is not silently capped at the pgvector
	// default of 40. The fused ANN path issues the largest inner LIMIT —
	// (KPerSignal+1)*fusedANNChunksPerMessage, ≈808 at the default
	// KPerSignal=100 — and Search over-fetches k*annOverFetchFactor; with
	// ef_search=40 the HNSW index would return at most ~40 candidates and
	// short-return below k on multi-chunk corpora. Sizing ef_search to
	// HNSWEfSearch keeps the over-fetch design intact. Setting a GUC is not
	// a data write, so this is safe even under default_transaction_read_only.
	// Larger values raise per-query latency, so it is sized to the worst-case
	// inner LIMIT for the default config rather than unboundedly.
	connConfig.RuntimeParams["hnsw.ef_search"] = strconv.Itoa(HNSWEfSearch)
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

// OpenPostgresDB opens a raw *sql.DB handle for the given PostgreSQL URL using
// the same connection config (statement_timeout, runtime params) as Store.Open.
// The returned cleanup func must be called when the handle is no longer needed.
// Use this for lightweight consumers that only need the *sql.DB handle without
// the full Store wrapper (e.g. embeddings metadata queries that live in the
// same PG database as messages but do not need store-level operations).
func OpenPostgresDB(dbURL string) (*sql.DB, func(), error) {
	return openPostgresDB(dbURL, false)
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

// BackupDatabase writes a point-in-time consistent copy of the SQLite database
// to dst using VACUUM INTO. PostgreSQL deployments should be backed up with
// pg_dump, pg_basebackup, or replication tooling outside msgvault.
func (s *Store) BackupDatabase(dst string) error {
	if s.IsPostgreSQL() {
		return errors.New("backup-before-dedup is SQLite-only (uses VACUUM INTO); " +
			"snapshot the PostgreSQL database with pg_dump out-of-band, " +
			"then rerun with --no-backup",
		)
	}
	if _, err := os.Stat(dst); err == nil {
		return fmt.Errorf("backup target already exists: %s", dst)
	}
	if _, err := s.DB().Exec("VACUUM INTO ?", dst); err != nil {
		return fmt.Errorf("vacuum into %s: %w", dst, err)
	}
	return nil
}

// IsPostgreSQL reports whether this store is backed by PostgreSQL.
// Engine factories use this to choose between the SQLite and PostgreSQL
// query paths.
func (s *Store) IsPostgreSQL() bool {
	return s.dialect.DriverName() == "pgx"
}

// WithExclusiveLock executes fn while holding an exclusive write lock on the
// database. In WAL mode this blocks concurrent writers (e.g. StartSync) while
// allowing reads (e.g. IsAttachmentPathReferenced) to proceed. Use this to
// serialize destructive file operations against concurrent sync attachment
// ingestion. The context controls both lock acquisition and the lifetime of
// the underlying connection; cancelling it aborts a pending BEGIN EXCLUSIVE
// and rolls back any held transaction.
//
// fn must NOT write through the store. The EXCLUSIVE lock is held on a
// dedicated connection (conn below), while every store write goes to the
// pool — a *different* connection. On PostgreSQL the EXCLUSIVE lock conflicts
// with the ROW EXCLUSIVE lock any INSERT/UPDATE/DELETE acquires, so a write
// issued from fn would block on the pool waiting for a lock this same call is
// holding, deadlocking until statement_timeout cancels it. fn is for reads
// (ACCESS SHARE, which EXCLUSIVE permits) plus filesystem work only.
func (s *Store) WithExclusiveLock(ctx context.Context, fn func() error) error {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("acquire connection: %w", err)
	}
	defer func() { _ = conn.Close() }()

	if err := s.dialect.BeginExclusive(ctx, conn); err != nil {
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
	// A tx crossing the slow threshold is a diagnostic, not a problem —
	// bulk syncs routinely commit 100ms+ batches — so it logs at Info and
	// only escalates to Warn at 10x the threshold, where something is
	// genuinely wrong (lock contention, an unindexed cascade).
	ms := time.Since(start).Milliseconds()
	switch slowMs := sqlLogSlowMs.Load(); {
	case slowMs > 0 && ms >= 10*slowMs:
		slog.Warn("sql tx slow", "duration_ms", ms)
	case slowMs > 0 && ms >= slowMs:
		slog.Info("sql tx slow", "duration_ms", ms)
	default:
		slog.Debug("sql tx commit", "duration_ms", ms)
	}
	return nil
}

// runMaintenance runs fn inside a single transaction with the per-statement
// execution timeout disabled (finding S1). It is the one chokepoint for
// maintenance operations whose cost scales with archive size — cascade source
// deletes, FTS clear/backfill rewrites, GIN index builds, the attachment-dedup
// unique-index migration — which would otherwise be cancelled by the pool-wide
// 30s statement_timeout (postgresConnConfig) with SQLSTATE 57014 on a large
// archive.
//
// On PostgreSQL the first statement issued on the transaction is
// `SET LOCAL statement_timeout = 0`; SET LOCAL auto-resets at COMMIT/ROLLBACK,
// so the disabled timeout is scoped to this transaction and can never leak to
// another pooled connection. On SQLite MaintenanceTimeoutResetSQL is "" so no
// reset statement runs, and fn simply executes inside an ordinary transaction —
// SQLite has no statement_timeout, so behavior is unchanged. The reset and all
// of fn's statements run on the SAME tx (one connection), which is required for
// SET LOCAL to take effect.
//
// fn receives a *loggedTx, so its Exec/Query calls are Rebind-translated
// (? → $N on PG) just like withTx. The reset statement itself has no
// placeholders, so Rebind is a no-op on it.
func (s *Store) runMaintenance(ctx context.Context, fn func(ctx context.Context, tx *loggedTx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin maintenance tx: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	if reset := s.dialect.MaintenanceTimeoutResetSQL(); reset != "" {
		if _, err := tx.ExecContext(ctx, reset); err != nil {
			return fmt.Errorf("disable maintenance statement timeout: %w", err)
		}
	}

	if err := fn(ctx, tx); err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit maintenance tx: %w", err)
	}
	committed = true
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

func queryInChunks[T any](db chunkQuerier, ids []T, prefixArgs []any, queryTemplate string, fn func(*loggedRows) error) error {
	const chunkSize = 500
	for i := 0; i < len(ids); i += chunkSize {
		end := min(i+chunkSize, len(ids))
		chunk := ids[i:end]

		placeholders := make([]string, len(chunk))
		args := make([]any, 0, len(prefixArgs)+len(chunk))
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
func insertInChunks(tx *loggedTx, c chunkInsert, valueBuilder func(start, end int) ([]string, []any)) error {
	// SQLite default SQLITE_MAX_VARIABLE_NUMBER is 999
	// Leave some margin for safety
	const maxParams = 900
	chunkSize := max(maxParams/c.valuesPerRow, 1)

	for i := 0; i < c.totalRows; i += chunkSize {
		end := min(i+chunkSize, c.totalRows)

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
func execInChunks[T any](db chunkQuerier, ids []T, prefixArgs []any, queryTemplate string) error {
	const chunkSize = 500
	for i := 0; i < len(ids); i += chunkSize {
		end := min(i+chunkSize, len(ids))
		chunk := ids[i:end]

		placeholders := make([]string, len(chunk))
		args := make([]any, 0, len(prefixArgs)+len(chunk))
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
		return true, "messages.embed_gen", nil
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

	// Legacy databases may hold duplicate (message_id, content_hash)
	// attachment rows from the old SELECT-then-INSERT UpsertAttachment.
	// Dedupe before creating the partial unique index that enforces
	// idempotency going forward. Gated on the applied_migrations ledger:
	// the dedupe's GROUP BY over the full attachments table is not free on
	// a large archive, and it never finds work after the first run.
	//
	// Both steps run under runMaintenance: on a large archive the dedupe
	// DELETE and the unique-index build over the full attachments table
	// exceed the pool-wide 30s statement_timeout, so the maintenance escape
	// hatch disables it for this transaction (finding S1). They share one tx
	// so the index is built against the just-deduped table. No-op timeout
	// reset on SQLite.
	attachmentsMigrated, err := s.IsMigrationApplied(migrationAttachmentsContentHashUnique)
	if err != nil {
		return err
	}
	if !attachmentsMigrated {
		if err := s.runMaintenance(context.Background(), func(ctx context.Context, tx *loggedTx) error {
			if err := s.dedupeAttachmentsBeforeUniqueIndex(ctx, tx); err != nil {
				return fmt.Errorf("dedupe attachments: %w", err)
			}
			if _, err := tx.ExecContext(ctx, `
				CREATE UNIQUE INDEX IF NOT EXISTS idx_attachments_msg_content_hash
				    ON attachments(message_id, content_hash)
				    WHERE content_hash IS NOT NULL AND content_hash != ''
			`); err != nil {
				return fmt.Errorf("create idx_attachments_msg_content_hash: %w", err)
			}
			return nil
		}); err != nil {
			return err
		}
		if err := s.MarkMigrationApplied(migrationAttachmentsContentHashUnique); err != nil {
			return err
		}
	}

	// Legacy databases may have idx_participants_phone as a non-unique
	// partial index (it was created that way before the schema flipped
	// to UNIQUE). `CREATE UNIQUE INDEX IF NOT EXISTS` in schema.sql
	// silently leaves the non-unique index in place, so
	// EnsureParticipantByPhone's ON CONFLICT (phone_number) finds no
	// matching unique constraint on upgraded DBs. Run a one-shot
	// migration that dedupes phone rows, drops the index, and
	// recreates it as UNIQUE.
	if err := s.ensureParticipantsPhoneUniqueIndex(); err != nil {
		return fmt.Errorf("ensure idx_participants_phone unique: %w", err)
	}

	// Migrations: add columns for databases created before these features.
	// The dialect determines the list. Both backends return ADD COLUMN
	// migrations for DBs created before later columns were introduced:
	// SQLite emits ALTER TABLE ADD COLUMN, PostgreSQL emits the equivalent
	// ALTER TABLE ADD COLUMN IF NOT EXISTS list (including search_fts).
	//
	// lastModifiedColumnAdded tracks whether the last_modified ALTER
	// actually fired, which forces the last_modified backfill below even if
	// its ledger sentinel is present: a just-added column holds NULLs that
	// must be stamped. Only SQLite can signal this — its ALTER errors with
	// a duplicate-column error when the column exists, while PostgreSQL's
	// IF NOT EXISTS form always succeeds; PG never needs the forced path
	// because its ADD COLUMN carries DEFAULT CURRENT_TIMESTAMP, which
	// backfills existing rows in the same statement.
	lastModifiedColumnAdded := false
	for _, m := range s.dialect.LegacyColumnMigrations() {
		if _, err := s.db.Exec(m.SQL); err != nil {
			if !s.dialect.IsDuplicateColumnError(err) {
				return fmt.Errorf("migrate schema (%s): %w", m.Desc, err)
			}
		} else if m.Desc == "last_modified" && !s.IsPostgreSQL() {
			lastModifiedColumnAdded = true
		}
	}

	// Partial covering index for the ListMessages page (GET /api/v1/messages).
	// That query counts and paginates live messages ordered by
	// COALESCE(sent_at, received_at, internal_date) DESC, id DESC. Without an
	// index matching both the live-messages predicate and that sort key, SQLite
	// falls back to a full scan of the messages table (multiple GB on a large
	// archive) plus a temp-B-tree sort for every page — measured at seconds per
	// 5-row page. The partial expression index lets COUNT read only the compact
	// index and lets the page query walk it in order and stop at LIMIT,
	// eliminating both the full scan and the sort (~29x faster COUNT, no sort).
	//
	// Runs after the legacy ADD COLUMN loop above so deleted_at /
	// deleted_from_source_at exist on upgraded DBs. SQLite only: PostgreSQL
	// autovacuum keeps planner statistics current and picks its own plan, and
	// the index expression syntax differs; the measured regression is specific
	// to the statistics-free SQLite archive. Built under runMaintenance so the
	// one-time index build over a large table is not cut off by the pool-wide
	// 30s statement_timeout (finding S1). IF NOT EXISTS is idempotent per start.
	if !s.IsPostgreSQL() {
		if err := s.runMaintenance(context.Background(), func(ctx context.Context, tx *loggedTx) error {
			_, err := tx.ExecContext(ctx, `
				CREATE INDEX IF NOT EXISTS idx_messages_live_sent_at
				    ON messages(COALESCE(sent_at, received_at, internal_date) DESC, id DESC)
				    WHERE deleted_at IS NULL AND deleted_from_source_at IS NULL
			`)
			return err
		}); err != nil {
			return fmt.Errorf("create idx_messages_live_sent_at: %w", err)
		}

		// Partial indexes over the deletion timestamps. The analytics cache
		// staleness check (cacheNeedsBuild) counts messages source-deleted or
		// dedup-hidden since the last build on every daemon start, before the
		// API server binds. Neither predicate is served by an existing index
		// (idx_messages_deleted leads with source_id), so each COUNT was a
		// full scan of the messages table — measured at ~4.5s on a cold page
		// cache over a 2.5M-row archive, which was the entire cold-start
		// latency of `msgvault search`. The partial form keeps the indexes
		// proportional to the deleted rows only, so live-message insert and
		// update paths pay no maintenance for them.
		if err := s.runMaintenance(context.Background(), func(ctx context.Context, tx *loggedTx) error {
			if _, err := tx.ExecContext(ctx, `
				CREATE INDEX IF NOT EXISTS idx_messages_deleted_from_source_at
				    ON messages(deleted_from_source_at)
				    WHERE deleted_from_source_at IS NOT NULL
			`); err != nil {
				return err
			}
			_, err := tx.ExecContext(ctx, `
				CREATE INDEX IF NOT EXISTS idx_messages_deleted_at
				    ON messages(deleted_at)
				    WHERE deleted_at IS NOT NULL
			`)
			return err
		}); err != nil {
			return fmt.Errorf("create deletion timestamp indexes: %w", err)
		}
	}

	// Backfill last_modified for rows that predate the column. SQLite cannot
	// ADD COLUMN with a non-constant default, so the legacy ADD COLUMN above
	// leaves existing rows NULL; this one-shot UPDATE sets them to
	// CURRENT_TIMESTAMP so the embed worker's CAS token is a comparable value
	// (a NULL token would never satisfy `last_modified = ?` and the row would
	// loop "needs embedding" forever). Idempotent and portable: on a fresh
	// DB (or PostgreSQL, whose ADD COLUMN ... DEFAULT CURRENT_TIMESTAMP
	// backfills automatically) no rows are NULL, so this is a no-op. Gated
	// on the applied_migrations ledger: last_modified has no index, so the
	// UPDATE's WHERE clause is a full scan of the messages table — the
	// dominant cost of daemon startup on a large archive — and it never
	// finds work after the first run. Run under runMaintenance so the
	// full-table UPDATE on a large archive is not cut off by the pool-wide
	// statement_timeout (no-op reset on SQLite).
	lastModifiedMigrated, err := s.IsMigrationApplied(migrationMessagesLastModifiedBackfill)
	if err != nil {
		return err
	}
	if !lastModifiedMigrated || lastModifiedColumnAdded {
		if err := s.runMaintenance(context.Background(), func(ctx context.Context, tx *loggedTx) error {
			_, err := tx.ExecContext(ctx,
				`UPDATE messages SET last_modified = `+s.dialect.Now()+
					` WHERE last_modified IS NULL`)
			return err
		}); err != nil {
			return fmt.Errorf("backfill last_modified: %w", err)
		}
		if err := s.MarkMigrationApplied(migrationMessagesLastModifiedBackfill); err != nil {
			return err
		}
	}

	// Create FTS indexes that depend on columns just added by the legacy
	// migrations (PostgreSQL's GIN index on messages.search_fts). No-op on
	// SQLite. Must run after the migration loop above. [cr2-10]
	//
	// Run under runMaintenance: the GIN build over a populated messages
	// table can exceed the pool-wide 30s statement_timeout on a large
	// archive, so the maintenance hatch disables it for this tx (finding
	// S1). No-op timeout reset on SQLite.
	if err := s.runMaintenance(context.Background(), func(ctx context.Context, tx *loggedTx) error {
		return s.dialect.EnsureFTSIndex(tx)
	}); err != nil {
		return fmt.Errorf("ensure FTS index: %w", err)
	}

	// Create the last_modified maintenance triggers. Must run after the
	// migration loop above adds the last_modified column on legacy DBs.
	// SQLite is a no-op here (its triggers ride schema.sql); PostgreSQL
	// creates them idempotently. Run under runMaintenance for consistency
	// with EnsureFTSIndex (no statement_timeout cap on the DDL).
	if err := s.runMaintenance(context.Background(), func(ctx context.Context, tx *loggedTx) error {
		return s.dialect.EnsureTriggers(tx)
	}); err != nil {
		return fmt.Errorf("ensure last_modified triggers: %w", err)
	}

	// Drop the obsolete partial index over messages needing embedding. It was
	// redundant with the per-generation embed watermark (the work-finder scan
	// rides the messages PRIMARY KEY B-tree via `id > :watermark ORDER BY id`)
	// and useless during a rebuild (old-gen leftovers carry a non-NULL embed_gen
	// that an `embed_gen IS NULL` index never covers), while costing index
	// maintenance on the two hottest write paths (message insert + embed_gen
	// stamp). DROP IF EXISTS is idempotent and portable across SQLite/PG; it
	// cleans up any dev DB that already created the index. Run under
	// runMaintenance to match the original CREATE's transaction context.
	if err := s.runMaintenance(context.Background(), func(ctx context.Context, tx *loggedTx) error {
		_, err := tx.ExecContext(ctx,
			`DROP INDEX IF EXISTS idx_messages_embed_gen`)
		return err
	}); err != nil {
		return fmt.Errorf("drop idx_messages_embed_gen: %w", err)
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

// dedupeAttachmentsBeforeUniqueIndex removes duplicate
// (message_id, content_hash) rows from attachments so the partial
// unique index idx_attachments_msg_content_hash can be created. Pre-fix
// UpsertAttachment used a SELECT-then-INSERT pattern that could create
// duplicates under concurrency; this cleans them up once. Idempotent.
func (s *Store) dedupeAttachmentsBeforeUniqueIndex(ctx context.Context, tx *loggedTx) error {
	_, err := tx.ExecContext(ctx, `
		DELETE FROM attachments
		WHERE content_hash IS NOT NULL AND content_hash != ''
		  AND id NOT IN (
			SELECT MIN(id) FROM attachments
			WHERE content_hash IS NOT NULL AND content_hash != ''
			GROUP BY message_id, content_hash
		  )
	`)
	return err
}

// NeedsFTSBackfill reports whether the FTS index needs to be populated.
//
// This runs an anti-join that scans every message when the index is already
// complete (the healthy steady state), so it is expensive on a large archive.
// Callers on hot request paths must not invoke it per request — see the
// server-level memoization in handleCLISearch.
func (s *Store) NeedsFTSBackfill() bool {
	if !s.fts5Available {
		return false
	}
	return s.dialect.FTSNeedsBackfill(s.db.DB)
}

// NeedsFTSBackfillQuick is the cheap, hot-path-safe form of NeedsFTSBackfill:
// true means a backfill is certainly needed; false may miss interior index
// holes (SQLite) that only the full probe finds.
func (s *Store) NeedsFTSBackfillQuick() bool {
	if !s.fts5Available {
		return false
	}
	return s.dialect.FTSNeedsBackfillQuick(s.db.DB)
}

// Stats holds database statistics.
//
// MessageCount is the count of active messages: those still present in the
// source account (deleted_at IS NULL AND deleted_from_source_at IS NULL).
// SourceDeletedCount is the count of archived messages that were deleted from
// the source account but are retained in the archive (deleted_at IS NULL AND
// deleted_from_source_at IS NOT NULL). The archive is the system of record,
// so the canonical total is MessageCount + SourceDeletedCount; callers that
// display a total must label the two populations rather than pick one
// silently. Dedup-hidden rows (deleted_at IS NOT NULL) are excluded from
// both counts.
type Stats struct {
	MessageCount       int64
	SourceDeletedCount int64
	ThreadCount        int64
	AttachmentCount    int64
	LabelCount         int64
	SourceCount        int64
	DatabaseSize       int64
}

// GetStats returns statistics about the database.
// Delegates to GetStatsForScope with no scope filter (global counts).
func (s *Store) GetStats() (*Stats, error) {
	return s.GetStatsForScope(nil)
}

// GetStatsContext is the context-aware form of GetStats. Request paths pass
// the request context so the count queries carry the request_id for SQL
// logging and are cancelled with the request.
func (s *Store) GetStatsContext(ctx context.Context) (*Stats, error) {
	return s.GetStatsForScopeContext(ctx, nil)
}

// GetStatsForScope returns statistics scoped to the given source IDs.
// When sourceIDs is nil or empty, returns global counts.
// All message-derived counts (threads, attachments, labels) exclude
// dedup-hidden and source-deleted messages via LiveMessagesWhere.
// DatabaseSize is always the global file size — it cannot be decomposed per source.
func (s *Store) GetStatsForScope(sourceIDs []int64) (*Stats, error) {
	return s.GetStatsForScopeContext(context.Background(), sourceIDs)
}

// GetStatsForScopeContext is the context-aware form of GetStatsForScope.
func (s *Store) GetStatsForScopeContext(ctx context.Context, sourceIDs []int64) (*Stats, error) {
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
				"SELECT COUNT(*) FROM messages WHERE " + SourceDeletedMessagesWhere(""),
				nil,
				&stats.SourceDeletedCount,
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
				"SELECT COUNT(*) FROM messages WHERE " + SourceDeletedMessagesWhere("") + " AND " + inClause,
				cloneArgs(),
				&stats.SourceDeletedCount,
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
			row = s.db.QueryRowContext(ctx, q.query, q.args...)
		} else {
			row = s.db.QueryRowContext(ctx, q.query)
		}
		if err := row.Scan(q.dest); err != nil {
			if s.dialect.IsNoSuchTableError(err) {
				continue
			}
			return nil, fmt.Errorf("get stats %q: %w", q.query, err)
		}
	}

	// DatabaseSize: file size for SQLite, pg_database_size() for PostgreSQL.
	if size, err := s.dialect.DatabaseSize(s.db.DB, s.dbPath); err == nil {
		stats.DatabaseSize = size
	}

	return stats, nil
}
