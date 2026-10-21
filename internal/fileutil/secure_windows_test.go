//go:build windows

package fileutil

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unsafe"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows"
)

func TestSecureMkdirAllProtectedAtCreation(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "a", "b", "tokens")
	var logs bytes.Buffer
	logger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(logger) })

	original := createDirectory
	t.Cleanup(func() { createDirectory = original })
	var calls []string
	createDirectory = func(path16 *uint16, attributes *windows.SecurityAttributes) error {
		err := windows.CreateDirectory(path16, attributes)
		require.NoError(t, err)
		dir := windows.UTF16PtrToString(path16)
		if rest, ok := strings.CutPrefix(dir, `\\?\UNC\`); ok {
			dir = `\\` + rest
		} else {
			dir = strings.TrimPrefix(dir, `\\?\`)
		}
		assertProtectedDirectory(t, dir)
		calls = append(calls, dir)
		return nil
	}

	require.NoError(t, SecureMkdirAll(path, 0o700))
	assert.Equal(t, []string{filepath.Join(root, "a"), filepath.Join(root, "a", "b"), path}, calls)
	assert.Empty(t, logs.String(), "protected creation must succeed without falling back")
}

func TestSecureMkdirAllFallback(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "a", "b", "tokens")
	var logs bytes.Buffer
	logger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(logger) })

	original := createDirectory
	t.Cleanup(func() { createDirectory = original })
	createDirectory = func(_ *uint16, _ *windows.SecurityAttributes) error {
		return windows.ERROR_ACCESS_DENIED
	}

	require.NoError(t, SecureMkdirAll(path, 0o700))
	for _, dir := range []string{filepath.Join(root, "a"), filepath.Join(root, "a", "b"), path} {
		assert.DirExists(t, dir)
		assertProtectedDirectory(t, dir)
	}
	assert.Contains(t, logs.String(), "level=WARN")
	assert.Contains(t, logs.String(), "protected directory creation failed")
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

func TestSecureMkdirAllPathForms(t *testing.T) {
	tests := []struct {
		name      string
		prepare   func(*testing.T) (string, string)
		writeFile bool
	}{
		{
			name: "long path",
			prepare: func(t *testing.T) (string, string) {
				t.Helper()
				root, err := filepath.Abs(t.TempDir())
				require.NoError(t, err)
				path := filepath.Join(root, strings.Repeat("a", 100), strings.Repeat("b", 100), strings.Repeat("c", 100))
				require.Greater(t, len(path), 260)
				return path, `\\?\` + path
			},
		},
		{
			name: "ordinary trailing dot",
			prepare: func(t *testing.T) (string, string) {
				t.Helper()
				path := filepath.Join(t.TempDir(), "out.")
				return path, path
			},
			writeFile: true,
		},
		{
			name: "extended path trailing dot",
			prepare: func(t *testing.T) (string, string) {
				t.Helper()
				path := `\\?\` + filepath.Join(t.TempDir(), "out.")
				return path, path
			},
		},
		{
			name: "relative path under long working directory",
			prepare: func(t *testing.T) (string, string) {
				t.Helper()
				long := filepath.Join(t.TempDir(), strings.Repeat("w", 100), strings.Repeat("x", 100))
				require.NoError(t, SecureMkdirAll(long, 0o700))
				t.Chdir(long)
				return filepath.Join("a", "b"), filepath.Join(long, "a", "b")
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path, protectedPath := tt.prepare(t)
			require.NoError(t, SecureMkdirAll(path, 0o700))
			if tt.writeFile {
				require.NoError(t, os.WriteFile(filepath.Join(path, "message.eml"), []byte("x"), 0o600))
			}
			assertProtectedDirectory(t, protectedPath)
		})
	}
}

func TestSecureMkdirAllRejectsFileComponent(t *testing.T) {
	require := require.New(t)
	path := filepath.Join(t.TempDir(), "file")
	require.NoError(os.WriteFile(path, nil, 0o600))
	for _, dir := range []string{path, filepath.Join(path, "tokens")} {
		require.Error(SecureMkdirAll(dir, 0o700))
	}
}
