package query

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestEntryKeyFactsMatchSQLProducedEntryKeys pins the equivalence between the
// Go entry-key builder (used by the file metadata endpoint) and the SQL
// expression the explore engine renders inside DuckDB, for both the
// per-message and the chat conversation classification.
func TestEntryKeyFactsMatchSQLProducedEntryKeys(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	b := NewTestDataBuilder(t)
	source := b.AddSourceWithType("archive@example.com", "gmail")
	when := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	email := b.AddMessage(MessageOpt{SourceID: source, Subject: "Email", SentAt: when, MessageType: "email"})
	b.AddAttachmentWithMIME(41, email, 10, "report.pdf", "application/pdf")
	chat := b.AddMessage(MessageOpt{
		SourceID: source, SentAt: when.Add(time.Hour),
		MessageType: "whatsapp", ConversationID: 900, ConversationType: "group_chat",
	})
	b.AddAttachmentWithMIME(42, chat, 10, "photo.png", "image/png")

	result, err := b.BuildEngine().SearchFiles(context.Background(), FileSearchRequest{
		Sort: SortSpec{Field: "occurred_at", Direction: "asc"}, Page: PageSpec{Limit: 25},
	})
	requirements.NoError(err)
	requirements.Len(result.Files, 2)

	emailFile, chatFile := result.Files[0], result.Files[1]
	emailFacts := EntryKeyFacts{
		SourceID: source, SourceMessageID: fmt.Sprintf("msg%d", email),
		MessageID: email, ConversationID: emailFile.ConversationID,
		MessageType: "email", ConversationType: "email",
	}
	assertions.Equal(emailFacts.EntryKey(), emailFile.EntryKey)
	assertions.Equal(fmt.Sprintf("source:%d:message:msg%d", source, email), emailFile.EntryKey)

	chatFacts := EntryKeyFacts{
		SourceID: source, SourceMessageID: fmt.Sprintf("msg%d", chat),
		MessageID: chat, ConversationID: 900,
		MessageType: "whatsapp", ConversationType: "group_chat",
	}
	assertions.Equal(chatFacts.EntryKey(), chatFile.EntryKey)
	assertions.Equal(fmt.Sprintf("source:%d:conversation:900", source), chatFile.EntryKey)
}

func TestEntryKeyFallsBackToInternalMessageID(t *testing.T) {
	facts := EntryKeyFacts{
		SourceID: 3, MessageID: 42, ConversationID: 7,
		MessageType: "email", ConversationType: "email_thread",
	}
	assert.Equal(t, "source:3:message:42", facts.EntryKey())
}

func TestIsChatEntryClassification(t *testing.T) {
	tests := []struct {
		name             string
		messageType      string
		conversationType string
		want             bool
	}{
		{name: "email stays a message entry", messageType: "email", conversationType: "email_thread", want: false},
		{name: "known text type regardless of conversation", messageType: "iMessage", conversationType: "email_thread", want: true},
		{name: "ambiguous type in chat conversation", messageType: "", conversationType: "group_chat", want: true},
		{name: "ambiguous type in email thread", messageType: "chat", conversationType: "email_thread", want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert.Equal(t, test.want, IsChatEntry(test.messageType, test.conversationType))
		})
	}
}
