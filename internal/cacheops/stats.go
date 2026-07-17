package cacheops

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "github.com/duckdb/duckdb-go/v2" // DuckDB driver (database/sql)
	"go.kenn.io/msgvault/internal/query"
)

const (
	StatusReady        = "ready"
	StatusNoCacheFiles = "no_cache_files"
	StatusNoCacheData  = "no_cache_data"
	StatusInterrupted  = "interrupted"

	tableAttachments = "attachments"
	tableMessages    = "messages"
)

type CacheStats struct {
	Status              string     `json:"status"`
	TotalMessages       int64      `json:"total_messages,omitempty"`
	Sources             int64      `json:"sources,omitempty"`
	UniqueSenders       int64      `json:"unique_senders,omitempty"`
	UniqueDomains       int64      `json:"unique_domains,omitempty"`
	MinYear             *int64     `json:"min_year,omitempty"`
	MaxYear             *int64     `json:"max_year,omitempty"`
	TotalSizeBytes      int64      `json:"total_size_bytes,omitempty"`
	AttachmentSizeBytes int64      `json:"attachment_size_bytes,omitempty"`
	LastSyncAt          *time.Time `json:"last_sync_at,omitempty"`
	LastMessageID       *int64     `json:"last_message_id,omitempty"`
	Warnings            []string   `json:"warnings,omitempty"`
}

func CollectStats(ctx context.Context, analyticsDir string) (*CacheStats, error) {
	// Hold the shared cache lock across file discovery, DuckDB queries, and
	// the sync-state read so a concurrent rebuild cannot remove files
	// mid-collection.
	release, err := query.AcquireCacheReadLock(ctx, analyticsDir)
	if err != nil {
		return nil, fmt.Errorf("lock analytics cache for stats: %w", err)
	}
	defer release()

	readiness, err := query.InspectCacheReadiness(analyticsDir)
	if err != nil {
		return nil, fmt.Errorf("inspect analytics cache readiness: %w", err)
	}
	switch readiness {
	case query.CacheAbsent:
		return &CacheStats{Status: StatusNoCacheFiles}, nil
	case query.CacheInterrupted:
		return &CacheStats{Status: StatusInterrupted}, nil
	case query.CacheReady:
		// Continue while holding the same shared lock through all Parquet reads.
	default:
		return nil, fmt.Errorf("unknown analytics cache readiness %q", readiness)
	}

	state, err := query.ReadCacheSyncState(analyticsDir)
	if err != nil {
		return nil, fmt.Errorf("read analytics cache state: %w", err)
	}

	db, err := sql.Open("duckdb", "")
	if err != nil {
		return nil, fmt.Errorf("open duckdb: %w", err)
	}
	defer func() { _ = db.Close() }()

	escapedDir := strings.ReplaceAll(analyticsDir, "'", "''")
	statsSQL := fmt.Sprintf(`
		WITH msg AS (
			SELECT * FROM read_parquet('%s/messages/**/*.parquet', hive_partitioning=true)
		),
		mr AS (
			SELECT * FROM read_parquet('%s/message_recipients/*.parquet')
		),
		p AS (
			SELECT * FROM read_parquet('%s/participants/*.parquet')
		)
		SELECT
			COUNT(*) as total_messages,
			COUNT(DISTINCT m.source_id) as sources,
			(SELECT COUNT(DISTINCT p2.email_address)
			 FROM mr mr2
			 JOIN p p2 ON p2.id = mr2.participant_id
			 WHERE mr2.recipient_type = 'from') as unique_senders,
			(SELECT COUNT(DISTINCT p2.domain)
			 FROM mr mr2
			 JOIN p p2 ON p2.id = mr2.participant_id
			 WHERE mr2.recipient_type = 'from') as unique_domains,
			MIN(m.year) as min_year,
			MAX(m.year) as max_year,
			COALESCE(SUM(m.size_estimate), 0) as total_size
		FROM msg m
		`, escapedDir, escapedDir, escapedDir)

	var minYear, maxYear sql.NullInt64
	result := &CacheStats{Status: StatusReady}
	err = db.QueryRow(statsSQL).Scan(
		&result.TotalMessages,
		&result.Sources,
		&result.UniqueSenders,
		&result.UniqueDomains,
		&minYear,
		&maxYear,
		&result.TotalSizeBytes,
	)
	if err != nil {
		return nil, fmt.Errorf("query stats: %w", err)
	}
	if minYear.Valid {
		result.MinYear = &minYear.Int64
	}
	if maxYear.Valid {
		result.MaxYear = &maxYear.Int64
	}

	attachmentsDir := filepath.Join(analyticsDir, tableAttachments)
	if _, err := os.Stat(attachmentsDir); err == nil {
		attachSQL := fmt.Sprintf(`
			SELECT COALESCE(SUM(size), 0) FROM read_parquet('%s/attachments/*.parquet')
			`, escapedDir)
		if err := db.QueryRow(attachSQL).Scan(&result.AttachmentSizeBytes); err != nil {
			result.Warnings = append(result.Warnings, fmt.Sprintf("could not read attachment stats: %v", err))
		}
	}

	result.LastSyncAt = &state.LastSyncAt
	result.LastMessageID = &state.LastMessageID

	return result, nil
}
