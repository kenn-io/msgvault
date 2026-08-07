package store_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"regexp"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

// TestMessagesColumnClassificationIsExhaustive makes the hand-written trigger
// column list safe to maintain. A column added to messages later that nobody
// classifies would silently never bump the watermark, and a consumer would miss
// the change forever -- invisible in production and untestable after the fact.
// Every real column must appear in exactly one list.
func TestMessagesColumnClassificationIsExhaustive(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)

	actual, err := store.MessagesTableColumns(st)
	require.NoError(err)
	require.NotEmpty(actual)

	classified := map[string]int{}
	for _, c := range store.MessagesContentColumns {
		classified[c]++
	}
	for _, c := range store.MessagesNonContentColumns {
		classified[c]++
	}

	for _, col := range actual {
		assert.Equal(1, classified[col],
			"messages.%s must appear in exactly one of MessagesContentColumns / "+
				"MessagesNonContentColumns (found %d). Classify it: does changing it "+
				"mean a consumer should re-read the message?", col, classified[col])
	}

	actualSet := map[string]bool{}
	for _, col := range actual {
		actualSet[col] = true
	}
	for col := range classified {
		if col == "search_fts" && !st.IsPostgreSQL() {
			continue // PostgreSQL-only column
		}
		assert.True(actualSet[col], "%q is classified but is not a column of messages", col)
	}
}

// contentChangedPast is the fixed far-past watermark the helpers stamp before
// exercising a write, so "did the trigger fire?" is an exact string comparison
// instead of a sleep long enough for the clock to tick. It is written in a form
// both backends accept: SQLite stores the text verbatim in its DATETIME column,
// PostgreSQL parses it as TIMESTAMPTZ.
const contentChangedPast = "2000-01-01 00:00:00+00"

// seedMessage inserts one message with a unique source_message_id derived from
// n, and NO body row, so body-insert tests have something to insert. Returns
// the message id. It deliberately does not reuse seedMessageForLM, which
// hard-codes one source_message_id (so a second call upserts the same row) and
// already inserts a body row (so a later INSERT INTO message_bodies would
// violate the primary key).
func seedMessage(t *testing.T, st *store.Store, n int) int64 {
	t.Helper()
	src, err := st.GetOrCreateSource("gmail", "alice@example.com")
	require.NoError(t, err, "GetOrCreateSource")
	convID, err := st.EnsureConversationWithType(src.ID, fmt.Sprintf("conv-%d", n), "email_thread", "Subject")
	require.NoError(t, err, "EnsureConversationWithType")
	id, err := st.UpsertMessage(&store.Message{
		SourceID:        src.ID,
		SourceMessageID: fmt.Sprintf("msg-%d", n),
		ConversationID:  convID,
		MessageType:     "email",
		Subject:         sql.NullString{String: fmt.Sprintf("subject %d", n), Valid: true},
	})
	require.NoError(t, err, "UpsertMessage")
	return id
}

// persistMessage runs the REAL production persist path for message n:
// PersistMessage, which upserts the message row and its body row in one
// transaction exactly as every importer does. The resync tests must go through
// this rather than a hand-written UPDATE, because upsertMessageBody always
// executes its ON CONFLICT DO UPDATE even when messageBodyChanges reports no
// change -- an UpsertMessage-only test would pass while production churned on
// every sync.
func persistMessage(t *testing.T, st *store.Store, n int, subject, body string) int64 {
	t.Helper()
	src, err := st.GetOrCreateSource("gmail", "alice@example.com")
	require.NoError(t, err, "GetOrCreateSource")
	convID, err := st.EnsureConversationWithType(src.ID, fmt.Sprintf("conv-%d", n), "email_thread", "Subject")
	require.NoError(t, err, "EnsureConversationWithType")
	id, err := st.PersistMessage(&store.MessagePersistData{
		Message: &store.Message{
			SourceID:        src.ID,
			SourceMessageID: fmt.Sprintf("msg-%d", n),
			ConversationID:  convID,
			MessageType:     "email",
			Subject:         sql.NullString{String: subject, Valid: true},
		},
		BodyText: sql.NullString{String: body, Valid: true},
	})
	require.NoError(t, err, "PersistMessage")
	return id
}

// readContentChangedAt reads the watermark via CAST(... AS TEXT) to defeat
// go-sqlite3's DATETIME -> time.Time coercion, exactly as readLM does, and
// fails loudly on NULL: a NULL watermark is the specific defect the INSERT
// trigger exists to prevent, and it would otherwise surface as an opaque scan
// error.
func readContentChangedAt(t *testing.T, st *store.Store, id int64) string {
	t.Helper()
	got := readRawContentChangedAt(t, st, id)
	require.Truef(t, got.Valid, "content_changed_at is NULL for message %d", id)
	return got.String
}

// readRawContentChangedAt is readContentChangedAt without the non-NULL
// requirement, for the one test whose subject IS whether the value is NULL.
func readRawContentChangedAt(t *testing.T, st *store.Store, id int64) sql.NullString {
	t.Helper()
	var got sql.NullString
	require.NoError(t, st.DB().QueryRow(
		st.Rebind(`SELECT CAST(content_changed_at AS TEXT) FROM messages WHERE id = ?`), id).Scan(&got),
		"read content_changed_at")
	return got
}

// stampContentChangedAt writes contentChangedPast so a subsequent trigger bump
// is a different, easily-asserted value without sleeping for the clock to tick.
// The explicit write survives: the statement names only content_changed_at, so
// the UPDATE OF column list never matches and the trigger does not fire.
func stampContentChangedAt(t *testing.T, st *store.Store, id int64) string {
	t.Helper()
	_, err := st.DB().Exec(
		st.Rebind(`UPDATE messages SET content_changed_at = ? WHERE id = ?`), contentChangedPast, id)
	require.NoError(t, err, "stamp content_changed_at")
	return readContentChangedAt(t, st, id)
}

// altConversationID creates a second conversation in the same source as the
// given message and returns its id, so a conversation_id write moves the
// message to a real thread instead of tripping the foreign key.
func altConversationID(t *testing.T, st *store.Store, id int64) int64 {
	t.Helper()
	var sourceID int64
	require.NoError(t, st.DB().QueryRow(
		st.Rebind(`SELECT source_id FROM messages WHERE id = ?`), id).Scan(&sourceID),
		"read source_id")
	convID, err := st.EnsureConversationWithType(
		sourceID, fmt.Sprintf("conv-alt-%d", id), "email_thread", "Other thread")
	require.NoError(t, err, "EnsureConversationWithType(alt)")
	return convID
}

// altParticipantID creates a participant and returns its id, so a sender_id
// write points at a real row rather than an arbitrary integer the foreign key
// would reject.
func altParticipantID(t *testing.T, st *store.Store, id int64) int64 {
	t.Helper()
	pid, err := st.EnsureParticipant(
		fmt.Sprintf("sender-%d@example.com", id), "Changed Sender", "example.com")
	require.NoError(t, err, "EnsureParticipant")
	return pid
}

