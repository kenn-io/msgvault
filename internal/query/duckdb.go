package query

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	_ "github.com/duckdb/duckdb-go/v2" // DuckDB driver (database/sql)
	"golang.org/x/sync/semaphore"

	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/internal/store"
)

// duckDBQueryConcurrency caps how many heavy analytic queries execute
// concurrently against the DuckDB engine. A single DuckDB query already
// parallelizes across every core (SET threads = GOMAXPROCS), so a handful of
// concurrent heavy queries starve the box — the F2 runaway-query incident
// pegged the daemon at ~1600% CPU. Waiters block on a context-aware weighted
// semaphore, so a cancelled or timed-out request releases its place in line
// instead of piling up.
const duckDBQueryConcurrency = 2

// DuckDBEngine implements Engine using DuckDB for fast Parquet queries.
// It uses a hybrid approach:
//   - DuckDB with Parquet for fast aggregate queries
//   - DuckDB's sqlite_scan for list queries (ListMessages, ListAccounts) — non-Windows only
//   - Direct SQLite for FTS search and message body retrieval (sqlite_scan can't use FTS5)
//
// On Windows, the sqlite_scanner extension is not available (DuckDB's extension
// repository does not publish MinGW builds). All SQLite queries route through
// sqliteEngine instead.
//
// Deletion handling: The Python ETL excludes deleted messages (deleted_from_source_at IS NOT NULL)
// when building Parquet files. However, messages deleted AFTER the Parquet build will still
// appear in aggregates until the next `build-parquet --full-rebuild`. For the full deletion
// index solution, see beads issue msgvault-ozj.
type DuckDBEngine struct {
	db               *sql.DB
	analyticsDir     string
	sqlitePath       string        // Path to SQLite database for sqlite_scan queries
	sqliteDB         *sql.DB       // Direct SQLite connection for FTS and body retrieval
	sqliteEngine     *SQLiteEngine // Reusable engine for FTS cache, created once if sqliteDB is set
	hasSQLiteScanner bool          // true if DuckDB's sqlite extension is loaded
	tempTableSeq     atomic.Uint64 // Unique suffix for temp tables to avoid concurrent collisions

	// querySem bounds concurrent heavy query execution (see
	// duckDBQueryConcurrency). Acquired at the top of each expensive public
	// method via acquireQuerySlot; cheap PK/detail lookups are not gated so
	// they are never starved behind a slow aggregate.
	querySem *semaphore.Weighted

	// optionalCols tracks which columns exist in each Parquet table's schema.
	// Used to gracefully handle stale cache files that lack newer columns
	// (e.g. phone_number, attachment_count, sender_id, message_type added in PR #160).
	// Map: table_name -> column_name -> exists_in_parquet
	//
	// Guarded by optColsMu because a long-running server (e.g. mcp-http) may
	// have the analytics cache rebuilt underneath it by build-cache/sync. When
	// that happens the column set can change, so optionalCols is re-probed on
	// demand (see ensureFreshOptionalCols) rather than only at construction.
	optColsMu    sync.RWMutex
	optionalCols map[string]map[string]bool
	cacheFP      string // fingerprint of the Parquet cache at last probe

	// Search result cache: keeps the materialized temp table alive across
	// pagination calls for the same search query, avoiding repeated Parquet scans.
	searchCacheMu    sync.Mutex  // protects cache fields from concurrent goroutines
	searchCacheKey   string      // deterministic key from conditions+args
	searchCacheTable string      // temp table name (e.g. "_search_matches_42")
	searchCacheCount int64       // cached COUNT(*) from materialization
	searchCacheStats *TotalStats // cached stats from Phase 4
}

// DuckDBOptions configures optional DuckDB engine behavior.
type DuckDBOptions struct {
	// DisableSQLiteScanner prevents loading the sqlite_scanner extension even
	// on platforms where it would normally be available. This forces all SQLite
	// queries to route through sqliteEngine, matching the Windows code path.
	// Useful for testing the non-scanner code path on Linux/macOS.
	DisableSQLiteScanner bool
}

// NewDuckDBEngine creates a new DuckDB-backed query engine.
// analyticsDir should point to ~/.msgvault/analytics/
// sqlitePath should point to ~/.msgvault/msgvault.db
// sqliteDB is a direct SQLite connection for FTS search and body retrieval
//
// The engine uses a hybrid approach:
//   - DuckDB's sqlite_scan for list queries (ListMessages, ListAccounts, etc.)
//   - Direct SQLite (sqliteDB) for FTS search and message body retrieval
//
// If sqlitePath is empty, only aggregate queries and GetTotalStats will work.
// If sqliteDB is nil, Search will fall back to LIKE queries and body extraction
// from raw MIME may be slower.
func NewDuckDBEngine(analyticsDir string, sqlitePath string, sqliteDB *sql.DB, opts ...DuckDBOptions) (*DuckDBEngine, error) {
	var opt DuckDBOptions
	if len(opts) > 0 {
		opt = opts[0]
	}
	// Open in-memory DuckDB
	db, err := sql.Open("duckdb", "")
	if err != nil {
		return nil, fmt.Errorf("open duckdb: %w", err)
	}

	// Constrain to single connection to ensure session settings (SET threads, ATTACH)
	// are applied consistently. DuckDB session settings don't propagate across
	// pooled connections, so limiting to one connection avoids inconsistent behavior.
	db.SetMaxOpenConns(1)

	// Enable multithreading for better query performance.
	// Use GOMAXPROCS(0) instead of NumCPU() to respect container CPU limits.
	threads := runtime.GOMAXPROCS(0)
	if _, err := db.Exec(fmt.Sprintf("SET threads = %d", threads)); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("set threads: %w", err)
	}

	// Install and load SQLite extension if we have a SQLite path.
	// On Windows, the sqlite_scanner extension is not available for MinGW
	// builds — all detail queries route through sqliteEngine instead.
	// DisableSQLiteScanner forces the same fallback on any platform (for testing).
	// On other platforms, try to load but fall back gracefully (e.g. no internet).
	var hasSQLiteScanner bool
	if sqlitePath != "" && runtime.GOOS != "windows" && !opt.DisableSQLiteScanner {
		if _, err := db.Exec("INSTALL sqlite; LOAD sqlite;"); err != nil {
			log.Printf("[warn] sqlite_scanner extension unavailable, falling back to direct SQLite: %v", err)
		} else {
			// Attach SQLite database as read-only
			escapedPath := strings.ReplaceAll(sqlitePath, "'", "''")
			attachSQL := fmt.Sprintf("ATTACH '%s' AS sqlite_db (TYPE sqlite, READ_ONLY)", escapedPath)
			if _, err := db.Exec(attachSQL); err != nil {
				log.Printf("[warn] failed to attach SQLite via sqlite_scanner, falling back to direct SQLite: %v", err)
			} else {
				hasSQLiteScanner = true
			}
		}
	}

	// Create reusable SQLiteEngine if we have a direct connection
	// This preserves FTS cache across calls
	var sqliteEngine *SQLiteEngine
	if sqliteDB != nil {
		sqliteEngine = NewSQLiteEngine(sqliteDB)
	}

	engine := &DuckDBEngine{
		db:               db,
		analyticsDir:     analyticsDir,
		sqlitePath:       sqlitePath,
		sqliteDB:         sqliteDB,
		sqliteEngine:     sqliteEngine,
		hasSQLiteScanner: hasSQLiteScanner,
		querySem:         semaphore.NewWeighted(duckDBQueryConcurrency),
	}

	// Probe Parquet schemas for optional columns added in PR #160 (WhatsApp import).
	// Old cache files may lack these columns; we'll supply defaults in parquetCTEs().
	engine.optionalCols, engine.cacheFP = stableOptionalColumns(engine.cacheFingerprint, func() map[string]map[string]bool {
		return probeAllOptionalColumns(db, analyticsDir)
	})
	var missing []string
	for _, col := range []struct{ table, col string }{
		{datasetParticipants, "phone_number"},
		{datasetMessages, "attachment_count"},
		{datasetMessages, "sender_id"},
		{datasetMessages, "message_type"},
		{datasetConversations, "title"},
		{datasetConversations, "conversation_type"},
		{"sources", "source_type"},
	} {
		if !engine.optionalCols[col.table][col.col] {
			missing = append(missing, col.table+"."+col.col)
		}
	}
	if len(missing) > 0 {
		log.Printf("[warn] Parquet cache missing columns %v — run 'msgvault build-cache --full-rebuild' to update", missing)
	}
	// Register SQL views over Parquet files for raw SQL access.
	// Pass the already-probed optionalCols to avoid a redundant schema probe.
	if err := RegisterViewsWithColumns(db, analyticsDir, engine.optionalCols); err != nil {
		log.Printf("[warn] failed to register SQL views: %v", err)
		// Non-fatal: existing CTE-based queries still work.
	}

	return engine, nil
}

// Close releases DuckDB resources, including any cached search temp table.
func (e *DuckDBEngine) Close() error {
	e.searchCacheMu.Lock()
	e.dropSearchCache()
	e.searchCacheMu.Unlock()
	return e.db.Close()
}

// QuerySQL executes an arbitrary SQL query against the DuckDB engine
// and returns the results in a columnar format. Views registered by
// RegisterViews (base + convenience) are available.
func (e *DuckDBEngine) QuerySQL(
	ctx context.Context, sqlStr string,
) (*QueryResult, error) {
	if err := EnsureReadOnly(sqlStr); err != nil {
		return nil, err
	}

	release, err := e.acquireQuerySlot(ctx)
	if err != nil {
		return nil, err
	}
	defer release()

	// Refresh views if the cache changed since startup, so the registered
	// views match the current Parquet schema.
	e.ensureFreshOptionalCols()

	// codeql[go/sql-injection] -- QuerySQL is an explicit trusted-user SQL
	// interface over the user's local archive, not an injection boundary.
	rows, err := e.db.QueryContext(ctx, sqlStr)
	if err != nil {
		return nil, fmt.Errorf("execute query: %w", err)
	}
	defer func() { _ = rows.Close() }()

	cols, err := rows.Columns()
	if err != nil {
		return nil, fmt.Errorf("get columns: %w", err)
	}

	result := &QueryResult{Columns: cols}
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, fmt.Errorf("scan row: %w", err)
		}
		for i, v := range vals {
			if b, ok := v.([]byte); ok {
				vals[i] = string(b)
			}
		}
		result.Rows = append(result.Rows, vals)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate rows: %w", err)
	}
	result.RowCount = len(result.Rows)
	return result, nil
}

// acquireQuerySlot blocks until a DuckDB query slot is free or ctx is done,
// bounding concurrent heavy queries (see duckDBQueryConcurrency). The returned
// release function frees the slot and must be deferred by the caller. Callers
// must not nest acquisitions — every gated method is a top-level entry point
// that does not call another gated method, so the two slots cannot deadlock.
func (e *DuckDBEngine) acquireQuerySlot(ctx context.Context) (func(), error) {
	if e.querySem == nil {
		return func() {}, nil
	}
	if err := e.querySem.Acquire(ctx, 1); err != nil {
		return nil, fmt.Errorf("acquire query slot: %w", err)
	}
	return func() { e.querySem.Release(1) }, nil
}

// hasSQLite returns true if DuckDB's sqlite_scanner extension is loaded,
// allowing sqlite_db.* queries. On Windows this is always false.
func (e *DuckDBEngine) hasSQLite() bool {
	return e.hasSQLiteScanner
}

// parquetGlob returns the glob pattern for reading message Parquet files.
func (e *DuckDBEngine) parquetGlob() string {
	return filepath.Join(e.analyticsDir, datasetMessages, "**", "*.parquet")
}

// parquetPath returns the path pattern for a specific Parquet table.
func (e *DuckDBEngine) parquetPath(table string) string {
	return filepath.Join(e.analyticsDir, table, "*.parquet")
}

// hasCol returns true if the named column exists in the Parquet schema for the given table.
func (e *DuckDBEngine) hasCol(table, col string) bool {
	e.optColsMu.RLock()
	defer e.optColsMu.RUnlock()
	if e.optionalCols == nil {
		return true // no probe data — assume present (backwards compatible)
	}
	tbl, ok := e.optionalCols[table]
	if !ok {
		return true // table not probed — assume present
	}
	return tbl[col]
}

// cacheFingerprint computes a cheap signature of the analytics Parquet cache.
// It combines, per required table, the file count and each file's size and
// modification time. When build-cache or sync rewrites the cache underneath a
// long-running process the fingerprint changes, letting the engine detect that
// its probed column set and materialized search cache are stale. Pure
// filesystem stats — no DuckDB access.
func (e *DuckDBEngine) cacheFingerprint() string {
	var b strings.Builder
	for _, g := range e.cacheFingerprintGlobs() {
		matches, _ := filepath.Glob(g)
		fmt.Fprintf(&b, "%s#%d|", g, len(matches))
		for _, m := range matches {
			if fi, err := os.Stat(m); err == nil {
				fmt.Fprintf(&b, "%s=%d,%d;", m, fi.Size(), fi.ModTime().UnixNano())
			}
		}
	}
	return b.String()
}

func (e *DuckDBEngine) cacheFingerprintGlobs() []string {
	globs := make([]string, 0, len(RequiredParquetDirs))
	for _, dir := range RequiredParquetDirs {
		if dir == datasetMessages {
			globs = append(globs, filepath.Join(e.analyticsDir, dir, "*", "*.parquet"))
			continue
		}
		globs = append(globs, e.parquetPath(dir))
	}
	return globs
}

func stableOptionalColumns(
	cacheFingerprint func() string,
	probe func() map[string]map[string]bool,
) (map[string]map[string]bool, string) {
	for {
		before := cacheFingerprint()
		cols := probe()
		after := cacheFingerprint()
		if before == after {
			return cols, after
		}
		log.Printf("[info] analytics cache changed during Parquet schema probe — retrying")
	}
}

// ensureFreshOptionalCols re-probes the Parquet schema (and re-registers the
// SQL views) when the analytics cache has changed since the last probe. This
// guards long-running engines (e.g. the mcp-http server) against a binder
// error when build-cache rewrites the cache with a different column set:
// without it, a stale "column present" verdict puts a now-absent column into a
// SELECT * REPLACE list, which DuckDB rejects with
// "Column ... in REPLACE list not found in FROM clause". Cheap on the common
// no-change path (a handful of os.Stat calls).
func (e *DuckDBEngine) ensureFreshOptionalCols() {
	fp := e.cacheFingerprint()

	e.optColsMu.RLock()
	unchanged := fp == e.cacheFP
	e.optColsMu.RUnlock()
	if unchanged {
		return
	}

	e.optColsMu.Lock()
	defer e.optColsMu.Unlock()
	if fp == e.cacheFP { // another goroutine refreshed while we waited
		return
	}

	newCols, fp := stableOptionalColumns(e.cacheFingerprint, func() map[string]map[string]bool {
		return probeAllOptionalColumns(e.db, e.analyticsDir)
	})
	e.optionalCols = newCols
	e.cacheFP = fp
	if err := RegisterViewsWithColumns(e.db, e.analyticsDir, newCols); err != nil {
		log.Printf("[warn] re-register views after analytics cache change: %v", err)
	}
	log.Printf("[info] analytics cache changed — re-probed Parquet optional columns")
}

