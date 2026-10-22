package imazingcsv

import (
	"testing"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep the package constructor available beside local helpers.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep the package constructor available beside local helpers.
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestImportImazingAliasReplyCycleCanIncludeOneNewOccurrence(t *testing.T) {
	for _, notify := range []bool{false, true} {
		t.Run(map[bool]string{false: "native-silent-control", true: "notifying"}[notify], func(t *testing.T) {
			assert := Assert.New(t)

			require := Require.New(t)

			st := testutil.NewTestStore(t)
			aEarly := []string{"Synthetic Conversation", "2024-06-01 11:00:00", "", "", "iMessage", "Incoming", "+15550000002", "Synthetic Sender A", "", "", "", "Synthetic A early", "", ""}
			aLate := []string{"Synthetic Conversation", "2024-06-01 13:00:00", "", "", "iMessage", "Incoming", "+15550000002", "Synthetic Sender A", "", "Replying to:\nSynthetic Sender B (+15550000003)\n2024-06-01 12:00:00\nSynthetic B new", "", "Synthetic A late", "", ""}
			b := []string{"Synthetic Conversation", "2024-06-01 12:00:00", "", "", "iMessage", "Incoming", "+15550000003", "Synthetic Sender B", "", "Replying to:\nSynthetic Sender A (+15550000002)\n2024-06-01 11:00:00\nSynthetic A early", "", "Synthetic B new", "", ""}
			opts := Options{Owner: "+15550000001", Timezone: "UTC"}
			_, err := NewImporter(st, opts).ImportPath(t.Context(), newTestExport(t, [][]string{aEarly, aLate}))
			require.NoError(err)
			var source, conversation int64
			var from, to string
			require.NoError(st.DB().QueryRow("SELECT source_id,conversation_id,source_message_id FROM messages WHERE snippet='Synthetic A early'").Scan(&source, &conversation, &from))
			require.NoError(st.DB().QueryRow("SELECT source_message_id FROM messages WHERE snippet='Synthetic A late'").Scan(&to))
			require.NoError(st.MigrateSourceMessageID(source, conversation, from, to))
			_, err = st.ConfigureMCPEvents(t.Context(), store.MCPEventsConfig{Enabled: true, ImportNotifications: true, Principal: "owner:synthetic-mixed-cycle", Capabilities: []store.MCPEventCapability{{Family: "msgvault.import_message_archived", SourceType: SourceType, Kinds: []string{"message"}}}})
			require.NoError(err)
			opts.NotifyImports = notify
			export := newTestExport(t, [][]string{aEarly, aLate, b})
			summary, err := NewImporter(st, opts).ImportPath(t.Context(), export)
			require.NoError(err)
			assert.Equal(2, summary.RepliesLinked)
			assert.Zero(summary.RepliesUnresolved)
			var aID, bID, aParent, bParent int64
			require.NoError(st.DB().QueryRow("SELECT id,reply_to_message_id FROM messages WHERE snippet='Synthetic A late'").Scan(&aID, &aParent))
			require.NoError(st.DB().QueryRow("SELECT id,reply_to_message_id FROM messages WHERE snippet='Synthetic B new'").Scan(&bID, &bParent))
			assert.Equal(bID, aParent)
			assert.Equal(aID, bParent)
			var count int
			require.NoError(st.DB().QueryRow("SELECT COUNT(*) FROM mcp_event_log").Scan(&count))
			want := 0
			if notify {
				want = 1
			}
			assert.Equal(want, count)
			if notify {
				var messageID int64
				require.NoError(st.DB().QueryRow("SELECT message_id FROM mcp_event_log").Scan(&messageID))
				assert.Equal(bID, messageID)
			}
			_, _, err = st.DeleteAllDedupedContext(t.Context())
			require.NoError(err)
			_, err = NewImporter(st, opts).ImportPath(t.Context(), export)
			require.NoError(err)
			require.NoError(st.DB().QueryRow("SELECT COUNT(*) FROM messages").Scan(&count))
			assert.Equal(2, count)
			require.NoError(st.DB().QueryRow("SELECT COUNT(*) FROM mcp_event_log").Scan(&count))
			assert.Equal(want, count)
		})
	}
}
