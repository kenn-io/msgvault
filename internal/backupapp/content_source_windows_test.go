//go:build windows

package backupapp_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/packstore"
	"golang.org/x/sys/windows"
)

func TestRepackRetriesExternalWindowsSharingViolation(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newVaultFixture(t)
	_, _, oldPackID := makeSparsePackedVault(t, f)
	oldPath := filepath.Join(f.attDir, "packs", oldPackID[:2], oldPackID+packstore.PackExt)
	name, err := windows.UTF16PtrFromString(oldPath)
	require.NoError(err)
	handle, err := windows.CreateFile(name, windows.GENERIC_READ,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL, 0)
	require.NoError(err)
	closed := false
	t.Cleanup(func() {
		if !closed {
			require.NoError(windows.CloseHandle(handle))
		}
	})

	_, err = f.maint.Repack(context.Background(), packstore.RepackOptions{})
	require.Error(err, "an external handle without delete sharing must defer physical cleanup")
	has, hasErr := f.store.HasPackRecord(oldPackID)
	require.NoError(hasErr)
	assert.False(has, "the committed mapping swap removes stale catalog authority before physical cleanup")
	assert.FileExists(oldPath)
	require.NoError(windows.CloseHandle(handle))
	closed = true

	repairStats, err := f.maint.Pack(context.Background(), packstore.PackOptions{})
	require.NoError(err)
	assert.Equal(1, repairStats.PacksRemoved)
	assert.NoFileExists(oldPath)
}
