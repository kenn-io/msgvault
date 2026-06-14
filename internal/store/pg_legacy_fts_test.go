package store_test

import (
	"os"
	"strings"
	"testing"

	requirepkg "github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/testutil"
)

// TestInitSchema_PGLegacyMessagesGetsFTSColumn pins cr2-10: a PostgreSQL
// database whose `messages` table predates FTS support (no search_fts column)
// must come up with a working FTS column + GIN index after InitSchema.
//
// Before the fix, schema_pg.sql's `CREATE INDEX ... USING GIN (search_fts)`
// ran as part of the single transactional schema-file Exec, BEFORE the
// LegacyColumnMigrations could add the column — so the index failed with
// "column search_fts does not exist" (SQLSTATE 42703) and the whole apply
// rolled back, making InitSchema fail loud on upgrade. The fix adds an
// idempotent ADD COLUMN migration and moves the GIN index into
// EnsureFTSIndex, which runs after migrations.
//
// We simulate the legacy state faithfully: build the full current schema,
// then drop the search_fts column (CASCADE drops its GIN index too), then
// re-run InitSchema and assert the column + index + availability are restored.
func TestInitSchema_PGLegacyMessagesGetsFTSColumn(t *testing.T) {
	testDB := os.Getenv("MSGVAULT_TEST_DB")
	if !strings.HasPrefix(testDB, "postgres://") && !strings.HasPrefix(testDB, "postgresql://") {
		t.Skip("cr2-10 legacy-upgrade test requires MSGVAULT_TEST_DB pointing at PostgreSQL")
	}

	st := testutil.NewTestStore(t) // builds the full current schema in an isolated schema
	db := st.DB()

	// Simulate a legacy DB created before FTS: drop the column (and its GIN
	// index via CASCADE). This is the exact pre-upgrade shape.
	_, err := db.Exec(`ALTER TABLE messages DROP COLUMN search_fts CASCADE`)
	requirepkg.NoError(t, err, "drop search_fts to simulate legacy schema")

	var preCol int
	requirepkg.NoError(t, db.QueryRow(`
		SELECT COUNT(*) FROM information_schema.columns
		WHERE table_schema = current_schema()
		  AND table_name = 'messages' AND column_name = 'search_fts'`).Scan(&preCol),
		"probe search_fts column (pre)")
	requirepkg.Equal(t, 0, preCol, "precondition: legacy schema has no search_fts column")

	// Upgrade: re-run InitSchema. Must succeed (the bug made this fail loud).
	requirepkg.NoError(t, st.InitSchema(), "InitSchema must succeed on a legacy PG schema")

	// The search_fts column was re-added by the migration.
	var colCount int
	requirepkg.NoError(t, db.QueryRow(`
		SELECT COUNT(*) FROM information_schema.columns
		WHERE table_schema = current_schema()
		  AND table_name = 'messages' AND column_name = 'search_fts'`).Scan(&colCount),
		"probe search_fts column")
	requirepkg.Equal(t, 1, colCount, "search_fts column must exist after upgrade")

	// The GIN index was created by EnsureFTSIndex (post-migration).
	var idxCount int
	requirepkg.NoError(t, db.QueryRow(`
		SELECT COUNT(*) FROM pg_indexes
		WHERE schemaname = current_schema()
		  AND tablename = 'messages' AND indexname = 'messages_search_fts_idx'`).Scan(&idxCount),
		"probe GIN index")
	requirepkg.Equal(t, 1, idxCount, "messages_search_fts_idx must exist after upgrade")

	// And the dialect now reports FTS available.
	requirepkg.True(t, st.FTS5Available(), "FTS must be available after legacy upgrade")
}
