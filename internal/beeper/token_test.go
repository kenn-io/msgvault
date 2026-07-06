package beeper

import (
	"os"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTokenRoundTrip(t *testing.T) {
	dir := t.TempDir()

	assert.False(t, HasToken(dir))
	_, err := LoadToken(dir)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "add-beeper")

	require.NoError(t, SaveToken(dir, "secret-token"))
	assert.True(t, HasToken(dir))

	got, err := LoadToken(dir)
	require.NoError(t, err)
	assert.Equal(t, "secret-token", got)

	if runtime.GOOS != "windows" {
		info, err := os.Stat(TokenPath(dir))
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0600), info.Mode().Perm())
	}

	// Overwrite is atomic and replaces the value.
	require.NoError(t, SaveToken(dir, "rotated"))
	got, err = LoadToken(dir)
	require.NoError(t, err)
	assert.Equal(t, "rotated", got)

	require.NoError(t, DeleteToken(dir))
	assert.False(t, HasToken(dir))
	require.NoError(t, DeleteToken(dir), "double delete is not an error")
}