// updateMessageColumn writes a genuinely different, type-appropriate value to
// col. Foreign keys (conversation_id, sender_id) get a freshly created valid
// parent row rather than an arbitrary integer; metadata routes through
// SetMessageMetadata so the JSONB cast PostgreSQL requires comes from the
// dialect instead of being duplicated here.
func updateMessageColumn(t *testing.T, st *store.Store, id int64, col string) error {
	t.Helper()
	corrected := time.Date(2021, 3, 4, 5, 6, 7, 0, time.UTC)
	var value any
	switch col {
	case "source_message_id":
		value = fmt.Sprintf("changed-src-%d", id)
	case "conversation_id":
		value = altConversationID(t, st, id)
	case "sender_id":
		value = altParticipantID(t, st, id)
	case "message_type":
		value = "sms"
	case "sent_at", "received_at", "internal_date", "deleted_at", "deleted_from_source_at":
		value = corrected
	case "subject":
		value = "changed subject"
	case "snippet":
		value = "changed snippet"
	case "metadata":
		return st.SetMessageMetadata(id, sql.NullString{String: `{"changed":true}`, Valid: true})
	case "size_estimate":
		value = int64(4242)
	case "has_attachments":
		value = true
	case "attachment_count":
		value = 7
	default:
		require.Failf(t, "unhandled content column",
			"updateMessageColumn: no write defined for messages.%s -- add one so the "+
				"column's trigger coverage is actually exercised", col)
		return nil
	}
	_, err := st.DB().Exec(
		st.Rebind(fmt.Sprintf(`UPDATE messages SET %s = ? WHERE id = ?`, col)), value, id)
	return err
}

// TestContentChangedAt_ContentColumnUpdateBumps walks every column classified as
// content and proves changing it moves the watermark. Table-driven off the same
// list the triggers are built from, so a column added to the list but missing
// from the trigger fails here.
func TestContentChangedAt_ContentColumnUpdateBumps(t *testing.T) {
	st := testutil.NewTestStore(t)
	for i, col := range store.MessagesContentColumns {
		t.Run(col, func(t *testing.T) {
			id := seedMessage(t, st, i+1)
			base := stampContentChangedAt(t, st, id)
			require.NoErrorf(t, updateMessageColumn(t, st, id, col), "update messages.%s", col)
			assert.NotEqualf(t, base, readContentChangedAt(t, st, id),
				"changing messages.%s must bump content_changed_at: it is classified as "+
					"content, so a consumer that missed it would hold a stale copy forever", col)
		})
	}
}

// TestContentChangedAt_BookkeepingUpdateDoesNotBump is the other half. embed_gen
// is the motivating case: the embed worker stamps it continuously, and a
// consumer woken by every embedding stamp would re-read the archive on every
// index-generation rollover.
func TestContentChangedAt_BookkeepingUpdateDoesNotBump(t *testing.T) {
	st := testutil.NewTestStore(t)
	cases := []struct {
		col   string
		value any
	}{
		{"embed_gen", int64(7)},
		{"indexing_version", 2},
		{"is_read", false},
		{"read_at", time.Date(2021, 3, 4, 5, 6, 7, 0, time.UTC)},
		{"archived_at", time.Date(2021, 3, 4, 5, 6, 7, 0, time.UTC)},
		{"delete_batch_id", "batch-1"},
	}
	for i, tc := range cases {
		t.Run(tc.col, func(t *testing.T) {
			id := seedMessage(t, st, i+1)
			base := stampContentChangedAt(t, st, id)
			_, err := st.DB().Exec(
				st.Rebind(fmt.Sprintf(`UPDATE messages SET %s = ? WHERE id = ?`, tc.col)), tc.value, id)
			require.NoErrorf(t, err, "update messages.%s", tc.col)
			assert.Equalf(t, base, readContentChangedAt(t, st, id),
				"messages.%s is bookkeeping, not content: writing it must leave "+
					"content_changed_at alone", tc.col)
		})
	}
}

// TestContentChangedAt_SameValueWriteDoesNotBump: both backends fire UPDATE OF
// on the columns a statement NAMES, not the ones whose value changed. Without
// the value guard this passes vacuously and the feed reports every message a
// sync touches.
func TestContentChangedAt_SameValueWriteDoesNotBump(t *testing.T) {
	st := testutil.NewTestStore(t)
	id := seedMessage(t, st, 1)
	base := stampContentChangedAt(t, st, id)

	// Self-assignment puts five content columns in the SET list -- all that
	// UPDATE OF inspects on either backend -- while changing no value, NULL
	// columns included.
	_, err := st.DB().Exec(st.Rebind(`
		UPDATE messages
		   SET subject = subject,
		       snippet = snippet,
		       size_estimate = size_estimate,
		       has_attachments = has_attachments,
		       conversation_id = conversation_id
		 WHERE id = ?`), id)
	require.NoError(t, err, "same-value update")

	assert.Equal(t, base, readContentChangedAt(t, st, id),
		"an UPDATE naming content columns but changing no value must not bump "+
			"content_changed_at; the UPDATE OF column list alone is not enough")
}

// TestContentChangedAt_ResyncOfUnchangedMessageDoesNotBump exercises the real
// production path, not a hand-written UPDATE: PersistMessage on a message the
// archive already holds, unchanged, must leave the watermark alone. It must go
// through PersistMessage (message upsert + body upsert in one transaction),
// because upsertMessageBody always executes its ON CONFLICT DO UPDATE even when
// messageBodyChanges reports no change.
func TestContentChangedAt_ResyncOfUnchangedMessageDoesNotBump(t *testing.T) {
	st := testutil.NewTestStore(t)
	id := persistMessage(t, st, 1, "original subject", "original body")
	base := stampContentChangedAt(t, st, id)

	require.Equal(t, id, persistMessage(t, st, 1, "original subject", "original body"),
		"resync must land on the same row")

	assert.Equal(t, base, readContentChangedAt(t, st, id),
		"re-persisting an unchanged message must not bump content_changed_at: "+
			"UpsertMessage re-assigns ten content columns on every sync, and the body "+
			"upsert always runs, so an unguarded trigger reports the whole archive as changed")
}

// TestContentChangedAt_ResyncOfChangedMessageBumps is its complement, for both a
// changed subject and a changed body.
func TestContentChangedAt_ResyncOfChangedMessageBumps(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)

	subjID := persistMessage(t, st, 1, "original subject", "shared body")
	subjBase := stampContentChangedAt(t, st, subjID)
	require.Equal(subjID, persistMessage(t, st, 1, "corrected subject", "shared body"),
		"subject resync must land on the same row")
	assert.NotEqual(subjBase, readContentChangedAt(t, st, subjID),
		"a resync carrying a changed subject must bump content_changed_at")

	bodyID := persistMessage(t, st, 2, "steady subject", "original body")
	bodyBase := stampContentChangedAt(t, st, bodyID)
	require.Equal(bodyID, persistMessage(t, st, 2, "steady subject", "corrected body"),
		"body resync must land on the same row")
	assert.NotEqual(bodyBase, readContentChangedAt(t, st, bodyID),
		"a resync carrying a changed body must bump content_changed_at")
}

// TestContentChangedAt_BodyWriteBumpsParent: a body edit must reach the parent's
// watermark. The body lives in a separate table, so without this a
// repair-encoding pass would be invisible to a consumer.
func TestContentChangedAt_BodyWriteBumpsParent(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	id := seedMessage(t, st, 1)

	insBase := stampContentChangedAt(t, st, id)
	require.NoError(st.UpsertMessageBody(id,
		sql.NullString{String: "first body", Valid: true},
		sql.NullString{}), "insert body")
	assert.NotEqual(insBase, readContentChangedAt(t, st, id),
		"a message_bodies INSERT must bump the parent's content_changed_at")

	updBase := stampContentChangedAt(t, st, id)
	_, err := st.DB().Exec(
		st.Rebind(`UPDATE message_bodies SET body_text = ? WHERE message_id = ?`),
		"corrected body", id)
	require.NoError(err, "update body")
	assert.NotEqual(updBase, readContentChangedAt(t, st, id),
		"a message_bodies UPDATE must bump the parent's content_changed_at")
}

