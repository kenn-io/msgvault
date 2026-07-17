package packcompat_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/packstore"

	"go.kenn.io/msgvault/internal/attachmentstore"
)

func TestCrossReadKitStoreReadsFrozenMsgvaultPack(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	fixtureDir := filepath.Join("testdata", "msgvault-v1")
	manifestBytes, err := os.ReadFile(filepath.Join(fixtureDir, "manifest.json"))
	require.NoError(err)
	var manifest fixtureManifest
	require.NoError(json.Unmarshal(manifestBytes, &manifest))
	packBytes, err := os.ReadFile(filepath.Join(fixtureDir, manifest.PackFile))
	require.NoError(err)
	dir := t.TempDir()
	final := filepath.Join(dir, "packs", manifest.PackID[:2], manifest.PackFile)
	require.NoError(os.MkdirAll(filepath.Dir(final), 0o700))
	require.NoError(os.WriteFile(final, packBytes, 0o600))
	locations := make(map[packstore.Hash]packstore.Location, len(manifest.Blobs))
	for _, blob := range manifest.Blobs {
		hash, parseErr := packstore.ParseHash(blob.Hash)
		require.NoError(parseErr)
		entry := packstore.IndexEntry{Hash: hash, PackID: manifest.PackID,
			Offset: int64(blob.Offset), StoredLen: int64(blob.StoredLen), RawLen: int64(blob.RawLen),
			Flags: blob.Flags, CRC32C: blob.CRC32C}
		locations[hash] = packstore.Location{Member: true, Pack: &entry}
	}
	blobs, err := attachmentstore.New(crossReadResolver(locations), dir)
	require.NoError(err)
	t.Cleanup(func() { require.NoError(blobs.Close()) })
	for _, blob := range manifest.Blobs {
		want, decodeErr := base64.StdEncoding.DecodeString(blob.ContentBase64)
		require.NoError(decodeErr)
		reader, size, openErr := blobs.Open(blob.Hash)
		require.NoError(openErr)
		got, readErr := io.ReadAll(reader)
		require.NoError(readErr)
		require.NoError(reader.Close())
		assert.Equal(int64(len(want)), size)
		assert.Equal(want, got)
	}
}

type crossReadResolver map[packstore.Hash]packstore.Location

func (r crossReadResolver) Resolve(_ context.Context, hash packstore.Hash) (packstore.Location, error) {
	return r[hash], nil
}
