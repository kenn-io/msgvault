package imazingcsv

import (
	"strconv"
	"testing"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep the package constructor available beside local helpers.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep the package constructor available beside local helpers.
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestImportImazingCanonicalSnapshotSurvivesDerivedFTSFailure(t *testing.T) {
	for _, notify := range []bool{false, true} {
		t.Run(strconv.FormatBool(notify), func(t *testing.T) {
			assert := Assert.New(t)

			require := Require.New(t)

			st := testutil.NewTestStore(t)
			_, err := st.ConfigureMCPEvents(t.Context(), store.MCPEventsConfig{Enabled: true, ImportNotifications: true, Principal: "owner:synthetic-fts", Capabilities: []store.MCPEventCapability{{Family: "msgvault.import_message_archived", SourceType: SourceType, Kinds: []string{"message"}}}})
			require.NoError(err)
			if st.IsPostgreSQL() {
				_, err = st.DB().Exec(`ALTER TABLE messages ADD CONSTRAINT synthetic_imazing_fts_failure CHECK (search_fts IS NULL)`)
			} else {
				_, err = st.DB().Exec(`DROP TABLE messages_fts`)
			}
			require.NoError(err)
			row := []string{"Synthetic Conversation", "2024-06-01 12:00:00", "", "", "iMessage", "Outgoing", "", "", "", "", "", "Synthetic ready body", "", ""}
			summary, err := NewImporter(st, Options{Owner: "+15550000001", Timezone: "UTC", NotifyImports: notify}).ImportPath(t.Context(), newTestExport(t, [][]string{row}))
			require.NoError(err, "a derived search failure cannot roll back the ready archive")
			assert.Equal(1, summary.Messages)
			assert.NotEmpty(summary.ImportRunID)
			for _, table := range []string{"messages", "message_bodies", "message_raw", "import_occurrence_allocations", "import_message_decisions"} {
				var count int
				require.NoError(st.DB().QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count))
				assert.Equal(1, count, table)
			}
			var events int
			require.NoError(st.DB().QueryRow("SELECT COUNT(*) FROM mcp_event_log").Scan(&events))
			want := 0
			if notify {
				want = 1
			}
			assert.Equal(want, events)
		})
	}
}
