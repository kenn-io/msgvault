package cmd

import (
	"crypto/sha256"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/identityindex"
	"go.kenn.io/msgvault/internal/store"
)

func TestCacheIdentityDriftWithAppendPreservesMessageShards(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	tmp := setupTestSQLite(t)
	dbPath := filepath.Join(tmp, "test.db")
	analyticsDir := filepath.Join(tmp, "analytics")
	_, err := buildCache(dbPath, analyticsDir, true)
	require.NoError(err)
	before := snapshotMessagesDatasetBytes(t, analyticsDir)
	st, err := store.Open(dbPath)
	require.NoError(err)
	_, err = st.LinkParticipants(3, 4)
	require.NoError(err)
	_, err = st.DB().Exec(`INSERT INTO messages
		(id, source_id, source_message_id, conversation_id, message_type, sent_at, sender_id)
		VALUES (6, 1, 'message-6', 101, 'email', '2024-03-02 10:00:00', 3)`)
	require.NoError(err)
	require.NoError(st.Close())
	stale := cacheNeedsBuild(dbPath, analyticsDir)
	assert.True(stale.HasIdentityDrift)
	assert.True(stale.HasNew)
	assert.False(stale.FullRebuild, "identity links do not change baked message facts")
	result, err := buildCacheAuto(dbPath, analyticsDir)
	require.NoError(err)
	assert.Equal(int64(1), result.StagedCount)
	after := snapshotMessagesDatasetBytes(t, analyticsDir)
	for path, contents := range before {
		assert.Equal(sha256.Sum256([]byte(contents)), sha256.Sum256([]byte(after[path])), "retain message shard %s", path)
	}
	assert.False(cacheNeedsBuild(dbPath, analyticsDir).NeedsBuild)

	fullDir := filepath.Join(tmp, "full")
	_, err = buildCache(dbPath, fullDir, true)
	require.NoError(err)
	duckDB, err := sql.Open("duckdb", "")
	require.NoError(err)
	defer func() { require.NoError(duckDB.Close()) }()
	for _, dataset := range []string{identityindex.DatasetActivity, identityindex.DatasetLogicalContributions,
		identityindex.DatasetTemperatureContributions, identityindex.DatasetRelationshipDaily, identityindex.DatasetDomains} {
		pattern := func(root string) string {
			return "read_parquet('" + strings.ReplaceAll(filepath.ToSlash(filepath.Join(root, dataset, "**", "*.parquet")), "'", "''") + "')"
		}
		actual, want := pattern(analyticsDir), pattern(fullDir)
		var differences int64
		err := duckDB.QueryRow(`SELECT COUNT(*) FROM (
			(SELECT * FROM ` + actual + ` EXCEPT ALL SELECT * FROM ` + want + `)
			UNION ALL
			(SELECT * FROM ` + want + ` EXCEPT ALL SELECT * FROM ` + actual + `))`).Scan(&differences)
		require.NoError(err, dataset)
		assert.Zero(differences, dataset)
	}
}
