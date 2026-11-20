package telegram

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

func TestStoreMediaCancelled(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	root, attachments := t.TempDir(), t.TempDir()
	require.NoError(os.WriteFile(filepath.Join(root, "photo.jpg"), []byte("photo bytes"), 0600))
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	write := store.AttachmentWrite{}
	err := storeMedia(ctx, mediaSlot{key: "photo", path: "photo.jpg"}, &write, ImportOptions{RootDir: root, AttachmentsDir: attachments})
	require.ErrorIs(err, context.Canceled)
	assert.Empty(write.ContentHash)
	assert.Empty(write.StoragePath)
	require.NoError(filepath.WalkDir(attachments, func(_ string, entry os.DirEntry, err error) error {
		require.NoError(err)
		assert.True(entry.IsDir(), "cancelled copy must leave no blob or staging file")
		return nil
	}))
}

func TestMediaPlaceholdersMissingFileHasNoMIMEType(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	writes := mediaPlaceholders(&Message{File: "(File not included. Change data exporting settings to download.)", FileName: "report.pdf"})
	require.Len(writes, 1)
	assert.Equal("file", writes[0].Filename)
	assert.Empty(writes[0].MIMEType)
}
