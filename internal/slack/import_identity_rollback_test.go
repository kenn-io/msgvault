package slack

import (
	"database/sql"
	"testing"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep the package constructor available beside local helpers.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep the package constructor available beside local helpers.
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestImportSlackdumpRequiredFailureRetainsNativeWeakIdentityAndHistory(t *testing.T) {
	assert := Assert.New(t)

	require := Require.New(t)

	path := syntheticImportSlackdump(t)
	st := testutil.NewTestStore(t)
	weakID, err := st.EnsureParticipantByIdentifier("slack", "T_EXAMPLE:U_PEER", "Synthetic Weak Peer")
	require.NoError(err)
	source, err := st.GetOrCreateSource("eml", "history@example.net")
	require.NoError(err)
	conversation, err := st.EnsureConversationWithType(source.ID, "synthetic-history", "email", "Synthetic prior history")
	require.NoError(err)
	priorID, err := st.PersistMessageContext(t.Context(), &store.MessagePersistData{Message: &store.Message{SourceID: source.ID, SourceMessageID: "synthetic-prior-history", ConversationID: conversation, MessageType: "email", SenderID: sql.NullInt64{Int64: weakID, Valid: true}}, BodyText: sql.NullString{String: "Synthetic prior history", Valid: true}})
	require.NoError(err)
	_, err = st.ConfigureMCPEvents(t.Context(), store.MCPEventsConfig{Enabled: true, Principal: "owner:synthetic-slackdump", ImportNotifications: true, Capabilities: []store.MCPEventCapability{{Family: "msgvault.import_message_archived", SourceType: "slackdump", Kinds: []string{"message"}}}})
	require.NoError(err)
	if st.IsPostgreSQL() {
		_, err = st.DB().Exec(`CREATE FUNCTION synthetic_slackdump_identity_abort() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'synthetic required raw failure'; END $$`)
		require.NoError(err)
		_, err = st.DB().Exec(`CREATE TRIGGER synthetic_slackdump_identity_abort BEFORE INSERT ON message_raw FOR EACH ROW EXECUTE FUNCTION synthetic_slackdump_identity_abort()`)
	} else {
		_, err = st.DB().Exec(`CREATE TRIGGER synthetic_slackdump_identity_abort BEFORE INSERT ON message_raw BEGIN SELECT RAISE(ABORT,'synthetic required raw failure'); END`)
	}
	require.NoError(err)
	_, err = ImportSlackdump(t.Context(), st, path, SlackdumpImportOptions{Me: "owner@example.net", NotifyImports: true})
	require.Error(err)
	var senderID, aliasID int64
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT sender_id FROM messages WHERE id=?`), priorID).Scan(&senderID))
	assert.Equal(weakID, senderID, "an aborted import must not repoint prior history")
	require.NoError(st.DB().QueryRow(`SELECT participant_id FROM participant_identifiers WHERE identifier_type='slack' AND identifier_value='T_EXAMPLE:U_PEER'`).Scan(&aliasID))
	assert.Equal(weakID, aliasID)
	var participants, messages, events int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM participants`).Scan(&participants))
	assert.Equal(1, participants)
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&messages))
	assert.Equal(1, messages)
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log`).Scan(&events))
	assert.Zero(events)
	if st.IsPostgreSQL() {
		_, err = st.DB().Exec(`DROP TRIGGER synthetic_slackdump_identity_abort ON message_raw`)
	} else {
		_, err = st.DB().Exec(`DROP TRIGGER synthetic_slackdump_identity_abort`)
	}
	require.NoError(err)
	_, err = ImportSlackdump(t.Context(), st, path, SlackdumpImportOptions{Me: "owner@example.net", NotifyImports: true})
	require.NoError(err)
	var richID int64
	require.NoError(st.DB().QueryRow(`SELECT id FROM participants WHERE email_address='peer@example.net'`).Scan(&richID))
	assert.NotEqual(weakID, richID)
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT sender_id FROM messages WHERE id=?`), priorID).Scan(&senderID))
	assert.Equal(richID, senderID, "a successful native import still merges weak identity history")
	require.NoError(st.DB().QueryRow(`SELECT participant_id FROM participant_identifiers WHERE identifier_type='slack' AND identifier_value='T_EXAMPLE:U_PEER'`).Scan(&aliasID))
	assert.Equal(richID, aliasID)
}