// TestContentChangedAt_NewRowIsStamped proves every new message is stamped
// non-NULL, whichever writer this backend uses for inserts: the BEFORE INSERT
// trigger on PostgreSQL, the column DEFAULT on a SQLite database created from
// schema.sql (which then gets no INSERT trigger at all), the INSERT trigger on
// a SQLite database upgraded by ALTER TABLE. A NULL watermark drops the row out
// of the range query permanently.
func TestContentChangedAt_NewRowIsStamped(t *testing.T) {
	st := testutil.NewTestStore(t)
	id := seedMessage(t, st, 1)

	var nulls int
	require.NoError(t, st.DB().QueryRow(st.Rebind(
		`SELECT COUNT(*) FROM messages WHERE id = ? AND content_changed_at IS NULL`),
		id).Scan(&nulls), "count NULL content_changed_at")

	assert.Equal(t, 0, nulls,
		"every new message must be stamped by whichever writer this backend uses for "+
			"inserts, since a NULL watermark would hide the row from the feed forever")
	assert.NotEqual(t, contentChangedPast, readContentChangedAt(t, st, id),
		"a freshly inserted message must be stamped with the current time")
}

// TestContentChangedAt_LastModifiedUnaffected is the compatibility proof: a
// bookkeeping-only UPDATE still bumps last_modified, which the embed worker's
// CAS depends on. If this fails, the change has stopped being additive.
func TestContentChangedAt_LastModifiedUnaffected(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	id := seedMessage(t, st, 1)

	// Baseline last_modified after the stamp, not before. On PostgreSQL the
	// stamp is a plain UPDATE and bumps last_modified; on SQLite the trigger's
	// UPDATE OF scope excludes content_changed_at, so a statement naming only
	// that column does not. Ordering this way makes the assertion below hold on
	// either backend without asserting which one applies.
	ccBase := stampContentChangedAt(t, st, id)
	lmBase := baselineLM(t, st, id)

	_, err := st.DB().Exec(
		st.Rebind(`UPDATE messages SET embed_gen = ? WHERE id = ?`), int64(7), id)
	require.NoError(err, "bookkeeping-only update")

	assert.NotEqual(lmBase, readLM(t, st, id),
		"last_modified must still bump on ANY update: it is the embed worker's CAS token")
	assert.Equal(ccBase, readContentChangedAt(t, st, id),
		"the same bookkeeping-only update must leave content_changed_at alone")
}

// stampLastModified writes an explicit last_modified value directly. The write
// is not re-bumped by a trigger: the value differs from the stored one, so the
// last_modified trigger's WHEN guard (OLD.last_modified = NEW.last_modified)
// yields to it. The upgrade test needs a distinguishable, known-past
// value to prove the content_changed_at backfill seeds from last_modified
// rather than from "now"; unlike stampContentChangedAt (which stamps
// content_changed_at to a fixed constant), the value here must vary and must
// survive SQLite's strftime parsing in the backfill SQL, so it is passed with
// no timezone suffix -- a bare "YYYY-MM-DD HH:MM:SS" is the one form both
// SQLite's strftime and PostgreSQL's TIMESTAMPTZ parser accept unambiguously.
func stampLastModified(t *testing.T, st *store.Store, id int64, value string) {
	t.Helper()
	_, err := st.DB().Exec(
		st.Rebind(`UPDATE messages SET last_modified = ? WHERE id = ?`), value, id)
	require.NoError(t, err, "stamp last_modified")
}

// contentChangedAtTriggerNames are the four triggers EnsureTriggers can create
// (dialect_sqlite.go, dialect_pg.go): two on messages (INSERT, UPDATE) and two
// on message_bodies (INSERT, UPDATE), all of which reference content_changed_at
// and must be dropped before SQLite will allow the column itself to be dropped.
// Only three of them exist on a SQLite database created from schema.sql, whose
// column DEFAULT stamps inserts and where EnsureTriggers therefore skips
// trg_messages_content_changed_ins; the drops below are IF EXISTS for that
// reason.
var contentChangedAtTriggerNames = []struct {
	name  string
	table string
}{
	{"trg_messages_content_changed_ins", "messages"},
	{"trg_messages_content_changed_at", "messages"},
	{"trg_message_bodies_content_changed_ins", "message_bodies"},
	{"trg_message_bodies_content_changed_upd", "message_bodies"},
}

// dropContentChangedAtColumn tears content_changed_at back out of a store
// built by testutil.NewTestStore, reproducing the shape of an archive that
// predates the column, on both backends. SQLite refuses ALTER TABLE ... DROP
// COLUMN while any trigger or index still references the column, so the
// removal must happen in dependency order: triggers first, then the index,
// then the column. DROP TRIGGER syntax differs by backend -- PostgreSQL
// triggers are namespaced per-table and require "ON <table>"; SQLite triggers
// are named at the schema level and reject a table clause.
func dropContentChangedAtColumn(t *testing.T, st *store.Store) {
	t.Helper()
	for _, trg := range contentChangedAtTriggerNames {
		stmt := `DROP TRIGGER IF EXISTS ` + trg.name
		if st.IsPostgreSQL() {
			stmt += ` ON ` + trg.table
		}
		_, err := st.DB().Exec(stmt)
		require.NoErrorf(t, err, "drop trigger %s", trg.name)
	}
	_, err := st.DB().Exec(`DROP INDEX IF EXISTS idx_messages_content_changed_at`)
	require.NoError(t, err, "drop idx_messages_content_changed_at")
	_, err = st.DB().Exec(`ALTER TABLE messages DROP COLUMN content_changed_at`)
	require.NoError(t, err, "drop content_changed_at column")
}

// contentChangedBackfillMigration is the ledger name InitSchema records once the
// content_changed_at backfill has run.
const contentChangedBackfillMigration = "messages_content_changed_at_backfill"
const messageWatermarkTriggersMigration = "message_watermark_triggers_v1"

// clearContentChangedBackfillLedger deletes that row from applied_migrations so
// InitSchema treats the migration as never having run -- the ledger state of
// an archive from before the migration shipped.
func clearContentChangedBackfillLedger(t *testing.T, st *store.Store) {
	t.Helper()
	for _, name := range []string{
		contentChangedBackfillMigration,
		messageWatermarkTriggersMigration,
	} {
		_, err := st.DB().Exec(
			st.Rebind(`DELETE FROM applied_migrations WHERE name = ?`), name)
		require.NoErrorf(t, err, "clear migration ledger entry %s", name)
	}
}

// TestContentChangedAt_UpgradeFromDatabaseWithoutColumn proves the migration an
// existing archive actually performs: a database whose messages table predates
// content_changed_at gains the column, has every existing row seeded from
// last_modified, has working triggers afterwards, and -- the part a
// fresh-schema test cannot cover -- stamps rows INSERTED AFTER the upgrade.
// Without an INSERT trigger those rows would be NULL forever, because neither
// ADD COLUMN carries a default, and they would never appear in the feed.
//
// A store from testutil.NewTestStore already has the column, its index, the
// triggers this backend creates for it, and the backfill's ledger row, so the
// naive version of this test would find nothing to upgrade: InitSchema would
// skip the backfill (ledger already marked applied) and the ADD COLUMN
// migration would be a silent no-op (IsDuplicateColumnError).
// dropContentChangedAtColumn and clearContentChangedBackfillLedger reconstruct
// the pre-upgrade shape first.
func TestContentChangedAt_UpgradeFromDatabaseWithoutColumn(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	id := seedMessage(t, st, 1)
	stampLastModified(t, st, id, "2020-01-02 03:04:05")

	// Tear the column back out, in dependency order.
	dropContentChangedAtColumn(t, st)
	clearContentChangedBackfillLedger(t, st)

	require.NoError(st.InitSchema())

	// (a) existing rows seeded from last_modified, not from "now"
	assert.Contains(readContentChangedAt(t, st, id), "2020-01-02",
		"backfill must seed from last_modified")

	// (b) rows inserted after the upgrade are stamped
	fresh := seedMessage(t, st, 2)
	assert.NotEmpty(readContentChangedAt(t, st, fresh),
		"the INSERT trigger must stamp rows created after an upgrade")

	// (c) triggers actually work after the upgrade
	base := stampContentChangedAt(t, st, id)
	require.NoError(updateMessageColumn(t, st, id, "subject"))
	assert.NotEqual(base, readContentChangedAt(t, st, id),
		"the UPDATE trigger must work after an upgrade")

	// (d) the backfill records itself so reopens do not rescan the table
	applied, err := st.IsMigrationApplied(contentChangedBackfillMigration)
	require.NoError(err)
	assert.True(applied)
}

