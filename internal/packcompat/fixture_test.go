package packcompat_test

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/pack"
	"go.kenn.io/kit/packstore"

	"go.kenn.io/msgvault/internal/attachmentstore"
)

type fixtureManifest struct {
	PackID     string        `json:"pack_id"`
	PackFile   string        `json:"pack_file"`
	PackSHA256 string        `json:"pack_sha256"`
	Blobs      []fixtureBlob `json:"blobs"`
}

type fixtureBlob struct {
	Name          string `json:"name"`
	ContentBase64 string `json:"content_base64"`
	Hash          string `json:"hash"`
	Offset        uint64 `json:"offset"`
	StoredLen     uint64 `json:"stored_len"`
	RawLen        uint64 `json:"raw_len"`
	Flags         uint8  `json:"flags"`
	CRC32C        uint32 `json:"crc32c"`
}

func TestFrozenMsgvaultV1Pack(t *testing.T) {
	t.Parallel()
	assert := assert.New(t)
	require := require.New(t)

	fixtureDir := filepath.Join("testdata", "msgvault-v1")
	manifestBytes, err := os.ReadFile(filepath.Join(fixtureDir, "manifest.json"))
	require.NoError(err)

	var manifest fixtureManifest
	require.NoError(json.Unmarshal(manifestBytes, &manifest))
	require.True(pack.IsValidPackID(manifest.PackID))
	require.Equal(manifest.PackID+packstore.PackExt, manifest.PackFile)
	require.Len(manifest.Blobs, 3)

	fixturePackPath := filepath.Join(fixtureDir, manifest.PackFile)
	packBytes, err := os.ReadFile(fixturePackPath)
	require.NoError(err)
	packSum := sha256.Sum256(packBytes)
	assert.Equal(manifest.PackSHA256, hex.EncodeToString(packSum[:]))

	reader, err := pack.OpenReader(fixturePackPath, nil)
	require.NoError(err)
	t.Cleanup(func() { require.NoError(reader.Close()) })
	footerEntries := make(map[string]pack.Entry, len(reader.Entries()))
	for _, entry := range reader.Entries() {
		footerEntries[entry.ID.String()] = entry
	}
	require.Len(footerEntries, len(manifest.Blobs))

	attachmentsDir := t.TempDir()
	finalPath := filepath.Join(attachmentsDir, "packs", manifest.PackID[:2], manifest.PackFile)
	require.NoError(os.MkdirAll(filepath.Dir(finalPath), 0o700))
	require.NoError(os.WriteFile(finalPath, packBytes, 0o600))

	locations := make(crossReadResolver, len(manifest.Blobs))
	for _, blob := range manifest.Blobs {
		entry, ok := footerEntries[blob.Hash]
		require.True(ok, "footer entry for %s", blob.Name)
		assert.Equal(blob.Offset, entry.Offset, blob.Name)
		assert.Equal(blob.StoredLen, entry.StoredLen, blob.Name)
		assert.Equal(blob.RawLen, entry.RawLen, blob.Name)
		assert.Equal(pack.BlobFlags(blob.Flags), entry.Flags, blob.Name)
		assert.Equal(blob.CRC32C, entry.CRC32C, blob.Name)

		hash, err := packstore.ParseHash(blob.Hash)
		require.NoError(err)
		indexEntry := packstore.IndexEntry{
			Hash:      hash,
			PackID:    manifest.PackID,
			Offset:    int64(blob.Offset),
			StoredLen: int64(blob.StoredLen),
			RawLen:    int64(blob.RawLen),
			Flags:     blob.Flags,
			CRC32C:    blob.CRC32C,
		}
		locations[hash] = packstore.Location{Member: true, Pack: &indexEntry}
	}

	blobs, err := attachmentstore.New(locations, attachmentsDir)
	require.NoError(err)
	t.Cleanup(func() { require.NoError(blobs.Close()) })
	for _, blob := range manifest.Blobs {
		want, err := base64.StdEncoding.DecodeString(blob.ContentBase64)
		require.NoError(err, blob.Name)
		sum := sha256.Sum256(want)
		assert.Equal(blob.Hash, hex.EncodeToString(sum[:]), blob.Name)

		r, size, err := blobs.Open(blob.Hash)
		require.NoError(err, blob.Name)
		got, readErr := io.ReadAll(r)
		closeErr := r.Close()
		require.NoError(readErr, blob.Name)
		require.NoError(closeErr, blob.Name)
		assert.Equal(int64(len(want)), size, blob.Name)
		assert.Equal(want, got, blob.Name)
	}
}
