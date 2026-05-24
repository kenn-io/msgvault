package store

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
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
	// SQLite-only: this test pokes at sqlite_master and reseats the
	// applied_migrations row directly. The PG equivalent of the
	// migration is exercised by TestEnsureParticipantByPhone_Concurrent
	// (which would error at the first concurrent insert without a
	// real UNIQUE constraint).
	dbPath := filepath.Join(t.TempDir(), "phone_unique.db")
	st, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.InitSchema(); err != nil {
		t.Fatalf("InitSchema: %v", err)
	}

	// Roll back to the "legacy" state: clear the applied_migrations
	// sentinel, drop the unique index, recreate as non-unique.
	if _, err := st.db.Exec(
		`DELETE FROM applied_migrations WHERE name = ?`, migrationPhoneUniqueIndex,
	); err != nil {
		t.Fatalf("clear migration sentinel: %v", err)
	}
	if _, err := st.db.Exec(`DROP INDEX IF EXISTS idx_participants_phone`); err != nil {
		t.Fatalf("drop unique idx: %v", err)
	}
	if _, err := st.db.Exec(`
		CREATE INDEX idx_participants_phone ON participants(phone_number)
		    WHERE phone_number IS NOT NULL
	`); err != nil {
		t.Fatalf("create legacy non-unique idx: %v", err)
	}

	// Seed two duplicate-phone participants directly (the public API
	// no longer allows this, which is exactly the bug the unique
	// index closes). Use a source + conversation + messages so the
	// FK-repoint paths are also exercised.
	source, err := st.GetOrCreateSource("imessage", "+15555550100")
	if err != nil {
		t.Fatalf("GetOrCreateSource: %v", err)
	}
	convID, err := st.EnsureConversation(source.ID, "thread-phone-dup", "")
	if err != nil {
		t.Fatalf("EnsureConversation: %v", err)
	}

	// Two raw inserts that share +15555551234. id1 wins, id2 loses.
	insertParticipant := func(phone, displayName string) int64 {
		t.Helper()
		var id int64
		err := st.db.QueryRow(`
			INSERT INTO participants (phone_number, display_name, created_at, updated_at)
			VALUES (?, ?, datetime('now'), datetime('now'))
			RETURNING id
		`, phone, displayName).Scan(&id)
		if err != nil {
			t.Fatalf("insert participant %s: %v", phone, err)
		}
		return id
	}
	winner := insertParticipant("+15555551234", "Alice")
	loser := insertParticipant("+15555551234", "Alice (dup)")

	// Make sure the legacy schema actually permitted the duplicate.
	if winner == loser {
		t.Fatalf("seeded participants must have distinct ids (got %d, %d)", winner, loser)
	}

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
	if err != nil {
		t.Fatalf("UpsertMessage A: %v", err)
	}
	msgB, err := st.UpsertMessage(&Message{
		ConversationID:  convID,
		SourceID:        source.ID,
		SourceMessageID: "msg-B",
		MessageType:     "imessage",
		Subject:         sql.NullString{String: "B", Valid: true},
		SizeEstimate:    100,
	})
	if err != nil {
		t.Fatalf("UpsertMessage B: %v", err)
	}
	msgC, err := st.UpsertMessage(&Message{
		ConversationID:  convID,
		SourceID:        source.ID,
		SourceMessageID: "msg-C",
		MessageType:     "imessage",
		Subject:         sql.NullString{String: "C", Valid: true},
		SizeEstimate:    100,
	})
	if err != nil {
		t.Fatalf("UpsertMessage C: %v", err)
	}

	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := st.db.Exec(q, args...); err != nil {
			t.Fatalf("exec %q: %v", q, err)
		}
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
	if err := st.ensureParticipantsPhoneUniqueIndex(); err != nil {
		t.Fatalf("ensureParticipantsPhoneUniqueIndex: %v", err)
	}

	// 1) Loser row must be gone.
	var loserCount int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM participants WHERE id = ?`, loser).Scan(&loserCount); err != nil {
		t.Fatalf("count loser: %v", err)
	}
	if loserCount != 0 {
		t.Errorf("loser participant %d still present after merge", loser)
	}

	// 2) Exactly one participant for the duplicated phone.
	var phoneCount int
	if err := st.db.QueryRow(
		`SELECT COUNT(*) FROM participants WHERE phone_number = ?`,
		"+15555551234",
	).Scan(&phoneCount); err != nil {
		t.Fatalf("count duplicates: %v", err)
	}
	if phoneCount != 1 {
		t.Errorf("phone +15555551234 row count = %d, want 1 after dedupe", phoneCount)
	}

	// 3) msg-A recipient now points at winner (repoint succeeded).
	var msgAParticipant int64
	if err := st.db.QueryRow(
		`SELECT participant_id FROM message_recipients WHERE message_id = ? AND recipient_type = 'to'`,
		msgA,
	).Scan(&msgAParticipant); err != nil {
		t.Fatalf("read msg-A recipient: %v", err)
	}
	if msgAParticipant != winner {
		t.Errorf("msg-A recipient = %d, want winner %d", msgAParticipant, winner)
	}

	// 4) msg-B has exactly one 'to' recipient (winner) — the loser
	//    row was collapsed into the winner row by the dedupe step.
	var msgBCount int
	if err := st.db.QueryRow(
		`SELECT COUNT(*) FROM message_recipients WHERE message_id = ? AND recipient_type = 'to'`,
		msgB,
	).Scan(&msgBCount); err != nil {
		t.Fatalf("count msg-B recipients: %v", err)
	}
	if msgBCount != 1 {
		t.Errorf("msg-B 'to' recipient count = %d, want 1", msgBCount)
	}
	var msgBParticipant int64
	if err := st.db.QueryRow(
		`SELECT participant_id FROM message_recipients WHERE message_id = ? AND recipient_type = 'to'`,
		msgB,
	).Scan(&msgBParticipant); err != nil {
		t.Fatalf("read msg-B recipient: %v", err)
	}
	if msgBParticipant != winner {
		t.Errorf("msg-B recipient = %d, want winner %d", msgBParticipant, winner)
	}

	// 5) msg-C sender_id repointed to winner.
	var msgCSender sql.NullInt64
	if err := st.db.QueryRow(`SELECT sender_id FROM messages WHERE id = ?`, msgC).Scan(&msgCSender); err != nil {
		t.Fatalf("read msg-C sender: %v", err)
	}
	if !msgCSender.Valid || msgCSender.Int64 != winner {
		t.Errorf("msg-C sender = %+v, want winner %d", msgCSender, winner)
	}

	// 6) The index is now UNIQUE. Verify via sqlite_master.
	var sqlDef string
	if err := st.db.QueryRow(
		`SELECT sql FROM sqlite_master WHERE type = 'index' AND name = 'idx_participants_phone'`,
	).Scan(&sqlDef); err != nil {
		t.Fatalf("read idx_participants_phone sql: %v", err)
	}
	if !strings.Contains(strings.ToUpper(sqlDef), "UNIQUE") {
		t.Errorf("idx_participants_phone is not UNIQUE after migration; got %q", sqlDef)
	}

	// 7) Migration sentinel is set, so a re-run is a no-op.
	applied, err := st.IsMigrationApplied(migrationPhoneUniqueIndex)
	if err != nil {
		t.Fatalf("IsMigrationApplied: %v", err)
	}
	if !applied {
		t.Errorf("migration sentinel not set after successful run")
	}
	if err := st.ensureParticipantsPhoneUniqueIndex(); err != nil {
		t.Errorf("re-run of ensureParticipantsPhoneUniqueIndex must be a no-op, got %v", err)
	}

	// 8) Public API: EnsureParticipantByPhone with the duplicated
	//    number returns the winner id (the unique index is now
	//    actually enforcing ON CONFLICT (phone_number)).
	gotID, err := st.EnsureParticipantByPhone("+15555551234", "Alice (later call)", "imessage")
	if err != nil {
		t.Fatalf("EnsureParticipantByPhone: %v", err)
	}
	if gotID != winner {
		t.Errorf("EnsureParticipantByPhone returned id %d, want winner %d (ON CONFLICT must find the unique index)", gotID, winner)
	}
}
