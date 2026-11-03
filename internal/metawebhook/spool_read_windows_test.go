//go:build windows

package metawebhook

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows"
)

func TestReadStatsToleratesLockedDeliveryMetadata(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	s, _ := testSpool(t)
	id := accept(t, s, "synthetic")
	path, err := windows.UTF16PtrFromString(statePath(s.path("9", StatePending, id)))
	require.NoError(err)
	// An external handle without sharing reproduces the transient Windows error.
	handle, err := windows.CreateFile(path, windows.GENERIC_READ, 0, nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	require.NoError(err)
	t.Cleanup(func() { _ = windows.CloseHandle(handle) })
	var meta DeliveryMeta
	err = readJSON(statePath(s.path("9", StatePending, id)), &meta)
	require.ErrorIs(err, windows.ERROR_SHARING_VIOLATION)
	stats, err := ReadStats(s.dir)
	require.NoError(err)
	assert.Zero(stats["9"].Pending)
}
