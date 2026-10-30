package telegram

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

func TestLiveWriterResolvesRepliesAtPageBoundary(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	ctx := context.Background()
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(SourceType, OwnerSourceIdentifier(999))
	require.NoError(err)
	writer, err := NewLiveWriter(ctx, st, source.ID, 999, "Owner")
	require.NoError(err)
	chat := Chat{ID: -123, Type: "private_group", Name: "Synthetic"}
	require.NoError(writer.Write(ctx, chat, Message{ID: 2, SentAt: time.Unix(100, 0), Text: "child", ReplyTo: 1, RawJSON: `{}`}))
	require.NoError(writer.Write(ctx, chat, Message{ID: 1, SentAt: time.Unix(90, 0), Text: "parent", RawJSON: `{}`}))
	var linked bool
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT reply_to_message_id IS NOT NULL FROM messages WHERE source_id = ? AND source_message_id = ?`), source.ID, "-123:2").Scan(&linked))
	assert.False(linked)
	require.NoError(writer.ResolveReplies(ctx))
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT reply_to_message_id IS NOT NULL FROM messages WHERE source_id = ? AND source_message_id = ?`), source.ID, "-123:2").Scan(&linked))
	assert.True(linked)
}

func TestLiveWriterRefreshesConversationStatsAfterFlush(t *testing.T) {
	for _, tc := range []struct {
		name          string
		failMediaSave bool
	}{
		{name: "successful write"},
		{name: "partial write failure", failMediaSave: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			st := testutil.NewTestStore(t)
			source, err := st.GetOrCreateSource(SourceType, OwnerSourceIdentifier(999))
			require.NoError(t, err)
			writer, err := NewLiveWriter(ctx, st, source.ID, 999, "Owner")
			require.NoError(t, err)

			message := Message{ID: 7, SentAt: time.Unix(100, 0).UTC(), Text: "synthetic message", RawJSON: `{}`}
			if tc.failMediaSave {
				_, err := st.DB().Exec(`CREATE TRIGGER reject_live_attachment BEFORE INSERT ON attachments BEGIN SELECT RAISE(ABORT, 'synthetic attachment failure'); END`)
				require.NoError(t, err)
				message.LiveMedia = &LiveMedia{SourceID: "document:7", Filename: "sample.bin", Kind: "document", Metadata: `{}`}
			}
			err = writer.Write(ctx, Chat{ID: -123, Type: "private_group", Name: "Synthetic"}, message)
			if tc.failMediaSave {
				require.ErrorContains(t, err, "synthetic attachment failure")
			} else {
				require.NoError(t, err)
			}
			var beforeCount int
			require.NoError(t, st.DB().QueryRow(st.Rebind(`
				SELECT c.message_count FROM conversations c JOIN messages m ON m.conversation_id = c.id
				WHERE m.source_id = ? AND m.source_message_id = ?`), source.ID, "-123:7").Scan(&beforeCount))
			assert.Zero(t, beforeCount, "a message write must not rescan its conversation")
			require.NoError(t, writer.FlushConversationStats(ctx))

			var count int
			var lastAt sql.NullTime
			var preview sql.NullString
			require.NoError(t, st.DB().QueryRow(st.Rebind(`
				SELECT c.message_count, c.last_message_at, c.last_message_preview
				FROM conversations c JOIN messages m ON m.conversation_id = c.id
				WHERE m.source_id = ? AND m.source_message_id = ?`), source.ID, "-123:7").Scan(&count, &lastAt, &preview))
			assert.Equal(t, 1, count)
			assert.True(t, lastAt.Valid)
			assert.Equal(t, message.SentAt, lastAt.Time)
			assert.True(t, preview.Valid)
			assert.Equal(t, message.Text, preview.String)
		})
	}
}

