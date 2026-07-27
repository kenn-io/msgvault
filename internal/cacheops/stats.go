package cacheops

import (
	"context"
	"fmt"
	"time"

	"go.kenn.io/msgvault/internal/query"
)

const (
	StatusReady        = "ready"
	StatusNoCacheFiles = "no_cache_files"
	StatusNoCacheData  = "no_cache_data"
	StatusInterrupted  = "interrupted"
	StatusStaleSchema  = "stale_schema"
	StatusDrifted      = "drifted"

	tableMessages = "messages"
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
	case query.CacheStaleSchema:
		return &CacheStats{Status: StatusStaleSchema}, nil
	case query.CacheDrifted:
		return &CacheStats{Status: StatusDrifted}, nil
	case query.CacheReady:
		// Continue while holding the same shared lock through all Parquet reads.
	default:
		return nil, fmt.Errorf("unknown analytics cache readiness %q", readiness)
	}

	state, err := query.ReadCacheSyncState(analyticsDir)
	if err != nil {
		return nil, fmt.Errorf("read analytics cache state: %w", err)
	}

	result := &CacheStats{
		Status:              StatusReady,
		TotalMessages:       state.Stats.TotalMessages,
		Sources:             state.Stats.Sources,
		UniqueSenders:       state.Stats.UniqueSenders,
		UniqueDomains:       state.Stats.UniqueDomains,
		MinYear:             state.Stats.MinYear,
		MaxYear:             state.Stats.MaxYear,
		TotalSizeBytes:      state.Stats.TotalSizeBytes,
		AttachmentSizeBytes: state.Stats.AttachmentSizeBytes,
	}
	result.LastSyncAt = &state.LastSyncAt
	result.LastMessageID = &state.LastMessageID

	return result, nil
}
