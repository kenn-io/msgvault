//go:build !windows

package api

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// blockSettingsConfigFilesystem makes the settings editor's next write fail
// with a plain filesystem error by removing write permission from the config
// directory.
func blockSettingsConfigFilesystem(t *testing.T, path string) {
	t.Helper()
	dir := filepath.Dir(path)
	require.NoError(t, os.Chmod(dir, 0o500))
	t.Cleanup(func() { require.NoError(t, os.Chmod(dir, 0o700)) })
}
