package imazingcsv

import (
	"database/sql"
	"path/filepath"
	"testing"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep the package constructor available beside local helpers.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep the package constructor available beside local helpers.
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestImportImazingNotifyingReplyKeepsScopedArchivedMatchingAndClearsAmbiguity(t *testing.T) {
	assert := Assert.New(t)

	require := Require.New(t)

	st := testutil.NewTestStore(t)
	_, err := st.ConfigureMCPEvents(t.Context(), store.MCPEventsConfig{Enabled: true, ImportNotifications: true, Principal: "owner:synthetic-reply-scope", Capabilities: []store.MCPEventCapability{{Family: "msgvault.import_message_archived", SourceType: SourceType, Kinds: []string{"message"}}}})
	require.NoError(err)
	parent := []string{"Synthetic Conversation", "2024-06-01 12:00:00", "", "", "iMessage", "Incoming", "+15550000002", "Synthetic Sender", "", "", "", "Synthetic parent body", "", ""}
	child := []string{"Synthetic Conversation", "2024-06-01 12:01:00", "", "", "iMessage", "Outgoing", "", "", "", "Replying to:\nSynthetic Sender (+15550000002)\n2024-06-01 12:00:00\nSynthetic parent body", "", "Synthetic child reply", "", ""}
	unrelated := []string{"Synthetic Other Conversation", "2024-06-01 11:00:00", "", "", "iMessage", "Incoming", "+15550000003", "Synthetic Other Sender", "", "", "", "Synthetic unrelated body", "", ""}
	export := newTestExport(t, [][]string{child, parent, unrelated})
	importer := NewImporter(st, Options{Owner: "+15550000001", Timezone: "UTC", NotifyImports: true})
	first, err := importer.ImportPath(t.Context(), export)
	require.NoError(err)
	assert.Equal(1, first.RepliesLinked)
	var parentID, childID, unrelatedID int64
	require.NoError(st.DB().QueryRow("SELECT id FROM messages WHERE snippet='Synthetic parent body'").Scan(&parentID))
	require.NoError(st.DB().QueryRow("SELECT id FROM messages WHERE snippet='Synthetic child reply'").Scan(&childID))
	require.NoError(st.DB().QueryRow("SELECT id FROM messages WHERE snippet='Synthetic unrelated body'").Scan(&unrelatedID))
	_, err = st.DB().Exec(st.Rebind("UPDATE message_raw SET raw_data=?,compression=NULL WHERE message_id=?"), []byte("not JSON"), unrelatedID)
	require.NoError(err)
	writeTestCSV(t, filepath.Join(export, "csv", "messages.csv"), [][]string{child})
	contracted, err := importer.ImportPath(t.Context(), export)
	require.NoError(err, "unrelated archived raw data is outside the requested reply conversation")
	assert.Equal(1, contracted.RepliesLinked)
	assert.Zero(contracted.RepliesUnresolved)
	var linked sql.NullInt64
	require.NoError(st.DB().QueryRow(st.Rebind("SELECT reply_to_message_id FROM messages WHERE id=?"), childID).Scan(&linked))
	assert.Equal(sql.NullInt64{Int64: parentID, Valid: true}, linked)
	writeTestCSV(t, filepath.Join(export, "csv", "messages.csv"), [][]string{child, parent, parent})
	ambiguous, err := importer.ImportPath(t.Context(), export)
	require.NoError(err)
	assert.Zero(ambiguous.RepliesLinked)
	assert.Equal(1, ambiguous.RepliesUnresolved)
	require.NoError(st.DB().QueryRow(st.Rebind("SELECT reply_to_message_id FROM messages WHERE id=?"), childID).Scan(&linked))
	assert.False(linked.Valid)
	child[9] = ""
	writeTestCSV(t, filepath.Join(export, "csv", "messages.csv"), [][]string{child})
	cleared, err := importer.ImportPath(t.Context(), export)
	require.NoError(err)
	assert.Zero(cleared.RepliesLinked)
	assert.Zero(cleared.RepliesUnresolved)
	require.NoError(st.DB().QueryRow(st.Rebind("SELECT reply_to_message_id FROM messages WHERE id=?"), childID).Scan(&linked))
	assert.False(linked.Valid)
	var count int
	require.NoError(st.DB().QueryRow("SELECT COUNT(*) FROM mcp_event_log").Scan(&count))
	assert.Equal(4, count, "only the additional native parent occurrence is a new import")
}
