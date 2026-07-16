package cmd

import (
	"context"
	"database/sql"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "github.com/duckdb/duckdb-go/v2" // DuckDB driver (database/sql)
	"github.com/gofrs/flock"
	_ "github.com/mattn/go-sqlite3" // SQLite driver (database/sql)
	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/cacheops"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/store"
)

var fullRebuild bool
var buildCacheAutoFlag bool

const buildCacheDaemonSubprocessEnv = "MSGVAULT_DAEMON_BUILD_CACHE_PARENT_PID"

// buildCacheMu serializes concurrent buildCache calls. The scheduler may
// trigger syncs for multiple accounts in parallel, each of which calls
// buildCache on completion. Without this lock, concurrent writes to shared
// files (_last_sync.json, parquet directories) can corrupt the cache.
var buildCacheMu sync.Mutex

// cacheBuildFileLock returns the inter-process lock that serializes cache
// builds across processes. buildCacheMu only covers one process, but cache
// writers span several: the daemon's own build subprocesses and daemon-owned
// CLI children whose ingest commands rebuild the cache in-process via
// rebuildCacheAfterWrite (e.g. the daemon's background startup build racing
// the very sync that auto-started it). The lock file lives NEXT TO the
// analytics directory, not inside it: stateless replacement and account
// removal can replace live dataset directories, while this stable lock inode
// must continue excluding other writers. The OS releases the lock if the
// holder dies.
func cacheBuildFileLock(analyticsDir string) (*flock.Flock, error) {
	lockPath := query.CacheBuildLockPath(analyticsDir)
	if err := os.MkdirAll(filepath.Dir(lockPath), 0755); err != nil {
		return nil, fmt.Errorf("create analytics parent dir: %w", err)
	}
	return flock.New(lockPath), nil
}

// invalidateSyncStateFile makes the cache's _last_sync.json unusable so
// every later staleness probe demands a full rebuild: removal first, and if
// the file cannot be unlinked (e.g. no directory write permission),
// overwriting it with content that fails to parse. Only when neither works
// does it return an error.
func invalidateSyncStateFile(stateFile string) error {
	removeErr := os.Remove(stateFile)
	if removeErr == nil || os.IsNotExist(removeErr) {
		return nil
	}
	if writeErr := os.WriteFile(stateFile, []byte("invalidated"), 0600); writeErr != nil {
		return fmt.Errorf("invalidate cache sync state: remove: %w; overwrite: %w",
			removeErr, writeErr)
	}
	return nil
}

// lockCacheAndInvalidateSyncState returns an exclusive writer lock with the
// cache commit marker already invalidated. Callers must keep the lock through
// their database mutation and the lock-held cache rebuild. A destructive
// mutation must not proceed when either protection step fails.
func lockCacheAndInvalidateSyncState(analyticsDir string) (*flock.Flock, error) {
	buildLock, err := cacheBuildFileLock(analyticsDir)
	if err != nil {
		return nil, fmt.Errorf("create analytics cache lock: %w", err)
	}
	if err := buildLock.Lock(); err != nil {
		return nil, fmt.Errorf("lock analytics cache: %w", err)
	}
	if err := invalidateSyncStateFile(query.CacheStatePath(analyticsDir)); err != nil {
		unlockErr := buildLock.Unlock()
		return nil, errors.Join(
			fmt.Errorf("invalidate analytics cache before mutation: %w", err),
			wrapError(unlockErr, "unlock analytics cache after invalidation failure"),
		)
	}
	return buildLock, nil
}

func wrapError(err error, message string) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", message, err)
}

// buildCacheAfterSnapshotHook is a deterministic test seam for writes that
// race with cache construction after its source watermark is captured.
var buildCacheAfterSnapshotHook func()

// buildCacheBeforeStateWriteHook is a deterministic test seam for source
// mutations that finish after table COPY operations but before cache state is
// persisted.
var buildCacheBeforeStateWriteHook func()

// buildCacheBeforeMessagesExportHook is a deterministic test seam for staged
// export failures before the messages COPY begins.
var buildCacheBeforeMessagesExportHook func() error

// buildCacheWriteStateFile persists the cache sync state; a test seam for
// simulating state persistence failures.
var buildCacheWriteStateFile = os.WriteFile

// cacheSchemaVersion tracks the Parquet schema layout. Bump this whenever
// columns are added/removed/renamed in the COPY queries below so that
// incremental builds automatically trigger a full rebuild instead of
// producing Parquet files with mismatched schemas.
const cacheSchemaVersion = 7 // v7: include meeting transcripts in the searchable Parquet cache

// sentCacheExportMessageWhere identifies rows eligible for the searchable
// Parquet cache. Calendar events are the sole archived message type excluded
// from this dataset; analytics eligibility is enforced separately by the
// query engine.
func sentCacheExportMessageWhere(alias string) string {
	qualifier := ""
	if alias != "" {
		qualifier = alias + "."
	}
	return fmt.Sprintf(
		"%ssent_at IS NOT NULL AND COALESCE(%smessage_type, '') <> 'calendar_event'",
		qualifier, qualifier,
	)
}

func exportableMessageWhere(alias string) string {
	qualifier := ""
	if alias != "" {
		qualifier = alias + "."
	}
	return sentCacheExportMessageWhere(alias) + " AND " + qualifier + "deleted_at IS NULL"
}

func cacheLiveMessageWhere(alias string) string {
	qualifier := ""
	if alias != "" {
		qualifier = alias + "."
	}
	return exportableMessageWhere(alias) + " AND " + qualifier + "deleted_from_source_at IS NULL"
}

// syncState tracks the message and sync-run watermarks covered by the cache.
type syncState = query.CacheSyncState

type cacheSyncCounters struct {
	additions      int64
	updates        int64
	failedRunCount int64
	failedRunIDSum int64
}

type sqlRowQuerier interface {
	QueryRow(query string, args ...any) *sql.Row
}

type sqlRunner interface {
	sqlRowQuerier
	Exec(query string, args ...any) (sql.Result, error)
	Query(query string, args ...any) (*sql.Rows, error)
}

func readCacheSyncCounters(db sqlRowQuerier) (cacheSyncCounters, error) {
	var counters cacheSyncCounters
	err := db.QueryRow(`
		SELECT
			COALESCE(SUM(COALESCE(sr.messages_added, 0)), 0),
			COALESCE(SUM(COALESCE(sr.messages_updated, 0)), 0),
			COALESCE(SUM(CASE WHEN sr.status = 'failed' THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN sr.status = 'failed' THEN sr.id ELSE 0 END), 0)
		FROM sync_runs sr
		JOIN sources src ON src.id = sr.source_id
		WHERE sr.status IN ('completed', 'failed')
		  AND sr.completed_at IS NOT NULL
		  AND src.source_type <> ?
	`, sourceTypeCalendar).Scan(
		&counters.additions,
		&counters.updates,
		&counters.failedRunCount,
		&counters.failedRunIDSum,
	)
	return counters, err
}

