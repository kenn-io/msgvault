package export

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/mime"
)

func TestLooseBlobWritesCountsCreatedOnly(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	dir := t.TempDir()
	other := t.TempDir()
	assert.Equal(int64(0), LooseBlobWrites(dir), "fresh directory")

	_, err := StoreAttachmentFile(dir, &mime.Attachment{Content: []byte("first blob")})
	require.NoError(err)
	assert.Equal(int64(1), LooseBlobWrites(dir), "new blob counts")

	_, err = StoreAttachmentFile(dir, &mime.Attachment{Content: []byte("first blob")})
	require.NoError(err)
	assert.Equal(int64(1), LooseBlobWrites(dir), "deduplicated blob does not count")

	_, err = StoreAttachmentFileDurable(dir, &mime.Attachment{Content: []byte("durable blob")})
	require.NoError(err)
	assert.Equal(int64(2), LooseBlobWrites(dir), "durable write counts")

	src := filepath.Join(t.TempDir(), "source.bin")
	require.NoError(os.WriteFile(src, []byte("from path blob"), 0o600))
	rel, _, _, err := StoreAttachmentFromPath(dir, src, 0)
	require.NoError(err)
	assert.NotEmpty(rel)
	assert.Equal(int64(3), LooseBlobWrites(dir), "path import counts")
	rel, _, _, err = StoreAttachmentFromPath(dir, src, 0)
	require.NoError(err)
	assert.NotEmpty(rel)
	assert.Equal(int64(3), LooseBlobWrites(dir), "existing path import does not count")

	rel, hash, size, err := StoreAttachmentStream(t.Context(), dir, strings.NewReader("streamed blob"), 64)
	require.NoError(err)
	assert.Equal(hash[:2]+"/"+hash, rel)
	assert.Equal(int64(len("streamed blob")), size)
	assert.Equal(int64(4), LooseBlobWrites(dir), "stream counts")
	rel, _, _, err = StoreAttachmentStream(t.Context(), dir, strings.NewReader("streamed blob"), 64)
	require.NoError(err)
	assert.NotEmpty(rel)
	assert.Equal(int64(4), LooseBlobWrites(dir), "deduplicated stream does not count")
	rel, _, _, err = StoreAttachmentStream(t.Context(), dir, strings.NewReader("too large"), 4)
	require.ErrorIs(err, ErrAttachmentTooLarge)
	assert.Empty(rel)
	assert.Equal(int64(4), LooseBlobWrites(dir), "over-cap stream publishes nothing")

	assert.Equal(int64(0), LooseBlobWrites(other), "counters are per attachments directory")
}

// A corrupt existing blob is a store failure, never a size-cap skip.
func TestStoreAttachmentStreamCorruptExistingBlobIsNotTooLarge(t *testing.T) {
	require := require.New(t)
	dir := t.TempDir()
	rel, _, _, err := StoreAttachmentStream(t.Context(), dir, strings.NewReader("recording bytes"), 64)
	require.NoError(err)
	stored := filepath.Join(dir, filepath.FromSlash(rel))
	require.NoError(os.Chmod(stored, 0o600))
	require.NoError(os.WriteFile(stored, []byte("corrupt  bytes!"), 0o600))
	rel, _, _, err = StoreAttachmentStream(t.Context(), dir, strings.NewReader("recording bytes"), 64)
	require.Error(err)
	require.NotErrorIs(err, ErrAttachmentTooLarge)
	require.Empty(rel)
}
