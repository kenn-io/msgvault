package cmd

import (
	"database/sql"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep the package constructor available beside local helpers.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep the package constructor available beside local helpers.
)

// CLI source discovery is a write too. A rejecting import must not create its
// account source before the native consumer gets a chance to reject the flag.
func TestImportCommandRejectsNotificationBeforeSourceDiscoveryWrites(t *testing.T) {
	for _, format := range []string{"google_voice", "apple_messages", "mbox", "apple-mail"} {
		t.Run(format, func(t *testing.T) {
			require := Require.New(t)

			cfg, parent := importCommandCaptureFixture(t, false)
			input := t.TempDir()
			var command *cobra.Command
			var args []string
			switch format {
			case "google_voice":
				require.NoError(os.MkdirAll(filepath.Join(input, "Calls"), 0700))
				require.NoError(os.WriteFile(filepath.Join(input, "Phones.vcf"), []byte("BEGIN:VCARD\nVERSION:3.0\nFN:Synthetic Owner\nitem1.TEL:+15550001000\nitem1.X-ABLabel:Google Voice\nEND:VCARD\n"), 0600))
				command = cloneImportCommandForTest(t, importGvoiceCmd)
				args = []string{input}
			case "apple_messages":
				file := filepath.Join(input, "synthetic-chat.db")
				db, err := sql.Open("sqlite3", file)
				require.NoError(err)
				_, err = db.Exec("PRAGMA journal_mode=WAL; CREATE TABLE message(date INTEGER)")
				require.NoError(err)
				require.NoError(db.Close())
				command = cloneImportCommandForTest(t, importImessageCmd)
				args = []string{"--db-path", file, "--me", "owner@example.net"}
			case "mbox", "apple-mail":
				content := "From: sender@example.net\r\nTo: owner@example.net\r\nMessage-ID: <preflight@example.net>\r\nSubject: Synthetic preflight\r\n\r\nSynthetic body\r\n"
				if format == "mbox" {
					file := filepath.Join(input, "synthetic.mbox")
					require.NoError(os.WriteFile(file, []byte("From sender@example.net Mon Jan 1 00:00:00 2001\n"+content), 0600))
					command = cloneImportCommandForTest(t, importMboxCmd)
					args = []string{"owner@example.net", file, "--no-resume"}
				} else {
					directory := filepath.Join(input, "Inbox.mbox", "Messages")
					require.NoError(os.MkdirAll(directory, 0700))
					require.NoError(os.WriteFile(filepath.Join(directory, "1.emlx"), []byte(fmt.Sprintf("%d\n%s", len(content), content)), 0600))
					command = cloneImportCommandForTest(t, importEmlxCmd)
					args = []string{input, "--identifier", "owner@example.net", "--no-resume"}
				}
			}
			command.SetContext(testInvocationContext(t.Context(), cfg, invocationOptions{}))
			command.SetOut(io.Discard)
			command.SetErr(io.Discard)
			command.SetArgs(append(args, "--notify-imports"))
			require.ErrorContains(command.Execute(), "import_notifications_disabled")
			for _, table := range []string{"sources", "sync_runs", "import_runs", "messages", "mcp_event_log"} {
				var count int
				require.NoError(parent.DB().QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count))
				Assert.Zero(t, count, table)
			}
		})
	}
}