func TestLiveWriterBatchesConversationStatRefreshes(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(SourceType, OwnerSourceIdentifier(999))
	require.NoError(t, err)
	writer, err := NewLiveWriter(ctx, st, source.ID, 999, "Owner")
	require.NoError(t, err)
	_, err = st.DB().Exec(`CREATE TABLE conversation_stat_refreshes (id INTEGER PRIMARY KEY)`)
	require.NoError(t, err)
	_, err = st.DB().Exec(`CREATE TRIGGER count_conversation_stat_refresh AFTER UPDATE OF message_count, last_message_at, last_message_preview ON conversations BEGIN INSERT INTO conversation_stat_refreshes(id) VALUES (NULL); END`)
	require.NoError(t, err)

	chat := Chat{ID: -123, Type: "private_group", Name: "Synthetic"}
	require.NoError(t, writer.Write(ctx, chat, Message{ID: 2, SentAt: time.Unix(100, 0).UTC(), Text: "second", RawJSON: `{}`}))
	require.NoError(t, writer.Write(ctx, chat, Message{ID: 1, SentAt: time.Unix(90, 0).UTC(), Text: "first", RawJSON: `{}`}))
	var refreshes int
	require.NoError(t, st.DB().QueryRow(`SELECT COUNT(*) FROM conversation_stat_refreshes`).Scan(&refreshes))
	assert.Zero(t, refreshes)
	require.NoError(t, writer.FlushConversationStats(ctx))
	require.NoError(t, st.DB().QueryRow(`SELECT COUNT(*) FROM conversation_stat_refreshes`).Scan(&refreshes))
	assert.Equal(t, 1, refreshes)
}

func TestLiveWriterFlushesConversationStatsAfterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(SourceType, OwnerSourceIdentifier(999))
	require.NoError(t, err)
	writer, err := NewLiveWriter(ctx, st, source.ID, 999, "Owner")
	require.NoError(t, err)
	require.NoError(t, writer.Write(ctx, Chat{ID: -123, Type: "private_group", Name: "Synthetic"}, Message{
		ID: 7, SentAt: time.Unix(100, 0).UTC(), Text: "synthetic message", RawJSON: `{}`,
	}))
	cancel()
	require.NoError(t, writer.FlushConversationStats(ctx))
	var count int
	require.NoError(t, st.DB().QueryRow(st.Rebind(`
		SELECT c.message_count FROM conversations c JOIN messages m ON m.conversation_id = c.id
		WHERE m.source_id = ? AND m.source_message_id = ?`), source.ID, "-123:7").Scan(&count))
	assert.Equal(t, 1, count)
}

