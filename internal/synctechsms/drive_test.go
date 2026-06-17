package synctechsms

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDriveImportSkipsUnstableAndAlreadyImportedFiles(t *testing.T) {
	now := time.Date(2026, 5, 22, 4, 30, 0, 0, time.UTC)
	files := []DriveFile{
		{ID: "new", Name: "new.xml", Size: 100, Checksum: "newsum", ModifiedTime: now.Add(-2 * time.Minute)},
		{ID: "old", Name: "old.xml", Size: 100, Checksum: "oldsum", ModifiedTime: now.Add(-30 * time.Minute)},
	}
	got := SelectStableDriveFiles(files, now, 10*time.Minute, map[string]string{"old": "oldsum"})
	assert.Empty(t, got, "stable selection should be empty when only old is already imported")
	got = SelectStableDriveFiles(files, now, 10*time.Minute, map[string]string{})
	require.Len(t, got, 1, "stable selection should pick only old")
	assert.Equal(t, "old", got[0].ID)
}

func TestDriveClientInterfaceIsSmall(t *testing.T) {
	var _ DriveClient = fakeDriveClient{}
}

type fakeDriveClient struct{}

func (fakeDriveClient) ListBackupFiles(context.Context, string) ([]DriveFile, error) { return nil, nil }
func (fakeDriveClient) DownloadToFile(context.Context, string, string) error         { return nil }
