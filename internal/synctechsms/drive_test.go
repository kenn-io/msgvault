package synctechsms

import (
	"context"
	"testing"
	"time"
)

func TestDriveImportSkipsUnstableAndAlreadyImportedFiles(t *testing.T) {
	now := time.Date(2026, 5, 22, 4, 30, 0, 0, time.UTC)
	files := []DriveFile{
		{ID: "new", Name: "new.xml", Size: 100, Checksum: "newsum", ModifiedTime: now.Add(-2 * time.Minute)},
		{ID: "old", Name: "old.xml", Size: 100, Checksum: "oldsum", ModifiedTime: now.Add(-30 * time.Minute)},
	}
	got := SelectStableDriveFiles(files, now, 10*time.Minute, map[string]string{"old": "oldsum"})
	if len(got) != 0 {
		t.Fatalf("stable selection = %#v, want none", got)
	}
	got = SelectStableDriveFiles(files, now, 10*time.Minute, map[string]string{})
	if len(got) != 1 || got[0].ID != "old" {
		t.Fatalf("stable selection = %#v, want old only", got)
	}
}

func TestDriveClientInterfaceIsSmall(t *testing.T) {
	var _ DriveClient = fakeDriveClient{}
}

type fakeDriveClient struct{}

func (fakeDriveClient) ListBackupFiles(context.Context, string) ([]DriveFile, error) { return nil, nil }
func (fakeDriveClient) DownloadToFile(context.Context, string, string) error         { return nil }
