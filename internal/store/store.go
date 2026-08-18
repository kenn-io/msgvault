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
	"slices"
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

	// Test-only seams into the migration and backfill paths, nil in
	// production and settable only from export_test.go. They belong to the
	// Store rather than the package because more than one Store can be
	// migrating at once inside a single test binary — test fixtures build
	// their schemas concurrently — and a hook installed by one test must
	// never fire on another Store's migration. As package-level variables
	// they were also a data race between a test that installs one and any
	// concurrent migration that reads it.
	initSchemaWindowHook             func()
	contentChangedBackfillBatchHook  func(fromID, toID int64) error
	backfillFTSBatchErrHook          func(fromID, toID int64) error
	attachmentRoleRepairPreparedHook func()

	// Zero means "use the production batch size"; see
	// contentChangedBackfillBatch. Per-Store for the same reason.
	contentChangedBackfillBatchSizeOverride int64
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

	s := &Store{
		db:      newLoggedDB(db, dialect.Rebind),
		dbPath:  dbPath,
		dialect: dialect,
	}

	// Probe like the read-only opens do: a Store must know whether full-text
	// search is available in the database it just opened. InitSchema re-probes
	// after creating the FTS objects, so a caller that initializes a fresh
	// database still gets the right answer; a caller that opens an already
	// initialized one no longer has to run InitSchema to learn it.
	//
	// As in OpenReadOnly, this constructor takes no context, so the probe
	// cannot be cancelled; its error is still checked rather than dropped.
	available, err := dialect.FTSAvailable(context.Background(), db)
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("probe FTS availability: %w", err)
	}
	s.fts5Available = available

	return s, nil
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

	s := &Store{
		db:           newLoggedDB(db, dialect.Rebind),
		dbPath:       dbURL,
		dialect:      dialect,
		closeCleanup: cleanup,
	}

	// See openSQLite: availability is a property of the database, not of
	// whether this caller happened to run InitSchema.
	available, err := dialect.FTSAvailable(context.Background(), db)
	if err != nil {
		_ = db.Close()
		cleanup()
		return nil, fmt.Errorf("probe FTS availability: %w", err)
	}
	s.fts5Available = available

	return s, nil
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

	// OpenReadOnly takes no context, so the probe cannot be cancelled and its
	// error is only ever ctx's; it is still checked rather than dropped, so a
	// context-carrying form of this constructor cannot inherit a swallowed one.
	available, err := dialect.FTSAvailable(context.Background(), db)
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("probe FTS availability: %w", err)
	}
	s.fts5Available = available

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

	// As in OpenReadOnly: no context to honour here, but the error is checked
	// rather than dropped.
	available, err := dialect.FTSAvailable(context.Background(), db)
	if err != nil {
		_ = db.Close()
		cleanup()
		return nil, fmt.Errorf("probe FTS availability: %w", err)
	}
	s.fts5Available = available

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
	return s.BackupDatabaseContext(context.Background(), dst)
}

