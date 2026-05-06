package store_test

import (
	"database/sql"
	"testing"
	"time"

	"github.com/wesm/msgvault/internal/store"
	"github.com/wesm/msgvault/internal/testutil"
)

func TestRecomputeConversationStats(t *testing.T) {
	st := testutil.NewTestStore(t)

	source, err := st.GetOrCreateSource("whatsapp", "+15550000001")
	if err != nil {
		t.Fatalf("GetOrCreateSource: %v", err)
	}

	convID, err := st.EnsureConversationWithType(source.ID, "conv-1", "whatsapp_dm", "Test Chat")
	if err != nil {
		t.Fatalf("EnsureConversationWithType: %v", err)
	}

	// Verify initial message_count is 0 (stats not maintained on insert).
	var initialCount int
	if err := st.DB().QueryRow(
		`SELECT message_count FROM conversations WHERE id = ?`, convID,
	).Scan(&initialCount); err != nil {
		t.Fatalf("initial message_count scan: %v", err)
	}
	if initialCount != 0 {
		t.Errorf("initial message_count = %d, want 0", initialCount)
	}

	sentAt := time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC)
	msg1 := &store.Message{
		SourceID:        source.ID,
		SourceMessageID: "msg-1",
		ConversationID:  convID,
		MessageType:     "whatsapp",
		SentAt:          sql.NullTime{Time: sentAt, Valid: true},
		Snippet:         sql.NullString{String: "hello", Valid: true},
	}
	if _, err := st.UpsertMessage(msg1); err != nil {
		t.Fatalf("UpsertMessage msg1: %v", err)
	}

	sentAt2 := sentAt.Add(time.Hour)
	msg2 := &store.Message{
		SourceID:        source.ID,
		SourceMessageID: "msg-2",
		ConversationID:  convID,
		MessageType:     "whatsapp",
		SentAt:          sql.NullTime{Time: sentAt2, Valid: true},
		Snippet:         sql.NullString{String: "world", Valid: true},
	}
	if _, err := st.UpsertMessage(msg2); err != nil {
		t.Fatalf("UpsertMessage msg2: %v", err)
	}

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
	if _, err := st.UpsertMessage(msg3); err != nil {
		t.Fatalf("UpsertMessage msg3: %v", err)
	}

	// Add a conversation participant so participant_count is non-zero.
	participantID, err := st.EnsureParticipantByPhone("+15559876543", "Bob", "whatsapp")
	if err != nil {
		t.Fatalf("EnsureParticipantByPhone: %v", err)
	}
	if err := st.EnsureConversationParticipant(convID, participantID, "member"); err != nil {
		t.Fatalf("EnsureConversationParticipant: %v", err)
	}

	// Recompute and verify counts.
	if err := st.RecomputeConversationStats(source.ID); err != nil {
		t.Fatalf("RecomputeConversationStats: %v", err)
	}

	var count int
	var participantCount int
	var lastMsgAt sql.NullTime
	var preview sql.NullString
	if err := st.DB().QueryRow(
		`SELECT message_count, participant_count, last_message_at, last_message_preview
		 FROM conversations WHERE id = ?`, convID,
	).Scan(&count, &participantCount, &lastMsgAt, &preview); err != nil {
		t.Fatalf("post-recompute scan: %v", err)
	}
	if count != 3 {
		t.Errorf("message_count = %d, want 3", count)
	}
	if participantCount != 1 {
		t.Errorf("participant_count = %d, want 1", participantCount)
	}
	if !lastMsgAt.Valid {
		t.Error("last_message_at is NULL, want a timestamp")
	}
	// msg2 and msg3 share the same sent_at; msg3 has the higher id, so its
	// snippet ("tie-breaker") must win via the `id DESC` tie-breaker.
	if !preview.Valid || preview.String != "tie-breaker" {
		t.Errorf("last_message_preview = %q, want %q", preview.String, "tie-breaker")
	}

	// Idempotency: calling again should produce the same result.
	if err := st.RecomputeConversationStats(source.ID); err != nil {
		t.Fatalf("RecomputeConversationStats (second call): %v", err)
	}
	if err := st.DB().QueryRow(
		`SELECT message_count FROM conversations WHERE id = ?`, convID,
	).Scan(&count); err != nil {
		t.Fatalf("idempotency scan: %v", err)
	}
	if count != 3 {
		t.Errorf("idempotency: message_count = %d, want 3", count)
	}
}

