package store

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
	require := require.New(t)
	assert := assert.New(t)
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

// TestEnsureParticipantsPhoneUniqueIndex_RewritesLinkEdges covers the
// migration's underlying mergeParticipant path (used by
// dedupeParticipantsByPhone), which is a second, tx-level merge
// implementation distinct from the public MergeParticipants. It must be
// link-aware the same way: a link edge on the loser participant must
// survive the merge by repointing onto the winner rather than being
// silently dropped by participant_links' ON DELETE CASCADE, and the
// identity revision must bump since the merge changed the link graph.
func TestEnsureParticipantsPhoneUniqueIndex_RewritesLinkEdges(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	dbPath := filepath.Join(t.TempDir(), "phone_unique_links.db")
	st, err := Open(dbPath)
	require.NoError(err, "Open")
	t.Cleanup(func() { _ = st.Close() })
	require.NoError(st.InitSchema(), "InitSchema")

	_, err = st.db.Exec(`DELETE FROM applied_migrations WHERE name = ?`, migrationPhoneUniqueIndex)
	require.NoError(err, "clear migration sentinel")
	_, err = st.db.Exec(`DROP INDEX IF EXISTS idx_participants_phone`)
	require.NoError(err, "drop unique idx")
	_, err = st.db.Exec(`
		CREATE INDEX idx_participants_phone ON participants(phone_number)
		    WHERE phone_number IS NOT NULL
	`)
	require.NoError(err, "create legacy non-unique idx")

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
	third, err := st.EnsureParticipant("carol@example.com", "Carol", "example.com")
	require.NoError(err, "EnsureParticipant third")

	// Link the loser (not the winner) to a third participant, mirroring a
	// user having asserted "loser and carol are the same person" before an
	// unrelated phone-based dedupe absorbs loser into winner.
	_, err = st.LinkParticipants(loser, third)
	require.NoError(err, "LinkParticipants loser-third")
	revBeforeMerge, err := st.IdentityRevision()
	require.NoError(err, "IdentityRevision before merge")
	acctRevBeforeMerge, err := st.AccountIdentityRevision()
	require.NoError(err, "AccountIdentityRevision before merge")

	require.NoError(st.ensureParticipantsPhoneUniqueIndex(), "ensureParticipantsPhoneUniqueIndex")

	revAfter, err := st.IdentityRevision()
	require.NoError(err, "IdentityRevision after merge")
	assert.Equal(revBeforeMerge+1, revAfter, "dedupe merge must bump identity revision when it touches links")
	acctRevAfter, err := st.AccountIdentityRevision()
	require.NoError(err, "AccountIdentityRevision after merge")
	assert.Equal(acctRevBeforeMerge+1, acctRevAfter,
		"dedupe merge must bump the account identity revision (it repoints messages.sender_id)")

	clusters, err := st.ParticipantClusters()
	require.NoError(err, "ParticipantClusters")
	canonical := min(winner, third)
	assert.Equal(map[int64]int64{winner: canonical, third: canonical}, clusters,
		"link must repoint from loser to winner")
}

