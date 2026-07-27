package query

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPrepareDaemonSpillDirRemovesOnlyValidatedStaleDirectories(t *testing.T) {
	home := t.TempDir()
	tmpRoot := filepath.Join(home, "tmp")
	require.NoError(t, os.MkdirAll(tmpRoot, 0o700))

	stale := filepath.Join(tmpRoot, "duckdb-query-2147483647")
	live := filepath.Join(tmpRoot, fmt.Sprintf("duckdb-query-%d", os.Getpid()))
	malformed := filepath.Join(tmpRoot, "duckdb-query-not-a-pid")
	zeroPID := filepath.Join(tmpRoot, "duckdb-query-0")
	for _, dir := range []string{stale, live, malformed, zeroPID} {
		require.NoError(t, os.MkdirAll(dir, 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "spill.tmp"), []byte("data"), 0o600))
	}

	owned, err := PrepareDaemonSpillDir(home)
	require.NoError(t, err)
	assert.Equal(t, live, owned)
	assert.NoDirExists(t, stale)
	assert.DirExists(t, live)
	assert.DirExists(t, malformed)
	assert.DirExists(t, zeroPID)
}

func TestDuckDBEngineCloseRemovesOnlyOwnedSpillDirectory(t *testing.T) {
	root := t.TempDir()
	owned := filepath.Join(root, "owned")
	foreign := filepath.Join(root, "foreign")
	require.NoError(t, os.MkdirAll(owned, 0o700))
	require.NoError(t, os.MkdirAll(foreign, 0o700))

	engine, err := NewDuckDBEngine("", "", nil, DuckDBOptions{
		TempDirectory:    owned,
		OwnTempDirectory: true,
	})
	require.NoError(t, err)
	require.NoError(t, engine.Close())
	assert.NoDirExists(t, owned)
	assert.DirExists(t, foreign)
}