var buildCacheCmd = &cobra.Command{
	Use:     "build-cache",
	Aliases: []string{"build-parquet"}, // Backward compatibility
	Short:   "Build analytics cache for fast TUI queries",
	Long: `Build analytics cache from the SQLite database.

This command exports normalized tables to Parquet files for fast aggregate queries.
DuckDB joins the Parquet files at query time, which is much faster than joining
during export (especially for incremental updates).

The cache files are stored in ~/.msgvault/analytics/:
  - messages/year=*/     Core message data, partitioned by year
  - participants/        Email addresses and domains
  - message_recipients/  Links messages to participants (from/to/cc/bcc)
  - labels/              Label definitions
  - message_labels/      Links messages to labels
  - attachments/         Attachment metadata

By default, this performs an incremental update (only adding new messages).
Use --full-rebuild to recreate all cache files from scratch.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		if isDaemonBuildCacheChild() {
			return runBuildCacheLocal(fullRebuild, buildCacheAutoFlag)
		}
		return runBuildCacheHTTP(cmd, fullRebuild)
	},
}

func runBuildCacheHTTP(cmd *cobra.Command, fullRebuild bool) error {
	st, _, err := OpenHTTPStore(cmd.Context())
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	return st.BuildCLICache(cmd.Context(), fullRebuild, func(stream, data string) error {
		switch stream {
		case cliStreamStdout:
			_, err := fmt.Fprint(cmd.OutOrStdout(), data)
			if err != nil {
				return fmt.Errorf("write build-cache stdout: %w", err)
			}
		case cliStreamStderr:
			_, err := fmt.Fprint(cmd.ErrOrStderr(), data)
			if err != nil {
				return fmt.Errorf("write build-cache stderr: %w", err)
			}
		}
		return nil
	})
}

func runBuildCacheLocal(fullRebuild, auto bool) error {
	dbPath := cfg.DatabaseDSN()
	analyticsDir := cfg.AnalyticsDir()

	// The Parquet cache is a SQLite -> DuckDB ETL; feeding a postgres:// DSN to
	// the SQLite driver inside buildCache fails immediately with a confusing
	// driver error.
	if store.IsPostgresURL(dbPath) {
		return errors.New("build-cache is SQLite-only; PostgreSQL backends do not use the Parquet analytics cache")
	}

	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		return fmt.Errorf("database not found: %s\nRun 'msgvault init-db' first", dbPath)
	}

	release, err := acquireBuildCacheWriteLock(cfg)
	if err != nil {
		return err
	}
	defer release()

	// No schema init or startup migrations here: this path only runs as a
	// daemon-owned child (isDaemonBuildCacheChild), and the parent daemon
	// already ran InitSchema at startup. Running runStartupMigrations in the
	// child would apply the deliberately deferred legacy identity migration
	// concurrently with an ingest command, populating account_identities
	// before that ingest's confirmDefaultIdentity and suppressing the
	// source's own address — the exact race the daemon defers it to avoid.

	var result *buildResult
	if auto {
		result, err = buildCacheAuto(dbPath, analyticsDir)
	} else {
		result, err = buildCache(dbPath, analyticsDir, fullRebuild)
	}
	if err != nil {
		return err
	}

	if result.Skipped {
		fmt.Println("No new messages to export.")
	} else {
		fmt.Printf("Exported %d messages to %s\n", result.ExportedCount, result.OutputDir)
	}
	fmt.Println("\nCache build complete! The TUI will now use fast cached queries.")
	return nil
}

func acquireBuildCacheWriteLock(cfg *config.Config) (func(), error) {
	if isDaemonBuildCacheChild() {
		return func() {}, nil
	}
	return acquireDirectSQLiteWriteLock(cfg)
}

func isDaemonBuildCacheChild() bool {
	return os.Getenv(buildCacheDaemonSubprocessEnv) == strconv.Itoa(os.Getppid())
}

type buildResult struct {
	ExportedCount int64
	MaxMessageID  int64
	OutputDir     string
	Skipped       bool
}

// buildCache builds the cache with the caller's explicit fullRebuild demand
// (e.g. `build-cache --full-rebuild`), which is honored unconditionally.
func buildCache(dbPath, analyticsDir string, fullRebuild bool) (*buildResult, error) {
	return buildCacheImpl(dbPath, analyticsDir, fullRebuild, false)
}

// buildCacheAuto builds the cache for automatic (staleness-derived) callers.
// Their fullRebuild decision predates the inter-process build lock, so it is
// re-evaluated once the lock is held: a build another process finished while
// we waited can make the work unnecessary or downgrade a full rebuild to an
// incremental one, instead of erasing a cache that was just completed.
func buildCacheAuto(dbPath, analyticsDir string) (*buildResult, error) {
	return buildCacheImpl(dbPath, analyticsDir, false, true)
}

func buildCacheImpl(dbPath, analyticsDir string, fullRebuild, recheckStaleness bool) (*buildResult, error) {
	buildCacheMu.Lock()
	defer buildCacheMu.Unlock()

	buildLock, err := acquireCacheBuildLock(analyticsDir)
	if err != nil {
		return nil, err
	}
	defer func() { _ = buildLock.Unlock() }()
	return buildCacheLocked(dbPath, analyticsDir, fullRebuild, recheckStaleness)
}

func acquireCacheBuildLock(analyticsDir string) (*flock.Flock, error) {
	buildLock, err := cacheBuildFileLock(analyticsDir)
	if err != nil {
		return nil, err
	}
	if locked, err := buildLock.TryLock(); err != nil {
		return nil, fmt.Errorf("acquire cache build lock: %w", err)
	} else if !locked {
		fmt.Println("Waiting for another msgvault process to finish a cache build...")
		if err := buildLock.Lock(); err != nil {
			return nil, fmt.Errorf("acquire cache build lock: %w", err)
		}
	}
	return buildLock, nil
}

// buildCacheLocked exports and publishes a cache while the caller holds the
// exclusive cross-process cache lock.
func buildCacheLocked(dbPath, analyticsDir string, fullRebuild, recheckStaleness bool) (*buildResult, error) {
	if err := cleanupStaleCacheStaging(analyticsDir); err != nil {
		return nil, err
	}
	if recheckStaleness {
		staleness := cacheNeedsBuild(dbPath, analyticsDir)
		if !staleness.NeedsBuild {
			return &buildResult{Skipped: true, OutputDir: analyticsDir}, nil
		}
		fullRebuild = staleness.FullRebuild
	}

	// Load sync state for incremental updates
	var lastMessageID int64
	var previousState syncState
	var hasPreviousState bool
	readiness, err := query.InspectCacheReadiness(analyticsDir)
	if err != nil {
		return nil, fmt.Errorf("inspect analytics cache before build: %w", err)
	}
	if !fullRebuild && readiness == query.CacheReady {
		state, err := query.ReadCacheSyncState(analyticsDir)
		if err != nil {
			return nil, fmt.Errorf("read analytics cache state before build: %w", err)
		}
		if state.SchemaVersion != cacheSchemaVersion {
			fmt.Printf("Cache schema version mismatch (have v%d, need v%d). Forcing full rebuild.\n",
				state.SchemaVersion, cacheSchemaVersion)
			fullRebuild = true
		} else {
			previousState = state
			hasPreviousState = true
			lastMessageID = state.LastMessageID
		}
	}

	// Keep metadata reads and every source-table export on one SQLite snapshot.
	// On platforms with sqlite_scanner, sqlite_query preserves native SQLite
	// indexes while the surrounding DuckDB transaction pins the same snapshot
	// for the COPY statements. The CSV fallback reads through one SQLite
	// transaction before exposing the static files to DuckDB.
	db, err := sql.Open("duckdb", "")
	if err != nil {
		return nil, fmt.Errorf("open duckdb: %w", err)
	}
	db.SetMaxOpenConns(1)
	defer func() { _ = db.Close() }()
	sourceSnapshot, err := openCacheSourceSnapshot(db, dbPath)
	if err != nil {
		return nil, err
	}
	defer func() { _ = sourceSnapshot.Close() }()

	// Record the freshness boundary immediately before the first source read.
	// A sync or deletion that finishes after this instant may not be represented
	// by the snapshot and must invalidate the cache on the next check.
	cacheWatermark := time.Now().UTC().Truncate(time.Second)

	var maxMessageID sql.NullInt64
	var lastCompletedSyncRunID int64
	var syncCounters cacheSyncCounters
	// Use indexed query: id is PRIMARY KEY, sent_at has an index
	maxIDQuery := `SELECT MAX(id) FROM messages WHERE sent_at IS NOT NULL`
	if err := sourceSnapshot.QueryRow(maxIDQuery).Scan(&maxMessageID); err != nil {
		return nil, fmt.Errorf("get max message id: %w", err)
	}
	maxID := int64(0)
	if maxMessageID.Valid {
		maxID = maxMessageID.Int64
	}
	var hasSyncRunsTable int
	if err := sourceSnapshot.QueryRow(`
		SELECT COUNT(*) FROM sqlite_master
		WHERE type = 'table' AND name = 'sync_runs'
	`).Scan(&hasSyncRunsTable); err != nil {
		return nil, fmt.Errorf("check sync_runs table: %w", err)
	}
	if hasSyncRunsTable > 0 {
		if err := sourceSnapshot.QueryRow(`
			SELECT COALESCE(MAX(id), 0) FROM sync_runs
			WHERE status = 'completed' AND completed_at IS NOT NULL
		`).Scan(&lastCompletedSyncRunID); err != nil {
			return nil, fmt.Errorf("get last completed sync run id: %w", err)
		}
		if syncCounters, err = readCacheSyncCounters(sourceSnapshot); err != nil {
			return nil, fmt.Errorf("get cache sync counters: %w", err)
		}
	}
	if !fullRebuild && hasPreviousState && hasSyncRunsTable > 0 {
		updatesChanged := syncCounters.updates != previousState.LastCacheUpdateCount
		coveredAdditionsChanged := syncCounters.additions != previousState.LastCacheAdditionCount &&
			maxID <= previousState.LastMessageID
		failedSyncChanged := syncCounters.failedRunCount != previousState.LastFailedSyncRunCount ||
			syncCounters.failedRunIDSum != previousState.LastFailedSyncRunIDSum
		if updatesChanged || coveredAdditionsChanged || failedSyncChanged {
			fmt.Println("Existing cached messages changed. Forcing full rebuild...")
			fullRebuild = true
			lastMessageID = 0
		}
	}

	if hasPreviousState && maxID <= lastMessageID && !fullRebuild {
		if err := sourceSnapshot.Close(); err != nil {
			return nil, fmt.Errorf("close SQLite snapshot after metadata check: %w", err)
		}
		return &buildResult{Skipped: true, OutputDir: analyticsDir}, nil
	}

	replaceAll := fullRebuild || !hasPreviousState
	if replaceAll {
		lastMessageID = 0
	}
	var expectedBatchCount, expectedTotalCount int64
	expectedCountQuery := "SELECT COUNT(*) FROM messages WHERE " +
		exportableMessageWhere("") + " AND id <= ? AND id > ?"
	if err := sourceSnapshot.QueryRow(expectedCountQuery, maxID, lastMessageID).Scan(&expectedBatchCount); err != nil {
		return nil, fmt.Errorf("count expected staged messages: %w", err)
	}
	expectedTotalQuery := "SELECT COUNT(*) FROM messages WHERE " +
		exportableMessageWhere("") + " AND id <= ?"
	if err := sourceSnapshot.QueryRow(expectedTotalQuery, maxID).Scan(&expectedTotalCount); err != nil {
		return nil, fmt.Errorf("count expected cached messages: %w", err)
	}
	if buildCacheAfterSnapshotHook != nil {
		buildCacheAfterSnapshotHook()
	}
	if err := sourceSnapshot.Prepare(); err != nil {
		return nil, err
	}

	staging, err := newCacheStaging(analyticsDir)
	if err != nil {
		return nil, err
	}
	defer func() { _ = staging.cleanup() }()
	exportDB := sourceSnapshot.DuckDB()

	// Every COPY targets a same-filesystem sibling staging directory. Live
	// Parquet and its commit marker remain untouched until verification passes.
	for _, subdir := range query.RequiredParquetDirs {
		if err := os.MkdirAll(filepath.Join(staging.root, subdir), 0755); err != nil {
			return nil, fmt.Errorf("create %s dir: %w", subdir, err)
		}
	}

	if replaceAll {
		fmt.Println("Building analytics cache...")
	} else {
		fmt.Println("Updating analytics cache...")
	}
	buildStart := time.Now()

	// Build WHERE clause for incremental exports
	idFilter := fmt.Sprintf(" AND TRY_CAST(m.id AS BIGINT) <= %d", maxID)
	if !replaceAll && lastMessageID > 0 {
		idFilter += fmt.Sprintf(" AND TRY_CAST(m.id AS BIGINT) > %d", lastMessageID)
	}

	// Junction rows are searchable exactly when their parent message is
	// exportable. This includes meeting attendees and excludes calendar
	// invitees, hidden rows, and messages without a timestamp.
	exportableJunctionWhere := fmt.Sprintf(
		"TRY_CAST(message_id AS BIGINT) IN (SELECT CAST(m.id AS BIGINT) FROM sqlite_db.messages m WHERE %s AND TRY_CAST(m.id AS BIGINT) <= %d)",
		exportableMessageWhere("m"), maxID,
	)
	junctionFilter := func(incremental string) string {
		if incremental != "" {
			return incremental + " AND " + exportableJunctionWhere
		}
		return " WHERE " + exportableJunctionWhere
	}

	junctionFile := "data.parquet"

	// runExport executes a COPY query and prints timing info.
	runExport := func(label, query string) error {
		start := time.Now()
		fmt.Printf("  %-25s", label+"...")
		if _, err := exportDB.Exec(query); err != nil {
			fmt.Println()
			return err
		}
		fmt.Printf(" done (%s)\n", time.Since(start).Round(time.Millisecond))
		return nil
	}

	// Export each table separately - this is MUCH faster than joining during export
	// because DuckDB can use SQLite indexes efficiently for simple queries

	// 1. Export message_recipients (large junction table)
	recipientsDir := filepath.Join(staging.root, "message_recipients")
	escapedRecipientsDir := strings.ReplaceAll(recipientsDir, "'", "''")
	recipientsFilter := ""
	if !replaceAll && lastMessageID > 0 {
		recipientsFilter = fmt.Sprintf(" WHERE message_id > %d", lastMessageID)
	}
	recipientsFilter = junctionFilter(recipientsFilter)
	if err := runExport("message_recipients", fmt.Sprintf(`
	COPY (
		SELECT
			message_id,
			participant_id,
			recipient_type,
			COALESCE(TRY_CAST(display_name AS VARCHAR), '') as display_name
		FROM sqlite_db.message_recipients%s
	) TO '%s/%s' (
		FORMAT PARQUET,
		COMPRESSION 'zstd'
	)
	`, recipientsFilter, escapedRecipientsDir, junctionFile)); err != nil {
		return nil, fmt.Errorf("export message_recipients: %w", err)
	}

	// 2. Export message_labels (large junction table)
	messageLabelsDir := filepath.Join(staging.root, "message_labels")
	escapedMessageLabelsDir := strings.ReplaceAll(messageLabelsDir, "'", "''")
	messageLabelsFilter := ""
	if !replaceAll && lastMessageID > 0 {
		messageLabelsFilter = fmt.Sprintf(" WHERE message_id > %d", lastMessageID)
	}
	messageLabelsFilter = junctionFilter(messageLabelsFilter)
	if err := runExport("message_labels", fmt.Sprintf(`
	COPY (
		SELECT
			message_id,
			label_id
		FROM sqlite_db.message_labels%s
	) TO '%s/%s' (
		FORMAT PARQUET,
		COMPRESSION 'zstd'
	)
	`, messageLabelsFilter, escapedMessageLabelsDir, junctionFile)); err != nil {
		return nil, fmt.Errorf("export message_labels: %w", err)
	}

	// 3. Export attachments
	attachmentsDir := filepath.Join(staging.root, tableAttachments)
	escapedAttachmentsDir := strings.ReplaceAll(attachmentsDir, "'", "''")
	attachmentsFilter := ""
	if !replaceAll && lastMessageID > 0 {
		attachmentsFilter = fmt.Sprintf(" WHERE message_id > %d", lastMessageID)
	}
	attachmentsFilter = junctionFilter(attachmentsFilter)
	if err := runExport(tableAttachments, fmt.Sprintf(`
	COPY (
		SELECT
			message_id,
			size,
			COALESCE(TRY_CAST(filename AS VARCHAR), '') as filename
		FROM sqlite_db.attachments%s
	) TO '%s/%s' (
		FORMAT PARQUET,
		COMPRESSION 'zstd'
	)
	`, attachmentsFilter, escapedAttachmentsDir, junctionFile)); err != nil {
		return nil, fmt.Errorf("export attachments: %w", err)
	}

	// 4. Export participants
	participantsDir := filepath.Join(staging.root, tableParticipants)
	escapedParticipantsDir := strings.ReplaceAll(participantsDir, "'", "''")
	if err := runExport(tableParticipants, fmt.Sprintf(`
	COPY (
		SELECT
			id,
			COALESCE(TRY_CAST(email_address AS VARCHAR), '') as email_address,
			COALESCE(TRY_CAST(domain AS VARCHAR), '') as domain,
			COALESCE(TRY_CAST(display_name AS VARCHAR), '') as display_name,
			COALESCE(TRY_CAST(phone_number AS VARCHAR), '') as phone_number
		FROM sqlite_db.participants
	) TO '%s/participants.parquet' (
		FORMAT PARQUET,
		COMPRESSION 'zstd'
	)
	`, escapedParticipantsDir)); err != nil {
		return nil, fmt.Errorf("export participants: %w", err)
	}

	// 5. Export labels
	labelsDir := filepath.Join(staging.root, tableLabels)
	escapedLabelsDir := strings.ReplaceAll(labelsDir, "'", "''")
	if err := runExport(tableLabels, fmt.Sprintf(`
	COPY (
		SELECT
			id,
			COALESCE(TRY_CAST(name AS VARCHAR), '') as name
		FROM sqlite_db.labels
	) TO '%s/labels.parquet' (
		FORMAT PARQUET,
		COMPRESSION 'zstd'
	)
	`, escapedLabelsDir)); err != nil {
		return nil, fmt.Errorf("export labels: %w", err)
	}

	// 6. Export sources
	sourcesDir := filepath.Join(staging.root, "sources")
	escapedSourcesDir := strings.ReplaceAll(sourcesDir, "'", "''")
	if err := runExport("sources", fmt.Sprintf(`
	COPY (
		SELECT
			id,
			identifier as account_email,
			COALESCE(TRY_CAST(source_type AS VARCHAR), 'gmail') as source_type
		FROM sqlite_db.sources
	) TO '%s/sources.parquet' (
		FORMAT PARQUET,
		COMPRESSION 'zstd'
	)
	`, escapedSourcesDir)); err != nil {
		return nil, fmt.Errorf("export sources: %w", err)
	}

	// 7. Export conversations (for Gmail thread IDs)
	conversationsDir := filepath.Join(staging.root, tableConversations)
	escapedConversationsDir := strings.ReplaceAll(conversationsDir, "'", "''")
	if err := runExport(tableConversations, fmt.Sprintf(`
	COPY (
		SELECT
			id,
			COALESCE(TRY_CAST(source_conversation_id AS VARCHAR), '') as source_conversation_id,
			COALESCE(TRY_CAST(title AS VARCHAR), '') as title,
			COALESCE(TRY_CAST(conversation_type AS VARCHAR), 'email') as conversation_type
		FROM sqlite_db.conversations c
		WHERE EXISTS (
			SELECT 1 FROM sqlite_db.messages m
			WHERE m.conversation_id = c.id
			  AND `+exportableMessageWhere("m")+`
			  AND TRY_CAST(m.id AS BIGINT) <= %d
		)
	) TO '%s/conversations.parquet' (
		FORMAT PARQUET,
		COMPRESSION 'zstd'
	)
	`, maxID, escapedConversationsDir)); err != nil {
		return nil, fmt.Errorf("export conversations: %w", err)
	}

	if buildCacheBeforeMessagesExportHook != nil {
		if err := buildCacheBeforeMessagesExportHook(); err != nil {
			return nil, err
		}
	}

	// 8. Export messages (partitioned by year) into staging.
	messagesDir := filepath.Join(staging.root, tableMessages)
	escapedMessagesDir := strings.ReplaceAll(messagesDir, "'", "''")

	if err := runExport(tableMessages, fmt.Sprintf(`
	COPY (
		SELECT
			m.id,
			m.source_id,
			m.source_message_id,
			m.conversation_id,
			CASE WHEN m.subject IS NULL THEN NULL ELSE COALESCE(TRY_CAST(m.subject AS VARCHAR), '') END as subject,
			CASE WHEN m.snippet IS NULL THEN NULL ELSE COALESCE(TRY_CAST(m.snippet AS VARCHAR), '') END as snippet,
			m.sent_at,
			m.size_estimate,
			m.has_attachments,
			COALESCE(TRY_CAST(m.attachment_count AS INTEGER), 0) as attachment_count,
			m.deleted_from_source_at,
			m.sender_id,
			COALESCE(TRY_CAST(m.message_type AS VARCHAR), '') as message_type,
			CAST(EXTRACT(YEAR FROM m.sent_at) AS INTEGER) as year,
			CAST(EXTRACT(MONTH FROM m.sent_at) AS INTEGER) as month
		FROM sqlite_db.messages m
		WHERE `+exportableMessageWhere("m")+`%s
	) TO '%s' (
		FORMAT PARQUET,
		PARTITION_BY (year),
		OVERWRITE_OR_IGNORE,
		COMPRESSION 'zstd'
	)
	`, idFilter, escapedMessagesDir)); err != nil {
		return nil, fmt.Errorf("export messages: %w", err)
	}

	// An archive with no exportable messages produces no partitioned Parquet
	// at all (COPY ... PARTITION_BY writes nothing for zero rows), which
	// would make every read_parquet over the messages glob error on a
	// running daemon — e.g. right after removing the last account. Write one
	// empty, schema-compatible shard so queries return zero rows instead.
	// Only the cleared-directory builds need this; an incremental no-op
	// leaves the previous shards in place.
	if expectedTotalCount == 0 && replaceAll {
		emptyShardDir := filepath.Join(messagesDir, "year=0")
		if err := os.MkdirAll(emptyShardDir, 0755); err != nil {
			return nil, fmt.Errorf("create empty messages shard dir: %w", err)
		}
		escapedEmptyShard := strings.ReplaceAll(
			filepath.Join(emptyShardDir, "empty.parquet"), "'", "''")
		// Same column list as the partitioned export minus the year
		// partition column, which hive_partitioning derives from the path.
		if _, err := exportDB.Exec(fmt.Sprintf(`
		COPY (
			SELECT
				m.id,
				m.source_id,
				m.source_message_id,
				m.conversation_id,
				CASE WHEN m.subject IS NULL THEN NULL ELSE COALESCE(TRY_CAST(m.subject AS VARCHAR), '') END as subject,
				CASE WHEN m.snippet IS NULL THEN NULL ELSE COALESCE(TRY_CAST(m.snippet AS VARCHAR), '') END as snippet,
				m.sent_at,
				m.size_estimate,
				m.has_attachments,
				COALESCE(TRY_CAST(m.attachment_count AS INTEGER), 0) as attachment_count,
				m.deleted_from_source_at,
				m.sender_id,
				COALESCE(TRY_CAST(m.message_type AS VARCHAR), '') as message_type,
				CAST(EXTRACT(MONTH FROM m.sent_at) AS INTEGER) as month
			FROM sqlite_db.messages m
			WHERE 1 = 0
		) TO '%s' (FORMAT PARQUET, COMPRESSION 'zstd')
		`, escapedEmptyShard)); err != nil {
			return nil, fmt.Errorf("export empty messages shard: %w", err)
		}
	}

	fmt.Printf("  %-25s %s\n", "Total:", time.Since(buildStart).Round(time.Millisecond))

	stagedCount, err := countStagedMessages(exportDB, messagesDir, replaceAll)
	if err != nil {
		return nil, err
	}
	if stagedCount != expectedBatchCount {
		return nil, fmt.Errorf("staged message row count %d does not match SQLite snapshot count %d; retry",
			stagedCount, expectedBatchCount)
	}
	if err := validateStagedReplacementDatasets(exportDB, staging.root, replaceAll); err != nil {
		return nil, err
	}
	if err := sourceSnapshot.Close(); err != nil {
		return nil, fmt.Errorf("close SQLite cache snapshot: %w", err)
	}
	if buildCacheBeforeStateWriteHook != nil {
		buildCacheBeforeStateWriteHook()
	}
	if hasSyncRunsTable > 0 {
		checkDB, openErr := sql.Open("sqlite3", dbPath+"?mode=ro")
		if openErr != nil {
			return nil, fmt.Errorf("reopen sqlite for cache consistency check: %w", openErr)
		}
		currentCounters, counterErr := readCacheSyncCounters(checkDB)
		closeErr := checkDB.Close()
		if counterErr != nil {
			return nil, fmt.Errorf("recheck cache sync counters: %w", counterErr)
		}
		if closeErr != nil {
			return nil, fmt.Errorf("close sqlite after cache consistency check: %w", closeErr)
		}
		if currentCounters != syncCounters {
			return nil, fmt.Errorf(
				"sync counters changed during cache export (additions %d→%d, updates %d→%d, failed runs count %d→%d, id sum %d→%d); retry",
				syncCounters.additions, currentCounters.additions,
				syncCounters.updates, currentCounters.updates,
				syncCounters.failedRunCount, currentCounters.failedRunCount,
				syncCounters.failedRunIDSum, currentCounters.failedRunIDSum,
			)
		}
	}

	// Save sync state using the pre-export watermark so any deletion
	// that occurs during or after the build is detected as stale.
	state := syncState{
		LastMessageID:          maxID,
		LastSyncAt:             cacheWatermark,
		SchemaVersion:          cacheSchemaVersion,
		LastCompletedSyncRunID: lastCompletedSyncRunID,
		LastCacheAdditionCount: syncCounters.additions,
		LastCacheUpdateCount:   syncCounters.updates,
		LastFailedSyncRunCount: syncCounters.failedRunCount,
		LastFailedSyncRunIDSum: syncCounters.failedRunIDSum,
	}
	stateData, err := json.Marshal(state)
	if err != nil {
		return nil, fmt.Errorf("marshal sync state: %w", err)
	}
	if err := publishCache(staging, analyticsDir, replaceAll, stateData); err != nil {
		return nil, err
	}

	return &buildResult{
		ExportedCount: expectedTotalCount,
		MaxMessageID:  maxID,
		OutputDir:     analyticsDir,
	}, nil
}

func countStagedMessages(db sqlRowQuerier, messagesDir string, requireShard bool) (int64, error) {
	files, err := filepath.Glob(filepath.Join(messagesDir, "*", "*.parquet"))
	if err != nil {
		return 0, fmt.Errorf("list staged message shards: %w", err)
	}
	if len(files) == 0 {
		if requireShard {
			return 0, errors.New("staged messages contain no Parquet shards")
		}
		return 0, nil
	}
	escaped := strings.ReplaceAll(filepath.Join(messagesDir, "**", "*.parquet"), "'", "''")
	var count int64
	if err := db.QueryRow(fmt.Sprintf(
		"SELECT COUNT(*) FROM read_parquet('%s', hive_partitioning=true)", escaped,
	)).Scan(&count); err != nil {
		return 0, fmt.Errorf("read staged message shards: %w", err)
	}
	return count, nil
}

func validateStagedReplacementDatasets(db sqlRowQuerier, stagingDir string, replaceAll bool) error {
	for _, dataset := range query.RequiredParquetDirs {
		// countStagedMessages already verifies and reads every message shard.
		if dataset == tableMessages || !replacesCacheDataset(dataset, replaceAll) {
			continue
		}
		pattern := filepath.Join(stagingDir, dataset, "*.parquet")
		escaped := strings.ReplaceAll(pattern, "'", "''")
		var ignored int64
		if err := db.QueryRow(fmt.Sprintf(
			"SELECT COUNT(*) FROM read_parquet('%s')", escaped,
		)).Scan(&ignored); err != nil {
			return fmt.Errorf("validate staged cache dataset %s: %w", dataset, err)
		}
	}
	return nil
}

var cacheStatsCmd = &cobra.Command{
	Use:     "cache-stats",
	Aliases: []string{"parquet-stats"}, // Backward compatibility
	Short:   "Show statistics about the analytics cache",
	Long: `Display statistics about the analytics cache, including row counts and file sizes.

Total messages counts the analytics-cache population: it includes messages
deleted from their source account (the archive retains them) but excludes
dedup-hidden rows and messages without a timestamp. This differs from the
'stats' command, which reports active messages from the SQLite system of
record.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		st, _, err := OpenHTTPStore(cmd.Context())
		if err != nil {
			return err
		}
		defer func() { _ = st.Close() }()

		stats, err := st.GetCLICacheStats(cmd.Context())
		if err != nil {
			return fmt.Errorf("cache stats: %w", err)
		}
		return printCacheStats(cmd.OutOrStdout(), cmd.ErrOrStderr(), stats)
	},
}

