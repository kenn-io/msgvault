package export

import (
	"os"
	"path/filepath"
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

	assert.Equal(int64(0), LooseBlobWrites(other), "counters are per attachments directory")
}
