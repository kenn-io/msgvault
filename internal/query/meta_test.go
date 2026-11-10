package query

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil/dbtest"
)

func TestImportedTypesVisibleAsTextConversations(t *testing.T) {
	for _, tc := range []struct{ messageType, conversationType string }{
		{"facebook_comments", "public_comments"},
		{"instagram_comments", "public_comments"},
		{"groupme", "group_chat"},
	} {
		t.Run(tc.messageType, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			tdb := dbtest.NewTestDB(t, "../store/schema.sql")
			_, err := tdb.DB.ExecContext(t.Context(), fmt.Sprintf(`
 INSERT INTO sources(id,source_type,identifier) VALUES(7,'%[1]s','app:9:page:100');
 INSERT INTO conversations(id,source_id,source_conversation_id,conversation_type,title) VALUES(701,7,'100_200','%[2]s','Example post');
 INSERT INTO participants(id,display_name) VALUES(11,'Example Customer');
 INSERT INTO messages(id,conversation_id,source_id,source_message_id,message_type,sent_at,snippet,sender_id) VALUES(801,701,7,'100_201','%[1]s','2026-10-01 12:00:00','Example comment',11);
 INSERT INTO conversation_participants(conversation_id,participant_id) VALUES(701,11);
 `, tc.messageType, tc.conversationType))
			require.NoError(err)
			rows, err := NewSQLiteEngine(tdb.DB).ListConversations(t.Context(), TextFilter{})
			require.NoError(err)
			require.Len(rows, 1)
			assert.Equal(int64(701), rows[0].ConversationID)
			assert.Contains(KnownMessageTypes, tc.messageType)
			assert.True(IsChatEntry(tc.messageType, tc.conversationType))
		})
	}
}
