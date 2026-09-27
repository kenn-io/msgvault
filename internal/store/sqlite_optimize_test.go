package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func plannerMaintenanceRecords(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var records []map[string]any
	for _, record := range decodeAll(t, buf) {
		message, _ := record["msg"].(string)
		if message == "SQLite planner statistics maintenance interrupted" ||
			message == "SQLite planner statistics maintenance failed" {
			records = append(records, record)
		}
	}
	return records
}

type plannerMaintenanceSignalHandler struct {
	slog.Handler

	interrupted chan struct{}
	once        sync.Once
}

func (h *plannerMaintenanceSignalHandler) Handle(ctx context.Context, record slog.Record) error {
	if record.Message == "SQLite planner statistics maintenance interrupted" {
		h.once.Do(func() { close(h.interrupted) })
	}
	if err := h.Handler.Handle(ctx, record); err != nil {
		return fmt.Errorf("handle planner maintenance log: %w", err)
	}
	return nil
}

func TestOptimizeSQLiteCancellationLogsDebug(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	s, err := OpenForTest(filepath.Join(t.TempDir(), "archive.db"))
	require.NoError(err)
	t.Cleanup(func() { _ = s.Close() })
	require.NoError(s.InitSchema())
	s.db.SetMaxOpenConns(1)
	s.db.SetMaxIdleConns(1)
	blocker, err := s.db.Conn(t.Context())
	require.NoError(err)
	t.Cleanup(func() { _ = blocker.Close() })

	buf := captureSlog(t)
	s.optimizeSQLiteBestEffort(context.Background(), "cancellation proof")

	records := plannerMaintenanceRecords(t, buf)
	require.Len(records, 1)
	assert.Equal("DEBUG", records[0]["level"])
	assert.Equal("SQLite planner statistics maintenance interrupted", records[0]["msg"])
	assert.Equal("cancellation proof", records[0]["trigger"])
	errorText, ok := records[0]["error"].(string)
	require.True(ok)
	assert.Contains(errorText, "context deadline exceeded")
}

func TestPlannerMaintenanceLogLevel(t *testing.T) {
	contextDeadlineText := errors.New("context deadline exceeded")
	cases := []struct {
		name      string
		err       error
		wantCount int
		wantLevel string
		wantMsg   string
	}{
		{name: "nil", wantCount: 0},
		{
			name:      "canceled",
			err:       context.Canceled,
			wantCount: 1,
			wantLevel: "DEBUG",
			wantMsg:   "SQLite planner statistics maintenance interrupted",
		},
		{
			name:      "deadline exceeded",
			err:       context.DeadlineExceeded,
			wantCount: 1,
			wantLevel: "DEBUG",
			wantMsg:   "SQLite planner statistics maintenance interrupted",
		},
		{
			name:      "wrapped canceled",
			err:       fmt.Errorf("reserve connection: %w", context.Canceled),
			wantCount: 1,
			wantLevel: "DEBUG",
			wantMsg:   "SQLite planner statistics maintenance interrupted",
		},
		{
			name:      "wrapped deadline exceeded",
			err:       fmt.Errorf("reserve connection: %w", context.DeadlineExceeded),
			wantCount: 1,
			wantLevel: "DEBUG",
			wantMsg:   "SQLite planner statistics maintenance interrupted",
		},
		{
			name:      "same text unrelated error",
			err:       contextDeadlineText,
			wantCount: 1,
			wantLevel: "WARN",
			wantMsg:   "SQLite planner statistics maintenance failed",
		},
		{
			name:      "wrapped unrelated error",
			err:       fmt.Errorf("database failure: %w", errors.New("database is closed")),
			wantCount: 1,
			wantLevel: "WARN",
			wantMsg:   "SQLite planner statistics maintenance failed",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			buf := captureSlog(t)
			logSQLiteOptimizeError("classifier", tc.err)

			records := plannerMaintenanceRecords(t, buf)
			require := require.New(t)
			assert := assert.New(t)
			require.Len(records, tc.wantCount)
			for _, record := range records {
				assert.Equal(tc.wantLevel, record["level"])
				assert.Equal(tc.wantMsg, record["msg"])
				assert.Equal("classifier", record["trigger"])
				if tc.err != nil {
					assert.Equal(tc.err.Error(), record["error"])
				}
			}
		})
	}
}

