package store

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestInitSchema_OneShotMigrationsGatedOnLedger verifies the data
// migrations InitSchema used to re-verify on every start (the attachments
// dedupe, attribution provenance reconciliation, and the
// messages.last_modified and messages.content_changed_at backfills — full
// messages-table scans on a large archive) are gated on the applied_migrations
// ledger:
//
//  1. a fresh InitSchema runs them once and records all sentinels,
//  2. a later InitSchema with the sentinel present skips the work,
//  3. clearing the sentinel makes the next InitSchema run it again.
//
// SQLite-only: it reseats applied_migrations rows directly, mirroring
// TestEnsureParticipantsPhoneUniqueIndex_LegacyNonUnique.
func TestInitSchema_OneShotMigrationsGatedOnLedger(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	dbPath := filepath.Join(t.TempDir(), "ledger.db")
	st, err := Open(dbPath)
	require.NoError(err, "Open")
	t.Cleanup(func() { _ = st.Close() })
	require.NoError(st.InitSchema(), "first InitSchema")

	for _, name := range []string{
		migrationAttachmentsContentHashUnique,
		migrationIdentityMatchSourceSupport,
		migrationAttachmentOccurrenceUnique,
		migrationMessageAttributionProvenance,
		migrationMessagesLastModifiedBackfill,
		migrationMessagesContentChangedAtBackfill,
	} {
		applied, err := st.IsMigrationApplied(name)
		require.NoError(err, "IsMigrationApplied %s", name)
		assert.True(applied, "first InitSchema must record %s", name)
	}

	// Seed one message, then put it in the pre-migration state (NULL
	// last_modified). The explicit NULL write sticks: the last_modified
	// trigger's WHEN guard yields to explicit writes.
	source, err := st.GetOrCreateSource("gmail", "alice@example.com")
	require.NoError(err, "GetOrCreateSource")
	convID, err := st.EnsureConversation(source.ID, "t1", "")
	require.NoError(err, "EnsureConversation")
	msgID, err := st.UpsertMessage(&Message{
		SourceID: source.ID, ConversationID: convID,
		SourceMessageID: "m1", MessageType: "email",
		Subject: sql.NullString{String: "hello", Valid: true},
	})
	require.NoError(err, "UpsertMessage")
	_, err = st.db.Exec(
		`UPDATE messages SET last_modified = NULL WHERE id = ?`, msgID)
	require.NoError(err, "null out last_modified")

	// Same for content_changed_at, whose backfill is the most expensive of the
	// set: it walks the whole messages table in committed batches, minutes to
	// hours on a large archive. The statement names only content_changed_at, and
	// no UPDATE OF list matches that column, so no trigger re-stamps the row.
	_, err = st.db.Exec(
		`UPDATE messages SET content_changed_at = NULL WHERE id = ?`, msgID)
	require.NoError(err, "null out content_changed_at")

	columnIsNull := func(column string) bool {
		var isNull bool
		require.NoError(st.db.QueryRow(
			`SELECT `+column+` IS NULL FROM messages WHERE id = ?`, msgID,
		).Scan(&isNull), "read "+column)
		return isNull
	}
	lastModifiedIsNull := func() bool { return columnIsNull("last_modified") }
	contentChangedIsNull := func() bool { return columnIsNull("content_changed_at") }

	// Sentinel present: re-running InitSchema must skip the backfill scan.
	require.NoError(st.InitSchema(), "second InitSchema")
	assert.True(lastModifiedIsNull(),
		"backfill must not run while its sentinel is recorded")
	assert.True(contentChangedIsNull(),
		"the content_changed_at backfill must not rescan while its sentinel is "+
			"recorded: re-running it is the minutes-to-hours cost this ledger exists "+
			"to spend exactly once")

	// Sentinel cleared: the next InitSchema must backfill and re-record.
	_, err = st.db.Exec(
		`DELETE FROM applied_migrations WHERE name = ?`,
		migrationMessagesLastModifiedBackfill)
	require.NoError(err, "clear backfill sentinel")
	require.NoError(st.InitSchema(), "third InitSchema")
	assert.False(lastModifiedIsNull(),
		"backfill must run once its sentinel is cleared")
	applied, err := st.IsMigrationApplied(migrationMessagesLastModifiedBackfill)
	require.NoError(err, "IsMigrationApplied after re-run")
	assert.True(applied, "re-run must re-record the sentinel")
	assert.True(contentChangedIsNull(),
		"clearing the last_modified sentinel must not drag the content_changed_at "+
			"backfill along with it: the two are gated independently")

	// And the same for content_changed_at's own sentinel.
	_, err = st.db.Exec(
		`DELETE FROM applied_migrations WHERE name = ?`,
		migrationMessagesContentChangedAtBackfill)
	require.NoError(err, "clear content_changed_at backfill sentinel")
	require.NoError(st.InitSchema(), "fourth InitSchema")
	assert.False(contentChangedIsNull(),
		"the content_changed_at backfill must run once its sentinel is cleared")
	applied, err = st.IsMigrationApplied(migrationMessagesContentChangedAtBackfill)
	require.NoError(err, "IsMigrationApplied after content_changed_at re-run")
	assert.True(applied, "re-run must re-record the content_changed_at sentinel")
}
