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

func TestOptimizeSQLiteSerializesConcurrentMaintenance(t *testing.T) {
	require := require.New(t)

	s, err := OpenForTest(filepath.Join(t.TempDir(), "archive.db"))
	require.NoError(err)
	defer func() { _ = s.Close() }()
	require.NoError(s.InitSchema())

	const poolSize = 4
	s.db.SetMaxOpenConns(poolSize)
	s.db.SetMaxIdleConns(poolSize)
	blockers := make([]*sql.Conn, 0, poolSize-1)
	for range poolSize - 1 {
		conn, connErr := s.db.Conn(t.Context())
		require.NoError(connErr)
		blockers = append(blockers, conn)
	}

	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	started := make(chan struct{}, poolSize)
	results := make(chan error, poolSize)
	for range poolSize {
		go func() {
			started <- struct{}{}
			results <- s.optimizeSQLite(ctx)
		}()
	}
	for range poolSize {
		<-started
	}
	require.Eventually(func() bool {
		stats := s.db.Stats()
		return stats.InUse == poolSize && stats.WaitCount > 0
	}, time.Second, time.Millisecond)
	// Give every unguarded call time to queue behind the exhausted pool.
	time.Sleep(25 * time.Millisecond)
	for _, conn := range blockers {
		require.NoError(conn.Close())
	}

	for range poolSize {
		require.NoError(<-results)
	}
}
