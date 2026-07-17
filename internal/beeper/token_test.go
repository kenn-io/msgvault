package beeper

import (
	"os"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTokenRoundTrip(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	dir := t.TempDir()

	_, err := LoadToken(dir)
	require.Error(err)
	assert.Contains(err.Error(), "add-beeper")

	require.NoError(SaveToken(dir, "secret-token"))

	got, err := LoadToken(dir)
	require.NoError(err)
	assert.Equal("secret-token", got)

	if runtime.GOOS != "windows" {
		info, err := os.Stat(tokenPath(dir))
		require.NoError(err)
		assert.Equal(os.FileMode(0600), info.Mode().Perm())
	}

	// Overwrite is atomic and replaces the value.
	require.NoError(SaveToken(dir, "rotated"))
	got, err = LoadToken(dir)
	require.NoError(err)
	assert.Equal("rotated", got)

	require.NoError(DeleteToken(dir))
	require.NoError(DeleteToken(dir), "double delete is not an error")
}
