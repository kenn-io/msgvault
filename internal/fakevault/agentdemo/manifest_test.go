package agentdemo_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/fakevault/agentdemo"
)

func TestGenerateDirRoundTripsManifest(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	dir := t.TempDir()
	m, err := agentdemo.GenerateDir(t.Context(), agentdemo.Options{Dir: dir, Seed: 1})
	require.NoError(err)
	loaded, err := agentdemo.Load(dir)
	require.NoError(err)
	assert.Equal(m, loaded)
	assert.FileExists(filepath.Join(dir, "msgvault.db"))
}

func TestGenerateDirRefusesExistingDatabase(t *testing.T) {
	for _, name := range []string{"regular file", "dangling symlink"} {
		t.Run(name, func(t *testing.T) {
			require := require.New(t)
			dir := t.TempDir()
			path := filepath.Join(dir, "msgvault.db")
			target := filepath.Join(t.TempDir(), "untouched.db")
			if name == "regular file" {
				require.NoError(os.WriteFile(path, []byte("keep me"), 0600))
			} else {
				require.NoError(os.Symlink(target, path))
			}
			_, err := agentdemo.GenerateDir(t.Context(), agentdemo.Options{Dir: dir})
			require.ErrorContains(err, "already exists")
			if name == "regular file" {
				data, err := os.ReadFile(path)
				require.NoError(err)
				assert.Equal(t, "keep me", string(data))
			} else {
				assert.NoFileExists(t, target)
			}
		})
	}
}
