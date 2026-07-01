package cacheops

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "github.com/duckdb/duckdb-go/v2" // DuckDB driver (database/sql)
)

const (
	StatusReady        = "ready"
	StatusNoCacheFiles = "no_cache_files"
	StatusNoCacheData  = "no_cache_data"

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

type syncState struct {
	LastMessageID int64     `json:"last_message_id"`
	LastSyncAt    time.Time `json:"last_sync_at"`
}

func CollectStats(analyticsDir string) (*CacheStats, error) {
	messagesDir := filepath.Join(analyticsDir, tableMessages)
	if _, err := os.Stat(messagesDir); os.IsNotExist(err) {
		return &CacheStats{Status: StatusNoCacheFiles}, nil
	}

	parquetFiles, err := filepath.Glob(filepath.Join(messagesDir, "**", "*.parquet"))
	if err != nil {
		return nil, fmt.Errorf("check for cache files: %w", err)
	}
	if len(parquetFiles) == 0 {
		parquetFiles, _ = filepath.Glob(filepath.Join(messagesDir, "*", "*.parquet"))
	}
	if len(parquetFiles) == 0 {
		return &CacheStats{Status: StatusNoCacheData}, nil
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

	stateFile := filepath.Join(analyticsDir, "_last_sync.json")
	if data, err := os.ReadFile(stateFile); err == nil {
		var state syncState
		if json.Unmarshal(data, &state) == nil {
			result.LastSyncAt = &state.LastSyncAt
			result.LastMessageID = &state.LastMessageID
		}
	}

	return result, nil
}