func TestLiveWriterEditPersistsNewMediaManifestBeforeRecovery(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	ctx := context.Background()
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(SourceType, OwnerSourceIdentifier(999))
	require.NoError(err)
	writer, err := NewLiveWriter(ctx, st, source.ID, 999, "Owner")
	require.NoError(err)
	chat := Chat{ID: -123, Type: "private_group", Name: "Synthetic"}
	sent := time.Unix(100, 0).UTC()
	original := Message{ID: 7, SentAt: sent, Text: "before edit", RawJSON: `{}`,
		LiveMedia: &LiveMedia{SourceID: "document:old", Filename: "old.bin", Kind: "document", Metadata: `{}`},
	}
	require.NoError(writer.Write(ctx, chat, original))

	_, err = st.DB().Exec(`CREATE TRIGGER reject_live_attachment BEFORE INSERT ON attachments BEGIN SELECT RAISE(ABORT, 'synthetic attachment failure'); END`)
	require.NoError(err)
	edited := Message{ID: 7, SentAt: sent, Text: "after edit", Edited: true, EditedAt: time.Unix(200, 0).UTC(), RawJSON: `{"id":7}`,
		LiveMedia: &LiveMedia{SourceID: "document:new", Filename: "new.bin", Kind: "document", Metadata: `{}`},
	}
	err = writer.WriteEdit(ctx, chat, edited)
	require.ErrorContains(err, "synthetic attachment failure")
	_, err = st.DB().Exec(`DROP TRIGGER reject_live_attachment`)
	require.NoError(err)

	var messageID int64
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT id FROM messages WHERE source_id = ? AND source_message_id = ?`), source.ID, "-123:7").Scan(&messageID))
	var metadata string
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT metadata FROM messages WHERE id = ?`), messageID).Scan(&metadata))
	assert.Contains(metadata, syntheticAttachmentHash(chat.ID, edited.ID, "api:document:old"))
	assert.Contains(metadata, syntheticAttachmentHash(chat.ID, edited.ID, "api:document:new"))

	require.NoError(writer.WriteEdit(ctx, chat, edited))
	var attachmentCount int
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT COUNT(*) FROM attachments WHERE message_id = ?`), messageID).Scan(&attachmentCount))
	assert.Equal(2, attachmentCount, "edit recovery preserves old media and adds the new occurrence")
}

func TestLiveWriterEditUsesMigratedMediaIdentity(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(SourceType, OwnerSourceIdentifier(999))
	require.NoError(t, err)
	legacyWriter, err := NewLiveWriter(ctx, st, source.ID, 999, "Owner")
	require.NoError(t, err)

	const legacyChatID int64 = 123
	const canonicalChatID int64 = -1000000000123
	const messageID int64 = 7
	sentAt := time.Unix(100, 0).UTC()
	legacyChat := Chat{ID: legacyChatID, Type: chatTypePrivateSupergroup, Name: "Synthetic"}
	original := Message{
		ID: messageID, SentAt: sentAt, Text: "before edit", RawJSON: `{}`,
		LiveMedia: &LiveMedia{SourceID: "document:old", Filename: "old.bin", Kind: "document", Metadata: `{}`},
	}
	require.NoError(t, legacyWriter.Write(ctx, legacyChat, original))
	secondEvidence := Message{ID: 8, SentAt: sentAt.Add(time.Second), Text: "migration evidence", RawJSON: `{}`}
	require.NoError(t, legacyWriter.Write(ctx, legacyChat, secondEvidence))
	require.NoError(t, st.MigrateTelegramExportKeysContext(ctx, source.ID, []store.TelegramExportKeyChange{{
		LegacyID: legacyChatID, MarkedID: canonicalChatID, ConversationType: "group_chat",
		Evidence: []store.TelegramExportKeyEvidence{
			{MessageID: messageID, SentAt: sentAt, Text: original.Text},
			{MessageID: secondEvidence.ID, SentAt: secondEvidence.SentAt, Text: secondEvidence.Text},
		},
	}}))

	writer, err := NewLiveWriter(ctx, st, source.ID, 999, "Owner")
	require.NoError(t, err)
	edited := Message{
		ID: messageID, SentAt: sentAt, Text: "after edit", Edited: true,
		EditedAt: time.Unix(200, 0).UTC(), RawJSON: `{"id":7}`,
		LiveMedia: &LiveMedia{SourceID: "document:new", Filename: "new.bin", Kind: "document", Metadata: `{}`},
	}
	require.NoError(t, writer.WriteEdit(ctx, Chat{ID: canonicalChatID, Type: chatTypePrivateSupergroup, Name: "Synthetic"}, edited))

	var messageKey, metadata string
	require.NoError(t, st.DB().QueryRow(st.Rebind(`SELECT source_message_id, metadata FROM messages WHERE source_id = ? AND source_message_id = ?`), source.ID, "-1000000000123:7").Scan(&messageKey, &metadata))
	assert.Equal(t, "-1000000000123:7", messageKey)
	var recovery telegramMessageMetadata
	require.NoError(t, json.Unmarshal([]byte(metadata), &recovery))
	assert.Equal(t, legacyChatID, recovery.LegacyMediaChatID)
	assert.Contains(t, recovery.AttachmentPartKeys, syntheticAttachmentHash(legacyChatID, messageID, "api:document:old"))
	assert.Contains(t, recovery.AttachmentPartKeys, syntheticAttachmentHash(legacyChatID, messageID, "api:document:new"))

	var attachmentCount int
	require.NoError(t, st.DB().QueryRow(st.Rebind(`SELECT COUNT(*) FROM attachments a JOIN messages m ON m.id = a.message_id WHERE m.source_id = ? AND m.source_message_id = ?`), source.ID, messageKey).Scan(&attachmentCount))
	assert.Equal(t, 2, attachmentCount, "the edit recovery stores media under the retained legacy identity")
}
