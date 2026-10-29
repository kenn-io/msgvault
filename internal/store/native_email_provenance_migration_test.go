package store

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/conversationpolicy"
)

func TestInitSchemaBackfillsNativeEmailObservationsFromPayloads(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	s, err := Open(filepath.Join(t.TempDir(), "archive.db"))
	requirements.NoError(err)
	t.Cleanup(func() { requirements.NoError(s.Close()) })
	requirements.NoError(s.InitSchemaContext(t.Context()))

	apple, err := s.GetOrCreateSource("apple_messages", "synthetic-native-email-backfill")
	requirements.NoError(err)
	imazing, err := s.GetOrCreateSource("imazing_csv", "synthetic-imazing-email-backfill")
	requirements.NoError(err)
	appleRoom, err := s.EnsureConversation(apple.ID, "native-room", "Native room")
	requirements.NoError(err)
	imazingRoom, err := s.EnsureConversation(imazing.ID, "imazing-room", "iMazing room")
	requirements.NoError(err)
	appleParticipant, err := s.EnsureParticipant("apple-peer@example.test", "Apple Peer", "example.test")
	requirements.NoError(err)
	imazingParticipant, err := s.EnsureParticipant("imazing-peer@example.test", "iMazing Peer", "example.test")
	requirements.NoError(err)

	appleMessage, err := s.UpsertMessage(&Message{
		SourceID: apple.ID, ConversationID: appleRoom, SourceMessageID: "apple-message",
		MessageType: "imessage", SenderID: sql.NullInt64{Int64: appleParticipant, Valid: true},
	})
	requirements.NoError(err)
	requirements.NoError(s.UpsertMessageRawWithFormat(appleMessage,
		[]byte(`{"handle_id":"apple-peer@example.test","is_from_me":0}`), "imessage_json"))
	imazingMessage, err := s.UpsertMessage(&Message{
		SourceID: imazing.ID, ConversationID: imazingRoom, SourceMessageID: "imazing-message",
		MessageType: "imessage", SenderID: sql.NullInt64{Int64: imazingParticipant, Valid: true},
	})
	requirements.NoError(err)
	requirements.NoError(s.UpsertMessageRawWithFormat(imazingMessage,
		[]byte(`{"Sender ID":"imazing-peer@example.test","Type":"Incoming"}`), "imazing_csv_json"))

	_, err = s.DB().ExecContext(t.Context(), s.Rebind(
		`DELETE FROM applied_migrations WHERE name = ?`), migrationNativeEmailObservationProvenance)
	requirements.NoError(err)
	requirements.NoError(s.InitSchemaContext(t.Context()))

	for _, want := range []struct {
		participant int64
		source      int64
		value       string
		prefix      string
	}{
		{participant: appleParticipant, source: apple.ID, value: "apple-peer@example.test", prefix: "imessage:handle:"},
		{participant: imazingParticipant, source: imazing.ID, value: "imazing-peer@example.test", prefix: "imazing_csv:participant:"},
	} {
		observations, err := s.ListParticipantObservationsContext(t.Context(), want.participant, true)
		requirements.NoError(err)
		requirements.Len(observations, 1)
		assertions.Equal(ContactAddressEmail, observations[0].AddressKind)
		assertions.Equal(want.value, observations[0].NormalizedValue)
		requirements.NotNil(observations[0].SourceID)
		assertions.Equal(want.source, *observations[0].SourceID)
		requirements.NotNil(observations[0].Envelope.SourceRef)
		assertions.Contains(*observations[0].Envelope.SourceRef, want.prefix)
	}

	cold, err := s.GetOrCreateSource("beeper", "synthetic-native-email-cold-room")
	requirements.NoError(err)
	coldRoom, err := s.EnsureConversation(cold.ID, "cold-room", "Cold room")
	requirements.NoError(err)
	requirements.NoError(s.SetConversationPolicyOverride(t.Context(), cold.ID, "cold-room", conversationpolicy.Cold))
	for _, participant := range []int64{appleParticipant, imazingParticipant} {
		requirements.NoError(s.EnsureConversationParticipantContext(t.Context(), coldRoom, participant, "member"))
	}
	for _, participant := range []int64{appleParticipant, imazingParticipant} {
		var eligible bool
		requirements.NoError(s.DB().QueryRowContext(t.Context(), s.Rebind(`SELECT `+
			FullParticipantValueWhere("p.id", "'email'", "p.email_address")+
			` FROM participants p WHERE p.id=?`), participant).Scan(&eligible))
		assertions.True(eligible,
			"archived native email evidence must remain eligible after a Cold chat is linked")
	}

	requirements.NoError(s.InitSchemaContext(t.Context()))
	for _, participant := range []int64{appleParticipant, imazingParticipant} {
		observations, err := s.ListParticipantObservationsContext(t.Context(), participant, true)
		requirements.NoError(err)
		assertions.Len(observations, 1, "a completed migration must not duplicate archived email evidence")
	}
}

func TestInitSchemaReplacesLegacyParticipantObservationIdentityIndex(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	s, err := Open(filepath.Join(t.TempDir(), "archive.db"))
	requirements.NoError(err)
	t.Cleanup(func() { requirements.NoError(s.Close()) })
	requirements.NoError(s.InitSchemaContext(t.Context()))
	_, err = s.DB().ExecContext(t.Context(), `DROP INDEX idx_participant_observations_identity`)
	requirements.NoError(err)
	_, err = s.DB().ExecContext(t.Context(), `CREATE UNIQUE INDEX idx_participant_observations_identity
		ON participant_contact_observations(
			participant_id, source_id, address_kind, service_id, scope_kind, scope_value, normalized_value
		) WHERE active_until IS NULL AND superseded_at IS NULL`)
	requirements.NoError(err)
	_, err = s.DB().ExecContext(t.Context(), s.Rebind(
		`DELETE FROM applied_migrations WHERE name = ?`), migrationParticipantObservationIdentity)
	requirements.NoError(err)
	requirements.NoError(s.InitSchemaContext(t.Context()))

	var definition string
	requirements.NoError(s.DB().QueryRowContext(t.Context(), s.Rebind(
		`SELECT sql FROM sqlite_master WHERE type='index' AND name=?`),
		"idx_participant_observations_identity").Scan(&definition))
	assertions.Contains(definition, "provider_user_id")
	assertions.Contains(definition, "source_ref")
}
