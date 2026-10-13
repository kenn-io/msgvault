package slack

import (
	"testing"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep the package constructor available beside local helpers.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep the package constructor available beside local helpers.
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestImportSlackdumpRequiresBothNotificationChoices(t *testing.T) {
	for _, tc := range []struct{ global, run bool }{{false, false}, {false, true}, {true, false}, {true, true}} {
		name := "silent"
		if tc.global {
			name = "global_" + name
		}
		if tc.run {
			name += "_requested"
		}
		t.Run(name, func(t *testing.T) {
			assert := Assert.New(t)

			require := Require.New(t)

			root := syntheticImportSlackdump(t)
			st := testutil.NewTestStore(t)
			_, err := st.ConfigureMCPEvents(t.Context(), store.MCPEventsConfig{Enabled: true, Principal: "owner:synthetic-slackdump", ImportNotifications: tc.global, Capabilities: []store.MCPEventCapability{{Family: "msgvault.import_message_archived", SourceType: "slackdump", Kinds: []string{"message"}}}})
			require.NoError(err)
			summary, err := ImportSlackdump(t.Context(), st, root, SlackdumpImportOptions{Me: "owner@example.net", NotifyImports: tc.run})
			if tc.run && !tc.global {
				require.Error(err)
				for _, table := range []string{"sources", "participants", "conversations", "messages", "sync_runs", "import_runs", "import_message_decisions", "mcp_event_log"} {
					var n int
					require.NoError(st.DB().QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n))
					assert.Zero(n, "disabled request must fail before "+table)
				}
				return
			}
			require.NoError(err)
			require.NotEmpty(summary.ImportRunID)
			assert.Equal(1, summary.MessagesAdded)
			var events int
			require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log WHERE family='msgvault.import_message_archived'`).Scan(&events))
			want := 0
			if tc.global && tc.run {
				want = 1
			}
			assert.Equal(want, events)
		})
	}
}

func TestImportSlackdumpUnavailableNotificationConsumerRejectsBeforeWrites(t *testing.T) {
	require := Require.New(t)

	root := syntheticImportSlackdump(t)
	st := testutil.NewTestStore(t)
	_, err := st.ConfigureMCPEvents(t.Context(), store.MCPEventsConfig{Enabled: true, Principal: "owner:synthetic-slackdump", ImportNotifications: true})
	require.NoError(err)
	_, err = ImportSlackdump(t.Context(), st, root, SlackdumpImportOptions{Me: "owner@example.net", NotifyImports: true})
	require.Error(err)
	for _, table := range []string{"sources", "messages", "sync_runs", "import_runs"} {
		var n int
		require.NoError(st.DB().QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n))
		Assert.Zero(t, n)
	}
}
