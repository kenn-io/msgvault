package store_test

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestPersistMessageAttachmentsAndReactions(t *testing.T) {
	f := storetest.New(t)
	pid := f.EnsureParticipant("reactor@example.com", "Reactor", "example.com")
	reactions := []store.ReactionRef{{ParticipantID: pid, Type: "emoji", Value: "👍"}}
	data := &store.MessagePersistData{
		Message:     &store.Message{SourceID: f.Source.ID, ConversationID: f.ConvID, SourceMessageID: "snapshot", MessageType: "signal", PreserveAttachmentStats: true},
		BodyText:    sql.NullString{String: "Original text", Valid: true},
		Attachments: []store.AttachmentWrite{{Filename: "photo.jpg", MIMEType: "image/jpeg", ContentHash: "photo-hash", StoragePath: "photo.jpg", Size: 11, SourcePartKey: "signal:attachment:photo-hash:0"}},
		Reactions:   &reactions,
	}
	id, err := f.Store.PersistMessageContext(t.Context(), data)
	require.NoError(t, err)
	assert.Equal(t, int64(1), data.ReactionsAdded)
	original, err := f.Store.GetMessage(id)
	require.NoError(t, err)
	require.Len(t, original.Attachments, 1)
	attachmentID := original.Attachments[0].ID
	assert.Equal(t, "photo.jpg", original.Attachments[0].Filename)
	assert.True(t, original.HasAttachments)
	var attachmentCount int
	require.NoError(t, f.Store.DB().QueryRow(f.Store.Rebind(`SELECT attachment_count FROM messages WHERE id = ?`), id).Scan(&attachmentCount))
	assert.Equal(t, 1, attachmentCount)
	readReactions := func() []string {
		t.Helper()
		rows, err := f.Store.DB().Query(f.Store.Rebind(`SELECT reaction_value FROM reactions WHERE message_id = ? ORDER BY reaction_value`), id)
		require.NoError(t, err)
		defer func() { require.NoError(t, rows.Close()) }()
		values := []string{}
		for rows.Next() {
			var value string
			require.NoError(t, rows.Scan(&value))
			values = append(values, value)
		}
		require.NoError(t, rows.Err())
		return values
	}
	assert.Equal(t, []string{"👍"}, readReactions())
	_, err = f.Store.PersistMessageContext(t.Context(), data)
	require.NoError(t, err)
	assert.Zero(t, data.ReactionsAdded)
	again, err := f.Store.GetMessage(id)
	require.NoError(t, err)
	require.Len(t, again.Attachments, 1)
	assert.Equal(t, attachmentID, again.Attachments[0].ID)

	// A reaction failure must roll back text and attachment additions together.
	data.BodyText.String = "Failed replacement"
	data.Attachments = []store.AttachmentWrite{{Filename: "new.jpg", MIMEType: "image/jpeg", StoragePath: "new.jpg", SourcePartKey: "signal:attachment:new:0"}}
	reactions = []store.ReactionRef{{ParticipantID: -1, Type: "emoji", Value: "x"}}
	_, err = f.Store.PersistMessageContext(t.Context(), data)
	require.Error(t, err)
	body, err := f.Store.GetMessageBodyText(id)
	require.NoError(t, err)
	assert.Equal(t, "Original text", body)
	unchanged, err := f.Store.GetMessage(id)
	require.NoError(t, err)
	assert.Equal(t, original.Attachments, unchanged.Attachments)
	assert.Equal(t, []string{"👍"}, readReactions())

	data.BodyText.String = "Updated text"
	data.Attachments = nil
	reactions = []store.ReactionRef{{ParticipantID: pid, Type: "emoji", Value: "❤"}, {ParticipantID: pid, Type: "emoji", Value: "❤"}}
	_, err = f.Store.PersistMessageContext(t.Context(), data)
	require.NoError(t, err)
	assert.Equal(t, int64(1), data.ReactionsAdded)
	assert.Equal(t, []string{"❤"}, readReactions())
	data.Reactions = nil
	_, err = f.Store.PersistMessageContext(t.Context(), data)
	require.NoError(t, err)
	assert.Equal(t, []string{"❤"}, readReactions())
	reactions = []store.ReactionRef{}
	data.Reactions = &reactions
	_, err = f.Store.PersistMessageContext(t.Context(), data)
	require.NoError(t, err)
	assert.Zero(t, data.ReactionsAdded)
	assert.Empty(t, readReactions())
	final, err := f.Store.GetMessage(id)
	require.NoError(t, err)
	assert.Equal(t, original.Attachments, final.Attachments)
	require.NoError(t, f.Store.DB().QueryRow(f.Store.Rebind(`SELECT attachment_count FROM messages WHERE id = ?`), id).Scan(&attachmentCount))
	assert.Equal(t, 1, attachmentCount)
}