func printCacheStats(out io.Writer, errOut io.Writer, stats *cacheops.CacheStats) error {
	if stats == nil {
		stats = &cacheops.CacheStats{Status: cacheops.StatusNoCacheFiles}
	}
	for _, warning := range stats.Warnings {
		if err := writeCacheStatsLine(errOut, "Warning: %s\n", warning); err != nil {
			return err
		}
	}

	switch stats.Status {
	case cacheops.StatusNoCacheFiles:
		if err := writeCacheStatsLine(out, "No cache files found.\n"); err != nil {
			return err
		}
		if err := writeCacheStatsLine(out, "Run 'msgvault build-cache' to create them.\n"); err != nil {
			return err
		}
	case cacheops.StatusNoCacheData:
		if err := writeCacheStatsLine(out, "No cache data found (directory exists but contains no data).\n"); err != nil {
			return err
		}
		if err := writeCacheStatsLine(out, "Run 'msgvault build-cache' to populate it.\n"); err != nil {
			return err
		}
	case cacheops.StatusInterrupted:
		if err := writeCacheStatsLine(out, "Analytics cache publication was interrupted.\n"); err != nil {
			return err
		}
		if err := writeCacheStatsLine(out, "Run 'msgvault build-cache' to repair it.\n"); err != nil {
			return err
		}
	case cacheops.StatusReady:
		if err := writeCacheStatsLine(out, "Cache Statistics:\n"); err != nil {
			return err
		}
		if err := writeCacheStatsLine(out, "  Total messages:    %d (includes messages deleted from source)\n", stats.TotalMessages); err != nil {
			return err
		}
		if err := writeCacheStatsLine(out, "  Accounts:          %d\n", stats.Sources); err != nil {
			return err
		}
		if err := writeCacheStatsLine(out, "  Unique senders:    %d\n", stats.UniqueSenders); err != nil {
			return err
		}
		if err := writeCacheStatsLine(out, "  Unique domains:    %d\n", stats.UniqueDomains); err != nil {
			return err
		}
		if stats.MinYear != nil && stats.MaxYear != nil {
			if err := writeCacheStatsLine(out, "  Year range:        %d-%d\n", *stats.MinYear, *stats.MaxYear); err != nil {
				return err
			}
		}
		if err := writeCacheStatsLine(out, "  Total size:        %.1f MB\n", float64(stats.TotalSizeBytes)/1024/1024); err != nil {
			return err
		}
		if err := writeCacheStatsLine(out, "  Attachment size:   %.1f MB\n", float64(stats.AttachmentSizeBytes)/1024/1024); err != nil {
			return err
		}
		if stats.LastSyncAt != nil {
			if err := writeCacheStatsLine(out, "  Last sync:         %s\n", stats.LastSyncAt.Format("2006-01-02 15:04:05")); err != nil {
				return err
			}
		}
		if stats.LastMessageID != nil {
			if err := writeCacheStatsLine(out, "  Last message ID:   %d\n", *stats.LastMessageID); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("unknown cache stats status %q", stats.Status)
	}

	return nil
}

func writeCacheStatsLine(w io.Writer, format string, args ...any) error {
	_, err := fmt.Fprintf(w, format, args...)
	if err != nil {
		return fmt.Errorf("write cache stats: %w", err)
	}
	return nil
}

// cacheSourceSnapshot keeps every source read in one SQLite transaction. On
// platforms with sqlite_scanner, the DuckDB transaction owns that SQLite
// transaction directly. The fallback exports every table from one go-sqlite3
// transaction before DuckDB reads the resulting static CSV files.
type cacheSourceSnapshot struct {
	duckDB   *sql.DB
	duckTx   *sql.Tx
	sqliteDB *sql.DB
	sqliteTx *sql.Tx
	tmpDir   string
}

func openCacheSourceSnapshot(duckDB *sql.DB, dbPath string) (*cacheSourceSnapshot, error) {
	if runtime.GOOS != "windows" {
		// Try sqlite_scanner; fall back to CSV when the extension is unavailable
		// (for example in an air-gapped installation). Parallel scanner workers
		// open independent SQLite connections, so disable only that parallelism
		// to keep every scan inside the attached database's read transaction.
		if _, err := duckDB.Exec("INSTALL sqlite; LOAD sqlite;"); err != nil {
			fmt.Fprintf(os.Stderr, "  sqlite_scanner unavailable, using CSV fallback: %v\n", err)
		} else if _, err := duckDB.Exec("SET sqlite_disable_multithreaded_scans = true"); err != nil {
			fmt.Fprintf(os.Stderr, "  sqlite snapshot scans unavailable, using CSV fallback: %v\n", err)
		} else {
			escapedPath := strings.ReplaceAll(dbPath, "'", "''")
			if _, err := duckDB.Exec(fmt.Sprintf("ATTACH '%s' AS sqlite_db (TYPE sqlite, READ_ONLY)", escapedPath)); err != nil {
				fmt.Fprintf(os.Stderr, "  sqlite attach failed, using CSV fallback: %v\n", err)
			} else {
				duckTx, err := duckDB.BeginTx(context.Background(), nil)
				if err != nil {
					return nil, fmt.Errorf("begin DuckDB cache snapshot: %w", err)
				}
				return &cacheSourceSnapshot{duckDB: duckDB, duckTx: duckTx}, nil
			}
		}
	}

	// Prefer the database's parent directory for temporary CSV files, then
	// fall back through the configured temporary locations.
	tmpDir, err := config.MkTempDir(".cache-tmp-*", filepath.Dir(dbPath))
	if err != nil {
		return nil, err
	}

	sqliteDB, err := sql.Open("sqlite3", dbPath+"?mode=ro")
	if err != nil {
		_ = os.RemoveAll(tmpDir)
		return nil, fmt.Errorf("open sqlite for CSV export: %w", err)
	}
	sqliteTx, err := sqliteDB.BeginTx(context.Background(), nil)
	if err != nil {
		_ = sqliteDB.Close()
		_ = os.RemoveAll(tmpDir)
		return nil, fmt.Errorf("begin SQLite cache snapshot: %w", err)
	}
	return &cacheSourceSnapshot{
		duckDB: duckDB, sqliteDB: sqliteDB, sqliteTx: sqliteTx, tmpDir: tmpDir,
	}, nil
}

// QueryRow executes source metadata SQL inside the same snapshot used by the
// table exports. sqlite_query preserves native SQLite query planning and
// indexes when sqlite_scanner is active.
func (s *cacheSourceSnapshot) QueryRow(query string, args ...any) *sql.Row {
	if s.sqliteTx != nil {
		return s.sqliteTx.QueryRow(query, args...)
	}
	escapedQuery := strings.ReplaceAll(query, "'", "''")
	duckQuery := fmt.Sprintf("SELECT * FROM sqlite_query('sqlite_db', '%s'", escapedQuery)
	if len(args) > 0 {
		placeholders := strings.TrimSuffix(strings.Repeat("?,", len(args)), ",")
		duckQuery += ", params=row(" + placeholders + ")"
	}
	duckQuery += ")"
	return s.duckTx.QueryRow(duckQuery, args...)
}

func (s *cacheSourceSnapshot) DuckDB() sqlRunner {
	if s.duckTx != nil {
		return s.duckTx
	}
	return s.duckDB
}

// Prepare materializes the CSV fallback after metadata has pinned the SQLite
// read transaction. sqlite_scanner needs no preparation because DuckDB's
// transaction reads the attached database directly.
func (s *cacheSourceSnapshot) Prepare() error {
	if s.sqliteTx == nil {
		return nil
	}

	// Tables and the SELECT queries to export them.
	// Column lists match what the COPY-to-Parquet queries expect.
	tables := []struct {
		name          string
		query         string
		typeOverrides string // DuckDB types parameter for read_csv_auto (empty = infer all)
	}{
		// deleted_at is exported so the main COPY query can apply the
		// `deleted_at IS NULL` filter on this path the same way it does
		// on the sqlite_scanner path; otherwise DuckDB binds against a
		// CSV view that lacks the column and the export fails on Windows.
		{tableMessages, "SELECT id, source_id, source_message_id, conversation_id, subject, snippet, sent_at, size_estimate, has_attachments, attachment_count, deleted_from_source_at, deleted_at, sender_id, message_type FROM messages WHERE sent_at IS NOT NULL",
			"types={'sent_at': 'TIMESTAMP', 'deleted_from_source_at': 'TIMESTAMP', 'deleted_at': 'TIMESTAMP'}"},
		{"message_recipients", "SELECT message_id, participant_id, recipient_type, display_name FROM message_recipients", ""},
		{"message_labels", "SELECT message_id, label_id FROM message_labels", ""},
		{tableAttachments, "SELECT message_id, size, filename FROM attachments", ""},
		{tableParticipants, "SELECT id, email_address, domain, display_name, phone_number FROM participants", ""},
		{tableLabels, "SELECT id, name FROM labels", ""},
		{"sources", "SELECT id, identifier, source_type FROM sources", ""},
		{tableConversations, "SELECT id, source_conversation_id, title, COALESCE(conversation_type, 'email_thread') AS conversation_type FROM conversations", ""},
	}

	for _, t := range tables {
		csvPath := filepath.Join(s.tmpDir, t.name+".csv")
		if err := exportToCSV(s.sqliteTx, t.query, csvPath); err != nil {
			return fmt.Errorf("export %s to CSV: %w", t.name, err)
		}
	}
	if err := s.closeSQLite(); err != nil {
		return fmt.Errorf("close SQLite cache snapshot after CSV export: %w", err)
	}

	// Create sqlite_db schema with views pointing to CSV files.
	// This lets the existing COPY queries reference sqlite_db.tablename unchanged.
	if _, err := s.duckDB.Exec("CREATE SCHEMA sqlite_db"); err != nil {
		return fmt.Errorf("create sqlite_db schema: %w", err)
	}
	for _, t := range tables {
		csvPath := filepath.Join(s.tmpDir, t.name+".csv")
		// DuckDB handles both forward and backslash paths, but normalize to forward.
		escaped := strings.ReplaceAll(csvPath, "\\", "/")
		escaped = strings.ReplaceAll(escaped, "'", "''")
		csvOpts := "header=true, nullstr='\\N'"
		if t.typeOverrides != "" {
			csvOpts += ", " + t.typeOverrides
		}
		viewSQL := fmt.Sprintf(
			`CREATE VIEW sqlite_db."%s" AS SELECT * FROM read_csv_auto('%s', %s)`,
			t.name, escaped, csvOpts,
		)
		if _, err := s.duckDB.Exec(viewSQL); err != nil {
			return fmt.Errorf("create view sqlite_db.%s: %w", t.name, err)
		}
	}

	return nil
}

func (s *cacheSourceSnapshot) closeSQLite() error {
	var result error
	if s.sqliteTx != nil {
		if err := s.sqliteTx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
			result = errors.Join(result, fmt.Errorf("rollback SQLite snapshot: %w", err))
		}
		s.sqliteTx = nil
	}
	if s.sqliteDB != nil {
		if err := s.sqliteDB.Close(); err != nil {
			result = errors.Join(result, fmt.Errorf("close SQLite snapshot database: %w", err))
		}
		s.sqliteDB = nil
	}
	return result
}

