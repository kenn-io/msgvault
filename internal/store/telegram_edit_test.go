package store_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestTelegramEditUpdatesTextAndPreservesMetadata(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	ctx := context.Background()
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("telegram", "user999")
	require.NoError(err)
	conv, err := st.EnsureConversationWithType(source.ID, "-123", "group_chat", "Synthetic")
	require.NoError(err)
	messageID, err := st.PersistMessage(&store.MessagePersistData{Message: &store.Message{SourceID: source.ID, ConversationID: conv, SourceMessageID: "-123:7", MessageType: "telegram", SentAt: sql.NullTime{Time: time.Unix(100, 0), Valid: true}}, BodyText: sql.NullString{String: "old synthetic", Valid: true}, Metadata: &sql.NullString{String: `{"attachment_part_keys":["synthetic-key"],"referenced_message_id":"-123:6"}`, Valid: true}})
	require.NoError(err)
	changed, err := st.ApplyTelegramEditContext(ctx, source.ID, "-123:7", time.Unix(200, 0).UTC(), "new synthetic", []byte(`{"id":7}`), nil)
	require.NoError(err)
	assert.True(changed)
	var body, metadata string
	var edited bool
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT body_text FROM message_bodies WHERE message_id = ?`), messageID).Scan(&body))
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT metadata, is_edited FROM messages WHERE id = ?`), messageID).Scan(&metadata, &edited))
	assert.Equal("new synthetic", body)
	assert.True(edited)
	matches, err := st.MessageContentMatchesContext(ctx, messageID, sql.NullString{String: "new synthetic", Valid: true}, store.FTSDoc{Subject: "Synthetic", Body: "new synthetic"})
	require.NoError(err)
	assert.True(matches)
	var fields map[string]any
	require.NoError(json.Unmarshal([]byte(metadata), &fields))
	assert.Equal([]any{"synthetic-key"}, fields["attachment_part_keys"])
	assert.Equal("-123:6", fields["referenced_message_id"])
	assert.Equal("1970-01-01T00:03:20Z", fields["edited_at"])
	changed, err = st.ApplyTelegramEditContext(ctx, source.ID, "-123:7", time.Unix(200, 0).UTC(), "new synthetic", []byte(`{"id":7}`), nil)
	require.NoError(err)
	assert.False(changed)
	changed, err = st.ApplyTelegramEditContext(ctx, source.ID, "-123:7", time.Unix(150, 0).UTC(), "older", []byte(`{"id":7}`), nil)
	require.NoError(err)
	assert.False(changed)
	_, err = st.ApplyTelegramEditContext(ctx, source.ID, "-123:7", time.Unix(200, 0).UTC(), "conflicting", []byte(`{"id":7}`), nil)
	require.ErrorContains(err, "conflicting")
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT body_text FROM message_bodies WHERE message_id = ?`), messageID).Scan(&body))
	assert.Equal("new synthetic", body)
}

func TestTelegramEditCreatesAttachmentManifestForLegacyMessage(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	ctx := context.Background()
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("telegram", "user999")
	require.NoError(err)
	conv, err := st.EnsureConversationWithType(source.ID, "-123", "group_chat", "Synthetic")
	require.NoError(err)
	_, err = st.PersistMessage(&store.MessagePersistData{
		Message: &store.Message{
			SourceID: source.ID, ConversationID: conv, SourceMessageID: "-123:7",
			MessageType: "telegram", SentAt: sql.NullTime{Time: time.Unix(100, 0), Valid: true},
		},
		BodyText: sql.NullString{String: "old synthetic", Valid: true},
		Metadata: &sql.NullString{String: `{"referenced_message_id":"-123:6"}`, Valid: true},
	})
	require.NoError(err)

	changed, err := st.ApplyTelegramEditContext(ctx, source.ID, "-123:7", time.Unix(200, 0).UTC(),
		"new synthetic", []byte(`{"id":7}`), []string{"telegram:attachment:new"})
	require.NoError(err)
	assert.True(changed)
	var raw string
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT metadata FROM messages WHERE source_id = ? AND source_message_id = ?`), source.ID, "-123:7").Scan(&raw))
	var fields struct {
		ReferencedMessageID string   `json:"referenced_message_id"`
		AttachmentPartKeys  []string `json:"attachment_part_keys"`
	}
	require.NoError(json.Unmarshal([]byte(raw), &fields))
	assert.Equal("-123:6", fields.ReferencedMessageID)
	assert.Equal([]string{"telegram:attachment:new"}, fields.AttachmentPartKeys)
}
