package api

import (
	"context"
	"testing"

	"github.com/gotd/td/tg"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/telegram"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestApplyAPIEditUpdatesExistingMessageAndReplays(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	ctx := context.Background()
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(telegram.SourceType, telegram.OwnerSourceIdentifier(999))
	require.NoError(err)
	writer, err := telegram.NewLiveWriter(ctx, st, source.ID, 999, "Synthetic")
	require.NoError(err)
	dialog := CatalogDialog{ChatID: -123, NativeID: 123, Kind: "group", Title: "Synthetic"}
	msg := &tg.Message{ID: 7, PeerID: &tg.PeerChat{ChatID: 123}, Date: 100, Message: "original"}
	require.NoError(ArchiveAPIMessageWithWriter(ctx, writer, dialog, msg, nil, nil))
	msg.Message = "edited synthetic"
	msg.SetEditDate(200)
	require.NoError(ApplyAPIEdit(ctx, writer, dialog, msg, nil, nil))
	require.NoError(ApplyAPIEdit(ctx, writer, dialog, msg, nil, nil))
	var messageID int64
	var edited bool
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT id, is_edited FROM messages WHERE source_id = ? AND source_message_id = ?`), source.ID, "-123:7").Scan(&messageID, &edited))
	var body string
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT body_text FROM message_bodies WHERE message_id = ?`), messageID).Scan(&body))
	assert.Equal("edited synthetic", body)
	assert.True(edited)
	assert.Equal(int64(1), writer.Counts().Updated)
}

func TestApplyAPIEditReplayRecoversMediaAndRefreshesConversationStats(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	ctx := context.Background()
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(telegram.SourceType, telegram.OwnerSourceIdentifier(999))
	require.NoError(err)
	writer, err := telegram.NewLiveWriter(ctx, st, source.ID, 999, "Synthetic")
	require.NoError(err)
	dialog := CatalogDialog{ChatID: -123, NativeID: 123, Kind: "group", Title: "Synthetic"}
	edited := &tg.Message{ID: 7, PeerID: &tg.PeerChat{ChatID: 123}, Date: 100, Message: "edited synthetic"}
	edited.SetEditDate(200)
	media := &tg.MessageMediaDocument{}
	media.SetDocument(&tg.Document{
		ID: 101, AccessHash: 202, FileReference: []byte("synthetic-reference"),
		DCID: 3, Size: 42, MimeType: "application/pdf",
		Attributes: []tg.DocumentAttributeClass{&tg.DocumentAttributeFilename{FileName: "attachment.pdf"}},
	})
	edited.SetMedia(media)
	require.NoError(ApplyAPIEdit(ctx, writer, dialog, edited, nil, nil))
	require.NoError(writer.FlushConversationStats(ctx))

	var messageID, conversationID int64
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT id, conversation_id FROM messages WHERE source_id = ? AND source_message_id = ?`), source.ID, "-123:7").Scan(&messageID, &conversationID))
	_, err = st.DB().Exec(st.Rebind(`DELETE FROM attachments WHERE message_id = ?`), messageID)
	require.NoError(err)
	_, err = st.DB().Exec(st.Rebind(`UPDATE conversations SET last_message_preview = ? WHERE id = ?`), "stale preview", conversationID)
	require.NoError(err)

	// The first edit persisted its manifest, but a crash can happen before
	// its pending attachment and conversation stats finish recovering.
	require.NoError(ApplyAPIEdit(ctx, writer, dialog, edited, nil, nil))
	require.NoError(writer.FlushConversationStats(ctx))

	var attachmentCount int
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT COUNT(*) FROM attachments WHERE message_id = ?`), messageID).Scan(&attachmentCount))
	assert.Equal(1, attachmentCount, "an equal edit replay restores manifest-listed media")
	var preview string
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT last_message_preview FROM conversations WHERE id = ?`), conversationID).Scan(&preview))
	assert.Equal("edited synthetic", preview, "an equal edit replay refreshes conversation stats")
}

func TestApplyAPIEditRejectsMismatchedPeer(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(telegram.SourceType, telegram.OwnerSourceIdentifier(999))
	require.NoError(t, err)
	writer, err := telegram.NewLiveWriter(ctx, st, source.ID, 999, "Synthetic")
	require.NoError(t, err)
	msg := &tg.Message{ID: 7, PeerID: &tg.PeerChat{ChatID: 124}, Date: 100, Message: "synthetic"}
	msg.SetEditDate(200)
	err = ApplyAPIEdit(ctx, writer, CatalogDialog{ChatID: -123, NativeID: 123, Kind: "group"}, msg, nil, nil)
	require.ErrorContains(t, err, "dialog")
}

func TestApplyAPIEditArchivesPreviouslyUnseenMessage(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	ctx := context.Background()
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(telegram.SourceType, telegram.OwnerSourceIdentifier(999))
	require.NoError(err)
	writer, err := telegram.NewLiveWriter(ctx, st, source.ID, 999, "Synthetic")
	require.NoError(err)
	msg := &tg.Message{ID: 7, PeerID: &tg.PeerChat{ChatID: 123}, Date: 100, Message: "first observed"}
	msg.SetEditDate(200)
	require.NoError(ApplyAPIEdit(ctx, writer, CatalogDialog{ChatID: -123, NativeID: 123, Kind: "group", Title: "Synthetic"}, msg, nil, nil))
	var edited bool
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT is_edited FROM messages WHERE source_id = ? AND source_message_id = ?`), source.ID, "-123:7").Scan(&edited))
	assert.True(edited)
	assert.Equal(int64(1), writer.Counts().Added)
}