func (s *cacheSourceSnapshot) Close() error {
	var result error
	if s.duckTx != nil {
		if err := s.duckTx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
			result = errors.Join(result, fmt.Errorf("rollback DuckDB cache snapshot: %w", err))
		}
		s.duckTx = nil
	}
	result = errors.Join(result, s.closeSQLite())
	if s.tmpDir != "" {
		_ = os.RemoveAll(s.tmpDir)
		s.tmpDir = ""
	}
	return result
}

// csvNullStr is written for NULL values in CSV exports so DuckDB can
// distinguish NULL from empty string via the nullstr option.
const csvNullStr = `\N`

// exportToCSV exports the results of a SQL query to a CSV file.
// NULL values are written as \N (PostgreSQL convention).
func exportToCSV(db sqlRunner, query string, dest string) error {
	rows, err := db.Query(query)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()

	f, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()

	w := csv.NewWriter(f)

	cols, err := rows.Columns()
	if err != nil {
		return err
	}
	if err := w.Write(cols); err != nil {
		return err
	}

	values := make([]sql.NullString, len(cols))
	ptrs := make([]any, len(cols))
	for i := range values {
		ptrs[i] = &values[i]
	}

	for rows.Next() {
		if err := rows.Scan(ptrs...); err != nil {
			return err
		}
		record := make([]string, len(cols))
		for i, v := range values {
			if v.Valid {
				record[i] = v.String
			} else {
				record[i] = csvNullStr
			}
		}
		if err := w.Write(record); err != nil {
			return err
		}
	}

	w.Flush()
	if err := w.Error(); err != nil {
		return err
	}
	return rows.Err()
}

