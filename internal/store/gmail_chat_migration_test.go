package store_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestInitSchemaClassifiesLegacyGmailChatMessages(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := storetest.New(t).Store

	gmail, err := st.GetOrCreateSource("gmail", "archive@example.com")
	requirements.NoError(err)
	gmailLabels, err := st.EnsureLabelsBatch(gmail.ID, map[string]store.LabelInfo{
		"CHAT": {Name: "CHAT", Type: "system"},
	})
	requirements.NoError(err)
	chatConversation, err := st.EnsureConversation(gmail.ID, "chat-thread", "")
	requirements.NoError(err)
	chatMessage, err := st.UpsertMessage(&store.Message{
		SourceID: gmail.ID, ConversationID: chatConversation,
		SourceMessageID: "legacy-chat", MessageType: "email",
	})
	requirements.NoError(err)
	requirements.NoError(st.ReplaceMessageLabels(chatMessage, []int64{gmailLabels["CHAT"]}))
	emailConversation, err := st.EnsureConversation(gmail.ID, "email-thread", "")
	requirements.NoError(err)
	_, err = st.UpsertMessage(&store.Message{
		SourceID: gmail.ID, ConversationID: emailConversation,
		SourceMessageID: "ordinary-email", MessageType: "email",
	})
	requirements.NoError(err)

	imap, err := st.GetOrCreateSource("imap", "imap@example.com")
	requirements.NoError(err)
	imapLabels, err := st.EnsureLabelsBatch(imap.ID, map[string]store.LabelInfo{
		"CHAT": {Name: "CHAT", Type: "user"},
	})
	requirements.NoError(err)
	imapConversation, err := st.EnsureConversation(imap.ID, "imap-thread", "")
	requirements.NoError(err)
	imapMessage, err := st.UpsertMessage(&store.Message{
		SourceID: imap.ID, ConversationID: imapConversation,
		SourceMessageID: "imap-chat-folder", MessageType: "email",
	})
	requirements.NoError(err)
	requirements.NoError(st.ReplaceMessageLabels(imapMessage, []int64{imapLabels["CHAT"]}))

	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`
		DELETE FROM applied_migrations WHERE name = ?
	`), "gmail_chat_classification_v1")
	requirements.NoError(err)
	requirements.NoError(st.InitSchemaContext(t.Context()))

	for _, want := range []struct {
		sourceMessageID  string
		messageType      string
		conversationType string
	}{
		{sourceMessageID: "legacy-chat", messageType: "google_chat", conversationType: "chat"},
		{sourceMessageID: "ordinary-email", messageType: "email", conversationType: "email_thread"},
		{sourceMessageID: "imap-chat-folder", messageType: "email", conversationType: "email_thread"},
	} {
		var messageType, conversationType string
		err := st.DB().QueryRowContext(t.Context(), st.Rebind(`
			SELECT m.message_type, c.conversation_type
			FROM messages m
			JOIN conversations c ON c.id = m.conversation_id
			WHERE m.source_message_id = ?
		`), want.sourceMessageID).Scan(&messageType, &conversationType)
		requirements.NoError(err)
		assertions.Equal(want.messageType, messageType)
		assertions.Equal(want.conversationType, conversationType)
	}

	applied, err := st.IsMigrationApplied("gmail_chat_classification_v1")
	requirements.NoError(err)
	assertions.True(applied)
}
