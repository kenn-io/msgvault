package cmd

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/attachmentpolicy"
	"go.kenn.io/msgvault/internal/imessage"
	"go.kenn.io/msgvault/internal/operations"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func newImessageFixture(t *testing.T) string {
	t.Helper()
	path, db := testutil.NewIMessageChatDB(t)
	_, err := db.ExecContext(t.Context(), `
CREATE TABLE attachment (guid TEXT, filename TEXT, transfer_name TEXT, mime_type TEXT, uti TEXT, total_bytes INTEGER, is_sticker INTEGER);
CREATE TABLE message_attachment_join (message_id INTEGER, attachment_id INTEGER);
INSERT INTO message (text, date) VALUES ('hello', 700000000000000000);`)
	require.NoError(t, err)
	require.NoError(t, db.Close())
	return path
}

func TestImportImessageRecordsOneSyncRun(t *testing.T) {
	require, assert := require.New(t), assert.New(t)
	st := testutil.NewTestStore(t)
	path := newImessageFixture(t)
	client, err := imessage.NewClient(t.Context(), path)
	require.NoError(err)
	t.Cleanup(func() { _ = client.Close() })
	src, err := resolveImessageSource(st)
	require.NoError(err)

	_, err = importImessageRecorded(t.Context(), st, client, src.ID, path)
	require.NoError(err)

	var total, completed int
	require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(
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
	path := newImessageFixture(t)
	client, err := imessage.NewClient(t.Context(), path)
	require.NoError(err)
	defer func() { _ = client.Close() }()
	summary, err := importImessageRecorded(t.Context(), st, client, src.ID, path)
	require.ErrorIs(err, store.ErrSyncAlreadyActive)
	assert.Nil(summary)
	var count int
	require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT COUNT(*) FROM sync_runs WHERE source_id = ?`), src.ID).Scan(&count))
	assert.Equal(1, count)
	var status string
	require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT status FROM sync_runs WHERE id = ?`), activeID).Scan(&status))
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
	_, err = chat.ExecContext(t.Context(), `INSERT INTO message (text, date) VALUES ('second', 700000001000000000),
 ('rejected', 700000002000000000)`)
	require.NoError(err)
	require.NoError(chat.Close())
	src, err := resolveImessageSource(st)
	require.NoError(err)
	// The third chat.db row (ROWID 3) fails to archive, like a skipped message.
	testutil.RejectMessageInserts(t, st, "3")

	for range 2 {
		client, err := imessage.NewClient(t.Context(), path)
		require.NoError(err)
		_, err = importImessageRecorded(t.Context(), st, client, src.ID, path)
		require.NoError(client.Close())
		require.NoError(err)
	}

	rows, err := st.DB().QueryContext(t.Context(), st.Rebind(`SELECT messages_processed, messages_added, messages_updated, errors_count
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
		"first run adds two and skips one; the rerun keeps the two unchanged messages and skips the same one")
}

// imessageRunCounters is one sync_runs row's persisted counters.
type imessageRunCounters struct{ processed, added, updated, errs int64 }

func latestImessageRunCounters(t *testing.T, st *store.Store, sourceID int64) imessageRunCounters {
	t.Helper()
	var c imessageRunCounters
	require.NoError(t, st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT messages_processed, messages_added, messages_updated, errors_count
 FROM sync_runs WHERE source_id = ? ORDER BY id DESC LIMIT 1`), sourceID).Scan(&c.processed, &c.added, &c.updated, &c.errs))
	return c
}

