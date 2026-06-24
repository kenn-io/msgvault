package store_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

// seedMessageForLM creates a source + conversation + one message (with a body
// row) and returns the message id. Shared by the last_modified trigger tests.
func seedMessageForLM(t *testing.T, st *store.Store) int64 {
	t.Helper()
	src, err := st.GetOrCreateSource("gmail", "alice@example.com")
	require.NoError(t, err, "GetOrCreateSource")
	convID, err := st.EnsureConversationWithType(src.ID, "conv-lm", "email_thread", "Subject")
	require.NoError(t, err, "EnsureConversationWithType")
	id, err := st.UpsertMessage(&store.Message{
		SourceID:        src.ID,
		SourceMessageID: "msg-lm",
		ConversationID:  convID,
		MessageType:     "email",
		Subject:         sql.NullString{String: "original subject", Valid: true},
	})
	require.NoError(t, err, "UpsertMessage")
	require.NoError(t, st.UpsertMessageBody(id,
		sql.NullString{String: "original body", Valid: true},
		sql.NullString{}), "UpsertMessageBody")
	return id
}

// baselineLM stamps a fixed, far-past last_modified on the message so a
// subsequent trigger-driven bump produces a different, easily-asserted value
// without needing the test to sleep for the timestamp resolution to tick. The
// explicit write is itself preserved (not re-bumped) because the trigger's
// WHEN guard only fires when OLD.last_modified == NEW.last_modified, and here
// they differ.
func baselineLM(t *testing.T, st *store.Store, id int64) string {
	t.Helper()
	const past = "2000-01-01 00:00:00+00"
	_, err := st.DB().Exec(
		st.Rebind(`UPDATE messages SET last_modified = ? WHERE id = ?`), past, id)
	require.NoError(t, err, "set baseline last_modified")
	return readLM(t, st, id)
}

// readLM reads last_modified as a comparable string on both backends. On
// SQLite it CASTs to TEXT to defeat go-sqlite3's DATETIME→time.Time coercion
// (the same trick the embed worker uses); on PostgreSQL it casts to text in
// SQL so the comparison is a plain string on either backend.
func readLM(t *testing.T, st *store.Store, id int64) string {
	t.Helper()
	expr := "CAST(last_modified AS TEXT)"
	var s string
	require.NoError(t, st.DB().QueryRow(
		st.Rebind(`SELECT `+expr+` FROM messages WHERE id = ?`), id).Scan(&s),
		"read last_modified")
	return s
}

// TestLastModified_MessageUpdateBumps verifies any UPDATE to a message row
// bumps last_modified via the trigger.
func TestLastModified_MessageUpdateBumps(t *testing.T) {
	st := testutil.NewTestStore(t)
	id := seedMessageForLM(t, st)
	base := baselineLM(t, st, id)

	// A content UPDATE that does NOT touch last_modified must trigger a bump.
	_, err := st.DB().Exec(
		st.Rebind(`UPDATE messages SET subject = ? WHERE id = ?`), "changed subject", id)
	require.NoError(t, err, "update subject")

	got := readLM(t, st, id)
	assert.NotEqual(t, base, got, "message UPDATE must bump last_modified")
}

// TestLastModified_EmbedGenUpdateBumps verifies even an embed_gen-only UPDATE
// bumps last_modified — expected/harmless (the worker's CAS WHERE matches the
// PRE-trigger value, so its own stamp still succeeds; see
// SetEmbedGenIfUnchanged).
func TestLastModified_EmbedGenUpdateBumps(t *testing.T) {
	st := testutil.NewTestStore(t)
	id := seedMessageForLM(t, st)
	base := baselineLM(t, st, id)

	_, err := st.DB().Exec(
		st.Rebind(`UPDATE messages SET embed_gen = ? WHERE id = ?`), int64(7), id)
	require.NoError(t, err, "update embed_gen")

	got := readLM(t, st, id)
	assert.NotEqual(t, base, got, "embed_gen-only UPDATE bumps last_modified (expected)")
}

