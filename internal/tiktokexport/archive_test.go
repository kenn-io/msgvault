package tiktokexport

import (
	"archive/zip"
	"context"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRejectUnsafeZIP(t *testing.T) {
	for _, name := range []string{"../outside.json", "/absolute.json", `folder\outside.json`} {
		t.Run(name, func(t *testing.T) {
			require := require.New(t)

			file := filepath.Join(t.TempDir(), "archive.zip")
			f, err := os.Create(file)
			require.NoError(err)
			zw := zip.NewWriter(f)
			w, err := zw.Create(name)
			require.NoError(err)
			_, err = w.Write([]byte(`{}`))
			require.NoError(err)
			require.NoError(zw.Close())
			require.NoError(f.Close())
			_, err = readArchive(t.Context(), file)
			require.ErrorContains(err, "unsafe member path")
		})
	}
}

func TestRejectSymlinkAndCancellation(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	dir := t.TempDir()
	fixture, err := filepath.Abs("testdata/json/user_data_tiktok.json")
	require.NoError(err)
	if err := os.Symlink(fixture, filepath.Join(dir, "data.json")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	_, err = readArchive(t.Context(), dir)
	require.ErrorContains(err, "symbolic link")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = readArchive(ctx, "testdata/json")
	assert.ErrorIs(err, context.Canceled)
}

func TestRejectDuplicateAndOversizedZIPMembers(t *testing.T) {
	for _, oversized := range []bool{false, true} {
		t.Run(strconv.FormatBool(oversized), func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)

			file := filepath.Join(t.TempDir(), "archive.zip")
			f, err := os.Create(file)
			require.NoError(err)
			zw := zip.NewWriter(f)
			if oversized {
				_, err = zw.CreateRaw(&zip.FileHeader{Name: "data.json", Method: zip.Store, UncompressedSize64: uint64(maxFileBytes + 1)})
				require.NoError(err)
			} else {
				_, err = zw.Create("data.json")
				require.NoError(err)
				_, err = zw.Create("data.json")
				require.NoError(err)
			}
			require.NoError(zw.Close())
			require.NoError(f.Close())
			_, err = readArchive(t.Context(), file)
			if oversized {
				assert.ErrorContains(err, "64 MiB")
			} else {
				assert.ErrorContains(err, "duplicate")
			}
		})
	}
}