func (e *DuckDBEngine) currentCacheFingerprint() string {
	e.optColsMu.RLock()
	defer e.optColsMu.RUnlock()
	return e.cacheFP
}

// parquetCTEs returns common CTEs for reading all Parquet tables.
// This is used by aggregate queries that need to join across tables.
// parquetCTEs returns the WITH clause body that defines CTEs for all Parquet
// tables. Columns are explicitly cast to their expected types using DuckDB's
// REPLACE syntax, because Parquet schema inference from SQLite can store
// integer/boolean columns as VARCHAR, causing type mismatch errors in JOINs
// and COALESCE expressions.
//
// Optional columns (phone_number, attachment_count, sender_id, message_type)
// are handled gracefully: if the Parquet file predates their addition, they
// are synthesised with sensible defaults instead of causing a binder error.
func (e *DuckDBEngine) parquetCTEs() string {
	// Re-probe if build-cache/sync rewrote the cache underneath us, so the
	// REPLACE list below never references a column the current Parquet lacks.
	e.ensureFreshOptionalCols()

	// --- messages CTE ---
	msgReplace := []string{
		"CAST(id AS BIGINT) AS id",
		"CAST(source_id AS BIGINT) AS source_id",
		"CAST(source_message_id AS VARCHAR) AS source_message_id",
		"CAST(conversation_id AS BIGINT) AS conversation_id",
		"CAST(subject AS VARCHAR) AS subject",
		"CAST(snippet AS VARCHAR) AS snippet",
		"CAST(size_estimate AS BIGINT) AS size_estimate",
		"COALESCE(TRY_CAST(has_attachments AS BOOLEAN), false) AS has_attachments",
	}
	var msgExtra []string
	if e.hasCol(datasetMessages, "attachment_count") {
		msgReplace = append(msgReplace, "COALESCE(TRY_CAST(attachment_count AS INTEGER), 0) AS attachment_count")
	} else {
		msgExtra = append(msgExtra, "0 AS attachment_count")
	}
	if e.hasCol(datasetMessages, "sender_id") {
		msgReplace = append(msgReplace, "TRY_CAST(sender_id AS BIGINT) AS sender_id")
	} else {
		msgExtra = append(msgExtra, "NULL::BIGINT AS sender_id")
	}
	if e.hasCol(datasetMessages, "message_type") {
		msgReplace = append(msgReplace, "COALESCE(CAST(message_type AS VARCHAR), '') AS message_type")
	} else {
		msgExtra = append(msgExtra, "'' AS message_type")
	}
	if e.hasCol(datasetMessages, "deleted_at") {
		msgReplace = append(msgReplace, "TRY_CAST(deleted_at AS TIMESTAMP) AS deleted_at")
	} else {
		msgExtra = append(msgExtra, "NULL::TIMESTAMP AS deleted_at")
	}
	msgCTE := fmt.Sprintf("SELECT * REPLACE (\n\t\t\t\t%s\n\t\t\t)", strings.Join(msgReplace, ",\n\t\t\t\t"))
	if len(msgExtra) > 0 {
		msgCTE += ", " + strings.Join(msgExtra, ", ")
	}
	msgCTE += fmt.Sprintf(" FROM read_parquet('%s', hive_partitioning=true, union_by_name=true)", e.parquetGlob())

	// --- participants CTE ---
	pReplace := []string{
		"CAST(id AS BIGINT) AS id",
		"CAST(email_address AS VARCHAR) AS email_address",
		"CAST(domain AS VARCHAR) AS domain",
		"CAST(display_name AS VARCHAR) AS display_name",
	}
	var pExtra []string
	if e.hasCol(datasetParticipants, "phone_number") {
		pReplace = append(pReplace, "COALESCE(CAST(phone_number AS VARCHAR), '') AS phone_number")
	} else {
		pExtra = append(pExtra, "'' AS phone_number")
	}
	pCTE := fmt.Sprintf("SELECT * REPLACE (\n\t\t\t\t%s\n\t\t\t)", strings.Join(pReplace, ",\n\t\t\t\t"))
	if len(pExtra) > 0 {
		pCTE += ", " + strings.Join(pExtra, ", ")
	}
	pCTE += fmt.Sprintf(" FROM read_parquet('%s')", e.parquetPath(datasetParticipants))

	// --- conversations CTE ---
	convReplace := []string{
		"CAST(id AS BIGINT) AS id",
		"CAST(source_conversation_id AS VARCHAR) AS source_conversation_id",
	}
	var convExtra []string
	if e.hasCol(datasetConversations, "title") {
		convReplace = append(convReplace, "COALESCE(CAST(title AS VARCHAR), '') AS title")
	} else {
		convExtra = append(convExtra, "'' AS title")
	}
	if e.hasCol(datasetConversations, "conversation_type") {
		convReplace = append(convReplace, "COALESCE(CAST(conversation_type AS VARCHAR), 'email') AS conversation_type")
	} else {
		convExtra = append(convExtra, "'email' AS conversation_type")
	}
	convCTE := fmt.Sprintf("SELECT * REPLACE (\n\t\t\t\t%s\n\t\t\t)", strings.Join(convReplace, ",\n\t\t\t\t"))
	if len(convExtra) > 0 {
		convCTE += ", " + strings.Join(convExtra, ", ")
	}
	convCTE += fmt.Sprintf(" FROM read_parquet('%s')", e.parquetPath(datasetConversations))

	// --- sources CTE ---
	srcReplace := []string{
		"CAST(id AS BIGINT) AS id",
	}
	var srcExtra []string
	if e.hasCol("sources", "source_type") {
		srcReplace = append(srcReplace, "COALESCE(CAST(source_type AS VARCHAR), 'gmail') AS source_type")
	} else {
		srcExtra = append(srcExtra, "'gmail' AS source_type")
	}
	srcCTE := fmt.Sprintf("SELECT * REPLACE (\n\t\t\t\t%s\n\t\t\t)", strings.Join(srcReplace, ",\n\t\t\t\t"))
	if len(srcExtra) > 0 {
		srcCTE += ", " + strings.Join(srcExtra, ", ")
	}
	srcCTE += fmt.Sprintf(" FROM read_parquet('%s')", e.parquetPath("sources"))

	return fmt.Sprintf(`
		msg AS (
			%s
		),
		mr AS (
			SELECT * REPLACE (
				CAST(message_id AS BIGINT) AS message_id,
				CAST(participant_id AS BIGINT) AS participant_id,
				CAST(recipient_type AS VARCHAR) AS recipient_type,
				CAST(display_name AS VARCHAR) AS display_name
			) FROM read_parquet('%s')
		),
		p AS (
			%s
		),
		lbl AS (
			SELECT * REPLACE (
				CAST(id AS BIGINT) AS id,
				CAST(name AS VARCHAR) AS name
			) FROM read_parquet('%s')
		),
		ml AS (
			SELECT * REPLACE (
				CAST(message_id AS BIGINT) AS message_id,
				CAST(label_id AS BIGINT) AS label_id
			) FROM read_parquet('%s')
		),
		att AS (
			SELECT CAST(message_id AS BIGINT) AS message_id,
				SUM(COALESCE(TRY_CAST(size AS BIGINT), 0)) as attachment_size,
				COUNT(*) as attachment_count
			FROM read_parquet('%s')
			GROUP BY 1
		),
		src AS (
			%s
		),
		conv AS (
			%s
		)
		`, msgCTE,
		e.parquetPath("message_recipients"),
		pCTE,
		e.parquetPath("labels"),
		e.parquetPath("message_labels"),
		e.parquetPath("attachments"),
		srcCTE,
		convCTE)
}

func duckDBMessageTypeCondition(alias string, messageTypes []string) (string, []any) {
	var conditions []string
	var args []any
	var exact []string
	includeEmail := false

	for _, typ := range messageTypes {
		typ = strings.TrimSpace(strings.ToLower(typ))
		if typ == "" {
			continue
		}
		if typ == messageTypeEmail {
			includeEmail = true
			continue
		}
		exact = append(exact, typ)
	}

	col := alias + ".message_type"
	if includeEmail {
		conditions = append(conditions,
			fmt.Sprintf("(%s = ? OR %s IS NULL OR %s = '')", col, col, col))
		args = append(args, messageTypeEmail)
	}
	if len(exact) > 0 {
		placeholders := make([]string, len(exact))
		for i, typ := range exact {
			placeholders[i] = "?"
			args = append(args, typ)
		}
		conditions = append(conditions,
			fmt.Sprintf("%s IN (%s)", col, strings.Join(placeholders, ",")))
	}
	if len(conditions) == 0 {
		return "", nil
	}
	return "(" + strings.Join(conditions, " OR ") + ")", args
}

// escapeILIKE escapes ILIKE wildcard characters (% and _) in user input.
func escapeILIKE(s string) string {
	s = strings.ReplaceAll(s, "\\", "\\\\") // Escape backslash first
	s = strings.ReplaceAll(s, "%", "\\%")
	s = strings.ReplaceAll(s, "_", "\\_")
	return s
}

// buildWhereClause builds WHERE conditions for Parquet queries.
// Column references use msg. prefix to be explicit since aggregate queries join multiple CTEs.
// buildAggregateSearchConditions builds SQL conditions for a search query in aggregate views.
// Returns conditions and args that can be appended to existing conditions.
// buildAggregateSearchConditions builds WHERE conditions for aggregate search.
// keyColumns are SQL expressions for the grouping dimension that text terms
// should filter on (e.g. "p.email_address", "p.display_name"). When nil,
// text terms search subject + sender (the default for Senders/Time views).
func (e *DuckDBEngine) buildAggregateSearchConditions(searchQuery string, keyColumns ...string) ([]string, []any) {
	if searchQuery == "" {
		return nil, nil
	}

	var conditions []string
	var args []any

	q := search.Parse(searchQuery)

	// Text terms: always search subject + sender, plus the view's grouping
	// key columns when provided (e.g., label name in Labels view).
	// Uses ILIKE for performance on Parquet scans.
	for _, term := range q.TextTerms {
		termPattern := "%" + escapeILIKE(term) + "%"
		var parts []string
		parts = append(parts, `msg.subject ILIKE ? ESCAPE '\'`)
		args = append(args, termPattern)
		parts = append(parts, `COALESCE(msg.snippet, '') ILIKE ? ESCAPE '\'`)
		args = append(args, termPattern)
		parts = append(parts, `EXISTS (
			SELECT 1 FROM mr mr_search
			JOIN p p_search ON p_search.id = mr_search.participant_id
			WHERE mr_search.message_id = msg.id
			  AND mr_search.recipient_type = 'from'
			  AND (p_search.email_address ILIKE ? ESCAPE '\' OR COALESCE(p_search.display_name, '') ILIKE ? ESCAPE '\')
		)`)
		args = append(args, termPattern, termPattern)
		for _, col := range keyColumns {
			parts = append(parts, col+` ILIKE ? ESCAPE '\'`)
			args = append(args, termPattern)
		}
		conditions = append(conditions, "("+strings.Join(parts, " OR ")+")")
	}

	// Append non-text filters (from:, to:, subject:, label:, has:, dates, sizes).
	nonTextConds, nonTextArgs := e.buildNonTextSearchConditions(q, keyColumns...)
	conditions = append(conditions, nonTextConds...)
	args = append(args, nonTextArgs...)

	return conditions, args
}

// buildNonTextSearchConditions builds WHERE conditions for the non-text
// portion of a parsed search query (from:, to:, subject:, label:, has:,
// date/size filters). Extracted from buildAggregateSearchConditions so
// callers that handle text terms themselves (e.g. buildStatsSearchConditions)
// can append non-text filters without having to compute how many args
// the text-term portion produced.
func (e *DuckDBEngine) buildNonTextSearchConditions(q *search.Query, keyColumns ...string) ([]string, []any) {
	var conditions []string
	var args []any

	if len(q.MessageTypes) > 0 {
		condition, conditionArgs := duckDBMessageTypeCondition("msg", q.MessageTypes)
		if condition != "" {
			conditions = append(conditions, condition)
			args = append(args, conditionArgs...)
		}
	}

	// from: filter - match sender email
	for _, from := range q.FromAddrs {
		fromPattern := "%" + escapeILIKE(from) + "%"
		conditions = append(conditions, `EXISTS (
			SELECT 1 FROM mr mr_from
			JOIN p p_from ON p_from.id = mr_from.participant_id
			WHERE mr_from.message_id = msg.id
			  AND mr_from.recipient_type = 'from'
			  AND p_from.email_address ILIKE ? ESCAPE '\'
		)`)
		args = append(args, fromPattern)
	}

	// to: filter - match recipient email (to or cc, consistent with SearchFast)
	for _, to := range q.ToAddrs {
		toPattern := "%" + escapeILIKE(to) + "%"
		conditions = append(conditions, `EXISTS (
			SELECT 1 FROM mr mr_to
			JOIN p p_to ON p_to.id = mr_to.participant_id
			WHERE mr_to.message_id = msg.id
			  AND mr_to.recipient_type IN ('to', 'cc', 'bcc')
			  AND p_to.email_address ILIKE ? ESCAPE '\'
		)`)
		args = append(args, toPattern)
	}

	// subject: filter
	for _, subj := range q.SubjectTerms {
		subjPattern := "%" + escapeILIKE(subj) + "%"
		conditions = append(conditions, "msg.subject ILIKE ? ESCAPE '\\'")
		args = append(args, subjPattern)
	}

	// label: filter - case-insensitive substring match.
	// In the Labels aggregate view (keyColumns includes the label column),
	// filter the grouping column directly so only matching labels appear
	// in results — not all labels from matching messages.
	labelKeyCol := ""
	for _, col := range keyColumns {
		if strings.HasSuffix(col, ".name") &&
			strings.HasPrefix(col, "lbl") {
			labelKeyCol = col
			break
		}
	}
	if labelKeyCol != "" && len(q.Labels) > 0 {
		// Labels view: filter the grouped label column directly.
		// Use OR so label:arrow label:inbox shows both matching labels.
		var labelParts []string
		for _, label := range q.Labels {
			labelParts = append(labelParts, labelKeyCol+` ILIKE ? ESCAPE '\'`)
			args = append(args, "%"+escapeILIKE(label)+"%")
		}
		conditions = append(conditions, "("+strings.Join(labelParts, " OR ")+")")
	} else {
		// Non-label views: use EXISTS to filter messages by label.
		for _, label := range q.Labels {
			conditions = append(conditions, `EXISTS (
				SELECT 1 FROM ml ml_label
				JOIN lbl l_label ON l_label.id = ml_label.label_id
				WHERE ml_label.message_id = msg.id
				  AND l_label.name ILIKE ? ESCAPE '\'
			)`)
			args = append(args, "%"+escapeILIKE(label)+"%")
		}
	}

	// has:attachment filter
	if q.HasAttachment != nil && *q.HasAttachment {
		conditions = append(conditions, "msg.has_attachments = 1")
	}

	// Date filters from search query
	if q.AfterDate != nil {
		conditions = append(conditions, "msg.sent_at >= CAST(? AS TIMESTAMP)")
		args = append(args, q.AfterDate.Format("2006-01-02 15:04:05"))
	}
	if q.BeforeDate != nil {
		conditions = append(conditions, "msg.sent_at < CAST(? AS TIMESTAMP)")
		args = append(args, q.BeforeDate.Format("2006-01-02 15:04:05"))
	}

	// Size filters
	if q.LargerThan != nil {
		conditions = append(conditions, "msg.size_estimate > ?")
		args = append(args, *q.LargerThan)
	}
	if q.SmallerThan != nil {
		conditions = append(conditions, "msg.size_estimate < ?")
		args = append(args, *q.SmallerThan)
	}
	if len(q.MessageTypes) > 0 {
		condition, conditionArgs := duckDBMessageTypeCondition("msg", q.MessageTypes)
		if condition != "" {
			conditions = append(conditions, condition)
			args = append(args, conditionArgs...)
		}
	}

	return conditions, args
}