// TestLastModified_BodyUpdateBumpsParent verifies an UPDATE to message_bodies
// bumps the PARENT message's last_modified (the repair-encoding rewrite path).
func TestLastModified_BodyUpdateBumpsParent(t *testing.T) {
	st := testutil.NewTestStore(t)
	id := seedMessageForLM(t, st)
	base := baselineLM(t, st, id)

	_, err := st.DB().Exec(
		st.Rebind(`UPDATE message_bodies SET body_text = ? WHERE message_id = ?`),
		"corrected body", id)
	require.NoError(t, err, "update body")

	got := readLM(t, st, id)
	assert.NotEqual(t, base, got, "message_bodies UPDATE must bump parent last_modified")
}

// TestLastModified_BodyInsertBumpsParent verifies an INSERT into
// message_bodies bumps the parent message's last_modified.
func TestLastModified_BodyInsertBumpsParent(t *testing.T) {
	require := require.New(t)
	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource("gmail", "bob@example.com")
	require.NoError(err, "GetOrCreateSource")
	convID, err := st.EnsureConversationWithType(src.ID, "conv-lm2", "email_thread", "Subject")
	require.NoError(err, "EnsureConversationWithType")
	id, err := st.UpsertMessage(&store.Message{
		SourceID:        src.ID,
		SourceMessageID: "msg-lm2",
		ConversationID:  convID,
		MessageType:     "email",
		Subject:         sql.NullString{String: "subject", Valid: true},
	})
	require.NoError(err, "UpsertMessage")
	base := baselineLM(t, st, id)

	require.NoError(st.UpsertMessageBody(id,
		sql.NullString{String: "first body", Valid: true},
		sql.NullString{}), "insert body")

	got := readLM(t, st, id)
	assert.NotEqual(t, base, got, "message_bodies INSERT must bump parent last_modified")
}