// TestEnsureParticipantsPhoneUniqueIndex_PreservesMetadata verifies that
// the dedupe merge does not discard contact metadata held only by a
// loser row: before deleting each loser, the winner's empty
// email_address, domain, and display_name are filled from it (mirroring
// MergeParticipants). email_address is UNIQUE, so the transfer must
// release the loser's value first — the migration completing without a
// constraint error is part of what this test proves.
//
// Precedence rule: losers merge in ascending-id order and only fill
// fields still empty on the winner, so for each field the lowest-id
// participant holding a value wins.
func TestEnsureParticipantsPhoneUniqueIndex_PreservesMetadata(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	dbPath := filepath.Join(t.TempDir(), "phone_unique_meta.db")
	st, err := Open(dbPath)
	require.NoError(err, "Open")
	t.Cleanup(func() { _ = st.Close() })
	require.NoError(st.InitSchema(), "InitSchema")

	// Roll back to the "legacy" state so duplicate phones can be seeded.
	_, err = st.db.Exec(`DELETE FROM applied_migrations WHERE name = ?`, migrationPhoneUniqueIndex)
	require.NoError(err, "clear migration sentinel")
	_, err = st.db.Exec(`DROP INDEX IF EXISTS idx_participants_phone`)
	require.NoError(err, "drop unique idx")
	_, err = st.db.Exec(`
		CREATE INDEX idx_participants_phone ON participants(phone_number)
		    WHERE phone_number IS NOT NULL
	`)
	require.NoError(err, "create legacy non-unique idx")

	insertParticipant := func(phone, email, domain, displayName string) int64 {
		t.Helper()
		var id int64
		err := st.db.QueryRow(`
			INSERT INTO participants (phone_number, email_address, domain, display_name, created_at, updated_at)
			VALUES (?, NULLIF(?, ''), NULLIF(?, ''), NULLIF(?, ''), datetime('now'), datetime('now'))
			RETURNING id
		`, phone, email, domain, displayName).Scan(&id)
		require.NoError(err, "insert participant %s", phone)
		return id
	}
	readMeta := func(id int64) (email, domain, displayName string) {
		t.Helper()
		err := st.db.QueryRow(`
			SELECT COALESCE(email_address, ''), COALESCE(domain, ''), COALESCE(display_name, '')
			  FROM participants WHERE id = ?
		`, id).Scan(&email, &domain, &displayName)
		require.NoError(err, "read participant %d metadata", id)
		return email, domain, displayName
	}

	// (a) Winner sparse, loser rich: the loser's email (UNIQUE column),
	// domain, and display_name must move onto the winner.
	sparseWinner := insertParticipant("+15555550001", "", "", "")
	richLoser := insertParticipant("+15555550001", "bob@example.com", "example.com", "Bob")

	// (b) Winner already populated: the loser's values are discarded.
	richWinner := insertParticipant("+15555550002", "carol@example.com", "example.com", "Carol")
	ignoredLoser := insertParticipant("+15555550002", "carol.alt@example.org", "example.org", "Carol Alt")

	// (c) Multiple losers: loser1 (lower id) fills email + display_name,
	// then loser2 fills only the still-empty domain.
	multiWinner := insertParticipant("+15555550003", "", "", "")
	multiLoser1 := insertParticipant("+15555550003", "dan@example.com", "", "Dan")
	multiLoser2 := insertParticipant("+15555550003", "dana@example.org", "example.org", "Dana")

	require.NoError(st.ensureParticipantsPhoneUniqueIndex(), "ensureParticipantsPhoneUniqueIndex")

	for _, loser := range []int64{richLoser, ignoredLoser, multiLoser1, multiLoser2} {
		var count int
		require.NoError(st.db.QueryRow(`SELECT COUNT(*) FROM participants WHERE id = ?`, loser).Scan(&count),
			"count loser %d", loser)
		assert.Equal(0, count, "loser participant %d still present after merge", loser)
	}

	email, domain, name := readMeta(sparseWinner)
	assert.Equal("bob@example.com", email, "sparse winner email")
	assert.Equal("example.com", domain, "sparse winner domain")
	assert.Equal("Bob", name, "sparse winner display_name")

	email, domain, name = readMeta(richWinner)
	assert.Equal("carol@example.com", email, "populated winner keeps its email")
	assert.Equal("example.com", domain, "populated winner keeps its domain")
	assert.Equal("Carol", name, "populated winner keeps its display_name")

	email, domain, name = readMeta(multiWinner)
	assert.Equal("dan@example.com", email, "multi-loser winner email from lowest-id loser")
	assert.Equal("example.org", domain, "multi-loser winner domain from the only loser holding one")
	assert.Equal("Dan", name, "multi-loser winner display_name from lowest-id loser")
}
