package cmd

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func explainQueryPlan(t *testing.T, s *store.Store, sql string, args ...any) string {
	t.Helper()
	rows, err := s.DB().Query("EXPLAIN QUERY PLAN "+sql, args...)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	var b strings.Builder
	for rows.Next() {
		var id, parent, notUsed int
		var detail string
		require.NoError(t, rows.Scan(&id, &parent, &notUsed, &detail))
		b.WriteString(detail)
		b.WriteString("\n")
	}
	require.NoError(t, rows.Err())
	return b.String()
}

// TestCacheStalenessCounts_UseDeletionIndexes verifies the two deletion-status
// COUNTs in cacheNeedsBuild are served by the partial deletion indexes instead
// of full scans of the messages table. These queries run on every daemon start
// before the API server binds, so a full scan on a cold page cache adds
// multiple seconds to every cold-start CLI command on a large archive.
func TestCacheStalenessCounts_UseDeletionIndexes(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	// The Parquet cache staleness check is a SQLite-only ETL path;
	// cacheNeedsBuild returns early for PostgreSQL DSNs.
	s := testutil.NewSQLiteTestStore(t)

	_, err := s.DB().Exec(
		`INSERT INTO sources (id, source_type, identifier) VALUES (1, 'gmail', 'user@example.com')`)
	require.NoError(err)
	_, err = s.DB().Exec(`INSERT INTO conversations
		(id, source_id, source_conversation_id, conversation_type)
		VALUES (1, 1, 'conv1', 'email')`)
	require.NoError(err)
	_, err = s.DB().Exec(`INSERT INTO messages
		(id, conversation_id, source_id, source_message_id, message_type, sent_at, deleted_from_source_at, deleted_at)
		VALUES
		(1, 1, 1, 'm1', 'email', '2025-01-01 00:00:00', NULL, NULL),
		(2, 1, 1, 'm2', 'email', '2025-01-02 00:00:00', '2025-06-01 00:00:00', NULL),
		(3, 1, 1, 'm3', 'email', '2025-01-03 00:00:00', NULL, '2025-06-02 00:00:00')`)
	require.NoError(err)

	for _, name := range []string{"idx_messages_deleted_from_source_at", "idx_messages_deleted_at"} {
		var idxCount int
		require.NoError(s.DB().QueryRow(
			`SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name = ?`, name,
		).Scan(&idxCount))
		assert.Equal(1, idxCount, "%s should be created by InitSchema", name)
	}

	deletedPlan := explainQueryPlan(t, s, deletedSinceBuildCountSQL(), "2025-05-01 00:00:00")
	assert.Contains(deletedPlan, "idx_messages_deleted_from_source_at",
		"source-deleted COUNT should use the partial index, not a full scan:\n%s", deletedPlan)
	assert.NotContains(deletedPlan, "SCAN messages", deletedPlan)

	hiddenPlan := explainQueryPlan(t, s, hiddenSinceBuildCountSQL(), "2025-05-01 00:00:00")
	assert.Contains(hiddenPlan, "idx_messages_deleted_at",
		"dedup-hidden COUNT should use the partial index, not a full scan:\n%s", hiddenPlan)
	assert.NotContains(hiddenPlan, "SCAN messages", hiddenPlan)

	// The queries still return correct counts through the indexes.
	var deleted, hidden int64
	require.NoError(s.DB().QueryRow(deletedSinceBuildCountSQL(), "2025-05-01 00:00:00").Scan(&deleted))
	require.NoError(s.DB().QueryRow(hiddenSinceBuildCountSQL(), "2025-05-01 00:00:00").Scan(&hidden))
	assert.Equal(int64(1), deleted)
	assert.Equal(int64(1), hidden)
}