// BackupDatabaseContext is the request-aware form of BackupDatabase.
func (s *Store) BackupDatabaseContext(ctx context.Context, dst string) (returnErr error) {
	if s.IsPostgreSQL() {
		return errors.New("backup-before-dedup is SQLite-only (uses VACUUM INTO); " +
			"snapshot the PostgreSQL database with pg_dump out-of-band, " +
			"then rerun with --no-backup",
		)
	}
	if _, err := os.Lstat(dst); err == nil {
		return fmt.Errorf("backup target already exists: %s", dst)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect backup target %s: %w", dst, err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	tempDir, err := os.MkdirTemp(
		filepath.Dir(dst),
		"."+filepath.Base(dst)+".tmp-*",
	)
	if err != nil {
		return fmt.Errorf("create temporary backup directory for %s: %w", dst, err)
	}
	defer func() {
		if err := os.RemoveAll(tempDir); err != nil {
			cleanupErr := fmt.Errorf("remove temporary backup directory %s: %w", tempDir, err)
			returnErr = errors.Join(returnErr, cleanupErr)
		}
	}()

	tempPath := filepath.Join(tempDir, "backup.db")
	if _, err := s.DB().ExecContext(ctx, "VACUUM INTO ?", tempPath); err != nil {
		return fmt.Errorf("vacuum into temporary backup for %s: %w", dst, err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := os.Lstat(dst); err == nil {
		return fmt.Errorf("backup target already exists: %s", dst)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect backup target %s: %w", dst, err)
	}
	if err := os.Rename(tempPath, dst); err != nil {
		return fmt.Errorf("publish backup %s: %w", dst, err)
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
	return s.withTxContext(context.Background(), fn)
}

// withTxContext is the request-aware form of withTx. Cancelling ctx aborts
// connection acquisition and every context-aware statement in the
// transaction.
func (s *Store) withTxContext(ctx context.Context, fn func(tx *loggedTx) error) error {
	return s.withTxOptionsContext(ctx, nil, fn)
}

// withReadSnapshotContext gives a multi-statement aggregate read one stable,
// read-only database snapshot. PostgreSQL needs REPEATABLE READ because its
// default READ COMMITTED isolation takes a new snapshot for each statement.
// SQLite's driver maps these options to its existing transaction snapshot.
func (s *Store) withReadSnapshotContext(
	ctx context.Context, fn func(tx *loggedTx) error,
) error {
	return s.withTxOptionsContext(ctx, &sql.TxOptions{
		Isolation: sql.LevelRepeatableRead,
		ReadOnly:  true,
	}, fn)
}

func (s *Store) withTxOptionsContext(
	ctx context.Context, opts *sql.TxOptions, fn func(tx *loggedTx) error,
) error {
	start := time.Now()
	slog.Debug("sql tx begin")
	tx, err := s.db.BeginTx(ctx, opts)
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
	if err := ctx.Err(); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		slog.Warn("sql tx commit failed",
			"error", err.Error(),
			"duration_ms", time.Since(start).Milliseconds())
		if errors.Is(err, sql.ErrTxDone) {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
		}
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
	if err := ctx.Err(); err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		if errors.Is(err, sql.ErrTxDone) {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
		}
		return fmt.Errorf("commit maintenance tx: %w", err)
	}
	committed = true
	return nil
}

// buildLargeIndexesConcurrently creates big-table indexes without blocking
// writers. CREATE INDEX CONCURRENTLY cannot run inside a transaction (unlike
// the runMaintenance escape hatch, which only disables the pool-wide
// statement_timeout for one transaction), so this runs on a dedicated
// autocommit connection acquired directly from the pool.
//
// PostgreSQL's IF NOT EXISTS matches by name only: it treats an INVALID
// leftover (from a process killed mid-build) as "already exists" and
// returns success with a NOTICE instead of rebuilding. So a bad index from
// an earlier crashed run would otherwise persist forever — degrading writes
// on every subsequent start — because a name-matched no-op never reaches the
// error path. This checks pg_index.indisvalid for a leftover BEFORE the
// first attempt, and keeps the on-error check too for a build that goes
// INVALID during this run.
//
// Both an initial failure and a failed retry are logged and swallowed: a
// future InitSchema call (the next process start) retries the same
// idempotent build. SQLite has no CONCURRENTLY equivalent and does not need
// one — its index build over a working-set-sized archive is fast enough to
// run inline in schema.sql — so this is a no-op there.
func (s *Store) buildLargeIndexesConcurrently(ctx context.Context) {
	if !s.IsPostgreSQL() {
		return
	}

	conn, err := s.db.Conn(ctx)
	if err != nil {
		slog.Warn("acquire connection for concurrent index build failed", "error", err.Error())
		return
	}
	defer func() { _ = conn.Close() }()

	if _, err := conn.ExecContext(ctx, "SET statement_timeout = 0"); err != nil {
		slog.Warn("disable statement timeout for concurrent index build failed", "error", err.Error())
		return
	}
	// The conn returned by s.db.Conn goes back to the pool on Close, not to
	// the OS: pgx's stdlib driver does not run DISCARD ALL on release, so a
	// session-level SET here would otherwise leak the disabled timeout onto
	// whichever pooled connection this physical one becomes for its
	// lifetime. RESET restores the role/database startup value. Deferred
	// before any fallible step below so it always runs, and on a bounded
	// context detached from ctx's cancellation so a caller-cancelled ctx
	// cannot skip it — but, unlike context.Background(), also cannot hang
	// the shutdown path indefinitely on an unresponsive server.
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		if _, err := conn.ExecContext(cleanupCtx, "RESET statement_timeout"); err != nil {
			slog.Warn("reset statement timeout after concurrent index build failed", "error", err.Error())
		}
	}()

	// Every index on a table that can already be large in an existing archive
	// belongs here rather than inline in schema_pg.sql, where it would build
	// under the pool-wide statement_timeout and could fail InitSchema outright.
	concurrentIndexes := []struct{ name, definition string }{
		{"idx_messages_source_id", "ON messages(source_id, id)"},
		{"idx_participants_email_lower", "ON participants(LOWER(email_address))"},
		{"idx_participant_identifiers_value_lower", "ON participant_identifiers(LOWER(identifier_value))"},
	}
	for _, index := range concurrentIndexes {
		if dropErr := dropInvalidIndexConcurrently(ctx, conn, index.name); dropErr != nil {
			slog.Warn("drop invalid leftover index failed", "index", index.name, "error", dropErr.Error())
		}

		build := func() error {
			_, err := conn.ExecContext(ctx,
				`CREATE INDEX CONCURRENTLY IF NOT EXISTS `+index.name+` `+index.definition)
			return err
		}

		if err := build(); err != nil {
			slog.Warn("concurrent index build failed, checking for an invalid leftover to retry",
				"index", index.name, "error", err.Error())
			if dropErr := dropInvalidIndexConcurrently(ctx, conn, index.name); dropErr != nil {
				slog.Warn("drop invalid leftover index failed", "index", index.name, "error", dropErr.Error())
			}
			if err := build(); err != nil {
				slog.Warn("concurrent index build failed after retry; will retry on next start",
					"index", index.name, "error", err.Error())
			}
		}
	}
}

// dropInvalidIndexConcurrently drops indexName only if PostgreSQL left it in
// the INVALID state (a CREATE INDEX CONCURRENTLY that failed or was
// cancelled partway through). A valid index that failed to build for some
// other transient reason (e.g. a concurrent CREATE already in flight) is
// left alone rather than dropped. The namespace check keeps this from
// matching an INVALID index of the same name in a different schema — the
// unqualified build/drop below both resolve the bare name via search_path,
// so this must resolve against that same current_schema() to stay
// consistent with what they will actually touch.
func dropInvalidIndexConcurrently(ctx context.Context, conn *sql.Conn, indexName string) error {
	var invalid bool
	if err := conn.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1
			FROM pg_class c
			JOIN pg_index i ON i.indexrelid = c.oid
			WHERE c.relname = $1
			  AND c.relnamespace = current_schema()::regnamespace
			  AND i.indisvalid = false
		)
	`, indexName).Scan(&invalid); err != nil {
		return fmt.Errorf("check invalid index: %w", err)
	}
	if !invalid {
		return nil
	}
	if _, err := conn.ExecContext(ctx, `DROP INDEX CONCURRENTLY IF EXISTS `+indexName); err != nil {
		return fmt.Errorf("drop invalid index: %w", err)
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
	QueryContext(ctx context.Context, query string, args ...any) (*loggedRows, error)
	Exec(query string, args ...any) (sql.Result, error)
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

func queryInChunks[T any](db chunkQuerier, ids []T, prefixArgs []any, queryTemplate string, fn func(*loggedRows) error) error {
	return queryInChunksContext(context.Background(), db, ids, prefixArgs, queryTemplate, fn)
}

func queryInChunksContext[T any](ctx context.Context, db chunkQuerier, ids []T, prefixArgs []any, queryTemplate string, fn func(*loggedRows) error) error {
	const chunkSize = 500
	for i := 0; i < len(ids); i += chunkSize {
		if err := ctx.Err(); err != nil {
			return err
		}

		end := min(i+chunkSize, len(ids))
		chunk := ids[i:end]

		placeholders := make([]string, len(chunk))
		args := slices.Clone(prefixArgs)
		for j, id := range chunk {
			placeholders[j] = "?"
			args = append(args, id)
		}

		query := fmt.Sprintf(queryTemplate, strings.Join(placeholders, ","))
		rows, err := db.QueryContext(ctx, query, args...)
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
func insertInChunks(tx interface {
	Exec(query string, args ...any) (sql.Result, error)
}, c chunkInsert, valueBuilder func(start, end int) ([]string, []any)) error {
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
	return execInChunksContext(context.Background(), db, ids, prefixArgs, queryTemplate)
}

// execInChunksContext is the context-aware form of execInChunks: every chunk's
// statement carries ctx, and the walk stops at the next chunk boundary once it
// is cancelled. Callers whose write scales with archive size — the legacy
// calendar-attribution backfill in particular — must use this form, because a
// chunked whole-table walk on a background context is unreachable by SIGINT and
// SIGTERM for as long as any one chunk waits on a lock.
func execInChunksContext[T any](
	ctx context.Context, db chunkQuerier, ids []T, prefixArgs []any, queryTemplate string,
) error {
	const chunkSize = 500
	for i := 0; i < len(ids); i += chunkSize {
		if err := ctx.Err(); err != nil {
			return err
		}

		end := min(i+chunkSize, len(ids))
		chunk := ids[i:end]

		placeholders := make([]string, len(chunk))
		args := slices.Clone(prefixArgs)
		for j, id := range chunk {
			placeholders[j] = "?"
			args = append(args, id)
		}

		query := fmt.Sprintf(queryTemplate, strings.Join(placeholders, ","))
		if _, err := db.ExecContext(ctx, query, args...); err != nil {
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

// initSchemaWindowHook is a test-only seam. It fires at the point in InitSchema
// where a message inserted by another connection is at its most exposed: the
// one-time content_changed_at backfill has already run AND recorded itself in
// the migration ledger, so it will never look for NULL watermarks again, while
// the remaining whole-table index builds still have minutes of work to do on a
// large archive. A row that lands here has to be stamped by the INSERT trigger
// or it never appears in the change feed at all, which is why the watermark
// triggers are created before the backfill rather than after the indexes. Nil
// in production, and per-Store: see the field's declaration.

// InitSchema initializes the database schema, uninterruptibly.
//
// Kept for callers that have no context to offer — short-lived tooling and test
// fixtures. Anything with a cancellable context, and in particular anything that
// opens an archive on behalf of a signal-handling process, must use
// InitSchemaContext: on an existing archive this runs a one-time full-table
// backfill that can take hours, and a background context makes SIGINT and
// SIGTERM unable to reach it.
func (s *Store) InitSchema() error {
	return s.InitSchemaContext(context.Background())
}

// InitSchemaContext initializes the database schema, honouring ctx.
// This creates all tables if they don't exist.
//
// EVERY statement this method issues carries ctx — with no exceptions, which is
// the property to preserve rather than a summary of the current call list.
// Directly or through the transaction it runs in, that covers the schema
// scripts, the legacy ADD COLUMN migrations, every migration-ledger read and
// write (runOnceMigration owns them), the maintenance transactions and the
// dialect DDL they carry, the legacy phone-unique migration including the
// participant merge and its link-graph rewrite, the attribution backfill
// including its chunked calendar UPDATE, the FTS schema, the FTS index build,
// the FTS availability probe, the archive-UID transaction, and the default
// collection. That matters most on PostgreSQL, where each of them can queue
// behind a conflicting lock on a table an import is already writing — a
// statement issued on context.Background() ignores SIGINT and SIGTERM for as
// long as that lock is held, leaving SIGKILL mid-write as the operator's only
// move.
//
// The closure is mechanically checkable and was established that way rather
// than by reading: walk every call this method makes, transitively, and look
// for one that reaches loggedDB/loggedTx Exec, Query, QueryRow or Begin, each
// of which substitutes context.Background(). Anything a helper hands to a
// dialect must be a boundQuerier, not the raw transaction. Three earlier rounds
// each bound part of this method and each left this comment claiming the whole
// of it; if you add a step, run that walk again before you touch this
// paragraph.
//
// Cancelling ctx stops the one-time content_changed_at backfill at the batch
// boundary it reaches. Every batch already committed is kept, the migration
// ledger is NOT marked, and the next open resumes where this one stopped — so an
// interrupted upgrade costs the batch in flight and nothing else. The same holds
// for the other ledger-gated migrations: a cancelled one is not marked applied,
// so the next open runs it again.
func (s *Store) InitSchemaContext(ctx context.Context) error {
	// Load and execute schema files provided by the dialect.
	for _, filename := range s.dialect.SchemaFiles() {
		schema, err := schemaFS.ReadFile(filename)
		if err != nil {
			return fmt.Errorf("read %s: %w", filename, err)
		}
		// ExecContext, not Exec: Exec substitutes context.Background(), and on
		// PostgreSQL these statements queue behind any conflicting lock on the
		// tables they touch, so a background context makes SIGINT and SIGTERM
		// unable to reach an upgrade that has not started moving yet.
		if _, err := s.db.ExecContext(ctx, string(schema)); err != nil {
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
	if err := s.runOnceMigration(
		ctx, migrationAttachmentsContentHashUnique, false,
		func(ctx context.Context) error {
			return s.runMaintenance(ctx, func(ctx context.Context, tx *loggedTx) error {
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
			})
		},
	); err != nil {
		return err
	}

	// The source-support tables were added after generated identity candidates
	// and evidence. Preserve old rows before source-removal cleanup starts using
	// support absence as proof that a row is stale.
	if err := s.ensureIdentityMatchCandidateSourceSupportColumns(ctx); err != nil {
		return fmt.Errorf("prepare identity match source support provenance: %w", err)
	}
	if err := s.runOnceMigration(
		ctx, migrationIdentityMatchSourceSupport, false,
		func(ctx context.Context) error {
			return s.runMaintenance(ctx, func(ctx context.Context, tx *loggedTx) error {
				return s.backfillLegacyIdentityMatchSourceSupport(ctx, tx)
			})
		},
	); err != nil {
		return err
	}

	// Legacy databases may have idx_participants_phone as a non-unique
	// partial index (it was created that way before the schema flipped
	// to UNIQUE). `CREATE UNIQUE INDEX IF NOT EXISTS` in schema.sql
	// silently leaves the non-unique index in place, so
	// EnsureParticipantByPhone's ON CONFLICT (phone_number) finds no
	// matching unique constraint on upgraded DBs. Run a one-shot
	// migration that dedupes phone rows, drops the index, and
	// recreates it as UNIQUE.
	if err := s.ensureParticipantsPhoneUniqueIndex(ctx); err != nil {
		return fmt.Errorf("ensure idx_participants_phone unique: %w", err)
	}

	// Seed the open communication-service catalog. The catalog is
	// presentation and normalization metadata, not an enum: unknown bridges
	// are registered as rows at runtime. The migration ledger prevents
	// startup from fighting later user edits or deletions.
	if err := s.seedCommunicationServices(ctx); err != nil {
		return fmt.Errorf("seed communication services: %w", err)
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
	//
	// Bound to ctx for the same reason as the schema scripts above: ALTER TABLE
	// takes an exclusive lock and waits for one, and this loop is where
	// content_changed_at itself arrives on an upgraded archive.
	lastModifiedColumnAdded := false
	for _, m := range s.dialect.LegacyColumnMigrations() {
		if _, err := s.db.ExecContext(ctx, m.SQL); err != nil {
			if !s.dialect.IsDuplicateColumnError(err) {
				return fmt.Errorf("migrate schema (%s): %w", m.Desc, err)
			}
		} else if m.Desc == "last_modified" && !s.IsPostgreSQL() {
			lastModifiedColumnAdded = true
		}
	}
	// Recovery reads only acceptances whose link transaction may not have
	// committed. Create this after the legacy-column loop: schema.sql is
	// executed before that loop, so an upgraded table does not have the column
	// yet when its CREATE TABLE IF NOT EXISTS is skipped.
	if _, err := s.db.ExecContext(ctx, `
		CREATE INDEX IF NOT EXISTS idx_identity_match_candidates_application_pending
		ON identity_match_candidates(application_pending, id)
		WHERE state = 'accepted'
	`); err != nil {
		return fmt.Errorf("index pending identity match applications: %w", err)
	}
	if err := s.ensureParticipantIdentifierServiceScopeIndex(ctx); err != nil {
		return fmt.Errorf("create participant identifier service-scope index: %w", err)
	}

	// This one-shot backfill must run after LegacyColumnMigrations so upgraded
	// databases have the nullable service/scope columns before they are read.
	if err := s.ensureParticipantIdentifierServiceScope(ctx); err != nil {
		return fmt.Errorf("classify participant identifier service scope: %w", err)
	}

	// Stable source-part identity supersedes content hash for attachment rows
	// that have it. The legacy hash index remains only for rows whose source
	// cannot provide an occurrence key. This runs after the column migrations
	// because upgraded archives do not have source_part_key before this point.
	if err := s.ensureAttachmentOccurrenceUniqueIndexes(ctx); err != nil {
		return err
	}

	// Create the message watermark, contextual embedding journal, and attachment
	// change journal triggers. This must run after the migration loop above,
	// which adds the legacy columns referenced by those triggers.
	//
	// It runs HERE, immediately after the columns exist, rather than at the end
	// of the upgrade: everything below is index builds and whole-table backfills
	// that take minutes on a large archive, and until the INSERT trigger exists
	// a message written by another connection lands with content_changed_at
	// NULL. That is terminal — the feed's range predicate excludes NULL and the
	// backfill's ledger sentinel means it never runs again — so the row would
	// never appear in the change feed. Ordering the triggers first makes the
	// INSERT trigger the writer for those rows and leaves the backfill's
	// `WHERE content_changed_at IS NULL` correctly finding nothing to do for
	// them.
	//
	// Safe with respect to the backfills below. Neither is re-stamped by the
	// content_changed_at triggers: those are scoped to INSERT or to
	// `UPDATE OF <content columns>`, and content_changed_at is not one of those
	// columns, so a statement naming only the watermark never matches. The
	// last_modified backfill writes a value that differs from the old NULL,
	// which is what both dialects' last_modified triggers yield to. The reverse
	// interaction — the content_changed_at backfill tripping the last_modified
	// trigger — is now backend-specific and documented at that backfill: on
	// PostgreSQL the trigger is blanket and still fires; on SQLite its UPDATE OF
	// scope excludes content_changed_at, so a statement naming only the
	// watermark no longer bumps last_modified.
	//
	// On SQLite this covers the content_changed_at triggers plus the messages
	// last_modified trigger, which needs an UPDATE OF scope built from the live
	// column list (see lastModifiedUpdateOfColumns) and so cannot be static SQL;
	// only the message_bodies last_modified pair still rides schema.sql. On
	// PostgreSQL it covers both sets.
	// Both dialects drop and recreate. The versioned migration makes that repair
	// once per trigger definition instead of taking trigger locks on every open;
	// bump the migration name whenever the definition or tracked column list
	// changes. Run under runMaintenance for consistency with EnsureFTSIndex (no
	// statement_timeout cap on the DDL).
	//
	// The dialect gets the transaction BOUND to ctx, not the raw one: runMaintenance
	// has just disabled the pool-wide statement_timeout, and on PostgreSQL a DROP or
	// CREATE TRIGGER queues behind any conflicting lock on `messages` with nothing
	// left to cut it off. Handed the raw transaction, whose Exec and QueryRow bottom
	// out in context.Background(), that wait ignores SIGINT and SIGTERM for as long
	// as the lock is held.
	if err := s.dialect.ValidateMessageWatermarks(boundQuerier{ctx: ctx, q: s.db}); err != nil {
		return fmt.Errorf("validate message watermarks: %w", err)
	}
	watermarkTriggersAlreadyApplied, err := s.IsMigrationAppliedContext(
		ctx, migrationMessageWatermarkTriggers)
	if err != nil {
		return fmt.Errorf("check message watermark trigger migration: %w", err)
	}
	if err := s.runOnceMigration(
		ctx, migrationMessageWatermarkTriggers, false,
		func(ctx context.Context) error {
			return s.runMaintenance(ctx, func(ctx context.Context, tx *loggedTx) error {
				return s.dialect.EnsureTriggers(boundQuerier{ctx: ctx, q: tx})
			})
		},
	); err != nil {
		return fmt.Errorf("ensure message watermark triggers: %w", err)
	}
	if err := s.runOnceMigration(
		ctx, migrationEmbeddingChangeJournalTriggers, false,
		func(ctx context.Context) error {
			// A fresh archive (or a pre-watermark archive) just ran the current
			// EnsureTriggers above, which already includes the journal definitions.
			// Only an archive that entered this InitSchema with watermark v1 applied
			// needs a second pass to upgrade its existing trigger set.
			if !watermarkTriggersAlreadyApplied {
				return nil
			}
			return s.runMaintenance(ctx, func(ctx context.Context, tx *loggedTx) error {
				return s.dialect.EnsureTriggers(boundQuerier{ctx: ctx, q: tx})
			})
		},
	); err != nil {
		return fmt.Errorf("ensure embedding change journal triggers: %w", err)
	}
	if err := s.runOnceMigration(
		ctx, migrationActivityProjectionTriggers, false,
		func(ctx context.Context) error {
			return s.runMaintenance(ctx, func(ctx context.Context, tx *loggedTx) error {
				return s.dialect.EnsureActivityProjectionTriggers(
					boundQuerier{ctx: ctx, q: tx},
				)
			})
		},
	); err != nil {
		return fmt.Errorf("ensure activity projection triggers: %w", err)
	}

	// Initialize explicit attribution provenance for every legacy message once
	// under the maintenance timeout escape hatch. Granola and Circleback
	// historically derived is_from_me from confirmed organizer identities.
	// Google Calendar combined that signal with Organizer.Self, so its archived
	// event payload separates source-native ownership from identity-derived
	// ownership. Other providers' existing values are source-native. Runtime
	// identity mutations can then update only rows whose derived attribution
	// actually changes instead of rewriting an entire source to initialize NULL
	// provenance.
	if err := s.runOnceMigration(
		ctx, migrationMessageAttributionProvenance, false,
		func(ctx context.Context) error {
			return s.runMaintenance(
				ctx,
				func(ctx context.Context, tx *loggedTx) error {
					if err := backfillLegacyMessageAttributionProvenance(ctx, tx); err != nil {
						return err
					}

					// Published message shards used to trust the effective
					// is_from_me value. The provenance migration changes cache
					// inputs even when no identity is added or removed, so advance
					// the account-identity revision in the same transaction as the
					// repaired rows. Empty archives have no stale shards to
					// invalidate.
					var hasMessages bool
					if err := tx.QueryRowContext(
						ctx,
						`SELECT EXISTS (SELECT 1 FROM messages)`,
					).Scan(&hasMessages); err != nil {
						return fmt.Errorf("check attribution migration cache impact: %w", err)
					}
					if !hasMessages {
						return nil
					}
					return s.bumpAccountIdentityRevisionContext(ctx, tx)
				},
			)
		},
	); err != nil {
		return err
	}

	// Relax message_recipients uniqueness to include the normalized envelope
	// address, so alias snapshots of one participant can coexist per message.
	// Must run after the legacy ADD COLUMN loop above (the unique index
	// expression reads email_address) and before any participant merge under
	// the new email-aware collision rules can run.
	if err := s.ensureRecipientEnvelopeUniqueIndex(ctx); err != nil {
		return fmt.Errorf("ensure idx_message_recipients_envelope unique: %w", err)
	}

	// Identity discovery scans one source in message-ID order. On SQLite the
	// plain idx_messages_source index already orders ties by rowid, so no
	// separate composite index is needed there (see schema.sql). PostgreSQL
	// still needs the explicit composite index, built via CREATE INDEX
	// CONCURRENTLY on a dedicated connection so a one-time build over an
	// existing archive never blocks writers or needs the pool-wide
	// statement_timeout escape hatch (CONCURRENTLY cannot run inside a
	// transaction at all, so runMaintenance does not apply here). Carries
	// ctx like every other statement in this method: a cancelled build
	// leaves at worst an INVALID leftover, which the next start drops and
	// rebuilds.
	s.buildLargeIndexesConcurrently(ctx)

	// Partial expression indexes for live-message listing and date filtering.
	// The first is a covering index for the ListMessages page
	// (GET /api/v1/messages).
	// That query counts and paginates live messages ordered by
	// COALESCE(sent_at, received_at, internal_date) DESC, id DESC. Without an
	// index matching both the live-messages predicate and that sort key, SQLite
	// falls back to a full scan of the messages table (multiple GB on a large
	// archive) plus a temp-B-tree sort for every page — measured at seconds per
	// 5-row page. The partial expression index lets COUNT read only the compact
	// index and lets the page query walk it in order and stop at LIMIT,
	// eliminating both the full scan and the sort (~29x faster COUNT, no sort).
	// The second serves query-engine filters that compare julianday(sent_at) so
	// mixed-offset timestamps remain correct without scanning the plain sent_at
	// index. The third matches SearchMessages' COALESCE-based Julian-day
	// predicate; without it, date-bounded counts scan every live message.
	//
	// Runs after the legacy ADD COLUMN loop above so deleted_at /
	// deleted_from_source_at exist on upgraded DBs. SQLite only: PostgreSQL
	// autovacuum keeps planner statistics current and picks its own plan, and
	// the index expression syntax differs; the measured regression is specific
	// to the statistics-free SQLite archive. Built under runMaintenance so the
	// one-time index build over a large table is not cut off by the pool-wide
	// 30s statement_timeout (finding S1). IF NOT EXISTS is idempotent per start.
	if !s.IsPostgreSQL() {
		if err := s.runMaintenance(ctx, func(ctx context.Context, tx *loggedTx) error {
			if _, err := tx.ExecContext(ctx, `
				CREATE INDEX IF NOT EXISTS idx_messages_live_sent_at
				    ON messages(COALESCE(sent_at, received_at, internal_date) DESC, id DESC)
				    WHERE deleted_at IS NULL AND deleted_from_source_at IS NULL
			`); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `
				CREATE INDEX IF NOT EXISTS idx_messages_sent_at_julianday
				    ON messages(julianday(sent_at))
			`); err != nil {
				return err
			}
			_, err := tx.ExecContext(ctx, `
				CREATE INDEX IF NOT EXISTS idx_messages_live_timestamp_julianday
				    ON messages(julianday(COALESCE(sent_at, received_at, internal_date)))
				    WHERE deleted_at IS NULL AND deleted_from_source_at IS NULL
			`)
			return err
		}); err != nil {
			return fmt.Errorf("create live message timestamp indexes: %w", err)
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
		if err := s.runMaintenance(ctx, func(ctx context.Context, tx *loggedTx) error {
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

	// Indexes over attachment thumbnail hash/path serve pack enumeration and
	// remove-account's per-candidate reference checks. The LOWER(hash)
	// expression indexes serve alias-aware pack reads, pruning, and repack
	// liveness without scanning the full attachments table for every packed
	// entry. PostgreSQL only here:
	// schema_pg.sql executes before any maintenance hatch is available, and on
	// an existing large archive the one-time builds can exceed the pool-wide
	// 30s statement_timeout, so they run under runMaintenance (finding S1) like
	// the other one-time index builds above. SQLite keeps both in schema.sql —
	// it has no statement_timeout. IF NOT EXISTS is idempotent per start.
	if s.IsPostgreSQL() {
		if err := s.runMaintenance(ctx, func(ctx context.Context, tx *loggedTx) error {
			if _, err := tx.ExecContext(ctx, `
				CREATE INDEX IF NOT EXISTS idx_attachments_thumbnail_hash
				    ON attachments(thumbnail_hash)
			`); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `
				CREATE INDEX IF NOT EXISTS idx_attachments_thumbnail_path
				    ON attachments(thumbnail_path)
			`); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `
				CREATE INDEX IF NOT EXISTS idx_attachments_content_hash_lower
				    ON attachments(LOWER(content_hash))
			`); err != nil {
				return err
			}
			_, err := tx.ExecContext(ctx, `
				CREATE INDEX IF NOT EXISTS idx_attachments_thumbnail_hash_lower
				    ON attachments(LOWER(thumbnail_hash))
			`)
			return err
		}); err != nil {
			return fmt.Errorf("create attachment lookup indexes: %w", err)
		}
	}

	// Index over rfc822_message_id serves dedup's per-group message lookup
	// (GetDuplicateGroupMessages / GetDuplicateGroupMessagesBatch). Without
	// it, each lookup was a full scan of the messages table — measured at
	// ~190ms/lookup, with one lookup per duplicate group, so a scan with
	// 22k groups burned the entire 30-minute CLI plan-request timeout
	// before content-hash comparison even started (kenn-io/msgvault#510).
	// Plain (non-partial) index: a partial WHERE rfc822_message_id IS NOT
	// NULL AND != '' form is not usable by the planner for this table's
	// bound `= ?` / `IN (...)` lookups — SQLite can't prove col = ? implies
	// col != '' since ? could bind to '' — so it would silently fall back
	// to SCAN (verified via EXPLAIN QUERY PLAN before writing this).
	// Identical DDL on both backends; runMaintenance already handles the
	// PostgreSQL statement_timeout exemption internally (finding S1). IF
	// NOT EXISTS is idempotent per start.
	if err := s.runMaintenance(ctx, func(ctx context.Context, tx *loggedTx) error {
		_, err := tx.ExecContext(ctx, `
			CREATE INDEX IF NOT EXISTS idx_messages_rfc822_message_id
			    ON messages(rfc822_message_id)
		`)
		return err
	}); err != nil {
		return fmt.Errorf("create rfc822 message id index: %w", err)
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
	if err := s.runOnceMigration(
		ctx, migrationMessagesLastModifiedBackfill, lastModifiedColumnAdded,
		func(ctx context.Context) error {
			if err := s.runMaintenance(ctx, func(ctx context.Context, tx *loggedTx) error {
				_, err := tx.ExecContext(ctx,
					`UPDATE messages SET last_modified = `+s.dialect.Now()+
						` WHERE last_modified IS NULL`)
				return err
			}); err != nil {
				return fmt.Errorf("backfill last_modified: %w", err)
			}
			return nil
		},
	); err != nil {
		return err
	}

	// Backfill content_changed_at for rows that predate the column, in
	// committed batches (backfillContentChangedAt). Gated on the ledger
	// because the scan never finds work after the first completed run; no
	// forced-rerun path is needed (unlike last_modified's) because the column
	// and its sentinel ship in the same release. The ledger is marked only
	// after every batch has committed, so an interrupted upgrade re-enters the
	// loop on the next open rather than declaring itself done.
	if err := s.runOnceMigration(
		ctx, migrationMessagesContentChangedAtBackfill, false,
		func(ctx context.Context) error {
			if err := s.backfillContentChangedAt(ctx); err != nil {
				return fmt.Errorf("backfill content_changed_at: %w", err)
			}
			return nil
		},
	); err != nil {
		return err
	}

	if s.initSchemaWindowHook != nil {
		s.initSchemaWindowHook()
	}

	// Keyset index for the content-change feed, on both backends. Composite
	// (content_changed_at, id) because the feed's cursor is exactly that pair.
	// Created here rather than in the schema files because those run before the
	// ADD COLUMN migration above (cr2-10). Under runMaintenance: the one-time
	// build over a large archive can exceed the pool-wide statement_timeout.
	if err := s.runMaintenance(ctx, func(ctx context.Context, tx *loggedTx) error {
		_, err := tx.ExecContext(ctx,
			`CREATE INDEX IF NOT EXISTS idx_messages_content_changed_at
			     ON messages(content_changed_at, id)`)
		return err
	}); err != nil {
		return fmt.Errorf("create content_changed_at index: %w", err)
	}

	// Create FTS indexes that depend on columns just added by the legacy
	// migrations (PostgreSQL's GIN index on messages.search_fts). No-op on
	// SQLite. Must run after the migration loop above. [cr2-10]
	//
	// Run under runMaintenance: the GIN build over a populated messages
	// table can exceed the pool-wide 30s statement_timeout on a large
	// archive, so the maintenance hatch disables it for this tx (finding
	// S1). No-op timeout reset on SQLite.
	//
	// The dialect gets the transaction BOUND to ctx, not the raw one, for the
	// same reason the trigger DDL above does: runMaintenance has just disabled
	// the pool-wide statement_timeout, and a GIN or partial index build queued
	// behind a conflicting lock on `messages` has nothing else left to cut it
	// off. Handed the raw transaction, whose Exec bottoms out in
	// context.Background(), that wait ignores SIGINT and SIGTERM for as long as
	// the lock is held.
	if err := s.runMaintenance(ctx, func(ctx context.Context, tx *loggedTx) error {
		return s.dialect.EnsureFTSIndex(boundQuerier{ctx: ctx, q: tx})
	}); err != nil {
		return fmt.Errorf("ensure FTS index: %w", err)
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
	if err := s.runMaintenance(ctx, func(ctx context.Context, tx *loggedTx) error {
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
		// Bound to ctx like the rest of the DDL above. A cancellation here is
		// not a missing module: IsNoSuchModuleError does not match it, so it is
		// reported rather than swallowed by the fall-through below.
		if _, err := s.db.ExecContext(ctx, string(ftsSchema)); err != nil {
			if !s.dialect.IsNoSuchModuleError(err) {
				return fmt.Errorf("init FTS schema: %w", err)
			}
			// Module not compiled in; availability stays false. Fall
			// through so the rest of schema init still runs.
		}
	}
	if err := s.ensureArchiveUIDContext(ctx); err != nil {
		return err
	}

	// Probe availability through the dialect so it works uniformly for
	// backends that carry FTS inside their main schema. The probe is a query
	// like any other and carries ctx: it returns a bool, so a cancellation that
	// was not reported as an error would be recorded as "search is unavailable"
	// on a store the daemon is about to hand to callers.
	available, err := s.dialect.FTSAvailable(ctx, s.db.DB)
	if err != nil {
		return fmt.Errorf("probe FTS availability: %w", err)
	}
	s.fts5Available = available

	if err := s.EnsureSeededAttributeDefinitionsContext(ctx); err != nil {
		return fmt.Errorf("ensure seeded attribute definitions: %w", err)
	}

	// Reconcile the system relationship type catalog on every open: insert
	// missing seeds, repair structural drift, and leave user-owned labels,
	// vCard mappings, colours, icons, and descriptions alone. See
	// EnsureSeededRelationshipTypes for the two column classes.
	if err := s.EnsureSeededRelationshipTypesContext(ctx); err != nil {
		return err
	}

	// Ensure the default "All" collection exists and contains every source.
	if err := s.EnsureDefaultCollectionContext(ctx); err != nil {
		return fmt.Errorf("ensure default collection: %w", err)
	}

	return nil
}

// runOnceMigration runs fn at most once per archive, gated on the
// applied_migrations ledger: fn runs when the ledger has no entry for name (or
// when force overrides that), and the entry is written only after fn returns
// successfully, so a migration that failed or was cancelled runs again on the
// next open.
//
// It is the single owner of the ledger statements for every one-time step in
// InitSchemaContext, and both of them carry ctx. That is not incidental. The
// read is a gate: answered on a cancelled context it lets the whole migration
// below it run to completion after the operator asked the daemon to stop. The
// write is worse: reached on a cancelled context it stamps "done" on a
// migration that was cut off, and every later open skips it. Keeping both in
// one place is what stops the next migration added to InitSchemaContext from
// reintroducing a contextless pair.
func (s *Store) runOnceMigration(
	ctx context.Context, name string, force bool, fn func(ctx context.Context) error,
) error {
	applied, err := s.IsMigrationAppliedContext(ctx, name)
	if err != nil {
		return err
	}
	if applied && !force {
		return nil
	}
	if err := fn(ctx); err != nil {
		return err
	}
	return s.MarkMigrationAppliedContext(ctx, name)
}

// contentChangedBackfillBatchSize is how many ROWS one backfill batch stamps —
// rows needing the stamp, not ids walked past. Same 5000 the FTS backfill uses
// (backfillFTSRangeContext): large enough that the per-transaction overhead
// disappears against the row work, small enough that one batch's transaction is
// short and its PostgreSQL dead tuples are reclaimable by autovacuum while the
// next batches run. Nothing overrides it in production; a test shrinks it for
// one Store at a time through the per-Store override, never by writing here.
const contentChangedBackfillBatchSize int64 = 5000

// contentChangedBackfillBatch is the batch size this Store's backfill uses:
// the production default unless a test has overridden it for this Store.
func (s *Store) contentChangedBackfillBatch() int64 {
	if s.contentChangedBackfillBatchSizeOverride > 0 {
		return s.contentChangedBackfillBatchSizeOverride
	}
	return contentChangedBackfillBatchSize
}

// contentChangedBackfillBatchHook is a test-only seam: when non-nil it is
// consulted before each batch with the batch's first and last id (inclusive),
// and a non-nil return aborts the backfill at that batch boundary. It lets a
// test interrupt an upgrade exactly between two committed batches — the case
// that decides whether the backfill is resumable — without racing a real crash,
// and it lets a test count the transactions an archive's shape costs. Nil (and
// thus a no-op) in production; only export_test.go ever sets it, and it is
// per-Store: see the field's declaration.

// backfillContentChangedAt seeds content_changed_at on rows that predate the
// column, walking the rows that still need it in committed batches.
//
// Seeded from last_modified rather than "now": an existing archive's
// last_modified is a reasonable estimate of when the row last moved, so each
// backfilled row reports a change time close to its real one. It does NOT
// shorten the first walk — an empty cursor selects every non-NULL watermark, so
// a first-run consumer still receives the whole archive, oldest change first,
// which is what an empty cursor means. What "now" would cost instead is the
// meaning: every pre-existing row would report the upgrade instant as its
// change time, one undifferentiated block, and a consumer resuming mid-walk
// would be paging by id through a single instant rather than through history.
// SQLite runs the seed through strftime so a legacy last_modified in any other
// textual form lands in the one format the triggers write -- its cursor
// comparison is lexical, and a single stray "2024-03-04T05:06:07Z" would sort
// into the wrong place and be skipped or repeated forever. PostgreSQL compares
// TIMESTAMPTZ natively and needs no normalisation.
//
// Batched rather than one whole-table UPDATE, on both backends, for two
// reasons that matter on a large archive at daemon startup:
//
//   - Resumability. Each batch is its own committed transaction, so a restart
//     mid-upgrade keeps every row already stamped and the next run does the
//     remainder. A single transaction over the whole table would roll all of it
//     back, so an archive big enough to take longer than the operator's
//     patience could never finish -- and on PostgreSQL each abandoned attempt
//     would leave a full table's worth of dead tuples behind.
//   - Bounded cost per transaction. PostgreSQL's MVCC rewrites every row it
//     updates, so one whole-table statement holds a transaction open for the
//     entire rewrite, doubles the table's on-disk size until VACUUM catches up
//     (autovacuum cannot reclaim anything while the transaction is open), and
//     writes WAL proportional to the whole table before a single byte is
//     reclaimable. SQLite has no MVCC bloat, but a whole-table UPDATE there
//     holds the single write lock for its full duration, blocking every writer
//     in the process, and grows the WAL by the size of the change before the
//     commit lets a checkpoint truncate it.
//
// Each batch runs under runMaintenance so PostgreSQL's pool-wide 30s
// statement_timeout does not cut a batch off (no-op reset on SQLite).
//
// The walk is a KEYSET walk over the rows that still need work — the next batch
// is "the first N rows with `content_changed_at IS NULL` above the last id
// stamped", ordered by id — rather than a march through the numeric span from
// MIN(id) to MAX(id). It carries the last id seen, so it never adds anything to
// an id and cannot overflow int64 at the top of the id space (where a computed
// batch end wraps negative, matches nothing, and wraps the cursor instead of
// terminating), it makes no assumption about which ids are legal (0 and negative
// ones both are: SQLite's INTEGER PRIMARY KEY is the rowid and the schema
// constrains it no further), and it costs one transaction per batch of ROWS
// rather than one per batch of ids — so a sparse archive, which is what a
// long-lived one looks like after deletions and subset copies, does not pay
// millions of empty transactions at daemon startup before the port is bound.
//
// Re-entry is correct because the predicate is `content_changed_at IS NULL`:
// an already-stamped row is selected by no batch, so a resumed run rewrites
// nothing it already did, and a row stamped by the INSERT trigger or column
// DEFAULT while the backfill is running is left alone. It is not free: the walk
// keeps its cursor in memory only, so a resumed run reads once past the stamped
// prefix before it reaches the remaining work (see the batch loop).
//
// On PostgreSQL each batch bumps last_modified on every row it touches, once,
// at upgrade: that trigger fires on any UPDATE leaving last_modified alone, and
// this statement names only content_changed_at. It cannot be suppressed from
// here — the trigger yields only to a statement writing a DIFFERENT
// last_modified, so preserving the old value is not expressible, and dropping
// the trigger around the backfill would leave the embed worker's CAS token
// unmaintained if the upgrade were interrupted. The consequence is bounded —
// embedding candidate selection keys off embed_gen, not last_modified, and the
// bump happens once per archive — so it is documented rather than worked
// around.
//
// SQLite does not bump: its last_modified trigger is scoped
// `UPDATE OF <every column except content_changed_at>`, which it must be so the
// content_changed_at stamp cannot clobber an explicit last_modified write (see
// lastModifiedUpdateOfColumns). This statement names only the watermark, so it
// no longer matches.
func (s *Store) backfillContentChangedAt(ctx context.Context) error {
	// The batch is chosen by reading ids first, then stamping exactly the range
	// they span. Selecting the ids is what makes the walk a keyset walk; the
	// UPDATE can then use a plain BETWEEN because the selected ids are, by
	// construction, every unstamped row in [first,last] -- they are a
	// contiguous run of the unstamped set in id order -- so a bounded range
	// beats an N-element IN list and matches nothing extra.
	//
	// The first batch has no lower bound at all rather than starting from some
	// smallest-imaginable id: an id of 0, or a negative one, is legal, and
	// there is no sentinel below math.MinInt64 to start a `>` comparison from.
	firstBatchSQL := s.dialect.Rebind(
		`SELECT id FROM messages WHERE content_changed_at IS NULL ORDER BY id LIMIT ?`)
	nextBatchSQL := s.dialect.Rebind(
		`SELECT id FROM messages WHERE content_changed_at IS NULL AND id > ? ORDER BY id LIMIT ?`)

	// The outer COALESCE is load-bearing, not belt-and-braces. strftime
	// returns NULL -- not an error -- for any input its parser rejects: a unix
	// epoch integer, an empty string, free text. last_modified is an untyped
	// SQLite DATETIME column, so such a value is storable, and the resulting
	// NULL watermark is terminal: the feed's range predicate excludes NULL and
	// the caller's ledger gate means this scan never runs again. Falling back
	// to "now" keeps the row in the feed at the cost of one over-fresh
	// watermark.
	stampSQL := `UPDATE messages
	             SET content_changed_at = COALESCE(
	                     strftime('%Y-%m-%d %H:%M:%f', last_modified),
	                     strftime('%Y-%m-%d %H:%M:%f', 'now'))
	             WHERE content_changed_at IS NULL AND id >= ? AND id <= ?`
	if s.IsPostgreSQL() {
		stampSQL = `UPDATE messages
		            SET content_changed_at = COALESCE(last_modified, clock_timestamp())
		            WHERE content_changed_at IS NULL AND id >= ? AND id <= ?`
	}
	stampSQL = s.dialect.Rebind(stampSQL)

	batchSize := s.contentChangedBackfillBatch()
	started := time.Now()
	lastLog := started
	var stamped int64
	var cursor int64
	for batch := 0; ; batch++ {
		// Selecting the batch inside the batch's own transaction, not before
		// it, so it runs under the same maintenance hatch as the stamp. The
		// select is not always cheap: a RESUMED run starts its walk at the
		// bottom of the table again -- there is nowhere durable to have kept
		// the cursor -- so its first statement reads past every row the
		// previous run already stamped, which on a large archive can exceed
		// PostgreSQL's pool-wide 30s statement_timeout and cancel the upgrade
		// (SQLSTATE 57014). There is no index to lean on either: the one on
		// content_changed_at is built later in InitSchema, after this walk.
		var firstID, lastID, count, affected int64
		if err := s.runMaintenance(ctx, func(ctx context.Context, tx *loggedTx) error {
			var err error
			firstID, lastID, count, err = nextContentChangedBackfillBatch(
				ctx, tx, firstBatchSQL, nextBatchSQL, batch == 0, cursor, batchSize)
			if err != nil {
				return err
			}
			if count == 0 {
				return nil
			}
			if s.contentChangedBackfillBatchHook != nil {
				// Before the stamp, so a test that aborts here aborts a batch
				// that has written nothing -- the transaction rolls back and
				// the committed batches before it are what an interrupted
				// upgrade leaves behind.
				if err := s.contentChangedBackfillBatchHook(firstID, lastID); err != nil {
					return err
				}
			}
			result, err := tx.ExecContext(ctx, stampSQL, firstID, lastID)
			if err != nil {
				return err
			}
			affected, err = result.RowsAffected()
			return err
		}); err != nil {
			return fmt.Errorf("batch above id %d: %w", cursor, err)
		}
		if count == 0 {
			break
		}
		stamped += affected
		cursor = lastID

		// Progress on the terms an operator watching a stalled startup needs:
		// how much has been stamped, and where the walk has reached. Time-
		// throttled rather than per-batch so a small archive logs nothing.
		if time.Since(lastLog) >= 30*time.Second {
			slog.Info("backfilling content_changed_at",
				slog.Int64("stamped", stamped),
				slog.Int64("stamped_to_id", lastID),
				slog.Duration("elapsed", time.Since(started)))
			lastLog = time.Now()
		}

		// A short batch is the last one: the SELECT found fewer unstamped rows
		// above the cursor than it asked for, so there are none beyond this
		// batch. Stopping here rather than on an empty batch saves the archive
		// one query per upgrade; rows inserted after this point are stamped by
		// the INSERT trigger, not by this walk.
		if count < batchSize {
			break
		}
	}
	if stamped > 0 {
		slog.Info("backfilled content_changed_at",
			slog.Int64("stamped", stamped),
			slog.Duration("elapsed", time.Since(started)))
	}
	return nil
}

// nextContentChangedBackfillBatch reads the next batch of ids needing the
// content_changed_at stamp: up to limit rows with a NULL watermark, in id
// order, above afterID (or from the start of the table when first is set).
// It returns the batch's first and last id and how many rows it spans; a count
// of zero means there is no work left above the cursor.
func nextContentChangedBackfillBatch(
	ctx context.Context,
	tx *loggedTx,
	firstBatchSQL, nextBatchSQL string,
	first bool,
	afterID, limit int64,
) (firstID, lastID, count int64, err error) {
	var rows *loggedRows
	if first {
		rows, err = tx.QueryContext(ctx, firstBatchSQL, limit)
	} else {
		rows, err = tx.QueryContext(ctx, nextBatchSQL, afterID, limit)
	}
	if err != nil {
		return 0, 0, 0, fmt.Errorf("select rows needing content_changed_at above id %d: %w", afterID, err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return 0, 0, 0, fmt.Errorf("scan message id needing content_changed_at: %w", err)
		}
		if count == 0 {
			firstID = id
		}
		lastID = id
		count++
	}
	if err := rows.Err(); err != nil {
		return 0, 0, 0, fmt.Errorf("read rows needing content_changed_at above id %d: %w", afterID, err)
	}
	return firstID, lastID, count, nil
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

func (s *Store) ensureAttachmentOccurrenceUniqueIndexes(ctx context.Context) error {
	return s.runOnceMigration(
		ctx,
		migrationAttachmentOccurrenceUnique,
		false,
		func(ctx context.Context) error {
			return s.runMaintenance(ctx, func(ctx context.Context, tx *loggedTx) error {
				if _, err := tx.ExecContext(ctx,
					`DROP INDEX IF EXISTS idx_attachments_msg_content_hash`); err != nil {
					return fmt.Errorf("drop legacy attachment hash identity index: %w", err)
				}
				if _, err := tx.ExecContext(ctx, `
					CREATE UNIQUE INDEX IF NOT EXISTS idx_attachments_msg_source_part
					    ON attachments(message_id, source_part_key)
					    WHERE source_part_key IS NOT NULL
				`); err != nil {
					return fmt.Errorf("create attachment source-part identity index: %w", err)
				}
				if _, err := tx.ExecContext(ctx, `
					CREATE UNIQUE INDEX IF NOT EXISTS idx_attachments_msg_content_hash
					    ON attachments(message_id, content_hash)
					    WHERE source_part_key IS NULL
					      AND content_hash IS NOT NULL
					      AND content_hash != ''
				`); err != nil {
					return fmt.Errorf("create legacy attachment hash identity index: %w", err)
				}
				return nil
			})
		},
	)
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
	return s.NeedsFTSBackfillQuickContext(context.Background())
}

// NeedsFTSBackfillQuickContext is the request-aware form of
// NeedsFTSBackfillQuick.
func (s *Store) NeedsFTSBackfillQuickContext(ctx context.Context) bool {
	if !s.fts5Available {
		return false
	}
	return s.dialect.FTSNeedsBackfillQuick(ctx, s.db.DB)
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

	// DatabaseSize: logical main-database page allocation for SQLite,
	// pg_database_size() for PostgreSQL.
	if size, err := s.dialect.DatabaseSize(ctx, s.db.DB, s.dbPath); err == nil {
		stats.DatabaseSize = size
	} else if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr
	}

	return stats, nil
}
