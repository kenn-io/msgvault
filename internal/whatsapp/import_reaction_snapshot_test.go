package whatsapp

import (
	"database/sql"
	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep the package constructor available beside local helpers.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep the package constructor available beside local helpers.
	"go.kenn.io/msgvault/internal/testutil"
	"testing"
)

func syntheticAndroidReactions(t *testing.T) string {
	t.Helper()
	return createGroupParticipantsImportFixture(t, func(db *sql.DB) {
		_, err := db.Exec(`CREATE TABLE message_add_on(_id INTEGER PRIMARY KEY,parent_message_row_id INTEGER,sender_jid_row_id INTEGER);
		CREATE TABLE message_add_on_reaction(message_add_on_row_id INTEGER,reaction TEXT,sender_timestamp INTEGER);
		INSERT INTO message_add_on VALUES(1,100,5),(2,200,5);
		INSERT INTO message_add_on_reaction VALUES(1,'+1',1700000002000),(2,'+1',1700000002000);`)
		Require.NoError(t, err)
	})
}

func TestImportWhatsAppAndroidReactionFailureRollsBackNativeSnapshot(t *testing.T) {
	assert := Assert.New(t)

	require := Require.New(t)

	path := syntheticAndroidReactions(t)
	st := testutil.NewTestStore(t)
	if st.IsPostgreSQL() {
		_, err := st.DB().Exec(`CREATE FUNCTION synthetic_android_reaction_abort() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'synthetic required reaction failure'; END $$`)
		require.NoError(err)
		_, err = st.DB().Exec(`CREATE TRIGGER synthetic_android_reaction_abort BEFORE INSERT ON reactions FOR EACH ROW EXECUTE FUNCTION synthetic_android_reaction_abort()`)
		require.NoError(err)
	} else {
		_, err := st.DB().Exec(`CREATE TRIGGER synthetic_android_reaction_abort BEFORE INSERT ON reactions BEGIN SELECT RAISE(ABORT,'synthetic required reaction failure'); END`)
		require.NoError(err)
	}
	opts := DefaultOptions()
	opts.Phone = "+12025550123"
	opts.DisplayName = "Synthetic Owner"
	summary, err := NewImporter(st, nil).Import(t.Context(), path, opts)
	require.NoError(err)
	assert.Zero(summary.MessagesAdded)
	assert.Positive(summary.Errors)
	for _, table := range []string{"participants", "participant_identifiers", "conversations", "conversation_participants", "messages", "message_bodies", "message_raw", "reactions", "import_message_decisions", "mcp_event_log"} {
		var n int
		require.NoError(st.DB().QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n))
		assert.Zero(n, table)
	}
}
