package query

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/dbtest"
)

func TestTextReadersHideRetiredPlaceholders(t *testing.T) {
	b := NewTestDataBuilder(t)
	sourceID := b.AddSourceWithType("@archive:example.org", "matrix")
	senderID := b.AddParticipant("member@example.org", "example.org", "Member")
	when := time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)
	for i, snippet := range []string{"readable", "source deleted", store.MatrixEncryptedPlaceholder, store.MatrixEncryptedPlaceholder} {
		opt := MessageOpt{SourceID: sourceID, SenderID: &senderID, ConversationID: 701, MessageType: "matrix", Snippet: snippet, SentAt: when.Add(time.Duration(i) * time.Hour)}
		if i == 1 {
			opt.DeletedFromSourceAt = &when
		}
		if i >= 2 {
			opt.InternalDeletedAt = &when
		}
		if i == 3 {
			opt.ConversationID = 702
		}
		b.AddMessage(opt)
	}
	tdb := dbtest.NewTestDB(t, "../store/schema.sql")
	_, err := tdb.DB.Exec(`
		INSERT INTO sources (id, source_type, identifier) VALUES (1, 'matrix', '@archive:example.org');
		INSERT INTO participants (id, email_address, display_name) VALUES (1, 'member@example.org', 'Member');
		INSERT INTO conversations (id, source_id, source_conversation_id, conversation_type) VALUES (701, 1, '!room:example.org', 'group_chat'), (702, 1, '!hidden:example.org', 'group_chat');
	`)
	require.NoError(t, err)
	for _, msg := range b.messages {
		_, err := tdb.DB.Exec(`INSERT INTO messages (id, source_id, conversation_id, source_message_id, sender_id, message_type, snippet, sent_at, deleted_at, deleted_from_source_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, msg.ID, msg.SourceID, msg.ConversationID, msg.SourceMessageID, msg.SenderID, msg.MessageType, msg.Snippet, msg.SentAt, msg.InternalDeletedAt, msg.DeletedFromSourceAt)
		require.NoError(t, err)
	}
	parquet := b.BuildEngine()
	for _, tc := range []struct {
		name   string
		engine TextEngine
	}{
		{"sqlite", NewSQLiteEngine(tdb.DB)},
		{"parquet", parquet},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, check := require.New(t), assert.New(t)
			messages, err := tc.engine.ListConversationMessages(t.Context(), 701, TextFilter{SortDirection: SortAsc})
			req.NoError(err)
			req.Len(messages, 2)
			check.Equal("readable", messages[0].Snippet)
			check.Equal("source deleted", messages[1].Snippet)
			check.NotNil(messages[1].DeletedAt)
			hidden, err := tc.engine.ListConversationMessages(t.Context(), 702, TextFilter{})
			req.NoError(err)
			check.Empty(hidden)
			conversations, err := tc.engine.ListConversations(t.Context(), TextFilter{})
			req.NoError(err)
			req.Len(conversations, 1)
			check.Equal(int64(701), conversations[0].ConversationID)
			check.Equal(int64(2), conversations[0].MessageCount)
			check.Equal("source deleted", conversations[0].LastPreview)
			check.Equal(when.Add(time.Hour), conversations[0].LastMessageAt.UTC())
			if sqlite, ok := tc.engine.(*SQLiteEngine); ok {
				page, revision, err := sqlite.ListConversationsSnapshot(t.Context(), TextFilter{})
				req.NoError(err)
				check.NotEmpty(revision)
				check.Equal(conversations, page)
			}
			groups, err := tc.engine.TextAggregate(t.Context(), TextViewSources, TextAggregateOptions{})
			req.NoError(err)
			req.Len(groups, 1)
			check.Equal(int64(2), groups[0].Count)
		})
	}
}
