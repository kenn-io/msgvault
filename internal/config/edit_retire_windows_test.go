//go:build windows

package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWindowsRetireConfigArtifactLeavesContentPreservingRecoveryFile(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "candidate.toml")
	content := []byte("transaction content")
	writeSecureWindowsTestConfig(t, path, content)
	expected, err := retainWindowsConfigArtifact(path, mustConfigIdentity(t, path))
	require.NoError(err)
	t.Cleanup(func() { require.NoError(expected.Close()) })

	require.NoError(retireWindowsConfigArtifactWithHook(path, expected, nil))
	assert.NoFileExists(path)
	tombstones, err := filepath.Glob(filepath.Join(dir, configRetiredPrefix+"*"))
	require.NoError(err)
	require.Len(tombstones, 1)
	assert.Equal(content, mustReadFile(t, tombstones[0]))
}

func TestWindowsRetireConfigArtifactPreservesExpectedHardlinkAndLateSubstitute(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "candidate.toml")
	secret := []byte("transaction secret")
	substitute := []byte("later writer")
	writeSecureWindowsTestConfig(t, path, secret)
	expected, err := retainWindowsConfigArtifact(path, mustConfigIdentity(t, path))
	require.NoError(err)
	t.Cleanup(func() { require.NoError(expected.Close()) })
	externalHardlink := filepath.Join(dir, "external-hardlink.toml")
	require.NoError(os.Link(path, externalHardlink))
	retainedPath := filepath.Join(dir, "retained-expected")

	err = retireWindowsConfigArtifactWithHook(path, expected, func(path string) error {
		require.NoError(os.Rename(path, retainedPath))
		return os.WriteFile(path, substitute, 0o600)
	})
	require.ErrorIs(err, ErrConfigChanged)
	require.ErrorIs(err, ErrConfigConflict)
	assert.Equal(substitute, mustReadFile(t, path))
	assert.Equal(secret, mustReadFile(t, retainedPath))
	assert.Equal(secret, mustReadFile(t, externalHardlink))
}

func TestWindowsRetireConfigArtifactReportsMovedExpectedEntry(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "candidate.toml")
	content := []byte("transaction content")
	writeSecureWindowsTestConfig(t, path, content)
	expected, err := retainWindowsConfigArtifact(path, mustConfigIdentity(t, path))
	require.NoError(err)
	t.Cleanup(func() { require.NoError(expected.Close()) })
	moved := filepath.Join(dir, "moved-candidate.toml")
	require.NoError(os.Rename(path, moved))

	err = retireWindowsConfigArtifact(path, expected)
	require.ErrorIs(err, ErrConfigChanged)
	require.ErrorIs(err, ErrConfigConflict)
	assert.Equal(content, mustReadFile(t, moved))
}
