package cmd

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/query"
)

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
	assert.Equal(newState, gotState)
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

	for _, dataset := range []string{"participants", "labels", "sources", "conversations"} {
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