// TestContentChangedAt_InterruptedBackfillResumesWhereItStopped is the whole
// reason the backfill walks the table in committed id batches instead of
// issuing one whole-table UPDATE.
//
// The backfill runs at daemon startup, and on a large archive it is minutes to
// hours of work. As a single transaction, an interruption anywhere in it — an
// operator's Ctrl-C, an OOM kill, a laptop lid — rolls back every row, so the
// next start begins again from nothing and the archive can never finish
// upgrading if the window between restarts is shorter than the backfill. On
// PostgreSQL each abandoned attempt also leaves a whole table's worth of dead
// tuples behind.
//
// Batched, an interruption costs only the batch in flight. This test proves
// both halves: rows stamped before the interruption are still stamped after it,
// and the next run does the remainder without revisiting them. The "without
// revisiting" half is made observable by moving last_modified — the value the
// backfill seeds from — on the already-stamped rows: a run that re-stamped them
// would drag their watermarks to the new value.
func TestContentChangedAt_InterruptedBackfillResumesWhereItStopped(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)

	const rows = 6
	ids := make([]int64, 0, rows)
	for n := 1; n <= rows; n++ {
		id := seedMessage(t, st, n)
		stampLastModified(t, st, id, fmt.Sprintf("2020-01-%02d 03:04:05", n))
		ids = append(ids, id)
	}

	// Reconstruct an archive that predates the column so InitSchema really runs
	// the backfill, and shrink the batch so six rows span several batches.
	dropContentChangedAtColumn(t, st)
	clearContentChangedBackfillLedger(t, st)
	defer st.SetContentChangedBackfillBatchSizeForTest(2)()

	// Interrupt at the second batch boundary: the first batch has committed and
	// nothing else has run.
	batches := 0
	restoreHook := st.SetContentChangedBackfillBatchHookForTest(func(fromID, toID int64) error {
		batches++
		if batches == 2 {
			return errors.New("simulated interruption mid-upgrade")
		}
		return nil
	})
	require.Error(st.InitSchema(), "the interrupted upgrade must report failure")
	restoreHook()
	require.Equal(2, batches, "the interruption must land at a batch boundary, not before the first batch")

	applied, err := st.IsMigrationApplied(contentChangedBackfillMigration)
	require.NoError(err)
	require.False(applied,
		"an interrupted backfill must not record itself as applied: the ledger gate would "+
			"stop the remaining rows from ever being stamped")

	stampedBeforeResume := map[int64]string{}
	for _, id := range ids {
		if got := readRawContentChangedAt(t, st, id); got.Valid {
			stampedBeforeResume[id] = got.String
		}
	}
	require.NotEmpty(stampedBeforeResume,
		"the batch that committed before the interruption must survive it; one whole-table "+
			"UPDATE would have rolled every stamped row back and the next run would start over")
	require.Less(len(stampedBeforeResume), len(ids),
		"the interruption must leave real work behind, or the resume half proves nothing")

	// Move the seed value on the rows that are already done. The resumed run
	// must not look at them, so their watermarks must not follow.
	for id := range stampedBeforeResume {
		stampLastModified(t, st, id, "2031-06-07 08:09:10")
	}

	require.NoError(st.InitSchema(), "the next open must finish the backfill")

	for i, id := range ids {
		got := readRawContentChangedAt(t, st, id)
		require.Truef(got.Valid,
			"message %d has a NULL watermark after the resumed backfill: the resumed run has to "+
				"cover every row the interrupted one did not reach", id)
		if before, ok := stampedBeforeResume[id]; ok {
			assert.Equalf(before, got.String,
				"message %d was stamped before the interruption and must not be re-stamped: "+
					"the resumed run does the remainder, it does not redo committed work", id)
			continue
		}
		assert.Containsf(got.String, fmt.Sprintf("2020-01-%02d", i+1),
			"message %d was stamped by the resumed run and must still be seeded from its own "+
				"last_modified", id)
	}

	applied, err = st.IsMigrationApplied(contentChangedBackfillMigration)
	require.NoError(err)
	assert.True(applied, "the completed backfill must record itself so later opens do not rescan")
}

// TestContentChangedAt_BackfillStopsWhenTheContextIsCancelled is the operator's
// exit from an upgrade that is going to take hours.
//
// The backfill walks every message in an existing archive and runs at daemon
// startup, before the port is bound. Run with a background context it cannot be
// interrupted at all: SIGINT and SIGTERM reach the process, the signal-cancelled
// root context is cancelled, and the backfill and its open transaction carry on
// regardless. The operator's only remaining move is SIGKILL, on a process in the
// middle of writing.
//
// This is NOT the resumability test above. That one injects an error to prove
// the committed batches survive; this one proves an operator can cause the stop
// in the first place. The two halves it adds are that cancellation is honoured
// promptly, and that a cancelled upgrade does not record itself as applied —
// which would strand every unstamped row outside the feed forever.
func TestContentChangedAt_BackfillStopsWhenTheContextIsCancelled(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)

	const rows = 8
	ids := make([]int64, 0, rows)
	for n := 1; n <= rows; n++ {
		id := seedMessage(t, st, n)
		stampLastModified(t, st, id, fmt.Sprintf("2020-01-%02d 03:04:05", n))
		ids = append(ids, id)
	}

	dropContentChangedAtColumn(t, st)
	clearContentChangedBackfillLedger(t, st)
	defer st.SetContentChangedBackfillBatchSizeForTest(2)()

	// Cancel at the second batch boundary: one batch has committed and the rest
	// of the table is still ahead, which is where an operator's Ctrl-C lands.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	batches := 0
	restoreHook := st.SetContentChangedBackfillBatchHookForTest(func(fromID, toID int64) error {
		batches++
		if batches == 2 {
			cancel()
		}
		return nil
	})
	err := st.InitSchemaContext(ctx)
	restoreHook()

	require.Error(err, "a cancelled initialisation must report failure, not a silent partial upgrade")
	require.ErrorIs(err, context.Canceled,
		"and report it as cancellation, so the daemon exits on the signal rather than "+
			"treating an operator's Ctrl-C as a corrupt archive")
	require.Equal(2, batches,
		"the backfill must stop at the batch the cancellation reached, not walk the rest "+
			"of the table first: on a real archive that is the hours the operator was "+
			"trying to get back")

	stamped := map[int64]string{}
	for _, id := range ids {
		if got := readRawContentChangedAt(t, st, id); got.Valid {
			stamped[id] = got.String
		}
	}
	require.NotEmpty(stamped,
		"the batch that committed before the cancellation must survive it")
	require.Less(len(stamped), len(ids),
		"the cancellation must leave real work behind, or the resume half proves nothing")

	applied, err := st.IsMigrationApplied(contentChangedBackfillMigration)
	require.NoError(err)
	require.False(applied,
		"a cancelled backfill must not record itself as applied: the ledger gate would "+
			"stop the remaining rows from ever being stamped")

	// The next open — an uncancelled one — finishes the job.
	require.NoError(st.InitSchema(), "the next open must resume and complete the backfill")
	for id, before := range stamped {
		assert.Equalf(before, readRawContentChangedAt(t, st, id).String,
			"message %d was stamped before the cancellation and must not be re-stamped", id)
	}
	for _, id := range ids {
		require.Truef(readRawContentChangedAt(t, st, id).Valid,
			"message %d must be stamped after the resumed run", id)
	}
	applied, err = st.IsMigrationApplied(contentChangedBackfillMigration)
	require.NoError(err)
	assert.True(applied, "the completed backfill must record itself")
}

