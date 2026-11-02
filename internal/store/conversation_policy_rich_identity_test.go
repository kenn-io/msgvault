package store_test

import (
	"database/sql"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/conversationpolicy"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"testing"
)

func TestConversationParticipantOriginTransaction(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)

	s := testutil.NewTestStore(t)
	s.DB().SetMaxOpenConns(1)
	src, err := s.GetOrCreateSource("beeper", "origin@example.test")
	requirements.NoError(err)
	room, err := s.EnsureConversation(src.ID, "origin", "Synthetic")
	requirements.NoError(err)
	requirements.NoError(s.SetConversationPolicyOverride(t.Context(), src.ID, "origin", conversationpolicy.Full))
	supportRoom, err := s.EnsureConversation(src.ID, "support-room", "Synthetic support room")
	requirements.NoError(err)
	requirements.NoError(s.SetConversationPolicyOverride(t.Context(), src.ID, "support-room", conversationpolicy.Full))
	weak, err := s.EnsureParticipantByIdentifier("beeper", "origin-user", "")
	requirements.NoError(err)
	mid, err := s.UpsertMessage(&store.Message{SourceID: src.ID, ConversationID: room, SourceMessageID: "origin-message", MessageType: "beeper", SenderID: sql.NullInt64{Int64: weak, Valid: true}})
	requirements.NoError(err)
	input := store.ConversationParticipantIdentity{IdentifierType: "beeper", IdentifierValue: "origin-user", Email: "origin-user@example.test", DisplayName: "Synthetic profile"}
	got, err := s.ResolveConversationParticipantContext(t.Context(), room, input)
	requirements.NoError(err)
	assertions.NotEqual(weak, got.ParticipantID)
	var sender int64
	requirements.NoError(s.DB().QueryRow(s.Rebind(`SELECT sender_id FROM messages WHERE id=?`), mid).Scan(&sender))
	assertions.Equal(got.ParticipantID, sender)
	supported, err := s.ResolveConversationParticipantContext(t.Context(), supportRoom, input)
	requirements.NoError(err)
	assertions.Equal(got, supported)
	revision, err := s.ConversationPolicyRevision(t.Context())
	requirements.NoError(err)
	_, err = s.DB().Exec(s.Rebind(`DELETE FROM participant_contact_observation_conversations
		WHERE conversation_id=? AND observation_id IN (
			SELECT id FROM participant_contact_observations WHERE participant_id=? AND normalized_value=?)`), supportRoom, got.ParticipantID, input.Email)
	requirements.NoError(err)
	var missingSupport int
	requirements.NoError(s.DB().QueryRow(s.Rebind(`SELECT COUNT(*) FROM participant_contact_observation_conversations
		WHERE conversation_id=? AND observation_id IN (
			SELECT id FROM participant_contact_observations WHERE participant_id=? AND normalized_value=?)`), supportRoom, got.ParticipantID, input.Email).Scan(&missingSupport))
	assertions.Zero(missingSupport)
	logs := captureAttachmentQueryLogs(t)
	again, err := s.ResolveConversationParticipantContext(t.Context(), supportRoom, input)
	requirements.NoError(err)
	assertions.Equal(got, again)
	assertions.NotContains(logs.String(), "SELECT c.id FROM conversations c JOIN sources src",
		"a stable sighting should not scan historical room provenance")
	var roomSupport int
	requirements.NoError(s.DB().QueryRow(s.Rebind(`SELECT COUNT(*) FROM participant_contact_observation_conversations
		WHERE conversation_id=? AND observation_id IN (
			SELECT id FROM participant_contact_observations WHERE participant_id=? AND normalized_value=?)`), supportRoom, got.ParticipantID, input.Email).Scan(&roomSupport))
	assertions.Equal(1, roomSupport, "a repeat sighting restores missing exact-room email support")
	after, err := s.ConversationPolicyRevision(t.Context())
	requirements.NoError(err)
	assertions.Equal(revision, after, "unchanged sighting must not invalidate work")
	var dirty int
	requirements.NoError(s.DB().QueryRow(`SELECT COUNT(*) FROM archive_metadata WHERE key LIKE 'conversation_policy_activity_dirty:%'`).Scan(&dirty))
	assertions.Zero(dirty)
	captured, err := s.CaptureConversationPolicyWork(t.Context())
	requirements.NoError(err)
	requirements.NoError(s.SetConversationPolicyOverride(t.Context(), src.ID, "origin", conversationpolicy.Cold))
	input.Phone = "+15550100994"
	stale, err := s.ResolveConversationParticipantContext(t.Context(), room, input)
	requirements.ErrorIs(err, store.ErrConversationPolicyStale)
	assertions.Zero(stale.ParticipantID)
	requirements.NoError(s.SetConversationPolicyOverride(t.Context(), src.ID, "origin", conversationpolicy.Full))
	_, err = s.ResolveConversationParticipantContext(captured, room, input)
	requirements.ErrorIs(err, store.ErrConversationPolicyStale, "promotion must not refresh original authority")
	upgraded, err := s.ResolveConversationParticipantContext(t.Context(), room, input)
	requirements.NoError(err)
	assertions.True(upgraded.ByPhone)
	assertions.NotEqual(got.ParticipantID, upgraded.ParticipantID)
	requirements.NoError(s.DB().QueryRow(s.Rebind(`SELECT sender_id FROM messages WHERE id=?`), mid).Scan(&sender))
	assertions.Equal(upgraded.ParticipantID, sender)
}

