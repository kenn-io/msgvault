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
	requirementsForTest := require.New(t)
	assertionsForTest := assert.New(t)
	home := t.TempDir()
	tmpRoot := filepath.Join(home, "tmp")
	requirementsForTest.NoError(os.MkdirAll(tmpRoot, 0o700))

	stale := filepath.Join(tmpRoot, "duckdb-query-2147483647")
	live := filepath.Join(tmpRoot, fmt.Sprintf("duckdb-query-%d", os.Getpid()))
	malformed := filepath.Join(tmpRoot, "duckdb-query-not-a-pid")
	zeroPID := filepath.Join(tmpRoot, "duckdb-query-0")
	for _, dir := range []string{stale, live, malformed, zeroPID} {
		requirementsForTest.NoError(os.MkdirAll(dir, 0o700))
		requirementsForTest.NoError(os.WriteFile(filepath.Join(dir, "spill.tmp"), []byte("data"), 0o600))
	}

	owned, err := PrepareDaemonSpillDir(home)
	requirementsForTest.NoError(err)
	assertionsForTest.Equal(live, owned)
	assertionsForTest.NoDirExists(stale)
	assertionsForTest.DirExists(live)
	assertionsForTest.DirExists(malformed)
	assertionsForTest.DirExists(zeroPID)
}

func TestDuckDBEngineCloseRemovesOnlyOwnedSpillDirectory(t *testing.T) {
	requirementsForTest := require.New(t)
	root := t.TempDir()
	owned := filepath.Join(root, "owned")
	foreign := filepath.Join(root, "foreign")
	requirementsForTest.NoError(os.MkdirAll(owned, 0o700))
	requirementsForTest.NoError(os.MkdirAll(foreign, 0o700))

	engine, err := NewDuckDBEngine("", "", nil, DuckDBOptions{
		TempDirectory:    owned,
		OwnTempDirectory: true,
	})
	requirementsForTest.NoError(err)
	requirementsForTest.NoError(engine.Close())
	assert.NoDirExists(t, owned)
	assert.DirExists(t, foreign)
}
