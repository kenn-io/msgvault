package store_test

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/testutil"
)

// TestInitSchema_PGCreatesThumbnailIndexes pins that the thumbnail hash/path
// indexes are built by InitSchema's maintenance path
// (statement_timeout disabled) rather than schema_pg.sql, and still ends up
// present and idempotent. On a large existing archive the one-time build can
// exceed the pool-wide 30s statement_timeout, which would have aborted the
// whole schema apply had it stayed in the schema file.
func TestInitSchema_PGCreatesThumbnailIndexes(t *testing.T) {
	require := require.New(t)
	testDB := os.Getenv("MSGVAULT_TEST_DB")
	if !strings.HasPrefix(testDB, "postgres://") && !strings.HasPrefix(testDB, "postgresql://") {
		t.Skip("PG-only: attachment thumbnail index maintenance-path build; requires MSGVAULT_TEST_DB pointing at PostgreSQL")
	}

	st := testutil.NewTestStore(t) // runs InitSchema in an isolated schema
	probe := func(indexName string) int {
		var n int
		require.NoError(st.DB().QueryRow(st.Rebind(`
			SELECT COUNT(*) FROM pg_indexes
			WHERE schemaname = current_schema()
			  AND tablename = 'attachments'
			  AND indexname = ?`), indexName).Scan(&n),
			"probe %s", indexName)
		return n
	}
	for _, name := range []string{"idx_attachments_thumbnail_hash", "idx_attachments_thumbnail_path"} {
		require.Equal(1, probe(name), "%s must exist after InitSchema", name)
	}

	// Re-running InitSchema (every daemon/CLI start does) stays idempotent.
	require.NoError(st.InitSchema(), "InitSchema must be idempotent")
	for _, name := range []string{"idx_attachments_thumbnail_hash", "idx_attachments_thumbnail_path"} {
		require.Equal(1, probe(name), "%s still present after a second InitSchema", name)
	}
}

func TestInitSchema_SQLiteCreatesThumbnailPathIndex(t *testing.T) {
	require := require.New(t)
	testDB := os.Getenv("MSGVAULT_TEST_DB")
	if strings.HasPrefix(testDB, "postgres://") || strings.HasPrefix(testDB, "postgresql://") {
		t.Skip("SQLite-only schema.sql index assertion")
	}

	st := testutil.NewTestStore(t)
	var n int
	require.NoError(st.DB().QueryRow(`
		SELECT COUNT(*) FROM sqlite_master
		WHERE type = 'index' AND name = 'idx_attachments_thumbnail_path'`).Scan(&n))
	require.Equal(1, n, "thumbnail_path lookup must use a real schema index")
}