func TestPlannerMaintenanceDatabaseErrorWarns(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	s, err := OpenForTest(filepath.Join(t.TempDir(), "archive.db"))
	require.NoError(err)
	require.NoError(s.db.Close())
	actualErr := s.optimizeSQLite(t.Context())
	require.Error(actualErr)

	buf := captureSlog(t)
	s.optimizeSQLiteBestEffort(t.Context(), "closed database")

	records := plannerMaintenanceRecords(t, buf)
	require.Len(records, 1)
	assert.Equal("WARN", records[0]["level"])
	assert.Equal("SQLite planner statistics maintenance failed", records[0]["msg"])
	assert.Equal("closed database", records[0]["trigger"])
	assert.Equal(actualErr.Error(), records[0]["error"])
}

func TestStoreCloseDatabaseErrorWarnsAndRunsCleanup(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	s, err := OpenForTest(filepath.Join(t.TempDir(), "archive.db"))
	require.NoError(err)
	cleaned := false
	s.closeCleanup = func() { cleaned = true }
	require.NoError(s.db.Close())
	_, actualErr := s.db.DB.ExecContext(context.Background(), "PRAGMA optimize=0x10002")
	require.Error(actualErr)

	buf := captureSlog(t)
	closeErr := s.Close()
	require.NoError(closeErr)
	assert.True(cleaned)

	records := plannerMaintenanceRecords(t, buf)
	require.Len(records, 1)
	assert.Equal("WARN", records[0]["level"])
	assert.Equal("SQLite planner statistics maintenance failed", records[0]["msg"])
	assert.Equal("store close", records[0]["trigger"])
	assert.Equal(actualErr.Error(), records[0]["error"])
}

func TestStoreCloseCancellationLogsDebugAndRunsCleanup(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	s, err := OpenForTest(filepath.Join(t.TempDir(), "archive.db"))
	require.NoError(err)
	require.NoError(s.InitSchema())
	s.db.SetMaxOpenConns(1)
	s.db.SetMaxIdleConns(1)
	blocker, err := s.db.Conn(t.Context())
	require.NoError(err)
	t.Cleanup(func() { _ = blocker.Close() })

	cleaned := false
	s.closeCleanup = func() { cleaned = true }
	var buf bytes.Buffer
	previous := slog.Default()
	signal := &plannerMaintenanceSignalHandler{
		Handler:     slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}),
		interrupted: make(chan struct{}),
	}
	slog.SetDefault(slog.New(signal))
	t.Cleanup(func() { slog.SetDefault(previous) })
	closeDone := make(chan error, 1)
	go func() { closeDone <- s.Close() }()

	select {
	case err := <-closeDone:
		require.FailNow("Close returned before its maintenance deadline", err)
	case <-signal.interrupted:
	case <-time.After(5 * time.Second):
		require.NoError(blocker.Close())
		<-closeDone
		require.FailNow("Close did not log interrupted maintenance")
	}
	require.NoError(blocker.Close())
	require.NoError(<-closeDone)

	records := plannerMaintenanceRecords(t, &buf)
	require.Len(records, 1)
	assert.Equal("DEBUG", records[0]["level"])
	assert.Equal("SQLite planner statistics maintenance interrupted", records[0]["msg"])
	assert.Equal("store close", records[0]["trigger"])
	assert.True(cleaned)
	for _, record := range decodeAll(t, &buf) {
		assert.False(record["msg"] == "sql error" && record["level"] == "WARN")
	}
}

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
		var analysisLimit int
		require.NoError(conn.QueryRowContext(t.Context(), "PRAGMA analysis_limit").Scan(&analysisLimit))
		assert.Equal(1000, analysisLimit, "every connection must keep ANALYZE bounded")
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

func TestSuccessfulSyncOptimizeThrottled(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	s, err := OpenForTest(filepath.Join(t.TempDir(), "archive.db"))
	require.NoError(err)
	defer func() { _ = s.Close() }()
	require.NoError(s.InitSchema())
	seedLiveMessages(t, s, 100)
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	s.syncOptimizeNow = func() time.Time { return now }
	runs := 0
	s.syncOptimizeHook = func() { runs++ }

	completeTestSync := func() {
		syncID, err := s.StartSync(1, "full")
		require.NoError(err)
		require.NoError(s.CompleteSyncAndUpdateSourceCursor(syncID, 1, "cursor"))
	}
	completeTestSync()
	assert.Equal(1, runs, "the first successful sync runs planner maintenance")
	assert.Positive(messagePlannerStatisticCount(t, s))

	now = now.Add(time.Hour)
	completeTestSync()
	assert.Equal(1, runs, "syncs within the throttle interval skip planner maintenance")

	now = now.Add(syncOptimizeInterval)
	completeTestSync()
	assert.Equal(2, runs, "maintenance resumes once the interval elapses")
}

