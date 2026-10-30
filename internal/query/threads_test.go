package query_test

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/identityindex"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestThreadsSearchMessageType(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	assert.True(query.IsKnownMessageType("threads"))
	assert.True(identityindex.IsChat("threads", "channel"))
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("threads", "threads:10:20")
	require.NoError(err)
	conv, err := st.EnsureConversationWithType(source.ID, "100", "channel", "Root")
	require.NoError(err)
	id, err := st.PersistMessageContext(t.Context(), &store.MessagePersistData{
		Message:  &store.Message{SourceID: source.ID, ConversationID: conv, SourceMessageID: "100", MessageType: "threads", Snippet: sql.NullString{String: "synthetic discussion", Valid: true}},
		BodyText: sql.NullString{String: "synthetic discussion", Valid: true},
	})
	require.NoError(err)
	otherSource, err := st.GetOrCreateSource("threads", "threads:10:21")
	require.NoError(err)
	otherConv, err := st.EnsureConversationWithType(otherSource.ID, "200", "channel", "Other root")
	require.NoError(err)
	for _, other := range []struct {
		sourceID, conversationID int64
		messageType              string
	}{
		{otherSource.ID, otherConv, "threads"},
		{source.ID, conv, "email"},
	} {
		_, err := st.PersistMessageContext(t.Context(), &store.MessagePersistData{
			Message: &store.Message{SourceID: other.sourceID, ConversationID: other.conversationID,
				SourceMessageID: "200", MessageType: other.messageType,
				Snippet: sql.NullString{String: "synthetic discussion", Valid: true}},
			BodyText: sql.NullString{String: "synthetic discussion", Valid: true},
		})
		require.NoError(err)
	}
	engine := query.NewSQLiteEngine(st.DB())
	if st.IsPostgreSQL() {
		engine = query.NewEngineWithDialect(st.DB(), query.PostgreSQLQueryDialect{})
	}
	rows, err := engine.SearchFast(t.Context(), search.ParseOR("discussion"), query.MessageFilter{SourceID: &source.ID, MessageType: "threads"}, 100, 0)
	require.NoError(err)
	require.Len(rows, 1)
	assert.Equal(id, rows[0].ID)
	if !st.IsPostgreSQL() {
		_, err = st.DB().ExecContext(t.Context(), `DROP TABLE messages_fts`)
		require.NoError(err)
		fallback := query.NewSQLiteEngine(st.DB())
		rows, err = fallback.SearchFast(t.Context(), search.ParseOR("discussion"), query.MessageFilter{SourceID: &source.ID, MessageType: "threads"}, 100, 0)
		require.NoError(err)
		require.Len(rows, 1)
		assert.Equal(id, rows[0].ID)
	}
}
