package store_test

import (
	"database/sql"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

// previousReleaseSQLiteLastModifiedTrigger is the definition schema.sql carried
// before the change feed: blanket, unscoped, and `IF NOT EXISTS`. Reproduced
// verbatim, because what this pins is what happens when a build carrying it
// re-execs its schema over an archive this build has already migrated.
const previousReleaseSQLiteLastModifiedTrigger = `
CREATE TRIGGER IF NOT EXISTS trg_messages_last_modified
AFTER UPDATE ON messages FOR EACH ROW
WHEN OLD.last_modified = NEW.last_modified
BEGIN
    UPDATE messages SET last_modified = CURRENT_TIMESTAMP WHERE id = NEW.id;
END;`

// previousReleasePGTriggers is what PostgreSQLDialect.EnsureTriggers ran before
// the change feed. It DROPs and CREATEs unconditionally, so unlike SQLite's it
// really does replace this build's messages trigger — which is safe there,
// because PostgreSQL stamps last_modified in a BEFORE trigger, in place, and so
// never needed the UPDATE OF scope SQLite's replacement exists for.
var previousReleasePGTriggers = []string{
	`CREATE OR REPLACE FUNCTION set_messages_last_modified() RETURNS trigger AS $$
	 BEGIN
	     NEW.last_modified := CURRENT_TIMESTAMP;
	     RETURN NEW;
	 END;
	 $$ LANGUAGE plpgsql`,
	`DROP TRIGGER IF EXISTS trg_messages_last_modified ON messages`,
	`CREATE TRIGGER trg_messages_last_modified
	     BEFORE UPDATE ON messages FOR EACH ROW
	     WHEN (OLD.last_modified IS NOT DISTINCT FROM NEW.last_modified)
	     EXECUTE FUNCTION set_messages_last_modified()`,
	`CREATE OR REPLACE FUNCTION bump_message_last_modified() RETURNS trigger AS $$
	 BEGIN
	     UPDATE messages SET last_modified = CURRENT_TIMESTAMP WHERE id = NEW.message_id;
	     RETURN NEW;
	 END;
	 $$ LANGUAGE plpgsql`,
	`DROP TRIGGER IF EXISTS trg_message_bodies_last_modified ON message_bodies`,
	`CREATE TRIGGER trg_message_bodies_last_modified
	     AFTER INSERT OR UPDATE ON message_bodies FOR EACH ROW
	     EXECUTE FUNCTION bump_message_last_modified()`,
}

// openArchiveAsPreviousRelease replays what the release before the change feed
// does when it opens an archive this build has migrated: it re-execs its schema
// file and installs its own last_modified triggers.
//
// The schema files are read from the package directory rather than reproduced,
// so the replay tracks the real ones. Between the two releases they differ only
// by the content_changed_at column, and every statement in them is `IF NOT
// EXISTS` (or `CREATE OR REPLACE`), so executing this build's copy over an
// existing archive is what the previous release's copy would do to it.
//
// The previous release's ALTER TABLE ADD COLUMN list is deliberately not
// replayed: it is this build's list minus its content_changed_at entry, and on
// an archive that already has the columns every statement in it raises the
// duplicate-column error the loop silences.
func openArchiveAsPreviousRelease(t *testing.T, st *store.Store) {
	t.Helper()

	schemaFile := "schema.sql"
	if st.IsPostgreSQL() {
		schemaFile = "schema_pg.sql"
	}
	schema, err := os.ReadFile(schemaFile)
	require.NoErrorf(t, err, "read %s", schemaFile)
	_, err = st.DB().Exec(string(schema))
	require.NoErrorf(t, err,
		"the previous release re-execs %s on every open; a migrated archive must "+
			"still satisfy it", schemaFile)

	stmts := []string{previousReleaseSQLiteLastModifiedTrigger}
	if st.IsPostgreSQL() {
		stmts = previousReleasePGTriggers
	}
	for _, stmt := range stmts {
		_, err := st.DB().Exec(stmt)
		require.NoError(t, err, "previous release trigger installation")
	}
}

// countContentChangedAtTriggers counts how many of the feed's own triggers are
// still installed ON THE ARCHIVE UNDER TEST, on either backend.
//
// SQLite's sqlite_master is per-database-file, so it is already scoped. PG's
// pg_trigger is not: it lists every trigger in the database, and each PostgreSQL
// test store lives in its own schema of ONE shared test database. An unscoped
// count therefore reads other tests' triggers as if they were this archive's,
// and the "same number before and after" assertion below then depends on nothing
// else in that database creating or dropping a trigger of the same name in
// between -- which anything running concurrently does, making the test fail for
// a reason that has nothing to do with the downgrade. Join through pg_class and
// pg_namespace to count only this store's schema.
func countContentChangedAtTriggers(t *testing.T, st *store.Store) int {
	t.Helper()
	query := `SELECT COUNT(*) FROM sqlite_master WHERE type = 'trigger' AND name = ?`
	if st.IsPostgreSQL() {
		query = `SELECT COUNT(*) FROM pg_trigger tg
		         JOIN pg_class c ON c.oid = tg.tgrelid
		         JOIN pg_namespace n ON n.oid = c.relnamespace
		         WHERE NOT tg.tgisinternal AND tg.tgname = ?
		           AND n.nspname = current_schema()`
	}
	total := 0
	for _, trg := range contentChangedAtTriggerNames {
		var n int
		require.NoErrorf(t, st.DB().QueryRow(st.Rebind(query), trg.name).Scan(&n),
			"count trigger %s", trg.name)
		total += n
	}
	return total
}

// TestDowngrade_PreviousReleaseCanOpenAndWriteAMigratedArchive pins the
// storage-format contract a released archive format owes its predecessor: after
// this build has migrated an archive, the release before it must still be able
// to open that archive and write to it.
//
// Nothing else proves it. Static analysis says it holds — the old build ignores
// the extra column and the extra ledger row, its `CREATE TRIGGER IF NOT EXISTS`
// leaves this build's scoped trigger in place on SQLite, and the feed's own
// triggers are not something it knows to drop — but an operator rolling a
// release back is not the moment to discover otherwise, and every one of those
// claims is about a file that outlives both builds.
//
// The parts that would break loudly are asserted directly: the schema re-exec,
// the trigger installation, and then ordinary writes that name only columns the
// previous release knows about.
func TestDowngrade_PreviousReleaseCanOpenAndWriteAMigratedArchive(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	st := testutil.NewTestStore(t)
	existing := seedMessage(t, st, 1)
	require.NotEmpty(readContentChangedAt(t, st, existing),
		"this build must have stamped the archive before the downgrade")

	triggersBefore := countContentChangedAtTriggers(t, st)
	require.NotZero(triggersBefore, "the feed's triggers must be installed to start with")

	openArchiveAsPreviousRelease(t, st)

	// 1. The feed's own triggers survive the downgrade. The previous release
	//    knows nothing about them, so it neither drops nor replaces them, and an
	//    archive it writes to therefore keeps its watermarks current — which is
	//    what makes the roll-forward afterwards uneventful.
	assert.Equal(triggersBefore, countContentChangedAtTriggers(t, st),
		"the previous release must leave the content_changed_at triggers installed")

	// 2. On SQLite the scoped last_modified trigger survives too: the previous
	//    release's definition is CREATE TRIGGER IF NOT EXISTS under the same
	//    name, so it is a no-op rather than a replacement. (On PostgreSQL the old
	//    definition DROPs and CREATEs, and replacing it there is harmless — the
	//    UPDATE OF scope exists for SQLite's second-UPDATE stamp, which
	//    PostgreSQL does not perform.)
	if !st.IsPostgreSQL() {
		var ddl string
		require.NoError(st.DB().QueryRow(
			`SELECT sql FROM sqlite_master WHERE type = 'trigger' AND name = ?`,
			"trg_messages_last_modified").Scan(&ddl), "read the last_modified trigger DDL")
		assert.Contains(ddl, "UPDATE OF",
			"the previous release's CREATE TRIGGER IF NOT EXISTS must not have "+
				"replaced this build's scoped definition")
	}

	// 3. The previous release writes. Its INSERT names only columns it knows, so
	//    it never mentions content_changed_at — and the row must still be stamped,
	//    or it would be invisible to the feed forever after the roll-forward.
	src, err := st.GetOrCreateSource("gmail", "alice@example.com")
	require.NoError(err, "GetOrCreateSource")
	convID, err := st.EnsureConversationWithType(src.ID, "conv-downgrade", "email_thread", "S")
	require.NoError(err, "EnsureConversationWithType")
	res, err := st.DB().Exec(st.Rebind(`
		INSERT INTO messages (source_id, source_message_id, conversation_id, message_type,
		                      subject, size_estimate)
		VALUES (?, ?, ?, ?, ?, ?)`),
		src.ID, "msg-written-by-previous-release", convID, "email", "old build subject", 42)
	require.NoError(err, "the previous release must still be able to insert a message")
	written, err := lastInsertedMessageID(st, res)
	require.NoError(err, "read the inserted message id")

	inserted := readRawContentChangedAt(t, st, written)
	assert.Truef(inserted.Valid,
		"message %d was written by a build that does not know the column; the "+
			"database has to stamp it, or the row never reaches the feed", written)

	// 4. And its updates still move both watermarks. Both are seeded into the
	//    past first, so the assertion is that the write moved them rather than
	//    that the clock ticked between two statements.
	lmBefore := baselineLM(t, st, written)
	ccaBefore := stampContentChangedAt(t, st, written)
	_, err = st.DB().Exec(
		st.Rebind(`UPDATE messages SET subject = ? WHERE id = ?`), "edited by old build", written)
	require.NoError(err, "the previous release must still be able to update a message")
	assert.NotEqual(lmBefore, readLM(t, st, written), "last_modified must still bump")
	assert.NotEqual(ccaBefore, readContentChangedAt(t, st, written),
		"the content watermark must advance on a write the previous release made")

	// 5. A body write, the other trigger pair, through the previous release's
	//    columns only.
	lmBefore = baselineLM(t, st, written)
	_, err = st.DB().Exec(st.Rebind(
		`INSERT INTO message_bodies (message_id, body_text) VALUES (?, ?)`),
		written, "body from the old build")
	require.NoError(err, "the previous release must still be able to write a body")
	assert.NotEqual(lmBefore, readLM(t, st, written),
		"a body write must still bump the parent message's last_modified")

	// 6. Rolling forward again is uneventful, and the rows the previous release
	//    wrote are in the feed's ordering rather than outside it.
	require.NoError(st.InitSchema(),
		"this build must re-open an archive the previous release has written to")
	assert.NotEmpty(readContentChangedAt(t, st, written),
		"the roll-forward must find the old build's row already stamped")
	assert.NotEmpty(readContentChangedAt(t, st, existing),
		"and must not have disturbed the rows that were there before")
}

// lastInsertedMessageID reads the id of a row just inserted with raw SQL.
// LastInsertId is a SQLite affordance PostgreSQL's driver does not offer, so on
// PostgreSQL the id is looked up by the natural key instead.
func lastInsertedMessageID(st *store.Store, res sql.Result) (int64, error) {
	if !st.IsPostgreSQL() {
		return res.LastInsertId()
	}
	var id int64
	err := st.DB().QueryRow(st.Rebind(
		`SELECT id FROM messages WHERE source_message_id = ?`),
		"msg-written-by-previous-release").Scan(&id)
	return id, err
}
