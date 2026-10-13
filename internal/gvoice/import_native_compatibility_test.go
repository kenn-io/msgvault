package gvoice

import (
	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep the package constructor available beside local helpers.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep the package constructor available beside local helpers.
	"go.kenn.io/msgvault/internal/testutil"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestImportGvoiceFailedEntryDoesNotCacheRolledBackIdentities(t *testing.T) {
	assert := Assert.New(t)

	require := Require.New(t)

	st := testutil.NewTestStore(t)
	configureSyntheticVoiceImports(t, st, true)
	directory := syntheticVoiceText(t)
	path := filepath.Join(directory, "Calls", "Synthetic Sender - Text - 2024-06-01T12_00_00Z.html")
	html, err := os.ReadFile(path)
	require.NoError(err)
	first := strings.ReplaceAll(string(html), "Synthetic native text", "Synthetic first")
	second := strings.ReplaceAll(strings.ReplaceAll(string(html), "Synthetic native text", "Synthetic second"), "12:00:00.000Z", "12:01:00.000Z")
	require.NoError(os.WriteFile(path, []byte(first+second), 0o600))
	if st.IsPostgreSQL() {
		_, err = st.DB().Exec(`CREATE FUNCTION synthetic_voice_first_abort() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.body_text='Synthetic first' THEN RAISE EXCEPTION 'synthetic first body failure'; END IF; RETURN NEW; END $$`)
		require.NoError(err)
		_, err = st.DB().Exec(`CREATE TRIGGER synthetic_voice_first_abort BEFORE INSERT ON message_bodies FOR EACH ROW EXECUTE FUNCTION synthetic_voice_first_abort()`)
	} else {
		_, err = st.DB().Exec(`CREATE TRIGGER synthetic_voice_first_abort BEFORE INSERT ON message_bodies WHEN NEW.body_text='Synthetic first' BEGIN SELECT RAISE(ABORT,'synthetic first body failure'); END`)
	}
	require.NoError(err)
	client, err := NewClient(directory, WithNotifyImports(true))
	require.NoError(err)
	source, err := st.GetOrCreateSource("google_voice", client.Identifier())
	require.NoError(err)
	summary, err := client.Import(t.Context(), st, source.ID)
	require.NoError(err)
	assert.Equal(1, summary.MessagesImported)
	assert.Equal(1, summary.Skipped)
	assert.Equal(2, summary.ParticipantsResolved)
	assert.Equal(1, summary.ConversationsImported)
	for _, table := range []struct {
		name string
		want int
	}{{"participants", 2}, {"messages", 1}, {"conversations", 1}, {"message_labels", 1}, {"message_recipients", 2}, {"import_message_decisions", 1}, {"mcp_event_log", 1}} {
		var got int
		require.NoError(st.DB().QueryRow("SELECT COUNT(*) FROM " + table.name).Scan(&got))
		assert.Equal(table.want, got, table.name)
	}
	query := `DROP TRIGGER synthetic_voice_first_abort`
	if st.IsPostgreSQL() {
		query += ` ON message_bodies`
	}
	_, err = st.DB().Exec(query)
	require.NoError(err)
	retry, err := client.Import(t.Context(), st, source.ID)
	require.NoError(err)
	assert.Equal(2, retry.MessagesImported)
	assert.Zero(retry.Skipped)
	var messages, events int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&messages))
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log`).Scan(&events))
	assert.Equal(2, messages)
	assert.Equal(2, events)
}

func TestImportGvoicePreservesAdditiveLabelsAndConversationMembers(t *testing.T) {
	assert := Assert.New(t)

	require := Require.New(t)

	st := testutil.NewTestStore(t)
	client, err := NewClient(syntheticVoiceText(t))
	require.NoError(err)
	source, err := st.GetOrCreateSource("google_voice", client.Identifier())
	require.NoError(err)
	first, err := client.Import(t.Context(), st, source.ID)
	require.NoError(err)
	require.Equal(1, first.MessagesImported)
	var message, conversation int64
	require.NoError(st.DB().QueryRow(`SELECT id,conversation_id FROM messages`).Scan(&message, &conversation))
	label, err := st.EnsureLabel(source.ID, "synthetic-retained", "Synthetic retained", "user")
	require.NoError(err)
	require.NoError(st.LinkMessageLabel(message, label))
	participant, err := st.EnsureParticipantByPhone("+15550000003", "Synthetic retained member", "google_voice")
	require.NoError(err)
	require.NoError(st.EnsureConversationParticipant(conversation, participant, "member"))
	retry, err := client.Import(t.Context(), st, source.ID)
	require.NoError(err)
	require.Equal(1, retry.MessagesImported)
	var labels, members int
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT COUNT(*) FROM message_labels WHERE message_id=?`), message).Scan(&labels))
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT COUNT(*) FROM conversation_participants WHERE conversation_id=?`), conversation).Scan(&members))
	assert.Equal(2, labels)
	assert.Equal(3, members)
}
