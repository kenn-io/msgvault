package tiktokexport

import (
	"archive/zip"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRejectUnsafeZIP(t *testing.T) {
	for _, name := range []string{"../outside.json", "/absolute.json", `folder\outside.json`} {
		t.Run(name, func(t *testing.T) {
			file := filepath.Join(t.TempDir(), "archive.zip")
			f, err := os.Create(file)
			require.NoError(t, err)
			zw := zip.NewWriter(f)
			w, err := zw.Create(name)
			require.NoError(t, err)
			_, err = w.Write([]byte(`{}`))
			require.NoError(t, err)
			require.NoError(t, zw.Close())
			require.NoError(t, f.Close())
			_, err = readArchive(t.Context(), file)
			require.ErrorContains(t, err, "unsafe member path")
		})
	}
}

func TestRejectSymlinkAndCancellation(t *testing.T) {
	dir := t.TempDir()
	fixture, err := filepath.Abs("testdata/json/user_data_tiktok.json")
	require.NoError(t, err)
	if err := os.Symlink(fixture, filepath.Join(dir, "data.json")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	_, err = readArchive(t.Context(), dir)
	require.ErrorContains(t, err, "symbolic link")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = readArchive(ctx, "testdata/json")
	assert.ErrorIs(t, err, context.Canceled)
}

func TestRejectDuplicateAndOversizedZIPMembers(t *testing.T) {
	for _, oversized := range []bool{false, true} {
		t.Run(fmt.Sprint(oversized), func(t *testing.T) {
			file := filepath.Join(t.TempDir(), "archive.zip")
			f, err := os.Create(file)
			require.NoError(t, err)
			zw := zip.NewWriter(f)
			if oversized {
				_, err = zw.CreateRaw(&zip.FileHeader{Name: "data.json", Method: zip.Store, UncompressedSize64: uint64(maxFileBytes + 1)})
				require.NoError(t, err)
			} else {
				_, err = zw.Create("data.json")
				require.NoError(t, err)
				_, err = zw.Create("data.json")
				require.NoError(t, err)
			}
			require.NoError(t, zw.Close())
			require.NoError(t, f.Close())
			_, err = readArchive(t.Context(), file)
			if oversized {
				assert.ErrorContains(t, err, "64 MiB")
			} else {
				assert.ErrorContains(t, err, "duplicate")
			}
		})
	}
}
