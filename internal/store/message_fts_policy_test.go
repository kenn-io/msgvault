package store_test

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestMessageFTSFailureIsMandatoryByDefault(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("gmail", "owner@example.com")
	require.NoError(err)
	if st.IsPostgreSQL() {
		_, err = st.DB().Exec(`ALTER TABLE messages ADD CONSTRAINT synthetic_message_fts_failure CHECK (search_fts IS NULL)`)
	} else {
		_, err = st.DB().Exec(`DROP TABLE messages_fts`)
	}
	require.NoError(err)
	_, err = st.PersistMessageContext(t.Context(), &store.MessagePersistData{
		Message:      &store.Message{SourceID: source.ID, SourceMessageID: "mandatory-index", MessageType: "email"},
		Conversation: &store.ConversationPersistData{SourceConversationID: "thread", ConversationType: "email_thread"},
		BodyText:     sql.NullString{String: "Synthetic body", Valid: true},
		FTS:          &store.FTSDoc{Body: "Synthetic body"},
	})
	require.ErrorContains(err, "upsert fts")
	var count int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&count))
	assert.Zero(count, "existing writers must retain mandatory index rollback")
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM conversations`).Scan(&count))
	assert.Zero(count, "canonical conversation writes roll back with the message")
}
