package imazingcsv

import (
	"testing"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep the package constructor available beside local helpers.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep the package constructor available beside local helpers.
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestImportImazingNotifyingReplyWriteFailureDoesNotPublishChild(t *testing.T) {
	assert := Assert.New(t)

	require := Require.New(t)

	st := testutil.NewTestStore(t)
	_, err := st.ConfigureMCPEvents(t.Context(), store.MCPEventsConfig{Enabled: true, ImportNotifications: true, Principal: "owner:synthetic-reply", Capabilities: []store.MCPEventCapability{{Family: "msgvault.import_message_archived", SourceType: SourceType, Kinds: []string{"message"}}}})
	require.NoError(err)
	parent := []string{"Synthetic Conversation", "2024-06-01 12:00:00", "", "", "iMessage", "Incoming", "+15550000002", "Synthetic Sender", "", "", "", "Synthetic parent body", "", ""}
	child := []string{"Synthetic Conversation", "2024-06-01 12:01:00", "", "", "iMessage", "Outgoing", "", "", "", "Replying to:\nSynthetic Sender (+15550000002)\n2024-06-01 12:00:00\nSynthetic parent body", "", "Synthetic child reply", "", ""}
	export := newTestExport(t, [][]string{parent, child})
	if st.IsPostgreSQL() {
		_, err = st.DB().Exec(`CREATE FUNCTION synthetic_imazing_reply_abort() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.reply_to_message_id IS NOT NULL THEN RAISE EXCEPTION 'synthetic required reply failure'; END IF; RETURN NEW; END $$`)
		require.NoError(err)
		_, err = st.DB().Exec(`CREATE TRIGGER synthetic_imazing_reply_abort BEFORE UPDATE OF reply_to_message_id ON messages FOR EACH ROW EXECUTE FUNCTION synthetic_imazing_reply_abort()`)
	} else {
		_, err = st.DB().Exec(`CREATE TRIGGER synthetic_imazing_reply_abort BEFORE UPDATE OF reply_to_message_id ON messages WHEN NEW.reply_to_message_id IS NOT NULL BEGIN SELECT RAISE(ABORT,'synthetic required reply failure'); END`)
	}
	require.NoError(err)
	importer := NewImporter(st, Options{Owner: "+15550000001", Timezone: "UTC", NotifyImports: true})
	_, err = importer.ImportPath(t.Context(), export)
	require.ErrorContains(err, "synthetic required reply failure")
	for _, table := range []string{"messages", "message_bodies", "message_raw", "import_occurrence_allocations", "import_message_decisions", "mcp_event_log"} {
		var count int
		require.NoError(st.DB().QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count))
		assert.Equal(1, count, "only the independently ready parent can remain in "+table)
	}
	var children int
	require.NoError(st.DB().QueryRow("SELECT COUNT(*) FROM messages WHERE snippet='Synthetic child reply'").Scan(&children))
	assert.Zero(children)
	if st.IsPostgreSQL() {
		_, err = st.DB().Exec(`DROP TRIGGER synthetic_imazing_reply_abort ON messages`)
		require.NoError(err)
		_, err = st.DB().Exec(`DROP FUNCTION synthetic_imazing_reply_abort()`)
	} else {
		_, err = st.DB().Exec(`DROP TRIGGER synthetic_imazing_reply_abort`)
	}
	require.NoError(err)
	retry, err := importer.ImportPath(t.Context(), export)
	require.NoError(err)
	assert.Equal(1, retry.RepliesLinked)
	var childKey string
	require.NoError(st.DB().QueryRow("SELECT source_message_id FROM messages WHERE snippet='Synthetic child reply'").Scan(&childKey))
	var decisionRun string
	require.NoError(st.DB().QueryRow(st.Rebind("SELECT import_run_id FROM import_message_decisions WHERE source_message_id=?"), childKey).Scan(&decisionRun))
	assert.Equal(retry.ImportRunID, decisionRun, "failed required reply write must not consume the child's import occurrence")
	var events int
	require.NoError(st.DB().QueryRow("SELECT COUNT(*) FROM mcp_event_log").Scan(&events))
	assert.Equal(2, events)
}

// This native behavior already works before the readiness conversion. Keep it
// while making parent links part of publication rather than a later write.
func TestImportImazingNotifyingReplyResolvesParentLaterInCSV(t *testing.T) {
	assert := Assert.New(t)

	require := Require.New(t)

	st := testutil.NewTestStore(t)
	_, err := st.ConfigureMCPEvents(t.Context(), store.MCPEventsConfig{Enabled: true, ImportNotifications: true, Principal: "owner:synthetic-reply", Capabilities: []store.MCPEventCapability{{Family: "msgvault.import_message_archived", SourceType: SourceType, Kinds: []string{"message"}}}})
	require.NoError(err)
	parent := []string{"Synthetic Conversation", "2024-06-01 12:00:00", "", "", "iMessage", "Incoming", "+15550000002", "Synthetic Sender", "", "", "", "Synthetic parent body", "", ""}
	child := []string{"Synthetic Conversation", "2024-06-01 12:01:00", "", "", "iMessage", "Outgoing", "", "", "", "Replying to:\nSynthetic Sender (+15550000002)\n2024-06-01 12:00:00\nSynthetic parent body", "", "Synthetic child reply", "", ""}
	export := newTestExport(t, [][]string{child, parent})
	summary, err := NewImporter(st, Options{Owner: "+15550000001", Timezone: "UTC", NotifyImports: true}).ImportPath(t.Context(), export)
	require.NoError(err)
	assert.Equal(1, summary.RepliesLinked)
	assert.Zero(summary.RepliesUnresolved)
	var parentID, linkedID int64
	require.NoError(st.DB().QueryRow("SELECT id FROM messages WHERE snippet='Synthetic parent body'").Scan(&parentID))
	require.NoError(st.DB().QueryRow("SELECT reply_to_message_id FROM messages WHERE snippet='Synthetic child reply'").Scan(&linkedID))
	assert.Equal(parentID, linkedID)
	var events int
	require.NoError(st.DB().QueryRow("SELECT COUNT(*) FROM mcp_event_log").Scan(&events))
	assert.Equal(2, events)
}
