package telegram

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
