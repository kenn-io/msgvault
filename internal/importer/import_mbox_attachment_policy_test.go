package importer

import (
	"os"
	"path/filepath"
	"testing"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep the package constructor available beside local helpers.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep the package constructor available beside local helpers.
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	testemail "go.kenn.io/msgvault/internal/testutil/email"
)

// Silent imports retain the archive's best-effort media behavior. Explicitly
// notifying imports cannot publish a receipt before selected media is ready.
func TestImportMboxAttachmentFailureRespectsNotificationChoice(t *testing.T) {
	for _, notify := range []bool{false, true} {
		t.Run(map[bool]string{false: "silent", true: "notifying"}[notify], func(t *testing.T) {
			assert := Assert.New(t)

			require := Require.New(t)

			st, tmp := testutil.NewTestStore(t), t.TempDir()
			_, err := st.ConfigureMCPEvents(t.Context(), store.MCPEventsConfig{Enabled: true, Principal: "synthetic-owner", ImportNotifications: true, Capabilities: []store.MCPEventCapability{{Family: "msgvault.import_message_archived", SourceType: "mbox", Kinds: []string{"message"}}}})
			require.NoError(err)
			raw := testemail.NewMessage().From("sender@example.net").To("owner@example.net").Header("Message-ID", "<synthetic-attachment@example.net>").Body("synthetic body").WithAttachment("synthetic.txt", "text/plain", []byte("synthetic attachment")).Bytes()
			mailbox := filepath.Join(tmp, "synthetic.mbox")
			require.NoError(os.WriteFile(mailbox, append([]byte("From sender@example.net Mon Jan 1 12:00:00 2024\n"), raw...), 0o600))
			attachments := filepath.Join(tmp, "attachments")
			require.NoError(os.WriteFile(attachments, []byte("synthetic not a directory"), 0o600))
			opts := MboxImportOptions{Identifier: "owner@example.net", NoResume: true, NotifyImports: notify, AttachmentsDir: attachments}
			first, err := ImportMbox(t.Context(), st, mailbox, opts)
			require.NoError(err)
			var rows, events int
			require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM message_raw`).Scan(&rows))
			require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log`).Scan(&events))
			assert.Zero(events)
			if notify {
				assert.Zero(rows)
				assert.Positive(first.Errors)
				var decisions int
				require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM import_message_decisions`).Scan(&decisions))
				assert.Zero(decisions, "failed readiness must not consume the first-import decision")
			} else {
				assert.Equal(1, rows)
				assert.Zero(first.Errors)
				assert.Equal(int64(1), first.MessagesAdded)
			}
			require.NoError(os.Remove(attachments))
			require.NoError(os.MkdirAll(attachments, 0o700))
			second, err := ImportMbox(t.Context(), st, mailbox, opts)
			require.NoError(err)
			require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM attachments`).Scan(&rows))
			require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log`).Scan(&events))
			if notify {
				assert.Equal(int64(1), second.MessagesAdded)
				assert.Equal(1, rows)
				assert.Equal(1, events)
			} else {
				assert.Equal(int64(1), second.MessagesSkipped)
				assert.Zero(rows)
				assert.Zero(events)
			}
		})
	}
}
