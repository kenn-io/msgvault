package store_test

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestRelayedSenderParticipantTransaction(t *testing.T) {
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("gmail", "reader@example.test")
	require.NoError(t, err)
	conv, err := st.EnsureConversation(t.Context(), source.ID, "thread", "")
	require.NoError(t, err)
	participants := []store.ParticipantPersistData{{IdentifierType: "github", IdentifierValue: "alice", DisplayName: "alice"}}
	persist := func(sourceMessageID string, valid bool) (int64, error) {
		return st.PersistEmailMessageWithParticipantsContext(t.Context(), source.ID, participants, func(ids []int64) *store.MessagePersistData {
			message := &store.Message{SourceID: source.ID, ConversationID: conv, SourceMessageID: sourceMessageID, MessageType: "email", SenderID: sql.NullInt64{Int64: ids[0], Valid: true}}
			if !valid {
				message.SourceID = -1
			}
			return &store.MessagePersistData{Message: message, Recipients: []store.RecipientSet{{Type: "from", ParticipantIDs: ids, EmailAddresses: []string{"notifications@github.com"}}}}
		})
	}
	_, err = persist("bad", false)
	require.Error(t, err)
	var count int
	require.NoError(t, st.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM participant_identifiers WHERE identifier_type = 'github' AND identifier_value = 'alice'`).Scan(&count))
	assert.Zero(t, count)
	first, err := persist("one", true)
	require.NoError(t, err)
	second, err := persist("two", true)
	require.NoError(t, err)
	var sender1, sender2 int64
	require.NoError(t, st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT sender_id FROM messages WHERE id = ?`), first).Scan(&sender1))
	require.NoError(t, st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT sender_id FROM messages WHERE id = ?`), second).Scan(&sender2))
	assert.Equal(t, sender1, sender2)
}
