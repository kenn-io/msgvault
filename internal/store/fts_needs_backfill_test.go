package store_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/search"
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
	_, err := f.Store.DB().ExecContext(t.Context(), `DROP TABLE messages_fts;
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
	_, err = f.Store.DB().ExecContext(t.Context(), "DELETE FROM messages_fts WHERE rowid = ?", ids[0])
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
	first := f.CreateMessage("msg-0")
	// Keep the second message in the next 5,000-ID backfill batch.
	second := seedMessageAtID(t, f.Store, 5000, first+5000)
	for _, id := range []int64{first, second} {
		require.NoError(f.Store.UpsertMessageBody(id,
			sql.NullString{String: "orchard", Valid: true}, sql.NullString{}))
	}
	_, err := f.Store.BackfillFTS(nil)
	require.NoError(err)
	assertFTSContains(t, f.Store, "orchard", 2)
	if isPostgresTestDB() {
		_, err = f.Store.DB().ExecContext(t.Context(), "UPDATE messages SET search_fts = NULL WHERE id IN ($1, $2)", first, second)
	} else {
		_, err = f.Store.DB().ExecContext(t.Context(), "DELETE FROM messages_fts WHERE rowid IN (?, ?)", first, second)
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

// TestPG_BackfillFTSUpgradesLegacyEntries reindexes older rows from stored text.
func TestPG_BackfillFTSUpgradesLegacyEntries(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	skipUnlessPostgres(t)
	f := storetest.New(t)
	ids := f.CreateMessages(2)
	for i, version := range []sql.NullInt64{{Int64: 2, Valid: true}, {}} {
		_, err := f.Store.DB().ExecContext(t.Context(), `UPDATE messages SET subject = 'stored marker', search_fts = setweight(to_tsvector('simple', 'obsolete phantom-marker'), 'B'), indexing_version = $2 WHERE id = $1`, ids[i], version)
		require.NoError(err)
	}
	current := f.CreateMessage("current-index")
	require.NoError(f.Store.UpsertFTS(current, "current marker", "", "", "", ""))
	n, err := f.Store.BackfillFTS(nil)
	require.NoError(err)
	assert.Equal(int64(2), n)
	for term, want := range map[string][]int64{"stored marker": ids, "obsolete": nil, "phantom marker": nil, "current marker": {current}} {
		got, _, err := f.Store.SearchMessageIDsQueryContext(t.Context(), &search.Query{TextTerms: []string{term}}, 10)
		require.NoError(err)
		assert.ElementsMatch(want, got, term)
	}
}

func TestPG_BackfillFTS_ContinuesAcrossLargeMessagePages(t *testing.T) {
	skipUnlessPostgres(t)
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	body := strings.Repeat("filler ", 83000) + "alice@example.com"
	for _, id := range f.CreateMessages(8) {
		require.NoError(f.Store.UpsertMessageBody(id, sql.NullString{String: body, Valid: true}, sql.NullString{}))
	}
	_, err := f.Store.DB().ExecContext(t.Context(), `CREATE TABLE fts_page_audit (n bigint);
		CREATE FUNCTION record_fts_page() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN INSERT INTO fts_page_audit SELECT count(*) FROM indexed_rows; RETURN NULL; END $$;
		CREATE TRIGGER record_fts_page AFTER UPDATE ON messages
		REFERENCING NEW TABLE AS indexed_rows FOR EACH STATEMENT EXECUTE FUNCTION record_fts_page()`)
	require.NoError(err)
	count, err := f.Store.BackfillFTS(nil)
	require.NoError(err)
	assert.EqualValues(8, count)
	var pages, largest int
	require.NoError(f.Store.DB().QueryRowContext(t.Context(), `SELECT count(*), max(n) FROM fts_page_audit`).Scan(&pages, &largest))
	assert.Greater(pages, 1)
	assert.Less(largest, 8)
	assertFTSContains(t, f.Store, "alice@example.com", 8)
}

func TestPG_BackfillFTS_ConcurrentUpdates(t *testing.T) {
	for _, refreshed := range []bool{false, true} {
		t.Run(fmt.Sprintf("refreshed=%t", refreshed), func(t *testing.T) {
			skipUnlessPostgres(t)
			require := require.New(t)
			f := storetest.New(t)
			ids := f.CreateMessages(65)
			if refreshed {
				_, err := f.Store.DB().ExecContext(t.Context(), `UPDATE messages SET search_fts = to_tsvector('simple', 'old'), indexing_version = 2`)
				require.NoError(err)
			}
			require.NoError(f.Store.UpsertMessageBody(ids[0], sql.NullString{String: "old body", Valid: true}, sql.NullString{}))
			tx, err := f.Store.DB().BeginTx(t.Context(), nil)
			require.NoError(err)
			t.Cleanup(func() { _ = tx.Rollback() })
			if refreshed {
				dialect := &store.PostgreSQLDialect{}
				for _, id := range ids[:64] {
					require.NoError(dialect.FTSUpsert(tx, store.FTSDoc{MessageID: id, FromAddr: "slackdisplayname"}))
				}
			} else {
				_, err = tx.ExecContext(t.Context(), `UPDATE messages SET subject = 'updated', search_fts = NULL WHERE id = $1`, ids[0])
				require.NoError(err)
				_, err = tx.ExecContext(t.Context(), `UPDATE message_bodies SET body_text = 'new@example.com' WHERE message_id = $1`, ids[0])
				require.NoError(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			done := make(chan error, 1)
			go func() { _, err := f.Store.BackfillFTSContext(ctx, nil); done <- err }()
			require.Eventually(func() bool {
				var waiting bool
				err := f.Store.DB().QueryRowContext(t.Context(), `SELECT EXISTS (SELECT 1 FROM pg_stat_activity WHERE datname = current_database() AND query LIKE 'SELECT m.id FROM messages m WHERE m.id IN (%' AND cardinality(pg_blocking_pids(pid)) > 0)`).Scan(&waiting)
				return err == nil && waiting
			}, 30*time.Second, 20*time.Millisecond)
			require.NoError(tx.Commit())
			require.NoError(<-done)
			if refreshed {
				assertFTSContains(t, f.Store, "slackdisplayname", 64)
				var version int
				require.NoError(f.Store.DB().QueryRowContext(t.Context(), `SELECT indexing_version FROM messages WHERE id = $1`, ids[64]).Scan(&version))
				assert.Equal(t, store.CurrentFTSIndexingVersion, version)
			} else {
				assertFTSContains(t, f.Store, "new@example.com", 1)
				assertFTSContains(t, f.Store, "old", 0)
			}
		})
	}
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
		_, err = f.Store.DB().ExecContext(
			t.Context(),
			"UPDATE messages SET search_fts = NULL WHERE id = $1", lowest)
	} else {
		_, err = f.Store.DB().ExecContext(
			t.Context(),
			"DELETE FROM messages_fts WHERE rowid = ?", lowest)
	}
	require.NoError(err, "punch a hole at the lowest id")

	assert.True(t, f.Store.NeedsFTSBackfill(),
		"NeedsFTSBackfill must be true when a LOW id is unindexed even if later ids are indexed")
}

func TestPG_BackfillFTS_ConcurrentRebuild(t *testing.T) {
	require := require.New(t)
	skipUnlessPostgres(t)
	f := storetest.New(t)
	ids := f.CreateMessages(65)
	_, err := f.Store.DB().ExecContext(t.Context(), `UPDATE messages SET subject = 'repair marker', search_fts = NULL, indexing_version = NULL WHERE id = ANY($1)`, ids[:64])
	require.NoError(err)
	_, err = f.Store.DB().ExecContext(t.Context(), `UPDATE messages SET subject = 'repair marker', search_fts = NULL, indexing_version = NULL WHERE id = $1`, ids[64])
	require.NoError(err)
	// Put the last row before the first page in heap order.
	_, err = f.Store.DB().ExecContext(t.Context(), `CREATE INDEX test_messages_reverse_id ON messages (id DESC)`)
	require.NoError(err)
	_, err = f.Store.DB().ExecContext(t.Context(), `CLUSTER messages USING test_messages_reverse_id`)
	require.NoError(err)
	_, err = f.Store.DB().ExecContext(t.Context(), `DROP INDEX test_messages_reverse_id`)
	require.NoError(err)
	var scrambled bool
	require.NoError(f.Store.DB().QueryRowContext(t.Context(), `SELECT NOT EXISTS (SELECT 1 FROM messages WHERE id = ANY($2) AND ctid <= (SELECT ctid FROM messages WHERE id = $1))`, ids[64], ids[:64]).Scan(&scrambled))
	require.True(scrambled)

	tx, err := f.Store.DB().BeginTx(t.Context(), nil)
	require.NoError(err)
	t.Cleanup(func() { _ = tx.Rollback() })
	_, err = tx.ExecContext(t.Context(), `SELECT id FROM messages WHERE id = $1 FOR UPDATE`, ids[64])
	require.NoError(err)
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	backfillDone := make(chan error, 1)
	go func() { _, err := f.Store.BackfillFTSContext(ctx, nil); backfillDone <- err }()
	require.Eventually(func() bool {
		var blocked bool
		err := f.Store.DB().QueryRowContext(t.Context(), `SELECT EXISTS (SELECT 1 FROM pg_stat_activity WHERE datname = current_database() AND query LIKE 'SELECT m.id FROM messages m WHERE m.id IN (%' AND cardinality(pg_blocking_pids(pid)) > 0)`).Scan(&blocked)
		return err == nil && blocked
	}, 30*time.Second, 20*time.Millisecond)

	rebuildDone := make(chan error, 1)
	go func() { _, err := f.Store.RebuildFTSContext(ctx, nil); rebuildDone <- err }()
	require.Eventually(func() bool {
		var blocked bool
		err := f.Store.DB().QueryRowContext(t.Context(), `SELECT EXISTS (SELECT 1 FROM pg_stat_activity WHERE datname = current_database() AND query LIKE 'DROP INDEX IF EXISTS messages_search_fts_idx%' AND cardinality(pg_blocking_pids(pid)) > 0)`).Scan(&blocked)
		return err == nil && blocked
	}, 30*time.Second, 20*time.Millisecond)
	require.NoError(tx.Commit())
	require.NoError(<-backfillDone)
	require.NoError(<-rebuildDone)
	assertFTSContains(t, f.Store, "repair marker", 65)
	needs, err := f.Store.NeedsFTSBackfillContext(t.Context())
	require.NoError(err)
	assert.False(t, needs)
}
