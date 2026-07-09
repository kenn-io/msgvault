package store_test

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/testutil"
)

// TestInitSchema_PGCreatesThumbnailHashIndex pins that
// idx_attachments_thumbnail_hash is built by InitSchema's maintenance path
// (statement_timeout disabled) rather than schema_pg.sql, and still ends up
// present and idempotent. On a large existing archive the one-time build can
// exceed the pool-wide 30s statement_timeout, which would have aborted the
// whole schema apply had it stayed in the schema file.
func TestInitSchema_PGCreatesThumbnailHashIndex(t *testing.T) {
	require := require.New(t)
	testDB := os.Getenv("MSGVAULT_TEST_DB")
	if !strings.HasPrefix(testDB, "postgres://") && !strings.HasPrefix(testDB, "postgresql://") {
		t.Skip("PG-only: idx_attachments_thumbnail_hash maintenance-path build; requires MSGVAULT_TEST_DB pointing at PostgreSQL")
	}

	st := testutil.NewTestStore(t) // runs InitSchema in an isolated schema
	probe := func() int {
		var n int
		require.NoError(st.DB().QueryRow(`
			SELECT COUNT(*) FROM pg_indexes
			WHERE schemaname = current_schema()
			  AND tablename = 'attachments'
			  AND indexname = 'idx_attachments_thumbnail_hash'`).Scan(&n),
			"probe idx_attachments_thumbnail_hash")
		return n
	}
	require.Equal(1, probe(), "index must exist after InitSchema")

	// Re-running InitSchema (every daemon/CLI start does) stays idempotent.
	require.NoError(st.InitSchema(), "InitSchema must be idempotent")
	require.Equal(1, probe(), "index still present after a second InitSchema")
}
