package telegram

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/attachmentpolicy"
	"go.kenn.io/msgvault/internal/export"
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

func TestLiveWriterPersistsMediaPlaceholderAndReplays(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	ctx := context.Background()
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(SourceType, OwnerSourceIdentifier(999))
	require.NoError(err)
	writer, err := NewLiveWriter(ctx, st, source.ID, 999, "Synthetic")
	require.NoError(err)
	chat := Chat{ID: -123, Type: "private_group", Name: "Synthetic"}
	message := Message{ID: 7, SentAt: time.Unix(100, 0), RawJSON: `{}`, LiveMedia: &LiveMedia{SourceID: "document:101", Filename: "example.ogg", MIMEType: "audio/ogg", Kind: "voice_message", Metadata: `{"document_id":101}`}}
	require.NoError(writer.Write(ctx, chat, message))
	require.NoError(writer.Write(ctx, chat, message))
	var count int
	var metadata, state string
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT COUNT(*), MIN(CAST(a.attachment_metadata AS TEXT)), MIN(a.attachment_state) FROM attachments a JOIN messages m ON m.id = a.message_id WHERE m.source_id = ? AND m.source_message_id = ?`), source.ID, "-123:7").Scan(&count, &metadata, &state))
	assert.Equal(1, count)
	assert.JSONEq(`{"document_id":101}`, metadata)
	assert.Equal("pending", state)
}

func TestLiveMediaDoesNotExpandArchivedManifest(t *testing.T) {
	require := require.New(t)

	ctx := context.Background()
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(SourceType, OwnerSourceIdentifier(999))
	require.NoError(err)
	writer, err := NewLiveWriter(ctx, st, source.ID, 999, "Synthetic")
	require.NoError(err)
	chat := Chat{ID: -123, Type: "private_group", Name: "Synthetic"}
	message := Message{ID: 7, SentAt: time.Unix(100, 0), RawJSON: `{}`}
	require.NoError(writer.Write(ctx, chat, message))
	message.LiveMedia = &LiveMedia{SourceID: "document:101", Kind: "file", Metadata: `{}`}
	require.NoError(writer.Write(ctx, chat, message))
	var count int
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT COUNT(*) FROM attachments a JOIN messages m ON m.id = a.message_id WHERE m.source_id = ? AND m.source_message_id = ?`), source.ID, "-123:7").Scan(&count))
	assert.Zero(t, count)
}

func TestLiveWriterFillsPendingMediaWithDownloadedContent(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	ctx := context.Background()
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(SourceType, OwnerSourceIdentifier(999))
	require.NoError(err)
	writer, err := NewLiveWriter(ctx, st, source.ID, 999, "Synthetic")
	require.NoError(err)
	chat := Chat{ID: -123, Type: "private_group"}
	media := &LiveMedia{SourceID: "document:101", Kind: "file", Metadata: `{}`}
	message := Message{ID: 7, SentAt: time.Unix(100, 0), RawJSON: `{}`, LiveMedia: media}
	require.NoError(writer.Write(ctx, chat, message))
	path, hash, size, err := export.StoreAttachmentStream(ctx, t.TempDir(), strings.NewReader("synthetic"), 20)
	require.NoError(err)
	media.StoragePath, media.ContentHash, media.Size = path, hash, size
	require.NoError(writer.Write(ctx, chat, message))
	var gotPath, gotHash, state string
	var gotSize int64
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT a.storage_path, a.content_hash, a.size, a.attachment_state FROM attachments a JOIN messages m ON m.id = a.message_id WHERE m.source_id = ? AND m.source_message_id = ?`), source.ID, "-123:7").Scan(&gotPath, &gotHash, &gotSize, &state))
	assert.Equal(path, gotPath)
	assert.Equal(hash, gotHash)
	assert.Equal(int64(9), gotSize)
	assert.Equal("stored", state)
}

func TestLiveWriterRecordsMediaPolicySkip(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	ctx := context.Background()
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(SourceType, OwnerSourceIdentifier(999))
	require.NoError(err)
	writer, err := NewLiveWriter(ctx, st, source.ID, 999, "Synthetic")
	require.NoError(err)
	message := Message{ID: 7, SentAt: time.Unix(100, 0), RawJSON: `{}`, LiveMedia: &LiveMedia{SourceID: "document:101", Kind: "file", Metadata: `{}`, SkipReason: attachmentpolicy.SkipSizeCap}}
	require.NoError(writer.Write(ctx, Chat{ID: -123, Type: "private_group"}, message))
	var state, reason string
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT a.attachment_state, a.attachment_skip_reason FROM attachments a JOIN messages m ON m.id = a.message_id WHERE m.source_id = ? AND m.source_message_id = ?`), source.ID, "-123:7").Scan(&state, &reason))
	assert.Equal("skipped", state)
	assert.Equal("size_cap", reason)
}

