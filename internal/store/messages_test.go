package store_test

import (
	"database/sql"
	"testing"
	"time"

	assertpkg "github.com/stretchr/testify/assert"
	requirepkg "github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestRecomputeConversationStats(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	st := testutil.NewTestStore(t)

	source, err := st.GetOrCreateSource("whatsapp", "+15550000001")
	require.NoError(err, "GetOrCreateSource")

	convID, err := st.EnsureConversationWithType(source.ID, "conv-1", "whatsapp_dm", "Test Chat")
	require.NoError(err, "EnsureConversationWithType")

	// Verify initial message_count is 0 (stats not maintained on insert).
	var initialCount int
	require.NoError(st.DB().QueryRow(
		st.Rebind(`SELECT message_count FROM conversations WHERE id = ?`), convID,
	).Scan(&initialCount), "initial message_count scan")
	assert.Equal(0, initialCount, "initial message_count")

	sentAt := time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC)
	msg1 := &store.Message{
		SourceID:        source.ID,
		SourceMessageID: "msg-1",
		ConversationID:  convID,
		MessageType:     "whatsapp",
		SentAt:          sql.NullTime{Time: sentAt, Valid: true},
		Snippet:         sql.NullString{String: "hello", Valid: true},
	}
	_, err = st.UpsertMessage(msg1)
	require.NoError(err, "UpsertMessage msg1")

	sentAt2 := sentAt.Add(time.Hour)
	msg2 := &store.Message{
		SourceID:        source.ID,
		SourceMessageID: "msg-2",
		ConversationID:  convID,
		MessageType:     "whatsapp",
		SentAt:          sql.NullTime{Time: sentAt2, Valid: true},
		Snippet:         sql.NullString{String: "world", Valid: true},
	}
	_, err = st.UpsertMessage(msg2)
	require.NoError(err, "UpsertMessage msg2")

	// msg3 has the SAME sent_at as msg2 but a different snippet.
	// After recompute, last_message_preview must come from msg3 (higher id),
	// exercising the `id DESC` tie-breaker in the SQL.
	msg3 := &store.Message{
		SourceID:        source.ID,
		SourceMessageID: "msg-3",
		ConversationID:  convID,
		MessageType:     "whatsapp",
		SentAt:          sql.NullTime{Time: sentAt2, Valid: true},
		Snippet:         sql.NullString{String: "tie-breaker", Valid: true},
	}
	_, err = st.UpsertMessage(msg3)
	require.NoError(err, "UpsertMessage msg3")

	// Add a conversation participant so participant_count is non-zero.
	participantID, err := st.EnsureParticipantByPhone("+15559876543", "Bob", "whatsapp")
	require.NoError(err, "EnsureParticipantByPhone")
	require.NoError(st.EnsureConversationParticipant(convID, participantID, "member"),
		"EnsureConversationParticipant")

	// Recompute and verify counts.
	require.NoError(st.RecomputeConversationStats(source.ID), "RecomputeConversationStats")

	var count int
	var participantCount int
	var lastMsgAt sql.NullTime
	var preview sql.NullString
	require.NoError(st.DB().QueryRow(
		st.Rebind(`SELECT message_count, participant_count, last_message_at, last_message_preview
		 FROM conversations WHERE id = ?`), convID,
	).Scan(&count, &participantCount, &lastMsgAt, &preview), "post-recompute scan")
	assert.Equal(3, count, "message_count")
	assert.Equal(1, participantCount, "participant_count")
	assert.True(lastMsgAt.Valid, "last_message_at should not be NULL")
	// msg2 and msg3 share the same sent_at; msg3 has the higher id, so its
	// snippet ("tie-breaker") must win via the `id DESC` tie-breaker.
	assert.True(preview.Valid, "preview valid")
	assert.Equal("tie-breaker", preview.String, "last_message_preview")

	// Idempotency: calling again should produce the same result.
	require.NoError(st.RecomputeConversationStats(source.ID), "RecomputeConversationStats (second call)")
	require.NoError(st.DB().QueryRow(
		st.Rebind(`SELECT message_count FROM conversations WHERE id = ?`), convID,
	).Scan(&count), "idempotency scan")
	assert.Equal(3, count, "idempotency message_count")
}

