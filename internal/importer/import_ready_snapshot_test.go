package importer

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep the package constructor available beside local helpers.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep the package constructor available beside local helpers.
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestImportEMLNotificationRequiresCompleteNativeMIMESnapshot(t *testing.T) {
	for _, failure := range []string{"none", "message_raw", "attachments"} {
		t.Run(failure, func(t *testing.T) {
			assert := Assert.New(t)

			require := Require.New(t)

			st, tmp := testutil.NewTestStore(t), t.TempDir()
			_, err := st.ConfigureMCPEvents(t.Context(), store.MCPEventsConfig{Enabled: true, Principal: "owner:synthetic-ready-eml", ImportNotifications: true, Capabilities: []store.MCPEventCapability{{Family: "msgvault.import_message_archived", SourceType: "eml", Kinds: []string{"message"}}}})
			require.NoError(err)
			root := filepath.Join(tmp, "Synthetic.mailbox")
			require.NoError(os.MkdirAll(root, 0o700))
			raw := []byte("From: sender@example.net\r\nTo: owner@example.net\r\nMessage-ID: <synthetic-ready@example.net>\r\nSubject: Synthetic ready message\r\nMIME-Version: 1.0\r\nContent-Type: multipart/mixed; boundary=synthetic-boundary\r\n\r\n--synthetic-boundary\r\nContent-Type: text/plain\r\n\r\nSynthetic ready body\r\n--synthetic-boundary\r\nContent-Type: application/octet-stream\r\nContent-Disposition: attachment; filename=synthetic.bin\r\nContent-Transfer-Encoding: base64\r\n\r\nc3ludGhldGlj\r\n--synthetic-boundary--\r\n")
			require.NoError(os.WriteFile(filepath.Join(root, "message.eml"), raw, 0o600))
			if failure != "none" {
				if st.IsPostgreSQL() {
					_, err = st.DB().ExecContext(t.Context(), `CREATE FUNCTION synthetic_required_snapshot_failure() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'synthetic required snapshot failure'; END $$`)
					require.NoError(err)
					_, err = st.DB().ExecContext(t.Context(), fmt.Sprintf(`CREATE TRIGGER synthetic_required_snapshot_failure BEFORE INSERT ON %s FOR EACH ROW EXECUTE FUNCTION synthetic_required_snapshot_failure()`, failure))
				} else {
					_, err = st.DB().ExecContext(t.Context(), fmt.Sprintf(`CREATE TRIGGER synthetic_required_snapshot_failure BEFORE INSERT ON %s BEGIN SELECT RAISE(ABORT,'synthetic required snapshot failure'); END`, failure))
				}
				require.NoError(err)
			}
			summary, err := ImportEMLDir(t.Context(), st, root, EMLImportOptions{Identifier: "owner@example.net", NotifyImports: true, NoResume: true, AttachmentsDir: filepath.Join(tmp, "synthetic-attachments")})
			require.NoError(err)
			require.NotNil(summary)
			tables := []string{"messages", "participants", "conversations", "message_bodies", "message_raw", "attachments", "import_message_decisions", "mcp_event_log"}
			if failure != "none" {
				assert.True(summary.HardErrors, "a selected attachment reference is a required snapshot write")
				for _, table := range tables {
					var count int
					require.NoError(st.DB().QueryRowContext(t.Context(), "SELECT COUNT(*) FROM "+table).Scan(&count))
					assert.Zero(count, "failed native snapshot must leave no participants, archive row, provenance or notification")
				}
				return
			}
			assert.False(summary.HardErrors)
			for _, table := range tables {
				var count int
				require.NoError(st.DB().QueryRowContext(t.Context(), "SELECT COUNT(*) FROM "+table).Scan(&count))
				expected := 1
				if table == "participants" {
					expected = 2
				}
				assert.Equal(expected, count)
			}
			var messageID int64
			require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT message_id FROM mcp_event_log WHERE family='msgvault.import_message_archived'`).Scan(&messageID))
			var body string
			require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT body_text FROM message_bodies WHERE message_id=?`), messageID).Scan(&body))
			assert.Contains(body, "Synthetic ready body")
			var recipients int
			require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT COUNT(*) FROM message_recipients WHERE message_id=?`), messageID).Scan(&recipients))
			assert.Equal(2, recipients)
		})
	}
}
