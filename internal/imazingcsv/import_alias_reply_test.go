package imazingcsv

import (
	"testing"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep the package constructor available beside local helpers.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep the package constructor available beside local helpers.
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

// The final CSV row wins when native aliases converge. A reply failure must
// preserve the entire prior canonical snapshot, including earlier alias writes.
func TestImportImazingNotifyingAliasReplyFailurePreservesCanonicalSnapshot(t *testing.T) {
	assert := Assert.New(t)

	require := Require.New(t)

	st := testutil.NewTestStore(t)
	parent := []string{"Synthetic Conversation", "2024-06-01 11:00:00", "", "", "iMessage", "Incoming", "+15550000002", "Synthetic Sender", "", "", "", "Synthetic parent body", "", ""}
	later := []string{"Synthetic Conversation", "2024-06-01 12:02:00", "", "", "iMessage", "Outgoing", "", "", "", "", "", "Synthetic later old", "", ""}
	earlier := []string{"Synthetic Conversation", "2024-06-01 12:01:00", "", "", "iMessage", "Outgoing", "", "", "", "Replying to:\nSynthetic Sender (+15550000002)\n2024-06-01 11:00:00\nSynthetic parent body", "", "Synthetic earlier old", "", ""}
	opts := Options{Owner: "+15550000001", Timezone: "UTC"}
	_, err := NewImporter(st, opts).ImportPath(t.Context(), newTestExport(t, [][]string{parent, later, earlier}))
	require.NoError(err)
	var source, conversation, canonicalID int64
	var oldKey, canonicalKey string
	require.NoError(st.DB().QueryRow("SELECT source_id,conversation_id,source_message_id FROM messages WHERE snippet='Synthetic later old'").Scan(&source, &conversation, &oldKey))
	require.NoError(st.DB().QueryRow("SELECT id,source_message_id FROM messages WHERE snippet='Synthetic earlier old'").Scan(&canonicalID, &canonicalKey))
	require.NoError(st.MigrateSourceMessageID(source, conversation, oldKey, canonicalKey))
	_, err = st.ConfigureMCPEvents(t.Context(), store.MCPEventsConfig{Enabled: true, ImportNotifications: true, Principal: "owner:synthetic-alias", Capabilities: []store.MCPEventCapability{{Family: "msgvault.import_message_archived", SourceType: SourceType, Kinds: []string{"message"}}}})
	require.NoError(err)
	if st.IsPostgreSQL() {
		_, err = st.DB().Exec(`CREATE FUNCTION synthetic_alias_reply_abort() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.reply_to_message_id IS NOT NULL THEN RAISE EXCEPTION 'synthetic alias reply failure'; END IF; RETURN NEW; END $$`)
		require.NoError(err)
		_, err = st.DB().Exec(`CREATE TRIGGER synthetic_alias_reply_abort BEFORE UPDATE OF reply_to_message_id ON messages FOR EACH ROW EXECUTE FUNCTION synthetic_alias_reply_abort()`)
	} else {
		_, err = st.DB().Exec(`CREATE TRIGGER synthetic_alias_reply_abort BEFORE UPDATE OF reply_to_message_id ON messages WHEN NEW.reply_to_message_id IS NOT NULL BEGIN SELECT RAISE(ABORT,'synthetic alias reply failure'); END`)
	}
	require.NoError(err)
	later[11] = "Synthetic later new"
	earlier[11] = "Synthetic earlier new"
	export := newTestExport(t, [][]string{parent, later, earlier})
	opts.NotifyImports = true
	_, err = NewImporter(st, opts).ImportPath(t.Context(), export)
	require.ErrorContains(err, "synthetic alias reply failure")
	var body string
	require.NoError(st.DB().QueryRow(st.Rebind("SELECT body_text FROM message_bodies WHERE message_id=?"), canonicalID).Scan(&body))
	assert.Equal("Synthetic earlier old", body)
	if st.IsPostgreSQL() {
		_, err = st.DB().Exec(`DROP TRIGGER synthetic_alias_reply_abort ON messages`)
		require.NoError(err)
		_, err = st.DB().Exec(`DROP FUNCTION synthetic_alias_reply_abort()`)
	} else {
		_, err = st.DB().Exec(`DROP TRIGGER synthetic_alias_reply_abort`)
	}
	require.NoError(err)
	_, err = NewImporter(st, opts).ImportPath(t.Context(), export)
	require.NoError(err)
	require.NoError(st.DB().QueryRow(st.Rebind("SELECT body_text FROM message_bodies WHERE message_id=?"), canonicalID).Scan(&body))
	assert.Equal("Synthetic earlier new", body, "keep CSV overwrite order rather than chronological order")
	var parentID, linked int64
	require.NoError(st.DB().QueryRow("SELECT id FROM messages WHERE snippet='Synthetic parent body'").Scan(&parentID))
	require.NoError(st.DB().QueryRow(st.Rebind("SELECT reply_to_message_id FROM messages WHERE id=?"), canonicalID).Scan(&linked))
	assert.Equal(parentID, linked)
	var count int
	require.NoError(st.DB().QueryRow("SELECT COUNT(*) FROM import_occurrence_allocations").Scan(&count))
	assert.Equal(3, count)
	require.NoError(st.DB().QueryRow("SELECT COUNT(*) FROM mcp_event_log").Scan(&count))
	assert.Zero(count, "retained aliases remain silent")
	_, _, err = st.DeleteAllDedupedContext(t.Context())
	require.NoError(err)
	_, err = NewImporter(st, opts).ImportPath(t.Context(), export)
	require.NoError(err)
	require.NoError(st.DB().QueryRow("SELECT COUNT(*) FROM messages").Scan(&count))
	assert.Equal(2, count)
	require.NoError(st.DB().QueryRow("SELECT COUNT(*) FROM mcp_event_log").Scan(&count))
	assert.Zero(count, "physical deletion cannot replay merged aliases")
}
