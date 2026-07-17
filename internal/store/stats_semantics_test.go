package store_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

// setDeletedFromSource marks a message as deleted from its source account
// (retained in the archive).
func setDeletedFromSource(f *storetest.Fixture, id int64) {
	f.T.Helper()
	_, err := f.Store.DB().Exec(
		f.Store.Rebind("UPDATE messages SET deleted_from_source_at = CURRENT_TIMESTAMP WHERE id = ?"),
		id,
	)
	require.NoError(f.T, err, "set deleted_from_source_at")
}

// setDedupHidden marks a message as a dedup loser (deleted_at), which every
// user-facing count must exclude regardless of the source-deletion split.
func setDedupHidden(f *storetest.Fixture, id int64) {
	f.T.Helper()
	_, err := f.Store.DB().Exec(
		f.Store.Rebind("UPDATE messages SET deleted_at = CURRENT_TIMESTAMP WHERE id = ?"),
		id,
	)
	require.NoError(f.T, err, "set deleted_at")
}

// TestStore_StatsSourceDeletedBreakdown asserts that GetStats splits the
// archived population into active and source-deleted counts, that the two are
// exact complements within the non-dedup-hidden set, and that dedup-hidden
// rows are excluded from both.
func TestStore_StatsSourceDeletedBreakdown(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)

	f.CreateMessages(5) // 5 active messages

	sourceDeleted := []int64{
		f.CreateMessage("src-del-1"),
		f.CreateMessage("src-del-2"),
		f.CreateMessage("src-del-3"),
	}
	for _, id := range sourceDeleted {
		setDeletedFromSource(f, id)
	}

	dedup := f.CreateMessage("dedup-hidden-1")
	setDedupHidden(f, dedup)

	stats, err := f.Store.GetStats()
	require.NoError(err, "GetStats")

	assert.Equal(int64(5), stats.MessageCount, "active MessageCount")
	assert.Equal(int64(3), stats.SourceDeletedCount, "SourceDeletedCount")

	// Canonical archived total excludes the dedup-hidden row.
	assert.Equal(int64(8), stats.MessageCount+stats.SourceDeletedCount, "canonical total")

	activeCount, err := f.Store.CountActiveMessages()
	require.NoError(err, "CountActiveMessages")
	deletedCount, err := f.Store.CountSourceDeletedMessages()
	require.NoError(err, "CountSourceDeletedMessages")
	assert.Equal(stats.MessageCount, activeCount, "CountActiveMessages matches stats")
	assert.Equal(stats.SourceDeletedCount, deletedCount, "CountSourceDeletedMessages matches stats")
}

// TestStore_CollectionSourceDeletedCount asserts the collection aggregate
// carries the same active/source-deleted split as global stats.
func TestStore_CollectionSourceDeletedCount(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)

	f.CreateMessages(4)
	del := f.CreateMessage("coll-src-del-1")
	setDeletedFromSource(f, del)

	_, err := f.Store.CreateCollection("team", "", []int64{f.Source.ID})
	require.NoError(err, "CreateCollection")

	coll, err := f.Store.GetCollectionByName("team")
	require.NoError(err, "GetCollectionByName")
	require.NotNil(coll, "collection")

	assert.Equal(int64(4), coll.MessageCount, "collection active count")
	assert.Equal(int64(1), coll.SourceDeletedCount, "collection source-deleted count")
}
