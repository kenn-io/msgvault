//go:build windows

package fileutil

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unsafe"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows"
)

func TestSecureMkdirAllProtectedDirectories(t *testing.T) {
	require := require.New(t)
	root := t.TempDir()
	path := filepath.Join(root, "a", "b", "tokens")
	require.NoError(SecureMkdirAll(path, 0o700))
	for _, dir := range []string{filepath.Join(root, "a"), filepath.Join(root, "a", "b"), path} {
		assertProtectedDirectory(t, dir)
		require.NoError(os.WriteFile(filepath.Join(dir, "secret"), []byte("test-secret"), 0o600))
	}
}

func assertProtectedDirectory(t *testing.T, path string) {
	t.Helper()
	require := require.New(t)
	assert := assert.New(t)
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	require.NoError(err)
	descriptor, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
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
	assert.Equal(uint32(FileAllAccess), uint32(ace.Mask))
	// #nosec G103 -- GetAce supplies an access-allowed ACE with a contiguous SID at SidStart.
	aceSID := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
	assert.True(aceSID.Equals(user.User.Sid))
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

func TestWin32CreatePath(t *testing.T) {
	drive247 := `C:\` + strings.Repeat("a", 244)
	drive248 := `C:\` + strings.Repeat("a", 245)
	unc247 := `\\server\share\` + strings.Repeat("a", 232)
	unc248 := `\\server\share\` + strings.Repeat("a", 233)
	tests := []struct {
		name   string
		dir    string
		length int
		want   string
	}{
		{"drive at 247", drive247, 247, drive247},
		{"drive at 248", drive248, 248, `\\?\C:\` + strings.Repeat("a", 245)},
		{"UNC at 248", unc248, 248, `\\?\UNC\server\share\` + strings.Repeat("a", 233)},
		{"UNC at 247", unc247, 247, unc247},
		{"extended prefix", `\\?\` + drive248, 252, `\\?\` + drive248},
		{"device prefix", `\\.\` + drive248, 252, `\\.\` + drive248},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Len(t, tt.dir, tt.length)
			assert.Equal(t, tt.want, win32CreatePath(tt.dir))
		})
	}
}

func TestSecureMkdirAllLongPath(t *testing.T) {
	require := require.New(t)
	root, err := filepath.Abs(t.TempDir())
	require.NoError(err)
	path := filepath.Join(root, strings.Repeat("a", 100), strings.Repeat("b", 100), strings.Repeat("c", 100))
	require.Greater(len(path), 260)
	require.NoError(SecureMkdirAll(path, 0o700))
	require.DirExists(path)
	if strings.HasPrefix(path, `\\`) {
		assertProtectedDirectory(t, `\\?\UNC\`+path[2:])
	} else {
		assertProtectedDirectory(t, `\\?\`+path)
	}
}

func TestSecureMkdirAllTrailingDotMatchesLaterAccess(t *testing.T) {
	require := require.New(t)
	dir := filepath.Join(t.TempDir(), "out.")
	require.NoError(SecureMkdirAll(dir, 0o700))
	require.NoError(os.WriteFile(filepath.Join(dir, "message.eml"), []byte("x"), 0o600))
}

func TestSecureMkdirAllExtendedPathTrailingDot(t *testing.T) {
	require := require.New(t)
	path := `\\?\` + filepath.Join(t.TempDir(), "out.")
	require.NoError(SecureMkdirAll(path, 0o700))
	require.DirExists(path)
	assertProtectedDirectory(t, path)
}

func TestSecureMkdirAllRelativePathUnderLongWorkingDirectory(t *testing.T) {
	require := require.New(t)
	long := filepath.Join(t.TempDir(), strings.Repeat("w", 100), strings.Repeat("x", 100))
	require.NoError(SecureMkdirAll(long, 0o700))
	t.Chdir(long)
	require.NoError(SecureMkdirAll(filepath.Join("a", "b"), 0o700))
	assertProtectedDirectory(t, filepath.Join(long, "a", "b"))
}

func TestSecureMkdirAllRejectsFileComponent(t *testing.T) {
	require := require.New(t)
	path := filepath.Join(t.TempDir(), "file")
	require.NoError(os.WriteFile(path, nil, 0o600))
	for _, dir := range []string{path, filepath.Join(path, "tokens")} {
		require.Error(SecureMkdirAll(dir, 0o700))
	}
}
