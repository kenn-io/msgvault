package store_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestAttachmentRoleSchemaDefaultsHistoricalRowsUnknown(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	messageID := f.CreateMessage("legacy-role-default")
	_, err := f.Store.DB().Exec(f.Store.Rebind(`
		INSERT INTO attachments
			(message_id, filename, mime_type, storage_path, content_hash, size, created_at)
		VALUES (?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)`),
		messageID, "legacy.png", "image/png", "legacy/attachment", "legacy-hash", 42,
	)
	require.NoError(err)

	fileID := singleAttachmentID(t, f, messageID)
	file, err := f.Store.GetFileMetadata(t.Context(), fileID)
	require.NoError(err)
	require.NotNil(file)
	assert.Equal(store.AttachmentRoleUnknown, file.AttachmentRole)
	assert.Equal(store.AttachmentRoleSourceUnknown, file.RoleSource)
	assert.Empty(file.SourcePartKey)
	assert.Empty(file.ContentID)
}

func TestUpsertAttachmentRecordPersistsRoleProvenanceAndSourcePartKey(t *testing.T) {
	require := require.New(t)
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
	require.NoError(err)

	fileID := singleAttachmentID(t, f, messageID)
	file, err := f.Store.GetFileMetadata(t.Context(), fileID)
	require.NoError(err)
	require.NotNil(file)
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
	require := require.New(t)
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
	require.NoError(f.Store.UpsertAttachmentRecord(t.Context(), messageID, write))

	write.Filename = "after.png"
	write.StoragePath = secondHash[:2] + "/" + secondHash
	write.ContentHash = secondHash
	write.Size = 200
	require.NoError(f.Store.UpsertAttachmentRecord(t.Context(), messageID, write))

	var (
		count       int
		filename    string
		contentHash string
		size        int64
	)
	require.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`
		SELECT COUNT(*), MIN(filename), MIN(content_hash), MIN(size)
		FROM attachments WHERE message_id = ?`), messageID).
		Scan(&count, &filename, &contentHash, &size))
	assert.Equal(1, count)
	assert.Equal("after.png", filename)
	assert.Equal(secondHash, contentHash)
	assert.Equal(int64(200), size)
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
