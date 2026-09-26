package fileutil

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSecureReplaceFileReplacesContentWithRequestedPerm(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	dir := t.TempDir()
	path := filepath.Join(dir, "token.json")
	require.NoError(os.WriteFile(path, []byte("old"), 0o644))
	require.NoError(os.Chmod(path, 0o644))

	require.NoError(SecureReplaceFile(path, []byte("new"), 0o600))

	got, err := os.ReadFile(path)
	require.NoError(err)
	assert.Equal("new", string(got))
	entries, err := os.ReadDir(dir)
	require.NoError(err)
	require.Len(entries, 1, "the staged file must not be left behind")
	assert.Equal("token.json", entries[0].Name())
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		require.NoError(err)
		assert.Equal(os.FileMode(0o600), info.Mode().Perm())
	}
}