// seedMessageAtID seeds message n and gives it an explicit id, so a test can
// place a row anywhere in the id space the backfill has to cope with. The two
// backends need opposite orders: SQLite's `INTEGER PRIMARY KEY` is the rowid and
// takes any 64-bit value, so the row is inserted first and moved afterwards;
// PostgreSQL's id is `GENERATED ALWAYS AS IDENTITY`, which refuses an UPDATE
// outright, so the identity sequence is repositioned before the insert instead.
// Returns the id, having checked the row really landed on it.
func seedMessageAtID(t *testing.T, st *store.Store, n int, id int64) int64 {
	t.Helper()
	if st.IsPostgreSQL() {
		// The default lower bound of a bigint identity is 1, so an id below that
		// needs MINVALUE lowered before RESTART will accept it. MINVALUE is only
		// ever lowered: raising it above the sequence's START (still 1) or up to
		// MAXVALUE is rejected outright.
		alter := fmt.Sprintf(`ALTER TABLE messages ALTER COLUMN id RESTART WITH %d`, id)
		if id < 1 {
			alter = fmt.Sprintf(
				`ALTER TABLE messages ALTER COLUMN id SET MINVALUE %d RESTART WITH %d`, id, id)
		}
		_, err := st.DB().Exec(alter)
		require.NoErrorf(t, err, "reposition the messages identity sequence to %d", id)
		got := seedMessage(t, st, n)
		require.Equalf(t, id, got, "message %d did not land on the requested id", n)
		return id
	}
	got := seedMessage(t, st, n)
	_, err := st.DB().Exec(
		st.Rebind(`UPDATE messages SET id = ? WHERE id = ?`), id, got)
	require.NoErrorf(t, err, "move message %d to id %d", n, id)
	return id
}

// countBackfillBatches installs the batch hook as a counter and returns a
// pointer to the count. The guard is not decoration: a backfill that walks the
// numeric id span instead of the rows needing work does not merely run slowly at
// the extremes of the id space, it never terminates, so a test that only
// asserted on the count would hang the whole suite instead of failing. The hook
// aborts the run once the batch count passes what any correct walk could need,
// turning that into a fast, readable failure.
func countBackfillBatches(t *testing.T, st *store.Store, maxBatches int) *int {
	t.Helper()
	batches := 0
	restore := st.SetContentChangedBackfillBatchHookForTest(func(fromID, toID int64) error {
		batches++
		if batches > maxBatches {
			return fmt.Errorf(
				"the backfill has run %d batches for an archive that needs at most %d: "+
					"it is walking the id span rather than the rows that need work",
				batches, maxBatches)
		}
		return nil
	})
	t.Cleanup(restore)
	return &batches
}

// TestContentChangedAt_BackfillStampsTheRowAtIDZero covers an archive whose
// highest message id is 0.
//
// SQLite's `INTEGER PRIMARY KEY` is the rowid, and the schema puts no positive
// constraint on it, so 0 is a legal id and an archive can consist of exactly
// that row. A backfill that reads `MAX(id) == 0` as "empty table" skips it and
// then records itself as applied — and the skip is permanent: the feed's range
// predicate excludes NULL and the ledger gate means the scan never runs again.
// The message is invisible to the feed for the life of the archive.
func TestContentChangedAt_BackfillStampsTheRowAtIDZero(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)

	id := seedMessageAtID(t, st, 1, 0)
	stampLastModified(t, st, id, "2020-01-02 03:04:05")

	// Reconstruct an archive that predates the column so InitSchema really runs
	// the backfill over this row.
	dropContentChangedAtColumn(t, st)
	clearContentChangedBackfillLedger(t, st)
	require.NoError(st.InitSchema())

	got := readRawContentChangedAt(t, st, id)
	require.True(got.Valid,
		"the message at id 0 has a NULL watermark after the upgrade: the backfill has "+
			"already recorded itself as applied, so nothing will ever stamp it and it can "+
			"never appear in the change feed")
	assert.Contains(got.String, "2020-01-02",
		"and it must be seeded from last_modified like every other backfilled row")
}

// TestContentChangedAt_BackfillFinishesAtTheEdgesOfTheIDSpace covers an archive
// holding a legal id near either end of the 64-bit range.
//
// A walk that advances by adding a batch size to a cursor overflows there: at
// the top the batch end wraps negative, so the range matches nothing and the
// cursor wraps instead of terminating. That is not a slow upgrade, it is an
// upgrade that never finishes — at daemon startup, before the port is bound, so
// the daemon never serves at all. The batch guard turns the non-termination into
// a failure this suite can report.
func TestContentChangedAt_BackfillFinishesAtTheEdgesOfTheIDSpace(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)

	low := seedMessageAtID(t, st, 1, math.MinInt64)
	high := seedMessageAtID(t, st, 2, math.MaxInt64)
	stampLastModified(t, st, low, "2020-01-02 03:04:05")
	stampLastModified(t, st, high, "2020-01-03 03:04:05")

	dropContentChangedAtColumn(t, st)
	clearContentChangedBackfillLedger(t, st)
	// Both rows fit in one batch, so anything past a handful of batches is the
	// walk crawling the id span.
	batches := countBackfillBatches(t, st, 4)

	require.NoError(st.InitSchema(),
		"the upgrade must finish on an archive holding ids at the edges of the id space")
	assert.Contains(readContentChangedAt(t, st, low), "2020-01-02", "the row at the bottom of the id space")
	assert.Contains(readContentChangedAt(t, st, high), "2020-01-03", "the row at the top of the id space")
	assert.LessOrEqual(*batches, 2, "two rows one batch apart need one batch, not %d", *batches)
}

// TestContentChangedAt_BackfillSkipsIDRangesWithNoWork covers a sparse archive:
// legal ids spread thinly over a wide span, which is what a long-lived archive
// looks like after deletions, imports from several sources, or a subset copy.
//
// The cost that matters is transactions, not rows. A walk that steps through the
// numeric span from MIN(id) to MAX(id) opens one transaction per batch of ids
// whether or not any row lives there, so an archive with a handful of rows
// spread over millions of ids pays millions of empty transactions at daemon
// startup — before the port is bound. A walk over the rows that need work pays
// one transaction per batch of rows.
func TestContentChangedAt_BackfillSkipsIDRangesWithNoWork(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)

	// Two rows, four million ids apart, against the production batch size of
	// 5000: one batch of work, 800 batches of empty span.
	const gap = 4_000_001
	first := seedMessageAtID(t, st, 1, 1)
	second := seedMessageAtID(t, st, 2, gap)
	stampLastModified(t, st, first, "2020-01-02 03:04:05")
	stampLastModified(t, st, second, "2020-01-03 03:04:05")

	dropContentChangedAtColumn(t, st)
	clearContentChangedBackfillLedger(t, st)
	batches := countBackfillBatches(t, st, 8)

	require.NoError(st.InitSchema())
	assert.Contains(readContentChangedAt(t, st, first), "2020-01-02", "the first row")
	assert.Contains(readContentChangedAt(t, st, second), "2020-01-03", "the second row")
	assert.Equal(1, *batches,
		"both rows fit in one batch of work, so the upgrade must open one transaction, "+
			"not one per batch of empty id span")
}

