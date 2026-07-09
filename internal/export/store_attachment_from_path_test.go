package export

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func writeSrcFile(t *testing.T, dir, name string, content []byte) string {
	t.Helper()
	p := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(p, content, 0o600))
	return p
}

func TestStoreAttachmentFromPath(t *testing.T) {
	content := []byte("hello packed world")
	sum := sha256.Sum256(content)
	wantHash := hex.EncodeToString(sum[:])

	t.Run("stores new file at canonical path", func(t *testing.T) {
		require := require.New(t)
		assert := assert.New(t)
		attDir := t.TempDir()
		src := writeSrcFile(t, t.TempDir(), "a.bin", content)

		rel, hash, size, err := StoreAttachmentFromPath(attDir, src, 0)
		require.NoError(err)
		assert.Equal(wantHash[:2]+"/"+wantHash, rel)
		assert.Equal(wantHash, hash)
		assert.Equal(int64(len(content)), size)

		got, err := os.ReadFile(filepath.Join(attDir, wantHash[:2], wantHash))
		require.NoError(err)
		assert.Equal(content, got)
	})

	t.Run("dedups against valid existing file", func(t *testing.T) {
		require := require.New(t)
		assert := assert.New(t)
		attDir := t.TempDir()
		src := writeSrcFile(t, t.TempDir(), "a.bin", content)
		_, _, _, err := StoreAttachmentFromPath(attDir, src, 0)
		require.NoError(err)

		rel, hash, _, err := StoreAttachmentFromPath(attDir, src, 0)
		require.NoError(err)
		assert.Equal(wantHash[:2]+"/"+wantHash, rel)
		assert.Equal(wantHash, hash)
	})

	t.Run("rejects source larger than maxSize but returns no hash", func(t *testing.T) {
		attDir := t.TempDir()
		src := writeSrcFile(t, t.TempDir(), "a.bin", content)

		_, hash, _, err := StoreAttachmentFromPath(attDir, src, 4)
		require.Error(t, err)
		assert.Empty(t, hash)
	})

	t.Run("errors on missing source", func(t *testing.T) {
		_, hash, _, err := StoreAttachmentFromPath(t.TempDir(), filepath.Join(t.TempDir(), "gone"), 0)
		require.Error(t, err)
		assert.Empty(t, hash)
	})

	t.Run("errors on symlink source", func(t *testing.T) {
		srcDir := t.TempDir()
		target := writeSrcFile(t, srcDir, "target.bin", content)
		link := filepath.Join(srcDir, "link.bin")
		require.NoError(t, os.Symlink(target, link))

		_, _, _, err := StoreAttachmentFromPath(t.TempDir(), link, 0)
		require.Error(t, err)
	})

	t.Run("errors on corrupt existing file", func(t *testing.T) {
		attDir := t.TempDir()
		// Pre-plant a wrong-content file at the canonical path.
		require.NoError(t, os.MkdirAll(filepath.Join(attDir, wantHash[:2]), 0o700))
		require.NoError(t, os.WriteFile(
			filepath.Join(attDir, wantHash[:2], wantHash), []byte("XXXXXXXXXXXXXXXXXX"), 0o600))

		src := writeSrcFile(t, t.TempDir(), "a.bin", content)
		_, _, _, err := StoreAttachmentFromPath(attDir, src, 0)
		require.Error(t, err)
	})
}
