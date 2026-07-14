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
	_ "github.com/mattn/go-sqlite3"    // SQLite driver (database/sql)
	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/cacheops"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/store"
)

var fullRebuild bool

const buildCacheDaemonSubprocessEnv = "MSGVAULT_DAEMON_BUILD_CACHE_PARENT_PID"

// buildCacheMu serializes concurrent buildCache calls. The scheduler may
// trigger syncs for multiple accounts in parallel, each of which calls
// buildCache on completion. Without this lock, concurrent writes to shared
// files (_last_sync.json, parquet directories) can corrupt the cache.
var buildCacheMu sync.Mutex

// buildCacheAfterSnapshotHook is a deterministic test seam for writes that
// race with cache construction after its source watermark is captured.
var buildCacheAfterSnapshotHook func()

// buildCacheBeforeStateWriteHook is a deterministic test seam for source
// mutations that finish after table COPY operations but before cache state is
// persisted.
var buildCacheBeforeStateWriteHook func()

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
type syncState struct {
	LastMessageID          int64     `json:"last_message_id"`
	LastSyncAt             time.Time `json:"last_sync_at"`
	SchemaVersion          int       `json:"schema_version,omitempty"`
	LastCompletedSyncRunID int64     `json:"last_completed_sync_run_id,omitempty"`
	LastCacheAdditionCount int64     `json:"last_cache_addition_count,omitempty"`
	LastCacheUpdateCount   int64     `json:"last_cache_update_count,omitempty"`
	LastFailedSyncRunCount int64     `json:"last_failed_sync_run_count,omitempty"`
	LastFailedSyncRunIDSum int64     `json:"last_failed_sync_run_id_sum,omitempty"`
}

type cacheSyncCounters struct {
	additions      int64
	updates        int64
	failedRunCount int64
	failedRunIDSum int64
}

