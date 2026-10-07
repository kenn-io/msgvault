package cmd

import (
	"context"
	"database/sql"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/imessage"
	"go.kenn.io/msgvault/internal/testutil"
)

func newImessageFixture(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "chat.db")
	db, err := sql.Open("sqlite3", path+"?_journal_mode=WAL")
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	_, err = db.Exec(`
 CREATE TABLE message (
  guid TEXT NOT NULL DEFAULT 'synthetic-message', text TEXT, attributedBody BLOB,
  date INTEGER NOT NULL, is_from_me INTEGER NOT NULL DEFAULT 0,
  service TEXT DEFAULT 'SMS', cache_has_attachments INTEGER NOT NULL DEFAULT 0,
  handle_id INTEGER DEFAULT 1
 );
 CREATE TABLE handle (id TEXT);
 CREATE TABLE chat (guid TEXT, display_name TEXT, chat_identifier TEXT);
 CREATE TABLE chat_message_join (chat_id INTEGER, message_id INTEGER);
 CREATE TABLE chat_handle_join (chat_id INTEGER, handle_id INTEGER);
 INSERT INTO handle (id) VALUES ('peer@example.test');
 INSERT INTO message (text, date) VALUES ('hello', 700000000000000000);
 `)
	require.NoError(t, err)
	return path
}

func TestImportImessageRecordsOneSyncRun(t *testing.T) {
	require, assert := require.New(t), assert.New(t)
	st := testutil.NewTestStore(t)
	client, err := imessage.NewClient(newImessageFixture(t))
	require.NoError(err)
	t.Cleanup(func() { _ = client.Close() })
	src, err := resolveImessageSource(st)
	require.NoError(err)

	_, err = importImessageRecorded(context.Background(), st, client, src.ID)
	require.NoError(err)

	var total, completed int
	require.NoError(st.DB().QueryRow(st.Rebind(
		`SELECT COUNT(*), COALESCE(SUM(CASE WHEN status='completed' THEN 1 ELSE 0 END),0) FROM sync_runs WHERE source_id = ?`),
		src.ID).Scan(&total, &completed))
	assert.Equal(1, total)
	assert.Equal(1, completed)
}

// TestImportImessageParallelNeverOverlaps runs many imports at once (the
// shape of N API triggers plus M relayed CLI imports) and checks that no two
// ever hold a running sync_runs row for the source, and that every import
// that ran left exactly one row.
func TestImportImessageParallelNeverOverlaps(t *testing.T) {
	require, assert := require.New(t), assert.New(t)
	st := testutil.NewTestStore(t)
	path := newImessageFixture(t)
	src, err := resolveImessageSource(st)
	require.NoError(err)

	stop := make(chan struct{})
	var maxRunning atomic.Int64
	monitorDone := make(chan struct{})
	go func() {
		defer close(monitorDone)
		for {
			select {
			case <-stop:
				return
			case <-time.After(time.Millisecond):
			}
			var n int64
			if err := st.DB().QueryRow(st.Rebind(
				`SELECT COUNT(*) FROM sync_runs WHERE source_id = ? AND status = 'running'`), src.ID).Scan(&n); err == nil && n > maxRunning.Load() {
				maxRunning.Store(n)
			}
		}
	}()

	const workers = 8
	var wg sync.WaitGroup
	var succeeded atomic.Int64
	for range workers {
		wg.Go(func() {
			client, err := imessage.NewClient(path)
			if err != nil {
				return
			}
			defer func() { _ = client.Close() }()
			if _, err := importImessageRecorded(context.Background(), st, client, src.ID); err == nil {
				succeeded.Add(1)
			}
		})
	}
	wg.Wait()
	close(stop)
	<-monitorDone

	assert.LessOrEqual(maxRunning.Load(), int64(1), "at most one running sync_runs row per source")
	var running, completed int
	require.NoError(st.DB().QueryRow(st.Rebind(
		`SELECT COALESCE(SUM(CASE WHEN status='running' THEN 1 ELSE 0 END),0), COALESCE(SUM(CASE WHEN status='completed' THEN 1 ELSE 0 END),0) FROM sync_runs WHERE source_id = ?`),
		src.ID).Scan(&running, &completed))
	assert.Zero(running)
	assert.Equal(int(succeeded.Load()), completed, "each successful import leaves exactly one completed row")
	assert.GreaterOrEqual(completed, 1)
}

// TestImportImessageRecordsProgressCounters proves each run row carries the
// import's processed, added, updated and error counts, including the skipped
// message that never reaches the archive.
func TestImportImessageRecordsProgressCounters(t *testing.T) {
	require, assert := require.New(t), assert.New(t)
	st := testutil.NewTestStore(t)
	path := newImessageFixture(t)
	chat, err := sql.Open("sqlite3", path)
	require.NoError(err)
	_, err = chat.Exec(`INSERT INTO message (text, date) VALUES ('second', 700000001000000000),
 ('rejected', 700000002000000000)`)
	require.NoError(err)
	require.NoError(chat.Close())
	src, err := resolveImessageSource(st)
	require.NoError(err)
	// The third chat.db row (ROWID 3) fails to archive, like a skipped message.
	testutil.RejectMessageInserts(t, st, "3")

	for range 2 {
		client, err := imessage.NewClient(path)
		require.NoError(err)
		_, err = importImessageRecorded(context.Background(), st, client, src.ID)
		require.NoError(client.Close())
		require.NoError(err)
	}

	rows, err := st.DB().Query(st.Rebind(`SELECT messages_processed, messages_added, messages_updated, errors_count
 FROM sync_runs WHERE source_id = ? ORDER BY id`), src.ID)
	require.NoError(err)
	defer func() { _ = rows.Close() }()
	var got [][4]int64
	for rows.Next() {
		var r [4]int64
		require.NoError(rows.Scan(&r[0], &r[1], &r[2], &r[3]))
		got = append(got, r)
	}
	require.NoError(rows.Err())
	assert.Equal([][4]int64{{3, 2, 0, 1}, {3, 0, 0, 1}}, got,
		"first run adds two and skips one; the rerun rewrites the two unchanged messages, which are not updates, and skips the same one")
}
