package store_test

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/search"
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

func TestExactSenderSearchPrefersEnvelope(t *testing.T) {
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("gmail", "reader@example.test")
	require.NoError(t, err)
	conv, err := st.EnsureConversation(t.Context(), source.ID, "thread", "")
	require.NoError(t, err)
	author, err := st.EnsureParticipant("author@example.test", "Author", "example.test")
	require.NoError(t, err)
	for _, tc := range []struct{ id, envelope string }{{"relayed", "relay@example.test"}, {"legacy", ""}} {
		_, err := st.PersistMessageContext(t.Context(), &store.MessagePersistData{
			Message:    &store.Message{SourceID: source.ID, ConversationID: conv, SourceMessageID: tc.id, MessageType: "email", SenderID: sql.NullInt64{Int64: author, Valid: true}},
			Recipients: []store.RecipientSet{{Type: "from", ParticipantIDs: []int64{author}, EmailAddresses: []string{tc.envelope}}},
		})
		require.NoError(t, err)
	}
	for term, want := range map[string]string{"relay@example.test": "relayed", "author@example.test": "legacy"} {
		results, count, err := st.SearchMessagesQueryContext(t.Context(), search.Parse("from:"+term), 0, 10)
		require.NoError(t, err)
		require.Equal(t, int64(1), count, term)
		require.Len(t, results, 1)
		assert.Equal(t, want, results[0].SourceMessageID)
	}
}
