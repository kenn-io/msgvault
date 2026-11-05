package store

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type snapshotExportSink struct {
	conversations []MessageExportConversation
	messages      []MessageExportMessage
}

func (s *snapshotExportSink) Source(MessageExportSource) error { return nil }
func (s *snapshotExportSink) Conversation(c MessageExportConversation) error {
	s.conversations = append(s.conversations, c)
	return nil
}
func (s *snapshotExportSink) Message(m MessageExportMessage) error {
	s.messages = append(s.messages, m)
	return nil
}

func TestExportMessagesConcurrentWriteSnapshot(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st, _, start, committed := exportSnapshotFixture(t, "gmail", "mr_from")
	filter := MessageExportFilter{Start: start, End: start.Add(time.Hour)}
	sink := &snapshotExportSink{}
	counts, err := st.ExportMessages(t.Context(), filter, sink)
	requirements.NoError(err)
	requirements.True(*committed, "the writer must commit between export phases")
	assertions.Equal(MessageExportCounts{Sources: 1, Conversations: 1, Messages: 1}, counts)
	requirements.Len(sink.conversations, 1)
	requirements.Len(sink.messages, 1)
	assertions.Equal("original", sink.conversations[0].ID)
	for _, message := range sink.messages {
		assertions.Equal(sink.conversations[0].ID, message.ConversationID)
	}
	fresh := &snapshotExportSink{}
	counts, err = st.ExportMessages(t.Context(), filter, fresh)
	requirements.NoError(err)
	assertions.Equal(MessageExportCounts{Sources: 1, Conversations: 2, Messages: 2}, counts)
	assertions.Len(fresh.conversations, 2)
	assertions.Len(fresh.messages, 2)
}

func TestDiscordExportConcurrentWriteSnapshot(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st, sourceID, start, committed := exportSnapshotFixture(t, "discord", "FROM messages m")
	containers, err := st.DiscordExport(t.Context(), sourceID, start, start.Add(time.Hour))
	requirements.NoError(err)
	requirements.True(*committed, "the writer must commit between export phases")
	requirements.Len(containers, 1)
	assertions.Equal("original", containers[0].ID)
	requirements.Len(containers[0].Messages, 1)
	assertions.Equal("original-message", containers[0].Messages[0].ID)
	fresh, err := st.DiscordExport(t.Context(), sourceID, start, start.Add(time.Hour))
	requirements.NoError(err)
	requirements.Len(fresh, 2)
	assertions.Equal("new-thread", fresh[0].ID)
	assertions.Equal("thread", fresh[0].Kind)
	requirements.Len(fresh[0].Messages, 1)
	assertions.Equal("new-message", fresh[0].Messages[0].ID)
}

// Commit an import between the conversation read and the message read.
// The hook changes only timing; every statement still runs on the real database.
func exportSnapshotFixture(t *testing.T, sourceType, boundary string) (*Store, int64, time.Time, *bool) {
	t.Helper()
	var st *Store
	if IsPostgresURL(os.Getenv("MSGVAULT_TEST_DB")) {
		st = newPGStoreInternal(t)
	} else {
		var err error
		st, err = OpenForTest(filepath.Join(t.TempDir(), "export.db"))
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, st.Close()) })
		require.NoError(t, st.InitSchema())
	}
	source, err := st.GetOrCreateSource(sourceType, "snapshot@example.com")
	require.NoError(t, err)
	conversationID, err := st.EnsureConversation(source.ID, "original", "Original")
	require.NoError(t, err)
	start := time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC)
	_, err = st.UpsertMessage(&Message{
		SourceID: source.ID, ConversationID: conversationID,
		SourceMessageID: "original-message", MessageType: sourceType,
		SentAt: sql.NullTime{Time: start, Valid: true},
	})
	require.NoError(t, err)
	writer, err := st.DB().BeginTx(t.Context(), nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = writer.Rollback() })
	var threadID int64
	err = writer.QueryRow(st.Rebind(`INSERT INTO conversations
		(source_id, source_conversation_id, conversation_type, title, metadata)
		VALUES (?, 'new-thread', 'channel', 'New thread', '{"discord_channel_type":11}')
		RETURNING id`), source.ID).Scan(&threadID)
	require.NoError(t, err)
	_, err = writer.Exec(st.Rebind(`INSERT INTO messages
		(source_id, conversation_id, source_message_id, message_type, sent_at)
		VALUES (?, ?, 'new-message', ?, ?)`), source.ID, threadID, sourceType, start)
	require.NoError(t, err)
	rebind := st.db.rebind
	committed := false
	st.db.rebind = func(query string) string {
		if !committed && strings.Contains(query, boundary) {
			committed = true
			require.NoError(t, writer.Commit())
		}
		return rebind(query)
	}
	t.Cleanup(func() { st.db.rebind = rebind })
	return st, source.ID, start, &committed
}
