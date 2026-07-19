package store_test

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func captureAttachmentQueryLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	store.ConfigureSQLLogging(store.SQLLogOptions{FullTrace: true})
	t.Cleanup(func() { store.ConfigureSQLLogging(store.SQLLogOptions{}) })
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return &logs
}

func TestListDiscordPendingAttachmentMessagesManyDownloadedUsesSingleQuery(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewSQLiteTestStore(t)
	source, err := st.GetOrCreateSource("discord", "source-many-downloaded")
	require.NoError(err)
	conversationID, err := st.EnsureConversationWithType(source.ID, "channel-many", "channel", "many")
	require.NoError(err)
	contentHash := strings.Repeat("a1", 32)
	for i := range 24 {
		messageID := insertStoreTestMessage(t, st, source.ID, conversationID, "downloaded-"+string(rune('a'+i)))
		refHash := contentHash[:62] + string("0123456789abcdef"[i%16]) + string("0123456789abcdef"[(i+1)%16])
		require.NoError(st.ReplaceMessageDiscordAttachments(messageID, []store.AttachmentRef{{
			StoragePath: refHash[:2] + "/" + refHash, ContentHash: refHash,
			SourceAttachmentID: "discord:file-" + string(rune('a'+i)),
		}}))
	}

	logs := captureAttachmentQueryLogs(t)
	pending, err := st.ListDiscordPendingAttachmentMessages(source.ID)
	require.NoError(err)
	assert.Empty(pending)
	assert.Equal(1, strings.Count(logs.String(), `"kind":"query"`),
		"pending scan must use one query regardless of message count")
}

func TestListDiscordPendingAttachmentMessagesGroupsScopesAndOrders(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewSQLiteTestStore(t)
	source, err := st.GetOrCreateSource("discord", "source-target")
	require.NoError(err)
	conversationID, err := st.EnsureConversationWithType(source.ID, "channel-target", "channel", "target")
	require.NoError(err)
	otherSource, err := st.GetOrCreateSource("discord", "source-other")
	require.NoError(err)
	otherConversationID, err := st.EnsureConversationWithType(otherSource.ID, "channel-other", "channel", "other")
	require.NoError(err)

	firstPendingID := insertStoreTestMessage(t, st, source.ID, conversationID, "pending-first")
	mixedPendingID := insertStoreTestMessage(t, st, source.ID, conversationID, "pending-mixed")
	beeperOnlyID := insertStoreTestMessage(t, st, source.ID, conversationID, "beeper-only")
	otherPendingID := insertStoreTestMessage(t, st, otherSource.ID, otherConversationID, "other-pending")

	firstHash := strings.Repeat("b2", 32)
	require.NoError(st.ReplaceMessageDiscordAttachments(firstPendingID, []store.AttachmentRef{{
		StoragePath: "discord:pending:first", SourceAttachmentID: "discord:first",
	}}))
	require.NoError(st.ReplaceMessageDiscordAttachments(mixedPendingID, []store.AttachmentRef{
		{
			StoragePath: firstHash[:2] + "/" + firstHash, ContentHash: firstHash,
			SourceAttachmentID: "discord:mixed-downloaded-1",
		},
		{
			StoragePath: firstHash[:2] + "/" + firstHash, ContentHash: firstHash,
			SourceAttachmentID: "discord:mixed-downloaded-2",
		},
		{
			StoragePath:        "https://cdn.discordapp.com/attachments/1/2/pending.bin",
			SourceAttachmentID: "discord:mixed-pending",
		},
	}))
	require.NoError(st.ReplaceMessageBeeperAttachments(beeperOnlyID, []store.AttachmentRef{{
		StoragePath: "beeper:pending", SourceAttachmentID: "beeper:only",
	}}))
	require.NoError(st.ReplaceMessageDiscordAttachments(otherPendingID, []store.AttachmentRef{{
		StoragePath: "discord:pending:other", SourceAttachmentID: "discord:other",
	}}))

	pending, err := st.ListDiscordPendingAttachmentMessages(source.ID)
	require.NoError(err)
	assert.Equal([]store.DiscordPendingAttachmentMessage{
		{MessageID: firstPendingID, SourceMessageID: "pending-first", ChatID: "channel-target"},
		{MessageID: mixedPendingID, SourceMessageID: "pending-mixed", ChatID: "channel-target"},
	}, pending)
}

