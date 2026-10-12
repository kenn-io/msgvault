package slack

import (
	"os"
	"path/filepath"
	"testing"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep the package constructor available beside local helpers.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep the package constructor available beside local helpers.
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestImportSlackdumpNotifyingRequiredSnapshotRollsBackNativeRelations(t *testing.T) {
	for _, table := range []string{"message_bodies", "message_raw", "message_recipients", "reactions", "mcp_event_log"} {
		t.Run(table, func(t *testing.T) {
			require := Require.New(t)

			path := syntheticImportSlackdump(t)
			require.NoError(os.WriteFile(filepath.Join(path, "example-channel/2024-01-01.json"), []byte(`[{"type":"message","ts":"1704067200.000001","user":"U_PEER","text":"Synthetic offline Slack message <@U_OWNER>","reactions":[{"name":"thumbsup","users":["U_OWNER"],"count":1}]}]`), 0o600))
			st := testutil.NewTestStore(t)
			_, err := st.ConfigureMCPEvents(t.Context(), store.MCPEventsConfig{Enabled: true, Principal: "owner:synthetic-slackdump", ImportNotifications: true, Capabilities: []store.MCPEventCapability{{Family: "msgvault.import_message_archived", SourceType: "slackdump", Kinds: []string{"message"}}}})
			require.NoError(err)
			if st.IsPostgreSQL() {
				_, err = st.DB().Exec(`CREATE FUNCTION synthetic_slackdump_ready_abort() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'synthetic required Slackdump snapshot failure'; END $$`)
				require.NoError(err)
				_, err = st.DB().Exec(`CREATE TRIGGER synthetic_slackdump_ready_abort BEFORE INSERT ON ` + table + ` FOR EACH ROW EXECUTE FUNCTION synthetic_slackdump_ready_abort()`)
			} else {
				_, err = st.DB().Exec(`CREATE TRIGGER synthetic_slackdump_ready_abort BEFORE INSERT ON ` + table + ` BEGIN SELECT RAISE(ABORT,'synthetic required Slackdump snapshot failure'); END`)
			}
			require.NoError(err)
			_, err = ImportSlackdump(t.Context(), st, path, SlackdumpImportOptions{Me: "owner@example.net", NotifyImports: true})
			require.Error(err, "native required projection failures remain fatal")
			for _, name := range []string{"participants", "participant_identifiers", "conversations", "conversation_participants", "messages", "message_bodies", "message_raw", "message_recipients", "reactions", "import_message_decisions", "mcp_event_log"} {
				var n int
				require.NoError(st.DB().QueryRow("SELECT COUNT(*) FROM " + name).Scan(&n))
				Assert.Zero(t, n, "failed native Slackdump snapshot leaked "+name)
			}
		})
	}
}
