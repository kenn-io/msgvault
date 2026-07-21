package query

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// CacheSchemaVersion is the sole schema compatibility version shared by the
// cache publisher and analytical readers. Bumped to 14 because ffe9904a
// widened owner_participants/is_from_me matching from email-only to
// identifier-type-aware (phone, chat handle, etc.): caches built under v13
// before that change baked the narrower email-only derivation, and without
// this bump they would never be flagged stale by schema-version comparison.
const CacheSchemaVersion = 14

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
	IdentityRevision       int64     `json:"identity_revision,omitempty"`
	// AccountIdentityRevision tracks identity mutations that invalidate
	// baked message data — confirming or removing a "me" address, and
	// participant merges (which repoint messages.sender_id) — separately
	// from IdentityRevision (which also covers plain participant
	// link/unlink). These changes invalidate the message-baked is_from_me
	// flag, which the lightweight identity-only refresh does not re-derive,
	// so this field must only advance on a full rebuild — see
	// cacheops.RefreshIdentityDatasets.
	AccountIdentityRevision int64     `json:"account_identity_revision,omitempty"`
	PublishedAt             time.Time `json:"published_at"`
	DatasetFingerprint      string    `json:"dataset_fingerprint"`
}

type CacheReadiness string

const (
	CacheAbsent      CacheReadiness = "absent"
	CacheReady       CacheReadiness = "ready"
	CacheInterrupted CacheReadiness = "interrupted"
	CacheStaleSchema CacheReadiness = "stale_schema"
	CacheDrifted     CacheReadiness = "drifted"
)

var (
	ErrCacheUnavailable  = errors.New("analytics cache unavailable")
	errInvalidCacheState = errors.New("invalid analytics cache state")
)

// CacheUnavailableError preserves the named readiness reason while remaining
// compatible with errors.Is(err, ErrCacheUnavailable).
type CacheUnavailableError struct {
	Readiness CacheReadiness
}

func (e *CacheUnavailableError) Error() string {
	return fmt.Sprintf("%s: cache is %s", ErrCacheUnavailable, e.Readiness)
}

func (e *CacheUnavailableError) Unwrap() error { return ErrCacheUnavailable }

// Revision identifies one committed cache publication. It intentionally uses
// only commit-marker fields, never ambient filesystem state.
func (s CacheSyncState) Revision() string {
	payload := fmt.Sprintf("v=%d|message=%d|watermark=%s|run=%d|add=%d|update=%d|fail_count=%d|fail_sum=%d|identity=%d|account_identity=%d|published=%s",
		s.SchemaVersion,
		s.LastMessageID,
		s.LastSyncAt.UTC().Format(time.RFC3339Nano),
		s.LastCompletedSyncRunID,
		s.LastCacheAdditionCount,
		s.LastCacheUpdateCount,
		s.LastFailedSyncRunCount,
		s.LastFailedSyncRunIDSum,
		s.IdentityRevision,
		s.AccountIdentityRevision,
		s.PublishedAt.UTC().Format(time.RFC3339Nano),
	)
	return fmt.Sprintf("cache-%x", sha256.Sum256([]byte(payload)))
}

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
		if state.LastSyncAt.IsZero() {
			return CacheInterrupted, nil
		}
		if state.SchemaVersion != CacheSchemaVersion {
			return CacheStaleSchema, nil
		}
		if !complete {
			return CacheInterrupted, nil
		}
		if state.PublishedAt.IsZero() || state.DatasetFingerprint == "" {
			return CacheInterrupted, nil
		}
		fingerprint, fingerprintErr := CacheDatasetFingerprint(analyticsDir)
		if fingerprintErr != nil {
			return "", fingerprintErr
		}
		if fingerprint != state.DatasetFingerprint {
			return CacheDrifted, nil
		}
		return CacheReady, nil
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

// CacheDatasetFingerprint records the committed dataset file set. It covers
// names, sizes, and modification times for every Parquet file without reading
// archive content into memory.
func CacheDatasetFingerprint(analyticsDir string) (string, error) {
	var records []string
	for _, dataset := range RequiredParquetDirs {
		root := filepath.Join(analyticsDir, dataset)
		err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if entry.IsDir() || !strings.EqualFold(filepath.Ext(entry.Name()), ".parquet") {
				return nil
			}
			info, err := entry.Info()
			if err != nil {
				return fmt.Errorf("read analytics dataset file info: %w", err)
			}
			rel, err := filepath.Rel(analyticsDir, path)
			if err != nil {
				return err
			}
			records = append(records, fmt.Sprintf("%s|%d|%d", filepath.ToSlash(rel), info.Size(), info.ModTime().UnixNano()))
			return nil
		})
		if err != nil {
			return "", fmt.Errorf("fingerprint analytics dataset %s: %w", dataset, err)
		}
	}
	sort.Strings(records)
	return fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(strings.Join(records, "\n")))), nil
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
