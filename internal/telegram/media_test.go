package telegram

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMediaWritesCancelled(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	root, attachments := t.TempDir(), t.TempDir()
	require.NoError(os.WriteFile(filepath.Join(root, "photo.jpg"), []byte("photo bytes"), 0600))
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	summary := &ImportSummary{}
	writes, err := mediaWrites(ctx, nil, 0, &Message{Photo: "photo.jpg"}, ImportOptions{RootDir: root, AttachmentsDir: attachments}, summary)
	require.ErrorIs(err, context.Canceled)
	assert.Empty(writes)
	assert.Zero(summary.AttachmentsStored)
	require.NoError(filepath.WalkDir(attachments, func(_ string, entry os.DirEntry, err error) error {
		require.NoError(err)
		assert.True(entry.IsDir(), "cancelled copy must leave no blob or staging file")
		return nil
	}))
}