func TestEnsureParticipantByPhone_IdentifierType(t *testing.T) {
	st := testutil.NewTestStore(t)

	// Create participant via WhatsApp
	id1, err := st.EnsureParticipantByPhone("+15551234567", "Alice", "whatsapp")
	if err != nil {
		t.Fatalf("EnsureParticipantByPhone(whatsapp): %v", err)
	}
	if id1 == 0 {
		t.Fatal("expected non-zero participant ID")
	}

	// Same phone via iMessage — should return the same participant ID
	id2, err := st.EnsureParticipantByPhone("+15551234567", "Alice", "imessage")
	if err != nil {
		t.Fatalf("EnsureParticipantByPhone(imessage): %v", err)
	}
	if id2 != id1 {
		t.Errorf("imessage call returned participant ID %d, want %d (same as whatsapp)", id2, id1)
	}

	// Both participant_identifiers rows should exist
	var count int
	err = st.DB().QueryRow(
		`SELECT COUNT(*) FROM participant_identifiers WHERE participant_id = ?`,
		id1,
	).Scan(&count)
	if err != nil {
		t.Fatalf("count participant_identifiers: %v", err)
	}
	if count != 2 {
		t.Errorf("participant_identifiers count = %d, want 2", count)
	}

	// Verify each identifier type is present
	for _, identType := range []string{"whatsapp", "imessage"} {
		var exists int
		err = st.DB().QueryRow(
			`SELECT COUNT(*) FROM participant_identifiers
			 WHERE participant_id = ? AND identifier_type = ?`,
			id1, identType,
		).Scan(&exists)
		if err != nil {
			t.Fatalf("check identifier_type %q: %v", identType, err)
		}
		if exists != 1 {
			t.Errorf("identifier_type %q count = %d, want 1", identType, exists)
		}
	}
}

func TestUpdateParticipantDisplayNameByEmail(t *testing.T) {
	st := testutil.NewTestStore(t)

	// Create an unnamed email participant (e.g. inserted by iMessage import
	// for an Apple ID handle).
	res, err := st.DB().Exec(
		`INSERT INTO participants (email_address) VALUES (?)`,
		"alice@example.com",
	)
	if err != nil {
		t.Fatalf("insert participant: %v", err)
	}
	pid, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("LastInsertId: %v", err)
	}

	// Backfilling on an empty display_name succeeds.
	updated, err := st.UpdateParticipantDisplayNameByEmail("alice@example.com", "Alice Example")
	if err != nil {
		t.Fatalf("UpdateParticipantDisplayNameByEmail: %v", err)
	}
	if !updated {
		t.Fatal("expected backfill to update existing participant")
	}

	got := readDisplayName(t, st, pid)
	if got != "Alice Example" {
		t.Errorf("display_name = %q, want %q", got, "Alice Example")
	}

	// Lookup is case-insensitive on the email.
	updatedMixed, err := st.UpdateParticipantDisplayNameByEmail("ALICE@example.com", "Should Not Overwrite")
	if err != nil {
		t.Fatalf("UpdateParticipantDisplayNameByEmail (case): %v", err)
	}
	if updatedMixed {
		t.Error("second update should not modify a non-empty display_name")
	}
	if got := readDisplayName(t, st, pid); got != "Alice Example" {
		t.Errorf("display_name overwritten: %q", got)
	}

	// Empty inputs are no-ops.
	if updated, err := st.UpdateParticipantDisplayNameByEmail("", "X"); err != nil || updated {
		t.Errorf("empty email: updated=%v err=%v", updated, err)
	}
	if updated, err := st.UpdateParticipantDisplayNameByEmail("x@y.com", ""); err != nil || updated {
		t.Errorf("empty name: updated=%v err=%v", updated, err)
	}

	// Unknown email is a no-op (does not create rows).
	if updated, err := st.UpdateParticipantDisplayNameByEmail("nobody@example.com", "Nobody"); err != nil || updated {
		t.Errorf("unknown email: updated=%v err=%v", updated, err)
	}
}

