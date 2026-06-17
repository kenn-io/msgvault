package cmd

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	_ "github.com/marcboeker/go-duckdb"
	assertpkg "github.com/stretchr/testify/assert"
	requirepkg "github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/fbmessenger"
	"go.kenn.io/msgvault/internal/store"
)

// TestBuildCache_AfterMessengerImport verifies invariant #3 from the plan:
// after importing Messenger JSON and running buildCache, the resulting
// Parquet partition files exist and contain the expected row count.
func TestBuildCache_AfterMessengerImport(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "msgvault.db")
	analyticsDir := filepath.Join(tmp, "analytics")

	st, err := store.Open(dbPath)
	require.NoError(err, "open store")
	require.NoError(st.InitSchema(), "init schema")

	fixture, err := filepath.Abs("../../../internal/fbmessenger/testdata/json_simple")
	require.NoError(err)
	summary, err := fbmessenger.ImportDYI(context.Background(), st, fbmessenger.ImportOptions{
		Me:             "wes@facebook.messenger",
		RootDir:        fixture,
		Format:         "auto",
		AttachmentsDir: t.TempDir(),
	})
	require.NoError(err, "ImportDYI")
	require.NoError(st.Close())
	require.Equal(int64(4), summary.MessagesAdded, "imported messages")

	result, err := buildCache(dbPath, analyticsDir, false)
	require.NoError(err, "buildCache")
	require.False(result.Skipped, "buildCache unexpectedly skipped")

	duckdb, err := sql.Open("duckdb", "")
	require.NoError(err, "open duckdb")
	defer func() { _ = duckdb.Close() }()

	var n int
	pattern := filepath.Join(analyticsDir, "messages", "**", "*.parquet")
	err = duckdb.QueryRow(
		`SELECT COUNT(*) FROM read_parquet(?, hive_partitioning=true)`, pattern,
	).Scan(&n)
	require.NoError(err, "duckdb scan")
	assert.Equal(4, n, "parquet messages")

	var mtype string
	err = duckdb.QueryRow(
		`SELECT DISTINCT message_type FROM read_parquet(?, hive_partitioning=true)`, pattern,
	).Scan(&mtype)
	require.NoError(err, "duckdb message_type")
	assert.Equal("fbmessenger", mtype, "message_type")
}
