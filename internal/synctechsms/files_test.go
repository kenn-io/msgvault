package synctechsms

import (
	"archive/zip"
	"os"
	"path/filepath"
	"testing"

	assertpkg "github.com/stretchr/testify/assert"
	requirepkg "github.com/stretchr/testify/require"
)

func TestDiscoverBackupFilesFromDirectory(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "sms-2024.xml"), `<smses count="0"></smses>`)
	writeFile(t, filepath.Join(dir, "calls-2024.xml"), `<calls count="0"></calls>`)
	writeFile(t, filepath.Join(dir, "notes.txt"), `ignore`)

	files, err := DiscoverBackupFiles(dir)
	require.NoError(err, "DiscoverBackupFiles")
	require.Len(files, 2, "files: %#v", files)
	assert.Equal(KindCalls, files[0].Kind, "files sorted/classified incorrectly: %#v", files)
	assert.Equal(KindMessages, files[1].Kind, "files sorted/classified incorrectly: %#v", files)
}

func TestDiscoverBackupFilesFromZip(t *testing.T) {
	zipPath := filepath.Join(t.TempDir(), "backup.zip")
	createZip(t, zipPath, map[string]string{
		"SMS.xml":   `<smses count="0"></smses>`,
		"Calls.xml": `<calls count="0"></calls>`,
	})
	files, err := DiscoverBackupFiles(zipPath)
	requirepkg.NoError(t, err, "DiscoverBackupFiles")
	requirepkg.Len(t, files, 2)
	assertpkg.NotNil(t, files[0].Opener, "zip file opener is nil")
}

func TestDiscoverRejectsEncryptedZip(t *testing.T) {
	require := requirepkg.New(t)
	zipPath := filepath.Join(t.TempDir(), "encrypted.zip")
	f, err := os.Create(zipPath)
	require.NoError(err, "create zip")
	zw := zip.NewWriter(f)
	w, err := zw.CreateHeader(&zip.FileHeader{Name: "sms.xml", Method: zip.Store, Flags: 0x1})
	require.NoError(err, "create encrypted zip entry")
	_, err = w.Write([]byte(`<smses count="0"></smses>`))
	require.NoError(err, "write encrypted zip entry")
	require.NoError(zw.Close(), "close zip")
	require.NoError(f.Close(), "close file")

	_, err = DiscoverBackupFiles(zipPath)
	require.ErrorIs(err, ErrEncryptedBackup)
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	requirepkg.NoError(t, os.WriteFile(path, []byte(body), 0o600), "write %s", path)
}

func createZip(t *testing.T, path string, entries map[string]string) {
	t.Helper()
	f, err := os.Create(path)
	requirepkg.NoError(t, err, "create zip")
	defer func() { _ = f.Close() }()
	zw := zip.NewWriter(f)
	for name, body := range entries {
		w, err := zw.Create(name)
		requirepkg.NoError(t, err, "create zip entry")
		_, err = w.Write([]byte(body))
		requirepkg.NoError(t, err, "write zip entry")
	}
	requirepkg.NoError(t, zw.Close(), "close zip")
}
