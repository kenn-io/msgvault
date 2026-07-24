//go:build darwin || linux

package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

func TestRetireConfigArtifactLeavesContentPreservingRecoveryFile(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "candidate.toml")
	content := []byte("transaction content")
	require.NoError(os.WriteFile(path, content, 0o600))
	dirfd, err := unix.Open(dir, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_DIRECTORY, 0)
	require.NoError(err)
	t.Cleanup(func() { require.NoError(unix.Close(dirfd)) })
	expected, err := retainConfigEntryAt(dirfd, filepath.Base(path), path, mustConfigIdentity(t, path))
	require.NoError(err)
	t.Cleanup(func() { require.NoError(expected.Close()) })

	require.NoError(retireConfigArtifactAtWithHook(dirfd, filepath.Base(path), path, expected, nil))
	assert.NoFileExists(path)
	tombstones, err := filepath.Glob(filepath.Join(dir, configRetiredPrefix+"*"))
	require.NoError(err)
	require.Len(tombstones, 1)
	assert.Equal(content, mustReadFile(t, tombstones[0]))
}

func TestRetireConfigArtifactPreservesExpectedHardlinkAndLateSubstitute(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "candidate.toml")
	secret := []byte("transaction secret")
	substitute := []byte("later writer")
	require.NoError(os.WriteFile(path, secret, 0o600))
	dirfd, err := unix.Open(dir, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_DIRECTORY, 0)
	require.NoError(err)
	t.Cleanup(func() { require.NoError(unix.Close(dirfd)) })
	expected, err := retainConfigEntryAt(dirfd, filepath.Base(path), path, mustConfigIdentity(t, path))
	require.NoError(err)
	t.Cleanup(func() { require.NoError(expected.Close()) })
	externalHardlink := filepath.Join(dir, "external-hardlink.toml")
	require.NoError(os.Link(path, externalHardlink))
	retainedPath := filepath.Join(dir, "retained-expected")

	err = retireConfigArtifactAtWithHook(dirfd, filepath.Base(path), path, expected, func(path string) error {
		require.NoError(os.Rename(path, retainedPath))
		return os.WriteFile(path, substitute, 0o600)
	})
	require.ErrorIs(err, ErrConfigChanged)
	require.ErrorIs(err, ErrConfigConflict)
	assert.Equal(substitute, mustReadFile(t, path))
	assert.Equal(secret, mustReadFile(t, retainedPath))
	assert.Equal(secret, mustReadFile(t, externalHardlink))
}

func TestRetireConfigArtifactReportsMovedExpectedEntry(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "candidate.toml")
	content := []byte("transaction content")
	require.NoError(os.WriteFile(path, content, 0o600))
	dirfd, err := unix.Open(dir, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_DIRECTORY, 0)
	require.NoError(err)
	t.Cleanup(func() { require.NoError(unix.Close(dirfd)) })
	expected, err := retainConfigEntryAt(dirfd, filepath.Base(path), path, mustConfigIdentity(t, path))
	require.NoError(err)
	t.Cleanup(func() { require.NoError(expected.Close()) })
	moved := filepath.Join(dir, "moved-candidate.toml")
	require.NoError(os.Rename(path, moved))

	err = retireConfigArtifactAt(dirfd, filepath.Base(path), path, expected)
	require.ErrorIs(err, ErrConfigChanged)
	require.ErrorIs(err, ErrConfigConflict)
	assert.Equal(content, mustReadFile(t, moved))
}