// rebuildCacheAfterWrite refreshes the SQLite-backed analytics cache after a
// write operation. Cache maintenance is part of the operation result: SQLite
// remains authoritative, but callers must surface any refresh failure.
func rebuildCacheAfterWrite(dbPath string) error {
	if store.IsPostgresURL(dbPath) {
		return nil
	}
	analyticsDir := cfg.AnalyticsDir()
	staleness := cacheNeedsBuild(dbPath, analyticsDir)
	if !staleness.NeedsBuild {
		return nil
	}
	result, err := buildCacheAuto(dbPath, analyticsDir)
	if err != nil {
		return fmt.Errorf("refresh analytics cache: %w", err)
	}
	if !result.Skipped {
		logger.Info("cache rebuilt", "exported", result.ExportedCount)
	}
	return nil
}

// buildCacheSubprocess runs `msgvault build-cache` as a child process
// instead of calling buildCache in-process.
//
// The daemon (`serve`) holds a long-lived go-sqlite3 connection to the
// SQLite database for its entire lifetime. buildCache, in turn, uses
// DuckDB's sqlite_scanner extension, which statically links its OWN copy
// of the SQLite library and ATTACHes the same database file. Two
// independent SQLite library instances in one process do not share the
// unix VFS's in-process POSIX advisory-lock and WAL-index bookkeeping, so
// when DuckDB's copy opens/closes the WAL it can drop the daemon's
// advisory locks and leave the on-disk -wal/-shm inconsistent with the
// daemon's in-memory WAL-index. After that, every newly-opened go-sqlite3
// connection in the process fails with "disk I/O error: no such file or
// directory" until the daemon restarts (see issue #379).
//
// Running build-cache in a fresh process keeps DuckDB's SQLite copy out of
// the daemon's address space entirely, so the daemon's own connections are
// never affected.
//
// Global flags that affect config resolution (--config, --home, --local)
// are forwarded so the child loads identical configuration. --no-log-file
// keeps the child from writing to the daemon's log file; its output is
// captured and surfaced on failure instead.
func buildCacheSubprocess(ctx context.Context, fullRebuild, auto bool) error {
	// Serialize with each other so parallel per-account syncs in the
	// daemon don't spawn concurrent cache builds racing on shared files.
	buildCacheMu.Lock()
	defer buildCacheMu.Unlock()

	cmd, err := newBuildCacheSubprocessCommand(ctx, fullRebuild, auto)
	if err != nil {
		return err
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("build-cache subprocess: %w; output: %s",
			err, strings.TrimSpace(string(out)))
	}
	return nil
}

