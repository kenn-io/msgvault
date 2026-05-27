package store

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	assertpkg "github.com/stretchr/testify/assert"
	requirepkg "github.com/stretchr/testify/require"
)

// TestEnsureParticipantsPhoneUniqueIndex_LegacyNonUnique simulates an
// upgraded database whose idx_participants_phone was created as a
// NON-unique partial index (the shape before the schema bumped it to
// UNIQUE). The migration must:
//
//  1. recognise that the migration has not yet run (applied_migrations
//     entry absent),
//  2. dedupe duplicate phone rows by re-pointing FKs from losers to
//     the winner (lowest id), then deleting losers,
//  3. drop the legacy non-unique index and create a UNIQUE one,
//  4. mark the migration applied so subsequent InitSchema calls are
//     no-ops.
//
// The post-state is verified end-to-end: only one participant row per
// phone, FKs were preserved (no orphan recipients), and a second
// EnsureParticipantByPhone with the same number returns the existing
// id (proving ON CONFLICT (phone_number) now binds to a real UNIQUE
// constraint).
func TestEnsureParticipantsPhoneUniqueIndex_LegacyNonUnique(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	// SQLite-only: this test pokes at sqlite_master and reseats the
	// applied_migrations row directly. The PG equivalent of the
	// migration is exercised by TestEnsureParticipantByPhone_Concurrent
	// (which would error at the first concurrent insert without a
	// real UNIQUE constraint).
	dbPath := filepath.Join(t.TempDir(), "phone_unique.db")
	st, err := Open(dbPath)
	require.NoError(err, "Open")
	t.Cleanup(func() { _ = st.Close() })
	require.NoError(st.InitSchema(), "InitSchema")

	// Roll back to the "legacy" state: clear the applied_migrations
	// sentinel, drop the unique index, recreate as non-unique.
	_, err = st.db.Exec(
		`DELETE FROM applied_migrations WHERE name = ?`, migrationPhoneUniqueIndex,
	)
	require.NoError(err, "clear migration sentinel")
	_, err = st.db.Exec(`DROP INDEX IF EXISTS idx_participants_phone`)
	require.NoError(err, "drop unique idx")
	_, err = st.db.Exec(`
		CREATE INDEX idx_participants_phone ON participants(phone_number)
		    WHERE phone_number IS NOT NULL
	`)
	require.NoError(err, "create legacy non-unique idx")

	// Seed two duplicate-phone participants directly (the public API
	// no longer allows this, which is exactly the bug the unique
	// index closes). Use a source + conversation + messages so the
	// FK-repoint paths are also exercised.
	source, err := st.GetOrCreateSource("imessage", "+15555550100")
	require.NoError(err, "GetOrCreateSource")
	convID, err := st.EnsureConversation(source.ID, "thread-phone-dup", "")
	require.NoError(err, "EnsureConversation")

	// Two raw inserts that share +15555551234. id1 wins, id2 loses.
	insertParticipant := func(phone, displayName string) int64 {
		t.Helper()
		var id int64
		err := st.db.QueryRow(`
			INSERT INTO participants (phone_number, display_name, created_at, updated_at)
			VALUES (?, ?, datetime('now'), datetime('now'))
			RETURNING id
		`, phone, displayName).Scan(&id)
		require.NoError(err, "insert participant %s", phone)
		return id
	}
	winner := insertParticipant("+15555551234", "Alice")
	loser := insertParticipant("+15555551234", "Alice (dup)")

	// Make sure the legacy schema actually permitted the duplicate.
	require.NotEqual(winner, loser, "seeded participants must have distinct ids")

	// Attach FK references to BOTH participants so we can prove the
	// repoint+dedupe logic runs end-to-end:
	//   - a message recipient on each (different message → no key
	//     conflict, both rows should survive the repoint onto winner)
	//   - a recipient where winner+loser appear on the SAME message
	//     with the same type → the loser row must be removed before
	//     the repoint (UNIQUE(message_id, participant_id, recipient_type))
	//   - a sender_id on a third message pointing to loser → plain
	//     UPDATE
	msgA, err := st.UpsertMessage(&Message{
		ConversationID:  convID,
		SourceID:        source.ID,
		SourceMessageID: "msg-A",
		MessageType:     "imessage",
		Subject:         sql.NullString{String: "A", Valid: true},
		SizeEstimate:    100,
	})
	require.NoError(err, "UpsertMessage A")
	msgB, err := st.UpsertMessage(&Message{
		ConversationID:  convID,
		SourceID:        source.ID,
		SourceMessageID: "msg-B",
		MessageType:     "imessage",
		Subject:         sql.NullString{String: "B", Valid: true},
		SizeEstimate:    100,
	})
	require.NoError(err, "UpsertMessage B")
	msgC, err := st.UpsertMessage(&Message{
		ConversationID:  convID,
		SourceID:        source.ID,
		SourceMessageID: "msg-C",
		MessageType:     "imessage",
		Subject:         sql.NullString{String: "C", Valid: true},
		SizeEstimate:    100,
	})
	require.NoError(err, "UpsertMessage C")

	exec := func(q string, args ...any) {
		t.Helper()
		_, err := st.db.Exec(q, args...)
		require.NoError(err, "exec %q", q)
	}
	// Recipient on msg-A: only loser → survives, repoints to winner.
	exec(`INSERT INTO message_recipients (message_id, participant_id, recipient_type) VALUES (?, ?, 'to')`,
		msgA, loser)
	// Recipient on msg-B: both winner and loser as 'to' → loser must
	// be deleted before the repoint to avoid violating the UNIQUE.
	exec(`INSERT INTO message_recipients (message_id, participant_id, recipient_type) VALUES (?, ?, 'to')`,
		msgB, winner)
	exec(`INSERT INTO message_recipients (message_id, participant_id, recipient_type) VALUES (?, ?, 'to')`,
		msgB, loser)
	// Sender on msg-C: loser → plain UPDATE onto winner.
	exec(`UPDATE messages SET sender_id = ? WHERE id = ?`, loser, msgC)

	// Run the migration we are testing.
	require.NoError(st.ensureParticipantsPhoneUniqueIndex(), "ensureParticipantsPhoneUniqueIndex")

	// 1) Loser row must be gone.
	var loserCount int
	require.NoError(st.db.QueryRow(`SELECT COUNT(*) FROM participants WHERE id = ?`, loser).Scan(&loserCount),
		"count loser")
	assert.Equal(0, loserCount, "loser participant %d still present after merge", loser)

	// 2) Exactly one participant for the duplicated phone.
	var phoneCount int
	require.NoError(st.db.QueryRow(
		`SELECT COUNT(*) FROM participants WHERE phone_number = ?`,
		"+15555551234",
	).Scan(&phoneCount), "count duplicates")
	assert.Equal(1, phoneCount, "phone +15555551234 row count after dedupe")

	// 3) msg-A recipient now points at winner (repoint succeeded).
	var msgAParticipant int64
	require.NoError(st.db.QueryRow(
		`SELECT participant_id FROM message_recipients WHERE message_id = ? AND recipient_type = 'to'`,
		msgA,
	).Scan(&msgAParticipant), "read msg-A recipient")
	assert.Equal(winner, msgAParticipant, "msg-A recipient")

	// 4) msg-B has exactly one 'to' recipient (winner) — the loser
	//    row was collapsed into the winner row by the dedupe step.
	var msgBCount int
	require.NoError(st.db.QueryRow(
		`SELECT COUNT(*) FROM message_recipients WHERE message_id = ? AND recipient_type = 'to'`,
		msgB,
	).Scan(&msgBCount), "count msg-B recipients")
	assert.Equal(1, msgBCount, "msg-B 'to' recipient count")
	var msgBParticipant int64
	require.NoError(st.db.QueryRow(
		`SELECT participant_id FROM message_recipients WHERE message_id = ? AND recipient_type = 'to'`,
		msgB,
	).Scan(&msgBParticipant), "read msg-B recipient")
	assert.Equal(winner, msgBParticipant, "msg-B recipient")

	// 5) msg-C sender_id repointed to winner.
	var msgCSender sql.NullInt64
	require.NoError(st.db.QueryRow(`SELECT sender_id FROM messages WHERE id = ?`, msgC).Scan(&msgCSender),
		"read msg-C sender")
	assert.True(msgCSender.Valid, "msg-C sender = %+v, want winner %d", msgCSender, winner)
	assert.Equal(winner, msgCSender.Int64, "msg-C sender")

	// 6) The index is now UNIQUE. Verify via sqlite_master.
	var sqlDef string
	require.NoError(st.db.QueryRow(
		`SELECT sql FROM sqlite_master WHERE type = 'index' AND name = 'idx_participants_phone'`,
	).Scan(&sqlDef), "read idx_participants_phone sql")
	assert.Contains(strings.ToUpper(sqlDef), "UNIQUE",
		"idx_participants_phone is not UNIQUE after migration; got %q", sqlDef)

	// 7) Migration sentinel is set, so a re-run is a no-op.
	applied, err := st.IsMigrationApplied(migrationPhoneUniqueIndex)
	require.NoError(err, "IsMigrationApplied")
	assert.True(applied, "migration sentinel not set after successful run")
	require.NoError(st.ensureParticipantsPhoneUniqueIndex(),
		"re-run of ensureParticipantsPhoneUniqueIndex must be a no-op")

	// 8) Public API: EnsureParticipantByPhone with the duplicated
	//    number returns the winner id (the unique index is now
	//    actually enforcing ON CONFLICT (phone_number)).
	gotID, err := st.EnsureParticipantByPhone("+15555551234", "Alice (later call)", "imessage")
	require.NoError(err, "EnsureParticipantByPhone")
	assert.Equal(winner, gotID, "EnsureParticipantByPhone returned id %d, want winner %d (ON CONFLICT must find the unique index)", gotID, winner)
}
