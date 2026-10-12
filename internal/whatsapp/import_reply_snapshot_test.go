package whatsapp

import (
	"database/sql"
	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep the package constructor available beside local helpers.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep the package constructor available beside local helpers.
	"go.kenn.io/msgvault/internal/testutil"
	"testing"
)

func syntheticAndroidQuote(t *testing.T) string {
	t.Helper()
	return createGroupParticipantsImportFixture(t, func(db *sql.DB) {
		_, err := db.Exec(`CREATE TABLE message_quoted(message_row_id INTEGER,key_id TEXT); INSERT INTO message_quoted VALUES(200,'message-incoming');`)
		Require.NoError(t, err)
	})
}

func TestImportWhatsAppAndroidReplyFailureRollsBackChildSnapshot(t *testing.T) {
	assert := Assert.New(t)

	require := Require.New(t)

	path := syntheticAndroidQuote(t)
	st := testutil.NewTestStore(t)
	if st.IsPostgreSQL() {
		_, err := st.DB().Exec(`CREATE FUNCTION synthetic_android_reply_abort() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.source_message_id='message-second-group' AND NEW.reply_to_message_id IS NOT NULL THEN RAISE EXCEPTION 'synthetic required reply failure'; END IF; RETURN NEW; END $$`)
		require.NoError(err)
		_, err = st.DB().Exec(`CREATE TRIGGER synthetic_android_reply_abort BEFORE UPDATE ON messages FOR EACH ROW EXECUTE FUNCTION synthetic_android_reply_abort()`)
		require.NoError(err)
	} else {
		_, err := st.DB().Exec(`CREATE TRIGGER synthetic_android_reply_abort BEFORE UPDATE OF reply_to_message_id ON messages WHEN NEW.source_message_id='message-second-group' AND NEW.reply_to_message_id IS NOT NULL BEGIN SELECT RAISE(ABORT,'synthetic required reply failure'); END`)
		require.NoError(err)
	}
	opts := DefaultOptions()
	opts.Phone = "+12025550123"
	opts.DisplayName = "Synthetic Owner"
	_, err := NewImporter(st, nil).Import(t.Context(), path, opts)
	require.Error(err, "native reply write failures remain fatal")
	var parent, child int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM messages WHERE source_message_id='message-incoming'`).Scan(&parent))
	assert.Equal(1, parent)
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM messages WHERE source_message_id='message-second-group'`).Scan(&child))
	assert.Zero(child, "required native reply failure must roll back the entire child")
}

func TestImportWhatsAppAndroidCrossChatReplyAndUnresolvedReimportRemainNative(t *testing.T) {
	assert := Assert.New(t)

	require := Require.New(t)

	path := syntheticAndroidQuote(t)
	st := testutil.NewTestStore(t)
	opts := DefaultOptions()
	opts.Phone = "+12025550123"
	opts.DisplayName = "Synthetic Owner"
	first, err := NewImporter(st, nil).Import(t.Context(), path, opts)
	require.NoError(err)
	assert.Zero(first.Errors)
	var parentID int64
	var replyID sql.NullInt64
	require.NoError(st.DB().QueryRow(`SELECT id FROM messages WHERE source_message_id='message-incoming'`).Scan(&parentID))
	require.NoError(st.DB().QueryRow(`SELECT reply_to_message_id FROM messages WHERE source_message_id='message-second-group'`).Scan(&replyID))
	require.True(replyID.Valid)
	assert.Equal(parentID, replyID.Int64)
	db, err := sql.Open("sqlite3", path)
	require.NoError(err)
	_, err = db.Exec(`UPDATE message_quoted SET key_id='synthetic-unavailable-parent'`)
	require.NoError(err)
	require.NoError(db.Close())
	second, err := NewImporter(st, nil).Import(t.Context(), path, opts)
	require.NoError(err)
	assert.Zero(second.Errors)
	require.NoError(st.DB().QueryRow(`SELECT reply_to_message_id FROM messages WHERE source_message_id='message-second-group'`).Scan(&replyID))
	require.True(replyID.Valid)
	assert.Equal(parentID, replyID.Int64)
}
