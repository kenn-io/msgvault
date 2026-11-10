package query

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil/dbtest"
)

func TestMetaMessagesVisibleAsTextConversations(t *testing.T) {
	for _, test := range []struct{ kind, source, identifier, conversationID, messageID, conversationType, title, snippet string }{
		{"facebook_comments", "facebook_comments", "app:9:page:100", "100_200", "100_201", "public_comments", "Example post", "Example comment"},
		{"instagram_comments", "instagram_comments", "app:9:page:100", "100_200", "100_201", "public_comments", "Example post", "Example comment"},
		{"instagram", "instagram_messages", "orbit.example", "inbox/fictional", "ig:v1:9eb6fb43c374a0f75d8413e28359697c136b8c31492b361d95a71f2d60998e6e:f381d2c678356978d8dd328d259fb5c61f79f27b7cfd885a661ba280c8416ef0:0", "direct_chat", "Fictional circle", "The telescope is ready."},
	} {
		t.Run(test.kind, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			tdb := dbtest.NewTestDB(t, "../store/schema.sql")
			_, err := tdb.DB.ExecContext(t.Context(), `INSERT INTO sources(id,source_type,identifier) VALUES(7,?,?)`, test.source, test.identifier)
			require.NoError(err)
			_, err = tdb.DB.ExecContext(t.Context(), `INSERT INTO conversations(id,source_id,source_conversation_id,conversation_type,title) VALUES(701,7,?,?,?)`, test.conversationID, test.conversationType, test.title)
			require.NoError(err)
			_, err = tdb.DB.ExecContext(t.Context(), `INSERT INTO participants(id,display_name) VALUES(11,'Mira Example')`)
			require.NoError(err)
			_, err = tdb.DB.ExecContext(t.Context(), `INSERT INTO messages(id,conversation_id,source_id,source_message_id,message_type,sent_at,snippet,sender_id) VALUES(801,701,7,?,?,'2026-10-01 12:00:00',?,11)`, test.messageID, test.kind, test.snippet)
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
