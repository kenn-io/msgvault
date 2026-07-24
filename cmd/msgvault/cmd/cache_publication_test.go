package cmd

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/query"
)

func TestCachePublicationCommitsRevisionTimestampAndDatasetFingerprint(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	parent := t.TempDir()
	analyticsDir := filepath.Join(parent, "analytics")
	staging, err := newCacheStaging(analyticsDir)
	require.NoError(err)
	t.Cleanup(func() { _ = staging.cleanup() })
	writePublicationTree(t, staging.root, "new.parquet")
	input, err := json.Marshal(query.CacheSyncState{
		SchemaVersion:          query.CacheSchemaVersion,
		LastMessageID:          41,
		LastSyncAt:             time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC),
		LastCacheUpdateCount:   3,
		LastFailedSyncRunCount: 2,
		LastFailedSyncRunIDSum: 19,
	})
	require.NoError(err)

	require.NoError(publishCache(staging, analyticsDir, true, input))
	state, err := query.ReadCacheSyncState(analyticsDir)
	require.NoError(err)
	assert.False(state.PublishedAt.IsZero())
	fingerprint, err := query.CacheDatasetFingerprint(analyticsDir)
	require.NoError(err)
	assert.Equal(fingerprint, state.DatasetFingerprint)
	assert.NotEmpty(state.Revision())
	readiness, err := query.InspectCacheReadiness(analyticsDir)
	require.NoError(err)
	assert.Equal(query.CacheReady, readiness)
}

func TestCachePublicationFullReplacesEveryDatasetAndWritesStateLast(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	parent := t.TempDir()
	analyticsDir := filepath.Join(parent, "analytics")
	writePublicationTree(t, analyticsDir, "old.parquet")
	oldState := []byte(`{"last_sync_at":"2026-07-15T10:00:00Z"}`)
	require.NoError(os.WriteFile(query.CacheStatePath(analyticsDir), oldState, 0o600))

	staging, err := newCacheStaging(analyticsDir)
	require.NoError(err)
	t.Cleanup(func() { _ = staging.cleanup() })
	writePublicationTree(t, staging.root, "new.parquet")
	newState := []byte(`{"last_sync_at":"2026-07-15T11:00:00Z"}`)

	require.NoError(publishCache(staging, analyticsDir, true, newState))

	for _, dataset := range query.RequiredParquetDirs {
		assert.False(publicationFileExists(analyticsDir, dataset, "old.parquet"), dataset)
		assert.True(publicationFileExists(analyticsDir, dataset, "new.parquet"), dataset)
	}
	gotState, err := os.ReadFile(query.CacheStatePath(analyticsDir))
	require.NoError(err)
	var committed query.CacheSyncState
	require.NoError(json.Unmarshal(gotState, &committed))
	assert.Equal(time.Date(2026, 7, 15, 11, 0, 0, 0, time.UTC), committed.LastSyncAt)
	assert.False(committed.PublishedAt.IsZero())
	assert.NotEmpty(committed.DatasetFingerprint)
}

func TestIncrementalPublicationReplacesDimensionsAndPrefixesAppends(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	parent := t.TempDir()
	analyticsDir := filepath.Join(parent, "analytics")
	writePublicationTree(t, analyticsDir, "old.parquet")
	require.NoError(os.WriteFile(query.CacheStatePath(analyticsDir),
		[]byte(`{"last_sync_at":"2026-07-15T10:00:00Z"}`), 0o600))

	staging, err := newCacheStaging(analyticsDir)
	require.NoError(err)
	t.Cleanup(func() { _ = staging.cleanup() })
	writePublicationTree(t, staging.root, "data.parquet")

	require.NoError(publishCache(staging, analyticsDir, false,
		[]byte(`{"last_sync_at":"2026-07-15T11:00:00Z"}`)))

	for _, dataset := range []string{"participants", "participant_identifiers", "labels", "sources", "conversations"} {
		assert.False(publicationFileExists(analyticsDir, dataset, "old.parquet"), dataset)
		assert.True(publicationFileExists(analyticsDir, dataset, "data.parquet"), dataset)
	}
	for _, dataset := range []string{"message_recipients", "message_labels", "attachments"} {
		assert.True(publicationFileExists(analyticsDir, dataset, "old.parquet"), dataset)
		assert.True(publicationFileExists(analyticsDir, dataset, staging.buildID+"-data.parquet"), dataset)
	}
	assert.True(publicationFileExists(analyticsDir, "messages", "old.parquet"))
	assert.FileExists(filepath.Join(analyticsDir, "messages", "year=2024",
		staging.buildID+"-data.parquet"))
}