// buildWhereClause builds WHERE conditions for aggregate queries.
// buildStatsSearchConditions builds search conditions for GetTotalStats.
// For 1:N views (Recipients, RecipientNames, Labels), text terms filter via
// EXISTS subqueries on the grouping dimension so stats match visible rows.
// For 1:1 views, falls back to the default subject+sender search.
func (e *DuckDBEngine) buildStatsSearchConditions(searchQuery string, groupBy ViewType) ([]string, []any) {
	if searchQuery == "" {
		return nil, nil
	}

	q := search.Parse(searchQuery)

	var conditions []string
	var args []any

	// Text terms — use EXISTS for 1:N views since the stats query has no
	// participant/label joins.
	for _, term := range q.TextTerms {
		termPattern := "%" + escapeILIKE(term) + "%"
		switch groupBy {
		case ViewRecipients, ViewRecipientNames:
			conditions = append(conditions, `EXISTS (
				SELECT 1 FROM mr mr_rs
				JOIN p p_rs ON p_rs.id = mr_rs.participant_id
				WHERE mr_rs.message_id = msg.id
				  AND mr_rs.recipient_type IN ('to', 'cc', 'bcc')
				  AND (p_rs.email_address ILIKE ? ESCAPE '\' OR p_rs.display_name ILIKE ? ESCAPE '\')
			)`)
			args = append(args, termPattern, termPattern)
		case ViewLabels:
			conditions = append(conditions, `EXISTS (
				SELECT 1 FROM ml ml_rs
				JOIN lbl lbl_rs ON lbl_rs.id = ml_rs.label_id
				WHERE ml_rs.message_id = msg.id
				  AND lbl_rs.name ILIKE ? ESCAPE '\'
			)`)
			args = append(args, termPattern)
		default:
			// Default: search subject, snippet, and sender
			conditions = append(conditions, `(
				msg.subject ILIKE ? ESCAPE '\' OR
				COALESCE(msg.snippet, '') ILIKE ? ESCAPE '\' OR
				EXISTS (
					SELECT 1 FROM mr mr_search
					JOIN p p_search ON p_search.id = mr_search.participant_id
					WHERE mr_search.message_id = msg.id
					  AND mr_search.recipient_type = 'from'
					  AND (p_search.email_address ILIKE ? ESCAPE '\' OR p_search.display_name ILIKE ? ESCAPE '\')
				)
			)`)
			args = append(args, termPattern, termPattern, termPattern, termPattern)
		}
	}

	// Non-text filters (from:, to:, subject:, label:, etc.) are the same
	// regardless of view — delegate to the non-text helper directly so we
	// don't have to track how many args the text-term portion emits.
	nonTextConds, nonTextArgs := e.buildNonTextSearchConditions(q)
	conditions = append(conditions, nonTextConds...)
	args = append(args, nonTextArgs...)

	return conditions, args
}

// keyColumns are passed through to buildAggregateSearchConditions to control
// which columns text search terms filter on.
func (e *DuckDBEngine) buildWhereClause(opts AggregateOptions, keyColumns ...string) (string, []any) {
	var conditions []string
	var args []any

	conditions = append(conditions, store.LiveMessagesWhere("msg", opts.HideDeletedFromSource))
	conditions, args = appendSourceFilter(conditions, args, "msg.", opts.SourceID, opts.SourceIDs)

	if opts.After != nil {
		conditions = append(conditions, "msg.sent_at >= CAST(? AS TIMESTAMP)")
		args = append(args, opts.After.Format("2006-01-02 15:04:05"))
	}

	if opts.Before != nil {
		conditions = append(conditions, "msg.sent_at < CAST(? AS TIMESTAMP)")
		args = append(args, opts.Before.Format("2006-01-02 15:04:05"))
	}

	if opts.WithAttachmentsOnly {
		conditions = append(conditions, "msg.has_attachments = 1")
	}

	// Text search filter for aggregates - filter on view's key columns
	searchConds, searchArgs := e.buildAggregateSearchConditions(opts.SearchQuery, keyColumns...)
	conditions = append(conditions, searchConds...)
	args = append(args, searchArgs...)

	if len(conditions) == 0 {
		return "1=1", args
	}
	return strings.Join(conditions, " AND "), args
}

// timeExpr returns the SQL expression for time grouping based on granularity.
func timeExpr(g TimeGranularity) string {
	switch g {
	case TimeYear:
		return "CAST(msg.year AS VARCHAR)"
	case TimeDay:
		return "strftime(msg.sent_at, '%Y-%m-%d')"
	default: // TimeMonth
		return "CAST(msg.year AS VARCHAR) || '-' || LPAD(CAST(msg.month AS VARCHAR), 2, '0')"
	}
}

// aggViewDef defines the varying parts of an aggregate query for each view type.
type aggViewDef struct {
	keyExpr    string // SQL expression for the grouping key (e.g. "p.email_address")
	joinClause string // JOIN clause specific to this view
	nullGuard  string // WHERE condition to exclude NULL keys
	// keyColumns for buildWhereClause search filtering (passed through to buildAggregateSearchConditions)
	keyColumns []string
}

// getViewDef returns the aggregate query definition for a given view type.
// The tablePrefix is used to alias tables in SubAggregate to avoid conflicts
// with CTE names used in filter conditions. Pass "" for top-level aggregates.
func getViewDef(view ViewType, granularity TimeGranularity, tablePrefix string) (aggViewDef, error) {
	// Use prefix for table aliases in SubAggregate (e.g. "mr_agg", "p_agg")
	// to avoid ambiguity with CTE names used in WHERE clause EXISTS subqueries.
	mrAlias := "mr"
	pAlias := "p"
	mlAlias := "ml"
	lblAlias := "lbl"
	if tablePrefix != "" {
		mrAlias = "mr_" + tablePrefix
		pAlias = "p_" + tablePrefix
		mlAlias = "ml_" + tablePrefix
		lblAlias = "lbl_" + tablePrefix
	}

	switch view {
	case ViewSenders:
		return aggViewDef{
			keyExpr:    pAlias + ".email_address",
			joinClause: fmt.Sprintf("JOIN mr %s ON %s.message_id = msg.id AND %s.recipient_type = 'from'\n\t\t\t\tJOIN p %s ON %s.id = %s.participant_id", mrAlias, mrAlias, mrAlias, pAlias, pAlias, mrAlias),
			nullGuard:  pAlias + ".email_address IS NOT NULL",
		}, nil
	case ViewSenderNames:
		nameExpr := recipientNameExpr(mrAlias, pAlias)
		return aggViewDef{
			keyExpr:    nameExpr,
			joinClause: fmt.Sprintf("JOIN mr %s ON %s.message_id = msg.id AND %s.recipient_type = 'from'\n\t\t\t\tJOIN p %s ON %s.id = %s.participant_id", mrAlias, mrAlias, mrAlias, pAlias, pAlias, mrAlias),
			nullGuard:  nameExpr + " != ''",
			keyColumns: []string{mrAlias + ".display_name", pAlias + ".email_address", pAlias + ".display_name", pAlias + ".phone_number"},
		}, nil
	case ViewRecipients:
		return aggViewDef{
			keyExpr:    pAlias + ".email_address",
			joinClause: fmt.Sprintf("JOIN mr %s ON %s.message_id = msg.id AND %s.recipient_type IN ('to', 'cc', 'bcc')\n\t\t\t\tJOIN p %s ON %s.id = %s.participant_id", mrAlias, mrAlias, mrAlias, pAlias, pAlias, mrAlias),
			nullGuard:  pAlias + ".email_address IS NOT NULL",
			keyColumns: []string{pAlias + ".email_address", pAlias + ".display_name"},
		}, nil
	case ViewRecipientNames:
		nameExpr := recipientNameExpr(mrAlias, pAlias)
		return aggViewDef{
			keyExpr:    nameExpr,
			joinClause: fmt.Sprintf("JOIN mr %s ON %s.message_id = msg.id AND %s.recipient_type IN ('to', 'cc', 'bcc')\n\t\t\t\tJOIN p %s ON %s.id = %s.participant_id", mrAlias, mrAlias, mrAlias, pAlias, pAlias, mrAlias),
			nullGuard:  nameExpr + " != ''",
			keyColumns: []string{mrAlias + ".display_name", pAlias + ".email_address", pAlias + ".display_name", pAlias + ".phone_number"},
		}, nil
	case ViewDomains:
		return aggViewDef{
			keyExpr:    pAlias + ".domain",
			joinClause: fmt.Sprintf("JOIN mr %s ON %s.message_id = msg.id AND %s.recipient_type = 'from'\n\t\t\t\tJOIN p %s ON %s.id = %s.participant_id", mrAlias, mrAlias, mrAlias, pAlias, pAlias, mrAlias),
			nullGuard:  pAlias + ".domain IS NOT NULL AND " + pAlias + ".domain != ''",
		}, nil
	case ViewLabels:
		return aggViewDef{
			keyExpr:    lblAlias + ".name",
			joinClause: fmt.Sprintf("JOIN ml %s ON %s.message_id = msg.id\n\t\t\t\tJOIN lbl %s ON %s.id = %s.label_id", mlAlias, mlAlias, lblAlias, lblAlias, mlAlias),
			nullGuard:  lblAlias + ".name IS NOT NULL",
			keyColumns: []string{lblAlias + ".name"},
		}, nil
	case ViewTime:
		return aggViewDef{
			keyExpr:   timeExpr(granularity),
			nullGuard: "msg.sent_at IS NOT NULL",
		}, nil
	default:
		return aggViewDef{}, fmt.Errorf("unsupported view type: %v", view)
	}
}

// runAggregation executes a generic aggregation query using the view definition.
func (e *DuckDBEngine) runAggregation(ctx context.Context, def aggViewDef, whereClause string, args []any, opts AggregateOptions) ([]AggregateRow, error) {
	limit := opts.Limit
	if limit == 0 {
		limit = 100
	}

	fullWhere := whereClause
	if def.nullGuard != "" {
		fullWhere += " AND " + def.nullGuard
	}

	query := fmt.Sprintf(`
		WITH %s
		SELECT key, count, total_size, attachment_size, attachment_count, total_unique
		FROM (
			SELECT
				%s as key,
				COUNT(*) as count,
				COALESCE(SUM(CAST(msg.size_estimate AS BIGINT)), 0) as total_size,
				CAST(COALESCE(SUM(att.attachment_size), 0) AS BIGINT) as attachment_size,
				CAST(COALESCE(SUM(att.attachment_count), 0) AS BIGINT) as attachment_count,
				COUNT(*) OVER() as total_unique
			FROM msg
			%s
			LEFT JOIN att ON att.message_id = msg.id
			WHERE %s
			GROUP BY %s
		)
		%s
		LIMIT ?
	`, e.parquetCTEs(), def.keyExpr, def.joinClause, fullWhere, def.keyExpr, e.sortClause(opts))

	args = append(args, limit)
	return e.executeAggregateQuery(ctx, query, args)
}

// sortClause returns ORDER BY clause for aggregates.
func (e *DuckDBEngine) sortClause(opts AggregateOptions) string {
	field := "count"
	switch opts.SortField {
	case SortBySize:
		field = "total_size"
	case SortByAttachmentSize:
		field = "attachment_size"
	case SortByName:
		field = "key"
	default:
		// SortByCount (and any unset field) keeps the "count" default.
	}

	dir := "DESC"
	if opts.SortDirection == SortAsc {
		dir = "ASC"
	}

	return fmt.Sprintf("ORDER BY %s %s", field, dir)
}

// aggregateByView is the generic implementation for all AggregateBy* methods.
func (e *DuckDBEngine) aggregateByView(ctx context.Context, view ViewType, opts AggregateOptions) ([]AggregateRow, error) {
	def, err := getViewDef(view, opts.TimeGranularity, "")
	if err != nil {
		return nil, err
	}
	where, args := e.buildWhereClause(opts, def.keyColumns...)
	return e.runAggregation(ctx, def, where, args, opts)
}

