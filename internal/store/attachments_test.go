package store_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestReplaceAndListMessageDiscordAttachments(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("discord", "123456789012345678")
	require.NoError(err)
	conversationID, err := st.EnsureConversationWithType(
		source.ID, "234567890123456789", "channel", "general",
	)
	require.NoError(err)
	messageID := insertStoreTestMessage(t, st, source.ID, conversationID, "345678901234567890")

	beeperRef := store.AttachmentRef{
		Filename:           "beeper.jpg",
		MimeType:           "image/jpeg",
		StoragePath:        "be/eper",
		ContentHash:        "beeper-hash",
		Size:               12,
		SourceAttachmentID: "beeper:asset-1",
	}
	require.NoError(st.ReplaceMessageBeeperAttachments(messageID, []store.AttachmentRef{beeperRef}))

	want := map[string]store.AttachmentRef{
		"discord:attachment-1": {
			Filename:           "image.png",
			MimeType:           "image/png",
			StoragePath:        "ab/abcdef",
			ContentHash:        "abcdef",
			Size:               4096,
			SourceAttachmentID: "discord:attachment-1",
			MediaType:          "image",
			Width:              640,
			Height:             480,
		},
		"discord:attachment-2": {
			Filename:           "later.bin",
			MimeType:           "application/octet-stream",
			StoragePath:        "https://cdn.discordapp.com/attachments/1/2/later.bin",
			Size:               8192,
			SourceAttachmentID: "discord:attachment-2",
		},
	}
	require.NoError(st.ReplaceMessageDiscordAttachments(messageID, []store.AttachmentRef{
		want["discord:attachment-1"],
		want["discord:attachment-2"],
	}))

	got, err := st.MessageDiscordAttachments(messageID)
	require.NoError(err)
	assert.Equal(want, got)

	keep := want["discord:attachment-2"]
	keep.StoragePath = "https://cdn.discordapp.com/attachments/1/2/refreshed.bin"
	require.NoError(st.ReplaceMessageDiscordAttachments(messageID, []store.AttachmentRef{keep}))
	got, err = st.MessageDiscordAttachments(messageID)
	require.NoError(err)
	assert.Equal(map[string]store.AttachmentRef{keep.SourceAttachmentID: keep}, got)

	beeperGot, err := st.MessageBeeperAttachments(messageID)
	require.NoError(err)
	assert.Equal(map[string]store.AttachmentRef{beeperRef.SourceAttachmentID: beeperRef}, beeperGot)
}

func TestListDiscordPendingAttachmentMessages(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("discord", "123456789012345678")
	require.NoError(err)
	conversationID, err := st.EnsureConversationWithType(
		source.ID, "234567890123456789", "channel", "general",
	)
	require.NoError(err)
	pendingMessageID := insertStoreTestMessage(t, st, source.ID, conversationID, "345678901234567890")
	downloadedMessageID := insertStoreTestMessage(t, st, source.ID, conversationID, "345678901234567891")

	require.NoError(st.ReplaceMessageDiscordAttachments(pendingMessageID, []store.AttachmentRef{{
		StoragePath:        "https://cdn.discordapp.com/attachments/1/2/pending.bin",
		SourceAttachmentID: "discord:pending",
	}}))
	require.NoError(st.ReplaceMessageDiscordAttachments(downloadedMessageID, []store.AttachmentRef{{
		StoragePath:        "cd/downloaded",
		ContentHash:        "downloaded-hash",
		SourceAttachmentID: "discord:downloaded",
	}}))
	require.NoError(st.ReplaceMessageBeeperAttachments(downloadedMessageID, []store.AttachmentRef{{
		StoragePath:        "https://example.com/beeper-pending.bin",
		SourceAttachmentID: "beeper:pending",
	}}))

	otherSource, err := st.GetOrCreateSource("discord", "999999999999999999")
	require.NoError(err)
	otherConversationID, err := st.EnsureConversationWithType(
		otherSource.ID, "888888888888888888", "channel", "other",
	)
	require.NoError(err)
	otherMessageID := insertStoreTestMessage(t, st, otherSource.ID, otherConversationID, "777777777777777777")
	require.NoError(st.ReplaceMessageDiscordAttachments(otherMessageID, []store.AttachmentRef{{
		StoragePath:        "https://cdn.discordapp.com/attachments/9/8/pending.bin",
		SourceAttachmentID: "discord:other-pending",
	}}))

	items, err := st.ListDiscordPendingAttachmentMessages(source.ID)
	require.NoError(err)
	assert.Equal([]store.DiscordPendingAttachmentMessage{{
		MessageID:       pendingMessageID,
		SourceMessageID: "345678901234567890",
		ChatID:          "234567890123456789",
	}}, items)

	beeperItems, err := st.ListBeeperPendingAttachmentMessages(source.ID)
	require.NoError(err)
	assert.Equal([]store.BeeperPendingAttachmentMessage{{
		MessageID:       downloadedMessageID,
		SourceMessageID: "345678901234567891",
		ChatID:          "234567890123456789",
	}}, beeperItems)
}
