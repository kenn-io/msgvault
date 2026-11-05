package store

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func newSnapshotTestStore(t *testing.T) *Store {
	t.Helper()
	if IsPostgresURL(os.Getenv("MSGVAULT_TEST_DB")) {
		return newPGStoreInternal(t)
	}
	st, err := OpenForTest(filepath.Join(t.TempDir(), "snapshot.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, st.Close()) })
	require.NoError(t, st.InitSchema())
	return st
}

func commitSnapshotWriterOnQuery(t *testing.T, st *Store, writer *sql.Tx, boundary string) *bool {
	t.Helper()
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
	return &committed
}

// ExportSnapshotFixture commits an import between the conversation and message reads.
// The hook changes only timing; every statement still runs on the real database.
func ExportSnapshotFixture(t *testing.T, sourceType, boundary string) (*Store, int64, time.Time, *bool) {
	t.Helper()
	st := newSnapshotTestStore(t)
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
	committed := commitSnapshotWriterOnQuery(t, st, writer, boundary)
	return st, source.ID, start, committed
}
