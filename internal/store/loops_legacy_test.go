package store_test

import (
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/loops"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestLoopListIncludesLegacyNullMessageTypes(t *testing.T) {
	t.Run("question stays open", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		st, sourceID, conversationID := newLegacyNullableMessageTypeStore(t)
		require.NoError(st.AddAccountIdentity(sourceID, storetest.OwnerAddress, "manual"))

		question := persistLegacyLoopEmail(t, st, sourceID, conversationID, "null-question",
			"alex@example.test", storetest.OwnerAddress, "Can you send the contract?", time.Now().Add(-48*time.Hour))
		_, err := st.DB().ExecContext(t.Context(), st.Rebind(
			`UPDATE messages SET message_type = NULL WHERE id = ?`), question)
		require.NoError(err)

		result, err := loops.List(t.Context(), st, loops.Options{}, time.Now().UTC())
		require.NoError(err)
		require.Len(result.Loops, 1)
		assert.Equal(question, result.Loops[0].Anchor.ID)
		assert.Equal(loops.ReplyOwed, result.Loops[0].Kind)
	})

	t.Run("answer settles question", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		st, sourceID, conversationID := newLegacyNullableMessageTypeStore(t)
		require.NoError(st.AddAccountIdentity(sourceID, storetest.OwnerAddress, "manual"))
		now := time.Now().UTC()
		persistLegacyLoopEmail(t, st, sourceID, conversationID, "outgoing-question",
			storetest.OwnerAddress, "alex@example.test", "Can you send the contract?", now.Add(-5*24*time.Hour))
		answer := persistLegacyLoopEmail(t, st, sourceID, conversationID, "null-answer",
			"alex@example.test", storetest.OwnerAddress, "Here is the contract.", now.Add(-3*24*time.Hour))
		_, err := st.DB().ExecContext(t.Context(), st.Rebind(
			`UPDATE messages SET message_type = NULL WHERE id = ?`), answer)
		require.NoError(err)

		result, err := loops.List(t.Context(), st, loops.Options{}, now)
		require.NoError(err)
		assert.Empty(result.Loops, "a legacy NULL-type reply settles the open question")
	})
}

func persistLegacyLoopEmail(
	t *testing.T,
	st *store.Store,
	sourceID, conversationID int64,
	sourceMessageID, from, to, body string,
	sentAt time.Time,
) int64 {
	t.Helper()
	require := require.New(t)
	senderID, err := st.EnsureParticipant(from, "", "example.test")
	require.NoError(err)
	recipientID, err := st.EnsureParticipant(to, "", "example.test")
	require.NoError(err)
	messageID, err := st.PersistMessageContext(t.Context(), &store.MessagePersistData{
		Message: &store.Message{
			SourceID: sourceID, ConversationID: conversationID, SourceMessageID: sourceMessageID,
			MessageType: store.MessageTypeEmail, SenderID: sql.NullInt64{Int64: senderID, Valid: true},
			SentAt: sql.NullTime{Time: sentAt, Valid: true}, IsFromMe: from == storetest.OwnerAddress,
		},
		BodyText: sql.NullString{String: body, Valid: true},
		Recipients: []store.RecipientSet{
			{Type: "from", ParticipantIDs: []int64{senderID}, EmailAddresses: []string{from}},
			{Type: "to", ParticipantIDs: []int64{recipientID}, EmailAddresses: []string{to}},
		},
	})
	require.NoError(err)
	return messageID
}
