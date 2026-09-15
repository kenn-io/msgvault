package store_test

import (
	"database/sql"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/attachmentpolicy"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestAttachmentRoleSchemaDefaultsHistoricalRowsUnknown(t *testing.T) {
	assert := assert.New(t)
	f := storetest.New(t)
	messageID := f.CreateMessage("legacy-role-default")
	_, err := f.Store.DB().Exec(f.Store.Rebind(`
		INSERT INTO attachments
			(message_id, filename, mime_type, storage_path, content_hash, size, created_at)
		VALUES (?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)`),
		messageID, "legacy.png", "image/png", "legacy/attachment", "legacy-hash", 42,
	)
	require.NoError(t, err)

	fileID := singleAttachmentID(t, f, messageID)
	file, err := f.Store.GetFileMetadata(t.Context(), fileID)
	require.NoError(t, err)
	require.NotNil(t, file)
	assert.Equal(store.AttachmentRoleUnknown, file.AttachmentRole)
	assert.Equal(store.AttachmentRoleSourceUnknown, file.RoleSource)
	assert.Empty(file.SourcePartKey)
	assert.Empty(file.ContentID)
}

func TestUpsertAttachmentRecordPersistsRoleProvenanceAndSourcePartKey(t *testing.T) {
	assert := assert.New(t)
	f := storetest.New(t)
	messageID := f.CreateMessage("typed-attachment-write")
	hash := strings.Repeat("ab", 32)

	err := f.Store.UpsertAttachmentRecord(t.Context(), messageID, store.AttachmentWrite{
		Filename:           "photo.png",
		MIMEType:           "image/png",
		StoragePath:        hash[:2] + "/" + hash,
		ContentHash:        hash,
		Size:               2048,
		SourceAttachmentID: "provider:file-1",
		MediaType:          "image",
		Width:              640,
		Height:             480,
		Role:               store.AttachmentRoleStandalone,
		RoleSource:         store.AttachmentRoleSourceProviderExplicit,
		SourcePartKey:      "provider:file-1",
		ContentID:          "asset-1@example.invalid",
	})
	require.NoError(t, err)

	fileID := singleAttachmentID(t, f, messageID)
	file, err := f.Store.GetFileMetadata(t.Context(), fileID)
	require.NoError(t, err)
	require.NotNil(t, file)
	assert.Equal(store.AttachmentRoleStandalone, file.AttachmentRole)
	assert.Equal(store.AttachmentRoleSourceProviderExplicit, file.RoleSource)
	assert.Equal("provider:file-1", file.SourcePartKey)
	assert.Equal("asset-1@example.invalid", file.ContentID)
}

func TestLegacyUpsertAttachmentFailsClosed(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	messageID := f.CreateMessage("legacy-attachment-api")
	require.NoError(f.Store.UpsertAttachment(
		messageID, "legacy.png", "image/png", "legacy/path", "legacy-content", 12,
	))

	file, err := f.Store.GetFileMetadata(t.Context(), singleAttachmentID(t, f, messageID))
	require.NoError(err)
	require.NotNil(file)
	assert.Equal(store.AttachmentRoleUnknown, file.AttachmentRole)
	assert.Equal(store.AttachmentRoleSourceLegacyAPI, file.RoleSource)
}

func TestSourceAwareUpsertPromotesMatchingLegacyOccurrence(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	messageID := f.CreateMessage("promote-legacy-occurrence")
	hash := strings.Repeat("bc", 32)
	require.NoError(f.Store.UpsertAttachment(
		messageID, "legacy.pdf", "application/pdf", "legacy/path", hash, 12,
	))
	legacyID := singleAttachmentID(t, f, messageID)

	require.NoError(f.Store.UpsertAttachmentRecord(t.Context(), messageID, store.AttachmentWrite{
		Filename: "current.pdf", MIMEType: "application/pdf", StoragePath: hash[:2] + "/" + hash,
		ContentHash: hash, Size: 12, Role: store.AttachmentRoleStandalone,
		RoleSource: store.AttachmentRoleSourceMIMEDisposition, SourcePartKey: "mime:2",
	}))

	var id int64
	var count int
	var role, roleSource, partKey, filename, storagePath string
	require.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`
		SELECT MIN(id), COUNT(*), MIN(attachment_role), MIN(role_source),
		       MIN(source_part_key), MIN(filename), MIN(storage_path)
		FROM attachments WHERE message_id = ?`), messageID).
		Scan(&id, &count, &role, &roleSource, &partKey, &filename, &storagePath))
	assert.Equal(legacyID, id)
	assert.Equal(1, count)
	assert.Equal("standalone", role)
	assert.Equal("mime_disposition", roleSource)
	assert.Equal("mime:2", partKey)
	assert.Equal("current.pdf", filename)
	assert.Equal(hash[:2]+"/"+hash, storagePath)
}

