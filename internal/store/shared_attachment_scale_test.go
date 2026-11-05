package store

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSharedAttachmentAtArchiveScale(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := openTestStore(t)
	source, err := st.GetOrCreateSource("gmail", "archive@example.com")
	require.NoError(err)
	conversationID, err := st.EnsureConversation(source.ID, "shared-logo", "Shared logo")
	require.NoError(err)
	id := seedMessage(t, st, source.ID, conversationID, "shared-1", "Ordinary message", "Ordinary snippet")
	require.NoError(st.UpsertMessageBody(id, sql.NullString{String: "Ordinary body", Valid: true}, sql.NullString{}))
	require.NoError(st.UpsertAttachment(id, "logo.png", "image/png", "", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", 42))

	const total = 32767
	_, err = st.DB().Exec(`WITH RECURSIVE copies(n) AS (
		VALUES(2) UNION ALL SELECT n + 1 FROM copies WHERE n < ?
	)
	INSERT INTO messages (source_id, conversation_id, source_message_id, message_type, subject, snippet)
	SELECT m.source_id, m.conversation_id, 'shared-' || copies.n, m.message_type, m.subject, m.snippet
	FROM copies CROSS JOIN messages m WHERE m.id = ?`, total, id)
	require.NoError(err)
	_, err = st.DB().Exec(`INSERT INTO message_bodies (message_id, body_text, body_html)
		SELECT m.id, b.body_text, b.body_html FROM messages m CROSS JOIN message_bodies b
		WHERE b.message_id = ? AND m.id <> ?`, id, id)
	require.NoError(err)
	_, err = st.DB().Exec(`INSERT INTO attachments (message_id, filename, mime_type, content_hash, size, storage_path)
		SELECT m.id, a.filename, a.mime_type, a.content_hash, a.size, a.storage_path FROM messages m CROSS JOIN attachments a
		WHERE a.message_id = ? AND m.id <> ?`, id, id)
	require.NoError(err)

	rows, err := st.DB().Query(`SELECT id FROM messages ORDER BY id`)
	require.NoError(err)
	t.Cleanup(func() { require.NoError(rows.Close()) })
	messages := make([]APIMessage, 0, total)
	for rows.Next() {
		var message APIMessage
		require.NoError(rows.Scan(&message.ID))
		messages = append(messages, message)
	}
	require.NoError(rows.Err())
	require.NoError(rows.Close())
	require.Len(messages, total)

	err = st.PopulateMessageBodiesContext(t.Context(), messages)
	if assert.NoError(err) {
		for _, message := range messages {
			assert.Equal("Ordinary body", message.BodyText, "message %d", message.ID)
			assert.Len(message.Attachments, 1, "message %d", message.ID)
		}
	}
}