// TestContentChangedAt_BackfillNeverMintsANullWatermark covers the values
// SQLite's strftime refuses.
//
// The backfill seeds content_changed_at from last_modified. strftime returns
// NULL rather than an error for any input its parser rejects — a unix integer,
// an empty string, anything malformed — and last_modified is a DATETIME text
// column SQLite does not type-check. A NULL watermark is terminal: the feed's
// range predicate excludes NULL, and the migration ledger guarantees the
// `WHERE content_changed_at IS NULL` scan never runs again, so the row is
// invisible to the feed forever.
//
// PostgreSQL cannot fail this way — last_modified is a real TIMESTAMPTZ there
// and the backfill copies it without conversion — so this is SQLite-only.
func TestContentChangedAt_BackfillNeverMintsANullWatermark(t *testing.T) {
	testutil.SkipIfPostgres(t, "only SQLite's untyped DATETIME text can hold a value strftime rejects")
	require := require.New(t)
	assert := assert.New(t)

	st := testutil.NewTestStore(t)
	// One row per value strftime cannot parse, plus a parseable control so the
	// test still proves the backfill seeds from last_modified when it can.
	unparseable := map[string]int64{
		"1700000000":          seedMessage(t, st, 1), // a unix epoch integer
		"":                    seedMessage(t, st, 2), // an empty string
		"not a time":          seedMessage(t, st, 3), // free text
		"2020-13-45":          seedMessage(t, st, 4), // structurally plausible, out of range
		"2020-01-02 03:04:05": seedMessage(t, st, 5), // the control: parseable
	}
	for value, id := range unparseable {
		stampLastModified(t, st, id, value)
	}

	// Reconstruct an archive that predates the column so InitSchema really runs
	// the backfill over these rows.
	dropContentChangedAtColumn(t, st)
	clearContentChangedBackfillLedger(t, st)
	require.NoError(st.InitSchema())

	for value, id := range unparseable {
		assert.Truef(readRawContentChangedAt(t, st, id).Valid,
			"message %d has last_modified %q, which strftime maps to NULL: the backfill has "+
				"already marked itself applied, so nothing will ever stamp this row and it can "+
				"never appear in the change feed", id, value)
	}
	assert.Contains(readContentChangedAt(t, st, unparseable["2020-01-02 03:04:05"]), "2020-01-02",
		"a parseable last_modified must still seed the watermark")

	// The feed itself is the point: every seeded row has to be reachable from a
	// zero cursor. Settle the clock first — the fallback stamps "now", and the
	// feed deliberately withholds the instant it is reading in.
	settleFeedClock(t, st)
	page, err := st.ListChangedMessages(context.Background(), store.ChangedMessagesCursor{}, 100)
	require.NoError(err)
	seen := map[int64]bool{}
	for _, m := range page.Messages {
		seen[m.ID] = true
	}
	for value, id := range unparseable {
		assert.Truef(seen[id], "message %d (last_modified %q) is missing from the change feed", id, value)
	}
}

// TestContentChangedAt_MessageInsertedDuringUpgradeIsStamped closes the window
// between the backfill and the triggers. The backfill records itself in the
// migration ledger the moment it finishes and never looks for NULL watermarks
// again, while InitSchema still has whole-table index builds ahead of it — on a
// large archive, minutes of them. A message written by another connection in
// that window used to land with content_changed_at NULL, which is terminal: the
// feed's range predicate excludes NULL and the ledger gate means nothing will
// ever fix it. Creating the triggers before the backfill makes the INSERT
// trigger the writer for that row instead.
//
// The insert is performed by a test-only hook placed at exactly that point in
// InitSchema, so the race is reproduced rather than raced.
func TestContentChangedAt_MessageInsertedDuringUpgradeIsStamped(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	existing := seedMessage(t, st, 1)
	stampLastModified(t, st, existing, "2020-01-02 03:04:05")

	// Reconstruct an archive that predates the column, so InitSchema really
	// runs the migration, the backfill, and the index builds.
	dropContentChangedAtColumn(t, st)
	clearContentChangedBackfillLedger(t, st)

	var concurrent int64
	restore := st.SetInitSchemaWindowHookForTest(func() {
		concurrent = seedMessage(t, st, 2)
	})
	defer restore()
	require.NoError(st.InitSchema())
	restore()

	require.NotZero(concurrent,
		"the hook never fired, so this test proves nothing about the window")
	assert.Truef(readRawContentChangedAt(t, st, concurrent).Valid,
		"message %d was inserted while InitSchema was still building indexes and "+
			"has a NULL content_changed_at: the backfill has already marked itself "+
			"applied, so nothing will ever stamp it and the row can never appear in "+
			"the change feed", concurrent)
}

// TestContentChangedAt_ColumnOrderMatchesAfterUpgrade guards against a fresh
// database and an upgraded one declaring messages' columns in different orders.
// ALTER TABLE always appends, so a column placed mid-table in schema.sql but
// appended by the migration diverges, and any read that goes by position writes
// or interprets values in the wrong columns. This asserts content_changed_at
// lands in the same position either way.
//
// subset.go's messages copy used to be exactly such a reader
// ("INSERT INTO messages SELECT * FROM src.messages"); it now names the columns
// the source and destination share, so it no longer relies on this. The two
// layouts still meet — a subset's source and destination are routinely one
// fresh and one upgraded — so the invariant is worth pinning on its own.
//
// Scoped to content_changed_at on purpose: last_modified and embed_gen already
// disagree at this commit (a pre-existing upstream defect out of scope for this
// change), so asserting the whole column order would fail for reasons this
// work did not cause.
func TestContentChangedAt_ColumnOrderMatchesAfterUpgrade(t *testing.T) {
	require := require.New(t)

	fresh := testutil.NewTestStore(t)
	freshCols, err := store.MessagesTableColumns(fresh)
	require.NoError(err)

	upgraded := testutil.NewTestStore(t)
	dropContentChangedAtColumn(t, upgraded)
	clearContentChangedBackfillLedger(t, upgraded)
	require.NoError(upgraded.InitSchema())
	upgradedCols, err := store.MessagesTableColumns(upgraded)
	require.NoError(err)

	// slices.Index returns -1 for "not found", and -1 == -1 would make the
	// position assertion below pass vacuously if the column were missing from
	// both schemas instead of proving it lands in the same real position.
	freshIdx := slices.Index(freshCols, "content_changed_at")
	upgradedIdx := slices.Index(upgradedCols, "content_changed_at")
	require.GreaterOrEqual(freshIdx, 0, "content_changed_at must exist in the fresh schema")
	require.GreaterOrEqual(upgradedIdx, 0, "content_changed_at must exist in the upgraded schema")

	assert.Equal(t, freshIdx, upgradedIdx,
		"content_changed_at must occupy the same column position on fresh and upgraded databases; "+
			"a divergence silently corrupts any read of a message row that goes by position")
}

// contentChangedStampShape is the fixed-width text layout SQLite watermarks
// must have. The feed's cursor comparison is lexical on SQLite, so a stamp of a
// different width sorts into the wrong place and is skipped or repeated
// forever.
var contentChangedStampShape = regexp.MustCompile(`^\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}\.\d{3}$`)

