package gvoice

import (
	"os"
	"path/filepath"
	"testing"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep the package constructor available beside local helpers.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep the package constructor available beside local helpers.
	"go.kenn.io/msgvault/internal/testutil"
)

// The native parser must not leave an incomplete canonical projection when
// a required body/raw write fails, even though the import continues per entry.
func TestImportGvoiceRequiredSnapshotFailureRollsBackNativeRelations(t *testing.T) {
	text := `<html><body><div class="message"><abbr class="dt" title="2024-06-01T12:00:00.000Z"></abbr><a class="tel" href="tel:+15550000002"><span class="fn">Synthetic Sender</span></a><q>Synthetic text</q></div></body></html>`
	group := `<html><body><div class="participants"><a class="tel" href="tel:+15550000002"></a><a class="tel" href="tel:+15550000003"></a></div><div class="message"><abbr class="dt" title="2024-06-01T12:00:00.000Z"></abbr><a class="tel" href="tel:+15550000002"><span class="fn">Synthetic Sender</span></a><q>Synthetic group text</q></div></body></html>`
	call := `<html><head><title>Received call from Synthetic Sender</title></head><body><div class="haudio"><div class="contributor vcard"><a class="tel" href="tel:+15550000002"><span class="fn">Synthetic Sender</span></a></div><abbr class="published" title="2024-06-01T12:00:00.000Z"></abbr><abbr class="duration" title="PT42S"></abbr></div></body></html>`
	voicemail := `<html><head><title>Voicemail from Synthetic Sender</title></head><body><div class="haudio"><div class="contributor vcard"><a class="tel" href="tel:+15550000002"><span class="fn">Synthetic Sender</span></a></div><abbr class="published" title="2024-06-01T12:00:00.000Z"></abbr><abbr class="duration" title="PT42S"></abbr><audio src="synthetic.mp3"></audio></div></body></html>`
	for _, fixture := range []struct{ name, filename, html string }{{"text", "Synthetic Sender - Text - 2024-06-01T12_00_00Z.html", text}, {"group", "Group Conversation - 2024-06-01T12_00_00Z.html", group}, {"call", "Synthetic Sender - Received - 2024-06-01T12_00_00Z.html", call}, {"voicemail", "Synthetic Sender - Voicemail - 2024-06-01T12_00_00Z.html", voicemail}} {
		for _, table := range []string{"message_bodies", "message_raw", "message_labels", "message_recipients", "conversation_participants"} {
			t.Run(fixture.name+"/"+table, func(t *testing.T) {
				assert := Assert.New(t)

				require := Require.New(t)

				st := testutil.NewTestStore(t)
				source, err := st.GetOrCreateSource("google_voice", "+15550000001")
				require.NoError(err)
				var triggerErr error
				if st.IsPostgreSQL() {
					_, triggerErr = st.DB().Exec(`CREATE FUNCTION synthetic_gvoice_ready_abort() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'synthetic required voice snapshot failure'; END $$`)
					require.NoError(triggerErr)
					_, triggerErr = st.DB().Exec(`CREATE TRIGGER synthetic_gvoice_ready_abort BEFORE INSERT ON ` + table + ` FOR EACH ROW EXECUTE FUNCTION synthetic_gvoice_ready_abort()`)
				} else {
					_, triggerErr = st.DB().Exec(`CREATE TRIGGER synthetic_gvoice_ready_abort BEFORE INSERT ON ` + table + ` BEGIN SELECT RAISE(ABORT,'synthetic required voice snapshot failure'); END`)
				}
				require.NoError(triggerErr)
				directory := t.TempDir()
				require.NoError(os.Mkdir(filepath.Join(directory, "Calls"), 0o700))
				require.NoError(os.WriteFile(filepath.Join(directory, "Phones.vcf"), []byte("BEGIN:VCARD\nVERSION:3.0\nitem1.TEL:+15550000001\nitem1.X-ABLabel:Google Voice\nEND:VCARD\n"), 0o600))
				require.NoError(os.WriteFile(filepath.Join(directory, "Calls", fixture.filename), []byte(fixture.html), 0o600))
				client, err := NewClient(directory)
				require.NoError(err)
				summary, err := client.Import(t.Context(), st, source.ID)
				require.NoError(err, "required row failures preserve native per-entry continuation")
				assert.Zero(summary.MessagesImported)
				assert.Equal(1, summary.Skipped)
				for _, name := range []string{"participants", "participant_identifiers", "conversations", "conversation_participants", "messages", "message_bodies", "message_raw", "message_recipients", "message_labels", "attachments", "import_message_decisions", "mcp_event_log"} {
					var count int
					require.NoError(st.DB().QueryRow("SELECT COUNT(*) FROM " + name).Scan(&count))
					assert.Zero(count, "failed native projection leaked "+name)
				}
			})
		}
	}
}