var runBuildCacheSubprocess = buildCacheSubprocess

func buildCacheSubprocessStream(
	ctx context.Context,
	fullRebuild, auto bool,
	emit func(api.CLICacheBuildEvent) error,
) error {
	buildCacheMu.Lock()
	defer buildCacheMu.Unlock()

	cmd, err := newBuildCacheSubprocessCommand(ctx, fullRebuild, auto)
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("open build-cache subprocess stdout: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("open build-cache subprocess stderr: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start build-cache subprocess: %w", err)
	}

	var emitMu sync.Mutex
	emitLocked := func(event api.CLICacheBuildEvent) error {
		if emit == nil {
			return nil
		}
		emitMu.Lock()
		defer emitMu.Unlock()
		return emit(event)
	}

	streamErrCh := make(chan error, 2)
	go func() {
		streamErrCh <- streamBuildCachePipe(stdout, cliStreamStdout, emitLocked)
	}()
	go func() {
		streamErrCh <- streamBuildCachePipe(stderr, cliStreamStderr, emitLocked)
	}()

	firstStreamErr := <-streamErrCh
	secondStreamErr := <-streamErrCh
	waitErr := cmd.Wait()
	if firstStreamErr != nil {
		return firstStreamErr
	}
	if secondStreamErr != nil {
		return secondStreamErr
	}
	if waitErr != nil {
		return fmt.Errorf("build-cache subprocess: %w", waitErr)
	}
	return nil
}

