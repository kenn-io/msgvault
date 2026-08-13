package store_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	internalmime "go.kenn.io/msgvault/internal/mime"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestRepairHistoricalAttachmentRolesUsesUnambiguousRawMIME(t *testing.T) {
	assert := assert.New(t)
	f := storetest.New(t)
	messageID := seedHistoricalMIMEAttachment(t, f, "repair-explicit", "attachment", "asset-1", "unique-bytes")

	progress, err := f.Store.RepairHistoricalAttachmentRolesBatch(t.Context(), 10)
	require.NoError(t, err)
	assert.True(progress.Completed)
	assert.Equal(1, progress.MessagesScanned)
	assert.Equal(1, progress.AttachmentsUpdated)
	assert.Equal(messageID, progress.LastMessageID)

	var role, roleSource, sourcePartKey, contentID string
	require.NoError(t, f.Store.DB().QueryRow(f.Store.Rebind(`
		SELECT attachment_role, role_source, source_part_key, content_id
		FROM attachments WHERE message_id = ?`), messageID).
		Scan(&role, &roleSource, &sourcePartKey, &contentID))
	assert.Equal("inline", role, "a Content-ID is inline evidence even with attachment disposition")
	assert.Equal("raw_mime_repair", roleSource)
	assert.NotEmpty(sourcePartKey)
	assert.Equal("asset-1", contentID)
}

func TestRepairHistoricalAttachmentRolesLeavesAmbiguousBytesUnknown(t *testing.T) {
	assert := assert.New(t)
	f := storetest.New(t)
	messageID := f.CreateMessage("repair-ambiguous")
	raw := historicalMIME(t, "attachment", "first", "same-bytes", "attachment", "second", "same-bytes")
	require.NoError(t, f.Store.UpsertMessageRaw(messageID, raw))
	insertHistoricalAttachment(t, f, messageID, "same-bytes")

	progress, err := f.Store.RepairHistoricalAttachmentRolesBatch(t.Context(), 10)
	require.NoError(t, err)
	assert.True(progress.Completed)
	assert.Zero(progress.AttachmentsUpdated)

	var role, roleSource string
	require.NoError(t, f.Store.DB().QueryRow(f.Store.Rebind(`
		SELECT attachment_role, role_source FROM attachments WHERE message_id = ?`), messageID).
		Scan(&role, &roleSource))
	assert.Equal("unknown", role)
	assert.Equal("unknown", roleSource)
}

func TestRepairHistoricalAttachmentRolesResumesFromDurableCursor(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	firstID := seedHistoricalMIMEAttachment(t, f, "repair-first", "attachment", "first", "first-bytes")
	secondID := seedHistoricalMIMEAttachment(t, f, "repair-second", "inline", "second", "second-bytes")
	require.Less(firstID, secondID)
	raw, err := f.Store.GetMessageRaw(secondID)
	require.NoError(err)
	parsed, err := internalmime.Parse(raw)
	require.NoError(err)
	require.NotEmpty(parsed.Attachments)
	for _, part := range parsed.Attachments {
		assert.Equal(digestString("second-bytes"), part.ContentHash)
		assert.NotEmpty(part.PartKey)
	}

	first, err := f.Store.RepairHistoricalAttachmentRolesBatch(t.Context(), 1)
	require.NoError(err)
	assert.False(first.Completed)
	assert.Equal(firstID, first.LastMessageID)
	assert.Equal(1, first.AttachmentsUpdated)

	second, err := f.Store.RepairHistoricalAttachmentRolesBatch(t.Context(), 1)
	require.NoError(err)
	assert.True(second.Completed)
	assert.Equal(secondID, second.LastMessageID)
	assert.Equal(1, second.AttachmentsUpdated)

	var role, roleSource string
	require.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`
		SELECT attachment_role, role_source FROM attachments WHERE message_id = ?`), secondID).
		Scan(&role, &roleSource))
	assert.Equal("inline", role)
	assert.Equal("raw_mime_repair", roleSource)
}

