package query

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// CacheSyncState is the commit marker written after a complete analytics
// cache publication. SQLite remains authoritative; these watermarks only
// describe the Parquet snapshot that readers may use.
type CacheSyncState struct {
	LastMessageID          int64     `json:"last_message_id"`
	LastSyncAt             time.Time `json:"last_sync_at"`
	SchemaVersion          int       `json:"schema_version,omitempty"`
	LastCompletedSyncRunID int64     `json:"last_completed_sync_run_id,omitempty"`
	LastCacheAdditionCount int64     `json:"last_cache_addition_count,omitempty"`
	LastCacheUpdateCount   int64     `json:"last_cache_update_count,omitempty"`
	LastFailedSyncRunCount int64     `json:"last_failed_sync_run_count,omitempty"`
	LastFailedSyncRunIDSum int64     `json:"last_failed_sync_run_id_sum,omitempty"`
}

type CacheReadiness string

const (
	CacheAbsent      CacheReadiness = "absent"
	CacheReady       CacheReadiness = "ready"
	CacheInterrupted CacheReadiness = "interrupted"
)

var (
	ErrCacheUnavailable  = errors.New("analytics cache unavailable")
	errInvalidCacheState = errors.New("invalid analytics cache state")
)

func CacheStatePath(analyticsDir string) string {
	return filepath.Join(analyticsDir, "_last_sync.json")
}

func ReadCacheSyncState(analyticsDir string) (CacheSyncState, error) {
	data, err := os.ReadFile(CacheStatePath(analyticsDir))
	if err != nil {
		return CacheSyncState{}, err
	}
	var state CacheSyncState
	if err := json.Unmarshal(data, &state); err != nil {
		return CacheSyncState{}, fmt.Errorf("%w: %w", errInvalidCacheState, err)
	}
	return state, nil
}

// InspectCacheReadiness classifies only committed live cache paths. Sibling
// staging directories are deliberately outside analyticsDir and never enter
// this inspection.
func InspectCacheReadiness(analyticsDir string) (CacheReadiness, error) {
	info, err := os.Stat(analyticsDir)
	switch {
	case err == nil && !info.IsDir():
		return "", fmt.Errorf("inspect analytics cache root: %s is not a directory", analyticsDir)
	case errors.Is(err, os.ErrNotExist):
		return CacheAbsent, nil
	case err != nil:
		return "", fmt.Errorf("inspect analytics cache root: %w", err)
	}

	anyParquet := false
	complete := true
	for _, dataset := range RequiredParquetDirs {
		hasParquet, err := datasetHasParquet(analyticsDir, dataset)
		if err != nil {
			return "", fmt.Errorf("inspect analytics cache dataset %s: %w", dataset, err)
		}
		anyParquet = anyParquet || hasParquet
		complete = complete && hasParquet
	}

	state, err := ReadCacheSyncState(analyticsDir)
	switch {
	case err == nil:
		if complete && !state.LastSyncAt.IsZero() {
			return CacheReady, nil
		}
		return CacheInterrupted, nil
	case errors.Is(err, os.ErrNotExist):
		if !anyParquet {
			return CacheAbsent, nil
		}
		return CacheInterrupted, nil
	case errors.Is(err, errInvalidCacheState):
		return CacheInterrupted, nil
	default:
		return "", fmt.Errorf("read analytics cache state: %w", err)
	}
}

func datasetHasParquet(analyticsDir, dataset string) (bool, error) {
	datasetDir := filepath.Join(analyticsDir, dataset)
	entries, err := os.ReadDir(datasetDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	for _, entry := range entries {
		if !entry.IsDir() && strings.EqualFold(filepath.Ext(entry.Name()), ".parquet") {
			return true, nil
		}
		if dataset != datasetMessages || !entry.IsDir() {
			continue
		}
		partitionEntries, err := os.ReadDir(filepath.Join(datasetDir, entry.Name()))
		if err != nil {
			return false, err
		}
		for _, partitionEntry := range partitionEntries {
			if !partitionEntry.IsDir() && strings.EqualFold(filepath.Ext(partitionEntry.Name()), ".parquet") {
				return true, nil
			}
		}
	}
	return false, nil
}
