package fbmessenger

import (
	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep the package constructor available beside local helpers.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep the package constructor available beside local helpers.
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"testing"
)

func configureSyntheticMessengerImports(t *testing.T, st *store.Store, global bool) {
	t.Helper()
	_, err := st.ConfigureMCPEvents(t.Context(), store.MCPEventsConfig{Enabled: true, Principal: "owner:synthetic-messenger", ImportNotifications: global, Capabilities: []store.MCPEventCapability{{Family: "msgvault.import_message_archived", SourceType: "facebook_messenger", Kinds: []string{"message"}}}})
	Require.NoError(t, err)
}

func TestImportMessengerRequiresBothNotificationChoices(t *testing.T) {
	for _, tc := range []struct {
		name           string
		global, notify bool
		events         int
		reject         bool
	}{
		{"default-off", false, false, 0, false}, {"global-only", true, false, 0, false}, {"run-only", false, true, 0, true}, {"both", true, true, 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := Assert.New(t)

			require := Require.New(t)

			st := testutil.NewTestStore(t)
			configureSyntheticMessengerImports(t, st, tc.global)
			opts := syntheticMessengerImport(t)
			opts.NotifyImports = tc.notify
			first, err := ImportDYI(t.Context(), st, opts)
			if tc.reject {
				require.ErrorContains(err, "import_notifications_disabled")
				for _, table := range []string{"sources", "sync_runs", "import_runs", "participants", "messages", "mcp_event_log"} {
					var count int
					require.NoError(st.DB().QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count))
					assert.Zero(count)
				}
				return
			}
			require.NoError(err)
			require.NotEmpty(first.ImportRunID)
			var notify, completed bool
			var format string
			require.NoError(st.DB().QueryRow(st.Rebind(`SELECT import_format,notify_imports,completed FROM import_runs WHERE id=?`), first.ImportRunID).Scan(&format, &notify, &completed))
			assert.Equal("facebook_messenger", format)
			assert.Equal(tc.notify, notify)
			assert.True(completed)
			second, err := ImportDYI(t.Context(), st, opts)
			require.NoError(err)
			assert.NotEqual(first.ImportRunID, second.ImportRunID)
			var events, messages int
			require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log WHERE family='msgvault.import_message_archived'`).Scan(&events))
			assert.Equal(tc.events, events)
			require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&messages))
			assert.Equal(1, messages)
		})
	}
}

func TestImportMessengerUnavailableProducerRejectsBeforeWrites(t *testing.T) {
	require := Require.New(t)

	st := testutil.NewTestStore(t)
	_, err := st.ConfigureMCPEvents(t.Context(), store.MCPEventsConfig{Enabled: true, ImportNotifications: true, Principal: "owner:synthetic-messenger", Capabilities: []store.MCPEventCapability{{Family: "msgvault.import_message_archived", SourceType: "eml", Kinds: []string{"message"}}}})
	require.NoError(err)
	opts := syntheticMessengerImport(t)
	opts.NotifyImports = true
	_, err = ImportDYI(t.Context(), st, opts)
	require.ErrorContains(err, "import_notifications_unavailable")
	for _, table := range []string{"sources", "sync_runs", "import_runs", "participants", "messages", "mcp_event_log"} {
		var count int
		require.NoError(st.DB().QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count))
		Assert.Zero(t, count)
	}
}
