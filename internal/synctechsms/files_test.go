package synctechsms

import (
	"archive/zip"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestDiscoverBackupFilesFromDirectory(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "sms-2024.xml"), `<smses count="0"></smses>`)
	writeFile(t, filepath.Join(dir, "calls-2024.xml"), `<calls count="0"></calls>`)
	writeFile(t, filepath.Join(dir, "notes.txt"), `ignore`)

	files, err := DiscoverBackupFiles(dir)
	if err != nil {
		t.Fatalf("DiscoverBackupFiles: %v", err)
	}
	if len(files) != 2 {
		t.Fatalf("len(files) = %d, want 2: %#v", len(files), files)
	}
	if files[0].Kind != KindCalls || files[1].Kind != KindMessages {
		t.Fatalf("files sorted/classified incorrectly: %#v", files)
	}
}

func TestDiscoverBackupFilesFromZip(t *testing.T) {
	zipPath := filepath.Join(t.TempDir(), "backup.zip")
	createZip(t, zipPath, map[string]string{
		"SMS.xml":   `<smses count="0"></smses>`,
		"Calls.xml": `<calls count="0"></calls>`,
	})
	files, err := DiscoverBackupFiles(zipPath)
	if err != nil {
		t.Fatalf("DiscoverBackupFiles: %v", err)
	}
	if len(files) != 2 {
		t.Fatalf("len(files) = %d, want 2", len(files))
	}
	if files[0].Opener == nil {
		t.Fatal("zip file opener is nil")
	}
}

func TestDiscoverRejectsEncryptedZip(t *testing.T) {
	zipPath := filepath.Join(t.TempDir(), "encrypted.zip")
	f, err := os.Create(zipPath)
	if err != nil {
		t.Fatalf("create zip: %v", err)
	}
	zw := zip.NewWriter(f)
	w, err := zw.CreateHeader(&zip.FileHeader{Name: "sms.xml", Method: zip.Store, Flags: 0x1})
	if err != nil {
		t.Fatalf("create encrypted zip entry: %v", err)
	}
	if _, err := w.Write([]byte(`<smses count="0"></smses>`)); err != nil {
		t.Fatalf("write encrypted zip entry: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close zip: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close file: %v", err)
	}

	_, err = DiscoverBackupFiles(zipPath)
	if !errors.Is(err, ErrEncryptedBackup) {
		t.Fatalf("DiscoverBackupFiles error = %v, want ErrEncryptedBackup", err)
	}
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func createZip(t *testing.T, path string, entries map[string]string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create zip: %v", err)
	}
	defer f.Close()
	zw := zip.NewWriter(f)
	for name, body := range entries {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatalf("create zip entry: %v", err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatalf("write zip entry: %v", err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close zip: %v", err)
	}
}
