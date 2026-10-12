package imessage

import (
	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep the package constructor available beside local helpers.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep the package constructor available beside local helpers.
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"testing"
)

func syntheticAppleImportClient(t *testing.T, opts ...ClientOption) (*Client, error) {
	t.Helper()
	path, db := newChatDB(t)
	_, err := db.Exec(`INSERT INTO message(date,text) VALUES(757425600,'Synthetic opted-in Apple message')`)
	Require.NoError(t, err)
	client, err := NewClient(path, opts...)
	if err == nil {
		t.Cleanup(func() { Require.NoError(t, client.Close()) })
	}
	return client, err
}

func configureSyntheticAppleImports(t *testing.T, st *store.Store, global bool) {
	t.Helper()
	_, err := st.ConfigureMCPEvents(t.Context(), store.MCPEventsConfig{Enabled: true, Principal: "owner:synthetic-apple", ImportNotifications: global, Capabilities: []store.MCPEventCapability{{Family: "msgvault.import_message_archived", SourceType: "apple_messages", Kinds: []string{"message"}}}})
	Require.NoError(t, err)
}

func TestImportImessageRequiresBothNotificationChoices(t *testing.T) {
	for _, tc := range []struct {
		name           string
		global, notify bool
		events         int
		reject         bool
	}{{"default-off", false, false, 0, false}, {"global-only", true, false, 0, false}, {"run-only", false, true, 0, true}, {"both", true, true, 1, false}} {
		t.Run(tc.name, func(t *testing.T) {
			assert := Assert.New(t)

			require := Require.New(t)

			st := testutil.NewTestStore(t)
			configureSyntheticAppleImports(t, st, tc.global)
			source, err := st.GetOrCreateSource("apple_messages", "synthetic-device")
			require.NoError(err)
			client, err := syntheticAppleImportClient(t, WithNotifyImports(tc.notify))
			require.NoError(err)
			first, err := client.Import(t.Context(), st, source.ID)
			if tc.reject {
				require.ErrorContains(err, "import_notifications_disabled")
				for _, table := range []string{"sync_runs", "import_runs", "participants", "messages", "mcp_event_log"} {
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
			assert.Equal("apple_messages", format)
			assert.Equal(tc.notify, notify)
			assert.True(completed)
			second, err := client.Import(t.Context(), st, source.ID)
			require.NoError(err)
			assert.NotEqual(first.ImportRunID, second.ImportRunID)
			var events, messages int
			require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log WHERE family='msgvault.import_message_archived'`).Scan(&events))
			require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&messages))
			assert.Equal(tc.events, events)
			assert.Equal(1, messages)
		})
	}
}

func TestImportImessageUnavailableProducerRejectsBeforeWrites(t *testing.T) {
	require := Require.New(t)

	st := testutil.NewTestStore(t)
	_, err := st.ConfigureMCPEvents(t.Context(), store.MCPEventsConfig{Enabled: true, ImportNotifications: true, Principal: "owner:synthetic-apple", Capabilities: []store.MCPEventCapability{{Family: "msgvault.import_message_archived", SourceType: "eml", Kinds: []string{"message"}}}})
	require.NoError(err)
	source, err := st.GetOrCreateSource("apple_messages", "synthetic-device")
	require.NoError(err)
	client, err := syntheticAppleImportClient(t, WithNotifyImports(true))
	require.NoError(err)
	_, err = client.Import(t.Context(), st, source.ID)
	require.ErrorContains(err, "import_notifications_unavailable")
	for _, table := range []string{"sync_runs", "import_runs", "participants", "messages", "mcp_event_log"} {
		var count int
		require.NoError(st.DB().QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count))
		Assert.Zero(t, count)
	}
}
