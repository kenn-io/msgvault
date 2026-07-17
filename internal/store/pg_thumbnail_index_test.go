package store_test

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/testutil"
)

// TestInitSchema_PGCreatesAttachmentMaintenanceIndexes pins that the thumbnail
// hash/path and case-normalized hash indexes are built by InitSchema's
// maintenance path (statement_timeout disabled) rather than schema_pg.sql,
// and still ends up present and idempotent. On a large existing archive the one-time build can
// exceed the pool-wide 30s statement_timeout, which would have aborted the
// whole schema apply had it stayed in the schema file.
func TestInitSchema_PGCreatesAttachmentMaintenanceIndexes(t *testing.T) {
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
	for _, name := range []string{
		"idx_attachments_thumbnail_hash",
		"idx_attachments_thumbnail_path",
		"idx_attachments_content_hash_lower",
		"idx_attachments_thumbnail_hash_lower",
	} {
		require.Equal(1, probe(name), "%s must exist after InitSchema", name)
	}

	// Simulate an upgraded schema that predates the case-normalized indexes.
	for _, name := range []string{
		"idx_attachments_content_hash_lower",
		"idx_attachments_thumbnail_hash_lower",
	} {
		_, err := st.DB().Exec(`DROP INDEX ` + name)
		require.NoError(err, "drop %s", name)
	}
	require.NoError(st.InitSchema(), "InitSchema must restore upgrade indexes through maintenance")
	for _, name := range []string{
		"idx_attachments_thumbnail_hash",
		"idx_attachments_thumbnail_path",
		"idx_attachments_content_hash_lower",
		"idx_attachments_thumbnail_hash_lower",
	} {
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

func TestInitSchema_SQLiteCreatesAttachmentHashExpressionIndexes(t *testing.T) {
	require := require.New(t)
	testDB := os.Getenv("MSGVAULT_TEST_DB")
	if strings.HasPrefix(testDB, "postgres://") || strings.HasPrefix(testDB, "postgresql://") {
		t.Skip("SQLite-only expression-index assertion")
	}

	st := testutil.NewTestStore(t)
	names := []string{
		"idx_attachments_content_hash_lower",
		"idx_attachments_thumbnail_hash_lower",
	}
	probe := func(name string) int {
		var n int
		require.NoError(st.DB().QueryRow(`
			SELECT COUNT(*) FROM sqlite_master
			WHERE type = 'index' AND name = ?`, name).Scan(&n))
		return n
	}
	for _, name := range names {
		require.Equal(1, probe(name), "%s must exist after fresh InitSchema", name)
	}

	for _, name := range names {
		_, err := st.DB().Exec(`DROP INDEX ` + name)
		require.NoError(err, "drop %s", name)
	}
	require.NoError(st.InitSchema(), "InitSchema must add expression indexes to upgraded databases")
	for _, name := range names {
		require.Equal(1, probe(name), "%s must exist after upgraded InitSchema", name)
	}
}