func TestAttachmentSourcePartKeyPreservesDistinctDuplicateBytes(t *testing.T) {
	f := storetest.New(t)
	messageID := f.CreateMessage("duplicate-byte-parts")
	hash := strings.Repeat("cd", 32)
	for _, partKey := range []string{"mime:1.2", "mime:1.3"} {
		require.NoError(t, f.Store.UpsertAttachmentRecord(t.Context(), messageID, store.AttachmentWrite{
			Filename:      "same.png",
			MIMEType:      "image/png",
			StoragePath:   hash[:2] + "/" + hash,
			ContentHash:   hash,
			Size:          100,
			Role:          store.AttachmentRoleStandalone,
			RoleSource:    store.AttachmentRoleSourceMIMEDisposition,
			SourcePartKey: partKey,
		}))
	}

	var count int
	require.NoError(t, f.Store.DB().QueryRow(f.Store.Rebind(`
		SELECT COUNT(*) FROM attachments WHERE message_id = ?`), messageID).Scan(&count))
	assert.Equal(t, 2, count)
}

func TestAttachmentSourcePartKeyResyncUpdatesOneOccurrence(t *testing.T) {
	assert := assert.New(t)
	f := storetest.New(t)
	messageID := f.CreateMessage("source-part-resync")
	firstHash := strings.Repeat("de", 32)
	secondHash := strings.Repeat("ef", 32)
	write := store.AttachmentWrite{
		Filename:      "before.png",
		MIMEType:      "image/png",
		StoragePath:   firstHash[:2] + "/" + firstHash,
		ContentHash:   firstHash,
		Size:          100,
		Role:          store.AttachmentRoleStandalone,
		RoleSource:    store.AttachmentRoleSourceMIMEDisposition,
		SourcePartKey: "mime:2.1",
	}
	require.NoError(t, f.Store.UpsertAttachmentRecord(t.Context(), messageID, write))

	write.Filename = "after.png"
	write.StoragePath = secondHash[:2] + "/" + secondHash
	write.ContentHash = secondHash
	write.Size = 200
	require.NoError(t, f.Store.UpsertAttachmentRecord(t.Context(), messageID, write))

	var (
		count       int
		filename    string
		contentHash string
		size        int64
	)
	require.NoError(t, f.Store.DB().QueryRow(f.Store.Rebind(`
		SELECT COUNT(*), MIN(filename), MIN(content_hash), MIN(size)
		FROM attachments WHERE message_id = ?`), messageID).
		Scan(&count, &filename, &contentHash, &size))
	assert.Equal(1, count)
	assert.Equal("after.png", filename)
	assert.Equal(secondHash, contentHash)
	assert.Equal(int64(200), size)
}

func TestUpsertAttachmentRecordPreservingStoredKeepsBlobForMissingResync(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	messageID := f.CreateMessage("preserve-stored-attachment")
	hash := strings.Repeat("fa", 32)
	write := store.AttachmentWrite{
		Filename: "before.jpg", MIMEType: "image/jpeg", StoragePath: hash[:2] + "/" + hash,
		ContentHash: hash, Size: 91, Role: store.AttachmentRoleStandalone,
		RoleSource: store.AttachmentRoleSourceImporterSemantics, SourcePartKey: "imazing:photo",
		State: attachmentpolicy.StateStored,
	}
	require.NoError(f.Store.UpsertAttachmentRecordPreservingStored(t.Context(), messageID, write))
	fileID := singleAttachmentID(t, f, messageID)

	write.Filename = "after.jpg"
	write.StoragePath = ""
	write.ContentHash = ""
	write.Size = 0
	write.State = attachmentpolicy.StateFailed
	write.SkipReason = attachmentpolicy.SkipFetchFailure
	require.NoError(f.Store.UpsertAttachmentRecordPreservingStored(t.Context(), messageID, write))

	var id, size int64
	var filename, storagePath, contentHash, state, skipReason string
	require.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`
		SELECT id, filename, storage_path, content_hash, size,
		       COALESCE(attachment_state, ''), COALESCE(attachment_skip_reason, '')
		FROM attachments WHERE message_id = ? AND source_part_key = ?`),
		messageID, write.SourcePartKey).Scan(
		&id, &filename, &storagePath, &contentHash, &size, &state, &skipReason,
	))
	assert.Equal(fileID, id)
	assert.Equal("after.jpg", filename)
	assert.Equal(hash[:2]+"/"+hash, storagePath)
	assert.Equal(hash, contentHash)
	assert.Equal(int64(91), size)
	assert.Equal(string(attachmentpolicy.StateStored), state)
	assert.Empty(skipReason)
}