func newBuildCacheSubprocessCommand(ctx context.Context, fullRebuild, auto bool) (*exec.Cmd, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("locate msgvault executable: %w", err)
	}

	args := globalConfigFlagArgs()
	args = append(args, "--no-log-file", "build-cache")
	if fullRebuild {
		args = append(args, "--full-rebuild")
	}
	if auto {
		args = append(args, "--auto")
	}

	// exe is this binary (os.Executable) and args are our own fixed subcommand
	// plus operator-controlled config flags, not untrusted input.
	cmd := exec.CommandContext(ctx, exe, args...) //nolint:gosec // exe is os.Executable; args are internally constructed
	cmd.Env = buildCacheDaemonChildEnv(os.Environ(), os.Getpid())
	return cmd, nil
}

func streamBuildCachePipe(
	r io.Reader,
	eventType string,
	emit func(api.CLICacheBuildEvent) error,
) error {
	buf := make([]byte, 32*1024)
	var firstErr error
	for {
		n, err := r.Read(buf)
		if n > 0 && firstErr == nil {
			if emitErr := emit(api.CLICacheBuildEvent{
				Type: eventType,
				Data: string(buf[:n]),
			}); emitErr != nil {
				firstErr = emitErr
			}
		}
		if errors.Is(err, io.EOF) {
			return firstErr
		}
		if err != nil {
			if firstErr != nil {
				return firstErr
			}
			return fmt.Errorf("read build-cache subprocess %s: %w", eventType, err)
		}
	}
}

