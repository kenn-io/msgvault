package importer

import (
	"os"
	"path/filepath"
	"testing"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep the package constructor available beside local helpers.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep the package constructor available beside local helpers.
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

// Missing the parser-specific ready producer must reject the explicit request,
// even when global import permission and an unrelated capability are enabled.
func TestImportNativeNotificationUnavailableRejectsBeforeWrites(t *testing.T) {
	for _, format := range []string{"eml", "maildir", "mbox", "apple-mail"} {
		for _, unavailable := range []string{"missing", "wrong-format", "wrong-kind", "wrong-family"} {
			t.Run(format+"/"+unavailable, func(t *testing.T) {
				assert := Assert.New(t)

				require := Require.New(t)

				st := testutil.NewTestStore(t)
				cfg := store.MCPEventsConfig{Enabled: true, Principal: "owner:synthetic-availability", ImportNotifications: true}
				switch unavailable {
				case "wrong-format":
					cfg.Capabilities = []store.MCPEventCapability{{Family: "msgvault.import_message_archived", SourceType: "gmail", Kinds: []string{"message"}}}
				case "wrong-kind":
					cfg.Capabilities = []store.MCPEventCapability{{Family: "msgvault.import_message_archived", SourceType: format, Kinds: []string{"reaction"}}}
				case "wrong-family":
					cfg.Capabilities = []store.MCPEventCapability{{Family: "msgvault.message_archived", SourceType: format, Kinds: []string{"message"}}}
				}
				_, err := st.ConfigureMCPEvents(t.Context(), cfg)
				require.NoError(err)
				raw := []byte("From: sender@example.net\r\nTo: owner@example.net\r\nMessage-ID: <unavailable-import@example.net>\r\nSubject: Synthetic unavailable producer\r\n\r\nSynthetic body\r\n")
				root := t.TempDir()
				switch format {
				case "eml":
					mailbox := filepath.Join(root, "Synthetic.mailbox")
					require.NoError(os.MkdirAll(mailbox, 0o700))
					require.NoError(os.WriteFile(filepath.Join(mailbox, "message.eml"), raw, 0o600))
					_, err = ImportEMLDir(t.Context(), st, mailbox, EMLImportOptions{Identifier: "owner@example.net", SourceType: "gmail", NotifyImports: true, NoResume: true})
				case "maildir":
					for _, name := range []string{"cur", "new", "tmp"} {
						require.NoError(os.MkdirAll(filepath.Join(root, name), 0o700))
					}
					require.NoError(os.WriteFile(filepath.Join(root, "new", "synthetic-message"), raw, 0o600))
					_, err = ImportMaildir(t.Context(), st, root, MaildirImportOptions{Identifier: "owner@example.net", SourceType: "gmail", NotifyImports: true, NoResume: true})
				case "mbox":
					path := filepath.Join(root, "synthetic.mbox")
					require.NoError(os.WriteFile(path, append([]byte("From sender@example.net Mon Jan 1 12:00:00 2024\n"), raw...), 0o600))
					_, err = ImportMbox(t.Context(), st, path, MboxImportOptions{Identifier: "owner@example.net", SourceType: "gmail", NotifyImports: true, NoResume: true})
				case "apple-mail":
					mailbox := filepath.Join(root, "Mailboxes", "Synthetic.mbox", "Messages")
					require.NoError(os.MkdirAll(mailbox, 0o700))
					mkEmlx(t, mailbox, "1.emlx", raw)
					_, err = ImportEmlxDir(t.Context(), st, root, EmlxImportOptions{Identifier: "owner@example.net", SourceType: "gmail", NotifyImports: true, NoResume: true})
				}
				require.ErrorContains(err, "import_notifications_unavailable")
				for _, table := range []string{"sources", "sync_runs", "import_runs", "messages", "mcp_event_log"} {
					var count int
					require.NoError(st.DB().QueryRowContext(t.Context(), "SELECT COUNT(*) FROM "+table).Scan(&count))
					assert.Zero(count, "unavailable producer must reject before writing %s", table)
				}
			})
		}
	}
}
