package store

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/sqliteutil"
)

func openSQLiteSizeTestDB(t *testing.T) (*sql.DB, string) {
	t.Helper()

	dbPath := filepath.Join(t.TempDir(), "size.db")
	db, err := sql.Open(
		sqliteutil.DriverName(),
		dbPath+"?_journal_mode=WAL&_busy_timeout=5000&_synchronous=OFF",
	)
	require.NoError(t, err, "open SQLite database")
	t.Cleanup(func() {
		_ = db.Close()
	})
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	require.NoError(t, db.PingContext(context.Background()), "ping SQLite database")
	return db, dbPath
}

func sqliteLogicalMainSize(t *testing.T, db *sql.DB) int64 {
	t.Helper()

	var pageCount int64
	require.NoError(t, db.QueryRow("PRAGMA page_count").Scan(&pageCount), "read page count")
	var pageSize int64
	require.NoError(t, db.QueryRow("PRAGMA page_size").Scan(&pageSize), "read page size")
	return pageCount * pageSize
}

func TestSQLiteDatabaseSizeReportsLogicalMainDatabasePages(t *testing.T) {
	db, dbPath := openSQLiteSizeTestDB(t)
	_, err := db.Exec(`CREATE TABLE payload (value BLOB NOT NULL)`)
	require.NoError(t, err, "create payload table")

	var busy, logFrames, checkpointedFrames int
	require.NoError(t, db.QueryRow("PRAGMA wal_checkpoint(TRUNCATE)").Scan(
		&busy, &logFrames, &checkpointedFrames,
	), "checkpoint schema")
	require.Zero(t, busy, "checkpoint busy")
	_, err = db.Exec("PRAGMA wal_autocheckpoint = 0")
	require.NoError(t, err, "disable automatic WAL checkpoint")

	_, err = db.Exec(`INSERT INTO payload(value) VALUES (zeroblob(2097152))`)
	require.NoError(t, err, "insert WAL-resident payload")

	mainInfo, err := os.Stat(dbPath)
	require.NoError(t, err, "stat main database")
	walInfo, err := os.Stat(dbPath + "-wal")
	require.NoError(t, err, "stat WAL")
	require.Positive(t, walInfo.Size(), "WAL contains committed pages")

	want := sqliteLogicalMainSize(t, db)
	require.Greater(t, want, mainInfo.Size(),
		"logical page allocation must include committed growth not checkpointed into the main file")

	got, err := (&SQLiteDialect{}).DatabaseSize(context.Background(), db, dbPath)
	require.NoError(t, err, "DatabaseSize")
	assert.Equal(t, want, got)
	assert.NotEqual(t, mainInfo.Size()+walInfo.Size(), got,
		"logical main-database size excludes WAL framing and sidecar overhead")
}

func TestSQLiteDatabaseSizeContextCancelsConnectionWait(t *testing.T) {
	db, dbPath := openSQLiteSizeTestDB(t)

	held, err := db.Conn(context.Background())
	require.NoError(t, err, "hold only SQLite connection")
	defer func() { _ = held.Close() }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type result struct {
		size int64
		err  error
	}
	done := make(chan result, 1)
	go func() {
		size, sizeErr := (&SQLiteDialect{}).DatabaseSize(ctx, db, dbPath)
		done <- result{size: size, err: sizeErr}
	}()

	select {
	case early := <-done:
		require.FailNowf(t, "DatabaseSize bypassed context-aware SQL",
			"returned before the only database connection was available: size=%d err=%v",
			early.size, early.err)
	case <-time.After(25 * time.Millisecond):
	}

	cancel()
	select {
	case got := <-done:
		assert.Zero(t, got.size)
		require.ErrorIs(t, got.err, context.Canceled)
	case <-time.After(time.Second):
		require.FailNow(t, "DatabaseSize did not stop after context cancellation")
	}
}

func TestSQLiteDatabaseSizeReportsQueryError(t *testing.T) {
	db, dbPath := openSQLiteSizeTestDB(t)
	require.NoError(t, db.Close(), "close SQLite database")

	got, err := (&SQLiteDialect{}).DatabaseSize(context.Background(), db, dbPath)
	assert.Zero(t, got)
	require.Error(t, err)
	assert.ErrorContains(t, err, "query SQLite page count")
}
