package store_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
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

// TestLastModified_ExplicitWriteSurvivesContentUpdate pins the yield documented
// on baselineLM: a statement that sets last_modified by hand keeps that value
// rather than having it re-stamped.
//
// The risk is backend-specific and only appears once content_changed_at exists.
// PostgreSQL stamps that watermark in a BEFORE trigger, assigning NEW in place,
// so the caller's statement stays a single UPDATE. SQLite cannot assign to NEW,
// so its content_changed_at trigger issues a SECOND UPDATE of the same row —
// and a last_modified trigger that fires on every UPDATE re-fires on that one,
// overwriting the value the caller just wrote. ApplyMessageDateRepairs' CAS
// compares against exactly this token, so losing the write disarms it silently.
func TestLastModified_ExplicitWriteSurvivesContentUpdate(t *testing.T) {
	st := testutil.NewTestStore(t)
	control := seedMessage(t, st, 91)
	withContent := seedMessage(t, st, 92)

	const explicit = "2000-06-15 12:30:45+00"

	// Control: an explicit write naming no content column. Both backends
	// already yield to this, so its stored form is the canonical rendering of
	// the literal on this backend — which is what the real case must match.
	_, err := st.DB().Exec(
		st.Rebind(`UPDATE messages SET last_modified = ? WHERE id = ?`), explicit, control)
	require.NoError(t, err, "explicit last_modified write alone")

	// The real case: the same explicit write, in a statement that also changes
	// a content column and so trips the content_changed_at trigger.
	_, err = st.DB().Exec(
		st.Rebind(`UPDATE messages SET subject = ?, last_modified = ? WHERE id = ?`),
		"changed subject", explicit, withContent)
	require.NoError(t, err, "explicit last_modified write beside a content column")

	assert.Equal(t, readLM(t, st, control), readLM(t, st, withContent),
		"an explicit last_modified write must survive a content-column update: "+
			"ApplyMessageDateRepairs' CAS compares against exactly this token")
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
// FIRST — which executes the two message_bodies last_modified triggers, whose
// bodies REFERENCE messages.last_modified — BEFORE LegacyColumnMigrations adds
// the column. This only works because SQLite resolves a trigger body's column
// references lazily (at fire time, not create time). After the column is added,
// InitSchema's one-shot backfill stamps the pre-existing NULL rows.
//
// trg_messages_last_modified is not part of that ordering risk: EnsureTriggers
// creates it after the migrations (see lastModifiedUpdateOfColumns, which has
// to read the live column list). The message_bodies pair still rides schema.sql
// and so still lands before the column.
//
// Every existing SQLite user hits this exact path on upgrade, yet the other
// last_modified trigger tests all use a fresh DB where the column already
// exists when the triggers are created — so none of them exercise the
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
	_, err = seed.DB().Exec(
		seed.Rebind(`DELETE FROM applied_migrations WHERE name = ?`),
		messageWatermarkTriggersMigration)
	require.NoError(err, "restore the pre-trigger-migration ledger state")

	var preCols int
	require.NoError(seed.DB().QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info('messages') WHERE name = 'last_modified'`).Scan(&preCols),
		"check column dropped")
	require.Equal(0, preCols, "precondition: messages must lack last_modified before upgrade")
	require.NoError(seed.Close(), "close seed store")

	// 2. Reopen and run the PRODUCTION upgrade entry point. (a) It must succeed:
	//    schema.sql creates the two message_bodies last_modified triggers, whose
	//    bodies reference messages.last_modified, before LegacyColumnMigrations
	//    adds the column. trg_messages_last_modified is not in that window —
	//    EnsureTriggers creates it after the migrations — as the preamble above
	//    describes.
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

// TestLastModified_UpgradeReplacesBlanketTrigger reconstructs the archive every
// deployed SQLite user is holding — one built before content_changed_at
// existed, still carrying schema.sql's original blanket `AFTER UPDATE ON
// messages` last_modified trigger — and asserts that opening it with this build
// corrects it. It is the upgrade path, not a hypothetical one.
//
// That correction is the riskiest part of moving the trigger out of schema.sql.
// `CREATE TRIGGER IF NOT EXISTS` cannot replace an existing definition, so a
// blanket trigger left in place on an existing archive would go on firing on
// the SECOND UPDATE that stamps content_changed_at, and go on overwriting
// whatever last_modified the caller's statement had just written by hand — the
// exact clobber the `UPDATE OF` scope exists to prevent, and one that every
// other test in this file misses because a fresh database gets the scoped
// definition from the start. EnsureTriggers DROPs before it CREATEs precisely
// so the fix reaches an existing archive; nothing else pins that.
//
// SQLite-only: PostgreSQL stamps content_changed_at in a BEFORE trigger, in
// place, so it has no second UPDATE to re-enter and no blanket trigger to
// replace.
func TestLastModified_UpgradeReplacesBlanketTrigger(t *testing.T) {
	testutil.SkipIfPostgres(t, "the blanket-trigger clobber is a SQLite-only failure")
	require := require.New(t)
	assert := assert.New(t)

	dbPath := filepath.Join(t.TempDir(), "blanket.db")

	// 1. Build a real archive, then wind it back to its pre-feature shape: no
	//    content_changed_at column, and schema.sql's original blanket
	//    last_modified trigger in place of the scoped one.
	seed, err := store.OpenForTest(dbPath)
	require.NoError(err, "open seed store")
	require.NoError(seed.InitSchema(), "seed InitSchema")
	_, err = seed.DB().Exec(`
INSERT INTO sources (id, source_type, identifier) VALUES (1, 'gmail', 'alice@example.com');
INSERT INTO conversations (id, source_id, conversation_type) VALUES (1, 1, 'email_thread');
INSERT INTO messages (id, conversation_id, source_id, source_message_id, message_type, subject)
VALUES (1, 1, 1, 'm1', 'email', 'original one'),
       (2, 1, 1, 'm2', 'email', 'original two');
`)
	require.NoError(err, "seed rows")

	for _, stmt := range []string{
		`DROP TRIGGER IF EXISTS trg_messages_content_changed_ins`,
		`DROP TRIGGER IF EXISTS trg_messages_content_changed_at`,
		`DROP TRIGGER IF EXISTS trg_message_bodies_content_changed_ins`,
		`DROP TRIGGER IF EXISTS trg_message_bodies_content_changed_upd`,
		`DROP INDEX IF EXISTS idx_messages_content_changed_at`,
		`ALTER TABLE messages DROP COLUMN content_changed_at`,
		`DROP TRIGGER IF EXISTS trg_messages_last_modified`,
		// Verbatim from schema.sql before this change moved it to EnsureTriggers.
		`CREATE TRIGGER trg_messages_last_modified
		 AFTER UPDATE ON messages FOR EACH ROW
		 WHEN OLD.last_modified = NEW.last_modified
		 BEGIN
		     UPDATE messages SET last_modified = CURRENT_TIMESTAMP WHERE id = NEW.id;
		 END`,
	} {
		_, err = seed.DB().Exec(stmt)
		require.NoErrorf(err, "wind the archive back: %s", stmt)
	}
	_, err = seed.DB().Exec(
		seed.Rebind(`DELETE FROM applied_migrations WHERE name = ?`),
		messageWatermarkTriggersMigration)
	require.NoError(err, "restore the pre-trigger-migration ledger state")

	var preSQL string
	require.NoError(seed.DB().QueryRow(
		`SELECT sql FROM sqlite_master WHERE type = 'trigger' AND name = 'trg_messages_last_modified'`).
		Scan(&preSQL), "read the reconstructed trigger")
	require.NotContains(preSQL, "UPDATE OF",
		"the fixture is only meaningful if the archive really carries the BLANKET "+
			"definition going in")
	require.NoError(seed.Close(), "close seed store")

	// 2. Open it with this build. InitSchema is the production upgrade path.
	st, err := store.OpenForTest(dbPath)
	require.NoError(err, "reopen the archive")
	t.Cleanup(func() { _ = st.Close() })
	require.NoError(st.InitSchema(), "InitSchema on an archive with the blanket trigger")

	// 3a. The trigger is scoped now, and there is exactly one thing on messages
	//     writing last_modified — a replacement, not a second trigger racing the
	//     first.
	var postSQL string
	require.NoError(st.DB().QueryRow(
		`SELECT sql FROM sqlite_master WHERE type = 'trigger' AND name = 'trg_messages_last_modified'`).
		Scan(&postSQL), "read the upgraded trigger")
	assert.Contains(postSQL, "AFTER UPDATE OF",
		"the blanket definition must be replaced by the scoped one on open")
	assert.NotContains(postSQL, "content_changed_at",
		"the scope must exclude content_changed_at — that exclusion is what stops "+
			"the watermark stamp from re-entering this trigger")

	var writers int
	require.NoError(st.DB().QueryRow(
		`SELECT COUNT(*) FROM sqlite_master
		 WHERE type = 'trigger' AND tbl_name = 'messages'
		   AND sql LIKE '%SET last_modified%'`).Scan(&writers),
		"count last_modified writers on messages")
	assert.Equal(1, writers,
		"exactly one trigger on messages may write last_modified; a leftover "+
			"blanket one beside the scoped one would clobber just as before")

	// 3b. The behaviour that scope buys: an explicit last_modified write in a
	//     statement that also changes a content column is NOT overwritten by the
	//     content_changed_at stamp's second UPDATE. Compared against a control
	//     that writes the same literal alone, so the assertion is about the
	//     clobber and not about how this backend renders the literal.
	const explicit = "2000-06-15 12:30:45+00"
	_, err = st.DB().Exec(
		st.Rebind(`UPDATE messages SET last_modified = ? WHERE id = ?`), explicit, int64(2))
	require.NoError(err, "control: explicit last_modified write alone")
	_, err = st.DB().Exec(
		st.Rebind(`UPDATE messages SET subject = ?, last_modified = ? WHERE id = ?`),
		"changed one", explicit, int64(1))
	require.NoError(err, "explicit last_modified write beside a content column")
	assert.Equal(readLM(t, st, 2), readLM(t, st, 1),
		"after the upgrade an explicit last_modified write must survive a content "+
			"update; with the blanket trigger still installed the stamp's second "+
			"UPDATE overwrites it and ApplyMessageDateRepairs' CAS is silently disarmed")

	// 3c. And the watermark the upgrade was for still works: the backfilled
	//     column advances on a content change.
	base := stampContentChangedAt(t, st, 1)
	_, err = st.DB().Exec(
		st.Rebind(`UPDATE messages SET snippet = ? WHERE id = ?`), "changed snippet", int64(1))
	require.NoError(err, "content update after upgrade")
	assert.Greater(readContentChangedAt(t, st, 1), base,
		"content_changed_at must still advance on an upgraded archive")
}

// quoteSQLiteIdentifier renders name as a double-quoted SQLite identifier,
// doubling any embedded quote, so a test can create a column whose NAME is
// hostile SQL.
func quoteSQLiteIdentifier(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// openArchiveWithMessagesColumn builds a real archive, adds a column with the
// given name to `messages`, and reopens it through the production upgrade entry
// point (InitSchema, which is what runs EnsureTriggers). It returns the reopened
// store and InitSchema's error so the caller can assert on either.
//
// The column is added to a database that has already been initialised, then the
// trigger migration marker is removed to exercise the next trigger version.
func openArchiveWithMessagesColumn(t *testing.T, name string) (*store.Store, error) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "hostile.db")

	seed, err := store.OpenForTest(dbPath)
	require.NoError(t, err, "open seed store")
	require.NoError(t, seed.InitSchema(), "seed InitSchema")
	_, err = seed.DB().Exec(
		`ALTER TABLE messages ADD COLUMN ` + quoteSQLiteIdentifier(name) + ` TEXT`)
	require.NoErrorf(t, err, "add column %q — SQLite accepts any name inside quotes", name)
	_, err = seed.DB().Exec(
		seed.Rebind(`DELETE FROM applied_migrations WHERE name = ?`),
		messageWatermarkTriggersMigration)
	require.NoError(t, err, "queue the trigger migration")
	require.NoError(t, seed.Close(), "close seed store")

	st, err := store.OpenForTest(dbPath)
	require.NoError(t, err, "reopen archive")
	t.Cleanup(func() { _ = st.Close() })
	return st, st.InitSchema()
}

func TestLastModified_TriggerMigrationDoesNotRunOnEveryOpen(t *testing.T) {
	testutil.SkipIfPostgres(t, "the test inspects SQLite trigger DDL")
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewSQLiteTestStore(t)

	_, err := st.DB().Exec(`
		DROP TRIGGER trg_messages_last_modified;
		CREATE TRIGGER trg_messages_last_modified
		AFTER UPDATE ON messages BEGIN SELECT 1; END;
	`)
	require.NoError(err, "install a recognizable post-migration trigger")

	require.NoError(st.InitSchema(), "second InitSchema")

	var ddl string
	require.NoError(st.DB().QueryRow(
		`SELECT sql FROM sqlite_master WHERE type = 'trigger' AND name = 'trg_messages_last_modified'`,
	).Scan(&ddl))
	assert.Contains(ddl, "SELECT 1",
		"an applied trigger migration must not drop and recreate triggers on every open")
}

// messagesTableExists reports whether the messages table is still there. It is
// the injection detector: the payload every case below carries drops it.
func messagesTableExists(t *testing.T, st *store.Store) bool {
	t.Helper()
	var n int
	require.NoError(t, st.DB().QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'messages'`).Scan(&n),
		"read sqlite_master")
	return n == 1
}

// TestLastModified_TriggerScopeEscapesAQuoteInAColumnName is the second-order
// SQL injection guard for the half of the payload that quoting alone does not
// neutralise.
//
// The last_modified trigger's `UPDATE OF <columns>` scope is interpolated, not
// bound — SQLite has no parameter form for an identifier — and its column list
// comes from the live `messages` table, so the archive supplies it. A name
// carrying a double quote would close the quoting the renderer just opened, so
// the renderer doubles it, which is exactly how SQL escapes a quote inside a
// quoted identifier and how SQLite's own `%w` formatter renders one.
//
// Escaping rather than refusing, because this list enumerates EVERY live column
// on EVERY open: refusing one name SQLite considers legal makes the whole
// archive permanently unopenable, and by then earlier ALTER TABLE statements in
// the same initialisation have already committed. So the archive must open, the
// payload must stay inert, and the trigger must go on working.
func TestLastModified_TriggerScopeEscapesAQuoteInAColumnName(t *testing.T) {
	testutil.SkipIfPostgres(t, "only the SQLite dialect interpolates the UPDATE OF column list")
	require := require.New(t)
	assert := assert.New(t)

	// The payload closes the identifier, ends the trigger body, starts a second
	// statement, and comments out the renderer's remaining text.
	st, err := openArchiveWithMessagesColumn(t, `x" END; DROP TABLE messages; --`)
	require.NoError(err,
		"a column name containing a double quote is a legal SQLite identifier and "+
			"must open, not make the archive unopenable")

	assert.True(messagesTableExists(t, st),
		"the escaped name's payload must not have executed: messages is gone")

	// And the trigger the scope belongs to still works on that archive.
	id := seedMessageForLM(t, st)
	base := baselineLM(t, st, id)
	_, err = st.DB().Exec(
		st.Rebind(`UPDATE messages SET subject = ? WHERE id = ?`), "changed", id)
	require.NoError(err, "update subject")
	assert.NotEqual(base, readLM(t, st, id),
		"last_modified must still bump on an archive carrying a quote in a column name")
}

// TestLastModified_TriggerScopeRoundTripsAQuotedColumnName is the other half of
// the escaping contract: the DDL the trigger is built from must name the same
// column the archive declared, undoubled, and the trigger must be scoped to it.
// A renderer that dropped or mangled the quote would silently scope the trigger
// to a column that does not exist.
func TestLastModified_TriggerScopeRoundTripsAQuotedColumnName(t *testing.T) {
	testutil.SkipIfPostgres(t, "only the SQLite dialect interpolates the UPDATE OF column list")
	require := require.New(t)
	assert := assert.New(t)

	const weird = `we"ird`
	st, err := openArchiveWithMessagesColumn(t, weird)
	require.NoError(err, `a column named we"ird must open`)

	var present int
	require.NoError(st.DB().QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info('messages') WHERE name = ?`, weird).Scan(&present),
		"read the column back")
	require.Equal(1, present, "the archive still declares the column under its own name")

	var ddl string
	require.NoError(st.DB().QueryRow(
		`SELECT sql FROM sqlite_master WHERE type = 'trigger' AND name = ?`,
		"trg_messages_last_modified").Scan(&ddl), "read the trigger DDL")
	assert.Contains(ddl, `"we""ird"`,
		"the trigger's UPDATE OF scope must carry the name in its escaped form")

	// The scope reaches it: an UPDATE naming only that column bumps last_modified.
	id := seedMessageForLM(t, st)
	base := baselineLM(t, st, id)
	_, err = st.DB().Exec(
		st.Rebind(`UPDATE messages SET "we""ird" = ? WHERE id = ?`), "changed", id)
	require.NoError(err, "update the oddly named column")
	assert.NotEqual(base, readLM(t, st, id),
		"the trigger must fire for the column whose name carried the quote")
}

// TestLastModified_TriggerScopeQuotesAHostileColumnName is the injection case
// plain quoting already covered, kept green beside the escaping tests above: a
// payload with no double quote in it.
//
// The payload closes the CREATE TRIGGER the renderer is building, starts a
// second statement, and opens a block comment that swallows the renderer's
// remaining text. All three parts are needed: go-sqlite3 runs every statement in
// the string it is handed, but EnsureTriggers runs inside one transaction, so a
// payload that left a syntax error behind would be rolled back. This one leaves
// none — SQLite accepts an unterminated comment at end of input — so it commits
// and the archive is destroyed. Quoted, the whole thing is one column name.
func TestLastModified_TriggerScopeQuotesAHostileColumnName(t *testing.T) {
	testutil.SkipIfPostgres(t, "only the SQLite dialect interpolates the UPDATE OF column list")
	require := require.New(t)
	assert := assert.New(t)

	st, err := openArchiveWithMessagesColumn(t,
		`x ON messages BEGIN SELECT 1; END; DROP TABLE messages; /*`)
	require.True(messagesTableExists(t, st),
		"the injected DROP TABLE must not have executed")
	require.NoError(err,
		"a hostile but quotable column name must open cleanly, not execute and not fail")

	// And the trigger the scope belongs to still works on that archive.
	id := seedMessageForLM(t, st)
	base := baselineLM(t, st, id)
	_, err = st.DB().Exec(
		st.Rebind(`UPDATE messages SET subject = ? WHERE id = ?`), "changed", id)
	require.NoError(err, "update subject")
	assert.NotEqual(base, readLM(t, st, id),
		"last_modified must still bump on an archive carrying an oddly named column")
}

// TestLastModified_TriggerScopeAcceptsAwkwardButLegalColumnNames is the
// regression half of the fix: rejecting is only correct for names that cannot be
// quoted. A space, a reserved word and a non-ASCII letter are all legal SQLite
// identifiers that a real archive can carry, and every one of them is fine once
// quoted — a fix that refused them would make legitimate archives unopenable.
func TestLastModified_TriggerScopeAcceptsAwkwardButLegalColumnNames(t *testing.T) {
	testutil.SkipIfPostgres(t, "only the SQLite dialect interpolates the UPDATE OF column list")

	for _, name := range []string{
		"a column with spaces",
		"order",
		"select",
		"naïve_zeichen_日本語",
	} {
		t.Run(name, func(t *testing.T) {
			st, err := openArchiveWithMessagesColumn(t, name)
			require.NoErrorf(t, err, "column %q is a legal identifier and must not be rejected", name)

			id := seedMessageForLM(t, st)
			base := baselineLM(t, st, id)
			_, err = st.DB().Exec(
				st.Rebind(`UPDATE messages SET subject = ? WHERE id = ?`), "changed", id)
			require.NoError(t, err, "update subject")
			assert.NotEqual(t, base, readLM(t, st, id),
				"last_modified must still bump with column %q in the trigger's scope", name)
		})
	}
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