func TestListDiscordAttachmentMessagesIncludesCompletedAndPendingInOneQuery(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewSQLiteTestStore(t)
	source, err := st.GetOrCreateSource("discord", "source-all-attachments")
	require.NoError(err)
	conversationID, err := st.EnsureConversationWithType(source.ID, "channel-all", "channel", "all")
	require.NoError(err)
	completedID := insertStoreTestMessage(t, st, source.ID, conversationID, "completed")
	pendingID := insertStoreTestMessage(t, st, source.ID, conversationID, "pending")
	beeperOnlyID := insertStoreTestMessage(t, st, source.ID, conversationID, "beeper-only")
	otherSource, err := st.GetOrCreateSource("discord", "other-all-attachments")
	require.NoError(err)
	otherConversationID, err := st.EnsureConversationWithType(otherSource.ID, "channel-other", "channel", "other")
	require.NoError(err)
	otherID := insertStoreTestMessage(t, st, otherSource.ID, otherConversationID, "other")

	hash := strings.Repeat("c3", 32)
	require.NoError(st.ReplaceMessageDiscordAttachments(completedID, []store.AttachmentRef{{
		StoragePath: hash[:2] + "/" + hash, ContentHash: hash, SourceAttachmentID: "discord:completed",
	}}))
	require.NoError(st.ReplaceMessageDiscordAttachments(pendingID, []store.AttachmentRef{{
		StoragePath: "discord:pending:pending", SourceAttachmentID: "discord:pending",
	}}))
	require.NoError(st.ReplaceMessageBeeperAttachments(beeperOnlyID, []store.AttachmentRef{{
		StoragePath: "beeper:pending", SourceAttachmentID: "beeper:only",
	}}))
	require.NoError(st.ReplaceMessageDiscordAttachments(otherID, []store.AttachmentRef{{
		StoragePath: "discord:pending:other", SourceAttachmentID: "discord:other",
	}}))

	logs := captureAttachmentQueryLogs(t)
	items, err := st.ListDiscordAttachmentMessages(source.ID)
	require.NoError(err)
	assert.Equal([]store.DiscordPendingAttachmentMessage{
		{MessageID: completedID, SourceMessageID: "completed", ChatID: "channel-all"},
		{MessageID: pendingID, SourceMessageID: "pending", ChatID: "channel-all"},
	}, items)
	assert.Equal(1, strings.Count(logs.String(), `"kind":"query"`), "all-attachment scan must use one query")
}

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
	assert.Equal(contentHash, got["discord:attachment-1"].ContentHash)
	assert.Equal(contentHash, got["discord:attachment-2"].ContentHash,
		"store reads must recover a duplicate alias hash from its trusted CAS path")

	message, err := st.GetMessage(messageID)
	require.NoError(err)
	require.Len(message.Attachments, 2)
	assert.Equal(contentHash, message.Attachments[0].ContentHash)
	assert.Equal(contentHash, message.Attachments[1].ContentHash)

	pending, err := st.ListDiscordPendingAttachmentMessages(source.ID)
	require.NoError(err)
	assert.Empty(pending, "hashless aliases of trusted local CAS paths are downloaded")

	remaining := got["discord:attachment-2"]
	var storedHash string
	require.NoError(st.DB().QueryRow(`
		SELECT COALESCE(content_hash, '') FROM attachments
		WHERE message_id = ? AND source_attachment_id = ?
	`, messageID, "discord:attachment-2").Scan(&storedHash))
	require.Empty(storedHash, "schema-level duplicate alias must retain an empty hash")
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
