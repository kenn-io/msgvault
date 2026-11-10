package query

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil/dbtest"
)

func TestMetaMessagesVisibleAsTextConversations(t *testing.T) {
	for _, test := range []struct{ kind, source, identifier, conversationType, title, snippet string }{
		{"facebook_comments", "facebook_comments", "app:9:page:100", "public_comments", "Example post", "Example comment"},
		{"instagram_comments", "instagram_comments", "app:9:page:100", "public_comments", "Example post", "Example comment"},
		{"instagram", "instagram_messages", "orbit.example", "direct_chat", "Fictional circle", "The telescope is ready."},
	} {
		t.Run(test.kind, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			tdb := dbtest.NewTestDB(t, "../store/schema.sql")
			_, err := tdb.DB.ExecContext(t.Context(), `INSERT INTO sources(id,source_type,identifier) VALUES(7,?,?)`, test.source, test.identifier)
			require.NoError(err)
			_, err = tdb.DB.ExecContext(t.Context(), `INSERT INTO conversations(id,source_id,source_conversation_id,conversation_type,title) VALUES(701,7,'inbox/fictional',?,?)`, test.conversationType, test.title)
			require.NoError(err)
			_, err = tdb.DB.ExecContext(t.Context(), `INSERT INTO participants(id,display_name) VALUES(11,'Mira Example')`)
			require.NoError(err)
			_, err = tdb.DB.ExecContext(t.Context(), `INSERT INTO messages(id,conversation_id,source_id,source_message_id,message_type,sent_at,snippet,sender_id) VALUES(801,701,7,'fictional-message',?,'2026-10-01 12:00:00',?,11)`, test.kind, test.snippet)
			require.NoError(err)
			_, err = tdb.DB.ExecContext(t.Context(), `INSERT INTO conversation_participants(conversation_id,participant_id) VALUES(701,11)`)
			require.NoError(err)
			rows, err := NewSQLiteEngine(tdb.DB).ListConversations(t.Context(), TextFilter{})
			require.NoError(err)
			require.Len(rows, 1)
			assert.Equal(int64(701), rows[0].ConversationID)
			assert.Contains(KnownMessageTypes, test.kind)
			assert.True(IsChatEntry(test.kind, test.conversationType))
		})
	}
}
