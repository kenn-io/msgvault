//go:build windows

package export

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOpenNoFollowWindowsRetainsFinalReparsePoint(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	require.NoError(os.WriteFile(target, []byte("target"), 0o600))
	link := filepath.Join(dir, "link")
	// Native Windows CI must support creating the reparse point; silently
	// skipping would leave the security property untested.
	require.NoError(os.Symlink(target, link))

	f, err := openNoFollow(link)
	require.NoError(err)
	info, err := f.Stat()
	require.NoError(err)
	require.NoError(f.Close())
	assert.True(info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular(),
		"non-follow handle must describe the reparse point, not its target")
}

func TestOpenNoFollowDurableWindowsCanFlushRegularFile(t *testing.T) {
	require := require.New(t)
	path := filepath.Join(t.TempDir(), "regular")
	require.NoError(os.WriteFile(path, []byte("durable"), 0o600))

	f, err := openNoFollowDurable(path)
	require.NoError(err)
	require.NoError(f.Sync(), "durable handle must include access required by FlushFileBuffers")
	require.NoError(f.Close())
}
