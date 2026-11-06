package store_test

import (
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestPersistMessageAttachmentsAndReactions(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	f := storetest.New(t)
	pid := f.EnsureParticipant("reactor@example.com", "Reactor", "example.com")
	reactions := []store.ReactionRef{{ParticipantID: pid, Type: "emoji", Value: "👍", CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}}
	data := &store.MessagePersistData{
		Message:     &store.Message{SourceID: f.Source.ID, ConversationID: f.ConvID, SourceMessageID: "snapshot", MessageType: "signal", PreserveAttachmentStats: true},
		BodyText:    sql.NullString{String: "Original text", Valid: true},
		Attachments: []store.AttachmentWrite{{Filename: "photo.jpg", MIMEType: "image/jpeg", ContentHash: "photo-hash", StoragePath: "photo.jpg", Size: 11, SourcePartKey: "signal:attachment:photo-hash:0"}},
		Reactions:   &reactions,
	}
	id, err := f.Store.PersistMessageContext(t.Context(), data)
	require.NoError(err)
	assert.Equal(int64(1), data.ReactionsAdded)
	original, err := f.Store.GetMessage(id)
	require.NoError(err)
	require.Len(original.Attachments, 1)
	assert.Equal("photo.jpg", original.Attachments[0].Filename)
	assert.True(original.HasAttachments)
	var attachmentCount int
	require.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`SELECT attachment_count FROM messages WHERE id = ?`), id).Scan(&attachmentCount))
	assert.Equal(1, attachmentCount)
	readReactions := func() []string {
		t.Helper()
		rows, err := f.Store.DB().Query(f.Store.Rebind(`SELECT reaction_value FROM reactions WHERE message_id = ? ORDER BY reaction_value`), id)
		require.NoError(err)
		defer func() { require.NoError(rows.Close()) }()
		values := []string{}
		for rows.Next() {
			var value string
			require.NoError(rows.Scan(&value))
			values = append(values, value)
		}
		require.NoError(rows.Err())
		return values
	}
	assert.Equal([]string{"👍"}, readReactions())
	// A new timestamp must not count the same reaction as newly added.
	reactions[0].CreatedAt = time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	_, err = f.Store.PersistMessageContext(t.Context(), data)
	require.NoError(err)
	assert.Zero(data.ReactionsAdded)

	// A reaction failure must roll back text and attachment additions together.
	data.BodyText.String = "Failed replacement"
	data.Attachments = []store.AttachmentWrite{{Filename: "new.jpg", MIMEType: "image/jpeg", StoragePath: "new.jpg", SourcePartKey: "signal:attachment:new:0"}}
	reactions = []store.ReactionRef{{ParticipantID: -1, Type: "emoji", Value: "x"}}
	_, err = f.Store.PersistMessageContext(t.Context(), data)
	require.Error(err)
	body, err := f.Store.GetMessageBodyText(id)
	require.NoError(err)
	assert.Equal("Original text", body)
	unchanged, err := f.Store.GetMessage(id)
	require.NoError(err)
	assert.Equal(original.Attachments, unchanged.Attachments)
	assert.Equal([]string{"👍"}, readReactions())

	data.BodyText.String = "Updated text"
	data.Attachments = nil
	data.Reactions = nil
	_, err = f.Store.PersistMessageContext(t.Context(), data)
	require.NoError(err)
	assert.Equal([]string{"👍"}, readReactions())
	final, err := f.Store.GetMessage(id)
	require.NoError(err)
	assert.Equal(original.Attachments, final.Attachments)
	require.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`SELECT attachment_count FROM messages WHERE id = ?`), id).Scan(&attachmentCount))
	assert.Equal(1, attachmentCount)

	// A supplied empty snapshot clears reactions; a nil pointer leaves them.
	reactions = nil
	data.Reactions = &reactions
	_, err = f.Store.PersistMessageContext(t.Context(), data)
	require.NoError(err)
	assert.Empty(readReactions())
}
