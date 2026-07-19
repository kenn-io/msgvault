package store_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestReplaceMessageDiscordAttachmentsPreservesDuplicateContentSourceIDs(t *testing.T) {
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

	contentHash := strings.Repeat("ab", 32)
	storagePath := contentHash[:2] + "/" + contentHash
	require.NoError(st.ReplaceMessageDiscordAttachments(messageID, []store.AttachmentRef{
		{
			Filename: "first.bin", MimeType: "application/octet-stream",
			StoragePath: storagePath, ContentHash: contentHash, Size: 12,
			SourceAttachmentID: "discord:attachment-1",
		},
		{
			Filename: "second.bin", MimeType: "application/x-second",
			StoragePath: storagePath, ContentHash: contentHash, Size: 12,
			SourceAttachmentID: "discord:attachment-2",
		},
	}))

	got, err := st.MessageDiscordAttachments(messageID)
	require.NoError(err)
	require.Len(got, 2)
	assert.Equal("first.bin", got["discord:attachment-1"].Filename)
	assert.Equal("second.bin", got["discord:attachment-2"].Filename)
	assert.Equal(storagePath, got["discord:attachment-1"].StoragePath)
	assert.Equal(storagePath, got["discord:attachment-2"].StoragePath)
	hashes := []string{
		got["discord:attachment-1"].ContentHash,
		got["discord:attachment-2"].ContentHash,
	}
	assert.ElementsMatch([]string{contentHash, ""}, hashes)

	pending, err := st.ListDiscordPendingAttachmentMessages(source.ID)
	require.NoError(err)
	assert.Empty(pending, "hashless aliases of trusted local CAS paths are downloaded")

	remaining := got["discord:attachment-2"]
	require.Empty(remaining.ContentHash, "fixture must retain the duplicate alias row")
	require.NoError(st.ReplaceMessageDiscordAttachments(messageID, []store.AttachmentRef{remaining}))
	got, err = st.MessageDiscordAttachments(messageID)
	require.NoError(err)
	require.Len(got, 1)
	assert.Equal(contentHash, got["discord:attachment-2"].ContentHash,
		"a surviving local alias must be promoted to own the CAS hash")
}

func TestReplaceMessageDiscordAttachmentsPersistsEmptyURLMarker(t *testing.T) {
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

	require.NoError(st.ReplaceMessageDiscordAttachments(messageID, []store.AttachmentRef{{
		Filename: "unavailable.bin", MimeType: "application/octet-stream", Size: 42,
		SourceAttachmentID: "discord:attachment-empty",
	}}))

	got, err := st.MessageDiscordAttachments(messageID)
	require.NoError(err)
	assert.Equal(map[string]store.AttachmentRef{
		"discord:attachment-empty": {
			Filename: "unavailable.bin", MimeType: "application/octet-stream", Size: 42,
			StoragePath: "discord:pending:attachment-empty", SourceAttachmentID: "discord:attachment-empty",
		},
	}, got)
	pending, err := st.ListDiscordPendingAttachmentMessages(source.ID)
	require.NoError(err)
	assert.Equal([]store.DiscordPendingAttachmentMessage{{
		MessageID: messageID, SourceMessageID: "345678901234567890", ChatID: "234567890123456789",
	}}, pending)
}

func TestIsDiscordAttachmentDownloadedRequiresTrustedCASPath(t *testing.T) {
	contentHash := strings.Repeat("ef", 32)
	localPath := contentHash[:2] + "/" + contentHash
	tests := []struct {
		name string
		ref  store.AttachmentRef
		want bool
	}{
		{name: "hashed local CAS row", ref: store.AttachmentRef{StoragePath: localPath, ContentHash: contentHash}, want: true},
		{name: "hashless local duplicate alias", ref: store.AttachmentRef{StoragePath: localPath}, want: true},
		{name: "source URL with hash", ref: store.AttachmentRef{StoragePath: "https://cdn.discordapp.com/attachments/1/2/file.bin", ContentHash: contentHash}},
		{name: "provider pending sentinel", ref: store.AttachmentRef{StoragePath: "discord:pending:2"}},
		{name: "mismatched hash and path", ref: store.AttachmentRef{StoragePath: localPath, ContentHash: strings.Repeat("ab", 32)}},
		{name: "malformed local path", ref: store.AttachmentRef{StoragePath: "../" + contentHash}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, store.IsDiscordAttachmentDownloaded(tt.ref))
		})
	}
}

func TestBeeperHashlessLocalPathRemainsPending(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("beeper", "synthetic-account")
	require.NoError(err)
	conversationID, err := st.EnsureConversationWithType(source.ID, "chat-1", "chat", "synthetic chat")
	require.NoError(err)
	messageID := insertStoreTestMessage(t, st, source.ID, conversationID, "message-1")
	contentHash := strings.Repeat("ef", 32)
	require.NoError(st.ReplaceMessageBeeperAttachments(messageID, []store.AttachmentRef{{
		StoragePath: contentHash[:2] + "/" + contentHash, SourceAttachmentID: "beeper:asset-1",
	}}))

	pending, err := st.ListBeeperPendingAttachmentMessages(source.ID)
	require.NoError(err)
	assert.Equal([]store.BeeperPendingAttachmentMessage{{
		MessageID: messageID, SourceMessageID: "message-1", ChatID: "chat-1",
	}}, pending)
}

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
	downloadedHash := strings.Repeat("cd", 32)

	require.NoError(st.ReplaceMessageDiscordAttachments(pendingMessageID, []store.AttachmentRef{{
		StoragePath:        "https://cdn.discordapp.com/attachments/1/2/pending.bin",
		SourceAttachmentID: "discord:pending",
	}}))
	require.NoError(st.ReplaceMessageDiscordAttachments(downloadedMessageID, []store.AttachmentRef{{
		StoragePath:        downloadedHash[:2] + "/" + downloadedHash,
		ContentHash:        downloadedHash,
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
