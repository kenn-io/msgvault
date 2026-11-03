package query

import (
	"strings"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil/dbtest"
	"testing"
)

func TestMetaCommentsVisibleAsTextConversations(t *testing.T) {
	for _, kind := range []string{"facebook_comments", "instagram_comments"} {
		t.Run(kind, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			tdb := dbtest.NewTestDB(t, "../store/schema.sql")
			_, err := tdb.DB.Exec(strings.ReplaceAll(`
 INSERT INTO sources(id,source_type,identifier) VALUES(7,'facebook_comments','app:9:page:100');
 INSERT INTO conversations(id,source_id,source_conversation_id,conversation_type,title) VALUES(701,7,'100_200','public_comments','Example post');
 INSERT INTO participants(id,display_name) VALUES(11,'Example Customer');
 INSERT INTO messages(id,conversation_id,source_id,source_message_id,message_type,sent_at,snippet,sender_id) VALUES(801,701,7,'100_201','facebook_comments','2026-10-01 12:00:00','Example comment',11);
 INSERT INTO conversation_participants(conversation_id,participant_id) VALUES(701,11);
 `, "facebook_comments", kind))
			require.NoError(err)
			rows, err := NewSQLiteEngine(tdb.DB).ListConversations(t.Context(), TextFilter{})
			require.NoError(err)
			require.Len(rows, 1)
			assert.Equal(int64(701), rows[0].ConversationID)
			assert.True(IsKnownMessageType(kind))
			assert.True(IsChatEntry(kind, "public_comments"))
		})
	}
}

func TestMessagesTypesVisible(t *testing.T) {
	for _, kind := range []string{"facebook_messages", "instagram_messages"} {
		t.Run(kind, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)

			assert.Contains(TextMessageTypes, kind)
			tdb := dbtest.NewTestDB(t, "../store/schema.sql")
			_, err := tdb.DB.Exec(strings.ReplaceAll(`
INSERT INTO sources(id,source_type,identifier) VALUES(7,'facebook_messages','app:9:page:100');
INSERT INTO conversations(id,source_id,source_conversation_id,conversation_type,title) VALUES(701,7,'psid:300','direct_chat','Example Customer');
INSERT INTO participants(id,display_name) VALUES(11,'Example Customer');
INSERT INTO messages(id,conversation_id,source_id,source_message_id,message_type,sent_at,snippet,sender_id) VALUES(801,701,7,'m_1','facebook_messages','2026-11-01 12:00:00','inbox fixture',11);
INSERT INTO conversation_participants(conversation_id,participant_id) VALUES(701,11);
`, "facebook_messages", kind))
			require.NoError(err)
			rows, err := NewSQLiteEngine(tdb.DB).ListConversations(t.Context(), TextFilter{})
			require.NoError(err)
			require.Len(rows, 1)
			assert.Equal(int64(701), rows[0].ConversationID)
			assert.True(IsKnownMessageType(kind))
			assert.True(IsChatEntry(kind, "direct_chat"))
		})
	}
}
