package imazingcsv

import (
	"path/filepath"
	"testing"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep the package constructor available beside local helpers.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep the package constructor available beside local helpers.
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestImportImazingNotificationUnavailableRejectsBeforeNativeWrites(t *testing.T) {
	for _, tc := range []struct {
		name    string
		enabled bool
		code    string
	}{{"global-off", false, "import_notifications_disabled"}, {"producer-unavailable", true, "import_notifications_unavailable"}} {
		t.Run(tc.name, func(t *testing.T) {
			require := Require.New(t)

			st := testutil.NewTestStore(t)
			_, err := st.ConfigureMCPEvents(t.Context(), store.MCPEventsConfig{Enabled: true, ImportNotifications: tc.enabled, Principal: "owner:synthetic-imazing", Capabilities: []store.MCPEventCapability{{Family: "msgvault.import_message_archived", SourceType: "eml", Kinds: []string{"message"}}}})
			require.NoError(err)
			_, err = NewImporter(st, Options{Owner: "+15550000001", Timezone: "UTC", NotifyImports: true}).ImportPath(t.Context(), filepath.Join(t.TempDir(), "not-opened"))
			require.ErrorContains(err, tc.code, "opt-in authority must be checked before export discovery")
			for _, table := range []string{"sources", "sync_runs", "import_runs", "messages", "import_message_decisions", "mcp_event_log"} {
				var count int
				require.NoError(st.DB().QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count))
				Assert.Zero(t, count, "rejected native notification must not create "+table)
			}
		})
	}
}

func TestImportImazingNotificationRequiresBothChoicesAndRetainsLogicalResult(t *testing.T) {
	for _, tc := range []struct {
		name           string
		global, notify bool
		events         int
	}{{"default-off", false, false, 0}, {"global-only", true, false, 0}, {"both-opted-in", true, true, 1}} {
		t.Run(tc.name, func(t *testing.T) {
			assert := Assert.New(t)

			require := Require.New(t)

			st := testutil.NewTestStore(t)
			_, err := st.ConfigureMCPEvents(t.Context(), store.MCPEventsConfig{Enabled: true, ImportNotifications: tc.global, Principal: "owner:synthetic-imazing", Capabilities: []store.MCPEventCapability{{Family: "msgvault.import_message_archived", SourceType: SourceType, Kinds: []string{"message"}}}})
			require.NoError(err)
			row := []string{"Synthetic Conversation", "2024-06-01 12:00:00", "", "", "iMessage", "Incoming", "sender@example.net", "Synthetic Sender", "", "", "", "Synthetic opted import", "", ""}
			export := newTestExport(t, [][]string{row})
			importer := NewImporter(st, Options{Owner: "+15550000001", Timezone: "UTC", NotifyImports: tc.notify})
			first, err := importer.ImportPath(t.Context(), export)
			require.NoError(err)
			require.NotEmpty(first.ImportRunID)
			var notify, complete bool
			var format string
			require.NoError(st.DB().QueryRow(st.Rebind("SELECT import_format,notify_imports,completed FROM import_runs WHERE id=?"), first.ImportRunID).Scan(&format, &notify, &complete))
			assert.Equal(SourceType, format)
			assert.Equal(tc.notify, notify)
			assert.True(complete)
			second, err := importer.ImportPath(t.Context(), export)
			require.NoError(err)
			require.NotEmpty(second.ImportRunID)
			assert.NotEqual(first.ImportRunID, second.ImportRunID)
			var events, messages int
			require.NoError(st.DB().QueryRow("SELECT COUNT(*) FROM mcp_event_log WHERE family='msgvault.import_message_archived'").Scan(&events))
			assert.Equal(tc.events, events, "fresh native runs do not replay the same logical occurrence")
			require.NoError(st.DB().QueryRow("SELECT COUNT(*) FROM messages").Scan(&messages))
			assert.Equal(1, messages)
		})
	}
}
