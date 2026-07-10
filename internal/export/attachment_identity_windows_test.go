//go:build windows

package export

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSnapshotAttachmentPathIdentityCoexistsWithOpenValidator(t *testing.T) {
	require := require.New(t)
	path := filepath.Join(t.TempDir(), "attachment")
	require.NoError(os.WriteFile(path, []byte("content"), 0o600))

	validator, err := openNoFollow(path)
	require.NoError(err)
	t.Cleanup(func() { require.NoError(validator.Close()) })

	info, err := snapshotAttachmentPathIdentity(path)
	require.NoError(err, "identity snapshot must share access with an open validator")
	descriptorInfo, err := validator.Stat()
	require.NoError(err)
	require.True(os.SameFile(info, descriptorInfo))
}

func TestSnapshotAttachmentPathIdentityReportsFinalReparsePoint(t *testing.T) {
	require := require.New(t)
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	require.NoError(os.WriteFile(target, []byte("target"), 0o600))
	link := filepath.Join(dir, "link")
	// Native Windows CI must support creating the reparse point; silently
	// skipping would leave the security property untested.
	require.NoError(os.Symlink(target, link))

	info, err := snapshotAttachmentPathIdentity(link)
	require.NoError(err)
	require.ErrorContains(validateNoFollowFileInfo(info), "reparse point")
}
