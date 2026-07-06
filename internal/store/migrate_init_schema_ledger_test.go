package store

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestInitSchema_OneShotMigrationsGatedOnLedger verifies the two data
// migrations InitSchema used to re-verify on every start (the attachments
// dedupe and the messages.last_modified backfill — a full messages-table
// scan, the dominant daemon-startup cost on a large archive) are gated on
// the applied_migrations ledger:
//
//  1. a fresh InitSchema runs them once and records both sentinels,
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
		migrationMessagesLastModifiedBackfill,
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

	lastModifiedIsNull := func() bool {
		var isNull bool
		require.NoError(st.db.QueryRow(
			`SELECT last_modified IS NULL FROM messages WHERE id = ?`, msgID,
		).Scan(&isNull), "read last_modified")
		return isNull
	}

	// Sentinel present: re-running InitSchema must skip the backfill scan.
	require.NoError(st.InitSchema(), "second InitSchema")
	assert.True(lastModifiedIsNull(),
		"backfill must not run while its sentinel is recorded")

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
}