// TestContentChangedAt_FreshAndUpgradedStampsShareOneFormat pins the two
// writers of a SQLite watermark against each other. A fresh database stamps new
// rows from the column DEFAULT in schema.sql; a database upgraded by ALTER
// TABLE cannot carry that DEFAULT (SQLite rejects a non-constant one there) and
// stamps from the INSERT trigger instead. Both must produce the identical
// layout, because the two databases can meet — subset.go copies messages
// between them and the cursor comparison is lexical.
func TestContentChangedAt_FreshAndUpgradedStampsShareOneFormat(t *testing.T) {
	testutil.SkipIfPostgres(t, "PostgreSQL compares TIMESTAMPTZ natively and has one stamping writer")
	require := require.New(t)
	assert := assert.New(t)

	fresh := testutil.NewTestStore(t)
	freshStamp := readContentChangedAt(t, fresh, seedMessage(t, fresh, 1))

	upgraded := testutil.NewTestStore(t)
	dropContentChangedAtColumn(t, upgraded)
	clearContentChangedBackfillLedger(t, upgraded)
	require.NoError(upgraded.InitSchema())
	upgradedStamp := readContentChangedAt(t, upgraded, seedMessage(t, upgraded, 1))

	assert.Regexp(contentChangedStampShape, freshStamp,
		"a fresh database's watermark must carry the millisecond layout the cursor sorts on")
	assert.Regexp(contentChangedStampShape, upgradedStamp,
		"an upgraded database's watermark must carry the same layout as a fresh one")
	assert.Len(upgradedStamp, len(freshStamp),
		"fresh and upgraded stamps must be the same width or lexical cursor comparison breaks")
}

// insertMessagesTriggerPrograms counts the trigger subprograms SQLite compiles
// into an INSERT on messages. The `Program` opcode is how the bytecode invokes a
// row trigger, and its presence — not the trigger body actually running — is
// what forces SQLite to open a statement journal per INSERT.
func insertMessagesTriggerPrograms(t *testing.T, st *store.Store, insert string, args ...any) int {
	t.Helper()
	rows, err := st.DB().Query("EXPLAIN "+insert, args...)
	require.NoError(t, err, "EXPLAIN insert")
	defer func() { _ = rows.Close() }()
	cols, err := rows.Columns()
	require.NoError(t, err)
	opcodeCol := slices.Index(cols, "opcode")
	require.GreaterOrEqual(t, opcodeCol, 0, "EXPLAIN must report an opcode column")

	cells := make([]sql.NullString, len(cols))
	targets := make([]any, len(cols))
	for i := range cells {
		targets[i] = &cells[i]
	}
	programs := 0
	for rows.Next() {
		require.NoError(t, rows.Scan(targets...))
		if cells[opcodeCol].String == "Program" {
			programs++
		}
	}
	require.NoError(t, rows.Err())
	return programs
}

// TestContentChangedAt_InsertRunsNoTriggerOnAFreshDatabase keeps message ingest
// at the cost it had before the watermark existed.
//
// SQLite triggers cannot assign to NEW, so an AFTER INSERT trigger that stamps
// the watermark has to re-UPDATE the row that was just inserted. Worse, merely
// HAVING a row trigger on messages makes SQLite compile a trigger subprogram
// into every INSERT and open a statement journal for it, whether or not the
// body runs — measured at 7.3s against 1.4s for 100k rows inserted inside one
// transaction (Linux, WAL, synchronous=FULL, same at OFF) with the WHEN guard
// never once satisfied. The cost is specific to inserts sharing a transaction:
// the same trigger over 20k messages persisted one transaction each made no
// measurable difference. The bulk paths are the ones that pay — fakevault's
// INSERT ... SELECT generator and subset.go's message copy. The column DEFAULT
// stamps fresh databases instead, and EnsureTriggers omits the INSERT trigger
// entirely when that DEFAULT is present.
//
// total_changes() is SQLite's per-connection count of rows written, and unlike
// changes() it does include rows written by trigger programs — which is exactly
// what has to be counted.
func TestContentChangedAt_InsertRunsNoTriggerOnAFreshDatabase(t *testing.T) {
	testutil.SkipIfPostgres(t, "PostgreSQL stamps in a BEFORE trigger, which needs no second write")
	require := require.New(t)
	assert := assert.New(t)

	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource("gmail", "insert-cost@example.com")
	require.NoError(err)
	conv, err := st.EnsureConversationWithType(src.ID, "insert-cost", "email_thread", "Insert cost")
	require.NoError(err)

	const insert = `INSERT INTO messages (source_id, source_message_id, conversation_id, message_type, subject)
		 VALUES (?,?,?,?,?)`

	assert.Zero(insertMessagesTriggerPrograms(t, st, insert, src.ID, "explain-only", conv, "email", "x"),
		"an INSERT into messages must compile no trigger subprogram on a fresh database: "+
			"SQLite opens a statement journal for every INSERT that has one, whether or not the "+
			"trigger body runs")

	// One pinned connection: total_changes() is per-connection and the pool
	// would otherwise hand the two readings out of different sessions.
	ctx := context.Background()
	conn, err := st.DB().Conn(ctx)
	require.NoError(err)
	defer func() { _ = conn.Close() }()

	totalChanges := func() int64 {
		var n int64
		require.NoError(conn.QueryRowContext(ctx, `SELECT total_changes()`).Scan(&n))
		return n
	}

	before := totalChanges()
	_, err = conn.ExecContext(ctx, insert, src.ID, "insert-cost-1", conv, "email", "insert cost")
	require.NoError(err)
	written := totalChanges() - before

	var stamp sql.NullString
	require.NoError(conn.QueryRowContext(ctx,
		`SELECT CAST(content_changed_at AS TEXT) FROM messages WHERE source_message_id = 'insert-cost-1'`).
		Scan(&stamp))
	require.True(stamp.Valid, "the inserted row must still get a watermark")
	assert.Regexp(contentChangedStampShape, stamp.String)

	assert.EqualValues(1, written,
		"inserting one message must write one row; the watermark has to come from the column "+
			"DEFAULT rather than from an AFTER INSERT trigger that re-UPDATEs the row")
}

// TestContentChangedAt_UpgradedDatabaseKeepsTheInsertTrigger is the other half
// of the DEFAULT optimisation. A database upgraded by ALTER TABLE ADD COLUMN
// cannot carry a non-constant DEFAULT, so dropping the INSERT trigger there
// would leave every row inserted after the upgrade with a NULL watermark —
// permanently invisible to the feed, since the backfill has already marked
// itself applied.
func TestContentChangedAt_UpgradedDatabaseKeepsTheInsertTrigger(t *testing.T) {
	testutil.SkipIfPostgres(t, "the DEFAULT/trigger split is a SQLite ALTER TABLE limitation")
	require := require.New(t)
	assert := assert.New(t)

	st := testutil.NewTestStore(t)
	dropContentChangedAtColumn(t, st)
	clearContentChangedBackfillLedger(t, st)
	require.NoError(st.InitSchema())

	src, err := st.GetOrCreateSource("gmail", "upgraded@example.com")
	require.NoError(err)
	conv, err := st.EnsureConversationWithType(src.ID, "upgraded", "email_thread", "Upgraded")
	require.NoError(err)
	_, err = st.DB().Exec(
		`INSERT INTO messages (source_id, source_message_id, conversation_id, message_type)
		 VALUES (?,?,?,?)`, src.ID, "upgraded-1", conv, "email")
	require.NoError(err)

	var stamp sql.NullString
	require.NoError(st.DB().QueryRow(
		`SELECT CAST(content_changed_at AS TEXT) FROM messages WHERE source_message_id = 'upgraded-1'`).
		Scan(&stamp))
	require.True(stamp.Valid,
		"a row inserted after an ALTER TABLE upgrade must still be stamped: the column has no "+
			"DEFAULT there, so the INSERT trigger is the only writer")
	assert.Regexp(contentChangedStampShape, stamp.String)
}

