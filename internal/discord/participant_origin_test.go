package discord

import (
	"encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/conversationpolicy"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestDiscordRetainedPageUsesLiveOrigin(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)

	st := testutil.NewTestStore(t)
	st.DB().SetMaxOpenConns(1)
	source, err := st.GetOrCreateSource("discord", "200")
	requirements.NoError(err)
	room, err := st.EnsureConversationWithType(source.ID, "300", "channel", "Synthetic")
	requirements.NoError(err)
	requirements.NoError(st.SetConversationPolicyOverride(t.Context(), source.ID, "300", conversationpolicy.Full))
	full, err := st.ConversationFullEligibleContext(t.Context(), room)
	requirements.NoError(err)
	requirements.True(full)
	// This existing bare alias must not acquire a shared name from retained Cold work.
	bare, err := st.EnsureParticipantByIdentifier(discordUserIdentifier, "400", "")
	requirements.NoError(err)
	manual, err := st.EnsureParticipant("independent@example.test", "Independent profile", "example.test")
	requirements.NoError(err)
	requirements.NoError(st.SetParticipantIdentifier(manual, discordUserIdentifier, "402"))
	message := importerTestMessage("1001", "300", "Retained Discord text")
	message.Author = User{ID: "400", GlobalName: "Fetched author"}
	message.Mentions = []User{{ID: "401", GlobalName: "Fetched mention"}, {ID: "402", GlobalName: "Message-local mention"}}
	message.Reactions = []Reaction{{Emoji: Emoji{Name: "synthetic"}, Count: 3}}
	webhook := importerTestMessage("1002", "300", "Webhook text")
	webhook.WebhookID = "500"
	webhook.Author = User{ID: "override-user", GlobalName: "Webhook presentation"}
	application := importerTestMessage("1003", "300", "Application text")
	application.Author = User{}
	application.Application = []byte(`{"id":"600","name":"Application presentation"}`)
	automated := importerTestMessage("1004", "300", "Automated text")
	automated.Author = User{}
	automated.Application = []byte(`{"name":"Automated presentation"}`)
	page := []Message{message, webhook, application, automated}
	requirements.NoError(st.SetConversationPolicyOverride(t.Context(), source.ID, "300", conversationpolicy.Cold))
	imp := newTestImporter(st, newImporterFakeAPI())
	summary := &ImportSummary{processedMessageIDs: map[string]struct{}{}}
	requirements.NoError(imp.persistPage(t.Context(), source.ID, room, page, summary, nil))
	for _, identity := range []struct{ kind, value string }{
		{discordUserIdentifier, "400"}, {discordUserIdentifier, "401"},
		{discordWebhookIdentifier, "500"}, {discordApplicationIdentifier, "600"},
		{discordAutomatedIdentifier, "guild:200:application"},
	} {
		pid, _, err := st.ParticipantByIdentifier(identity.kind, identity.value)
		requirements.NoError(err)
		requirements.NotZero(pid)
		var name string
		requirements.NoError(st.DB().QueryRow(st.Rebind(`SELECT COALESCE(display_name,'') FROM participants WHERE id=?`), pid).Scan(&name))
		assertions.Empty(name, "Cold shared profile for %s:%s", identity.kind, identity.value)
		if identity.value == "400" {
			assertions.Equal(bare, pid)
		}
	}
	var manualName string
	requirements.NoError(st.DB().QueryRow(st.Rebind(`SELECT display_name FROM participants WHERE id=?`), manual).Scan(&manualName))
	assertions.Equal("Independent profile", manualName)
	for _, expected := range []struct{ id, text, label string }{
		{"1001", "Retained Discord text", "Fetched author"},
		{"1002", "Webhook text", "Webhook presentation"},
		{"1003", "Application text", "Application presentation"},
		{"1004", "Automated text", "Automated presentation"},
	} {
		archived, err := st.InspectMessage(expected.id)
		requirements.NoError(err)
		assertions.Equal(expected.text, archived.BodyText)
		assertions.True(archived.RawDataExists)
		var label string
		messageID := messageIDBySource(t, st, source.ID, expected.id)
		requirements.NoError(st.DB().QueryRow(st.Rebind(`SELECT display_name FROM message_recipients WHERE message_id=? AND recipient_type='from'`), messageID).Scan(&label))
		assertions.Equal(expected.label, label)
	}
	id := messageIDBySource(t, st, source.ID, "1001")
	var mention string
	requirements.NoError(st.DB().QueryRow(st.Rebind(`SELECT mr.display_name FROM message_recipients mr JOIN participant_identifiers pi ON pi.participant_id=mr.participant_id WHERE mr.message_id=? AND mr.recipient_type='mention' AND pi.identifier_type=? AND pi.identifier_value=?`), id, discordUserIdentifier, "401").Scan(&mention))
	assertions.Equal("Fetched mention", mention)
	var metadata string
	requirements.NoError(st.DB().QueryRow(st.Rebind(`SELECT metadata FROM messages WHERE id=?`), id).Scan(&metadata))
	var decoded messageMetadata
	requirements.NoError(json.Unmarshal([]byte(metadata), &decoded))
	requirements.Len(decoded.ReactionSummaries, 1)
	assertions.Equal(3, decoded.ReactionSummaries[0].Count)
	assertions.Equal("synthetic", decoded.ReactionSummaries[0].Emoji)
	// The same retained page may admit names only after actual Full promotion.
	requirements.NoError(st.SetConversationPolicyOverride(t.Context(), source.ID, "300", conversationpolicy.Full))
	requirements.NoError(imp.persistPage(t.Context(), source.ID, room, page, summary, nil))
	var admitted string
	requirements.NoError(st.DB().QueryRow(st.Rebind(`SELECT display_name FROM participants WHERE id=?`), bare).Scan(&admitted))
	assertions.Equal("Fetched author", admitted)
	assertions.EqualValues(4, summary.MessagesAdded)
}
