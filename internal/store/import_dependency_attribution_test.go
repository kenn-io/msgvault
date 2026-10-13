package store_test

import (
	"database/sql"
	"testing"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep constructors available with local helpers.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep constructors available with local helpers.
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

// Missing the native source attribution entry must prevent this complete
// email dependency component from committing its ready import occurrences.
func TestImportDependencySnapshotsPreserveAccountAttribution(t *testing.T) {
	assert, require := Assert.New(t), Require.New(t)
	st := testutil.NewTestStore(t)
	_, err := st.ConfigureMCPEvents(t.Context(), store.MCPEventsConfig{Enabled: true, ImportNotifications: true, Principal: "owner:synthetic-import-attribution", Capabilities: []store.MCPEventCapability{{Family: "msgvault.import_message_archived", SourceType: "eml", Kinds: []string{"message"}}}})
	require.NoError(err)
	source, err := st.GetOrCreateSource("eml", "synthetic-import@example.net")
	require.NoError(err)
	attempt, err := st.StartSyncContext(t.Context(), source.ID, "full")
	require.NoError(err)
	scoped := st.ScopedToSync(source.ID, attempt)
	run, err := scoped.BeginNativeImportAttempt(t.Context(), source.ID, "eml", true, attempt, 0)
	require.NoError(err)
	view, err := scoped.WithImportRun(t.Context(), run.ID)
	require.NoError(err)
	snapshot := func(key, reply string) store.ParticipantMessageSnapshot {
		return store.ParticipantMessageSnapshot{SourceMessageID: key, Participants: []store.ParticipantPersistData{{EmailAddress: "synthetic-sender@example.net"}}, Build: func(ids []int64) *store.MessagePersistData {
			return &store.MessagePersistData{Message: &store.Message{SourceID: source.ID, SourceMessageID: key, MessageType: "email", SenderID: sql.NullInt64{Int64: ids[0], Valid: true}}, Conversation: &store.ConversationPersistData{SourceConversationID: "synthetic-dependency-thread", ConversationType: "email_thread"}, BodyText: sql.NullString{String: "Synthetic dependency body", Valid: true}, RawMIME: []byte("From: synthetic-sender@example.net\r\nTo: synthetic-import@example.net\r\n\r\nSynthetic dependency body"), ReplaceReply: true, ReplyToSourceMessageID: reply}
		}}
	}
	ids, err := view.PersistImportMessageSnapshotsContext(t.Context(), source.ID, []store.ParticipantMessageSnapshot{snapshot("first", "second"), snapshot("second", "first")})
	require.NoError(err)
	require.Len(ids, 2)
	for i, id := range ids {
		var parent int64
		var attributed bool
		require.NoError(st.DB().QueryRow(st.Rebind(`SELECT reply_to_message_id,account_path IS NOT NULL FROM messages WHERE id=?`), id).Scan(&parent, &attributed))
		assert.Equal(ids[1-i], parent, "the complete native reply component commits together")
		assert.True(attributed, "import persistence preserves native account derivation")
	}
	var occurrences int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log WHERE family='msgvault.import_message_archived'`).Scan(&occurrences))
	assert.Equal(2, occurrences)
}
