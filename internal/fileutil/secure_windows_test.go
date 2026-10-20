//go:build windows

package fileutil

import (
	"os"
	"path/filepath"
	"testing"
	"unsafe"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows"
)

func TestSecureMkdirAllProtectedDirectories(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	root := t.TempDir()
	path := filepath.Join(root, "a", "b", "tokens")
	require.NoError(SecureMkdirAll(path, 0o700))
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	require.NoError(err)
	for _, dir := range []string{filepath.Join(root, "a"), filepath.Join(root, "a", "b"), path} {
		descriptor, err := windows.GetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
		require.NoError(err)
		control, _, err := descriptor.Control()
		require.NoError(err)
		assert.NotZero(control & windows.SE_DACL_PROTECTED)
		dacl, _, err := descriptor.DACL()
		require.NoError(err)
		require.NotNil(dacl)
		require.Equal(uint16(1), dacl.AceCount)
		var ace *windows.ACCESS_ALLOWED_ACE
		require.NoError(windows.GetAce(dacl, 0, &ace))
		assert.Equal(uint8(windows.ACCESS_ALLOWED_ACE_TYPE), ace.Header.AceType)
		assert.Zero(ace.Header.AceFlags & windows.INHERITED_ACE)
		assert.Equal(uint32(0x1F01FF), ace.Mask, "FILE_ALL_ACCESS")
		// #nosec G103 -- GetAce supplies an access-allowed ACE with a contiguous SID at SidStart.
		aceSID := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		assert.True(aceSID.Equals(user.User.Sid))
		require.NoError(os.WriteFile(filepath.Join(dir, "secret"), []byte("test-secret"), 0o600))
	}
}

func TestSecureMkdirAllConcurrentCreation(t *testing.T) {
	require := require.New(t)
	path := filepath.Join(t.TempDir(), "a", "b", "tokens")
	const creators = 8
	start := make(chan struct{})
	results := make(chan error, creators)
	for range creators {
		go func() {
			<-start
			results <- SecureMkdirAll(path, 0o700)
		}()
	}
	close(start)
	for range creators {
		require.NoError(<-results)
	}
	info, err := os.Stat(path)
	require.NoError(err)
	require.True(info.IsDir())
}

func TestSecureMkdirAllRejectsFileComponent(t *testing.T) {
	require := require.New(t)
	path := filepath.Join(t.TempDir(), "file")
	require.NoError(os.WriteFile(path, nil, 0o600))
	for _, dir := range []string{path, filepath.Join(path, "tokens")} {
		require.Error(SecureMkdirAll(dir, 0o700))
	}
}