// rewriteMessagesContentChangedAtDefault replaces the DEFAULT expression on
// messages.content_changed_at in the stored schema.
//
// SQLite has no ALTER COLUMN, and ADD COLUMN refuses a non-constant default, so
// the only way to reach this state — from a test or from an operator with a
// SQLite shell — is to rewrite sqlite_master under PRAGMA writable_schema. All
// three statements run on ONE pinned connection: writable_schema is a
// per-connection setting and the pool would otherwise hand the UPDATE to a
// connection that still refuses it.
func rewriteMessagesContentChangedAtDefault(t *testing.T, st *store.Store, want string) {
	t.Helper()
	ctx := context.Background()
	conn, err := st.DB().Conn(ctx)
	require.NoError(t, err, "pin a connection")
	defer func() { _ = conn.Close() }()

	const read = `SELECT sql FROM sqlite_master WHERE type = 'table' AND name = 'messages'`
	var schema string
	require.NoError(t, conn.QueryRowContext(ctx, read).Scan(&schema),
		"read the messages schema")

	// Line-anchored, because the default expression itself contains a comma:
	// strftime('%Y-%m-%d %H:%M:%f','now'). The final comma is required and
	// captured separately so the rewrite cannot remove the column delimiter.
	declaration := regexp.MustCompile(`(?m)^(\s*content_changed_at DATETIME DEFAULT ).*(,)(\r?)$`)
	rewritten := declaration.ReplaceAllStringFunc(schema, func(line string) string {
		parts := declaration.FindStringSubmatch(line)
		return parts[1] + want + parts[2] + parts[3]
	})
	require.NotEqual(t, schema, rewritten,
		"the messages schema must declare a content_changed_at default to rewrite")

	for _, stmt := range []string{`PRAGMA writable_schema=ON`, "", `PRAGMA writable_schema=OFF`} {
		if stmt == "" {
			_, err = conn.ExecContext(ctx,
				`UPDATE sqlite_master SET sql = ? WHERE type = 'table' AND name = 'messages'`,
				rewritten)
			require.NoError(t, err, "rewrite the messages schema")
			continue
		}
		_, err = conn.ExecContext(ctx, stmt)
		require.NoErrorf(t, err, "exec %s", stmt)
	}
}

// readContentChangedAtDefault reports the DEFAULT SQLite records for the column.
func readContentChangedAtDefault(t *testing.T, st *store.Store) string {
	t.Helper()
	var dflt sql.NullString
	require.NoError(t, st.DB().QueryRow(
		`SELECT dflt_value FROM pragma_table_info('messages') WHERE name = 'content_changed_at'`).
		Scan(&dflt), "read content_changed_at default")
	return dflt.String
}

// TestContentChangedAt_NoncanonicalDefaultIsRejected covers the archive whose
// content_changed_at DEFAULT is neither absent nor the one this build writes.
//
// The INSERT trigger cannot rescue such an archive, which is what makes it a
// data-loss case rather than a slow one. The trigger fires only WHEN
// NEW.content_changed_at IS NULL, and SQLite applies a non-NULL column DEFAULT
// BEFORE the trigger runs — so any drifted non-NULL default stays authoritative
// and keeps writing a value with the wrong meaning or shape. The feed compares
// SQLite timestamps lexically, so once both shapes are in the table a cursor
// sorts rows into the wrong place and silently skips or repeats changes, with
// nothing anywhere reporting it.
//
// So the archive is refused at open, before a single row can be written with the
// wrong shape. Loud and immediate beats a feed that quietly loses records.
func TestContentChangedAt_NoncanonicalDefaultIsRejected(t *testing.T) {
	testutil.SkipIfPostgres(t, "the DEFAULT/trigger interaction is a SQLite one")
	require := require.New(t)
	assert := assert.New(t)

	dbPath := filepath.Join(t.TempDir(), "drifted-default.db")
	seed, err := store.OpenForTest(dbPath)
	require.NoError(err, "open seed store")
	require.NoError(seed.InitSchema(), "seed InitSchema")
	const driftedDefault = `'2000-01-01 00:00:00'`
	rewriteMessagesContentChangedAtDefault(t, seed, driftedDefault)
	require.NoError(seed.Close(), "close seed store")

	drifted, err := store.OpenForTest(dbPath)
	require.NoError(err, "reopen the drifted archive")
	require.Equal(driftedDefault, readContentChangedAtDefault(t, drifted),
		"precondition: the archive must really carry a drifted default")

	err = drifted.InitSchema()
	require.Error(err,
		"an archive whose content_changed_at default is not the one this build writes "+
			"must be refused, not opened with a trigger that cannot override the default")
	assert.Contains(err.Error(), "content_changed_at",
		"the error must name the column an operator has to repair")
	assert.Contains(err.Error(), driftedDefault,
		"and the default it found")
	assert.Contains(err.Error(), "strftime",
		"and the default it expects, so the repair does not need this source file")
	require.NoError(drifted.Close(), "close the drifted archive")

	// The remedy: put the default back, and the archive opens and stamps in the
	// one shape the feed's lexical cursor can order. The rewrite gets its own
	// handle because a sqlite_master edit only reaches connections opened after
	// it — which is also how an operator would do the repair, with the daemon
	// stopped.
	repairHandle, err := store.OpenForTest(dbPath)
	require.NoError(err, "reopen for repair")
	rewriteMessagesContentChangedAtDefault(t, repairHandle,
		`(strftime('%Y-%m-%d %H:%M:%f','now'))`)
	require.NoError(repairHandle.Close(), "close the repair handle")

	repaired, err := store.OpenForTest(dbPath)
	require.NoError(err, "reopen the repaired archive")
	t.Cleanup(func() { _ = repaired.Close() })
	require.Equal(`strftime('%Y-%m-%d %H:%M:%f','now')`,
		readContentChangedAtDefault(t, repaired), "precondition: the repair landed")
	require.NoError(repaired.InitSchema(), "a repaired archive must open")

	src, err := repaired.GetOrCreateSource("gmail", "repaired@example.com")
	require.NoError(err)
	conv, err := repaired.EnsureConversationWithType(src.ID, "repaired", "email_thread", "Repaired")
	require.NoError(err)
	_, err = repaired.DB().Exec(
		`INSERT INTO messages (source_id, source_message_id, conversation_id, message_type)
		 VALUES (?,?,?,?)`, src.ID, "repaired-1", conv, "email")
	require.NoError(err)

	var stamp sql.NullString
	require.NoError(repaired.DB().QueryRow(
		`SELECT CAST(content_changed_at AS TEXT) FROM messages WHERE source_message_id = 'repaired-1'`).
		Scan(&stamp))
	require.True(stamp.Valid, "the repaired archive must still stamp new rows")
	assert.Regexp(contentChangedStampShape, stamp.String,
		"and stamp them in the one shape the feed's lexical cursor can order")
}

// TestContentChangedAt_NullWatermarkIsStamped pins the null-safe comparison in
// the UPDATE trigger's yield guard. With `=` (SQLite) or `=`/`<>` (PostgreSQL)
// instead of IS / IS NOT DISTINCT FROM, `OLD.content_changed_at = NEW.content_changed_at`
// evaluates to NULL for a row whose watermark is NULL, the WHEN clause is never
// satisfied, and that row is stranded outside the feed forever. Rows can carry a
// NULL watermark on an archive copied or restored from a pre-backfill database.
func TestContentChangedAt_NullWatermarkIsStamped(t *testing.T) {
	st := testutil.NewTestStore(t)
	id := seedMessage(t, st, 1)

	// Naming only content_changed_at keeps the UPDATE OF list from matching, so
	// this write establishes the NULL precondition instead of being re-stamped.
	_, err := st.DB().Exec(
		st.Rebind(`UPDATE messages SET content_changed_at = NULL WHERE id = ?`), id)
	require.NoError(t, err, "clear content_changed_at")

	_, err = st.DB().Exec(
		st.Rebind(`UPDATE messages SET subject = ? WHERE id = ?`), "changed subject", id)
	require.NoError(t, err, "update subject")

	// readContentChangedAt fails on NULL, which is the assertion.
	assert.NotEmpty(t, readContentChangedAt(t, st, id),
		"a content update must stamp a row whose watermark is NULL: the guard has to be "+
			"null-safe or the row never rejoins the feed")
}
