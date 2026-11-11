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
			_testAssert := assert.New(t)
			_testRequire := require.New(t)

			st := testutil.NewTestStore(t)
			if upgrade {
				_, err := st.DB().ExecContext(t.Context(), "DROP TABLE IF EXISTS x_reply_attempts; DROP TABLE IF EXISTS x_post_versions")
				_testRequire.NoError(err)
				_, err = st.DB().ExecContext(t.Context(), "UPDATE archive_metadata SET value='15' WHERE key='schema_version'")
				_testRequire.NoError(err)
				_testRequire.Error(st.ValidateSchemaContext(t.Context()))
				_testRequire.NoError(st.InitSchemaContext(t.Context()))
			}
			source, err := st.GetOrCreateSource("x", "100")
			_testRequire.NoError(err)
			conv, err := st.EnsureConversationWithType(source.ID, "200", "channel", "X conversation 200")
			_testRequire.NoError(err)
			mid, err := st.UpsertMessage(&store.Message{SourceID: source.ID, ConversationID: conv, SourceMessageID: "200", MessageType: "x"})
			_testRequire.NoError(err)
			_, err = st.DB().ExecContext(t.Context(), st.Rebind(`INSERT INTO x_post_versions(source_id,post_id,canonical_id,message_id,version_position) VALUES(?,?,?,?,?)`), source.ID, "200", "200", mid, 0)
			_testRequire.NoError(err)
			_, err = st.DB().ExecContext(t.Context(), st.Rebind(`INSERT INTO x_post_versions(source_id,post_id,canonical_id,message_id,version_position) VALUES(?,?,?,?,?)`), source.ID, "200", "200", mid, 0)
			_testRequire.Error(err)
			_, err = st.DB().ExecContext(t.Context(), st.Rebind(`INSERT INTO chat_drafts(draft_id,source_id,conversation_id,source_conversation_id,conversation_type,reply_to_source_message_id,body,revision) VALUES(?,?,?,?,?,?,?,?)`), "draft-example", source.ID, conv, "200", "channel", "200", "Synthetic response", 1)
			_testRequire.NoError(err)
			insert := func(attempt, state string) error {
				_, err := st.DB().ExecContext(t.Context(), st.Rebind(`INSERT INTO x_reply_attempts(source_id,owner_id,draft_id,attempt_id,draft_revision,target_post_id,source_conversation_id,body_digest,state,reservation_id) VALUES(?,?,?,?,?,?,?,?,?,?)`), source.ID, "100", "draft-example", attempt, 1, "200", "200", "digest", state, "request-1")
				return err
			}
			_testRequire.NoError(insert("rejected-1", "rejected"))
			_testRequire.NoError(insert("rejected-2", "rejected"))
			_testRequire.NoError(insert("accepted-1", "remote_accepted"))
			for _, state := range []string{"admitted", "remote_unknown", "remote_accepted", "sent"} {
				_testRequire.Error(insert("blocked-"+state, state))
			}
			_testRequire.Error(insert("invalid", "made_up"))
			_, err = st.DB().ExecContext(t.Context(), st.Rebind("DELETE FROM chat_drafts WHERE draft_id=?"), "draft-example")
			_testRequire.NoError(err)
			var count int
			_testRequire.NoError(st.DB().QueryRowContext(t.Context(), "SELECT count(*) FROM x_reply_attempts").Scan(&count))
			_testAssert.Equal(3, count)
			// Content removal leaves ID aliases, including after deleting the local message.
			_, err = st.DB().ExecContext(t.Context(), st.Rebind("UPDATE x_post_versions SET suppressed_at=CURRENT_TIMESTAMP,availability='removed',cleanup_pending=1 WHERE source_id=?"), source.ID)
			_testRequire.NoError(err)
			_, err = st.DB().ExecContext(t.Context(), st.Rebind("DELETE FROM messages WHERE id=?"), mid)
			_testRequire.NoError(err)
			var linked sql.NullInt64
			var postID string
			_testRequire.NoError(st.DB().QueryRowContext(t.Context(), "SELECT message_id,post_id FROM x_post_versions").Scan(&linked, &postID))
			_testAssert.False(linked.Valid)
			_testAssert.Equal("200", postID)
			_testRequire.ErrorIs(st.RemoveSource(source.ID), store.ErrXRemovalPending)
			_testRequire.NoError(st.FinishXRemovalContext(t.Context(), source.ID, store.XRemovalWork{SourceID: source.ID, CanonicalID: "200", DerivedComplete: true}))
			_testRequire.NoError(st.RemoveSource(source.ID))
			_testRequire.NoError(st.DB().QueryRowContext(t.Context(), "SELECT count(*) FROM x_reply_attempts").Scan(&count))
			_testAssert.Zero(count)
			_testRequire.NoError(st.DB().QueryRowContext(t.Context(), "SELECT count(*) FROM x_post_versions").Scan(&count))
			_testAssert.Zero(count)
		})
	}
}
