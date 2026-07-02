package fileutil

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// assertPermNoMoreThan checks that the file at path has permissions no more
// permissive than want. This is umask-tolerant: a umask of 0077 turning 0644
// into 0600 is fine, but 0644 appearing as 0666 would fail.
func assertPermNoMoreThan(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	require.NoError(t, err, "Stat")
	got := info.Mode().Perm()
	assert.Zero(t, got&^want, "perm = %04o, has bits beyond %04o (extra: %04o)", got, want, got&^want)
}

func TestSecureWriteFile(t *testing.T) {
	tests := []struct {
		name string
		perm os.FileMode
	}{
		{"owner_only_0600", 0600},
		{"permissive_0644", 0644},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "testfile")
			data := []byte("hello secure world")

			require.NoError(t, SecureWriteFile(path, data, tt.perm), "SecureWriteFile")

			got, err := os.ReadFile(path)
			require.NoError(t, err, "ReadFile")
			assert.Equal(t, string(data), string(got))

			if runtime.GOOS != "windows" {
				assertPermNoMoreThan(t, path, tt.perm)
			}
		})
	}
}

func TestSecureMkdirAll(t *testing.T) {
	tests := []struct {
		name string
		perm os.FileMode
	}{
		{"owner_only_0700", 0700},
		{"permissive_0755", 0755},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "a", "b", "c")

			require.NoError(t, SecureMkdirAll(path, tt.perm), "SecureMkdirAll")

			info, err := os.Stat(path)
			require.NoError(t, err, "Stat")
			assert.True(t, info.IsDir(), "expected directory")

			if runtime.GOOS != "windows" {
				assertPermNoMoreThan(t, path, tt.perm)
			}
		})
	}
}

func TestSecureChmod(t *testing.T) {
	require := require.New(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "testfile")

	require.NoError(os.WriteFile(path, []byte("data"), 0644), "WriteFile")

	require.NoError(SecureChmod(path, 0600), "SecureChmod")

	if runtime.GOOS != "windows" {
		// Chmod sets exact mode (not subject to umask), so we can assert exactly.
		info, err := os.Stat(path)
		require.NoError(err, "Stat")
		assert.Equal(t, os.FileMode(0600), info.Mode().Perm())
	}
}

func TestSecureOpenFile(t *testing.T) {
	tests := []struct {
		name string
		perm os.FileMode
	}{
		{"owner_only_0600", 0600},
		{"permissive_0644", 0644},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require := require.New(t)
			dir := t.TempDir()
			path := filepath.Join(dir, "testfile")

			f, err := SecureOpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, tt.perm)
			require.NoError(err, "SecureOpenFile")
			if _, err := f.WriteString("data"); err != nil {
				_ = f.Close()
				require.NoError(err, "Write")
			}
			require.NoError(f.Close(), "Close")

			got, err := os.ReadFile(path)
			require.NoError(err, "ReadFile")
			assert.Equal(t, "data", string(got))

			if runtime.GOOS != "windows" {
				assertPermNoMoreThan(t, path, tt.perm)
			}
		})
	}
}

func TestSecureWriteFile_NonexistentParent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "no", "such", "dir", "file")

	err := SecureWriteFile(path, []byte("data"), 0600)
	require.Error(t, err, "expected error for nonexistent parent dir")
}

func TestSecureOpenFile_ReadOnly(t *testing.T) {
	require := require.New(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "testfile")
	require.NoError(os.WriteFile(path, []byte("existing"), 0644), "WriteFile")

	f, err := SecureOpenFile(path, os.O_RDONLY, 0)
	require.NoError(err, "SecureOpenFile read-only")
	defer func() { _ = f.Close() }()

	buf := make([]byte, 100)
	n, err := f.Read(buf)
	require.NoError(err, "Read")
	assert.Equal(t, "existing", string(buf[:n]))
}
