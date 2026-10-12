package whatsapp

import (
	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep the package constructor available beside local helpers.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep the package constructor available beside local helpers.
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"path/filepath"
	"testing"
)

func configureSyntheticWhatsAppImports(t *testing.T, st *store.Store, global bool) {
	t.Helper()
	_, err := st.ConfigureMCPEvents(t.Context(), store.MCPEventsConfig{Enabled: true, Principal: "owner:synthetic-whatsapp", ImportNotifications: global, Capabilities: []store.MCPEventCapability{{Family: "msgvault.import_message_archived", SourceType: "whatsapp", Kinds: []string{"message"}}}})
	Require.NoError(t, err)
}

func TestImportWhatsAppAndroidRequiresBothNotificationChoices(t *testing.T) {
	for _, tc := range []struct {
		name           string
		global, notify bool
		events         int
		reject         bool
	}{
		{"default-off", false, false, 0, false}, {"global-only", true, false, 0, false}, {"run-only", false, true, 0, true}, {"both", true, true, 2, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := Assert.New(t)

			require := Require.New(t)

			st := testutil.NewTestStore(t)
			configureSyntheticWhatsAppImports(t, st, tc.global)
			path := createGroupParticipantsImportFixture(t, nil)
			opts := DefaultOptions()
			opts.Phone = "+12025550123"
			opts.DisplayName = "Synthetic Owner"
			opts.NotifyImports = tc.notify
			first, err := NewImporter(st, nil).Import(t.Context(), path, opts)
			if tc.reject {
				require.ErrorContains(err, "import_notifications_disabled")
				for _, table := range []string{"sources", "sync_runs", "import_runs", "participants", "messages", "mcp_event_log"} {
					var n int
					require.NoError(st.DB().QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n))
					assert.Zero(n, table)
				}
				return
			}
			require.NoError(err)
			require.NotEmpty(first.ImportRunID)
			var notify, completed bool
			var format string
			require.NoError(st.DB().QueryRow(st.Rebind(`SELECT import_format,notify_imports,completed FROM import_runs WHERE id=?`), first.ImportRunID).Scan(&format, &notify, &completed))
			assert.Equal("whatsapp", format)
			assert.Equal(tc.notify, notify)
			assert.True(completed)
			second, err := NewImporter(st, nil).Import(t.Context(), path, opts)
			require.NoError(err)
			assert.NotEqual(first.ImportRunID, second.ImportRunID)
			var events, messages int
			require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log WHERE family='msgvault.import_message_archived'`).Scan(&events))
			assert.Equal(tc.events, events)
			require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&messages))
			assert.Equal(2, messages)
		})
	}
}

func TestImportWhatsAppNativeUnavailableProducerRejectsBeforeWrites(t *testing.T) {
	for _, apple := range []bool{false, true} {
		t.Run(map[bool]string{false: "Android", true: "Apple"}[apple], func(t *testing.T) {
			require := Require.New(t)

			st := testutil.NewTestStore(t)
			_, err := st.ConfigureMCPEvents(t.Context(), store.MCPEventsConfig{Enabled: true, Principal: "owner:synthetic-whatsapp", ImportNotifications: true, Capabilities: []store.MCPEventCapability{{Family: "msgvault.import_message_archived", SourceType: "eml", Kinds: []string{"message"}}}})
			require.NoError(err)
			var path string
			if apple {
				path = createAppleChatFixture(t)
			} else {
				path = createGroupParticipantsImportFixture(t, nil)
			}
			opts := DefaultOptions()
			opts.Phone = "+12025550123"
			opts.NotifyImports = true
			_, err = NewImporter(st, nil).Import(t.Context(), path, opts)
			require.ErrorContains(err, "import_notifications_unavailable")
			for _, table := range []string{"sources", "sync_runs", "import_runs", "participants", "messages", "mcp_event_log"} {
				var n int
				require.NoError(st.DB().QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n))
				Assert.Zero(t, n, table)
			}
		})
	}
}

func TestImportWhatsAppAppleFreshLogicalRunsDoNotInheritLiveMode(t *testing.T) {
	assert := Assert.New(t)

	require := Require.New(t)

	path := createAppleChatFixture(t)
	createAppleLIDFixture(t, filepath.Dir(path))
	st := testutil.NewTestStore(t)
	opts := ImportOptions{Phone: "+15555550100", DisplayName: "Synthetic Owner"}
	client := NewImporter(st.WithIngestContext(store.IngestContext{Mode: store.IngestLive}), nil)
	var previous string
	for range 2 {
		summary, err := client.Import(t.Context(), path, opts)
		require.NoError(err)
		require.NotEmpty(summary.ImportRunID)
		assert.NotEqual(previous, summary.ImportRunID)
		previous = summary.ImportRunID
	}
	var completed, messages, decisions, events int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM import_runs WHERE completed=TRUE AND import_format='whatsapp' AND notify_imports=FALSE`).Scan(&completed))
	assert.Equal(2, completed)
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&messages))
	assert.Equal(4, messages)
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM import_message_decisions`).Scan(&decisions))
	assert.Equal(4, decisions)
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log`).Scan(&events))
	assert.Zero(events)
}