func TestRepairHistoricalAttachmentRolesCancellationLeavesCursorResumable(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	messageID := seedHistoricalMIMEAttachment(t, f, "repair-cancel", "attachment", "asset", "bytes")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err := f.Store.RepairHistoricalAttachmentRolesBatch(ctx, 10)
	require.ErrorIs(err, context.Canceled)

	progress, err := f.Store.RepairHistoricalAttachmentRolesBatch(t.Context(), 10)
	require.NoError(err)
	assert.True(progress.Completed)
	assert.Equal(messageID, progress.LastMessageID)
	assert.Equal(1, progress.AttachmentsUpdated)
}

func TestRepairHistoricalAttachmentRolesSkipsCorruptRawWithoutWedging(t *testing.T) {
	assert := assert.New(t)
	f := storetest.New(t)
	messageID := seedHistoricalMIMEAttachment(t, f, "repair-corrupt", "attachment", "", "bytes")
	_, err := f.Store.DB().Exec(f.Store.Rebind(`
		UPDATE message_raw SET raw_data = ?, compression = 'zlib' WHERE message_id = ?`),
		[]byte("not-zlib"), messageID)
	require.NoError(t, err)

	progress, err := f.Store.RepairHistoricalAttachmentRolesBatch(t.Context(), 10)
	require.NoError(t, err)
	assert.True(progress.Completed)
	assert.Equal(messageID, progress.LastMessageID)
	assert.Zero(progress.AttachmentsUpdated)

	var role string
	require.NoError(t, f.Store.DB().QueryRow(f.Store.Rebind(`
		SELECT attachment_role FROM attachments WHERE message_id = ?`), messageID).Scan(&role))
	assert.Equal("unknown", role)
}

func TestRepairHistoricalAttachmentRolesFindsMessagesAddedAfterCompletion(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	firstID := seedHistoricalMIMEAttachment(t, f, "repair-complete-first", "attachment", "", "first")

	first, err := f.Store.RepairHistoricalAttachmentRolesBatch(t.Context(), 10)
	require.NoError(err)
	assert.True(first.Completed)
	assert.Equal(firstID, first.LastMessageID)

	secondID := seedHistoricalMIMEAttachment(t, f, "repair-complete-second", "attachment", "", "second")
	require.Greater(secondID, firstID)
	second, err := f.Store.RepairHistoricalAttachmentRolesBatch(t.Context(), 10)
	require.NoError(err)
	assert.True(second.Completed)
	assert.Equal(secondID, second.LastMessageID)
	assert.Equal(1, second.AttachmentsUpdated)
}

func TestRepairHistoricalAttachmentRolesCollapsesResyncDuplicate(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	messageID := f.CreateMessage("repair-resync-duplicate")
	raw := historicalMIME(t, "attachment", "", "same-bytes")
	require.NoError(f.Store.UpsertMessageRaw(messageID, raw))
	parsed, err := internalmime.Parse(raw)
	require.NoError(err)
	require.Len(parsed.Attachments, 1)
	part := parsed.Attachments[0]

	_, err = f.Store.DB().Exec(f.Store.Rebind(`
		INSERT INTO attachments
			(message_id, filename, mime_type, storage_path, content_hash, size,
			 attachment_role, role_source, created_at)
		VALUES (?, 'legacy.pdf', 'application/pdf', 'legacy/path', ?, 10,
		        'unknown', 'legacy_api', CURRENT_TIMESTAMP)`), messageID, part.ContentHash)
	require.NoError(err)
	_, err = f.Store.DB().Exec(f.Store.Rebind(`
		INSERT INTO attachments
			(message_id, filename, mime_type, storage_path, content_hash, size,
			 attachment_role, role_source, source_part_key, created_at)
		VALUES (?, 'current.pdf', 'application/pdf', 'current/path', ?, 10,
		        'standalone', 'mime_disposition', ?, CURRENT_TIMESTAMP)`),
		messageID, part.ContentHash, part.PartKey)
	require.NoError(err)
	var keyedID int64
	require.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`
		SELECT id FROM attachments WHERE message_id = ? AND source_part_key = ?`),
		messageID, part.PartKey).Scan(&keyedID))
	_, err = f.Store.DB().Exec(f.Store.Rebind(`
		UPDATE messages SET has_attachments = TRUE, attachment_count = 2 WHERE id = ?`), messageID)
	require.NoError(err)

	progress, err := f.Store.RepairHistoricalAttachmentRolesBatch(t.Context(), 10)
	require.NoError(err)
	assert.True(progress.Completed)
	assert.Equal(1, progress.AttachmentsUpdated)

	var id int64
	var count int
	var filename, storagePath, role, roleSource, partKey string
	require.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`
		SELECT MIN(id), COUNT(*), MIN(filename), MIN(storage_path),
		       MIN(attachment_role), MIN(role_source), MIN(source_part_key)
		FROM attachments WHERE message_id = ?`), messageID).
		Scan(&id, &count, &filename, &storagePath, &role, &roleSource, &partKey))
	assert.Equal(keyedID, id)
	assert.Equal(1, count)
	assert.Equal("current.pdf", filename)
	assert.Equal("current/path", storagePath)
	assert.Equal("standalone", role)
	assert.Equal("mime_disposition", roleSource)
	assert.Equal(part.PartKey, partKey)
	var attachmentCount int
	require.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`
		SELECT attachment_count FROM messages WHERE id = ?`), messageID).Scan(&attachmentCount))
	assert.Equal(1, attachmentCount)
}