func TestConversationParticipantRollsBackRichMutation(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)

	s := testutil.NewTestStore(t)
	s.DB().SetMaxOpenConns(1)
	src, err := s.GetOrCreateSource("slack", "rollback-origin")
	requirements.NoError(err)
	room, err := s.EnsureConversation(src.ID, "origin", "Synthetic")
	requirements.NoError(err)
	requirements.NoError(s.SetConversationPolicyOverride(t.Context(), src.ID, "origin", conversationpolicy.Full))
	weak, err := s.EnsureParticipantByIdentifier("slack", "rollback-user", "")
	requirements.NoError(err)
	requirements.NoError(s.EnsureConversationParticipant(room, weak, "member"))
	if s.IsPostgreSQL() {
		_, err = s.DB().Exec(`CREATE FUNCTION fail_rich_cleanup() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.key='conversation_policy_cleanup_revision' THEN RAISE EXCEPTION 'synthetic rich cleanup failure'; END IF; RETURN NEW; END $$`)
		requirements.NoError(err)
		_, err = s.DB().Exec(`CREATE TRIGGER fail_rich_cleanup BEFORE INSERT OR UPDATE ON archive_metadata FOR EACH ROW EXECUTE FUNCTION fail_rich_cleanup()`)
	} else {
		_, err = s.DB().Exec(`CREATE TRIGGER fail_rich_cleanup BEFORE INSERT ON archive_metadata WHEN NEW.key='conversation_policy_cleanup_revision' BEGIN SELECT RAISE(ABORT,'synthetic rich cleanup failure'); END`)
	}
	requirements.NoError(err)
	got, err := s.ResolveConversationParticipantContext(t.Context(), room, store.ConversationParticipantIdentity{IdentifierType: "slack", IdentifierValue: "rollback-user", Email: "rollback-rich@example.test", DisplayName: "Rollback name"})
	requirements.ErrorContains(err, "synthetic rich cleanup failure")
	assertions.Zero(got.ParticipantID)
	pid, _, err := s.ParticipantByIdentifier("slack", "rollback-user")
	requirements.NoError(err)
	assertions.Equal(weak, pid)
	var count int
	requirements.NoError(s.DB().QueryRow(`SELECT COUNT(*) FROM participants WHERE email_address='rollback-rich@example.test' OR display_name='Rollback name'`).Scan(&count))
	assertions.Zero(count)
}
