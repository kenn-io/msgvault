package fbmessenger

import (
	"os"
	"path/filepath"
	"testing"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep the package constructor available beside local helpers.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep the package constructor available beside local helpers.
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func syntheticMessengerImport(t *testing.T) ImportOptions {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "your_activity_across_facebook/messages/inbox/synthetic_THREAD")
	Require.NoError(t, os.MkdirAll(dir, 0700))
	Require.NoError(t, os.WriteFile(filepath.Join(dir, "message_1.json"), []byte(`{
		"participants":[{"name":"Synthetic Owner"},{"name":"Synthetic Sender"}],
		"title":"Synthetic conversation", "thread_type":"Regular",
		"messages":[{"sender_name":"Synthetic Sender","timestamp_ms":1704067200000,
			"content":"Synthetic atomic Messenger message","type":"Generic",
			"reactions":[{"actor":"Synthetic Owner","reaction":"+1"}]}]
	}`), 0600))
	return ImportOptions{RootDir: root, Me: "synthetic.owner@facebook.messenger", Format: formatAuto, AttachmentsDir: t.TempDir()}
}

func messengerSnapshotAbort(t *testing.T, st *store.Store, table string) {
	t.Helper()
	if st.IsPostgreSQL() {
		_, err := st.DB().Exec(`CREATE FUNCTION synthetic_messenger_ready_abort() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'synthetic required Messenger snapshot failure'; END $$`)
		Require.NoError(t, err)
		_, err = st.DB().Exec(`CREATE TRIGGER synthetic_messenger_ready_abort BEFORE INSERT ON ` + table + ` FOR EACH ROW EXECUTE FUNCTION synthetic_messenger_ready_abort()`)
		Require.NoError(t, err)
	} else {
		_, err := st.DB().Exec(`CREATE TRIGGER synthetic_messenger_ready_abort BEFORE INSERT ON ` + table + ` BEGIN SELECT RAISE(ABORT,'synthetic required Messenger snapshot failure'); END`)
		Require.NoError(t, err)
	}
}

func TestImportMessengerRequiredSnapshotFailureRollsBackNativeRelations(t *testing.T) {
	for _, table := range []string{"message_bodies", "message_raw", "message_recipients", "message_labels", "conversation_participants", "reactions"} {
		t.Run(table, func(t *testing.T) {
			assert := Assert.New(t)

			require := Require.New(t)

			st := testutil.NewTestStore(t)
			messengerSnapshotAbort(t, st, table)
			summary, err := ImportDYI(t.Context(), st, syntheticMessengerImport(t))
			require.NoError(err, "native per-thread failures preserve continuation")
			assert.Zero(summary.MessagesAdded)
			assert.Positive(summary.Errors)
			for _, name := range []string{"participants", "participant_identifiers", "conversations", "conversation_participants", "messages", "message_bodies", "message_raw", "message_recipients", "message_labels", "reactions", "import_message_decisions", "mcp_event_log"} {
				var count int
				require.NoError(st.DB().QueryRow("SELECT COUNT(*) FROM " + name).Scan(&count))
				assert.Zero(count, "failed native snapshot leaked "+name)
			}
		})
	}
}