// Regression: a missing attachment inserts a row with a NULL content_hash,
// so a rerun while the bytes are still absent must not fail scanning that
// NULL into a string when the stored occurrence is loaded.
func TestUpsertAttachmentRecordPreservingStoredRerunWhileMissing(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	messageID := f.CreateMessage("preserve-missing-attachment-rerun")
	write := store.AttachmentWrite{
		Filename: "missing.pdf", MIMEType: "application/pdf",
		Role:          store.AttachmentRoleStandalone,
		RoleSource:    store.AttachmentRoleSourceImporterSemantics,
		SourcePartKey: "imazing_csv:attachment:missing",
		State:         attachmentpolicy.StateFailed,
		SkipReason:    attachmentpolicy.SkipFetchFailure,
	}
	require.NoError(f.Store.UpsertAttachmentRecordPreservingStored(t.Context(), messageID, write))

	var contentHash sql.NullString
	require.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`
		SELECT content_hash FROM attachments
		WHERE message_id = ? AND source_part_key = ?`),
		messageID, write.SourcePartKey).Scan(&contentHash))
	require.True(contentHash.Valid == false || contentHash.String == "",
		"precondition: missing attachment stores no content hash")

	require.NoError(f.Store.UpsertAttachmentRecordPreservingStored(t.Context(), messageID, write))

	var count int
	var storagePath, state, skipReason string
	require.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`
		SELECT COUNT(*), MIN(storage_path), MIN(attachment_state),
		       MIN(attachment_skip_reason)
		FROM attachments WHERE message_id = ? AND source_part_key = ?`),
		messageID, write.SourcePartKey).Scan(&count, &storagePath, &state, &skipReason))
	assert.Equal(1, count, "rerun must not duplicate the missing occurrence")
	assert.Empty(storagePath)
	assert.Equal(string(attachmentpolicy.StateFailed), state)
	assert.Equal(string(attachmentpolicy.SkipFetchFailure), skipReason)
}

func TestDeleteKeyedAttachmentsExceptContextOnlyRemovesStalePrefixRows(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	messageID := f.CreateMessage("reconcile-keyed-attachments")
	for _, key := range []string{
		"imazing_csv:attachment:old",
		"imazing_csv:attachment:current",
		"imazingXcsv:attachment:foreign",
		"mime:2",
	} {
		require.NoError(f.Store.UpsertAttachmentRecord(t.Context(), messageID, store.AttachmentWrite{
			Filename: key + ".bin", Role: store.AttachmentRoleStandalone,
			RoleSource: store.AttachmentRoleSourceImporterSemantics, SourcePartKey: key,
		}))
	}

	require.NoError(f.Store.DeleteKeyedAttachmentsExceptContext(
		t.Context(), messageID, "imazing_csv:attachment:", "imazing_csv:attachment:current",
	))

	rows, err := f.Store.DB().Query(f.Store.Rebind(`
		SELECT source_part_key FROM attachments WHERE message_id = ? ORDER BY source_part_key`), messageID)
	require.NoError(err)
	defer func() { require.NoError(rows.Close()) }()
	var keys []string
	for rows.Next() {
		var key string
		require.NoError(rows.Scan(&key))
		keys = append(keys, key)
	}
	require.NoError(rows.Err())
	// SQLite orders text byte-wise while PostgreSQL uses its database
	// collation, so only the surviving set matters here, not row order.
	assert.ElementsMatch([]string{
		"imazingXcsv:attachment:foreign", "imazing_csv:attachment:current", "mime:2",
	}, keys)

	require.NoError(f.Store.DeleteKeyedAttachmentsExceptContext(
		t.Context(), messageID, "imazing_csv:attachment:", "",
	))
	var count int
	require.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`
		SELECT COUNT(*) FROM attachments WHERE message_id = ?`), messageID).Scan(&count))
	assert.Equal(2, count, "an empty keep key removes importer-owned rows but preserves foreign rows")
}

func TestUpsertAttachmentRecordRejectsInvalidRoleEvidence(t *testing.T) {
	f := storetest.New(t)
	messageID := f.CreateMessage("invalid-role")
	err := f.Store.UpsertAttachmentRecord(t.Context(), messageID, store.AttachmentWrite{
		StoragePath: "provider:pending",
		Role:        store.AttachmentRole("photo"),
		RoleSource:  store.AttachmentRoleSourceProviderExplicit,
	})
	require.ErrorContains(t, err, "attachment role")

	var count int
	require.NoError(t, f.Store.DB().QueryRow(f.Store.Rebind(`
		SELECT COUNT(*) FROM attachments WHERE message_id = ?`), messageID).Scan(&count))
	assert.Zero(t, count)
}
