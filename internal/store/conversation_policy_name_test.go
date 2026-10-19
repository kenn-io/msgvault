package store_test

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/conversationpolicy"
	"go.kenn.io/msgvault/internal/personenrichment"
	"go.kenn.io/msgvault/internal/store"
)

func TestParticipantNameRepairRemainsEligibleAfterColdChatSighting(t *testing.T) {
	s, source, _, cold, _ := policyObservationFixture(t)
	const phone = "+15550123456"
	participantID, err := s.EnsureParticipantByPhone(phone, "", "sms")
	require.NoError(t, err)
	updated, err := s.UpdateParticipantDisplayNameByPhone(phone, "Native VCard Name")
	require.NoError(t, err)
	require.True(t, updated)
	require.NoError(t, s.SetConversationPolicyOverride(t.Context(), source, "cold", conversationpolicy.Cold))
	messageID, err := s.UpsertMessage(&store.Message{
		SourceID: source, ConversationID: cold, SourceMessageID: "vcard-name-cold-chat",
		MessageType: "chat", SenderID: sql.NullInt64{Int64: participantID, Valid: true},
	})
	require.NoError(t, err)
	_, err = s.DB().Exec(s.Rebind(`INSERT INTO message_recipients(message_id,participant_id,recipient_type,display_name)
		VALUES(?,?,'from',?)`), messageID, participantID, "Different Chat Name")
	require.NoError(t, err)

	eligibleNameCount := func(value string) int {
		var count int
		err := s.DB().QueryRow(s.Rebind(`SELECT COUNT(*) FROM participants p WHERE p.id=? AND `+
			store.FullParticipantNameWhere("p.id", "?")), participantID, value, value).Scan(&count)
		require.NoError(t, err)
		return count
	}
	assert.Equal(t, 1, eligibleNameCount("Native VCard Name"))
	assert.Zero(t, eligibleNameCount("Different Chat Name"))
}

func TestConversationPolicyRawNameRequiresExactFullValue(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)

	s, source, full, cold, pid := policyObservationFixture(t)
	peer, err := s.EnsureParticipant("peer@example.test", "Independent Peer", "example.test")
	requirements.NoError(err)
	requirements.NoError(s.SetConversationPolicyOverride(t.Context(), source, "cold", conversationpolicy.Cold))
	coldMessage, err := s.UpsertMessage(&store.Message{SourceID: source, ConversationID: cold, SourceMessageID: "cold-name", MessageType: "chat", SenderID: sql.NullInt64{Int64: pid, Valid: true}})
	requirements.NoError(err)
	_, err = s.DB().Exec(s.Rebind(`INSERT INTO message_recipients(message_id,participant_id,recipient_type,display_name) VALUES(?,?,'from',?)`), coldMessage, pid, "Synthetic User")
	requirements.NoError(err)
	// An unrelated Full sighting has no evidence for the earlier Cold name.
	fullMessage, err := s.UpsertMessage(&store.Message{SourceID: source, ConversationID: full, SourceMessageID: "full-without-name", MessageType: "chat", SenderID: sql.NullInt64{Int64: pid, Valid: true}})
	requirements.NoError(err)
	requirements.NoError(s.ReplaceConversationParticipantsContext(t.Context(), full, []store.ConversationParticipantRef{{ParticipantID: pid, Role: "member"}}))
	blank := ""
	person, _, err := s.CreatePersonFromParticipantWithDisplayNameContext(t.Context(), pid, &blank)
	requirements.NoError(err)
	check := func(want string) {
		packet, err := s.PersonMatchPairSummariesContext(t.Context(), pid, peer)
		require.NoError(t, err)
		assert.Equal(t, want, packet.Left.DisplayName)
		assert.Equal(t, "Independent Peer", packet.Right.DisplayName)
		input, err := s.LoadRequestInput(t.Context(), personenrichment.WorkLease{PersonID: person.ID})
		require.NoError(t, err)
		if want == "" {
			assert.Empty(t, input.Names)
		} else {
			require.Len(t, input.Names, 1)
			assert.Equal(t, want, input.Names[0].Value)
		}
	}
	check("")
	_, err = s.DB().Exec(s.Rebind(`INSERT INTO message_recipients(message_id,participant_id,recipient_type,display_name) VALUES(?,?,'from',?)`), fullMessage, pid, "Different Full Name")
	requirements.NoError(err)
	check("")
	_, err = s.DB().Exec(s.Rebind(`UPDATE message_recipients SET display_name=? WHERE message_id=?`), "Synthetic User", fullMessage)
	requirements.NoError(err)
	check("Synthetic User")
	requirements.NoError(s.SetConversationPolicyOverride(t.Context(), source, "full", conversationpolicy.Cold))
	check("")
	// Intentional profile naming has independent authority after all rooms cool.
	person, err = s.GetPersonContext(t.Context(), person.ID)
	requirements.NoError(err)
	chosen := "Chosen Profile Name"
	_, err = s.UpdatePersonDisplayNameContext(t.Context(), person.ID, person.Revision, &chosen)
	requirements.NoError(err)
	input, err := s.LoadRequestInput(t.Context(), personenrichment.WorkLease{PersonID: person.ID})
	requirements.NoError(err)
	requirements.Len(input.Names, 1)
	assertions.Equal(chosen, input.Names[0].Value)
	var archived string
	requirements.NoError(s.DB().QueryRow(s.Rebind(`SELECT display_name FROM message_recipients WHERE message_id=?`), coldMessage).Scan(&archived))
	assertions.Equal("Synthetic User", archived)
}