func TestCachePublicationCollisionFailsBeforeInvalidation(t *testing.T) {
	require := require.New(t)
	parent := t.TempDir()
	analyticsDir := filepath.Join(parent, "analytics")
	writePublicationTree(t, analyticsDir, "old.parquet")
	oldState := []byte(`{"last_sync_at":"2026-07-15T10:00:00Z"}`)
	require.NoError(os.WriteFile(query.CacheStatePath(analyticsDir), oldState, 0o600))

	staging, err := newCacheStaging(analyticsDir)
	require.NoError(err)
	t.Cleanup(func() { _ = staging.cleanup() })
	writePublicationTree(t, staging.root, "data.parquet")
	collision := filepath.Join(analyticsDir, "message_recipients", staging.buildID+"-data.parquet")
	require.NoError(os.WriteFile(collision, []byte("collision"), 0o600))

	err = publishCache(staging, analyticsDir, false,
		[]byte(`{"last_sync_at":"2026-07-15T11:00:00Z"}`))
	require.ErrorContains(err, "already exists")
	gotState, readErr := os.ReadFile(query.CacheStatePath(analyticsDir))
	require.NoError(readErr)
	require.Equal(oldState, gotState)
}

func TestCachePublicationFailureAfterInvalidationLeavesInterruptedState(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	parent := t.TempDir()
	analyticsDir := filepath.Join(parent, "analytics")
	writePublicationTree(t, analyticsDir, "old.parquet")
	require.NoError(os.WriteFile(query.CacheStatePath(analyticsDir),
		[]byte(`{"last_sync_at":"2026-07-15T10:00:00Z"}`), 0o600))

	staging, err := newCacheStaging(analyticsDir)
	require.NoError(err)
	t.Cleanup(func() { _ = staging.cleanup() })
	writePublicationTree(t, staging.root, "new.parquet")
	publishErr := errors.New("publish interrupted")
	buildCacheAfterStateInvalidationHook = func() error { return publishErr }
	t.Cleanup(func() { buildCacheAfterStateInvalidationHook = nil })
	err = publishCache(staging, analyticsDir, true,
		[]byte(`{"last_sync_at":"2026-07-15T11:00:00Z"}`))
	require.ErrorIs(err, publishErr)

	_, err = os.Stat(query.CacheStatePath(analyticsDir))
	require.ErrorIs(err, os.ErrNotExist)
	assert.True(publicationFileExists(analyticsDir, "sources", "old.parquet"))
}

func TestCachePublicationCleansOnlyPrivateStagingDirectories(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	parent := t.TempDir()
	analyticsDir := filepath.Join(parent, "analytics")
	stale := filepath.Join(parent, ".analytics.build-stale")
	unrelated := filepath.Join(parent, ".other.build-stale")
	require.NoError(os.MkdirAll(stale, 0o755))
	require.NoError(os.MkdirAll(unrelated, 0o755))
	require.NoError(os.WriteFile(query.CacheBuildLockPath(analyticsDir), []byte("lock"), 0o600))

	require.NoError(cleanupStaleCacheStaging(analyticsDir))
	assert.NoDirExists(stale)
	assert.DirExists(unrelated)
	assert.FileExists(query.CacheBuildLockPath(analyticsDir))
}

func writePublicationTree(t *testing.T, root, filename string) {
	t.Helper()
	for _, dataset := range query.RequiredParquetDirs {
		dir := filepath.Join(root, dataset)
		if dataset == "messages" {
			dir = filepath.Join(dir, "year=2024")
		}
		require.NoError(t, os.MkdirAll(dir, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(dir, filename), []byte(dataset), 0o600))
	}
}

func publicationFileExists(root, dataset, filename string) bool {
	if dataset == "messages" {
		_, err := os.Stat(filepath.Join(root, dataset, "year=2024", filename))
		return err == nil
	}
	_, err := os.Stat(filepath.Join(root, dataset, filename))
	return err == nil
}
