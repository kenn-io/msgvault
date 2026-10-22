package importer

import (
	"os"
	"path/filepath"
	"testing"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep the package constructor available beside local helpers.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep the package constructor available beside local helpers.
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestImportMboxNativeNotificationRequiresBothExplicitChoices(t *testing.T) {
	for _, choice := range []struct {
		name        string
		global, run bool
		want        int
		reject      bool
	}{
		{"default-silent", false, false, 0, false},
		{"global-only", true, false, 0, false},
		{"run-only", false, true, 0, true},
		{"both-opt-ins", true, true, 1, false},
	} {
		t.Run(choice.name, func(t *testing.T) {
			assert := Assert.New(t)

			require := Require.New(t)

			st, tmp := testutil.NewTestStore(t), t.TempDir()
			_, err := st.ConfigureMCPEvents(t.Context(), store.MCPEventsConfig{Enabled: true, Principal: "owner:synthetic-native-mbox", ImportNotifications: choice.global, Capabilities: []store.MCPEventCapability{{Family: "msgvault.import_message_archived", SourceType: "mbox", Kinds: []string{"message"}}}})
			require.NoError(err)
			root := filepath.Join(tmp, "synthetic.mbox")
			raw := []byte("From sender@example.net Mon Jan 1 12:00:00 2024\nFrom: sender@example.net\r\nTo: owner@example.net\r\nMessage-ID: <synthetic-choice@example.net>\r\nSubject: Synthetic explicit choice\r\n\r\nSynthetic body\r\n")
			require.NoError(os.WriteFile(root, raw, 0o600))
			summary, err := ImportMbox(t.Context(), st, root, MboxImportOptions{Identifier: "owner@example.net", NotifyImports: choice.run, NoResume: true})
			if choice.reject {
				require.Error(err)
				for _, table := range []string{"messages", "sources", "sync_runs", "import_runs"} {
					var count int
					require.NoError(st.DB().QueryRowContext(t.Context(), "SELECT COUNT(*) FROM "+table).Scan(&count))
					assert.Zero(count, "disabled notification request is rejected before native archive writes")
				}
				return
			}
			require.NoError(err)
			require.NotNil(summary)
			assert.False(summary.HardErrors)
			require.NotEmpty(summary.ImportRunID)
			var count int
			require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM mcp_event_log WHERE family='msgvault.import_message_archived'`).Scan(&count))
			assert.Equal(choice.want, count)
			// A fresh native invocation creates a new logical run but never emits a
			// second event for the same MIME identity.
			second, err := ImportMbox(t.Context(), st, root, MboxImportOptions{Identifier: "owner@example.net", NotifyImports: choice.run, NoResume: true})
			require.NoError(err)
			require.NotNil(second)
			assert.NotEqual(summary.ImportRunID, second.ImportRunID)
			require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM mcp_event_log WHERE family='msgvault.import_message_archived'`).Scan(&count))
			assert.Equal(choice.want, count)
		})
	}
}
