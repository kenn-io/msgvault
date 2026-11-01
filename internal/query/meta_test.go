package query

import (
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil/dbtest"
	"testing"
)

func TestFacebookCommentsVisibleAsTextConversations(t *testing.T) {
	tdb := dbtest.NewTestDB(t, "../store/schema.sql")
	_, err := tdb.DB.Exec(`
 INSERT INTO sources(id,source_type,identifier) VALUES(7,'facebook_comments','app:9:page:100');
 INSERT INTO conversations(id,source_id,source_conversation_id,conversation_type,title) VALUES(701,7,'100_200','public_comments','Example post');
 INSERT INTO participants(id,display_name) VALUES(11,'Example Customer');
 INSERT INTO messages(id,conversation_id,source_id,source_message_id,message_type,sent_at,snippet,sender_id) VALUES(801,701,7,'100_201','facebook_comments','2026-10-01 12:00:00','Example comment',11);
 INSERT INTO conversation_participants(conversation_id,participant_id) VALUES(701,11);
 `)
	require.NoError(t, err)
	rows, err := NewSQLiteEngine(tdb.DB).ListConversations(t.Context(), TextFilter{})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, int64(701), rows[0].ConversationID)
	assert.True(t, IsKnownMessageType("facebook_comments"))
	assert.True(t, IsChatEntry("facebook_comments", "public_comments"))
}
