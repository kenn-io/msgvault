package imazingcsv

import (
	"os"
	"path/filepath"
	"testing"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep the package constructor available beside local helpers.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep the package constructor available beside local helpers.
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestImportImazingNotifyingAttachmentFailureContinuesIndependentRows(t *testing.T) {
	for _, boundary := range []string{"file", "database"} {
		t.Run(boundary, func(t *testing.T) {
			assert := Assert.New(t)

			require := Require.New(t)

			st := testutil.NewTestStore(t)
			_, err := st.ConfigureMCPEvents(t.Context(), store.MCPEventsConfig{Enabled: true, ImportNotifications: true, Principal: "owner:synthetic-partial", Capabilities: []store.MCPEventCapability{{Family: "msgvault.import_message_archived", SourceType: SourceType, Kinds: []string{"message"}}}})
			require.NoError(err)
			bad := []string{"Synthetic Conversation", "2024-06-01 11:00:00", "", "", "iMessage", "Incoming", "+15550000002", "Synthetic Sender", "", "", "", "Synthetic unready parent", "bad.bin", "application/octet-stream"}
			child := []string{"Synthetic Conversation", "2024-06-01 12:00:00", "", "", "iMessage", "Outgoing", "", "", "", "Replying to:\nSynthetic Sender (+15550000002)\n2024-06-01 11:00:00\nSynthetic unready parent", "", "Synthetic dependent child", "", ""}
			good := []string{"Synthetic Conversation", "2024-06-01 13:00:00", "", "", "iMessage", "Outgoing", "", "", "", "", "", "Synthetic independent ready message", "good.bin", "application/octet-stream"}
			if boundary == "file" {
				bad[12] = "../bad.bin"
			}
			export := newTestExport(t, [][]string{bad, child, good})
			require.NoError(os.MkdirAll(filepath.Join(export, "attachments"), 0o700))
			for _, name := range []string{"bad.bin", "good.bin"} {
				require.NoError(os.WriteFile(filepath.Join(export, "attachments", name), []byte("Synthetic "+name+" bytes"), 0o600))
			}
			if boundary == "database" {
				if st.IsPostgreSQL() {
					_, err = st.DB().Exec(`CREATE FUNCTION synthetic_partial_attachment_abort() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.filename='bad.bin' THEN RAISE EXCEPTION 'synthetic partial attachment failure'; END IF; RETURN NEW; END $$`)
					require.NoError(err)
					_, err = st.DB().Exec(`CREATE TRIGGER synthetic_partial_attachment_abort BEFORE INSERT ON attachments FOR EACH ROW EXECUTE FUNCTION synthetic_partial_attachment_abort()`)
				} else {
					_, err = st.DB().Exec(`CREATE TRIGGER synthetic_partial_attachment_abort BEFORE INSERT ON attachments WHEN NEW.filename='bad.bin' BEGIN SELECT RAISE(ABORT,'synthetic partial attachment failure'); END`)
				}
				require.NoError(err)
			}
			opts := Options{Owner: "+15550000001", Timezone: "UTC", NotifyImports: true, AttachmentsDir: t.TempDir()}
			_, err = NewImporter(st, opts).ImportPath(t.Context(), export)
			if boundary == "file" {
				require.ErrorContains(err, "unsafe iMazing attachment reference")
			} else {
				require.ErrorContains(err, "synthetic partial attachment failure")
			}
			for _, table := range []string{"messages", "message_bodies", "message_raw", "attachments", "import_occurrence_allocations", "import_message_decisions", "mcp_event_log"} {
				var count int
				require.NoError(st.DB().QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count))
				assert.Equal(1, count, "independently ready later row must commit in "+table)
			}
			var ready int
			require.NoError(st.DB().QueryRow("SELECT COUNT(*) FROM messages WHERE snippet='Synthetic independent ready message'").Scan(&ready))
			assert.Equal(1, ready)
			var failedRun string
			require.NoError(st.DB().QueryRow("SELECT import_run_id FROM import_message_decisions").Scan(&failedRun))
			if boundary == "database" {
				if st.IsPostgreSQL() {
					_, err = st.DB().Exec(`DROP TRIGGER synthetic_partial_attachment_abort ON attachments`)
					require.NoError(err)
					_, err = st.DB().Exec(`DROP FUNCTION synthetic_partial_attachment_abort()`)
				} else {
					_, err = st.DB().Exec(`DROP TRIGGER synthetic_partial_attachment_abort`)
				}
				require.NoError(err)
			}
			bad[12] = "bad.bin"
			writeTestCSV(t, filepath.Join(export, "csv", "messages.csv"), [][]string{bad, child, good})
			retry, err := NewImporter(st, opts).ImportPath(t.Context(), export)
			require.NoError(err)
			assert.Equal(1, retry.RepliesLinked)
			assert.NotEqual(failedRun, retry.ImportRunID)
			var events int
			require.NoError(st.DB().QueryRow("SELECT COUNT(*) FROM mcp_event_log").Scan(&events))
			assert.Equal(3, events, "retry notifies only the two formerly unready occurrences")
			var decisions int
			require.NoError(st.DB().QueryRow(st.Rebind("SELECT COUNT(*) FROM import_message_decisions WHERE import_run_id=?"), retry.ImportRunID).Scan(&decisions))
			assert.Equal(2, decisions)
		})
	}
}
