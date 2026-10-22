package imessage

import (
	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep the package constructor available beside local helpers.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep the package constructor available beside local helpers.
	"go.kenn.io/msgvault/internal/testutil"
	"testing"
)

func TestImportImessageRequiredGeneratedGroupTitleRollsBack(t *testing.T) {
	assert := Assert.New(t)

	require := Require.New(t)

	path, db := newChatDB(t)
	_, err := db.Exec(`INSERT INTO message(date,text) VALUES(757425600,'Synthetic group title'); INSERT INTO chat(guid,display_name,chat_identifier) VALUES('any;+;synthetic-group','','synthetic-group'); INSERT INTO chat_message_join(chat_id,message_id) VALUES(1,1); INSERT INTO chat_handle_join(chat_id,handle_id) VALUES(1,1)`)
	require.NoError(err)
	client, err := NewClient(path)
	require.NoError(err)
	t.Cleanup(func() { Require.NoError(t, client.Close()) })
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("apple_messages", "synthetic-device")
	require.NoError(err)
	if st.IsPostgreSQL() {
		_, err = st.DB().Exec(`CREATE FUNCTION synthetic_imessage_title_abort() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.title<>'' THEN RAISE EXCEPTION 'synthetic required title failure'; END IF; RETURN NEW; END $$`)
		require.NoError(err)
		_, err = st.DB().Exec(`CREATE TRIGGER synthetic_imessage_title_abort BEFORE UPDATE ON conversations FOR EACH ROW EXECUTE FUNCTION synthetic_imessage_title_abort()`)
	} else {
		_, err = st.DB().Exec(`CREATE TRIGGER synthetic_imessage_title_abort BEFORE UPDATE OF title ON conversations WHEN NEW.title<>'' BEGIN SELECT RAISE(ABORT,'synthetic required title failure'); END`)
	}
	require.NoError(err)
	summary, err := client.Import(t.Context(), st, source.ID)
	require.NoError(err)
	assert.Zero(summary.MessagesImported)
	assert.Equal(1, summary.Skipped)
	for _, name := range []string{"participants", "participant_identifiers", "conversations", "conversation_participants", "messages", "message_bodies", "message_raw", "message_recipients", "message_labels", "import_message_decisions", "mcp_event_log"} {
		var count int
		require.NoError(st.DB().QueryRow("SELECT COUNT(*) FROM " + name).Scan(&count))
		assert.Zero(count, "failed title projection leaked "+name)
	}
}
