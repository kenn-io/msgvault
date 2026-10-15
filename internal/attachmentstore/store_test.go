package attachmentstore_test

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
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

func TestOpenLooseCandidates(t *testing.T) {
	for _, tc := range []struct {
		name        string
		canonical   bool
		paths       []string
		index       int
		errIs       error
		errContains string
	}{
		{"canonical", true, nil, -1, nil, ""},
		{"recorded after errors", false, []string{"missing", "../escape", "legacy/file", "legacy/file"}, 2, nil, ""},
		{"first path error", false, []string{"missing", "../escape", "https://example.com/file"}, 0, nil, "escapes attachments directory"},
		{"missing", false, []string{"", "missing", "missing"}, 0, os.ErrNotExist, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			dir := t.TempDir()
			hash := strings.Repeat("a", 64)
			path := filepath.Join(dir, "legacy", "file")
			if tc.canonical {
				path = filepath.Join(dir, "aa", hash)
			}
			require.NoError(os.MkdirAll(filepath.Dir(path), 0o700))
			require.NoError(os.WriteFile(path, []byte("document"), 0o600))
			reader, size, index, selected, err := attachmentstore.OpenLooseCandidates(dir, hash, func() ([]string, error) {
				assert.False(tc.canonical, "canonical bytes avoid recorded path lookup")
				return tc.paths, nil
			})
			if tc.errIs != nil || tc.errContains != "" {
				require.Error(err)
				if tc.errIs != nil {
					require.ErrorIs(err, tc.errIs)
				} else {
					require.ErrorContains(err, tc.errContains)
				}
				return
			}
			require.NoError(err)
			data, readErr := io.ReadAll(reader)
			require.NoError(errors.Join(readErr, reader.Close()))
			assert.Equal("document", string(data))
			assert.Equal(int64(len(data)), size)
			assert.Equal(tc.index, index)
			wantFile, err := os.Stat(path)
			require.NoError(err)
			selectedFile, err := os.Stat(selected)
			require.NoError(err)
			assert.True(os.SameFile(wantFile, selectedFile))
		})
	}
	t.Run("excluded canonical alias", func(t *testing.T) {
		require := require.New(t)
		dir := t.TempDir()
		hash := strings.Repeat("a", 64)
		path := filepath.Join(dir, "aa", hash)
		require.NoError(os.MkdirAll(filepath.Dir(path), 0o700))
		require.NoError(os.WriteFile(path, []byte("document"), 0o600))
		excluded, err := filepath.EvalSymlinks(path)
		require.NoError(err)
		if runtime.GOOS == "windows" {
			excluded = strings.ToUpper(excluded)
		}
		reader, _, _, _, err := attachmentstore.OpenLooseCandidates(dir, hash, func() ([]string, error) {
			return []string{"aa/" + hash}, nil
		}, excluded)
		if reader != nil {
			require.NoError(reader.Close())
		}
		require.ErrorIs(err, os.ErrNotExist)
	})
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

			stream, streamSize, err := blobs.OpenStream(context.Background(), uppercase)
			require.NoError(err)
			streamed, readErr := io.ReadAll(stream)
			require.NoError(errors.Join(readErr, stream.Close()))
			assert.Equal(int64(len(content)), streamSize)
			assert.Equal(content, streamed)

			incomplete, _, err := blobs.OpenStream(context.Background(), uppercase)
			require.NoError(err)
			_, err = incomplete.Read(make([]byte, 1))
			require.NoError(err)
			require.ErrorIs(incomplete.Close(), pack.ErrVerificationIncomplete)

			bounded, boundedSize, err := blobs.ReadBounded(uppercase, int64(len(content)))
			require.NoError(err)
			assert.Equal(int64(len(content)), boundedSize)
			assert.Equal(content, bounded)
		})
	}
}