func buildCacheDaemonChildEnv(base []string, parentPID int) []string {
	out := make([]string, 0, len(base)+1)
	prefix := buildCacheDaemonSubprocessEnv + "="
	value := prefix + strconv.Itoa(parentPID)
	replaced := false
	for _, entry := range base {
		if strings.HasPrefix(entry, prefix) {
			if !replaced {
				out = append(out, value)
				replaced = true
			}
			continue
		}
		out = append(out, entry)
	}
	if !replaced {
		out = append(out, value)
	}
	return out
}

// globalConfigFlagArgs reconstructs the persistent flags that affect
// configuration resolution so a child process loads the same config as
// the running one.
func globalConfigFlagArgs() []string {
	var args []string
	if cfgFile != "" {
		args = append(args, "--config", cfgFile)
	}
	if homeDir != "" {
		args = append(args, "--home", homeDir)
	}
	if useLocal {
		args = append(args, "--local")
	}
	// Forward the logging flags so an explicit level survives into subprocesses.
	// The daemon CLI subprocess otherwise quiets to WARN, defeating a user's
	// explicit --log-level/--verbose/--log-sql request.
	if logLevel != "" {
		args = append(args, "--log-level", logLevel)
	}
	if verbose {
		args = append(args, "--verbose")
	}
	if logSQL {
		args = append(args, "--log-sql")
	}
	if logSQLSlow != 0 {
		args = append(args, "--log-sql-slow-ms", strconv.FormatInt(logSQLSlow, 10))
	}
	return args
}

// rebuildCacheAfterScheduledSync rebuilds the Parquet cache if it is stale
// after a scheduled sync. The cache is SQLite-only, so it is skipped on
// PostgreSQL DSNs. The build runs in a subprocess (see buildCacheSubprocess)
// to keep DuckDB's bundled SQLite library out of a long-lived daemon's
// address space (issue #379).
func rebuildCacheAfterScheduledSync(ctx context.Context, identifier string) error {
	dbPath := cfg.DatabaseDSN()
	if store.IsPostgresURL(dbPath) {
		return nil
	}
	analyticsDir := cfg.AnalyticsDir()
	staleness := cacheNeedsBuild(dbPath, analyticsDir)
	if !staleness.NeedsBuild {
		return nil
	}
	logger.Info("rebuilding cache after sync",
		"identifier", identifier, "reason", staleness.Reason,
		"full_rebuild", staleness.FullRebuild)
	if err := runBuildCacheSubprocess(ctx, staleness.FullRebuild, true); err != nil {
		logger.Error("cache build failed", "error", err)
		return fmt.Errorf("refresh analytics cache: %w", err)
	}
	logger.Info("cache build completed")
	return nil
}

func init() {
	rootCmd.AddCommand(buildCacheCmd)
	rootCmd.AddCommand(cacheStatsCmd)
	buildCacheCmd.Flags().BoolVar(&fullRebuild, "full-rebuild", false, "Rebuild all cache files from scratch")
	// --auto marks a daemon-spawned, staleness-derived build whose rebuild
	// decision is re-evaluated under the build lock; explicit user builds
	// stay unconditional. Internal, so hidden.
	buildCacheCmd.Flags().BoolVar(&buildCacheAutoFlag, "auto", false, "Internal: staleness-derived build; re-evaluated under the build lock")
	_ = buildCacheCmd.Flags().MarkHidden("auto")
}