// TestImportImessageWriteFailureCounters proves counters include only committed
// changes when message persistence rolls back, and operation history can read
// runs containing rejected messages.
func TestImportImessageWriteFailureCounters(t *testing.T) {
	require, assert := require.New(t), assert.New(t)
	st := testutil.NewTestStore(t)
	if st.IsPostgreSQL() {
		t.Skip("the failure is injected with a SQLite trigger")
	}
	path := newImessageFixture(t)
	chat, err := sql.Open("sqlite3", path)
	require.NoError(err)
	t.Cleanup(func() { _ = chat.Close() })
	_, err = chat.ExecContext(t.Context(), `INSERT INTO message (text, date) VALUES ('second', 700000001000000000)`)
	require.NoError(err)
	src, err := resolveImessageSource(st)
	require.NoError(err)

	run := func() {
		t.Helper()
		client, err := imessage.NewClient(t.Context(), path)
		require.NoError(err)
		_, err = importImessageRecorded(t.Context(), st, client, src.ID, path)
		require.NoError(client.Close())
		require.NoError(err)
	}
	failRawWrites := func() {
		t.Helper()
		_, err := st.DB().ExecContext(t.Context(), `CREATE TRIGGER reject_raw_insert BEFORE INSERT ON message_raw
BEGIN SELECT RAISE(ABORT, 'synthetic rejection'); END;
CREATE TRIGGER reject_raw_update BEFORE UPDATE ON message_raw
BEGIN SELECT RAISE(ABORT, 'synthetic rejection'); END`)
		require.NoError(err)
	}
	allowRawWrites := func() {
		t.Helper()
		_, err := st.DB().ExecContext(t.Context(), `DROP TRIGGER reject_raw_insert; DROP TRIGGER reject_raw_update`)
		require.NoError(err)
	}

	// A raw-data failure rolls back each new message.
	failRawWrites()
	run()
	assert.Equal(imessageRunCounters{processed: 2, added: 0, updated: 0, errs: 2},
		latestImessageRunCounters(t, st, src.ID), "rolled-back messages are only skipped")
	allowRawWrites()

	// The rerun archives both messages for the first time.
	run()
	assert.Equal(imessageRunCounters{processed: 2, added: 2, updated: 0, errs: 0},
		latestImessageRunCounters(t, st, src.ID), "retrying rolled-back messages counts them as additions")

	// Everything archived and unchanged.
	run()
	assert.Equal(imessageRunCounters{processed: 2, added: 0, updated: 0, errs: 0},
		latestImessageRunCounters(t, st, src.ID), "an unchanged rerun adds and updates nothing")

	// The changed message rolls back; the unchanged message needs no write.
	_, err = chat.ExecContext(t.Context(), `UPDATE message SET text = 'second, edited' WHERE ROWID = 2`)
	require.NoError(err)
	failRawWrites()
	run()
	assert.Equal(imessageRunCounters{processed: 2, added: 0, updated: 0, errs: 1},
		latestImessageRunCounters(t, st, src.ID), "a rolled-back edit does not count as an update")
	allowRawWrites()

	// A new message that is rejected before anything is committed.
	_, err = chat.ExecContext(t.Context(), `INSERT INTO message (text, date) VALUES ('rejected', 700000002000000000)`)
	require.NoError(err)
	testutil.RejectMessageInserts(t, st, "3")
	run()
	assert.Equal(imessageRunCounters{processed: 3, added: 0, updated: 1, errs: 1},
		latestImessageRunCounters(t, st, src.ID), "the prior edit commits while a rejected new message is only skipped")

	snapshot, err := st.ListRuns(t.Context(), operations.Query{Kinds: []operations.Kind{operations.KindSourceSync}, Limit: 100})
	require.NoError(err, "operation history must read runs with partial writes")
	assert.Len(snapshot.Runs, 5)
	_, err = st.LaneStatus(t.Context(), operations.KindSourceSync)
	require.NoError(err, "lane status must read runs with partial writes")
}

// TestImportImessageAttachmentFailureIsPartial covers a run whose messages are
// committed but whose attachments cannot be stored: the run records those
// messages as additions and their attachment failures as errors, so operation
// history reports partial work instead of a failure that wrote nothing.
func TestImportImessageAttachmentFailureIsPartial(t *testing.T) {
	require, assert := require.New(t), assert.New(t)
	st := testutil.NewTestStore(t)
	path, _ := testutil.NewIMessageAttachmentChatDB(t)
	root := t.TempDir()
	require.NoError(os.MkdirAll(filepath.Join(root, "Attachments"), 0o700))
	require.NoError(os.WriteFile(filepath.Join(root, "Attachments", "voice.caf"), []byte("caffSYNTHETIC"), 0o600))
	// A file where the attachments directory belongs makes every store fail.
	dest := filepath.Join(t.TempDir(), "attachments")
	require.NoError(os.WriteFile(dest, []byte("synthetic destination blocker"), 0o600))
	src, err := resolveImessageSource(st)
	require.NoError(err)
	client, err := imessage.NewClient(t.Context(), path, imessage.WithAttachments(dest, root, attachmentpolicy.Policy{}))
	require.NoError(err)
	t.Cleanup(func() { _ = client.Close() })

	_, err = importImessageRecorded(t.Context(), st, client, src.ID, path)

	require.ErrorIs(err, imessage.ErrAttachmentImport)
	assert.Equal(imessageRunCounters{processed: 2, added: 2, updated: 0, errs: 2},
		latestImessageRunCounters(t, st, src.ID))
	snapshot, err := st.ListRuns(t.Context(), operations.Query{
		Kinds: []operations.Kind{operations.KindSourceSync}, Limit: 100,
	})
	require.NoError(err)
	require.Len(snapshot.Runs, 1)
	assert.Equal(operations.StatePartial, snapshot.Runs[0].State)
}
