package testutil

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/stretchr/testify/require"
)

// ArchiveEntry describes a single entry in a tar.gz archive for testing.
type ArchiveEntry struct {
	Name     string
	Content  string
	TypeFlag byte
	LinkName string
	Mode     int64
}

// CreateTarGz creates a tar.gz archive at path containing the given entries.
func CreateTarGz(t *testing.T, path string, entries []ArchiveEntry) {
	t.Helper()
	f, err := os.Create(path)
	require.NoError(t, err)
	defer func() { _ = f.Close() }()

	gzw := gzip.NewWriter(f)
	defer func() { _ = gzw.Close() }()
	tw := tar.NewWriter(gzw)
	defer func() { _ = tw.Close() }()

	for _, e := range entries {
		mode := e.Mode
		if mode == 0 {
			mode = 0644
		}
		h := &tar.Header{
			Name:     e.Name,
			Mode:     mode,
			Size:     int64(len(e.Content)),
			Typeflag: e.TypeFlag,
			Linkname: e.LinkName,
		}
		require.NoError(t, tw.WriteHeader(h))
		if len(e.Content) > 0 {
			_, err := tw.Write([]byte(e.Content))
			require.NoError(t, err)
		}
	}
}

// CreateZip creates a zip archive at path containing the given entries.
func CreateZip(t *testing.T, path string, entries []ArchiveEntry) {
	t.Helper()
	f, err := os.Create(path)
	require.NoError(t, err)
	defer func() { _ = f.Close() }()

	w := zip.NewWriter(f)
	for _, e := range entries {
		fw, err := w.Create(e.Name)
		require.NoErrorf(t, err, "create zip entry %s", e.Name)
		if len(e.Content) > 0 {
			_, err := fw.Write([]byte(e.Content))
			require.NoErrorf(t, err, "write zip entry %s", e.Name)
		}
	}
	require.NoError(t, w.Close(), "close zip writer")
}

// CreateTempZip creates a zip file in a temporary directory containing the
// provided entries (filename -> content). Returns the path to the zip file.
func CreateTempZip(t *testing.T, entries map[string]string) string {
	t.Helper()

	zipPath := filepath.Join(t.TempDir(), "test.zip")
	f, err := os.Create(zipPath)
	require.NoError(t, err, "create zip file")
	defer func() { _ = f.Close() }()

	w := zip.NewWriter(f)
	keys := make([]string, 0, len(entries))
	for name := range entries {
		keys = append(keys, name)
	}
	sort.Strings(keys)
	for _, name := range keys {
		content := entries[name]
		fw, err := w.Create(name)
		require.NoErrorf(t, err, "create zip entry %s", name)
		_, err = fw.Write([]byte(content))
		require.NoErrorf(t, err, "write zip entry %s", name)
	}
	require.NoError(t, w.Close(), "close zip writer")

	return zipPath
}
