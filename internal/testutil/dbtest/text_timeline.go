package dbtest

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/require"
)

// SeedTextTimeline inserts source 7, conversations 701 and 702, sender 11,
// and message 801 in conversation 701. Tests add their own changes to this base.
func SeedTextTimeline(tb testing.TB, db *sql.DB, messageType string) {
	tb.Helper()
	_, err := db.ExecContext(tb.Context(), `INSERT INTO sources (id, source_type, identifier) VALUES (7, ?, 'owner@example.com')`, messageType)
	require.NoError(tb, err)
	_, err = db.ExecContext(tb.Context(), `
		INSERT INTO participants (id, email_address, display_name) VALUES
			(11, 'sender@example.com', 'Original');
		INSERT INTO conversations (id, source_id, source_conversation_id, conversation_type, title) VALUES
			(701, 7, 'chat-701', 'direct_chat', 'Original'),
			(702, 7, 'chat-702', 'direct_chat', 'Other');
	`)
	require.NoError(tb, err)
	_, err = db.ExecContext(tb.Context(), `INSERT INTO messages (id, conversation_id, source_id, source_message_id, message_type, sent_at, snippet, sender_id, source_is_from_me)
		VALUES (801, 701, 7, 'message-801', ?, '2020-01-01 10:00:00', 'first', 11, FALSE)`, messageType)
	require.NoError(tb, err)
}