// Aggregate performs grouping based on the provided ViewType.
func (e *DuckDBEngine) Aggregate(ctx context.Context, groupBy ViewType, opts AggregateOptions) ([]AggregateRow, error) {
	release, err := e.acquireQuerySlot(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	return e.aggregateByView(ctx, groupBy, opts)
}

// buildFilterConditions builds WHERE conditions from a MessageFilter.
// Uses EXISTS subqueries for join-based filters (sender, recipient, label),
// which become semi-joins and avoid duplicates without needing DISTINCT.
func (e *DuckDBEngine) buildFilterConditions(filter MessageFilter) (string, []any) {
	var conditions []string
	var args []any

	conditions = append(conditions, store.LiveMessagesWhere("msg", filter.HideDeletedFromSource))
	conditions, args = appendSourceFilter(conditions, args, "msg.", filter.SourceID, filter.SourceIDs)

	if filter.ConversationID != nil {
		conditions = append(conditions, "msg.conversation_id = ?")
		args = append(args, *filter.ConversationID)
	}

	if filter.After != nil {
		conditions = append(conditions, "msg.sent_at >= CAST(? AS TIMESTAMP)")
		args = append(args, filter.After.Format("2006-01-02 15:04:05"))
	}

	if filter.Before != nil {
		conditions = append(conditions, "msg.sent_at < CAST(? AS TIMESTAMP)")
		args = append(args, filter.Before.Format("2006-01-02 15:04:05"))
	}

	if filter.WithAttachmentsOnly {
		conditions = append(conditions, "msg.has_attachments = true")
	}

	if filter.MessageType != "" {
		condition, conditionArgs := duckDBMessageTypeCondition("msg", []string{filter.MessageType})
		if condition != "" {
			conditions = append(conditions, condition)
			args = append(args, conditionArgs...)
		}
	}

	// Sender + sender-name filters - check both message_recipients (email)
	// and direct sender_id (WhatsApp/chat). Also checks phone_number for
	// phone-based lookups (e.g., from:+447...).
	//
	// When BOTH the email and the display name are filtered, they must
	// match the SAME from-row (or the SAME direct sender), not two
	// independent EXISTS that a multi-author message could satisfy via
	// different rows.
	if filter.Sender != "" && filter.SenderName != "" {
		conditions = append(conditions, fmt.Sprintf(`(EXISTS (
			SELECT 1 FROM mr
			JOIN p ON p.id = mr.participant_id
			WHERE mr.message_id = msg.id
			  AND mr.recipient_type = 'from'
			  AND (p.email_address = ? OR p.phone_number = ?)
			  AND %s = ?
		) OR EXISTS (
			SELECT 1 FROM p
			WHERE p.id = msg.sender_id
			  AND (p.email_address = ? OR p.phone_number = ?)
			  AND %s = ?
		))`, recipientNameExpr("mr", "p"), participantNameExpr("p")))
		args = append(args, filter.Sender, filter.Sender, filter.SenderName, filter.Sender, filter.Sender, filter.SenderName)
	} else if filter.Sender != "" {
		conditions = append(conditions, `(EXISTS (
			SELECT 1 FROM mr
			JOIN p ON p.id = mr.participant_id
			WHERE mr.message_id = msg.id
			  AND mr.recipient_type = 'from'
			  AND (p.email_address = ? OR p.phone_number = ?)
		) OR EXISTS (
			SELECT 1 FROM p
			WHERE p.id = msg.sender_id
			  AND (p.email_address = ? OR p.phone_number = ?)
		))`)
		args = append(args, filter.Sender, filter.Sender, filter.Sender, filter.Sender)
	} else if filter.MatchesEmpty(ViewSenders) {
		// A message has an "empty sender" only if it has no from-recipient AND no direct sender_id.
		conditions = append(conditions, `(NOT EXISTS (
			SELECT 1 FROM mr
			JOIN p ON p.id = mr.participant_id
			WHERE mr.message_id = msg.id
			  AND mr.recipient_type = 'from'
			  AND (
			    (p.email_address IS NOT NULL AND p.email_address != '') OR
			    (p.phone_number IS NOT NULL AND p.phone_number != '')
			  )
		) AND msg.sender_id IS NULL)`)
	}

	// Sender name filter - check both message_recipients (email) and direct sender_id (WhatsApp/chat)
	if filter.SenderName != "" && filter.Sender == "" {
		conditions = append(conditions, fmt.Sprintf(`(EXISTS (
			SELECT 1 FROM mr
			JOIN p ON p.id = mr.participant_id
			WHERE mr.message_id = msg.id
			  AND mr.recipient_type = 'from'
			  AND %s = ?
		) OR EXISTS (
			SELECT 1 FROM p
			WHERE p.id = msg.sender_id
			  AND %s = ?
		))`, recipientNameExpr("mr", "p"), participantNameExpr("p")))
		args = append(args, filter.SenderName, filter.SenderName)
	} else if filter.SenderName == "" && filter.MatchesEmpty(ViewSenderNames) {
		// A message has an "empty sender name" only if it has no from-recipient name AND no direct sender_id with a name.
		conditions = append(conditions, fmt.Sprintf(`(NOT EXISTS (
			SELECT 1 FROM mr
			JOIN p ON p.id = mr.participant_id
			WHERE mr.message_id = msg.id
			  AND mr.recipient_type = 'from'
			  AND %s != ''
		) AND NOT EXISTS (
			SELECT 1 FROM p
			WHERE p.id = msg.sender_id
			  AND %s IS NOT NULL
		))`, recipientNameExpr("mr", "p"), participantNameExpr("p")))
	}

	// Recipient + recipient-name filters - use EXISTS subquery (becomes
	// semi-join).
	//
	// When BOTH the email and the display name are filtered, they must match
	// the SAME to/cc/bcc row, not two independent EXISTS that a
	// multi-recipient message could satisfy via different rows.
	if filter.Recipient != "" && filter.RecipientName != "" {
		conditions = append(conditions, fmt.Sprintf(`EXISTS (
			SELECT 1 FROM mr
			JOIN p ON p.id = mr.participant_id
			WHERE mr.message_id = msg.id
			  AND mr.recipient_type IN ('to', 'cc', 'bcc')
			  AND p.email_address = ?
			  AND %s = ?
		)`, recipientNameExpr("mr", "p")))
		args = append(args, filter.Recipient, filter.RecipientName)
	} else if filter.Recipient != "" {
		conditions = append(conditions, `EXISTS (
			SELECT 1 FROM mr
			JOIN p ON p.id = mr.participant_id
			WHERE mr.message_id = msg.id
			  AND mr.recipient_type IN ('to', 'cc', 'bcc')
			  AND p.email_address = ?
		)`)
		args = append(args, filter.Recipient)
	} else if filter.MatchesEmpty(ViewRecipients) {
		conditions = append(conditions, "NOT EXISTS (SELECT 1 FROM mr WHERE mr.message_id = msg.id AND mr.recipient_type IN ('to', 'cc', 'bcc'))")
	}

	// Recipient name filter - use EXISTS subquery (becomes semi-join). When
	// the recipient email is also set, the combined predicate above already
	// constrains the name to the same to/cc/bcc row.
	if filter.RecipientName != "" && filter.Recipient == "" {
		conditions = append(conditions, fmt.Sprintf(`EXISTS (
			SELECT 1 FROM mr
			JOIN p ON p.id = mr.participant_id
			WHERE mr.message_id = msg.id
			  AND mr.recipient_type IN ('to', 'cc', 'bcc')
			  AND %s = ?
		)`, recipientNameExpr("mr", "p")))
		args = append(args, filter.RecipientName)
	} else if filter.RecipientName == "" && filter.MatchesEmpty(ViewRecipientNames) {
		conditions = append(conditions, fmt.Sprintf(`NOT EXISTS (
			SELECT 1 FROM mr
			JOIN p ON p.id = mr.participant_id
			WHERE mr.message_id = msg.id
			  AND mr.recipient_type IN ('to', 'cc', 'bcc')
			  AND %s != ''
		)`, recipientNameExpr("mr", "p")))
	}

	// Domain filter - use EXISTS subquery (becomes semi-join)
	if filter.Domain != "" {
		conditions = append(conditions, `EXISTS (
			SELECT 1 FROM mr
			JOIN p ON p.id = mr.participant_id
			WHERE mr.message_id = msg.id
			  AND mr.recipient_type = 'from'
			  AND p.domain = ?
		)`)
		args = append(args, filter.Domain)
	} else if filter.MatchesEmpty(ViewDomains) {
		conditions = append(conditions, `NOT EXISTS (
			SELECT 1 FROM mr
			JOIN p ON p.id = mr.participant_id
			WHERE mr.message_id = msg.id
			  AND mr.recipient_type = 'from'
			  AND p.domain IS NOT NULL
			  AND p.domain != ''
		)`)
	}

	// Label filter - case-insensitive EXISTS subquery (becomes semi-join)
	if filter.Label != "" {
		conditions = append(conditions, `EXISTS (
			SELECT 1 FROM ml
			JOIN lbl ON lbl.id = ml.label_id
			WHERE ml.message_id = msg.id
			  AND lbl.name ILIKE ? ESCAPE '\'
		)`)
		args = append(args, escapeILIKE(filter.Label))
	} else if filter.MatchesEmpty(ViewLabels) {
		conditions = append(conditions, "NOT EXISTS (SELECT 1 FROM ml WHERE ml.message_id = msg.id)")
	}

	// Time period filter
	if filter.TimeRange.Period != "" {
		granularity := inferTimeGranularity(filter.TimeRange.Granularity, filter.TimeRange.Period)
		conditions = append(conditions, timeExpr(granularity)+" = ?")
		args = append(args, filter.TimeRange.Period)
	}

	if len(conditions) == 0 {
		return "1=1", args
	}
	return strings.Join(conditions, " AND "), args
}

// inferTimeGranularity adjusts the granularity based on the time period string length.
func inferTimeGranularity(base TimeGranularity, period string) TimeGranularity {
	if base == TimeYear && len(period) > 4 {
		switch len(period) {
		case 7:
			return TimeMonth
		case 10:
			return TimeDay
		}
	}
	return base
}

// SubAggregate performs aggregation on a filtered subset of messages.
// This is used for sub-grouping after drill-down.
func (e *DuckDBEngine) SubAggregate(ctx context.Context, filter MessageFilter, groupBy ViewType, opts AggregateOptions) ([]AggregateRow, error) {
	release, err := e.acquireQuerySlot(ctx)
	if err != nil {
		return nil, err
	}
	defer release()

	def, err := getViewDef(groupBy, opts.TimeGranularity, "agg")
	if err != nil {
		return nil, err
	}

	// Reconcile opts.HideDeletedFromSource into filter so the helper
	// inside buildFilterConditions sees the OR of both fields.
	if opts.HideDeletedFromSource {
		filter.HideDeletedFromSource = true
	}
	where, args := e.buildFilterConditions(filter)

	// Add opts-based conditions (source_id, date range, attachment filter)
	if opts.SourceID != nil {
		where += " AND msg.source_id = ?"
		args = append(args, *opts.SourceID)
	}
	if opts.After != nil {
		where += " AND msg.sent_at >= CAST(? AS TIMESTAMP)"
		args = append(args, opts.After.Format("2006-01-02 15:04:05"))
	}
	if opts.Before != nil {
		where += " AND msg.sent_at < CAST(? AS TIMESTAMP)"
		args = append(args, opts.Before.Format("2006-01-02 15:04:05"))
	}
	if opts.WithAttachmentsOnly {
		where += " AND msg.has_attachments = true"
	}

	// Add search query conditions using the view's key columns
	searchConds, searchArgs := e.buildAggregateSearchConditions(opts.SearchQuery, def.keyColumns...)
	var whereSb1064 strings.Builder
	for _, cond := range searchConds {
		whereSb1064.WriteString(" AND " + cond)
	}
	where += whereSb1064.String()
	args = append(args, searchArgs...)

	return e.runAggregation(ctx, def, where, args, opts)
}

// executeAggregateQuery runs an aggregate query and returns the results.
// Expects 6 columns: key, count, total_size, attachment_size, attachment_count, total_unique.
func (e *DuckDBEngine) executeAggregateQuery(ctx context.Context, query string, args []any) ([]AggregateRow, error) {
	rows, err := e.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("aggregate query: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var results []AggregateRow
	for rows.Next() {
		var row AggregateRow
		// SQL uses CAST(... AS BIGINT) so we can scan directly into int64
		var attachmentSize sql.NullInt64
		var attachmentCount sql.NullInt64
		if err := rows.Scan(&row.Key, &row.Count, &row.TotalSize, &attachmentSize, &attachmentCount, &row.TotalUnique); err != nil {
			return nil, fmt.Errorf("scan aggregate row: %w", err)
		}
		if attachmentSize.Valid {
			row.AttachmentSize = attachmentSize.Int64
		}
		if attachmentCount.Valid {
			row.AttachmentCount = attachmentCount.Int64
		}
		results = append(results, row)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate aggregate rows: %w", err)
	}

	return results, nil
}

// GetTotalStats returns overall statistics from Parquet.
func (e *DuckDBEngine) GetTotalStats(ctx context.Context, opts StatsOptions) (*TotalStats, error) {
	release, err := e.acquireQuerySlot(ctx)
	if err != nil {
		return nil, err
	}
	defer release()

	stats := &TotalStats{}

	var conditions []string
	var args []any
	hasExplicitMessageTypes := false
	if opts.SearchQuery != "" {
		q := search.Parse(opts.SearchQuery)
		hasExplicitMessageTypes = len(q.MessageTypes) > 0
	}

	// Restrict to email messages only; NULL and '' handle pre-message_type data.
	if !hasExplicitMessageTypes {
		conditions = append(conditions, emailOnlyFilterMsg)
	}
	conditions = append(conditions, store.LiveMessagesWhere("msg", opts.HideDeletedFromSource))
	conditions, args = appendSourceFilter(conditions, args, "msg.", opts.SourceID, opts.SourceIDs)

	if opts.WithAttachmentsOnly {
		conditions = append(conditions, "msg.has_attachments = 1")
	}

	// Search filter — uses EXISTS subqueries so no row multiplication.
	// For 1:N views (Recipients, RecipientNames, Labels), filter on the
	// grouping key columns so stats match the visible aggregate rows.
	if opts.SearchQuery != "" {
		searchConds, searchArgs := e.buildStatsSearchConditions(opts.SearchQuery, opts.GroupBy)
		conditions = append(conditions, searchConds...)
		args = append(args, searchArgs...)
	}

	whereClause := "1=1"
	if len(conditions) > 0 {
		whereClause = strings.Join(conditions, " AND ")
	}

	// Message stats - join with attachment aggregates
	msgQuery := fmt.Sprintf(`
		WITH %s
		SELECT
			COUNT(*) as message_count,
			COALESCE(SUM(CASE WHEN msg.deleted_from_source_at IS NULL THEN 1 ELSE 0 END), 0) as active_count,
			COALESCE(SUM(CASE WHEN msg.deleted_from_source_at IS NOT NULL THEN 1 ELSE 0 END), 0) as source_deleted_count,
			COALESCE(SUM(CAST(msg.size_estimate AS BIGINT)), 0) as total_size,
			CAST(COALESCE(SUM(att.attachment_count), 0) AS BIGINT) as attachment_count,
			CAST(COALESCE(SUM(att.attachment_size), 0) AS BIGINT) as attachment_size,
			COUNT(DISTINCT msg.source_id) as account_count
		FROM msg
		LEFT JOIN att ON att.message_id = msg.id
		WHERE %s
	`, e.parquetCTEs(), whereClause)

	var attachmentSize sql.NullFloat64
	err = e.db.QueryRowContext(ctx, msgQuery, args...).Scan(
		&stats.MessageCount,
		&stats.ActiveMessageCount,
		&stats.SourceDeletedMessageCount,
		&stats.TotalSize,
		&stats.AttachmentCount,
		&attachmentSize,
		&stats.AccountCount,
	)
	if err != nil {
		return nil, fmt.Errorf("stats query: %w", err)
	}

	if attachmentSize.Valid {
		stats.AttachmentSize = int64(attachmentSize.Float64)
	}

	// Label count from joined tables
	labelQuery := fmt.Sprintf(`
		WITH %s
		SELECT COUNT(DISTINCT lbl.name)
		FROM msg
		JOIN ml ON ml.message_id = msg.id
		JOIN lbl ON lbl.id = ml.label_id
		WHERE %s
	`, e.parquetCTEs(), whereClause)

	if err := e.db.QueryRowContext(ctx, labelQuery, args...).Scan(&stats.LabelCount); err != nil {
		// Non-fatal: label count is informational, but log for debugging
		log.Printf("warning: label count query failed (using 0): %v", err)
		stats.LabelCount = 0
	}

	return stats, nil
}

// ListAccounts returns accounts from SQLite via DuckDB's sqlite_scan,
// or via direct SQLite connection on platforms without sqlite_scanner.
func (e *DuckDBEngine) ListAccounts(ctx context.Context) ([]AccountInfo, error) {
	if e.sqliteEngine != nil {
		return e.sqliteEngine.ListAccounts(ctx)
	}
	if !e.hasSQLite() {
		return nil, errors.New("ListAccounts requires SQLite: pass sqlitePath to NewDuckDBEngine")
	}

	rows, err := e.db.QueryContext(ctx, `
		SELECT id, source_type, identifier, COALESCE(display_name, '')
		FROM sqlite_db.sources
		ORDER BY identifier
	`)
	if err != nil {
		return nil, fmt.Errorf("list accounts: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var accounts []AccountInfo
	for rows.Next() {
		var acc AccountInfo
		if err := rows.Scan(&acc.ID, &acc.SourceType, &acc.Identifier, &acc.DisplayName); err != nil {
			return nil, fmt.Errorf("scan account: %w", err)
		}
		accounts = append(accounts, acc)
	}

	return accounts, rows.Err()
}

// ListMessages retrieves messages from Parquet files for fast filtered queries.
// Joins normalized Parquet tables to reconstruct denormalized view.
func (e *DuckDBEngine) ListMessages(ctx context.Context, filter MessageFilter) ([]MessageSummary, error) {
	where, args := e.buildFilterConditions(filter)

	// Build ORDER BY
	var orderBy string
	switch filter.Sorting.Field {
	case MessageSortByDate:
		orderBy = "msg.sent_at"
	case MessageSortBySize:
		orderBy = "msg.size_estimate"
	case MessageSortBySubject:
		orderBy = "msg.subject"
	default:
		orderBy = "msg.sent_at"
	}
	if filter.Sorting.Direction == SortDesc {
		orderBy += " DESC"
	} else {
		orderBy += " ASC"
	}
	// Append the unique PK as a tiebreaker so messages sharing the primary
	// sort key (e.g. identical sent_at) get a total, stable order — otherwise
	// LIMIT/OFFSET pagination can drop or duplicate rows across pages. Applied
	// to both the pagination-determining CTE order and the outer in-page order
	// below (both consume this orderBy). [C3]
	orderBy += ", msg.id DESC"

	limit := filter.Pagination.Limit
	if limit == 0 {
		limit = 500
	}

	// Optimized query structure:
	// 1. filtered_msgs: filter and paginate message IDs first (EXISTS becomes semi-join)
	// 2. msg_sender: only compute sender info for the filtered messages
	// 3. Final SELECT: join filtered messages with sender info
	query := fmt.Sprintf(`
		WITH %s,
		filtered_msgs AS (
			SELECT msg.id
			FROM msg
			WHERE %s
			ORDER BY %s
			LIMIT ? OFFSET ?
		),
		msg_sender AS (
			SELECT mr.message_id,
				   FIRST(p.email_address) as from_email,
				   FIRST(COALESCE(NULLIF(TRIM(mr.display_name), ''), NULLIF(TRIM(p.display_name), ''), NULLIF(p.phone_number, ''), p.email_address, '')) as from_name,
				   FIRST(COALESCE(p.phone_number, '')) as from_phone
			FROM mr
			JOIN p ON p.id = mr.participant_id
			WHERE mr.recipient_type = 'from'
			  AND mr.message_id IN (SELECT id FROM filtered_msgs)
			GROUP BY mr.message_id
		),
		direct_sender AS (
			SELECT msg.id as message_id,
				   COALESCE(p.email_address, '') as from_email,
				   COALESCE(p.display_name, '') as from_name,
				   COALESCE(p.phone_number, '') as from_phone
			FROM msg
			JOIN filtered_msgs fm ON fm.id = msg.id
			JOIN p ON p.id = msg.sender_id
			WHERE msg.sender_id IS NOT NULL
			  AND msg.id NOT IN (SELECT message_id FROM msg_sender)
		)
		SELECT
			msg.id,
			COALESCE(msg.source_message_id, '') as source_message_id,
			COALESCE(msg.conversation_id, 0) as conversation_id,
			COALESCE(c.source_conversation_id, '') as source_conversation_id,
			COALESCE(msg.subject, '') as subject,
			COALESCE(msg.snippet, '') as snippet,
			COALESCE(ms.from_email, ds.from_email, '') as from_email,
			COALESCE(ms.from_name, ds.from_name, '') as from_name,
			COALESCE(ms.from_phone, ds.from_phone, '') as from_phone,
			msg.sent_at,
			COALESCE(msg.size_estimate, 0) as size_estimate,
			COALESCE(msg.has_attachments, false) as has_attachments,
			COALESCE(msg.attachment_count, 0) as attachment_count,
			msg.deleted_from_source_at,
			COALESCE(msg.message_type, '') as message_type,
			COALESCE(c.title, '') as conv_title
		FROM msg
		JOIN filtered_msgs fm ON fm.id = msg.id
		LEFT JOIN msg_sender ms ON ms.message_id = msg.id
		LEFT JOIN direct_sender ds ON ds.message_id = msg.id
		LEFT JOIN conv c ON c.id = msg.conversation_id
		ORDER BY %s
	`, e.parquetCTEs(), where, orderBy, orderBy)

	args = append(args, limit, filter.Pagination.Offset)

	rows, err := e.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list messages: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var results []MessageSummary
	for rows.Next() {
		var msg MessageSummary
		var sentAt sql.NullTime
		var deletedAt sql.NullTime
		if err := rows.Scan(
			&msg.ID,
			&msg.SourceMessageID,
			&msg.ConversationID,
			&msg.SourceConversationID,
			&msg.Subject,
			&msg.Snippet,
			&msg.FromEmail,
			&msg.FromName,
			&msg.FromPhone,
			&sentAt,
			&msg.SizeEstimate,
			&msg.HasAttachments,
			&msg.AttachmentCount,
			&deletedAt,
			&msg.MessageType,
			&msg.ConversationTitle,
		); err != nil {
			return nil, fmt.Errorf("scan message: %w", err)
		}
		if sentAt.Valid {
			msg.SentAt = sentAt.Time
		}
		if deletedAt.Valid {
			msg.DeletedAt = &deletedAt.Time
		}
		results = append(results, msg)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate messages: %w", err)
	}

	if len(results) > 0 {
		if err := e.fetchParticipantsForMessages(ctx, results); err != nil {
			return nil, fmt.Errorf("fetch participants: %w", err)
		}
	}

	return results, nil
}

func (e *DuckDBEngine) fetchParticipantsForMessages(ctx context.Context, messages []MessageSummary) error {
	if len(messages) == 0 {
		return nil
	}
	if e.sqliteEngine != nil {
		return fetchParticipantsForMessageList(ctx, e.sqliteEngine.db, noopRebind, "", messages)
	}

	ids := make([]any, len(messages))
	placeholders := make([]string, len(messages))
	idToIndex := make(map[int64]int, len(messages))
	for i, msg := range messages {
		ids[i] = msg.ID
		placeholders[i] = "?"
		idToIndex[msg.ID] = i
	}

	rows, err := e.db.QueryContext(ctx, fmt.Sprintf(`
		WITH %s
		SELECT mr.message_id,
		       mr.recipient_type,
		       COALESCE(NULLIF(p.email_address, ''), NULLIF(p.phone_number, ''), '') AS address,
		       %s AS name
		FROM mr
		JOIN p ON p.id = mr.participant_id
		WHERE mr.message_id IN (%s)
		  AND mr.recipient_type IN ('to', 'cc', 'bcc')
		ORDER BY mr.message_id
	`, e.parquetCTEs(), recipientNameExpr("mr", "p"), strings.Join(placeholders, ",")), ids...)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var messageID int64
		var recipType, email, name string
		if err := rows.Scan(&messageID, &recipType, &email, &name); err != nil {
			return err
		}
		idx, ok := idToIndex[messageID]
		if !ok {
			continue
		}
		appendSummaryRecipient(&messages[idx], recipType, Address{Email: email, Name: name})
	}

	return rows.Err()
}

// parseLabelsJSON parses JSON array format into string slice.
// We use to_json(labels) in the SQL query to get proper JSON encoding,
// which handles commas, quotes, and special characters in label names.
func parseLabelsJSON(s string) []string {
	if s == "" || s == "[]" || s == "null" {
		return nil
	}
	var labels []string
	if err := json.Unmarshal([]byte(s), &labels); err != nil {
		// Fallback: if JSON parsing fails, return empty
		return nil
	}
	return labels
}

// fetchLabelsForMessages adds labels to message summaries.
// Uses DuckDB's sqlite_scanner when available, otherwise direct SQLite.
func (e *DuckDBEngine) fetchLabelsForMessages(ctx context.Context, messages []MessageSummary) error {
	if len(messages) == 0 {
		return nil
	}

	// Prefer direct SQLite (works on all platforms including Windows)
	if e.sqliteEngine != nil {
		return e.sqliteEngine.fetchLabelsForMessages(ctx, messages)
	}

	if !e.hasSQLite() {
		log.Printf("[warn] fetchLabelsForMessages: no label source available (sqliteEngine=nil, hasSQLiteScanner=false); labels will be empty")
		return nil
	}

	return fetchLabelsForMessageList(ctx, e.db, noopRebind, "sqlite_db.", messages)
}

// GetMessageSummariesByIDs delegates to the SQLite engine — the
// summary lookup is a small handful of indexed queries that gain
// nothing from going through Parquet, and SQLite's plan keeps the
// caller-supplied id order intact.
func (e *DuckDBEngine) GetMessageSummariesByIDs(ctx context.Context, ids []int64) ([]MessageSummary, error) {
	if e.sqliteEngine == nil {
		return nil, errors.New("GetMessageSummariesByIDs requires SQLite: pass sqlitePath to NewDuckDBEngine")
	}
	return e.sqliteEngine.GetMessageSummariesByIDs(ctx, ids)
}

// GetMessage retrieves a full message from SQLite.
// Uses direct SQLite connection when available for better BLOB handling.
func (e *DuckDBEngine) GetMessage(ctx context.Context, id int64) (*MessageDetail, error) {
	// Prefer direct SQLite for body/BLOB retrieval
	if e.sqliteEngine != nil {
		return e.sqliteEngine.GetMessage(ctx, id)
	}

	// Fall back to sqlite_scan
	if !e.hasSQLite() {
		return nil, errors.New("GetMessage requires SQLite: pass sqlitePath to NewDuckDBEngine")
	}

	return e.getMessageByQuery(ctx, "m.id = ?", id)
}

// GetMessageBySourceID retrieves a message by source ID from SQLite.
// Uses direct SQLite connection when available for better BLOB handling.
func (e *DuckDBEngine) GetMessageBySourceID(ctx context.Context, sourceMessageID string) (*MessageDetail, error) {
	// Prefer direct SQLite for body/BLOB retrieval
	if e.sqliteEngine != nil {
		return e.sqliteEngine.GetMessageBySourceID(ctx, sourceMessageID)
	}

	// Fall back to sqlite_scan
	if !e.hasSQLite() {
		return nil, errors.New("GetMessageBySourceID requires SQLite: pass sqlitePath to NewDuckDBEngine")
	}

	return e.getMessageByQuery(ctx, "m.source_message_id = ?", sourceMessageID)
}

// GetAttachment retrieves attachment metadata by ID.
// Attachments live in SQLite, so delegate to the SQLite engine.
func (e *DuckDBEngine) GetAttachment(ctx context.Context, id int64) (*AttachmentInfo, error) {
	if e.sqliteEngine != nil {
		return e.sqliteEngine.GetAttachment(ctx, id)
	}
	return nil, errors.New("GetAttachment requires SQLite: pass sqliteDB to NewDuckDBEngine")
}

// GetAttachmentsByHash retrieves attachment metadata by content hash.
// Attachments live in SQLite, so delegate to the SQLite engine.
func (e *DuckDBEngine) GetAttachmentsByHash(ctx context.Context, contentHash string) ([]AttachmentInfo, error) {
	if e.sqliteEngine != nil {
		return e.sqliteEngine.GetAttachmentsByHash(ctx, contentHash)
	}
	return nil, errors.New("GetAttachmentsByHash requires SQLite: pass sqliteDB to NewDuckDBEngine")
}

// GetMessageRaw returns the decompressed raw MIME data for a message.
func (e *DuckDBEngine) GetMessageRaw(ctx context.Context, id int64) ([]byte, error) {
	if e.sqliteDB != nil {
		return getMessageRawShared(ctx, e.sqliteDB, noopRebind, "", id)
	}
	return nil, errors.New("GetMessageRaw requires SQLite: pass sqliteDB to NewDuckDBEngine")
}

func (e *DuckDBEngine) getMessageByQuery(ctx context.Context, whereClause string, args ...any) (*MessageDetail, error) {
	return getMessageByQueryShared(ctx, e.db, noopRebind, "sqlite_db.", whereClause, args...)
}

// Search performs a Gmail-style search query.
// Uses direct SQLite connection for FTS5 support when available,
// falls back to LIKE queries via sqlite_scan otherwise.
func (e *DuckDBEngine) Search(ctx context.Context, q *search.Query, limit, offset int) ([]MessageSummary, error) {
	// Prefer direct SQLite for FTS5 support
	if e.sqliteEngine != nil {
		return e.sqliteEngine.Search(ctx, q, limit, offset)
	}

	// Fall back to sqlite_scan with LIKE queries (no FTS)
	if !e.hasSQLite() {
		return nil, errors.New("Search requires SQLite: pass sqlitePath to NewDuckDBEngine")
	}

	var conditions []string
	var args []any
	var joins []string

	// Exclude rows soft-deleted by deduplicate; gate source-deleted on
	// q.HideDeleted via the helper.
	conditions = append(conditions, store.LiveMessagesWhere("m", q.HideDeleted))

	// From filter
	if len(q.FromAddrs) > 0 {
		joins = append(joins, `
			JOIN sqlite_db.message_recipients mr_from ON mr_from.message_id = m.id AND mr_from.recipient_type = 'from'
			JOIN sqlite_db.participants p_from ON p_from.id = mr_from.participant_id
		`)
		placeholders := make([]string, len(q.FromAddrs))
		for i, addr := range q.FromAddrs {
			placeholders[i] = "?"
			args = append(args, addr)
		}
		conditions = append(conditions, fmt.Sprintf("LOWER(p_from.email_address) IN (%s)", strings.Join(placeholders, ",")))
	}

	// To filter
	if len(q.ToAddrs) > 0 {
		joins = append(joins, `
			JOIN sqlite_db.message_recipients mr_to ON mr_to.message_id = m.id AND mr_to.recipient_type = 'to'
			JOIN sqlite_db.participants p_to ON p_to.id = mr_to.participant_id
		`)
		placeholders := make([]string, len(q.ToAddrs))
		for i, addr := range q.ToAddrs {
			placeholders[i] = "?"
			args = append(args, addr)
		}
		conditions = append(conditions, fmt.Sprintf("LOWER(p_to.email_address) IN (%s)", strings.Join(placeholders, ",")))
	}

	// Label filter
	if len(q.Labels) > 0 {
		joins = append(joins, `
			JOIN sqlite_db.message_labels ml ON ml.message_id = m.id
			JOIN sqlite_db.labels l ON l.id = ml.label_id
		`)
		placeholders := make([]string, len(q.Labels))
		for i, label := range q.Labels {
			placeholders[i] = "?"
			args = append(args, label)
		}
		conditions = append(conditions, fmt.Sprintf("l.name IN (%s)", strings.Join(placeholders, ",")))
	}

	// Subject filter (case-insensitive with ILIKE)
	if len(q.SubjectTerms) > 0 {
		for _, term := range q.SubjectTerms {
			conditions = append(conditions, "m.subject ILIKE ?")
			args = append(args, "%"+term+"%")
		}
	}

	if len(q.MessageTypes) > 0 {
		condition, conditionArgs := duckDBMessageTypeCondition("m", q.MessageTypes)
		if condition != "" {
			conditions = append(conditions, condition)
			args = append(args, conditionArgs...)
		}
	}

	// Has attachment filter
	if q.HasAttachment != nil && *q.HasAttachment {
		conditions = append(conditions, "m.has_attachments = 1")
	}

	// Date range filters
	if q.AfterDate != nil {
		conditions = append(conditions, "m.sent_at >= CAST(? AS TIMESTAMP)")
		args = append(args, q.AfterDate.Format("2006-01-02 15:04:05"))
	}
	if q.BeforeDate != nil {
		conditions = append(conditions, "m.sent_at < CAST(? AS TIMESTAMP)")
		args = append(args, q.BeforeDate.Format("2006-01-02 15:04:05"))
	}

	// Size filters
	if q.LargerThan != nil {
		conditions = append(conditions, "m.size_estimate > ?")
		args = append(args, *q.LargerThan)
	}
	if q.SmallerThan != nil {
		conditions = append(conditions, "m.size_estimate < ?")
		args = append(args, *q.SmallerThan)
	}

	// Full-text search: use ILIKE fallback (FTS5 not available via sqlite_scan)
	// Only search subject/snippet; body is in separate table, use FTS for body search
	if len(q.TextTerms) > 0 {
		for _, term := range q.TextTerms {
			likeTerm := "%" + term + "%"
			conditions = append(conditions, "(m.subject ILIKE ? OR m.snippet ILIKE ?)")
			args = append(args, likeTerm, likeTerm)
		}
	}

	// Account filter
	conditions, args = appendSourceFilter(conditions, args, "m.", nil, q.AccountIDs)

	if limit == 0 {
		limit = 100
	}

	query := fmt.Sprintf(`
		SELECT DISTINCT
			m.id,
			m.source_message_id,
			m.conversation_id,
			COALESCE(conv.source_conversation_id, ''),
			COALESCE(m.subject, ''),
			COALESCE(m.snippet, ''),
			COALESCE(p_sender.email_address, ''),
			COALESCE(p_sender.display_name, ''),
			m.sent_at,
			COALESCE(m.size_estimate, 0),
			m.has_attachments,
			m.attachment_count,
			m.deleted_from_source_at,
			COALESCE(m.message_type, '')
		FROM sqlite_db.messages m
		LEFT JOIN sqlite_db.message_recipients mr_sender ON mr_sender.message_id = m.id AND mr_sender.recipient_type = 'from'
		LEFT JOIN sqlite_db.participants p_sender ON p_sender.id = mr_sender.participant_id
		LEFT JOIN sqlite_db.conversations conv ON conv.id = m.conversation_id
		%s
		WHERE %s
		ORDER BY m.sent_at DESC
		LIMIT ? OFFSET ?
	`, strings.Join(joins, "\n"), strings.Join(conditions, " AND "))

	args = append(args, limit, offset)

	rows, err := e.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("search messages: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var results []MessageSummary
	for rows.Next() {
		var msg MessageSummary
		var sentAt sql.NullTime
		var deletedAt sql.NullTime
		if err := rows.Scan(
			&msg.ID,
			&msg.SourceMessageID,
			&msg.ConversationID,
			&msg.SourceConversationID,
			&msg.Subject,
			&msg.Snippet,
			&msg.FromEmail,
			&msg.FromName,
			&sentAt,
			&msg.SizeEstimate,
			&msg.HasAttachments,
			&msg.AttachmentCount,
			&deletedAt,
			&msg.MessageType,
		); err != nil {
			return nil, fmt.Errorf("scan message: %w", err)
		}
		if sentAt.Valid {
			msg.SentAt = sentAt.Time
		}
		if deletedAt.Valid {
			msg.DeletedAt = &deletedAt.Time
		}
		results = append(results, msg)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate messages: %w", err)
	}

	// Fetch labels for results
	if len(results) > 0 {
		if err := e.fetchLabelsForMessages(ctx, results); err != nil {
			return nil, fmt.Errorf("fetch labels: %w", err)
		}
	}

	return results, nil
}

// SearchMessageBodies delegates to the direct SQLite engine so FTS5 can scope
// MATCH to the indexed body column. The sqlite_scanner fallback is
// intentionally unsupported: exact body search must never scan message_bodies.
func (e *DuckDBEngine) SearchMessageBodies(ctx context.Context, q *search.Query, limit, offset int) ([]MessageSummary, error) {
	if e.sqliteEngine == nil {
		return nil, fmt.Errorf("%w: a direct SQLite engine is required; reopen the query engine with a SQLite connection", ErrMessageBodySearchUnavailable)
	}
	return e.sqliteEngine.SearchMessageBodies(ctx, q, limit, offset)
}

// SearchByDomains returns message summaries for the given sender domains.
// It delegates to SQLite because domain search needs JOINs across
// participants and message_recipients that the Parquet cache doesn't carry.
func (e *DuckDBEngine) SearchByDomains(ctx context.Context, domains []string, after, before *time.Time, limit, offset int) ([]MessageSummary, error) {
	// Delegate to SQLite — domain search requires JOINs across participants
	// and message_recipients which are not available in the Parquet cache.
	if e.sqliteEngine != nil {
		return e.sqliteEngine.SearchByDomains(ctx, domains, after, before, limit, offset)
	}
	return nil, errors.New("SearchByDomains requires SQLite engine (participant data not in Parquet cache)")
}

func (e *DuckDBEngine) GetGmailIDsByFilter(ctx context.Context, filter MessageFilter) ([]string, error) {
	// Delegate to SQLite for authoritative deletion status.
	// Parquet cache may be stale if deletions occurred after the last build.
	if e.sqliteEngine != nil {
		return e.sqliteEngine.GetGmailIDsByFilter(ctx, filter)
	}

	// Fall back to Parquet if no SQLite engine available (shouldn't happen in practice)
	if e.analyticsDir == "" {
		return nil, errors.New("GetGmailIDsByFilter requires SQLite or Parquet data")
	}

	var conditions []string
	var args []any

	// Always exclude deleted messages.
	// Always pass true: this surface feeds remote-deletion staging and
	// must never honor an opt-in.
	conditions = append(conditions, store.LiveMessagesWhere("msg", true))
	conditions, args = appendSourceFilter(conditions, args, "msg.", filter.SourceID, filter.SourceIDs)

	// Use EXISTS subqueries for filtering (becomes semi-joins, no duplicates).
	// When BOTH the email and the display name are filtered, they must match
	// the SAME from-row (or the SAME direct sender), not two independent
	// EXISTS that a multi-author message could satisfy via different rows.
	if filter.Sender != "" && filter.SenderName != "" {
		conditions = append(conditions, fmt.Sprintf(`(EXISTS (
			SELECT 1 FROM mr
			JOIN p ON p.id = mr.participant_id
			WHERE mr.message_id = msg.id
			  AND mr.recipient_type = 'from'
			  AND (p.email_address = ? OR p.phone_number = ?)
			  AND %s = ?
		) OR EXISTS (
			SELECT 1 FROM p
			WHERE p.id = msg.sender_id
			  AND (p.email_address = ? OR p.phone_number = ?)
			  AND %s = ?
		))`, recipientNameExpr("mr", "p"), participantNameExpr("p")))
		args = append(args, filter.Sender, filter.Sender, filter.SenderName, filter.Sender, filter.Sender, filter.SenderName)
	} else if filter.Sender != "" {
		conditions = append(conditions, `(EXISTS (
			SELECT 1 FROM mr
			JOIN p ON p.id = mr.participant_id
			WHERE mr.message_id = msg.id
			  AND mr.recipient_type = 'from'
			  AND (p.email_address = ? OR p.phone_number = ?)
		) OR EXISTS (
			SELECT 1 FROM p
			WHERE p.id = msg.sender_id
			  AND (p.email_address = ? OR p.phone_number = ?)
		))`)
		args = append(args, filter.Sender, filter.Sender, filter.Sender, filter.Sender)
	} else if filter.SenderName != "" {
		conditions = append(conditions, fmt.Sprintf(`(EXISTS (
			SELECT 1 FROM mr
			JOIN p ON p.id = mr.participant_id
			WHERE mr.message_id = msg.id
			  AND mr.recipient_type = 'from'
			  AND %s = ?
		) OR EXISTS (
			SELECT 1 FROM p
			WHERE p.id = msg.sender_id
			  AND %s = ?
		))`, recipientNameExpr("mr", "p"), participantNameExpr("p")))
		args = append(args, filter.SenderName, filter.SenderName)
	}

	// When BOTH the recipient email and the display name are filtered, they
	// must match the SAME to/cc/bcc row, not two independent EXISTS that a
	// multi-recipient message could satisfy via different rows.
	if filter.Recipient != "" && filter.RecipientName != "" {
		conditions = append(conditions, fmt.Sprintf(`EXISTS (
			SELECT 1 FROM mr
			JOIN p ON p.id = mr.participant_id
			WHERE mr.message_id = msg.id
			  AND mr.recipient_type IN ('to', 'cc', 'bcc')
			  AND p.email_address = ?
			  AND %s = ?
		)`, recipientNameExpr("mr", "p")))
		args = append(args, filter.Recipient, filter.RecipientName)
	} else if filter.Recipient != "" {
		conditions = append(conditions, `EXISTS (
			SELECT 1 FROM mr
			JOIN p ON p.id = mr.participant_id
			WHERE mr.message_id = msg.id
			  AND mr.recipient_type IN ('to', 'cc', 'bcc')
			  AND p.email_address = ?
		)`)
		args = append(args, filter.Recipient)
	} else if filter.RecipientName != "" {
		conditions = append(conditions, fmt.Sprintf(`EXISTS (
			SELECT 1 FROM mr
			JOIN p ON p.id = mr.participant_id
			WHERE mr.message_id = msg.id
			  AND mr.recipient_type IN ('to', 'cc', 'bcc')
			  AND %s = ?
		)`, recipientNameExpr("mr", "p")))
		args = append(args, filter.RecipientName)
	}

	if filter.Domain != "" {
		conditions = append(conditions, `EXISTS (
			SELECT 1 FROM mr
			JOIN p ON p.id = mr.participant_id
			WHERE mr.message_id = msg.id
			  AND mr.recipient_type = 'from'
			  AND p.domain = ?
		)`)
		args = append(args, filter.Domain)
	}

	if filter.Label != "" {
		conditions = append(conditions, `EXISTS (
			SELECT 1 FROM ml
			JOIN lbl ON lbl.id = ml.label_id
			WHERE ml.message_id = msg.id
			  AND lbl.name ILIKE ? ESCAPE '\'
		)`)
		args = append(args, escapeILIKE(filter.Label))
	}

	if filter.TimeRange.Period != "" {
		granularity := inferTimeGranularity(filter.TimeRange.Granularity, filter.TimeRange.Period)
		// GetGmailIDsByFilter uses strftime for time filtering (no year/month columns)
		var te string
		switch granularity {
		case TimeYear:
			te = "strftime(msg.sent_at, '%Y')"
		case TimeDay:
			te = "strftime(msg.sent_at, '%Y-%m-%d')"
		default:
			te = "strftime(msg.sent_at, '%Y-%m')"
		}
		conditions = append(conditions, te+" = ?")
		args = append(args, filter.TimeRange.Period)
	}

	if filter.After != nil {
		conditions = append(conditions, "msg.sent_at >= ?")
		args = append(args, *filter.After)
	}
	if filter.Before != nil {
		conditions = append(conditions, "msg.sent_at < ?")
		args = append(args, *filter.Before)
	}

	// Build query — JOIN src to scope to Gmail sources authoritatively.
	query := fmt.Sprintf(`
		WITH %s
		SELECT msg.source_message_id
		FROM msg
		JOIN src ON src.id = msg.source_id AND COALESCE(src.source_type, 'gmail') = 'gmail'
		WHERE %s
		ORDER BY msg.sent_at DESC, msg.id DESC
	`, e.parquetCTEs(), strings.Join(conditions, " AND "))

	// Only add LIMIT if explicitly set (0 means no limit)
	if filter.Pagination.Limit > 0 {
		query += " LIMIT ?"
		args = append(args, filter.Pagination.Limit)
	}

	rows, err := e.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("get gmail ids: %w", err)
	}
	defer func() { _ = rows.Close() }()

	return collectGmailIDs(rows)
}

// GetGmailIDsByMessageIDs returns Gmail message IDs for internal message
// IDs, enforcing the same live-message and Gmail-source constraints as
// GetGmailIDsByFilter. Non-qualifying IDs are silently dropped. The
// lookup is chunked so large explicit selections stay under
// bind-parameter limits.
func (e *DuckDBEngine) GetGmailIDsByMessageIDs(ctx context.Context, ids []int64) ([]string, error) {
	// Delegate to SQLite for authoritative deletion status.
	if e.sqliteEngine != nil {
		return e.sqliteEngine.GetGmailIDsByMessageIDs(ctx, ids)
	}
	if e.analyticsDir == "" {
		return nil, errors.New("GetGmailIDsByMessageIDs requires SQLite or Parquet data")
	}
	if len(ids) == 0 {
		return nil, nil
	}
	return gmailIDsByMessageIDsChunked(ctx, ids, e.gmailIDsForMessageIDChunk)
}

func (e *DuckDBEngine) gmailIDsForMessageIDChunk(ctx context.Context, ids []int64) ([]gmailIDRow, error) {
	placeholders := make([]string, len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		placeholders[i] = "?"
		args[i] = id
	}
	query := fmt.Sprintf(`
		WITH %s
		SELECT msg.source_message_id, msg.sent_at, msg.id
		FROM msg
		JOIN src ON src.id = msg.source_id AND COALESCE(src.source_type, 'gmail') = 'gmail'
		WHERE %s AND msg.id IN (%s)
	`, e.parquetCTEs(), store.LiveMessagesWhere("msg", true), strings.Join(placeholders, ","))

	rows, err := e.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("get gmail ids by message ids: %w", err)
	}
	defer func() { _ = rows.Close() }()

	return collectGmailIDRows(rows)
}

// GetAccountsByGmailIDs returns the distinct Gmail account identifiers
// owning live messages with the given Gmail IDs, sorted ascending. See
// the SQLite implementation for the deletion-staging rationale and the
// chunking that keeps large selections under bind-parameter limits.
func (e *DuckDBEngine) GetAccountsByGmailIDs(ctx context.Context, gmailIDs []string) ([]string, error) {
	// Delegate to SQLite for authoritative deletion status.
	if e.sqliteEngine != nil {
		return e.sqliteEngine.GetAccountsByGmailIDs(ctx, gmailIDs)
	}
	if e.analyticsDir == "" {
		return nil, errors.New("GetAccountsByGmailIDs requires SQLite or Parquet data")
	}
	if len(gmailIDs) == 0 {
		return nil, nil
	}
	return accountsByGmailIDsChunked(ctx, gmailIDs, e.accountsForGmailIDChunk)
}

func (e *DuckDBEngine) accountsForGmailIDChunk(ctx context.Context, gmailIDs []string) ([]string, error) {
	placeholders := make([]string, len(gmailIDs))
	args := make([]any, len(gmailIDs))
	for i, id := range gmailIDs {
		placeholders[i] = "?"
		args[i] = id
	}
	// The Parquet sources dataset exposes the account identifier as
	// account_email (see build-cache ETL).
	query := fmt.Sprintf(`
		WITH %s
		SELECT src.account_email
		FROM src
		WHERE COALESCE(src.source_type, 'gmail') = 'gmail' AND EXISTS (
			SELECT 1 FROM msg
			WHERE msg.source_id = src.id AND %s AND msg.source_message_id IN (%s)
		)
		ORDER BY src.account_email
	`, e.parquetCTEs(), store.LiveMessagesWhere("msg", true), strings.Join(placeholders, ","))

	rows, err := e.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("get accounts by gmail ids: %w", err)
	}
	defer func() { _ = rows.Close() }()

	return collectGmailIDs(rows)
}

// HasParquetData checks if Parquet files exist and are usable.
func HasParquetData(analyticsDir string) bool {
	pattern := filepath.Join(analyticsDir, datasetMessages, "**", "*.parquet")
	matches, err := filepath.Glob(filepath.Join(analyticsDir, datasetMessages, "*", "*.parquet"))
	if err != nil {
		return false
	}
	_ = pattern // Used in queries, not glob
	return len(matches) > 0
}

// RequiredParquetDirs lists the analytics subdirectories that must each
// contain at least one .parquet file for the cache to be considered complete.
// Shared between the cache builder, TUI, and MCP startup paths.
var RequiredParquetDirs = []string{
	datasetMessages,
	"sources",
	datasetParticipants,
	"message_recipients",
	"labels",
	"message_labels",
	"attachments",
	datasetConversations,
}

// HasCompleteParquetData checks that all required parquet tables exist.
// Use this instead of HasParquetData when enabling DuckDB, since DuckDB
// unconditionally reads all tables (including conversations) and will fail
// at runtime if any are missing.
func HasCompleteParquetData(analyticsDir string) bool {
	for _, dir := range RequiredParquetDirs {
		pattern := filepath.Join(analyticsDir, dir, "*.parquet")
		matches, _ := filepath.Glob(pattern)
		if len(matches) > 0 {
			continue
		}
		// For messages, also check hive-partitioned layout (messages/year=*/*.parquet)
		if dir == datasetMessages {
			deepMatches, _ := filepath.Glob(filepath.Join(analyticsDir, dir, "*", "*.parquet"))
			if len(deepMatches) > 0 {
				continue
			}
		}
		return false
	}
	return true
}

// ParquetSyncState represents the sync state from _last_sync.json.
type ParquetSyncState struct {
	LastMessageID int64     `json:"last_message_id"`
	LastSyncAt    time.Time `json:"last_sync_at,omitzero"`
}

// SearchFast searches message metadata in Parquet files (no body text).
// This is much faster than FTS search for large archives.
// Searches subject, snippet, and sender/recipient metadata (case-insensitive).
func (e *DuckDBEngine) SearchFast(ctx context.Context, q *search.Query, filter MessageFilter, limit, offset int) ([]MessageSummary, error) {
	release, err := e.acquireQuerySlot(ctx)
	if err != nil {
		return nil, err
	}
	defer release()

	conditions, args := e.buildSearchConditions(q, filter)

	if limit == 0 {
		limit = 100
	}

	// Query with JOINs to reconstruct denormalized view
	query := fmt.Sprintf(`
		WITH %s,
		msg_labels AS (
			SELECT ml.message_id, LIST(lbl.name ORDER BY lbl.name) as labels
			FROM ml
			JOIN lbl ON lbl.id = ml.label_id
			GROUP BY ml.message_id
		),
		msg_sender AS (
			SELECT mr.message_id,
				   FIRST(p.email_address) as from_email,
				   FIRST(COALESCE(NULLIF(TRIM(mr.display_name), ''), NULLIF(TRIM(p.display_name), ''), NULLIF(p.phone_number, ''), p.email_address, '')) as from_name,
				   FIRST(COALESCE(p.phone_number, '')) as from_phone
			FROM mr
			JOIN p ON p.id = mr.participant_id
			WHERE mr.recipient_type = 'from'
			GROUP BY mr.message_id
		),
		direct_sender AS (
			SELECT msg.id as message_id,
				   COALESCE(p.email_address, '') as from_email,
				   COALESCE(p.display_name, '') as from_name,
				   COALESCE(p.phone_number, '') as from_phone
			FROM msg
			JOIN p ON p.id = msg.sender_id
			WHERE msg.sender_id IS NOT NULL
			  AND msg.id NOT IN (SELECT message_id FROM msg_sender)
		)
		SELECT
			COALESCE(msg.id, 0) as id,
			COALESCE(msg.source_message_id, '') as source_message_id,
			COALESCE(msg.conversation_id, 0) as conversation_id,
			COALESCE(c.source_conversation_id, '') as source_conversation_id,
			COALESCE(msg.subject, '') as subject,
			COALESCE(msg.snippet, '') as snippet,
			COALESCE(ms.from_email, ds.from_email, '') as from_email,
			COALESCE(ms.from_name, ds.from_name, '') as from_name,
			COALESCE(ms.from_phone, ds.from_phone, '') as from_phone,
			msg.sent_at,
			COALESCE(msg.size_estimate, 0) as size_estimate,
			COALESCE(msg.has_attachments, false) as has_attachments,
			COALESCE(att.attachment_count, 0) as attachment_count,
			CAST(COALESCE(to_json(mlbl.labels), '[]') AS VARCHAR) as labels,
			msg.deleted_from_source_at,
			COALESCE(msg.message_type, '') as message_type,
			COALESCE(c.title, '') as conv_title
		FROM msg
		LEFT JOIN msg_sender ms ON ms.message_id = msg.id
		LEFT JOIN direct_sender ds ON ds.message_id = msg.id
		LEFT JOIN att ON att.message_id = msg.id
		LEFT JOIN msg_labels mlbl ON mlbl.message_id = msg.id
		LEFT JOIN conv c ON c.id = msg.conversation_id
		WHERE %s
		ORDER BY msg.sent_at DESC, msg.id DESC
		LIMIT ? OFFSET ?
	`, e.parquetCTEs(), strings.Join(conditions, " AND "))

	args = append(args, limit, offset)

	rows, err := e.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("search fast: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var results []MessageSummary
	for rows.Next() {
		var msg MessageSummary
		var sentAt sql.NullTime
		var deletedAt sql.NullTime
		var labelsJSON string
		if err := rows.Scan(
			&msg.ID,
			&msg.SourceMessageID,
			&msg.ConversationID,
			&msg.SourceConversationID,
			&msg.Subject,
			&msg.Snippet,
			&msg.FromEmail,
			&msg.FromName,
			&msg.FromPhone,
			&sentAt,
			&msg.SizeEstimate,
			&msg.HasAttachments,
			&msg.AttachmentCount,
			&labelsJSON,
			&deletedAt,
			&msg.MessageType,
			&msg.ConversationTitle,
		); err != nil {
			return nil, fmt.Errorf("scan message: %w", err)
		}
		if sentAt.Valid {
			msg.SentAt = sentAt.Time
		}
		if deletedAt.Valid {
			msg.DeletedAt = &deletedAt.Time
		}
		// Parse labels from JSON array format
		msg.Labels = parseLabelsJSON(labelsJSON)
		results = append(results, msg)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate messages: %w", err)
	}

	return results, nil
}

// SearchFastCount returns the total count of messages matching a search query.
// This is used for pagination UI to show "N of M results".
func (e *DuckDBEngine) SearchFastCount(ctx context.Context, q *search.Query, filter MessageFilter) (int64, error) {
	release, err := e.acquireQuerySlot(ctx)
	if err != nil {
		return 0, err
	}
	defer release()

	conditions, args := e.buildSearchConditions(q, filter)

	// Count with JOINs for filters that need them
	query := fmt.Sprintf(`
		WITH %s,
		msg_sender AS (
			SELECT mr.message_id,
				   FIRST(p.email_address) as from_email,
				   FIRST(COALESCE(NULLIF(TRIM(mr.display_name), ''), NULLIF(TRIM(p.display_name), ''), NULLIF(p.phone_number, ''), p.email_address, '')) as from_name,
				   FIRST(COALESCE(p.phone_number, '')) as from_phone
			FROM mr
			JOIN p ON p.id = mr.participant_id
			WHERE mr.recipient_type = 'from'
			GROUP BY mr.message_id
		),
		direct_sender AS (
			SELECT msg.id as message_id,
				   COALESCE(p.email_address, '') as from_email,
				   COALESCE(p.display_name, '') as from_name,
				   COALESCE(p.phone_number, '') as from_phone
			FROM msg
			JOIN p ON p.id = msg.sender_id
			WHERE msg.sender_id IS NOT NULL
			  AND msg.id NOT IN (SELECT message_id FROM msg_sender)
		)
		SELECT COUNT(*) as cnt
		FROM msg
		LEFT JOIN msg_sender ms ON ms.message_id = msg.id
		LEFT JOIN direct_sender ds ON ds.message_id = msg.id
		WHERE %s
	`, e.parquetCTEs(), strings.Join(conditions, " AND "))

	var count int64
	if err := e.db.QueryRowContext(ctx, query, args...).Scan(&count); err != nil {
		return 0, fmt.Errorf("search fast count: %w", err)
	}
	return count, nil
}

// searchCacheKeyFor builds a deterministic cache key from search conditions,
// args, and the Parquet cache fingerprint. Same query+filter over the same
// analytics cache always produces the same key. Uses JSON encoding to avoid
// ambiguity from delimiter collisions (e.g. args containing commas or pipes).
func searchCacheKeyFor(conditions []string, args []any, cacheFP string) string {
	// JSON marshaling is unambiguous: each element is quoted/escaped independently.
	// Errors are impossible for string/int/float/bool args, but fall back to fmt.
	key := struct {
		C  []string `json:"c"`
		A  []any    `json:"a"`
		FP string   `json:"fp"`
	}{conditions, args, cacheFP}
	b, err := json.Marshal(key)
	if err != nil {
		// Fallback: should never happen with the types buildSearchConditions produces.
		return fmt.Sprintf("%v#%v#%s", conditions, args, cacheFP)
	}
	return string(b)
}

// dropSearchCache drops the cached temp table and clears all cache fields.
// Uses context.Background() so cleanup succeeds even if the caller's context
// is canceled (avoiding leaked temp tables on the single DuckDB connection).
// Caller must hold e.searchCacheMu.
func (e *DuckDBEngine) dropSearchCache() {
	if e.searchCacheTable != "" {
		_, _ = e.db.ExecContext(context.Background(), "DROP TABLE IF EXISTS "+e.searchCacheTable)
	}
	e.searchCacheKey = ""
	e.searchCacheTable = ""
	e.searchCacheCount = 0
	e.searchCacheStats = nil
}

// searchPageFromCache executes Phase 3 (paginated results) from the cached temp table.
// Returns a SearchFastResult with cached count and stats.
func (e *DuckDBEngine) searchPageFromCache(ctx context.Context, limit, offset int) (*SearchFastResult, error) {
	pageQuery := fmt.Sprintf(`
		WITH %s,
		page AS (
			SELECT sm.id FROM %s sm
			ORDER BY sm.sent_at DESC, sm.id DESC
			LIMIT ? OFFSET ?
		),
		msg_labels AS (
			SELECT ml.message_id, LIST(lbl.name ORDER BY lbl.name) as labels
			FROM ml
			JOIN lbl ON lbl.id = ml.label_id
			WHERE ml.message_id IN (SELECT id FROM page)
			GROUP BY ml.message_id
		)
		SELECT
			sm.id,
			sm.source_message_id,
			sm.conversation_id,
			COALESCE(c.source_conversation_id, '') as source_conversation_id,
			sm.subject,
			sm.snippet,
			sm.from_email,
			sm.from_name,
			COALESCE(sm.from_phone, '') as from_phone,
			sm.sent_at,
			sm.size_estimate,
			sm.has_attachments,
			COALESCE(att.attachment_count, 0) as attachment_count,
			CAST(COALESCE(to_json(mlbl.labels), '[]') AS VARCHAR) as labels,
			sm.deleted_from_source_at,
			COALESCE(sm.message_type, '') as message_type,
			COALESCE(c.title, '') as conv_title
		FROM %s sm
		JOIN page p ON p.id = sm.id
		LEFT JOIN att ON att.message_id = sm.id
		LEFT JOIN msg_labels mlbl ON mlbl.message_id = sm.id
		LEFT JOIN conv c ON c.id = sm.conversation_id
		ORDER BY sm.sent_at DESC, sm.id DESC
	`, e.parquetCTEs(), e.searchCacheTable, e.searchCacheTable)

	rows, err := e.db.QueryContext(ctx, pageQuery, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("search fast page: %w", err)
	}
	defer func() { _ = rows.Close() }()

	// Return a copy of cached stats to prevent callers from mutating the cache
	var statsCopy *TotalStats
	if e.searchCacheStats != nil {
		tmp := *e.searchCacheStats
		statsCopy = &tmp
	}

	result := &SearchFastResult{
		TotalCount: e.searchCacheCount,
		Stats:      statsCopy,
	}

	for rows.Next() {
		var msg MessageSummary
		var sentAt sql.NullTime
		var deletedAt sql.NullTime
		var labelsJSON string
		if err := rows.Scan(
			&msg.ID,
			&msg.SourceMessageID,
			&msg.ConversationID,
			&msg.SourceConversationID,
			&msg.Subject,
			&msg.Snippet,
			&msg.FromEmail,
			&msg.FromName,
			&msg.FromPhone,
			&sentAt,
			&msg.SizeEstimate,
			&msg.HasAttachments,
			&msg.AttachmentCount,
			&labelsJSON,
			&deletedAt,
			&msg.MessageType,
			&msg.ConversationTitle,
		); err != nil {
			return nil, fmt.Errorf("scan message: %w", err)
		}
		if sentAt.Valid {
			msg.SentAt = sentAt.Time
		}
		if deletedAt.Valid {
			msg.DeletedAt = &deletedAt.Time
		}
		msg.Labels = parseLabelsJSON(labelsJSON)
		result.Messages = append(result.Messages, msg)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate messages: %w", err)
	}

	return result, nil
}

// computeSearchStats computes stats (Phase 4) from the cached temp table.
// Returns nil stats on failure (best-effort).
func (e *DuckDBEngine) computeSearchStats(ctx context.Context) *TotalStats {
	msgStatsQuery := fmt.Sprintf(`
		WITH %s
		SELECT
			COUNT(*) as message_count,
			COALESCE(SUM(CASE WHEN sm.deleted_from_source_at IS NULL THEN 1 ELSE 0 END), 0) as active_count,
			COALESCE(SUM(CASE WHEN sm.deleted_from_source_at IS NOT NULL THEN 1 ELSE 0 END), 0) as source_deleted_count,
			COALESCE(SUM(sm.size_estimate), 0) as total_size,
			CAST(COALESCE(SUM(att.attachment_count), 0) AS BIGINT) as attachment_count,
			CAST(COALESCE(SUM(att.attachment_size), 0) AS BIGINT) as attachment_size,
			COUNT(DISTINCT sm.source_id) as account_count
		FROM %s sm
		LEFT JOIN att ON att.message_id = sm.id
	`, e.parquetCTEs(), e.searchCacheTable)

	stats := &TotalStats{}
	var attachmentSize sql.NullFloat64
	if err := e.db.QueryRowContext(ctx, msgStatsQuery).Scan(
		&stats.MessageCount,
		&stats.ActiveMessageCount,
		&stats.SourceDeletedMessageCount,
		&stats.TotalSize,
		&stats.AttachmentCount,
		&attachmentSize,
		&stats.AccountCount,
	); err != nil {
		log.Printf("warning: search stats query failed (stats will be nil): %v", err)
		return nil
	}
	if attachmentSize.Valid {
		stats.AttachmentSize = int64(attachmentSize.Float64)
	}

	// Label count — only ml/lbl Parquet tables needed.
	labelStatsQuery := fmt.Sprintf(`
		WITH %s
		SELECT COUNT(DISTINCT lbl.name)
		FROM %s sm
		JOIN ml ON ml.message_id = sm.id
		JOIN lbl ON lbl.id = ml.label_id
	`, e.parquetCTEs(), e.searchCacheTable)

	if err := e.db.QueryRowContext(ctx, labelStatsQuery).Scan(&stats.LabelCount); err != nil {
		log.Printf("warning: search label count query failed (using 0): %v", err)
		stats.LabelCount = 0
	}

	return stats
}

// SearchFastWithStats performs a single-scan fast search with temp table materialization.
// It denormalizes matching messages (with sender info) into a temp table using one
// Parquet scan, then reuses the in-memory temp table for count, pagination, and stats
// — eliminating all subsequent msg Parquet reads. Only small page-scoped lookups
// into label/attachment Parquet tables remain.
//
// The temp table is cached internally: if the same search conditions+args are
// requested again (e.g. pagination), the Parquet scan is skipped and the page
// is served directly from the cached temp table. A new search invalidates the
// old cache.
func (e *DuckDBEngine) SearchFastWithStats(ctx context.Context, q *search.Query, queryStr string,
	filter MessageFilter, statsGroupBy ViewType, limit, offset int) (*SearchFastResult, error) {
	release, err := e.acquireQuerySlot(ctx)
	if err != nil {
		return nil, err
	}
	defer release()

	conditions, args := e.buildSearchConditions(q, filter)

	if limit == 0 {
		limit = 100
	}

	e.searchCacheMu.Lock()
	defer e.searchCacheMu.Unlock()

	// Refresh before checking the search cache. A cache-hit page query may call
	// parquetCTEs(), but that is too late to decide whether the already
	// materialized temp table still represents the current Parquet data.
	e.ensureFreshOptionalCols()

	// Check cache: same conditions+args+Parquet fingerprint means same search,
	// serve from cached table.
	cacheKey := searchCacheKeyFor(conditions, args, e.currentCacheFingerprint())
	if cacheKey == e.searchCacheKey && e.searchCacheTable != "" {
		// Retry stats if a previous attempt failed (transient error).
		if e.searchCacheStats == nil {
			e.searchCacheStats = e.computeSearchStats(ctx)
		}
		return e.searchPageFromCache(ctx, limit, offset)
	}

	// Cache miss — drop old cache and materialize fresh.
	e.dropSearchCache()

	// Unique temp table name to avoid concurrent collisions.
	seq := e.tempTableSeq.Add(1)
	tempTable := fmt.Sprintf("_search_matches_%d", seq)

	// Phase 1: Materialize matching messages into temp table (single Parquet scan).
	// Stores all columns needed by later phases so they never re-read msg Parquet.
	// The msg_sender CTE is required because buildSearchConditions references ms.from_email.
	materializeQuery := fmt.Sprintf(`
		CREATE TEMP TABLE %s AS
		WITH %s,
		msg_sender AS (
			SELECT mr.message_id,
				   FIRST(p.email_address) as from_email,
				   FIRST(COALESCE(NULLIF(TRIM(mr.display_name), ''), NULLIF(TRIM(p.display_name), ''), NULLIF(p.phone_number, ''), p.email_address, '')) as from_name,
				   FIRST(COALESCE(p.phone_number, '')) as from_phone
			FROM mr
			JOIN p ON p.id = mr.participant_id
			WHERE mr.recipient_type = 'from'
			GROUP BY mr.message_id
		),
		direct_sender AS (
			SELECT msg.id as message_id,
				   COALESCE(p.email_address, '') as from_email,
				   COALESCE(p.display_name, '') as from_name,
				   COALESCE(p.phone_number, '') as from_phone
			FROM msg
			JOIN p ON p.id = msg.sender_id
			WHERE msg.sender_id IS NOT NULL
			  AND msg.id NOT IN (SELECT message_id FROM msg_sender)
		)
		SELECT
			msg.id,
			COALESCE(msg.source_message_id, '') as source_message_id,
			COALESCE(msg.conversation_id, 0) as conversation_id,
			COALESCE(msg.subject, '') as subject,
			COALESCE(msg.snippet, '') as snippet,
			COALESCE(ms.from_email, ds.from_email, '') as from_email,
			COALESCE(ms.from_name, ds.from_name, '') as from_name,
			COALESCE(ms.from_phone, ds.from_phone, '') as from_phone,
			msg.sent_at,
			COALESCE(CAST(msg.size_estimate AS BIGINT), 0) as size_estimate,
			COALESCE(msg.has_attachments, false) as has_attachments,
			msg.deleted_from_source_at,
			CAST(msg.source_id AS BIGINT) as source_id,
			COALESCE(msg.message_type, '') as message_type
		FROM msg
		LEFT JOIN msg_sender ms ON ms.message_id = msg.id
		LEFT JOIN direct_sender ds ON ds.message_id = msg.id
		WHERE %s
	`, tempTable, e.parquetCTEs(), strings.Join(conditions, " AND "))

	if _, err := e.db.ExecContext(ctx, materializeQuery, args...); err != nil {
		return nil, fmt.Errorf("materialize search matches: %w", err)
	}

	// Store temp table name so we can clean up on error.
	e.searchCacheTable = tempTable

	// Phase 2: Count (trivial — reads in-memory temp table only).
	// Best-effort: if count fails, use -1 (unknown total) and continue.
	var count int64
	if err := e.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+tempTable).Scan(&count); err != nil {
		log.Printf("warning: search count query failed (using -1): %v", err)
		count = -1
	}
	e.searchCacheCount = count

	// Phase 4: Stats from temp table (compute before page so cache is fully populated).
	e.searchCacheStats = e.computeSearchStats(ctx)

	// Store cache key — cache is now valid.
	e.searchCacheKey = cacheKey

	// Phase 3: Paginated results from cached temp table.
	return e.searchPageFromCache(ctx, limit, offset)
}

// buildSearchConditions builds WHERE conditions for search queries.
// Shared by SearchFast and SearchFastCount.
// Note: These conditions reference msg and ms (msg_sender) CTEs.
func (e *DuckDBEngine) buildSearchConditions(q *search.Query, filter MessageFilter) ([]string, []any) {
	var conditions []string
	var args []any

	// Apply basic filter conditions (ignoring join flags for search - we handle those differently)
	conditions = append(conditions,
		store.LiveMessagesWhere("msg", filter.HideDeletedFromSource),
	)
	messageTypes, noMessageTypeMatches := ScopedMessageTypes(q.MessageTypes, filter.MessageType)
	switch {
	case noMessageTypeMatches:
		conditions = append(conditions, "1=0")
	case len(messageTypes) > 0:
		condition, conditionArgs := duckDBMessageTypeCondition("msg", messageTypes)
		if condition != "" {
			conditions = append(conditions, condition)
			args = append(args, conditionArgs...)
		}
	default:
		// Restrict to email messages only; NULL and '' handle pre-message_type data.
		conditions = append(conditions, emailOnlyFilterMsg)
	}
	conditions, args = appendSourceFilter(conditions, args, "msg.", filter.SourceID, filter.SourceIDs)
	if filter.After != nil {
		conditions = append(conditions, "msg.sent_at >= CAST(? AS TIMESTAMP)")
		args = append(args, filter.After.Format("2006-01-02 15:04:05"))
	}
	if filter.Before != nil {
		conditions = append(conditions, "msg.sent_at < CAST(? AS TIMESTAMP)")
		args = append(args, filter.Before.Format("2006-01-02 15:04:05"))
	}
	if filter.WithAttachmentsOnly {
		conditions = append(conditions, "msg.has_attachments = true")
	}
	// Sender filter - check both message_recipients (email/phone) and direct sender_id (WhatsApp/chat)
	if filter.Sender != "" {
		conditions = append(conditions, `(EXISTS (
			SELECT 1 FROM mr
			JOIN p ON p.id = mr.participant_id
			WHERE mr.message_id = msg.id
			  AND mr.recipient_type = 'from'
			  AND (p.email_address = ? OR p.phone_number = ?)
		) OR EXISTS (
			SELECT 1 FROM p
			WHERE p.id = msg.sender_id
			  AND (p.email_address = ? OR p.phone_number = ?)
		))`)
		args = append(args, filter.Sender, filter.Sender, filter.Sender, filter.Sender)
	}
	if filter.Domain != "" {
		conditions = append(conditions, "ms.from_email ILIKE ?")
		args = append(args, "%@"+filter.Domain)
	}
	// Recipient filter - use EXISTS subquery for drill-down context (checks email and phone)
	if filter.Recipient != "" {
		conditions = append(conditions, `EXISTS (
			SELECT 1 FROM mr
			JOIN p ON p.id = mr.participant_id
			WHERE mr.message_id = msg.id
			  AND mr.recipient_type IN ('to', 'cc', 'bcc')
			  AND (p.email_address = ? OR p.phone_number = ?)
		)`)
		args = append(args, filter.Recipient, filter.Recipient)
	}
	// Label filter - use EXISTS subquery for drill-down context
	if filter.Label != "" {
		conditions = append(conditions, `EXISTS (
			SELECT 1 FROM ml
			JOIN lbl ON lbl.id = ml.label_id
			WHERE ml.message_id = msg.id
			  AND lbl.name ILIKE ? ESCAPE '\'
		)`)
		args = append(args, escapeILIKE(filter.Label))
	}
	if filter.TimeRange.Period != "" {
		granularity := inferTimeGranularity(filter.TimeRange.Granularity, filter.TimeRange.Period)
		conditions = append(conditions, timeExpr(granularity)+" = ?")
		args = append(args, filter.TimeRange.Period)
	}

	// Text search terms - search subject, snippet, and every participant row
	// without consulting message bodies (fast path). The EXISTS branch includes
	// all from rows as well as recipients because msg_sender retains only one
	// display sender for result hydration.
	// Uses ILIKE for performance on Parquet scans.
	if len(q.TextTerms) > 0 {
		for _, term := range q.TextTerms {
			termPattern := "%" + escapeILIKE(term) + "%"
			conditions = append(conditions, `(
				msg.subject ILIKE ? ESCAPE '\' OR
				COALESCE(msg.snippet, '') ILIKE ? ESCAPE '\' OR
				COALESCE(ms.from_email, ds.from_email, '') ILIKE ? ESCAPE '\' OR
				COALESCE(ms.from_name, ds.from_name, '') ILIKE ? ESCAPE '\' OR
				COALESCE(ms.from_phone, ds.from_phone, '') ILIKE ? ESCAPE '\' OR
				EXISTS (
					SELECT 1
					FROM mr mr_meta
					JOIN p p_meta ON p_meta.id = mr_meta.participant_id
					WHERE mr_meta.message_id = msg.id
					  AND (
						COALESCE(p_meta.email_address, '') ILIKE ? ESCAPE '\' OR
						COALESCE(p_meta.display_name, '') ILIKE ? ESCAPE '\' OR
						COALESCE(p_meta.phone_number, '') ILIKE ? ESCAPE '\' OR
						COALESCE(mr_meta.display_name, '') ILIKE ? ESCAPE '\'
					  )
				)
			)`)
			for range 9 {
				args = append(args, termPattern)
			}
		}
	}

	// From filter - check email, phone, display name via message_recipients and direct sender_id
	if len(q.FromAddrs) > 0 {
		for _, addr := range q.FromAddrs {
			pattern := "%" + escapeILIKE(addr) + "%"
			conditions = append(conditions, `(EXISTS (
				SELECT 1 FROM mr
				JOIN p ON p.id = mr.participant_id
				WHERE mr.message_id = msg.id
				  AND mr.recipient_type = 'from'
				  AND (p.email_address ILIKE ? ESCAPE '\' OR p.phone_number ILIKE ? ESCAPE '\' OR p.display_name ILIKE ? ESCAPE '\')
			) OR EXISTS (
				SELECT 1 FROM p
				WHERE p.id = msg.sender_id
				  AND (p.email_address ILIKE ? ESCAPE '\' OR p.phone_number ILIKE ? ESCAPE '\' OR p.display_name ILIKE ? ESCAPE '\')
			))`)
			args = append(args, pattern, pattern, pattern, pattern, pattern, pattern)
		}
	}

	// To filter - use EXISTS subquery to check recipients (email and phone)
	if len(q.ToAddrs) > 0 {
		for _, addr := range q.ToAddrs {
			pattern := "%" + escapeILIKE(addr) + "%"
			conditions = append(conditions, `EXISTS (
				SELECT 1 FROM mr
				JOIN p ON p.id = mr.participant_id
				WHERE mr.message_id = msg.id AND mr.recipient_type IN ('to', 'cc', 'bcc')
				AND (p.email_address ILIKE ? ESCAPE '\' OR p.phone_number ILIKE ? ESCAPE '\')
			)`)
			args = append(args, pattern, pattern)
		}
	}

	// Subject filter
	if len(q.SubjectTerms) > 0 {
		for _, term := range q.SubjectTerms {
			conditions = append(conditions, "msg.subject ILIKE ? ESCAPE '\\'")
			args = append(args, "%"+escapeILIKE(term)+"%")
		}
	}

	// Label filter - case-insensitive substring match
	if len(q.Labels) > 0 {
		for _, label := range q.Labels {
			conditions = append(conditions, `EXISTS (
				SELECT 1 FROM ml
				JOIN lbl ON lbl.id = ml.label_id
				WHERE ml.message_id = msg.id AND lbl.name ILIKE ? ESCAPE '\'
			)`)
			args = append(args, "%"+escapeILIKE(label)+"%")
		}
	}

	// Has attachment filter
	if q.HasAttachment != nil && *q.HasAttachment {
		conditions = append(conditions, "msg.has_attachments = 1")
	}

	// Date range filters
	if q.AfterDate != nil {
		conditions = append(conditions, "msg.sent_at >= CAST(? AS TIMESTAMP)")
		args = append(args, q.AfterDate.Format("2006-01-02 15:04:05"))
	}
	if q.BeforeDate != nil {
		conditions = append(conditions, "msg.sent_at < CAST(? AS TIMESTAMP)")
		args = append(args, q.BeforeDate.Format("2006-01-02 15:04:05"))
	}

	// Size filters
	if q.LargerThan != nil {
		conditions = append(conditions, "msg.size_estimate > ?")
		args = append(args, *q.LargerThan)
	}
	if q.SmallerThan != nil {
		conditions = append(conditions, "msg.size_estimate < ?")
		args = append(args, *q.SmallerThan)
	}
	if len(q.MessageTypes) > 0 {
		condition, conditionArgs := duckDBMessageTypeCondition("msg", q.MessageTypes)
		if condition != "" {
			conditions = append(conditions, condition)
			args = append(args, conditionArgs...)
		}
	}

	// Account filter
	conditions, args = appendSourceFilter(conditions, args, "msg.", nil, q.AccountIDs)

	// Default conditions if none specified
	if len(conditions) == 0 {
		conditions = append(conditions, "1=1")
	}

	return conditions, args
}
