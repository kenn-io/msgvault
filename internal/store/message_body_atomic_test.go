package store_test

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

// A body write that cannot also clear the message's embedding generation
// must not commit: the new text would otherwise keep the old embedding.
func TestUpsertMessageBodyRollsBackWhenEmbeddingResetFails(t *testing.T) {
	require := require.New(t)
	st := testutil.NewTestStore(t)
	if st.IsPostgreSQL() {
		t.Skip("uses a SQLite trigger to fail the embedding reset")
	}
	source, err := st.GetOrCreateSource("slack", "TEXAMPLE:UEXAMPLE")
	require.NoError(err)
	conversation, err := st.EnsureConversation(source.ID, "CEXAMPLE", "Example")
	require.NoError(err)
	id, err := st.UpsertMessage(&store.Message{
		SourceID: source.ID, ConversationID: conversation, SourceMessageID: "CEXAMPLE:1", MessageType: "slack",
	})
	require.NoError(err)
	require.NoError(st.UpsertMessageBody(id, sql.NullString{String: "original text", Valid: true}, sql.NullString{}))
	_, err = st.DB().Exec(`UPDATE messages SET embed_gen = 42 WHERE id = ?`, id)
	require.NoError(err)
	_, err = st.DB().Exec(`CREATE TRIGGER fail_embed_reset BEFORE UPDATE OF embed_gen ON messages
		WHEN NEW.embed_gen IS NULL BEGIN SELECT RAISE(ABORT, 'synthetic embedding reset failure'); END`)
	require.NoError(err)

	err = st.UpsertMessageBodyContext(t.Context(), id, sql.NullString{String: "edited text", Valid: true}, sql.NullString{})
	require.ErrorContains(err, "synthetic embedding reset failure")

	var body string
	var embedGen sql.NullInt64
	require.NoError(st.DB().QueryRow(`SELECT b.body_text, m.embed_gen FROM message_bodies b
		JOIN messages m ON m.id = b.message_id WHERE b.message_id = ?`, id).Scan(&body, &embedGen))
	assert.Equal(t, "original text", body)
	assert.Equal(t, sql.NullInt64{Int64: 42, Valid: true}, embedGen)
}
