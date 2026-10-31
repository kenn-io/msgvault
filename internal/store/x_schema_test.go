package store_test

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestXSchemaUpgradeAndReceiptLifetime(t *testing.T) {
	for _, upgrade := range []bool{false, true} {
		name := "fresh"
		if upgrade {
			name = "upgrade15"
		}
		t.Run(name, func(t *testing.T) {
			st := testutil.NewTestStore(t)
			if upgrade {
				_, err := st.DB().Exec("DROP TABLE IF EXISTS x_reply_attempts; DROP TABLE IF EXISTS x_post_versions")
				require.NoError(t, err)
				_, err = st.DB().Exec("UPDATE archive_metadata SET value='15' WHERE key='schema_version'")
				require.NoError(t, err)
				require.Error(t, st.ValidateSchemaContext(t.Context()))
				require.NoError(t, st.InitSchemaContext(t.Context()))
			}
			source, err := st.GetOrCreateSource("x", "100")
			require.NoError(t, err)
			conv, err := st.EnsureConversationWithType(source.ID, "200", "channel", "X conversation 200")
			require.NoError(t, err)
			mid, err := st.UpsertMessage(&store.Message{SourceID: source.ID, ConversationID: conv, SourceMessageID: "200", MessageType: "x"})
			require.NoError(t, err)
			_, err = st.DB().Exec(st.Rebind(`INSERT INTO x_post_versions(source_id,post_id,canonical_id,message_id,version_position) VALUES(?,?,?,?,?)`), source.ID, "200", "200", mid, 0)
			require.NoError(t, err)
			_, err = st.DB().Exec(st.Rebind(`INSERT INTO x_post_versions(source_id,post_id,canonical_id,message_id,version_position) VALUES(?,?,?,?,?)`), source.ID, "200", "200", mid, 0)
			require.Error(t, err)
			_, err = st.DB().Exec(st.Rebind(`INSERT INTO chat_drafts(draft_id,source_id,conversation_id,source_conversation_id,conversation_type,reply_to_source_message_id,body,revision) VALUES(?,?,?,?,?,?,?,?)`), "draft-example", source.ID, conv, "200", "channel", "200", "Synthetic response", 1)
			require.NoError(t, err)
			insert := func(attempt, state string) error {
				_, err := st.DB().Exec(st.Rebind(`INSERT INTO x_reply_attempts(source_id,owner_id,draft_id,attempt_id,draft_revision,target_post_id,source_conversation_id,body_digest,state,reservation_id) VALUES(?,?,?,?,?,?,?,?,?,?)`), source.ID, "100", "draft-example", attempt, 1, "200", "200", "digest", state, "request-1")
				return err
			}
			require.NoError(t, insert("rejected-1", "rejected"))
			require.NoError(t, insert("rejected-2", "rejected"))
			require.NoError(t, insert("accepted-1", "remote_accepted"))
			for _, state := range []string{"admitted", "remote_unknown", "remote_accepted", "sent"} {
				require.Error(t, insert("blocked-"+state, state))
			}
			require.Error(t, insert("invalid", "made_up"))
			_, err = st.DB().Exec(st.Rebind("DELETE FROM chat_drafts WHERE draft_id=?"), "draft-example")
			require.NoError(t, err)
			var count int
			require.NoError(t, st.DB().QueryRow("SELECT count(*) FROM x_reply_attempts").Scan(&count))
			assert.Equal(t, 3, count)
			// Content removal leaves ID aliases, including after deleting the local message.
			_, err = st.DB().Exec(st.Rebind("UPDATE x_post_versions SET suppressed_at=CURRENT_TIMESTAMP,availability='removed',cleanup_pending=1 WHERE source_id=?"), source.ID)
			require.NoError(t, err)
			_, err = st.DB().Exec(st.Rebind("DELETE FROM messages WHERE id=?"), mid)
			require.NoError(t, err)
			var linked sql.NullInt64
			var postID string
			require.NoError(t, st.DB().QueryRow("SELECT message_id,post_id FROM x_post_versions").Scan(&linked, &postID))
			assert.False(t, linked.Valid)
			assert.Equal(t, "200", postID)
			require.NoError(t, st.RemoveSource(source.ID))
			require.NoError(t, st.DB().QueryRow("SELECT count(*) FROM x_reply_attempts").Scan(&count))
			assert.Zero(t, count)
			require.NoError(t, st.DB().QueryRow("SELECT count(*) FROM x_post_versions").Scan(&count))
			assert.Zero(t, count)
		})
	}
}
