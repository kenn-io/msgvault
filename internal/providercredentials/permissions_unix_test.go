//go:build !windows

package providercredentials

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPrepareDirectoryRestoresWriteAccessBeforeLocking(t *testing.T) {
	require := require.New(t)
	dir := filepath.Join(t.TempDir(), "tokens")
	require.NoError(os.Mkdir(dir, 0o700))
	require.NoError(os.Chmod(dir, 0o500))
	t.Cleanup(func() { require.NoError(os.Chmod(dir, 0o700)) })

	require.NoError((nativePermissions{}).prepareDirectory(dir))
	info, err := os.Stat(dir)
	require.NoError(err)
	assert.Equal(t, os.FileMode(0o700), info.Mode().Perm())
	_, err = EnsureServerKey(dir)
	require.NoError(err)
}
