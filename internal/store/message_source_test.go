package store_test

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestGetMessageSourceContextReadsOnlyTheMessageSource(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("mbox", "offline@example.test")
	requirements.NoError(err)
	conversationID, err := st.EnsureConversation(source.ID, "offline-thread", "Archived")
	requirements.NoError(err)
	senderID, err := st.EnsureParticipant("sender@example.test", "Sender", "example.test")
	requirements.NoError(err)
	messageID, err := st.PersistMessage(&store.MessagePersistData{
		Message: &store.Message{
			SourceID: source.ID, SourceMessageID: "offline-1",
			ConversationID: conversationID, SenderID: sql.NullInt64{Int64: senderID, Valid: true},
			MessageType: store.MessageTypeEmail,
			Subject:     sql.NullString{String: "Archived", Valid: true},
		},
		BodyText:   sql.NullString{String: "body", Valid: true},
		RawMIME:    []byte("From: sender@example.test\r\n\r\nbody"),
		Recipients: []store.RecipientSet{{Type: "from", ParticipantIDs: []int64{senderID}, EmailAddresses: []string{"sender@example.test"}}},
	})
	requirements.NoError(err)

	got, err := st.GetMessageSourceContext(t.Context(), messageID)
	requirements.NoError(err)
	assertions.Equal(source.ID, got.ID)
	assertions.Equal("mbox", got.SourceType)

	_, err = st.GetMessageSourceContext(t.Context(), 999999)
	requirements.ErrorIs(err, store.ErrMessageNotFound)
}