func readCacheSyncCounters(db *sql.DB) (cacheSyncCounters, error) {
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
			return runBuildCacheLocal(fullRebuild)
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

func runBuildCacheLocal(fullRebuild bool) error {
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

	// Ensure schema is up to date before building cache.
	// Legacy databases may be missing columns (e.g. attachment_count,
	// sender_id, message_type, phone_number) that the export queries
	// reference. Running migrations first adds them.
	s, err := store.Open(dbPath)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	if err := s.InitSchema(); err != nil {
		_ = s.Close()
		return fmt.Errorf("init schema: %w", err)
	}
	if err := runStartupMigrations(s); err != nil {
		_ = s.Close()
		return fmt.Errorf("startup migrations: %w", err)
	}
	_ = s.Close()

	result, err := buildCache(dbPath, analyticsDir, fullRebuild)
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

func buildCache(dbPath, analyticsDir string, fullRebuild bool) (*buildResult, error) {
	buildCacheMu.Lock()
	defer buildCacheMu.Unlock()

	// Record the freshness boundary before reading any source metadata. A sync
	// or deletion that finishes after this instant may not be represented by
	// the bounded export and must invalidate the cache on the next check.
	cacheWatermark := time.Now().UTC().Truncate(time.Second)
	stateFile := filepath.Join(analyticsDir, "_last_sync.json")

	// Create output directory
	if err := os.MkdirAll(analyticsDir, 0755); err != nil {
		return nil, fmt.Errorf("create analytics dir: %w", err)
	}

	// Load sync state for incremental updates
	var lastMessageID int64
	var previousState syncState
	var hasPreviousState bool
	if !fullRebuild {
		if data, err := os.ReadFile(stateFile); err == nil {
			var state syncState
			if json.Unmarshal(data, &state) == nil {
				if state.SchemaVersion != cacheSchemaVersion {
					// Schema has changed — force a full rebuild.
					fmt.Printf("Cache schema version mismatch (have v%d, need v%d). Forcing full rebuild.\n",
						state.SchemaVersion, cacheSchemaVersion)
					fullRebuild = true
					lastMessageID = 0
				} else {
					previousState = state
					hasPreviousState = true
					lastMessageID = state.LastMessageID
				}
			}
		}
	}

	// Use direct SQLite to check for new messages (fast, uses indexes)
	// DuckDB's sqlite extension doesn't use SQLite indexes, so this query
	// would scan the entire table if we used DuckDB.
	sqliteDB, err := sql.Open("sqlite3", dbPath+"?mode=ro")
	if err != nil {
		return nil, fmt.Errorf("open sqlite for max id check: %w", err)
	}

	var maxMessageID sql.NullInt64
	var maxExportableMessageID sql.NullInt64
	var lastCompletedSyncRunID int64
	var syncCounters cacheSyncCounters
	// Use indexed query: id is PRIMARY KEY, sent_at has an index
	maxIDQuery := `SELECT MAX(id) FROM messages WHERE sent_at IS NOT NULL`
	if err := sqliteDB.QueryRow(maxIDQuery).Scan(&maxMessageID); err != nil {
		if closeErr := sqliteDB.Close(); closeErr != nil {
			return nil, fmt.Errorf("get max message id: %w; close sqlite: %w", err, closeErr)
		}
		return nil, fmt.Errorf("get max message id: %w", err)
	}
	maxID := int64(0)
	if maxMessageID.Valid {
		maxID = maxMessageID.Int64
	}
	maxExportableIDQuery := "SELECT MAX(id) FROM messages WHERE " + exportableMessageWhere("") + " AND id <= ?"
	if err := sqliteDB.QueryRow(maxExportableIDQuery, maxID).Scan(&maxExportableMessageID); err != nil {
		if closeErr := sqliteDB.Close(); closeErr != nil {
			return nil, fmt.Errorf("get max exportable message id: %w; close sqlite: %w", err, closeErr)
		}
		return nil, fmt.Errorf("get max exportable message id: %w", err)
	}
	var hasSyncRunsTable int
	if err := sqliteDB.QueryRow(`
		SELECT COUNT(*) FROM sqlite_master
		WHERE type = 'table' AND name = 'sync_runs'
	`).Scan(&hasSyncRunsTable); err != nil {
		if closeErr := sqliteDB.Close(); closeErr != nil {
			return nil, fmt.Errorf("check sync_runs table: %w; close sqlite: %w", err, closeErr)
		}
		return nil, fmt.Errorf("check sync_runs table: %w", err)
	}
	if hasSyncRunsTable > 0 {
		if err := sqliteDB.QueryRow(`
			SELECT COALESCE(MAX(id), 0) FROM sync_runs
			WHERE status = 'completed' AND completed_at IS NOT NULL
		`).Scan(&lastCompletedSyncRunID); err != nil {
			if closeErr := sqliteDB.Close(); closeErr != nil {
				return nil, fmt.Errorf("get last completed sync run id: %w; close sqlite: %w", err, closeErr)
			}
			return nil, fmt.Errorf("get last completed sync run id: %w", err)
		}
		if syncCounters, err = readCacheSyncCounters(sqliteDB); err != nil {
			if closeErr := sqliteDB.Close(); closeErr != nil {
				return nil, fmt.Errorf("get cache sync counters: %w; close sqlite: %w", err, closeErr)
			}
			return nil, fmt.Errorf("get cache sync counters: %w", err)
		}
	}
	if err := sqliteDB.Close(); err != nil {
		return nil, fmt.Errorf("close sqlite after metadata check: %w", err)
	}

	exportableMaxID := int64(0)
	if maxExportableMessageID.Valid {
		exportableMaxID = maxExportableMessageID.Int64
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

	// Check for missing required parquet tables independently of whether
	// new messages exist. A legacy cache might be missing tables (e.g.
	// conversations) regardless of message count. Force full rebuild to
	// avoid stale incr_*.parquet shards and ensure all tables are populated.
	// Gate on exportableMaxID > 0: when the DB has no email-analytics messages,
	// missing messages parquet is legitimate, not a sign of a broken cache.
	if !fullRebuild && exportableMaxID > 0 && missingRequiredParquet(analyticsDir) {
		fmt.Println("Backfilling missing cache tables (full rebuild)...")
		fullRebuild = true
		lastMessageID = 0
	}

	if maxID <= lastMessageID && !fullRebuild {
		return &buildResult{Skipped: true}, nil
	}
	if buildCacheAfterSnapshotHook != nil {
		buildCacheAfterSnapshotHook()
	}

	// Open DuckDB for the actual export
	db, err := sql.Open("duckdb", "")
	if err != nil {
		return nil, fmt.Errorf("open duckdb: %w", err)
	}
	defer func() { _ = db.Close() }()

	// Set up sqlite_db tables — either via DuckDB's sqlite extension (Linux/macOS)
	// or via CSV intermediate files (Windows, where sqlite_scanner is unavailable).
	cleanup, err := setupSQLiteSource(db, dbPath)
	if err != nil {
		return nil, err
	}
	defer cleanup()

	// On full rebuild, clear existing cache
	if fullRebuild {
		fmt.Println("Full rebuild: clearing existing cache...")
		for _, subdir := range query.RequiredParquetDirs {
			if err := os.RemoveAll(filepath.Join(analyticsDir, subdir)); err != nil {
				return nil, fmt.Errorf("clear existing cache: %w", err)
			}
		}
	}

	// Create subdirectories
	for _, subdir := range query.RequiredParquetDirs {
		if err := os.MkdirAll(filepath.Join(analyticsDir, subdir), 0755); err != nil {
			return nil, fmt.Errorf("create %s dir: %w", subdir, err)
		}
	}

	if fullRebuild {
		fmt.Println("Building analytics cache...")
	} else {
		fmt.Println("Updating analytics cache...")
	}
	buildStart := time.Now()

	// Build WHERE clause for incremental exports
	idFilter := fmt.Sprintf(" AND TRY_CAST(m.id AS BIGINT) <= %d", maxID)
	if !fullRebuild && lastMessageID > 0 {
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

	// Junction tables (message_recipients, message_labels, attachments) need
	// unique filenames per batch because Parquet files cannot be appended to —
	// DuckDB's COPY with APPEND silently overwrites a single file.
	// Using *.parquet glob in queries reads all batch files together.
	junctionFile := "data.parquet"
	if !fullRebuild && lastMessageID > 0 {
		junctionFile = fmt.Sprintf("incr_%d.parquet", lastMessageID)
	}

	// runExport executes a COPY query and prints timing info.
	runExport := func(label, query string) error {
		start := time.Now()
		fmt.Printf("  %-25s", label+"...")
		if _, err := db.Exec(query); err != nil {
			fmt.Println()
			return err
		}
		fmt.Printf(" done (%s)\n", time.Since(start).Round(time.Millisecond))
		return nil
	}

	// Export each table separately - this is MUCH faster than joining during export
	// because DuckDB can use SQLite indexes efficiently for simple queries

	// 1. Export messages (partitioned by year)
	messagesDir := filepath.Join(analyticsDir, tableMessages)
	escapedMessagesDir := strings.ReplaceAll(messagesDir, "'", "''")

	writeMode := "OVERWRITE_OR_IGNORE"
	if !fullRebuild && lastMessageID > 0 {
		writeMode = "APPEND"
	}

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
		%s,
		COMPRESSION 'zstd'
	)
	`, idFilter, escapedMessagesDir, writeMode)); err != nil {
		return nil, fmt.Errorf("export messages: %w", err)
	}

	// 2. Export message_recipients (large junction table)
	recipientsDir := filepath.Join(analyticsDir, "message_recipients")
	escapedRecipientsDir := strings.ReplaceAll(recipientsDir, "'", "''")
	recipientsFilter := ""
	if !fullRebuild && lastMessageID > 0 {
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

	// 3. Export message_labels (large junction table)
	messageLabelsDir := filepath.Join(analyticsDir, "message_labels")
	escapedMessageLabelsDir := strings.ReplaceAll(messageLabelsDir, "'", "''")
	messageLabelsFilter := ""
	if !fullRebuild && lastMessageID > 0 {
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

	// 4. Export attachments
	attachmentsDir := filepath.Join(analyticsDir, tableAttachments)
	escapedAttachmentsDir := strings.ReplaceAll(attachmentsDir, "'", "''")
	attachmentsFilter := ""
	if !fullRebuild && lastMessageID > 0 {
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

	// 5. Export participants
	participantsDir := filepath.Join(analyticsDir, tableParticipants)
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

	// 6. Export labels
	labelsDir := filepath.Join(analyticsDir, tableLabels)
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

	// 7. Export sources
	sourcesDir := filepath.Join(analyticsDir, "sources")
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

	// 8. Export conversations (for Gmail thread IDs)
	conversationsDir := filepath.Join(analyticsDir, tableConversations)
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

	fmt.Printf("  %-25s %s\n", "Total:", time.Since(buildStart).Round(time.Millisecond))

	// Count exported messages and verify Parquet files actually exist.
	// Calendar events are intentionally excluded from the email analytics
	// export, so a database with only calendar_event rows has maxID > 0 but no
	// message Parquet. Only require verifiable message rows when at least one
	// exportable non-calendar message exists.
	var exportedCount int64
	if exportableMaxID > 0 {
		countSQL := fmt.Sprintf("SELECT COUNT(*) FROM read_parquet('%s/**/*.parquet', hive_partitioning=true)", escapedMessagesDir)
		if err := db.QueryRow(countSQL).Scan(&exportedCount); err != nil {
			return nil, fmt.Errorf("verify exported parquet rows: %w; cache state not updated", err)
		}
		if exportedCount == 0 {
			return nil, fmt.Errorf("export produced 0 parquet rows from exportable messages through id %d; cache state not updated", exportableMaxID)
		}
	}
	if buildCacheBeforeStateWriteHook != nil {
		buildCacheBeforeStateWriteHook()
	}
	discardAttempt := func(cause error) error {
		closeErr := db.Close()
		removeErr := os.RemoveAll(analyticsDir)
		return errors.Join(cause, closeErr, removeErr)
	}
	if hasSyncRunsTable > 0 {
		checkDB, openErr := sql.Open("sqlite3", dbPath+"?mode=ro")
		if openErr != nil {
			return nil, discardAttempt(fmt.Errorf("reopen sqlite for cache consistency check: %w", openErr))
		}
		currentCounters, counterErr := readCacheSyncCounters(checkDB)
		closeErr := checkDB.Close()
		if counterErr != nil {
			return nil, discardAttempt(fmt.Errorf("recheck cache sync counters: %w", counterErr))
		}
		if closeErr != nil {
			return nil, discardAttempt(fmt.Errorf("close sqlite after cache consistency check: %w", closeErr))
		}
		if currentCounters != syncCounters {
			return nil, discardAttempt(fmt.Errorf(
				"sync counters changed during cache export (additions %d→%d, updates %d→%d, failed runs count %d→%d, id sum %d→%d); retry",
				syncCounters.additions, currentCounters.additions,
				syncCounters.updates, currentCounters.updates,
				syncCounters.failedRunCount, currentCounters.failedRunCount,
				syncCounters.failedRunIDSum, currentCounters.failedRunIDSum,
			))
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
	if err := os.WriteFile(stateFile, stateData, 0600); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to save sync state: %v\n", err)
	}

	return &buildResult{
		ExportedCount: exportedCount,
		MaxMessageID:  maxID,
		OutputDir:     analyticsDir,
	}, nil
}

// missingRequiredParquet returns true if some parquet data exists but is
// missing one or more required tables (e.g. upgrading from a cache that
// predates the conversations export). Returns false for a fresh empty cache.
func missingRequiredParquet(analyticsDir string) bool {
	if query.HasCompleteParquetData(analyticsDir) {
		return false
	}
	// Incomplete — check if any table has data (partial/broken cache vs fresh).
	for _, dir := range query.RequiredParquetDirs {
		pattern := filepath.Join(analyticsDir, dir, "*.parquet")
		if matches, _ := filepath.Glob(pattern); len(matches) > 0 {
			return true
		}
		// For messages, also check hive-partitioned layout (messages/year=*/*.parquet)
		if dir == tableMessages {
			if deep, _ := filepath.Glob(filepath.Join(analyticsDir, dir, "*", "*.parquet")); len(deep) > 0 {
				return true
			}
		}
	}
	return false
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

// setupSQLiteSource makes SQLite tables available to DuckDB as sqlite_db.*.
// On Linux/macOS it uses DuckDB's sqlite extension (ATTACH).
// On Windows it exports tables to CSV and creates DuckDB views, since the
// sqlite_scanner extension is not available for MinGW builds.
func setupSQLiteSource(duckDB *sql.DB, dbPath string) (cleanup func(), err error) {
	if runtime.GOOS != "windows" {
		// Try sqlite_scanner extension; fall back to CSV if unavailable
		// (e.g. air-gapped environment with no internet for extension download).
		if _, err := duckDB.Exec("INSTALL sqlite; LOAD sqlite;"); err != nil {
			fmt.Fprintf(os.Stderr, "  sqlite_scanner unavailable, using CSV fallback: %v\n", err)
		} else {
			escapedPath := strings.ReplaceAll(dbPath, "'", "''")
			if _, err := duckDB.Exec(fmt.Sprintf("ATTACH '%s' AS sqlite_db (TYPE sqlite, READ_ONLY)", escapedPath)); err != nil {
				fmt.Fprintf(os.Stderr, "  sqlite attach failed, using CSV fallback: %v\n", err)
			} else {
				return func() {}, nil
			}
		}
	}

	// CSV fallback: export SQLite tables to CSV, create DuckDB views.
	// Prefer the database's parent directory for temp files (avoids
	// cross-device moves), but fall back through system temp and
	// ~/.msgvault/tmp/ for read-only or restricted environments.
	tmpDir, err := config.MkTempDir(".cache-tmp-*", filepath.Dir(dbPath))
	if err != nil {
		return nil, err
	}

	sqliteDB, err := sql.Open("sqlite3", dbPath+"?mode=ro")
	if err != nil {
		_ = os.RemoveAll(tmpDir)
		return nil, fmt.Errorf("open sqlite for CSV export: %w", err)
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
		csvPath := filepath.Join(tmpDir, t.name+".csv")
		if err := exportToCSV(sqliteDB, t.query, csvPath); err != nil {
			_ = sqliteDB.Close()
			_ = os.RemoveAll(tmpDir)
			return nil, fmt.Errorf("export %s to CSV: %w", t.name, err)
		}
	}
	_ = sqliteDB.Close()

	// Create sqlite_db schema with views pointing to CSV files.
	// This lets the existing COPY queries reference sqlite_db.tablename unchanged.
	if _, err := duckDB.Exec("CREATE SCHEMA sqlite_db"); err != nil {
		_ = os.RemoveAll(tmpDir)
		return nil, fmt.Errorf("create sqlite_db schema: %w", err)
	}
	for _, t := range tables {
		csvPath := filepath.Join(tmpDir, t.name+".csv")
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
		if _, err := duckDB.Exec(viewSQL); err != nil {
			_ = os.RemoveAll(tmpDir)
			return nil, fmt.Errorf("create view sqlite_db.%s: %w", t.name, err)
		}
	}

	return func() { _ = os.RemoveAll(tmpDir) }, nil
}

// csvNullStr is written for NULL values in CSV exports so DuckDB can
// distinguish NULL from empty string via the nullstr option.
const csvNullStr = `\N`

// exportToCSV exports the results of a SQL query to a CSV file.
// NULL values are written as \N (PostgreSQL convention).
func exportToCSV(db *sql.DB, query string, dest string) error {
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

// rebuildCacheAfterWrite rebuilds the analytics cache after a write
// operation. Uses the staleness check to determine whether a full
// rebuild (deletions/mutations) or incremental export (new messages
// only) is needed. Logs a warning on failure — the data is safe in
// SQLite.
func rebuildCacheAfterWrite(dbPath string) {
	analyticsDir := cfg.AnalyticsDir()
	fullRebuild := false
	if staleness := cacheNeedsBuild(dbPath, analyticsDir); staleness.FullRebuild {
		fullRebuild = true
	}
	result, err := buildCache(dbPath, analyticsDir, fullRebuild)
	if err != nil {
		fmt.Fprintf(os.Stderr,
			"Warning: cache rebuild failed: %v\n", err)
		fmt.Fprintf(os.Stderr,
			"Run 'msgvault build-cache' to retry.\n")
		return
	}
	if !result.Skipped {
		logger.Info("cache rebuilt", "exported", result.ExportedCount)
	}
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
func buildCacheSubprocess(ctx context.Context, fullRebuild bool) error {
	// Serialize with each other so parallel per-account syncs in the
	// daemon don't spawn concurrent cache builds racing on shared files.
	buildCacheMu.Lock()
	defer buildCacheMu.Unlock()

	cmd, err := newBuildCacheSubprocessCommand(ctx, fullRebuild)
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

func buildCacheSubprocessStream(
	ctx context.Context,
	fullRebuild bool,
	emit func(api.CLICacheBuildEvent) error,
) error {
	buildCacheMu.Lock()
	defer buildCacheMu.Unlock()

	cmd, err := newBuildCacheSubprocessCommand(ctx, fullRebuild)
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

func newBuildCacheSubprocessCommand(ctx context.Context, fullRebuild bool) (*exec.Cmd, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("locate msgvault executable: %w", err)
	}

	args := globalConfigFlagArgs()
	args = append(args, "--no-log-file", "build-cache")
	if fullRebuild {
		args = append(args, "--full-rebuild")
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
func rebuildCacheAfterScheduledSync(ctx context.Context, identifier string) {
	dbPath := cfg.DatabaseDSN()
	if store.IsPostgresURL(dbPath) {
		return
	}
	analyticsDir := cfg.AnalyticsDir()
	staleness := cacheNeedsBuild(dbPath, analyticsDir)
	if !staleness.NeedsBuild {
		return
	}
	logger.Info("rebuilding cache after sync",
		"identifier", identifier, "reason", staleness.Reason,
		"full_rebuild", staleness.FullRebuild)
	if err := buildCacheSubprocess(ctx, staleness.FullRebuild); err != nil {
		logger.Error("cache build failed", "error", err)
		// Don't fail the sync for cache build errors.
	} else {
		logger.Info("cache build completed")
	}
}

func init() {
	rootCmd.AddCommand(buildCacheCmd)
	rootCmd.AddCommand(cacheStatsCmd)
	buildCacheCmd.Flags().BoolVar(&fullRebuild, "full-rebuild", false, "Rebuild all cache files from scratch")
}