func TestUpdateImessageParticipantDisplayNameByPhone(t *testing.T) {
	st := testutil.NewTestStore(t)

	// Case 1: legacy iMessage participant with display_name = phone_number.
	// Should be overwritten by the contact name.
	legacyID, err := st.EnsureParticipantByPhone("+15551111111", "+15551111111", "imessage")
	if err != nil {
		t.Fatalf("seed legacy: %v", err)
	}

	// Case 2: iMessage participant already named by another source. Real
	// name must be preserved.
	namedID, err := st.EnsureParticipantByPhone("+15552222222", "Bob From Gmail", "imessage")
	if err != nil {
		t.Fatalf("seed named: %v", err)
	}

	// Case 3: WhatsApp-only participant with display_name = phone_number.
	// Not iMessage, must NOT be touched (no imessage identifier exists).
	otherID, err := st.EnsureParticipantByPhone("+15553333333", "+15553333333", "whatsapp")
	if err != nil {
		t.Fatalf("seed other: %v", err)
	}

	// Apply contact-name backfill.
	updated, err := st.UpdateImessageParticipantDisplayNameByPhone("+15551111111", "Alice Real")
	if err != nil {
		t.Fatalf("backfill legacy: %v", err)
	}
	if !updated {
		t.Error("legacy placeholder should be replaced")
	}
	if got := readDisplayName(t, st, legacyID); got != "Alice Real" {
		t.Errorf("legacy display_name = %q, want %q", got, "Alice Real")
	}

	updated, err = st.UpdateImessageParticipantDisplayNameByPhone("+15552222222", "Should Not Win")
	if err != nil {
		t.Fatalf("backfill named: %v", err)
	}
	if updated {
		t.Error("real name from another source should be preserved")
	}
	if got := readDisplayName(t, st, namedID); got != "Bob From Gmail" {
		t.Errorf("named display_name = %q, want %q", got, "Bob From Gmail")
	}

	updated, err = st.UpdateImessageParticipantDisplayNameByPhone("+15553333333", "Not Allowed")
	if err != nil {
		t.Fatalf("backfill other: %v", err)
	}
	if updated {
		t.Error("non-iMessage participant should not be touched")
	}
	if got := readDisplayName(t, st, otherID); got != "+15553333333" {
		t.Errorf("non-iMessage display_name = %q, want %q", got, "+15553333333")
	}

	// Empty inputs are no-ops.
	if updated, err := st.UpdateImessageParticipantDisplayNameByPhone("", "X"); err != nil || updated {
		t.Errorf("empty phone: updated=%v err=%v", updated, err)
	}
	if updated, err := st.UpdateImessageParticipantDisplayNameByPhone("+15551111111", ""); err != nil || updated {
		t.Errorf("empty name: updated=%v err=%v", updated, err)
	}
}

