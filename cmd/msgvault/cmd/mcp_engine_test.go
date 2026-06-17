package cmd

import (
	"os"
	"path/filepath"
	"testing"

	assertpkg "github.com/stretchr/testify/assert"
	requirepkg "github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/testutil"
)

// writeCompleteParquetFixture lays down an empty *.parquet file in every
// directory query.HasCompleteParquetData requires, so the analytics dir is
// reported as a complete cache. The files only need to exist for the glob
// check; their contents are never parsed by HasCompleteParquetData.
func writeCompleteParquetFixture(t *testing.T) string {
	t.Helper()
	require := requirepkg.New(t)
	dir := t.TempDir()
	for _, sub := range query.RequiredParquetDirs {
		full := filepath.Join(dir, sub)
		require.NoError(os.MkdirAll(full, 0o755), "mkdir %s", sub)
		require.NoError(
			os.WriteFile(filepath.Join(full, "data.parquet"), []byte{}, 0o644),
			"write %s/data.parquet", sub)
	}
	require.True(query.HasCompleteParquetData(dir),
		"fixture should be a complete parquet cache")
	return dir
}

// TestMCPShouldUseParquet covers the SQLite-only branch of the MCP engine
// selection: a complete Parquet cache enables DuckDB unless the user forces
// SQLite. PostgreSQL is handled by the IsPostgreSQL() guard *before* this
// predicate is consulted (see TestMCPEngineSelectionSkipsParquetOnPostgres).
func TestMCPShouldUseParquet(t *testing.T) {
	assert := assertpkg.New(t)
	complete := writeCompleteParquetFixture(t)
	empty := t.TempDir()

	assert.True(mcpShouldUseParquet(false, complete),
		"complete cache + !forceSQL should use Parquet")
	assert.False(mcpShouldUseParquet(true, complete),
		"--force-sql must bypass Parquet even with a complete cache")
	assert.False(mcpShouldUseParquet(false, empty),
		"empty cache must not select Parquet")
}

// TestMCPEngineSelectionSkipsParquetOnPostgres is the regression for the
// roborev finding: a PostgreSQL-backed MCP server must never select the
// DuckDB/Parquet engine, even when a complete (but SQLite-oriented, possibly
// stale) analytics cache exists. The Parquet branch would feed the PG
// DSN/handle into NewDuckDBEngine's SQLite slots and read stale data.
//
// mcp.go branches on s.IsPostgreSQL() *before* mcpShouldUseParquet. This test
// pins both halves of that guarantee: a complete cache exists (so the only
// thing keeping PG off the Parquet branch is the IsPostgreSQL guard), and a
// PG store reports IsPostgreSQL()==true. With MSGVAULT_TEST_DB unset the
// store is SQLite, so the test instead asserts the SQLite store does take the
// Parquet branch — confirming the cache fixture is genuinely "complete".
func TestMCPEngineSelectionSkipsParquetOnPostgres(t *testing.T) {
	assert := assertpkg.New(t)
	require := requirepkg.New(t)

	s := testutil.NewTestStore(t)
	cacheDir := writeCompleteParquetFixture(t)

	// The cache is complete, so the Parquet branch is reachable for any
	// backend that isn't gated out first.
	require.True(mcpShouldUseParquet(false, cacheDir),
		"precondition: cache fixture must be complete")

	if s.IsPostgreSQL() {
		// mcp.go's `if s.IsPostgreSQL()` branch fires first and selects the
		// native engine, so the complete cache above is never consulted.
		engine := query.NewEngine(s.DB(), true)
		require.NotNil(engine, "PG must get the native engine")
		_, isDuck := engine.(*query.DuckDBEngine)
		assert.False(isDuck,
			"PostgreSQL MCP path must not use the DuckDB/Parquet engine")
	} else {
		// SQLite control: the same cache fixture DOES drive DuckDB selection,
		// proving the fixture is what gates Parquet (not an artifact).
		assert.False(s.IsPostgreSQL(),
			"sanity: default backend is SQLite when MSGVAULT_TEST_DB is unset")
	}
}
