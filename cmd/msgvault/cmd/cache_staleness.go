package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/store"
)

// cacheStaleness describes why the analytics cache needs a rebuild.
type cacheStaleness struct {
	NeedsBuild  bool
	HasNew      bool // new messages since last build
	HasDeleted  bool // deletions since last build
	HasUpdated  bool // updates or additions within the cached ID boundary require repair
	FullRebuild bool // must rewrite all shards (not incremental)
	Reason      string
}

// deletedSinceBuildCountSQL counts exportable messages source-deleted since
// the last cache build. It runs on every daemon start before the API server
// binds, so it must be served by idx_messages_deleted_from_source_at rather
// than a full messages scan (seconds of cold-start latency on a large
// archive); the query-plan test locks that in.
func deletedSinceBuildCountSQL() string {
	return `
		SELECT COUNT(*) FROM messages
		WHERE deleted_from_source_at IS NOT NULL
		  AND deleted_from_source_at >= ?
		  AND ` + sentCacheExportMessageWhere("")
}

// hiddenSinceBuildCountSQL counts exportable messages dedup-hidden since the
// last cache build. Same cold-start constraint as deletedSinceBuildCountSQL:
// it must be served by idx_messages_deleted_at.
func hiddenSinceBuildCountSQL() string {
	return `
		SELECT COUNT(*) FROM messages
		WHERE deleted_at IS NOT NULL
		  AND deleted_at >= ?
		  AND deleted_from_source_at IS NULL
		  AND ` + sentCacheExportMessageWhere("")
}

