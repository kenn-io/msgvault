package store_test

import (
	"os"
	"slices"
	"strings"
	"testing"

	assertpkg "github.com/stretchr/testify/assert"
	requirepkg "github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/testutil/storetest"
)

// isPostgresTestDB reports whether the active test backend is PostgreSQL,
// inferred from MSGVAULT_TEST_DB. Used by backend-portable tests that must issue
// dialect-specific SQL to set up a scenario (e.g. punching a hole in the FTS
// index differs between SQLite's messages_fts shadow table and PG's search_fts
// column).
func isPostgresTestDB() bool {
	db := os.Getenv("MSGVAULT_TEST_DB")
	return strings.HasPrefix(db, "postgres://") || strings.HasPrefix(db, "postgresql://")
}

// TestStore_NeedsFTSBackfill_Transition (finding for P2) verifies the
// FTSNeedsBackfill contract on BOTH backends: it reports true while any message
// lacks an FTS entry and false once backfill has populated them all. On
// PostgreSQL the probe is the EXISTS(search_fts IS NULL) short-circuit; on
// SQLite it is the MAX(rowid) vs MAX(id) comparison. Both must agree.
func TestStore_NeedsFTSBackfill_Transition(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	f := storetest.New(t)
	if !f.Store.FTS5Available() {
		t.Skip("FTS5 not available")
	}

	// Seed enough rows that SQLite's 10%-slack MAX comparison is unambiguous
	// (with 20 unindexed rows it cannot round to "already backfilled").
	const total = 20
	ids := f.CreateMessages(total)
	require.Len(ids, total)

	assert.True(f.Store.NeedsFTSBackfill(),
		"NeedsFTSBackfill must be true while messages have no FTS entry")

	_, err := f.Store.BackfillFTS(nil)
	require.NoError(err, "BackfillFTS")

	assert.False(f.Store.NeedsFTSBackfill(),
		"NeedsFTSBackfill must be false after a complete backfill")
}

// TestStore_NeedsFTSBackfill_HoleAtLowestID (F4) verifies that a hole left at a
// LOW id while later ids are indexed is detected on BOTH backends. This is the
// case the old SQLite MAX(rowid)-vs-MAX(id) heuristic missed: the FTS MAX still
// equals the messages MAX, so it reported "no backfill needed" even though id 1
// was unindexed. Holes are reachable in practice because UpsertFTS failures
// during sync are warn-and-continue while the message row still commits.
//
// Runs on both backends; before the fix this passed on PG (EXISTS probe) and
// failed on SQLite, proving the divergence.
func TestStore_NeedsFTSBackfill_HoleAtLowestID(t *testing.T) {
	require := requirepkg.New(t)
	f := storetest.New(t)
	if !f.Store.FTS5Available() {
		t.Skip("FTS5 not available")
	}

	const total = 20
	ids := f.CreateMessages(total)
	require.Len(ids, total)

	_, err := f.Store.BackfillFTS(nil)
	require.NoError(err, "BackfillFTS")
	require.False(f.Store.NeedsFTSBackfill(),
		"precondition: fully backfilled index must not need backfill")

	// Remove the FTS entry for the LOWEST id only. The highest id stays indexed,
	// so any MAX-based heuristic would wrongly report "complete".
	lowest := slices.Min(ids)
	if isPostgresTestDB() {
		_, err = f.Store.DB().Exec(
			"UPDATE messages SET search_fts = NULL WHERE id = $1", lowest)
	} else {
		_, err = f.Store.DB().Exec(
			"DELETE FROM messages_fts WHERE rowid = ?", lowest)
	}
	require.NoError(err, "punch a hole at the lowest id")

	assertpkg.True(t, f.Store.NeedsFTSBackfill(),
		"NeedsFTSBackfill must be true when a LOW id is unindexed even if later ids are indexed")
}
