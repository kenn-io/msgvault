package imessage

import (
	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep the package constructor available beside local helpers.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep the package constructor available beside local helpers.
	"go.kenn.io/msgvault/internal/testutil"
	"testing"
)

// Exercise the real native chat.db reader. Required projection failure must
// not leave identities, conversation membership or a partial archived message.
func TestImportImessageRequiredSnapshotFailureRollsBackNativeRelations(t *testing.T) {
	for _, owner := range []string{"", "owner@example.test", "+15550000001"} {
		for _, table := range []string{"message_bodies", "message_raw", "message_recipients", "message_labels", "conversation_participants"} {
			t.Run(owner+"/"+table, func(t *testing.T) {
				assert := Assert.New(t)

				require := Require.New(t)

				path, db := newChatDB(t)
				_, err := db.Exec(`INSERT INTO message(date,text) VALUES(757425600,'Synthetic atomic Apple message'); INSERT INTO chat(guid,display_name,chat_identifier) VALUES('any;+;synthetic-group','Synthetic group','synthetic-group'); INSERT INTO chat_message_join(chat_id,message_id) VALUES(1,1); INSERT INTO chat_handle_join(chat_id,handle_id) VALUES(1,1)`)
				require.NoError(err)
				client, err := NewClient(path, WithOwnerHandle(owner))
				require.NoError(err)
				t.Cleanup(func() { Require.NoError(t, client.Close()) })
				st := testutil.NewTestStore(t)
				source, err := st.GetOrCreateSource("apple_messages", "synthetic-device")
				require.NoError(err)
				if st.IsPostgreSQL() {
					_, err = st.DB().Exec(`CREATE FUNCTION synthetic_imessage_ready_abort() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'synthetic required Apple snapshot failure'; END $$`)
					require.NoError(err)
					_, err = st.DB().Exec(`CREATE TRIGGER synthetic_imessage_ready_abort BEFORE INSERT ON ` + table + ` FOR EACH ROW EXECUTE FUNCTION synthetic_imessage_ready_abort()`)
				} else {
					_, err = st.DB().Exec(`CREATE TRIGGER synthetic_imessage_ready_abort BEFORE INSERT ON ` + table + ` BEGIN SELECT RAISE(ABORT,'synthetic required Apple snapshot failure'); END`)
				}
				require.NoError(err)
				summary, err := client.Import(t.Context(), st, source.ID)
				require.NoError(err, "native per-message failures preserve continuation")
				assert.Zero(summary.MessagesImported)
				assert.Equal(1, summary.Skipped)
				for _, name := range []string{"participants", "participant_identifiers", "conversations", "conversation_participants", "messages", "message_bodies", "message_raw", "message_recipients", "message_labels", "import_message_decisions", "mcp_event_log"} {
					var count int
					require.NoError(st.DB().QueryRow("SELECT COUNT(*) FROM " + name).Scan(&count))
					assert.Zero(count, "failed native snapshot leaked "+name)
				}
			})
		}
	}
}
