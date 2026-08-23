package store

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func messagePlannerStatisticCount(t *testing.T, s *Store) int {
	t.Helper()
	var count int
	require.NoError(t, s.db.QueryRow(`
		SELECT COUNT(*)
		FROM sqlite_stat1
		WHERE tbl = 'messages'
	`).Scan(&count))
	return count
}

func TestInitSchemaRefreshesSQLitePlannerStatistics(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	s, err := OpenForTest(filepath.Join(t.TempDir(), "archive.db"))
	require.NoError(err)
	defer func() { _ = s.Close() }()
	require.NoError(s.InitSchema())

	seedLiveMessages(t, s, 100)
	assert.Zero(messagePlannerStatisticCount(t, s),
		"message statistics must be absent before the maintenance boundary")

	require.NoError(s.InitSchema())
	assert.Positive(messagePlannerStatisticCount(t, s),
		"schema initialization must analyze populated message indexes")
}

func TestCompleteSyncAndUpdateSourceCursorRefreshesSQLitePlannerStatistics(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	s, err := OpenForTest(filepath.Join(t.TempDir(), "archive.db"))
	require.NoError(err)
	defer func() { _ = s.Close() }()
	require.NoError(s.InitSchema())

	seedLiveMessages(t, s, 100)
	assert.Zero(messagePlannerStatisticCount(t, s),
		"message statistics must be absent before the maintenance boundary")

	syncID, err := s.StartSync(1, "full")
	require.NoError(err)
	require.NoError(s.CompleteSyncAndUpdateSourceCursor(syncID, 1, "cursor-1"))
	assert.Positive(messagePlannerStatisticCount(t, s),
		"successful sync must analyze populated message indexes")
}

func TestSyncCheckpointRefreshesSQLitePlannerStatistics(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	s, err := OpenForTest(filepath.Join(t.TempDir(), "archive.db"))
	require.NoError(err)
	defer func() { _ = s.Close() }()
	require.NoError(s.InitSchema())

	seedLiveMessages(t, s, 100)
	assert.Zero(messagePlannerStatisticCount(t, s),
		"message statistics must be absent before the maintenance boundary")

	syncID, err := s.StartSync(1, "full")
	require.NoError(err)
	require.NoError(s.UpdateSyncCheckpoint(syncID, &Checkpoint{MessagesProcessed: 100}))
	assert.Positive(messagePlannerStatisticCount(t, s),
		"sync checkpoints must analyze populated message indexes")
}
