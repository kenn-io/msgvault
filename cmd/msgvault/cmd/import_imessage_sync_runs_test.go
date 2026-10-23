package cmd

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/imessage"
	"go.kenn.io/msgvault/internal/operations"
	"go.kenn.io/msgvault/internal/store"
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
	require.NoError(t, db.Close())
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
	fresh, err := st.GetSourceByID(src.ID)
	require.NoError(err)
	assert.True(fresh.LastSyncAt.Valid, "successful imports advance source freshness")
}

// An admitted source run must survive a competing import unchanged.
func TestImportImessageRejectsActiveRun(t *testing.T) {
	require, assert := require.New(t), assert.New(t)
	st := testutil.NewTestStore(t)
	src, err := resolveImessageSource(st)
	require.NoError(err)
	activeID, err := st.StartSync(src.ID, "imessage_import")
	require.NoError(err)
	t.Cleanup(func() { _ = st.FailSync(activeID, "test cleanup") })
	client, err := imessage.NewClient(newImessageFixture(t))
	require.NoError(err)
	defer func() { _ = client.Close() }()
	summary, err := importImessageRecorded(context.Background(), st, client, src.ID)
	require.ErrorIs(err, store.ErrSyncAlreadyActive)
	assert.Nil(summary)
	var count int
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT COUNT(*) FROM sync_runs WHERE source_id = ?`), src.ID).Scan(&count))
	assert.Equal(1, count)
	var status string
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT status FROM sync_runs WHERE id = ?`), activeID).Scan(&status))
	assert.Equal("running", status)
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

// imessageRunCounters is one sync_runs row's persisted counters.
type imessageRunCounters struct{ processed, added, updated, errs int64 }

func latestImessageRunCounters(t *testing.T, st *store.Store, sourceID int64) imessageRunCounters {
	t.Helper()
	var c imessageRunCounters
	require.NoError(t, st.DB().QueryRow(st.Rebind(`SELECT messages_processed, messages_added, messages_updated, errors_count
 FROM sync_runs WHERE source_id = ? ORDER BY id DESC LIMIT 1`), sourceID).Scan(&c.processed, &c.added, &c.updated, &c.errs))
	return c
}

// TestImportImessagePartialWriteCounters proves the persisted counters are
// exact when a message fails after its row was committed: a new message is an
// addition, a changed existing one an update, an unchanged one neither, and a
// message that never reached the archive is only a skip. Every counter stays
// non-negative, and the operation history reads the run back.
func TestImportImessagePartialWriteCounters(t *testing.T) {
	require, assert := require.New(t), assert.New(t)
	st := testutil.NewTestStore(t)
	if st.IsPostgreSQL() {
		t.Skip("the failure is injected with a SQLite trigger")
	}
	path := newImessageFixture(t)
	chat, err := sql.Open("sqlite3", path)
	require.NoError(err)
	t.Cleanup(func() { _ = chat.Close() })
	_, err = chat.Exec(`INSERT INTO message (text, date) VALUES ('second', 700000001000000000)`)
	require.NoError(err)
	src, err := resolveImessageSource(st)
	require.NoError(err)

	run := func() {
		t.Helper()
		client, err := imessage.NewClient(path)
		require.NoError(err)
		_, err = importImessageRecorded(context.Background(), st, client, src.ID)
		require.NoError(client.Close())
		require.NoError(err)
	}
	failRawWrites := func() {
		t.Helper()
		_, err := st.DB().Exec(`CREATE TRIGGER reject_raw_write BEFORE INSERT ON message_raw
BEGIN SELECT RAISE(ABORT, 'synthetic rejection'); END`)
		require.NoError(err)
	}
	allowRawWrites := func() {
		t.Helper()
		_, err := st.DB().Exec(`DROP TRIGGER reject_raw_write`)
		require.NoError(err)
	}

	// New messages committed, then their raw-data write fails.
	failRawWrites()
	run()
	assert.Equal(imessageRunCounters{processed: 2, added: 2, updated: 0, errs: 2},
		latestImessageRunCounters(t, st, src.ID), "new messages committed before the failure are additions")
	allowRawWrites()

	// The rerun completes the half-written messages, which changes them.
	run()
	assert.Equal(imessageRunCounters{processed: 2, added: 0, updated: 2, errs: 0},
		latestImessageRunCounters(t, st, src.ID), "completing half-written messages is an update, not an addition")

	// Everything archived and unchanged.
	run()
	assert.Equal(imessageRunCounters{processed: 2, added: 0, updated: 0, errs: 0},
		latestImessageRunCounters(t, st, src.ID), "an unchanged rerun adds and updates nothing")

	// One existing message changed, then the raw-data write fails for both.
	_, err = chat.Exec(`UPDATE message SET text = 'second, edited' WHERE ROWID = 2`)
	require.NoError(err)
	failRawWrites()
	run()
	assert.Equal(imessageRunCounters{processed: 2, added: 0, updated: 1, errs: 2},
		latestImessageRunCounters(t, st, src.ID), "only the changed message counts as updated")
	allowRawWrites()

	// A new message that is rejected before anything is committed.
	_, err = chat.Exec(`INSERT INTO message (text, date) VALUES ('rejected', 700000002000000000)`)
	require.NoError(err)
	testutil.RejectMessageInserts(t, st, "3")
	run()
	assert.Equal(imessageRunCounters{processed: 3, added: 0, updated: 0, errs: 1},
		latestImessageRunCounters(t, st, src.ID), "a message that never reached the archive is only skipped")

	snapshot, err := st.ListRuns(context.Background(), operations.Query{Kinds: []operations.Kind{operations.KindSourceSync}, Limit: 100})
	require.NoError(err, "operation history must read runs with partial writes")
	assert.Len(snapshot.Runs, 5)
	_, err = st.LaneStatus(context.Background(), operations.KindSourceSync)
	require.NoError(err, "lane status must read runs with partial writes")
}
