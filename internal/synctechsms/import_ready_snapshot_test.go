package synctechsms

import (
	"os"
	"path/filepath"
	"testing"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep the package constructor available beside local helpers.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep the package constructor available beside local helpers.
	"go.kenn.io/msgvault/internal/testutil"
)

// The real backup parser currently writes identity/conversation/message/body/raw
// separately. A required write must not leave an incomplete ready projection.
func TestImportSynctechRequiredSnapshotFailureRollsBackNativeRelations(t *testing.T) {
	fixtures := []struct{ name, xml string }{
		{"sms", `<smses count="1"><sms address="+15550000002" date="1717214400000" type="1" body="Synthetic SMS body" contact_name="Synthetic Sender" /></smses>`},
		{"mms", `<smses count="1"><mms date="1717214460000" msg_box="1" m_id="synthetic-mms-1" sub="Synthetic subject"><parts><part seq="0" ct="text/plain" text="Synthetic MMS body" /><part seq="1" ct="application/octet-stream" cl="synthetic.bin" data="U3ludGhldGljIGF0dGFjaG1lbnQ=" /></parts><addrs><addr address="+15550000002" type="137" charset="106" /><addr address="+15550000001" type="151" charset="106" /></addrs></mms></smses>`},
		{"call", `<calls count="1"><call number="+15550000002" duration="42" date="1717218000000" type="1" presentation="1" contact_name="Synthetic Sender" /></calls>`},
	}
	for _, fixture := range fixtures {
		for _, table := range []string{"message_bodies", "message_raw"} {
			t.Run(fixture.name+"/"+table, func(t *testing.T) {
				require := Require.New(t)

				st := testutil.NewTestStore(t)
				var err error
				if st.IsPostgreSQL() {
					_, err = st.DB().Exec(`CREATE FUNCTION synthetic_synctech_ready_abort() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'synthetic required backup snapshot failure'; END $$`)
					require.NoError(err)
					_, err = st.DB().Exec(`CREATE TRIGGER synthetic_synctech_ready_abort BEFORE INSERT ON ` + table + ` FOR EACH ROW EXECUTE FUNCTION synthetic_synctech_ready_abort()`)
				} else {
					_, err = st.DB().Exec(`CREATE TRIGGER synthetic_synctech_ready_abort BEFORE INSERT ON ` + table + ` BEGIN SELECT RAISE(ABORT,'synthetic required backup snapshot failure'); END`)
				}
				require.NoError(err)
				directory := t.TempDir()
				require.NoError(os.WriteFile(filepath.Join(directory, fixture.name+".xml"), []byte(fixture.xml), 0o600))
				_, err = NewImporter(st, ImportOptions{OwnerPhone: "+15550000001", AttachmentsDir: t.TempDir(), IncludeSMS: true, IncludeMMS: true, IncludeCalls: true, IncludeAttachments: true}).ImportPath(directory)
				require.ErrorContains(err, "synthetic required backup snapshot failure")
				for _, name := range []string{"participants", "participant_identifiers", "conversations", "conversation_participants", "messages", "message_bodies", "message_raw", "message_recipients", "attachments", "import_message_decisions", "import_message_identities", "mcp_event_log"} {
					var count int
					require.NoError(st.DB().QueryRow("SELECT COUNT(*) FROM " + name).Scan(&count))
					Assert.Zero(t, count, "failed canonical write must roll back "+name)
				}
			})
		}
	}
}