func TestLiveWriterRecordsRetryableMediaFailure(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	ctx := context.Background()
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(SourceType, OwnerSourceIdentifier(999))
	require.NoError(err)
	writer, err := NewLiveWriter(ctx, st, source.ID, 999, "Synthetic")
	require.NoError(err)
	message := Message{ID: 7, SentAt: time.Unix(100, 0), RawJSON: `{}`, LiveMedia: &LiveMedia{SourceID: "document:101", Kind: "file", Metadata: `{}`, Failed: true}}
	require.NoError(writer.Write(ctx, Chat{ID: -123, Type: "private_group"}, message))
	var state, reason string
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT a.attachment_state, a.attachment_skip_reason FROM attachments a JOIN messages m ON m.id = a.message_id WHERE m.source_id = ? AND m.source_message_id = ?`), source.ID, "-123:7").Scan(&state, &reason))
	assert.Equal("failed", state)
	assert.Equal("fetch_failure", reason)
}

func TestLiveWriterProcessesOnlyMediaAllowedByManifest(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	ctx := context.Background()
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(SourceType, OwnerSourceIdentifier(999))
	require.NoError(err)
	writer, err := NewLiveWriter(ctx, st, source.ID, 999, "Synthetic")
	require.NoError(err)
	chat := Chat{ID: -123, Type: "private_group"}
	message := Message{ID: 7, SentAt: time.Unix(100, 0), RawJSON: `{}`}
	require.NoError(writer.Write(ctx, chat, message))
	called := 0
	writer.SetMediaProcessor(func(_ context.Context, _ Chat, media *LiveMedia) error {
		called++
		media.SkipReason = attachmentpolicy.SkipSizeCap
		return nil
	})
	message.LiveMedia = &LiveMedia{SourceID: "document:101", Kind: "file", Metadata: `{}`}
	require.NoError(writer.Write(ctx, chat, message))
	assert.Zero(called)
	message.ID = 8
	require.NoError(writer.Write(ctx, chat, message))
	assert.Equal(1, called)
	var state string
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT a.attachment_state FROM attachments a JOIN messages m ON m.id = a.message_id WHERE m.source_id = ? AND m.source_message_id = ?`), source.ID, "-123:8").Scan(&state))
	assert.Equal("skipped", state)
}

func TestLiveWriterDoesNotRedownloadStoredMedia(t *testing.T) {
	require := require.New(t)

	ctx := context.Background()
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(SourceType, OwnerSourceIdentifier(999))
	require.NoError(err)
	writer, err := NewLiveWriter(ctx, st, source.ID, 999, "Synthetic")
	require.NoError(err)
	path, hash, size, err := export.StoreAttachmentStream(ctx, t.TempDir(), strings.NewReader("synthetic"), 20)
	require.NoError(err)
	calls := 0
	writer.SetMediaProcessor(func(_ context.Context, _ Chat, media *LiveMedia) error {
		calls++
		media.StoragePath, media.ContentHash, media.Size = path, hash, size
		return nil
	})
	message := Message{ID: 7, SentAt: time.Unix(100, 0), RawJSON: `{}`, LiveMedia: &LiveMedia{SourceID: "document:101", Kind: "file", Metadata: `{}`}}
	chat := Chat{ID: -123, Type: "private_group"}
	require.NoError(writer.Write(ctx, chat, message))
	require.NoError(writer.Write(ctx, chat, message))
	assert.Equal(t, 1, calls)
}

