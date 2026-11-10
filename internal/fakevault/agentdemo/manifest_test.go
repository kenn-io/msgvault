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
	dir := t.TempDir()
	m, err := agentdemo.GenerateDir(t.Context(), agentdemo.Options{Dir: dir, Seed: 1})
	require.NoError(t, err)
	loaded, err := agentdemo.Load(dir)
	require.NoError(t, err)
	assert.Equal(t, m, loaded)
	assert.Equal(t, "agent-demo", m.Scenario)
	assert.Equal(t, 1, m.ScenarioVersion)
	assert.Equal(t, agentdemo.DefaultAsOf, m.AsOf)
	assert.Equal(t, uint64(1), m.Seed)
	assert.FileExists(t, filepath.Join(dir, "msgvault.db"))
}

func TestGenerateDirRefusesExistingDatabase(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "msgvault.db")
	require.NoError(t, os.WriteFile(path, []byte("keep me"), 0600))
	_, err := agentdemo.GenerateDir(t.Context(), agentdemo.Options{Dir: dir})
	require.ErrorContains(t, err, "already exists")
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "keep me", string(data))
}

func TestManifestRoundTrip(t *testing.T) {
	dir := t.TempDir()
	m := &agentdemo.Manifest{Scenario: agentdemo.Scenario, Seed: 7, AsOf: agentdemo.DefaultAsOf}
	require.NoError(t, m.Write(dir))
	got, err := agentdemo.Load(dir)
	require.NoError(t, err)
	assert.Equal(t, m, got)
}

func TestGenerateDirRefusesDanglingDatabaseSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(t.TempDir(), "untouched.db")
	require.NoError(t, os.Symlink(target, filepath.Join(dir, "msgvault.db")))
	_, err := agentdemo.GenerateDir(t.Context(), agentdemo.Options{Dir: dir})
	require.ErrorContains(t, err, "already exists")
	assert.NoFileExists(t, target)
}
