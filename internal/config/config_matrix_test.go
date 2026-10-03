package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadMatrixConfig(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	dir := t.TempDir()
	t.Setenv("MSGVAULT_HOME", dir)
	path := filepath.Join(dir, "config.toml")
	require.NoError(os.WriteFile(path, []byte(`[matrix]
enabled = true
schedule = "*/30 * * * *"
rooms = ["!included:example.org"]
exclude_rooms = ["!excluded:example.org"]
`), 0o600))
	cfg, err := Load(path, "")
	require.NoError(err)
	assert.True(cfg.Matrix.Enabled)
	assert.Equal([]string{"!included:example.org"}, cfg.Matrix.Rooms)
	assert.Equal([]string{"!excluded:example.org"}, cfg.Matrix.ExcludeRooms)
}
