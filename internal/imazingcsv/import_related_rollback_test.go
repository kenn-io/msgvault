package imazingcsv

import (
	"fmt"
	"testing"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep the package constructor available beside local helpers.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep the package constructor available beside local helpers.
	"go.kenn.io/msgvault/internal/testutil"
)

// A required canonical failure must not leave participant/identifier rows that
// a native receipt would depend on. Each retry uses the real CSV parser and
// message transaction; triggers inject the failure at the database boundary.
func TestImportImazingRequiredSnapshotFailureRollsBackRelatedIdentities(t *testing.T) {
	for _, kind := range []struct{ name, id string }{{"phone", "+15550000002"}, {"email", "sender@example.net"}, {"opaque", "synthetic-short-code"}, {"name-only", ""}} {
		for _, table := range []string{"message_bodies", "message_raw"} {
			t.Run(kind.name+"/"+table, func(t *testing.T) {
				assert := Assert.New(t)

				require := Require.New(t)

				st := testutil.NewTestStore(t)
				row := []string{"Synthetic Conversation", "2024-06-01 12:00:00", "", "", "iMessage", "Incoming", kind.id, "Synthetic Sender", "", "", "", "Synthetic atomic snapshot", "", ""}
				export := newTestExport(t, [][]string{row})
				importer := NewImporter(st, Options{Owner: "+15550000001", Timezone: "UTC"})
				var err error
				if st.IsPostgreSQL() {
					_, err = st.DB().Exec(`CREATE FUNCTION synthetic_imazing_related_abort() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'synthetic required snapshot failure'; END $$`)
					require.NoError(err)
					_, err = st.DB().Exec(fmt.Sprintf(`CREATE TRIGGER synthetic_imazing_related_abort BEFORE INSERT ON %s FOR EACH ROW EXECUTE FUNCTION synthetic_imazing_related_abort()`, table))
				} else {
					_, err = st.DB().Exec(fmt.Sprintf(`CREATE TRIGGER synthetic_imazing_related_abort BEFORE INSERT ON %s BEGIN SELECT RAISE(ABORT,'synthetic required snapshot failure'); END`, table))
				}
				require.NoError(err)
				_, err = importer.ImportPath(t.Context(), export)
				require.ErrorContains(err, "synthetic required snapshot failure")
				for _, name := range []string{"participants", "participant_identifiers", "conversations", "conversation_participants", "messages", "message_bodies", "message_raw", "import_occurrence_allocations", "import_message_decisions", "mcp_event_log"} {
					var count int
					require.NoError(st.DB().QueryRow("SELECT COUNT(*) FROM " + name).Scan(&count))
					assert.Zero(count, "failed native snapshot must roll back "+name)
				}
				if st.IsPostgreSQL() {
					_, err = st.DB().Exec(`DROP TRIGGER synthetic_imazing_related_abort ON ` + table)
					require.NoError(err)
					_, err = st.DB().Exec(`DROP FUNCTION synthetic_imazing_related_abort()`)
				} else {
					_, err = st.DB().Exec(`DROP TRIGGER synthetic_imazing_related_abort`)
				}
				require.NoError(err)
				summary, err := importer.ImportPath(t.Context(), export)
				require.NoError(err)
				assert.Equal(2, summary.Participants)
				for _, name := range []string{"conversations", "messages", "message_bodies", "message_raw", "import_occurrence_allocations", "import_message_decisions"} {
					var count int
					require.NoError(st.DB().QueryRow("SELECT COUNT(*) FROM " + name).Scan(&count))
					assert.Equal(1, count, "native retry commits exactly one "+name)
				}
				var participants int
				require.NoError(st.DB().QueryRow("SELECT COUNT(*) FROM participants").Scan(&participants))
				assert.Equal(2, participants)
			})
		}
	}
}