func TestDailyMaintenanceOptimizesAndTruncatesWAL(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	s, err := OpenForTest(filepath.Join(t.TempDir(), "archive.db"))
	require.NoError(err)
	defer func() { _ = s.Close() }()
	require.NoError(s.InitSchema())
	seedLiveMessages(t, s, 100)

	report, err := s.RunDailyMaintenance(t.Context())
	require.NoError(err)
	assert.NoError(report.OptimizeErr)
	assert.Equal(1, report.CheckpointAttempts)
	assert.Positive(report.WALBytesBefore, "seeded writes leave WAL frames")
	assert.Zero(report.WALBytesAfter, "TRUNCATE resets the WAL")
	assert.Positive(messagePlannerStatisticCount(t, s))
}

func TestDailyMaintenanceRetriesBusyCheckpoint(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	dbPath := filepath.Join(t.TempDir(), "archive.db")

	s, err := OpenForTest(dbPath)
	require.NoError(err)
	defer func() { _ = s.Close() }()
	require.NoError(s.InitSchema())
	seedLiveMessages(t, s, 10)
	s.checkpointRetryBackoff = []time.Duration{0, 0}

	// A reader outside the store's pool pins an old snapshot, so later WAL
	// frames cannot be checkpointed and TRUNCATE reports busy.
	reader, err := sql.Open("sqlite3", dbPath)
	require.NoError(err)
	defer func() { _ = reader.Close() }()
	readerTx, err := reader.Begin()
	require.NoError(err)
	defer func() { _ = readerTx.Rollback() }()
	var n int
	require.NoError(readerTx.QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&n))
	_, err = s.db.DB.Exec(`UPDATE messages SET snippet = 'changed after the reader snapshot'`)
	require.NoError(err)

	buf := captureSlog(t)
	report, err := s.RunDailyMaintenance(t.Context())
	require.Error(err)
	assert.Equal(3, report.CheckpointAttempts, "busy checkpoints are retried")
	assert.Contains(buf.String(), "SQLite WAL checkpoint failed after retries")
}

func TestCheckpointWALContextInterruptsBusyCheckpoint(t *testing.T) {
	require := require.New(t)
	dbPath := filepath.Join(t.TempDir(), "archive.db")
	s, err := OpenForTest(dbPath)
	require.NoError(err)
	defer func() { _ = s.Close() }()
	require.NoError(s.InitSchema())
	seedLiveMessages(t, s, 10)

	reader, err := sql.Open("sqlite3", dbPath)
	require.NoError(err)
	defer func() { _ = reader.Close() }()
	readerTx, err := reader.Begin()
	require.NoError(err)
	defer func() { _ = readerTx.Rollback() }()
	var n int
	require.NoError(readerTx.QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&n))
	_, err = s.db.DB.Exec(`UPDATE messages SET snippet = 'changed after the reader snapshot'`)
	require.NoError(err)

	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	started := time.Now()
	err = s.CheckpointWALContext(ctx)
	assert := assert.New(t)
	require.Error(err)
	assert.Less(time.Since(started), time.Second,
		"busy checkpoint returns within its bounded timeout")
}

func TestCheckpointWALPassiveContextHonorsDeadlineWaitingForPool(t *testing.T) {
	require := require.New(t)
	dbPath := filepath.Join(t.TempDir(), "archive.db")
	s, err := OpenForTest(dbPath)
	require.NoError(err)
	defer func() { _ = s.Close() }()
	require.NoError(s.InitSchema())
	s.db.SetMaxOpenConns(1)

	held, err := s.db.Conn(t.Context())
	require.NoError(err)
	defer func() { _ = held.Close() }()

	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	err = s.CheckpointWALPassive(ctx)
	require.ErrorIs(err, context.DeadlineExceeded)
}

func TestSQLiteContentionErrorsLogAtDebug(t *testing.T) {
	for _, err := range []error{
		sqlite3.Error{Code: sqlite3.ErrBusy},
		fmt.Errorf("optimize: %w", sqlite3.Error{Code: sqlite3.ErrInterrupt}),
		sqlite3.Error{Code: sqlite3.ErrLocked},
	} {
		buf := captureSlog(t)
		logSQLiteOptimizeError("contention", err)
		records := plannerMaintenanceRecords(t, buf)
		require.Len(t, records, 1)
		assert.Equal(t, "DEBUG", records[0]["level"], "%v is expected contention", err)
	}
}