func TestEnsureParticipantByPhone_IdentifierType(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	st := testutil.NewTestStore(t)

	// Create participant via WhatsApp
	id1, err := st.EnsureParticipantByPhone("+15551234567", "Alice", "whatsapp")
	require.NoError(err, "EnsureParticipantByPhone(whatsapp)")
	require.NotZero(id1, "expected non-zero participant ID")

	// Same phone via iMessage — should return the same participant ID
	id2, err := st.EnsureParticipantByPhone("+15551234567", "Alice", "imessage")
	require.NoError(err, "EnsureParticipantByPhone(imessage)")
	assert.Equal(id1, id2, "imessage call should return same participant ID as whatsapp")

	// Both participant_identifiers rows should exist
	var count int
	err = st.DB().QueryRow(
		st.Rebind(`SELECT COUNT(*) FROM participant_identifiers WHERE participant_id = ?`),
		id1,
	).Scan(&count)
	require.NoError(err, "count participant_identifiers")
	assert.Equal(2, count, "participant_identifiers count")

	// Verify each identifier type is present
	for _, identType := range []string{"whatsapp", "imessage"} {
		var exists int
		err = st.DB().QueryRow(
			st.Rebind(`SELECT COUNT(*) FROM participant_identifiers
			 WHERE participant_id = ? AND identifier_type = ?`),
			id1, identType,
		).Scan(&exists)
		require.NoError(err, "check identifier_type %q", identType)
		assert.Equal(1, exists, "identifier_type %q", identType)
	}
}

func TestUpdateParticipantDisplayNameByEmail(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	st := testutil.NewTestStore(t)

	// Create an unnamed email participant (e.g. inserted by iMessage import
	// for an Apple ID handle).
	var pid int64
	require.NoError(st.DB().QueryRow(
		st.Rebind(`INSERT INTO participants (email_address) VALUES (?) RETURNING id`),
		"alice@example.com",
	).Scan(&pid), "insert participant")

	// Backfilling on an empty display_name succeeds.
	updated, err := st.UpdateParticipantDisplayNameByEmail("alice@example.com", "Alice Example")
	require.NoError(err, "UpdateParticipantDisplayNameByEmail")
	require.True(updated, "expected backfill to update existing participant")

	got := readDisplayName(t, st, pid)
	assert.Equal("Alice Example", got, "display_name")

	// Lookup is case-insensitive on the email.
	updatedMixed, err := st.UpdateParticipantDisplayNameByEmail("ALICE@example.com", "Should Not Overwrite")
	require.NoError(err, "UpdateParticipantDisplayNameByEmail (case)")
	assert.False(updatedMixed, "second update should not modify a non-empty display_name")
	assert.Equal("Alice Example", readDisplayName(t, st, pid), "display_name should not be overwritten")

	// Empty inputs are no-ops.
	updated, err = st.UpdateParticipantDisplayNameByEmail("", "X")
	require.NoError(err, "empty email err")
	assert.False(updated, "empty email updated")
	updated, err = st.UpdateParticipantDisplayNameByEmail("x@y.com", "")
	require.NoError(err, "empty name err")
	assert.False(updated, "empty name updated")

	// Unknown email is a no-op (does not create rows).
	updated, err = st.UpdateParticipantDisplayNameByEmail("nobody@example.com", "Nobody")
	require.NoError(err, "unknown email err")
	assert.False(updated, "unknown email updated")
}

