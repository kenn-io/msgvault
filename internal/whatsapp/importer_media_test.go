package whatsapp

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHandleMediaFile(t *testing.T) {
	content := []byte("whatsapp media bytes")
	sum := sha256.Sum256(content)
	wantHash := hex.EncodeToString(sum[:])

	newOpts := func(t *testing.T) ImportOptions {
		t.Helper()
		mediaDir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(mediaDir, "photo.jpg"), content, 0o600))
		return ImportOptions{MediaDir: mediaDir, AttachmentsDir: t.TempDir()}
	}
	media := func(rel string) waMedia {
		return waMedia{FilePath: sql.NullString{String: rel, Valid: true}}
	}
	imp := &Importer{}

	t.Run("stores media at canonical content-addressed path", func(t *testing.T) {
		opts := newOpts(t)
		rel, hash := imp.handleMediaFile(media("photo.jpg"), opts)
		assert.Equal(t, filepath.Join(wantHash[:2], wantHash), rel)
		assert.Equal(t, wantHash, hash)

		got, err := os.ReadFile(filepath.Join(opts.AttachmentsDir, wantHash[:2], wantHash))
		require.NoError(t, err)
		assert.Equal(t, content, got)
	})

	t.Run("returns empty for missing media file", func(t *testing.T) {
		opts := newOpts(t)
		rel, hash := imp.handleMediaFile(media("nope.jpg"), opts)
		assert.Empty(t, rel)
		assert.Empty(t, hash)
	})

	t.Run("returns empty for oversized media file", func(t *testing.T) {
		opts := newOpts(t)
		opts.MaxMediaFileSize = 4
		rel, hash := imp.handleMediaFile(media("photo.jpg"), opts)
		assert.Empty(t, rel)
		assert.Empty(t, hash)
	})
}
