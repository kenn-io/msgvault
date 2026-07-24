//go:build windows

package api

import (
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows"
)

// blockSettingsConfigFilesystem makes the settings editor's next write fail
// with a plain filesystem error. Windows ignores directory permission bits,
// so the Unix chmod injection cannot work here; holding an exclusive
// (no-sharing) handle on the config file instead makes the editor's own open
// of the config fail with a sharing violation.
func blockSettingsConfigFilesystem(t *testing.T, path string) {
	t.Helper()
	encoded, err := windows.UTF16PtrFromString(path)
	require.NoError(t, err)
	handle, err := windows.CreateFile(
		encoded,
		windows.GENERIC_READ,
		0,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL,
		0,
	)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, windows.CloseHandle(handle)) })
}