func TestUpdateImessageParticipantDisplayNameByPhone(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	st := testutil.NewTestStore(t)

	// Case 1: legacy iMessage participant with display_name = phone_number.
	// Should be overwritten by the contact name.
	legacyID, err := st.EnsureParticipantByPhone("+15551111111", "+15551111111", "imessage")
	require.NoError(err, "seed legacy")

	// Case 2: iMessage participant already named by another source. Real
	// name must be preserved.
	namedID, err := st.EnsureParticipantByPhone("+15552222222", "Bob From Gmail", "imessage")
	require.NoError(err, "seed named")

	// Case 3: WhatsApp-only participant with display_name = phone_number.
	// Not iMessage, must NOT be touched (no imessage identifier exists).
	otherID, err := st.EnsureParticipantByPhone("+15553333333", "+15553333333", "whatsapp")
	require.NoError(err, "seed other")

	// Apply contact-name backfill.
	updated, err := st.UpdateImessageParticipantDisplayNameByPhone("+15551111111", "Alice Real")
	require.NoError(err, "backfill legacy")
	assert.True(updated, "legacy placeholder should be replaced")
	assert.Equal("Alice Real", readDisplayName(t, st, legacyID), "legacy display_name")

	updated, err = st.UpdateImessageParticipantDisplayNameByPhone("+15552222222", "Should Not Win")
	require.NoError(err, "backfill named")
	assert.False(updated, "real name from another source should be preserved")
	assert.Equal("Bob From Gmail", readDisplayName(t, st, namedID), "named display_name")

	updated, err = st.UpdateImessageParticipantDisplayNameByPhone("+15553333333", "Not Allowed")
	require.NoError(err, "backfill other")
	assert.False(updated, "non-iMessage participant should not be touched")
	assert.Equal("+15553333333", readDisplayName(t, st, otherID), "non-iMessage display_name")

	// Empty inputs are no-ops.
	updated, err = st.UpdateImessageParticipantDisplayNameByPhone("", "X")
	require.NoError(err, "empty phone err")
	assert.False(updated, "empty phone updated")
	updated, err = st.UpdateImessageParticipantDisplayNameByPhone("+15551111111", "")
	require.NoError(err, "empty name err")
	assert.False(updated, "empty name updated")
}