func TestRetitleImessageChats(t *testing.T) {
	st := testutil.NewTestStore(t)

	src, err := st.GetOrCreateSource("apple_messages", "local")
	if err != nil {
		t.Fatalf("source: %v", err)
	}

	otherSrc, err := st.GetOrCreateSource("whatsapp", "+15550000000")
	if err != nil {
		t.Fatalf("other source: %v", err)
	}

	// Named iMessage participant whose phone is the current title of a 1:1.
	namedID, err := st.EnsureParticipantByPhone("+15551111111", "Alice Real", "imessage")
	if err != nil {
		t.Fatalf("seed alice: %v", err)
	}

	// Email-backed iMessage participants did not always get an iMessage
	// participant_identifiers row, but the apple_messages conversation is
	// still enough context to safely refresh the raw email title.
	emailID, err := st.EnsureParticipant("alice@example.com", "Alice Email", "example.com")
	if err != nil {
		t.Fatalf("seed alice email: %v", err)
	}

	// iMessage participant whose name is still the phone (poisoned). Must
	// not be used as a title.
	poisonedID, err := st.EnsureParticipantByPhone("+15552222222", "+15552222222", "imessage")
	if err != nil {
		t.Fatalf("seed poisoned: %v", err)
	}

	// Non-iMessage participant whose phone is a conversation title — must
	// not be touched even if a real name exists elsewhere.
	whatsappID, err := st.EnsureParticipantByPhone("+15553333333", "Carol", "whatsapp")
	if err != nil {
		t.Fatalf("seed carol: %v", err)
	}

	// 1:1 with named participant — title is the phone, should be replaced.
	convNamedID, err := st.EnsureConversationWithType(src.ID, "imsg-1", "direct_chat", "+15551111111")
	if err != nil {
		t.Fatalf("conv named: %v", err)
	}
	if err := st.EnsureConversationParticipant(convNamedID, namedID, "member"); err != nil {
		t.Fatalf("link named: %v", err)
	}

	// 1:1 with email participant — title is the raw email, should be replaced.
	convEmailID, err := st.EnsureConversationWithType(src.ID, "imsg-email-1", "direct_chat", "alice@example.com")
	if err != nil {
		t.Fatalf("conv email: %v", err)
	}
	if err := st.EnsureConversationParticipant(convEmailID, emailID, "member"); err != nil {
		t.Fatalf("link email: %v", err)
	}

	// 1:1 with poisoned participant — title equals phone but participant
	// has no real name yet. Must remain unchanged.
	convPoisonedID, err := st.EnsureConversationWithType(src.ID, "imsg-2", "direct_chat", "+15552222222")
	if err != nil {
		t.Fatalf("conv poisoned: %v", err)
	}
	if err := st.EnsureConversationParticipant(convPoisonedID, poisonedID, "member"); err != nil {
		t.Fatalf("link poisoned: %v", err)
	}

	// Non-iMessage 1:1 — title is a phone, but the source isn't apple_messages.
	convOtherID, err := st.EnsureConversationWithType(otherSrc.ID, "wa-1", "direct_chat", "+15553333333")
	if err != nil {
		t.Fatalf("conv other: %v", err)
	}
	if err := st.EnsureConversationParticipant(convOtherID, whatsappID, "member"); err != nil {
		t.Fatalf("link other: %v", err)
	}

	// Group chat whose title was generated from raw participant handles
	// before contacts were backfilled. It should be regenerated with names.
	bobID, err := st.EnsureParticipantByPhone("+15554444444", "Bob Real", "imessage")
	if err != nil {
		t.Fatalf("seed bob: %v", err)
	}
	carolID, err := st.EnsureParticipantByPhone("+15555555555", "Carol Real", "imessage")
	if err != nil {
		t.Fatalf("seed carol: %v", err)
	}
	daveID, err := st.EnsureParticipantByPhone("+15556666666", "Dave Real", "imessage")
	if err != nil {
		t.Fatalf("seed dave: %v", err)
	}
	convGroupID, err := st.EnsureConversationWithType(
		src.ID, "imsg-group-1", "group_chat",
		"+15551111111, +15554444444, +15555555555 +1 more",
	)
	if err != nil {
		t.Fatalf("conv group: %v", err)
	}
	for _, pid := range []int64{namedID, bobID, carolID, daveID} {
		if err := st.EnsureConversationParticipant(convGroupID, pid, "member"); err != nil {
			t.Fatalf("link group participant %d: %v", pid, err)
		}
	}

	// Named group chats must not be overwritten, even when the participant
	// list would allow a generated title.
	convNamedGroupID, err := st.EnsureConversationWithType(
		src.ID, "imsg-group-2", "group_chat", "Road trip",
	)
	if err != nil {
		t.Fatalf("conv named group: %v", err)
	}
	for _, pid := range []int64{namedID, bobID, carolID} {
		if err := st.EnsureConversationParticipant(convNamedGroupID, pid, "member"); err != nil {
			t.Fatalf("link named group participant %d: %v", pid, err)
		}
	}

	n, err := st.RetitleImessageChats()
	if err != nil {
		t.Fatalf("RetitleImessageChats: %v", err)
	}
	if n != 3 {
		t.Errorf("rows updated = %d, want 3", n)
	}

	if got := readConvTitle(t, st, convNamedID); got != "Alice Real" {
		t.Errorf("named conv title = %q, want %q", got, "Alice Real")
	}
	if got := readConvTitle(t, st, convEmailID); got != "Alice Email" {
		t.Errorf("email conv title = %q, want %q", got, "Alice Email")
	}
	if got := readConvTitle(t, st, convPoisonedID); got != "+15552222222" {
		t.Errorf("poisoned conv title = %q, want unchanged", got)
	}
	if got := readConvTitle(t, st, convOtherID); got != "+15553333333" {
		t.Errorf("non-imessage conv title = %q, want unchanged", got)
	}
	if got := readConvTitle(t, st, convGroupID); got != "Alice Real, Bob Real, Carol Real +1 more" {
		t.Errorf("group conv title = %q, want refreshed generated title", got)
	}
	if got := readConvTitle(t, st, convNamedGroupID); got != "Road trip" {
		t.Errorf("named group conv title = %q, want unchanged", got)
	}

	// Idempotent: running again is a no-op.
	if n2, err := st.RetitleImessageChats(); err != nil || n2 != 0 {
		t.Errorf("idempotent rerun: rows=%d err=%v", n2, err)
	}
}

func readConvTitle(t *testing.T, st *store.Store, id int64) string {
	t.Helper()
	var title sql.NullString
	if err := st.DB().QueryRow(
		`SELECT title FROM conversations WHERE id = ?`, id,
	).Scan(&title); err != nil {
		t.Fatalf("scan title: %v", err)
	}
	return title.String
}

func readDisplayName(t *testing.T, st *store.Store, pid int64) string {
	t.Helper()
	var name sql.NullString
	if err := st.DB().QueryRow(
		`SELECT display_name FROM participants WHERE id = ?`, pid,
	).Scan(&name); err != nil {
		t.Fatalf("scan display_name: %v", err)
	}
	return name.String
}
