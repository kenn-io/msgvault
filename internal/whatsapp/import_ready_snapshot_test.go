package whatsapp

import (
	"testing"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep the package constructor available beside local helpers.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep the package constructor available beside local helpers.
	"go.kenn.io/msgvault/internal/testutil"
)

// Run the native Android SQLite reader, including its group participant
// fallback. A required projection failure must not archive a partial message.
func TestImportWhatsAppAndroidRequiredSnapshotFailureRollsBackNativeRelations(t *testing.T) {
	for _, table := range []string{"message_bodies", "message_raw", "conversation_participants"} {
		t.Run(table, func(t *testing.T) {
			assert := Assert.New(t)

			require := Require.New(t)

			path := createGroupParticipantsImportFixture(t, nil)
			st := testutil.NewTestStore(t)
			if st.IsPostgreSQL() {
				_, err := st.DB().Exec(`CREATE FUNCTION synthetic_whatsapp_ready_abort() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'synthetic required WhatsApp snapshot failure'; END $$`)
				require.NoError(err)
				_, err = st.DB().Exec(`CREATE TRIGGER synthetic_whatsapp_ready_abort BEFORE INSERT ON ` + table + ` FOR EACH ROW EXECUTE FUNCTION synthetic_whatsapp_ready_abort()`)
				require.NoError(err)
			} else {
				_, err := st.DB().Exec(`CREATE TRIGGER synthetic_whatsapp_ready_abort BEFORE INSERT ON ` + table + ` BEGIN SELECT RAISE(ABORT,'synthetic required WhatsApp snapshot failure'); END`)
				require.NoError(err)
			}
			opts := DefaultOptions()
			opts.Phone = "+12025550123"
			opts.DisplayName = "Synthetic Owner"
			summary, err := NewImporter(st, nil).Import(t.Context(), path, opts)
			require.NoError(err, "native per-message failures preserve continuation")
			assert.Zero(summary.MessagesAdded)
			assert.Positive(summary.Errors)
			for _, name := range []string{"participants", "participant_identifiers", "conversations", "conversation_participants", "messages", "message_bodies", "message_raw", "import_message_decisions", "mcp_event_log"} {
				var n int
				require.NoError(st.DB().QueryRow("SELECT COUNT(*) FROM " + name).Scan(&n))
				assert.Zero(n, "failed native snapshot leaked "+name)
			}
		})
	}
}