func TestRetitleImessageChats(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	st := testutil.NewTestStore(t)

	src, err := st.GetOrCreateSource("apple_messages", "local")
	require.NoError(err, "source")

	otherSrc, err := st.GetOrCreateSource("whatsapp", "+15550000000")
	require.NoError(err, "other source")

	// Named iMessage participant whose phone is the current title of a 1:1.
	namedID, err := st.EnsureParticipantByPhone("+15551111111", "Alice Real", "imessage")
	require.NoError(err, "seed alice")

	// Email-backed iMessage participants did not always get an iMessage
	// participant_identifiers row, but the apple_messages conversation is
	// still enough context to safely refresh the raw email title.
	emailID, err := st.EnsureParticipant("alice@example.com", "Alice Email", "example.com")
	require.NoError(err, "seed alice email")

	// iMessage participant whose name is still the phone (poisoned). Must
	// not be used as a title.
	poisonedID, err := st.EnsureParticipantByPhone("+15552222222", "+15552222222", "imessage")
	require.NoError(err, "seed poisoned")

	// Non-iMessage participant whose phone is a conversation title — must
	// not be touched even if a real name exists elsewhere.
	whatsappID, err := st.EnsureParticipantByPhone("+15553333333", "Carol", "whatsapp")
	require.NoError(err, "seed carol")

	// 1:1 with named participant — title is the phone, should be replaced.
	convNamedID, err := st.EnsureConversationWithType(src.ID, "imsg-1", "direct_chat", "+15551111111")
	require.NoError(err, "conv named")
	require.NoError(st.EnsureConversationParticipant(convNamedID, namedID, "member"), "link named")

	// 1:1 with email participant — title is the raw email, should be replaced.
	convEmailID, err := st.EnsureConversationWithType(src.ID, "imsg-email-1", "direct_chat", "alice@example.com")
	require.NoError(err, "conv email")
	require.NoError(st.EnsureConversationParticipant(convEmailID, emailID, "member"), "link email")

	// 1:1 with poisoned participant — title equals phone but participant
	// has no real name yet. Must remain unchanged.
	convPoisonedID, err := st.EnsureConversationWithType(src.ID, "imsg-2", "direct_chat", "+15552222222")
	require.NoError(err, "conv poisoned")
	require.NoError(st.EnsureConversationParticipant(convPoisonedID, poisonedID, "member"), "link poisoned")

	// Non-iMessage 1:1 — title is a phone, but the source isn't apple_messages.
	convOtherID, err := st.EnsureConversationWithType(otherSrc.ID, "wa-1", "direct_chat", "+15553333333")
	require.NoError(err, "conv other")
	require.NoError(st.EnsureConversationParticipant(convOtherID, whatsappID, "member"), "link other")

	// Group chat whose title was generated from raw participant handles
	// before contacts were backfilled. It should be regenerated with names.
	bobID, err := st.EnsureParticipantByPhone("+15554444444", "Bob Real", "imessage")
	require.NoError(err, "seed bob")
	carolID, err := st.EnsureParticipantByPhone("+15555555555", "Carol Real", "imessage")
	require.NoError(err, "seed carol")
	daveID, err := st.EnsureParticipantByPhone("+15556666666", "Dave Real", "imessage")
	require.NoError(err, "seed dave")
	convGroupID, err := st.EnsureConversationWithType(
		src.ID, "imsg-group-1", "group_chat",
		"+15551111111, +15554444444, +15555555555 +1 more",
	)
	require.NoError(err, "conv group")
	for _, pid := range []int64{namedID, bobID, carolID, daveID} {
		require.NoError(st.EnsureConversationParticipant(convGroupID, pid, "member"),
			"link group participant %d", pid)
	}

	// Named group chats must not be overwritten, even when the participant
	// list would allow a generated title.
	convNamedGroupID, err := st.EnsureConversationWithType(
		src.ID, "imsg-group-2", "group_chat", "Road trip",
	)
	require.NoError(err, "conv named group")
	for _, pid := range []int64{namedID, bobID, carolID} {
		require.NoError(st.EnsureConversationParticipant(convNamedGroupID, pid, "member"),
			"link named group participant %d", pid)
	}

	n, err := st.RetitleImessageChats()
	require.NoError(err, "RetitleImessageChats")
	assert.Equal(int64(3), n, "rows updated")

	assert.Equal("Alice Real", readConvTitle(t, st, convNamedID), "named conv title")
	assert.Equal("Alice Email", readConvTitle(t, st, convEmailID), "email conv title")
	assert.Equal("+15552222222", readConvTitle(t, st, convPoisonedID), "poisoned conv title (unchanged)")
	assert.Equal("+15553333333", readConvTitle(t, st, convOtherID), "non-imessage conv title (unchanged)")
	assert.Equal("Alice Real, Bob Real, Carol Real +1 more",
		readConvTitle(t, st, convGroupID), "group conv title (refreshed generated title)")
	assert.Equal("Road trip", readConvTitle(t, st, convNamedGroupID), "named group conv title (unchanged)")

	// Idempotent: running again is a no-op.
	n2, err := st.RetitleImessageChats()
	require.NoError(err, "idempotent rerun err")
	assert.Equal(int64(0), n2, "idempotent rerun rows")
}

func readConvTitle(t *testing.T, st *store.Store, id int64) string {
	t.Helper()
	var title sql.NullString
	requirepkg.NoError(t, st.DB().QueryRow(
		st.Rebind(`SELECT title FROM conversations WHERE id = ?`), id,
	).Scan(&title), "scan title")
	return title.String
}

func readDisplayName(t *testing.T, st *store.Store, pid int64) string {
	t.Helper()
	var name sql.NullString
	requirepkg.NoError(t, st.DB().QueryRow(
		st.Rebind(`SELECT display_name FROM participants WHERE id = ?`), pid,
	).Scan(&name), "scan display_name")
	return name.String
}
