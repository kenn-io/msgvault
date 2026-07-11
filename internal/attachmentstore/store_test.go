package attachmentstore_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/pack"
	"go.kenn.io/kit/packstore"

	"go.kenn.io/msgvault/internal/attachmentstore"
)

type locationResolver map[packstore.Hash]packstore.Location

func (r locationResolver) Resolve(_ context.Context, hash packstore.Hash) (packstore.Location, error) {
	return r[hash], nil
}

func TestUppercaseHashReadsLooseAndPackedContent(t *testing.T) {
	for _, storage := range []string{"loose", "packed"} {
		t.Run(storage, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			root := t.TempDir()
			layout, err := packstore.NewLayout(root, packstore.LayoutOptions{Staging: packstore.StagingSameDirectory})
			require.NoError(err)
			content := []byte("uppercase caller remains compatible across " + storage + " storage")
			id := pack.ComputeBlobID(content)
			hash, err := packstore.ParseHash(id.String())
			require.NoError(err)
			location := packstore.Location{Member: true}
			if storage == "loose" {
				path := layout.LoosePath(hash)
				require.NoError(os.MkdirAll(filepath.Dir(path), 0o700))
				require.NoError(os.WriteFile(path, content, 0o600))
			} else {
				require.NoError(pack.MkdirAllSynced(layout.PacksDir()))
				writer, writerErr := pack.NewWriter(layout.PacksDir(), pack.WriterOptions{})
				require.NoError(writerErr)
				entry, appendErr := writer.Append(content)
				require.NoError(appendErr)
				packID := writer.ID()
				_, sealErr := writer.Seal(layout.PackPath(packID))
				require.NoError(sealErr)
				location.Pack = &packstore.IndexEntry{Hash: hash, PackID: packID,
					Offset: int64(entry.Offset), StoredLen: int64(entry.StoredLen), RawLen: int64(entry.RawLen),
					Flags: uint8(entry.Flags), CRC32C: entry.CRC32C}
			}
			blobs, err := attachmentstore.New(locationResolver{hash: location}, root)
			require.NoError(err)
			t.Cleanup(func() { require.NoError(blobs.Close()) })
			uppercase := strings.ToUpper(hash.String())

			reader, size, err := blobs.Open(uppercase)
			require.NoError(err)
			got := make([]byte, size)
			_, err = reader.Read(got)
			require.NoError(err)
			require.NoError(reader.Close())
			assert.Equal(content, got)

			bounded, boundedSize, err := blobs.ReadBounded(uppercase, int64(len(content)))
			require.NoError(err)
			assert.Equal(int64(len(content)), boundedSize)
			assert.Equal(content, bounded)
		})
	}
}