func TestLiveEditRetriesPendingMediaBeforeUpdatingMessage(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	ctx := context.Background()
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(SourceType, OwnerSourceIdentifier(999))
	require.NoError(err)
	writer, err := NewLiveWriter(ctx, st, source.ID, 999, "Synthetic")
	require.NoError(err)
	chat := Chat{ID: -123, Type: "private_group"}
	message := Message{ID: 7, SentAt: time.Unix(100, 0), Text: "original", RawJSON: `{}`, LiveMedia: &LiveMedia{SourceID: "document:101", Kind: "file", Metadata: `{}`}}
	require.NoError(writer.Write(ctx, chat, message))
	path, hash, size, err := export.StoreAttachmentStream(ctx, t.TempDir(), strings.NewReader("synthetic"), 20)
	require.NoError(err)
	writer.SetMediaProcessor(func(_ context.Context, _ Chat, media *LiveMedia) error {
		media.StoragePath, media.ContentHash, media.Size = path, hash, size
		return nil
	})
	message.Text, message.Edited, message.EditedAt = "edited", true, time.Unix(101, 0)
	require.NoError(writer.WriteEdit(ctx, chat, message))
	var state string
	var id int64
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT m.id, a.attachment_state FROM attachments a JOIN messages m ON m.id = a.message_id WHERE m.source_id = ? AND m.source_message_id = ?`), source.ID, "-123:7").Scan(&id, &state))
	assert.Equal("stored", state)
	var body string
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT body_text FROM message_bodies WHERE message_id = ?`), id).Scan(&body))
	assert.Equal("edited", body)
	assert.Equal(SyncCounts{Processed: 2, Added: 1, Updated: 1}, writer.Counts())
}

func TestTelegramPendingMediaSelectionIsSourceScopedAndPaged(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	ctx := context.Background()
	st := testutil.NewTestStore(t)
	first, err := st.GetOrCreateSource(SourceType, OwnerSourceIdentifier(999))
	require.NoError(err)
	second, err := st.GetOrCreateSource(SourceType, OwnerSourceIdentifier(998))
	require.NoError(err)
	chat := Chat{ID: -123, Type: "private_group"}
	for _, source := range []*store.Source{first, second} {
		owner := int64(999)
		if source.ID == second.ID {
			owner = 998
		}
		writer, err := NewLiveWriter(ctx, st, source.ID, owner, "Synthetic")
		require.NoError(err)
		for id := int64(1); id <= 2; id++ {
			message := Message{ID: id, SentAt: time.Unix(100, 0), RawJSON: `{}`, LiveMedia: &LiveMedia{SourceID: fmt.Sprintf("document:%d", id), Kind: "file", Metadata: `{}`}}
			if id == 1 {
				message.LiveMedia.SkipReason = attachmentpolicy.SkipSizeCap
			}
			require.NoError(writer.Write(ctx, chat, message))
		}
		path, hash, size, err := export.StoreAttachmentStream(ctx, t.TempDir(), strings.NewReader("synthetic"), 20)
		require.NoError(err)
		stored := Message{ID: 3, SentAt: time.Unix(100, 0), RawJSON: `{}`, LiveMedia: &LiveMedia{SourceID: "document:3", Kind: "file", Metadata: `{}`, StoragePath: path, ContentHash: hash, Size: size}}
		require.NoError(writer.Write(ctx, chat, stored))
	}
	page, err := st.ListTelegramPendingMediaAfterContext(ctx, first.ID, 0, 1)
	require.NoError(err)
	require.Len(page, 1)
	assert.Equal("-123:1", page[0].SourceMessageID)
	next, err := st.ListTelegramPendingMediaAfterContext(ctx, first.ID, page[0].MessageID, 1)
	require.NoError(err)
	require.Len(next, 1)
	assert.Equal("-123:2", next[0].SourceMessageID)
	end, err := st.ListTelegramPendingMediaAfterContext(ctx, first.ID, next[0].MessageID, 1)
	require.NoError(err)
	assert.Empty(end)
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
	path, hash, size, err := export.StoreAttachmentStream(ctx, t.TempDir(), strings.NewReader("synthetic"), 20)
	require.NoError(t, err)
	processed := 0
	writer.SetMediaProcessor(func(_ context.Context, _ Chat, media *LiveMedia) error {
		processed++
		assert.Equal(t, "document:new", media.SourceID)
		media.StoragePath, media.ContentHash, media.Size = path, hash, size
		return nil
	})
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
	assert.Equal(t, 1, processed)
	var archivedID int64
	require.NoError(t, st.DB().QueryRow(st.Rebind(`SELECT id FROM messages WHERE source_id = ? AND source_message_id = ?`), source.ID, messageKey).Scan(&archivedID))
	attachments, err := st.MessageProviderAttachmentsContext(ctx, archivedID, "api:")
	require.NoError(t, err)
	assert.Equal(t, path, attachments["api:document:new"].StoragePath)
	assert.Equal(t, hash, attachments["api:document:new"].ContentHash)
}
