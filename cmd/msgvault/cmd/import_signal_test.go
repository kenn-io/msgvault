package cmd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolveSignalInput(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "database.sqlite")
	require.NoError(os.WriteFile(dbPath, []byte("db"), 0o600))
	otherFiles := t.TempDir()

	gotDB, gotFiles, err := resolveSignalInput(dir, "")
	require.NoError(err)
	assert.Equal(dbPath, gotDB)
	assert.Equal(dir, gotFiles)

	gotDB, gotFiles, err = resolveSignalInput(dbPath, otherFiles)
	require.NoError(err)
	assert.Equal(dbPath, gotDB)
	assert.Equal(otherFiles, gotFiles)

	_, _, err = resolveSignalInput(t.TempDir(), "")
	require.Error(err)
	assert.Contains(err.Error(), "no database.sqlite")

	_, _, err = resolveSignalInput(dbPath, filepath.Join(dir, "missing"))
	require.Error(err)
	assert.Contains(err.Error(), "attachments directory not found")
}
