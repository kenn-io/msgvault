package importer

import (
	"testing"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep the package constructor available beside local helpers.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep the package constructor available beside local helpers.
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func configureNativePSTImports(t *testing.T, st *store.Store, global, available bool) {
	t.Helper()
	cfg := store.MCPEventsConfig{Enabled: true, Principal: "owner:synthetic-pst", ImportNotifications: global}
	if available {
		cfg.Capabilities = []store.MCPEventCapability{{Family: "msgvault.import_message_archived", SourceType: "pst", Kinds: []string{"message"}}}
	}
	_, err := st.ConfigureMCPEvents(t.Context(), cfg)
	Require.NoError(t, err)
}

func TestNativePSTImportRequiresBothNotificationChoices(t *testing.T) {
	for _, tc := range []struct {
		name                   string
		global, run, available bool
	}{
		{"both_off", false, false, true}, {"global_only", true, false, true},
		{"run_only", false, true, true}, {"both_on", true, true, true},
		{"consumer_unavailable", true, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := Assert.New(t)

			require := Require.New(t)

			st := testutil.NewTestStore(t)
			configureNativePSTImports(t, st, tc.global, tc.available)
			summary, err := ImportPst(t.Context(), st, nativeSyntheticPST(t), PstImportOptions{Identifier: "owner@example.net", NoResume: true, NotifyImports: tc.run})
			if tc.run && (!tc.global || !tc.available) {
				require.Error(err)
				for _, table := range []string{"sources", "participants", "conversations", "messages", "sync_runs", "import_runs", "mcp_event_log"} {
					var count int
					require.NoError(st.DB().QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count))
					assert.Zero(count, "unavailable opt-in must fail before "+table)
				}
				return
			}
			require.NoError(err)
			require.NotNil(summary)
			require.False(summary.HardErrors)
			require.NotEmpty(summary.ImportRunID)
			var events int
			require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log WHERE family='msgvault.import_message_archived'`).Scan(&events))
			want := 0
			if tc.run {
				want = 1
			}
			assert.Equal(want, events)
			var frozen bool
			require.NoError(st.DB().QueryRow(st.Rebind(`SELECT notify_imports FROM import_runs WHERE id=?`), summary.ImportRunID).Scan(&frozen))
			assert.Equal(tc.run, frozen)
		})
	}
}