// cacheNeedsBuild checks if the analytics cache needs to be built or
// updated. Collects all staleness signals before returning so that
// e.g. a mixed add+delete sync correctly reports both.
//
// The Parquet cache is a SQLite-only ETL — when dbPath points at a
// PostgreSQL DSN, this returns "no build needed" rather than dispatching
// SQLite-shaped queries against pgx (which would fail on the ?
// placeholders and the sqlite_master probe).
func cacheNeedsBuild(dbPath, analyticsDir string) cacheStaleness {
	if store.IsPostgresURL(dbPath) {
		return cacheStaleness{}
	}
	messagesDir := filepath.Join(analyticsDir, tableMessages)
	stateFile := filepath.Join(analyticsDir, "_last_sync.json")

	hasParquetData := query.HasParquetData(analyticsDir)

	// Load last sync state
	data, err := os.ReadFile(stateFile)
	if err != nil {
		if !hasParquetData {
			return cacheStaleness{
				NeedsBuild: true, FullRebuild: true,
				Reason: "no cache exists",
			}
		}
		return cacheStaleness{
			NeedsBuild: true, FullRebuild: true,
			Reason: "no sync state found",
		}
	}

	var state syncState
	if err := json.Unmarshal(data, &state); err != nil {
		return cacheStaleness{
			NeedsBuild: true, FullRebuild: true,
			Reason: "invalid sync state",
		}
	}

	// A cache written under a different Parquet schema layout is stale even
	// when message counts match, so bumping cacheSchemaVersion must force a
	// full rebuild. buildCache re-checks this, but the daemon only calls it
	// when this gate reports NeedsBuild.
	if state.SchemaVersion != cacheSchemaVersion {
		return cacheStaleness{
			NeedsBuild: true, FullRebuild: true,
			Reason: fmt.Sprintf("cache schema v%d != current v%d",
				state.SchemaVersion, cacheSchemaVersion),
		}
	}

	db, err := store.Open(dbPath)
	if err != nil {
		return cacheStaleness{
			NeedsBuild: true, FullRebuild: true,
			Reason: "cannot verify cache status",
		}
	}
	defer func() { _ = db.Close() }()

	var maxLiveID int64
	err = db.DB().QueryRow(`
		SELECT COALESCE(MAX(id), 0) FROM messages
		WHERE ` + cacheLiveMessageWhere("")).Scan(&maxLiveID)
	if err != nil {
		return cacheStaleness{
			NeedsBuild: true, FullRebuild: true,
			Reason: "cannot verify cache status",
		}
	}

	if maxLiveID == 0 && !hasParquetData {
		return cacheStaleness{}
	}

	if !hasParquetData {
		return cacheStaleness{
			NeedsBuild: true, FullRebuild: true,
			Reason: "no cache exists",
		}
	}

	// Collect staleness signals without short-circuiting so a mixed
	// add+delete sync correctly triggers a full rebuild.
	var reasons []string
	result := cacheStaleness{}

	if maxLiveID > state.LastMessageID {
		newCount := maxLiveID - state.LastMessageID
		result.HasNew = true
		reasons = append(reasons,
			fmt.Sprintf("%d new messages", newCount))
	}

	syncAtStr := state.LastSyncAt.UTC().Format("2006-01-02 15:04:05")
	var deletedSinceBuild int64
	err = db.DB().QueryRow(deletedSinceBuildCountSQL(), syncAtStr).Scan(&deletedSinceBuild)
	if err != nil {
		return cacheStaleness{
			NeedsBuild: true, FullRebuild: true,
			Reason: "cannot verify deletion state",
		}
	}
	if deletedSinceBuild > 0 {
		result.HasDeleted = true
		result.FullRebuild = true
		reasons = append(reasons,
			fmt.Sprintf("%d deletions", deletedSinceBuild))
	}

	// Dedup-hidden rows (deleted_at) are excluded from the messages
	// Parquet export, so a dedup run after the last cache build leaves
	// stale duplicate rows in the cache. Detect that by counting hides
	// since LastSyncAt and force a full rebuild if any are present.
	// The deleted_from_source_at IS NULL clause keeps the count
	// disjoint from the deletedSinceBuild count above so a row that is
	// both source-deleted and dedup-hidden after LastSyncAt is reported
	// once (as a deletion), not double-counted in the reason string.
	var hiddenSinceBuild int64
	err = db.DB().QueryRow(hiddenSinceBuildCountSQL(), syncAtStr).Scan(&hiddenSinceBuild)
	if err != nil {
		return cacheStaleness{
			NeedsBuild: true, FullRebuild: true,
			Reason: "cannot verify dedup state",
		}
	}
	if hiddenSinceBuild > 0 {
		result.HasDeleted = true
		result.FullRebuild = true
		reasons = append(reasons,
			fmt.Sprintf("%d dedup-hidden", hiddenSinceBuild))
	}

	var hasSyncRunsTable int
	err = db.DB().QueryRow(`
		SELECT COUNT(*) FROM sqlite_master
		WHERE type = 'table' AND name = 'sync_runs'
	`).Scan(&hasSyncRunsTable)
	if err != nil {
		return cacheStaleness{
			NeedsBuild: true, FullRebuild: true,
			Reason: "cannot verify sync history",
		}
	}
	if hasSyncRunsTable > 0 {
		counters, counterErr := readCacheSyncCounters(db.DB())
		err = counterErr
		if err != nil {
			return cacheStaleness{
				NeedsBuild: true, FullRebuild: true,
				Reason: "cannot verify sync history",
			}
		}
		if counters.updates != state.LastCacheUpdateCount {
			result.HasUpdated = true
			result.FullRebuild = true
			if updateDelta := counters.updates - state.LastCacheUpdateCount; updateDelta > 0 {
				reasons = append(reasons,
					fmt.Sprintf("%d updated messages", updateDelta))
			} else {
				reasons = append(reasons, fmt.Sprintf(
					"cache update watermark changed from %d to %d",
					state.LastCacheUpdateCount, counters.updates))
			}
		}
		if counters.failedRunCount != state.LastFailedSyncRunCount ||
			counters.failedRunIDSum != state.LastFailedSyncRunIDSum {
			result.HasUpdated = true
			result.FullRebuild = true
			reasons = append(reasons, fmt.Sprintf(
				"failed sync watermark changed from count=%d,sum=%d to count=%d,sum=%d",
				state.LastFailedSyncRunCount, state.LastFailedSyncRunIDSum,
				counters.failedRunCount, counters.failedRunIDSum))
		}
		if counters.additions != state.LastCacheAdditionCount {
			// A larger message ID gives the incremental exporter an exact lower
			// boundary for ordinary append-only syncs. If the ID boundary did not
			// move (or history moved backwards), the changed addition counter may
			// describe related rows for a parent already present in Parquet, so a
			// full rebuild is the only safe repair.
			if counters.additions < state.LastCacheAdditionCount || maxLiveID <= state.LastMessageID {
				result.HasUpdated = true
				result.FullRebuild = true
				reasons = append(reasons, fmt.Sprintf(
					"cache addition watermark changed from %d to %d within message boundary %d",
					state.LastCacheAdditionCount, counters.additions, state.LastMessageID))
			}
		}
	}

	// Check if parquet files actually exist (directory might be empty)
	files, _ := filepath.Glob(
		filepath.Join(messagesDir, "*", "*.parquet"))
	if len(files) == 0 {
		result.FullRebuild = true
		reasons = append(reasons, "cache directory empty")
	}

	if missingRequiredParquet(analyticsDir) {
		result.FullRebuild = true
		reasons = append(reasons, "cache missing required tables")
	}

	if len(reasons) > 0 {
		result.NeedsBuild = true
		result.Reason = strings.Join(reasons, "; ")
	}

	return result
}
