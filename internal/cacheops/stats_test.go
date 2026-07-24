package cacheops

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gofrs/flock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/query"
)

// TestCollectStatsWaitsForCacheBuildLock pins that stats collection
// participates in the cache reader/writer protocol: a concurrent build's
// exclusive lock must block collection so files cannot vanish mid-scan.
func TestCollectStatsWaitsForCacheBuildLock(t *testing.T) {
	require := require.New(t)
	dir := t.TempDir()
	require.NoError(os.MkdirAll(filepath.Join(dir, tableMessages), 0o755), "create messages dir")

	build := flock.New(query.CacheBuildLockPath(dir))
	locked, err := build.TryLock()
	require.NoError(err, "acquire exclusive build lock")
	require.True(locked, "acquire exclusive build lock")

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	_, err = CollectStats(ctx, dir)
	require.Error(err, "stats collection must wait while a build holds the lock")

	require.NoError(build.Unlock(), "release build lock")
	stats, err := CollectStats(context.Background(), dir)
	require.NoError(err, "stats collection after the build releases")
	require.Equal(StatusNoCacheFiles, stats.Status, "empty cache directories are absent")
}

func TestCollectStatsClassifiesCacheReadiness(t *testing.T) {
	t.Run("absent", func(t *testing.T) {
		require := require.New(t)
		stats, err := CollectStats(context.Background(), t.TempDir())
		require.NoError(err)
		assert.Equal(t, StatusNoCacheFiles, stats.Status)
	})

	t.Run("ready", func(t *testing.T) {
		require := require.New(t)
		assert := assert.New(t)
		dir := writeCacheStatsFixture(t)
		stats, err := CollectStats(context.Background(), dir)
		require.NoError(err)
		assert.Equal(StatusReady, stats.Status)
		assert.Equal(int64(1), stats.TotalMessages)
		assert.Equal(int64(7), *stats.LastMessageID)
	})

	t.Run("files without state", func(t *testing.T) {
		require := require.New(t)
		dir := t.TempDir()
		require.NoError(os.MkdirAll(filepath.Join(dir, tableMessages), 0o755))
		require.NoError(os.WriteFile(filepath.Join(dir, tableMessages, "broken.parquet"), []byte("not parquet"), 0o600))

		stats, err := CollectStats(context.Background(), dir)
		require.NoError(err)
		assert.Equal(t, StatusInterrupted, stats.Status)
	})

	t.Run("malformed state", func(t *testing.T) {
		require := require.New(t)
		dir := writeCacheStatsFixture(t)
		require.NoError(os.WriteFile(query.CacheStatePath(dir), []byte("{"), 0o600))

		stats, err := CollectStats(context.Background(), dir)
		require.NoError(err)
		assert.Equal(t, StatusInterrupted, stats.Status)
	})

	t.Run("incomplete files", func(t *testing.T) {
		require := require.New(t)
		dir := writeCacheStatsFixture(t)
		require.NoError(os.RemoveAll(filepath.Join(dir, "labels")))

		stats, err := CollectStats(context.Background(), dir)
		require.NoError(err)
		assert.Equal(t, StatusInterrupted, stats.Status)
	})

	t.Run("stale schema", func(t *testing.T) {
		dir := writeCacheStatsFixture(t)
		state, err := query.ReadCacheSyncState(dir)
		require.NoError(t, err)
		state.SchemaVersion--
		writeCacheStatsState(t, dir, state)

		stats, err := CollectStats(context.Background(), dir)
		require.NoError(t, err)
		assert.Equal(t, StatusStaleSchema, stats.Status)
	})

	t.Run("drifted", func(t *testing.T) {
		dir := writeCacheStatsFixture(t)
		require.NoError(t, os.WriteFile(filepath.Join(dir, "sources", "drift.parquet"), []byte("drift"), 0o600))

		stats, err := CollectStats(context.Background(), dir)
		require.NoError(t, err)
		assert.Equal(t, StatusDrifted, stats.Status)
	})
}

func writeCacheStatsFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	db, err := sql.Open("duckdb", "")
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	datasets := map[string]string{
		"messages":                  `SELECT 1::BIGINT AS id, 2::BIGINT AS source_id, 100::BIGINT AS size_estimate`,
		"message_recipients":        `SELECT 1::BIGINT AS message_id, 3::BIGINT AS participant_id, 'from'::VARCHAR AS recipient_type`,
		"participants":              `SELECT 3::BIGINT AS id, 'sender@example.com'::VARCHAR AS email_address, 'example.com'::VARCHAR AS domain`,
		"participant_identifiers":   `SELECT 3::BIGINT AS participant_id, 'email'::VARCHAR AS identifier_type, 'sender@example.com'::VARCHAR AS identifier_value`,
		"attachments":               `SELECT 1::BIGINT AS message_id, 25::BIGINT AS size`,
		"sources":                   `SELECT 2::BIGINT AS id`,
		"labels":                    `SELECT 4::BIGINT AS id`,
		"message_labels":            `SELECT 1::BIGINT AS message_id, 4::BIGINT AS label_id`,
		"conversations":             `SELECT 5::BIGINT AS id`,
		"conversation_participants": `SELECT 5::BIGINT AS conversation_id, 3::BIGINT AS participant_id`,
		"owner_participants":        `SELECT 2::BIGINT AS source_id, 3::BIGINT AS participant_id`,
		"participant_clusters":      `SELECT 3::BIGINT AS participant_id, 3::BIGINT AS canonical_id`,
	}
	for dataset, selectSQL := range datasets {
		datasetDir := filepath.Join(dir, dataset)
		if dataset == tableMessages {
			datasetDir = filepath.Join(datasetDir, "year=2024")
		}
		require.NoError(t, os.MkdirAll(datasetDir, 0o755))
		path := strings.ReplaceAll(filepath.Join(datasetDir, "data.parquet"), "'", "''")
		_, err := db.Exec("COPY (" + selectSQL + ") TO '" + path + "' (FORMAT PARQUET)")
		require.NoError(t, err, "write %s fixture", dataset)
	}

	fingerprint, err := query.CacheDatasetFingerprint(dir)
	require.NoError(t, err)
	writeCacheStatsState(t, dir, query.CacheSyncState{
		LastMessageID:      7,
		LastSyncAt:         time.Date(2026, time.July, 15, 12, 0, 0, 0, time.UTC),
		SchemaVersion:      query.CacheSchemaVersion,
		PublishedAt:        time.Date(2026, time.July, 15, 12, 1, 0, 0, time.UTC),
		DatasetFingerprint: fingerprint,
	})
	return dir
}

func writeCacheStatsState(t *testing.T, dir string, state query.CacheSyncState) {
	t.Helper()
	stateData, err := json.Marshal(state)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(query.CacheStatePath(dir), stateData, 0o600))
}
