package store_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestPersistMessageAttachmentSnapshot(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("telegram", "user1")
	require.NoError(err)
	attachments := []store.AttachmentWrite{{Filename: "photo.jpg", ContentHash: "example-photo", SourcePartKey: "photo", Role: store.AttachmentRoleStandalone, RoleSource: store.AttachmentRoleSourceImporterSemantics}}
	data := &store.MessagePersistData{Message: &store.Message{SourceID: source.ID, SourceMessageID: "user2:1", MessageType: "telegram"}, Conversation: &store.ConversationPersistData{SourceConversationID: "user2", ConversationType: "direct_chat"}, MIMEAttachmentReplacement: &attachments, Edited: true}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = st.PersistMessageContext(ctx, data)
	require.ErrorIs(err, context.Canceled)
	var n int
	require.NoError(st.DB().QueryRow("SELECT count(*) FROM messages").Scan(&n))
	assert.Zero(n)
	require.NoError(st.DB().QueryRow("SELECT count(*) FROM attachments").Scan(&n))
	assert.Zero(n)
	id, err := st.PersistMessageContext(t.Context(), data)
	require.NoError(err)
	require.NoError(st.DB().QueryRow(st.Rebind("SELECT attachment_count FROM messages WHERE id = ?"), id).Scan(&n))
	assert.Equal(1, n)
	require.NoError(st.DB().QueryRow(st.Rebind("SELECT count(*) FROM attachments WHERE message_id = ?"), id).Scan(&n))
	assert.Equal(1, n)
	var edited bool
	require.NoError(st.DB().QueryRow(st.Rebind("SELECT is_edited FROM messages WHERE id = ?"), id).Scan(&edited))
	assert.True(edited)
	// A rejected attachment must roll back the message transaction too.
	data.Message.SourceMessageID = "user2:2"
	attachments[0].Role = "invalid"
	_, err = st.PersistMessageContext(t.Context(), data)
	require.Error(err)
	require.NoError(st.DB().QueryRow(st.Rebind("SELECT count(*) FROM messages WHERE source_message_id = ?"), "user2:2").Scan(&n))
	assert.Zero(n)
}
