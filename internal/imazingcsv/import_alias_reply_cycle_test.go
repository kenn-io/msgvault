package imazingcsv

import (
	"testing"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep the package constructor available beside local helpers.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep the package constructor available beside local helpers.
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

// Native merges can make earlier logical parents share a later physical row.
// Preserve that supported archive graph on notifying reimport and restoration.
func TestImportImazingNotifyingRetainedAliasReplyCycleRestoresWithoutReplay(t *testing.T) {
	assert := Assert.New(t)

	require := Require.New(t)

	st := testutil.NewTestStore(t)
	aEarly := []string{"Synthetic Conversation", "2024-06-01 11:00:00", "", "", "iMessage", "Incoming", "+15550000002", "Synthetic Sender A", "", "", "", "Synthetic A early", "", ""}
	aLate := []string{"Synthetic Conversation", "2024-06-01 13:00:00", "", "", "iMessage", "Incoming", "+15550000002", "Synthetic Sender A", "", "Replying to:\nSynthetic Sender B (+15550000003)\n2024-06-01 12:00:00\nSynthetic B early", "", "Synthetic A late", "", ""}
	bEarly := []string{"Synthetic Conversation", "2024-06-01 12:00:00", "", "", "iMessage", "Incoming", "+15550000003", "Synthetic Sender B", "", "", "", "Synthetic B early", "", ""}
	bLate := []string{"Synthetic Conversation", "2024-06-01 14:00:00", "", "", "iMessage", "Incoming", "+15550000003", "Synthetic Sender B", "", "Replying to:\nSynthetic Sender A (+15550000002)\n2024-06-01 13:00:00\nSynthetic A late", "", "Synthetic B late", "", ""}
	export := newTestExport(t, [][]string{aEarly, aLate, bEarly, bLate})
	opts := Options{Owner: "+15550000001", Timezone: "UTC"}
	_, err := NewImporter(st, opts).ImportPath(t.Context(), export)
	require.NoError(err)
	for _, pair := range [][2]string{{"Synthetic A early", "Synthetic A late"}, {"Synthetic B early", "Synthetic B late"}} {
		var source, conversation int64
		var from, to string
		require.NoError(st.DB().QueryRow(st.Rebind("SELECT source_id,conversation_id,source_message_id FROM messages WHERE snippet=?"), pair[0]).Scan(&source, &conversation, &from))
		require.NoError(st.DB().QueryRow(st.Rebind("SELECT source_message_id FROM messages WHERE snippet=?"), pair[1]).Scan(&to))
		require.NoError(st.MigrateSourceMessageID(source, conversation, from, to))
	}
	// The legacy native resolver accepts this graph. It does not guess away a
	// recorded alias because the surviving physical parent's date is later.
	_, err = NewImporter(st, opts).ImportPath(t.Context(), export)
	require.NoError(err)
	_, err = st.ConfigureMCPEvents(t.Context(), store.MCPEventsConfig{Enabled: true, ImportNotifications: true, Principal: "owner:synthetic-cycle", Capabilities: []store.MCPEventCapability{{Family: "msgvault.import_message_archived", SourceType: SourceType, Kinds: []string{"message"}}}})
	require.NoError(err)
	opts.NotifyImports = true
	for _, deleted := range []bool{false, true} {
		if deleted {
			_, _, err = st.DeleteAllDedupedContext(t.Context())
			require.NoError(err)
		}
		summary, err := NewImporter(st, opts).ImportPath(t.Context(), export)
		require.NoError(err)
		assert.Equal(2, summary.RepliesLinked)
		assert.Zero(summary.RepliesUnresolved)
		var a, b, aParent, bParent int64
		require.NoError(st.DB().QueryRow("SELECT id,reply_to_message_id FROM messages WHERE snippet='Synthetic A late'").Scan(&a, &aParent))
		require.NoError(st.DB().QueryRow("SELECT id,reply_to_message_id FROM messages WHERE snippet='Synthetic B late'").Scan(&b, &bParent))
		assert.Equal(b, aParent)
		assert.Equal(a, bParent)
		var events, allocations int
		require.NoError(st.DB().QueryRow("SELECT COUNT(*) FROM mcp_event_log").Scan(&events))
		assert.Zero(events)
		require.NoError(st.DB().QueryRow("SELECT COUNT(*) FROM import_occurrence_allocations").Scan(&allocations))
		assert.Equal(4, allocations)
	}
}
