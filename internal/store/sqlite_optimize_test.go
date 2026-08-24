package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

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

func TestSyncCheckpointDoesNotDrainSQLitePool(t *testing.T) {
	require := require.New(t)

	s, err := OpenForTest(filepath.Join(t.TempDir(), "archive.db"))
	require.NoError(err)
	defer func() { _ = s.Close() }()
	require.NoError(s.InitSchema())
	s.db.SetMaxOpenConns(2)
	s.db.SetMaxIdleConns(2)
	seedLiveMessages(t, s, 1)

	syncID, err := s.StartSync(1, "full")
	require.NoError(err)
	blocker, err := s.db.Conn(t.Context())
	require.NoError(err)

	checkpointDone := make(chan error, 1)
	go func() {
		checkpointDone <- s.UpdateSyncCheckpointContext(
			context.Background(), syncID, &Checkpoint{MessagesProcessed: 100},
		)
	}()
	select {
	case checkpointErr := <-checkpointDone:
		require.NoError(checkpointErr)
	case <-time.After(250 * time.Millisecond):
		require.NoError(blocker.Close())
		<-checkpointDone
		require.FailNow("sync checkpoint waited to reserve the SQLite pool")
	}
	require.NoError(blocker.Close())
}

func TestOptimizeSQLiteReloadsEveryPooledConnection(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	s, err := OpenForTest(filepath.Join(t.TempDir(), "archive.db"))
	require.NoError(err)
	defer func() { _ = s.Close() }()
	s.db.SetMaxOpenConns(2)
	s.db.SetMaxIdleConns(2)
	require.NoError(s.InitSchema())
	seedLiveMessages(t, s, 10_000)

	args := make([]any, 51)
	args[0] = 1
	for i := 1; i < len(args); i++ {
		args[i] = strconv.Itoa(i - 1)
	}
	query := `EXPLAIN QUERY PLAN
		SELECT id FROM messages
		WHERE source_id = ? AND source_message_id IN (` +
		strings.TrimSuffix(strings.Repeat("?,", len(args)-1), ",") + `)
		ORDER BY id`
	plan := func(conn *sql.Conn) string {
		rows, queryErr := conn.QueryContext(t.Context(), query, args...)
		require.NoError(queryErr)
		defer func() { require.NoError(rows.Close()) }()
		var details strings.Builder
		for rows.Next() {
			var id, parent, unused int
			var detail string
			require.NoError(rows.Scan(&id, &parent, &unused, &detail))
			details.WriteString(detail)
			details.WriteByte('\n')
		}
		require.NoError(rows.Err())
		return details.String()
	}

	first, err := s.db.Conn(t.Context())
	require.NoError(err)
	second, err := s.db.Conn(t.Context())
	require.NoError(err)
	for _, conn := range []*sql.Conn{first, second} {
		assert.Contains(plan(conn), "idx_messages_source (source_id=?)",
			"the fixture must preload the statistics-free plan on every connection")
	}
	require.NoError(first.Close())
	require.NoError(second.Close())

	require.NoError(s.optimizeSQLite(t.Context()))

	first, err = s.db.Conn(t.Context())
	require.NoError(err)
	second, err = s.db.Conn(t.Context())
	require.NoError(err)
	defer func() { require.NoError(first.Close()) }()
	defer func() { require.NoError(second.Close()) }()
	for _, conn := range []*sql.Conn{first, second} {
		refreshedPlan := plan(conn)
		assert.Contains(refreshedPlan, "(source_id=? AND source_message_id=?)",
			"every pooled connection must use the refreshed planner statistics")
		assert.NotContains(refreshedPlan, "idx_messages_source (source_id=?)",
			"no pooled connection may retain the statistics-free plan")
	}
}

func TestOptimizeSQLiteSkipsConcurrentMaintenance(t *testing.T) {
	require := require.New(t)

	s, err := OpenForTest(filepath.Join(t.TempDir(), "archive.db"))
	require.NoError(err)
	defer func() { _ = s.Close() }()
	require.NoError(s.InitSchema())

	s.db.SetMaxOpenConns(2)
	s.db.SetMaxIdleConns(2)
	blocker, err := s.db.Conn(t.Context())
	require.NoError(err)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	baselineWaitCount := s.db.Stats().WaitCount
	firstDone := make(chan error, 1)
	go func() { firstDone <- s.optimizeSQLite(ctx) }()
	require.Eventually(func() bool {
		stats := s.db.Stats()
		return stats.InUse == 2 && stats.WaitCount > baselineWaitCount
	}, time.Second, time.Millisecond)

	canceledCtx, cancelDuplicate := context.WithCancel(t.Context())
	cancelDuplicate()
	duplicateDone := make(chan error, 1)
	go func() { duplicateDone <- s.optimizeSQLite(canceledCtx) }()
	select {
	case duplicateErr := <-duplicateDone:
		require.NoError(duplicateErr)
	case <-time.After(250 * time.Millisecond):
		cancel()
		require.NoError(blocker.Close())
		<-firstDone
		<-duplicateDone
		require.FailNow("duplicate maintenance waited for the active call")
	}

	require.NoError(blocker.Close())
	require.NoError(<-firstDone)
}

func TestOptimizeSQLiteBoundsPoolReservation(t *testing.T) {
	require := require.New(t)

	s, err := OpenForTest(filepath.Join(t.TempDir(), "archive.db"))
	require.NoError(err)
	defer func() { _ = s.Close() }()
	require.NoError(s.InitSchema())
	s.db.SetMaxOpenConns(2)
	s.db.SetMaxIdleConns(2)
	blocker, err := s.db.Conn(t.Context())
	require.NoError(err)

	result := make(chan error, 1)
	go func() { result <- s.optimizeSQLite(context.Background()) }()
	select {
	case optimizeErr := <-result:
		require.ErrorIs(optimizeErr, context.DeadlineExceeded)
	case <-time.After(2 * time.Second):
		require.NoError(blocker.Close())
		<-result
		require.FailNow("planner maintenance did not bound pool reservation")
	}
	require.NoError(blocker.Close())
}

func TestCloseOptimizesWithoutDrainingSQLitePool(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	dbPath := filepath.Join(t.TempDir(), "archive.db")

	s, err := OpenForTest(dbPath)
	require.NoError(err)
	require.NoError(s.InitSchema())
	seedLiveMessages(t, s, 100)
	assert.Zero(messagePlannerStatisticCount(t, s))
	s.db.SetMaxOpenConns(2)
	s.db.SetMaxIdleConns(2)
	blocker, err := s.db.Conn(t.Context())
	require.NoError(err)

	closeDone := make(chan error, 1)
	go func() { closeDone <- s.Close() }()
	select {
	case closeErr := <-closeDone:
		require.NoError(closeErr)
	case <-time.After(2 * time.Second):
		require.NoError(blocker.Close())
		<-closeDone
		require.FailNow("store close waited to reserve the entire SQLite pool")
	}
	require.NoError(blocker.Close())

	reopened, err := OpenForTest(dbPath)
	require.NoError(err)
	defer func() { _ = reopened.Close() }()
	assert.Positive(messagePlannerStatisticCount(t, reopened),
		"store close must persist planner statistics")
}
