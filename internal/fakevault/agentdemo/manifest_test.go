package agentdemo_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/fakevault/agentdemo"
)

func TestGenerateDirWritesManifestHeader(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	dir := t.TempDir()
	m, err := agentdemo.GenerateDir(t.Context(), agentdemo.Options{Dir: dir, Seed: 1})
	require.NoError(err)
	loaded, err := agentdemo.Load(dir)
	require.NoError(err)
	assert.Equal(m, loaded)
	assert.Equal("agent-demo", m.Scenario)
	assert.Equal(1, m.ScenarioVersion)
	assert.Equal(agentdemo.DefaultAsOf, m.AsOf)
	assert.Equal(uint64(1), m.Seed)
	assert.FileExists(filepath.Join(dir, "msgvault.db"))
}

func TestGenerateDirRefusesExistingDatabase(t *testing.T) {
	require := require.New(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "msgvault.db")
	require.NoError(os.WriteFile(path, []byte("keep me"), 0600))
	_, err := agentdemo.GenerateDir(t.Context(), agentdemo.Options{Dir: dir})
	require.ErrorContains(err, "already exists")
	data, err := os.ReadFile(path)
	require.NoError(err)
	assert.Equal(t, "keep me", string(data))
}

func TestGenerateDirRefusesDanglingDatabaseSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(t.TempDir(), "untouched.db")
	require.NoError(t, os.Symlink(target, filepath.Join(dir, "msgvault.db")))
	_, err := agentdemo.GenerateDir(t.Context(), agentdemo.Options{Dir: dir})
	require.ErrorContains(t, err, "already exists")
	assert.NoFileExists(t, target)
}