func TestRepairHistoricalAttachmentRolesDoesNotApplyStaleMIMEEvidence(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	messageID := seedHistoricalMIMEAttachment(
		t, f, "repair-concurrent-resync", "attachment", "old-content", "old-bytes",
	)
	newHash := digestString("new-bytes")
	restore := f.Store.SetAttachmentRoleRepairPreparedHookForTest(func() {
		_, err := f.Store.DB().Exec(f.Store.Rebind(`
			UPDATE attachments
			SET content_hash = ?, storage_path = ?, size = ?
			WHERE message_id = ?`),
			newHash, newHash[:2]+"/"+newHash, len("new-bytes"), messageID)
		require.NoError(err)
	})
	defer restore()

	progress, err := f.Store.RepairHistoricalAttachmentRolesBatch(t.Context(), 10)
	require.NoError(err)
	assert.True(progress.Completed)
	assert.Zero(progress.AttachmentsUpdated)

	var hash, role, roleSource string
	require.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`
		SELECT content_hash, attachment_role, role_source
		FROM attachments WHERE message_id = ?`), messageID).
		Scan(&hash, &role, &roleSource))
	assert.Equal(newHash, hash)
	assert.Equal("unknown", role)
	assert.Equal("unknown", roleSource)
}

func seedHistoricalMIMEAttachment(
	t *testing.T,
	f *storetest.Fixture,
	sourceMessageID, disposition, contentID, content string,
) int64 {
	t.Helper()
	messageID := f.CreateMessage(sourceMessageID)
	require.NoError(t, f.Store.UpsertMessageRaw(messageID,
		historicalMIME(t, disposition, contentID, content)))
	insertHistoricalAttachment(t, f, messageID, content)
	return messageID
}

func insertHistoricalAttachment(t *testing.T, f *storetest.Fixture, messageID int64, content string) {
	t.Helper()
	hash := digestString(content)
	_, err := f.Store.DB().Exec(f.Store.Rebind(`
		INSERT INTO attachments
			(message_id, filename, mime_type, storage_path, content_hash, size, created_at)
		VALUES (?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)`),
		messageID, "asset.bin", "application/octet-stream", hash[:2]+"/"+hash, hash, len(content),
	)
	require.NoError(t, err)
}

func digestString(content string) string {
	digest := sha256.Sum256([]byte(content))
	return hex.EncodeToString(digest[:])
}

func historicalMIME(t *testing.T, parts ...string) []byte {
	t.Helper()
	require.Zero(t, len(parts)%3)
	var body strings.Builder
	body.WriteString("From: sender@example.com\r\nTo: recipient@example.com\r\n" +
		"MIME-Version: 1.0\r\nContent-Type: multipart/mixed; boundary=repair\r\n\r\n" +
		"--repair\r\nContent-Type: text/plain\r\n\r\nbody\r\n")
	var disposition, contentID string
	for index, part := range parts {
		switch index % 3 {
		case 0:
			disposition = part
		case 1:
			contentID = part
		case 2:
			_, _ = fmt.Fprintf(&body, "--repair\r\nContent-Type: application/octet-stream\r\n"+
				"Content-Disposition: %s; filename=asset.bin\r\nContent-ID: <%s>\r\n\r\n%s\r\n",
				disposition, contentID, part)
		}
	}
	body.WriteString("--repair--\r\n")
	return []byte(body.String())
}