// TestLastModified_UpgradePathMissingColumn covers the universal SQLite
// upgrade path for the last_modified watermark: a pre-existing archive whose
// messages table predates the column. On such a DB, InitSchema runs schema.sql
// FIRST — which executes `CREATE TRIGGER IF NOT EXISTS trg_messages_last_modified`,
// a trigger that REFERENCES last_modified — BEFORE LegacyColumnMigrations adds
// the column. This only works because SQLite resolves a trigger body's column
// references lazily (at fire time, not create time). After the column is added,
// InitSchema's one-shot backfill stamps the pre-existing NULL rows.
//
// Every existing SQLite user hits this exact path on upgrade, yet the other
// last_modified trigger tests all use a fresh DB where the column already
// exists when the trigger is created — so none of them exercise the
// trigger-before-column ordering. This test reconstructs the precondition by
// dropping the column (and the triggers that reference it, which SQLite would
// otherwise refuse to leave dangling) from a real schema, then re-runs the
// production InitSchema and asserts (a) it succeeds, (b) the column is added
// and backfilled to a non-NULL value for the pre-existing rows, and (c) the
// re-created trigger then functions as the CAS watermark.
//
// SQLite-only: it relies on ALTER TABLE DROP COLUMN and SQLite's deferred
// trigger column resolution. PostgreSQL's ADD COLUMN ... DEFAULT
// CURRENT_TIMESTAMP backfills automatically and its triggers are created
// after the column, so the upgrade ordering risk does not apply there.
func TestLastModified_UpgradePathMissingColumn(t *testing.T) {
	testutil.SkipIfPostgres(t, "SQLite ALTER TABLE DROP COLUMN + deferred trigger column resolution")
	require := require.New(t)
	assert := assert.New(t)

	dbPath := filepath.Join(t.TempDir(), "upgrade.db")

	// 1. Build a real schema, seed two messages (with bodies), then strip the
	//    last_modified column to reproduce a pre-last_modified archive.
	seed, err := store.OpenForTest(dbPath)
	require.NoError(err, "open seed store")
	require.NoError(seed.InitSchema(), "seed InitSchema")
	_, err = seed.DB().Exec(`
INSERT INTO sources (id, source_type, identifier) VALUES (1, 'gmail', 'alice@example.com');
INSERT INTO conversations (id, source_id, conversation_type) VALUES (1, 1, 'email_thread');
INSERT INTO messages (id, conversation_id, source_id, source_message_id, message_type, subject)
VALUES (1, 1, 1, 'm1', 'email', 'original one'),
       (2, 1, 1, 'm2', 'email', 'original two');
INSERT INTO message_bodies (message_id, body_text) VALUES (1, 'body one'), (2, 'body two');
`)
	require.NoError(err, "seed rows")

	// SQLite refuses to DROP a column while a trigger references it, so drop the
	// three last_modified triggers first; the resulting shape (messages without
	// last_modified, no last_modified triggers) is exactly what an archive built
	// before the column looks like.
	for _, trg := range []string{
		"trg_messages_last_modified",
		"trg_message_bodies_last_modified_upd",
		"trg_message_bodies_last_modified_ins",
	} {
		_, err = seed.DB().Exec(`DROP TRIGGER IF EXISTS ` + trg)
		require.NoErrorf(err, "drop trigger %s", trg)
	}
	_, err = seed.DB().Exec(`ALTER TABLE messages DROP COLUMN last_modified`)
	require.NoError(err, "drop last_modified to simulate pre-upgrade schema")

	var preCols int
	require.NoError(seed.DB().QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info('messages') WHERE name = 'last_modified'`).Scan(&preCols),
		"check column dropped")
	require.Equal(0, preCols, "precondition: messages must lack last_modified before upgrade")
	require.NoError(seed.Close(), "close seed store")

	// 2. Reopen and run the PRODUCTION upgrade entry point. (a) It must succeed:
	//    schema.sql creates trg_messages_last_modified (referencing last_modified)
	//    before LegacyColumnMigrations adds the column.
	st, err := store.OpenForTest(dbPath)
	require.NoError(err, "reopen upgraded store")
	t.Cleanup(func() { _ = st.Close() })
	require.NoError(st.InitSchema(),
		"InitSchema must succeed on a messages table lacking last_modified")

	// (b) The column now exists and the pre-existing rows were backfilled to a
	//     non-NULL value (a NULL CAS token would loop "needs embedding" forever).
	var postCols int
	require.NoError(st.DB().QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info('messages') WHERE name = 'last_modified'`).Scan(&postCols),
		"check column added")
	assert.Equal(1, postCols, "InitSchema must add last_modified")

	var nullCount int
	require.NoError(st.DB().QueryRow(
		`SELECT COUNT(*) FROM messages WHERE last_modified IS NULL`).Scan(&nullCount),
		"count NULL last_modified")
	assert.Equal(0, nullCount, "backfill must populate last_modified for pre-existing rows")

	// (c) The re-created trigger functions: a content UPDATE bumps last_modified.
	//     Baseline to a fixed far-past value so the bump is an unambiguous change.
	base := baselineLM(t, st, 1)
	_, err = st.DB().Exec(
		st.Rebind(`UPDATE messages SET subject = ? WHERE id = ?`), "changed one", int64(1))
	require.NoError(err, "update subject after upgrade")
	got := readLM(t, st, 1)
	assert.NotEqual(base, got,
		"re-created trigger must bump last_modified on UPDATE after upgrade")
}

// TestLastModified_NoInfiniteRecursion is a liveness check: a message UPDATE
// completes (the trigger's own UPDATE does not re-fire forever). If recursion
// were unbounded the Exec would error or hang; we simply require it returns.
func TestLastModified_NoInfiniteRecursion(t *testing.T) {
	st := testutil.NewTestStore(t)
	id := seedMessageForLM(t, st)
	ctx := context.Background()
	for range 5 {
		_, err := st.DB().ExecContext(ctx,
			st.Rebind(`UPDATE messages SET snippet = ? WHERE id = ?`), "s", id)
		require.NoError(t, err, "repeated update must not recurse/hang")
	}
}
