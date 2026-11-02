package query

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil/dbtest"
)

func TestLinkedInVisibleAsTextConversation(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	db := dbtest.NewTestDB(t, "../store/schema.sql")
	_, err := db.DB.Exec(`
 INSERT INTO sources(id,source_type,identifier) VALUES(7,'linkedin_export','example');
 INSERT INTO conversations(id,source_id,source_conversation_id,conversation_type,title) VALUES(701,7,'post:example','public_comments','Example post');
 INSERT INTO participants(id,display_name) VALUES(11,'Alex Example');
 INSERT INTO messages(id,conversation_id,source_id,source_message_id,message_type,sent_at,snippet,sender_id) VALUES(801,701,7,'comment:example','linkedin','2026-10-01 12:00:00','Example comment',11);
 INSERT INTO conversation_participants(conversation_id,participant_id) VALUES(701,11);
 `)
	require.NoError(err)
	rows, err := NewSQLiteEngine(db.DB).ListConversations(t.Context(), TextFilter{})
	require.NoError(err)
	require.Len(rows, 1)
	assert.Equal(int64(701), rows[0].ConversationID)
	assert.True(IsKnownMessageType("linkedin"))
	assert.True(IsChatEntry("linkedin", "public_comments"))
}
