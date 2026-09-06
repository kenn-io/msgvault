//go:build windows

package config

import (
	"io/fs"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSameConfigModePermComparesObservableWindowsMode pins the only mode
// information a Windows stat can report: Go derives permission bits from the
// read-only attribute (0666 writable, 0444 read-only) and can never observe
// the 0600 that config creation requests. Mode comparison must therefore
// treat every writable mode as equivalent and still flag read-only drift;
// the owner-only DACL is verified separately by validateOpenedConfigSecurity.
func TestSameConfigModePermComparesObservableWindowsMode(t *testing.T) {
	assert.True(t, sameConfigModePerm(fs.FileMode(0o600), fs.FileMode(0o666)),
		"creation requests 0600 while Windows observes 0666 for the same writable file")
	assert.True(t, sameConfigModePerm(fs.FileMode(0o666), fs.FileMode(0o600)))
	assert.False(t, sameConfigModePerm(fs.FileMode(0o600), fs.FileMode(0o444)),
		"a read-only flip must still be detected")
	assert.False(t, sameConfigModePerm(fs.FileMode(0o444), fs.FileMode(0o666)))
}

// TestEditConfigCreatesMissingConfigOnWindows reproduces the creation path
// that failed CI with "committed config identity changed before return":
// the final verification compared the creation-pinned 0600 mode with the
// 0666 Windows stat of the published file, so every config creation rejected
// its own publication. The same flow backs CardDAV account save.
func TestEditConfigCreatesMissingConfigOnWindows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	before, err := ReadConfigFile(path)
	require.NoError(t, err)
	require.False(t, before.Exists)

	after, err := EditConfigFile(path, before.ETag, []Edit{{Key: "web.theme", Value: "dark"}})
	require.NoError(t, err)
	require.True(t, after.Exists)
	assert.Equal(t, "[web]\ntheme = \"dark\"\n", string(mustReadFile(t, path)))
	current, err := ReadConfigFile(path)
	require.NoError(t, err)
	assert.True(t, SameConfigFileVersion(current, after),
		"a freshly created config must verify against its returned snapshot")
	require.NoError(t, verifyConfigOwnerOnly(path))
}
