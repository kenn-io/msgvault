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

	legacy, err := st.GetOrCreateSource("", "archive@example.com")
	requirements.NoError(err)
	legacyLabels, err := st.EnsureLabelsBatch(legacy.ID, map[string]store.LabelInfo{
		"CHAT": {Name: "CHAT", Type: "system"},
	})
	requirements.NoError(err)
	chatConversation, err := st.EnsureConversation(legacy.ID, "chat-thread", "")
	requirements.NoError(err)
	chatMessage, err := st.UpsertMessage(&store.Message{
		SourceID: legacy.ID, ConversationID: chatConversation,
		SourceMessageID: "legacy-chat", MessageType: "email",
	})
	requirements.NoError(err)
	requirements.NoError(st.ReplaceMessageLabels(chatMessage, []int64{legacyLabels["CHAT"]}))

	gmail, err := st.GetOrCreateSource("gmail", "explicit@example.com")
	requirements.NoError(err)
	gmailLabels, err := st.EnsureLabelsBatch(gmail.ID, map[string]store.LabelInfo{
		"CHAT": {Name: "CHAT", Type: "system"},
	})
	requirements.NoError(err)
	explicitChatConversation, err := st.EnsureConversation(gmail.ID, "explicit-chat-thread", "")
	requirements.NoError(err)
	explicitChatMessage, err := st.UpsertMessage(&store.Message{
		SourceID: gmail.ID, ConversationID: explicitChatConversation,
		SourceMessageID: "explicit-chat", MessageType: "email",
	})
	requirements.NoError(err)
	requirements.NoError(st.ReplaceMessageLabels(explicitChatMessage, []int64{gmailLabels["CHAT"]}))
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
		UPDATE applied_migrations SET version = 1 WHERE name = ?
	`), "gmail_chat_classification_v1")
	requirements.NoError(err)
	var revisionBefore int64
	requirements.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`
		SELECT COALESCE(MAX(revision), 0)
		FROM activity_projection_queue
		WHERE message_id IN (?, ?)
	`), chatMessage, explicitChatMessage).Scan(&revisionBefore))
	var initialMessageType, initialConversationType string
	requirements.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`
		SELECT m.message_type, c.conversation_type
		FROM messages m JOIN conversations c ON c.id = m.conversation_id
		WHERE m.source_message_id = ?
	`), "legacy-chat").Scan(&initialMessageType, &initialConversationType))
	assertions.Equal("email", initialMessageType)
	assertions.Equal("email_thread", initialConversationType)
	requirements.NoError(st.InitSchemaContext(t.Context()))

	for _, want := range []struct {
		sourceMessageID  string
		messageType      string
		conversationType string
	}{
		{sourceMessageID: "legacy-chat", messageType: "google_chat", conversationType: "chat"},
		{sourceMessageID: "explicit-chat", messageType: "google_chat", conversationType: "chat"},
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

	var version int
	requirements.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`
		SELECT version FROM applied_migrations WHERE name = ?
	`), "gmail_chat_classification_v1").Scan(&version))
	assertions.Equal(2, version)
	var legacySourceType, explicitSourceType string
	requirements.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`
		SELECT source_type FROM sources WHERE id = ?
	`), legacy.ID).Scan(&legacySourceType))
	requirements.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`
		SELECT source_type FROM sources WHERE id = ?
	`), gmail.ID).Scan(&explicitSourceType))
	assertions.Empty(legacySourceType)
	assertions.Equal("gmail", explicitSourceType)
	var revisionAfter int64
	requirements.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`
		SELECT COALESCE(MAX(revision), 0)
		FROM activity_projection_queue
		WHERE message_id IN (?, ?)
	`), chatMessage, explicitChatMessage).Scan(&revisionAfter))
	assertions.Greater(revisionAfter, revisionBefore)
	requirements.NoError(st.InitSchemaContext(t.Context()))
	var revisionFinal int64
	requirements.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`
		SELECT COALESCE(MAX(revision), 0)
		FROM activity_projection_queue
		WHERE message_id IN (?, ?)
	`), chatMessage, explicitChatMessage).Scan(&revisionFinal))
	assertions.Equal(revisionAfter, revisionFinal)

	applied, err := st.IsMigrationApplied("gmail_chat_classification_v1")
	requirements.NoError(err)
	assertions.True(applied)
}

func TestInitSchemaClassifiesLegacyGmailChatWithoutMigrationLedger(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := storetest.New(t).Store
	source, err := st.GetOrCreateSource("", "archive@example.com")
	requirements.NoError(err)
	labels, err := st.EnsureLabelsBatch(source.ID, map[string]store.LabelInfo{
		"CHAT": {Name: "CHAT", Type: "system"},
	})
	requirements.NoError(err)
	conversationID, err := st.EnsureConversation(source.ID, "chat-thread", "")
	requirements.NoError(err)
	messageID, err := st.UpsertMessage(&store.Message{
		SourceID: source.ID, ConversationID: conversationID,
		SourceMessageID: "legacy-chat-without-ledger", MessageType: "email",
	})
	requirements.NoError(err)
	requirements.NoError(st.ReplaceMessageLabels(messageID, []int64{labels["CHAT"]}))
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`
		DELETE FROM applied_migrations WHERE name = ?
	`), "gmail_chat_classification_v1")
	requirements.NoError(err)
	requirements.NoError(st.InitSchemaContext(t.Context()))

	var messageType, conversationType string
	requirements.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`
		SELECT m.message_type, c.conversation_type
		FROM messages m JOIN conversations c ON c.id = m.conversation_id
		WHERE m.source_message_id = ?
	`), "legacy-chat-without-ledger").Scan(&messageType, &conversationType))
	assertions.Equal("google_chat", messageType)
	assertions.Equal("chat", conversationType)
	var version int
	requirements.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`
		SELECT version FROM applied_migrations WHERE name = ?
	`), "gmail_chat_classification_v1").Scan(&version))
	assertions.Equal(2, version)
}
