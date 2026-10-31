package store_test

import (
	"database/sql"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestSQLite_BackfillFTS_ColumnsizeZero(t *testing.T) {
	if isPostgresTestDB() {
		t.Skip("SQLite FTS5 table option")
	}
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	_, err := f.Store.DB().Exec(`DROP TABLE messages_fts;
		CREATE VIRTUAL TABLE messages_fts USING fts5(
			message_id UNINDEXED, subject, body, from_addr, to_addr, cc_addr, columnsize=0)`)
	require.NoError(err)
	ids := f.CreateMessages(2)
	for _, id := range ids {
		require.NoError(f.Store.UpsertMessageBody(id,
			sql.NullString{String: "orchard", Valid: true}, sql.NullString{}))
	}
	_, err = f.Store.BackfillFTS(nil)
	require.NoError(err)
	_, err = f.Store.DB().Exec("DELETE FROM messages_fts WHERE rowid = ?", ids[0])
	require.NoError(err)
	n, err := f.Store.BackfillFTS(nil)
	require.NoError(err)
	assert.Equal(int64(1), n)
	assertFTSContains(t, f.Store, "orchard", 2)
}

func TestStore_BackfillFTS_ResumesCommittedBatch(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	if !f.Store.FTS5Available() {
		t.Skip("FTS5 not available")
	}
	ids := f.CreateMessages(5001)
	first, second := ids[0], ids[5000]
	for _, id := range []int64{first, second} {
		require.NoError(f.Store.UpsertMessageBody(id,
			sql.NullString{String: "orchard", Valid: true}, sql.NullString{}))
	}
	_, err := f.Store.BackfillFTS(nil)
	require.NoError(err)
	assertFTSContains(t, f.Store, "orchard", 2)
	if isPostgresTestDB() {
		_, err = f.Store.DB().Exec("UPDATE messages SET search_fts = NULL WHERE id IN ($1, $2)", first, second)
	} else {
		_, err = f.Store.DB().Exec("DELETE FROM messages_fts WHERE rowid IN (?, ?)", first, second)
	}
	require.NoError(err)

	interrupted := errors.New("second batch interrupted")
	restore := f.Store.SetBackfillFTSBatchErrHookForTest(func(fromID, _ int64) error {
		if fromID >= second {
			return interrupted
		}
		return nil
	})
	t.Cleanup(restore)
	n, err := f.Store.BackfillFTS(nil)
	require.ErrorIs(err, interrupted)
	assert.Equal(int64(1), n, "first batch committed before the interruption")
	matches, total, err := f.Store.SearchMessages("orchard", 0, 10)
	require.NoError(err)
	assert.Equal(int64(1), total)
	require.Len(matches, 1)
	assert.Equal(first, matches[0].ID)
	restore()

	n, err = f.Store.BackfillFTS(nil)
	require.NoError(err)
	assert.Equal(int64(1), n, "retry indexes only the second-batch gap")
	assertFTSContains(t, f.Store, "orchard", 2)
}

func TestPG_BackfillFTS_RefreshesOutdatedEntries(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	skipUnlessPostgres(t)
	f := storetest.New(t)
	ids := f.CreateMessages(2)
	_, err := f.Store.BackfillFTS(nil)
	require.NoError(err)
	_, err = f.Store.DB().Exec(`UPDATE messages
		SET search_fts = to_tsvector('simple', 'obsolete'), indexing_version = $1
		WHERE id = $2`, store.CurrentFTSIndexingVersion-1, ids[1])
	require.NoError(err)

	n, err := f.Store.BackfillFTS(nil)
	require.NoError(err)
	assert.Equal(int64(1), n, "only the outdated entry needs backfill")
	assertFTSContains(t, f.Store, "obsolete", 0)
}

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
	require := require.New(t)
	assert := assert.New(t)
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

// TestStore_NeedsFTSBackfillQuick_Transition verifies the cheap probe's
// contract on both backends: true while the index tail is unindexed, false
// once backfill completes. (Interior holes are explicitly out of contract on
// SQLite — the full NeedsFTSBackfill anti-join is authoritative for those.)
func TestStore_NeedsFTSBackfillQuick_Transition(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	if !f.Store.FTS5Available() {
		t.Skip("FTS5 not available")
	}

	const total = 20
	ids := f.CreateMessages(total)
	require.Len(ids, total)

	assert.True(f.Store.NeedsFTSBackfillQuick(),
		"quick probe must be true while the index tail is unindexed")

	_, err := f.Store.BackfillFTS(nil)
	require.NoError(err, "BackfillFTS")

	assert.False(f.Store.NeedsFTSBackfillQuick(),
		"quick probe must be false after a complete backfill")
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
	require := require.New(t)
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

	assert.True(t, f.Store.NeedsFTSBackfill(),
		"NeedsFTSBackfill must be true when a LOW id is unindexed even if later ids are indexed")
}
