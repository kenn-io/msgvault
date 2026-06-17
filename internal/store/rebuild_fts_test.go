package store_test

import (
	"database/sql"
	"testing"

	assertpkg "github.com/stretchr/testify/assert"
	requirepkg "github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

// assertFTSContains verifies that exactly wantCount messages match the given
// term through the SAME production search pipeline the app uses
// (Store.SearchMessages → BuildFTSArg → FTSSearchClause). Routing through
// SearchMessages — rather than hand-writing dialect FTS SQL — exercises the
// real query parser for whichever backend is active (to_tsquery('simple', …)
// with prefix lexemes on PG; the BuildFTSArg prefix shape on SQLite), so the
// helper cannot drift from production the way a bare websearch_to_tsquery /
// MATCH query did.
func assertFTSContains(t *testing.T, st *store.Store, term string, wantCount int) {
	t.Helper()
	_, total, err := st.SearchMessages(term, 0, 100)
	requirepkg.NoError(t, err, "SearchMessages %q", term)
	assertpkg.Equal(t, int64(wantCount), total, "FTS match count for %q", term)
}

// TestStore_RebuildFTS_HappyPath verifies RebuildFTS on a healthy database
// recreates the FTS index with correct searchable content.
// Runs on both SQLite and PostgreSQL — PG verification uses the tsvector column.
func TestStore_RebuildFTS_HappyPath(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	f := storetest.New(t)
	if !f.Store.FTS5Available() {
		t.Skip("FTS5 not available")
	}

	msgID1 := f.CreateMessage("rebuild-msg-1")
	require.NoError(f.Store.UpsertMessageBody(msgID1,
		sql.NullString{String: "apple pie filling", Valid: true}, sql.NullString{}),
		"UpsertMessageBody 1")

	pid1 := f.EnsureParticipant("alice@example.com", "Alice", "example.com")
	require.NoError(f.Store.ReplaceMessageRecipients(msgID1, "from",
		[]int64{pid1}, []string{"Alice"}), "ReplaceMessageRecipients")

	msgID2 := f.CreateMessage("rebuild-msg-2")
	require.NoError(f.Store.UpsertMessageBody(msgID2,
		sql.NullString{String: "banana bread recipe", Valid: true}, sql.NullString{}),
		"UpsertMessageBody 2")

	n, err := f.Store.RebuildFTS(nil)
	require.NoError(err, "RebuildFTS")
	assert.Equal(int64(2), n, "RebuildFTS rows")

	assertFTSContains(t, f.Store, "banana", 1)
	assertFTSContains(t, f.Store, "alice", 1)
}

// TestStore_RebuildFTS_BypassesAvailabilityFlag verifies the critical
// guarantee that RebuildFTS ignores the cached fts5Available flag. A corrupt
// FTS5 shadow table causes the availability probe to fail, which is exactly
// when the rebuild is needed — BackfillFTS would short-circuit here, but
// RebuildFTS must not.
// Runs on both SQLite and PostgreSQL — the availability-flag bypass is
// a Store-level concern independent of dialect.
func TestStore_RebuildFTS_BypassesAvailabilityFlag(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	f := storetest.New(t)
	if !f.Store.FTS5Available() {
		t.Skip("FTS5 not available")
	}

	msgID := f.CreateMessage("rebuild-bypass")
	require.NoError(f.Store.UpsertMessageBody(msgID,
		sql.NullString{String: "cherry tart dessert", Valid: true}, sql.NullString{}),
		"UpsertMessageBody")

	// Force the cached flag false to simulate a probe that saw a corrupt
	// shadow table (SQLite) or a missing column (PG) and returned false at
	// InitSchema time.
	store.SetFTS5AvailableForTest(f.Store, false)

	n, err := f.Store.RebuildFTS(nil)
	require.NoError(err, "RebuildFTS")
	assert.Equal(int64(1), n, "RebuildFTS rows")

	assert.True(f.Store.FTS5Available(), "FTS5Available() after rebuild")

	assertFTSContains(t, f.Store, "cherry", 1)
}

// TestStore_RebuildFTS_AfterTableDropped verifies that RebuildFTS recreates
// messages_fts from scratch when the table is missing entirely — the
// post-DROP state from the manual recovery procedure in issue #287.
// This test is SQLite-only: it drops the messages_fts virtual table, which
// has no equivalent on PostgreSQL (PG stores FTS inline on messages.search_fts;
// the PG DROP INDEX + recreate path is exercised by FTSRebuildSchema directly).
func TestStore_RebuildFTS_AfterTableDropped(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	testutil.SkipIfPostgres(t, "SQLite-only: drops the messages_fts virtual table; PG FTS is a column on messages, not a separate table — the PG DROP INDEX path is covered by dialect_pg tests")
	f := storetest.New(t)
	if !f.Store.FTS5Available() {
		t.Skip("FTS5 not available")
	}

	msgID := f.CreateMessage("rebuild-dropped")
	require.NoError(f.Store.UpsertMessageBody(msgID,
		sql.NullString{String: "date square confection", Valid: true}, sql.NullString{}),
		"UpsertMessageBody")

	_, err := f.Store.DB().Exec("DROP TABLE messages_fts")
	require.NoError(err, "DROP TABLE messages_fts")

	n, err := f.Store.RebuildFTS(nil)
	require.NoError(err, "RebuildFTS")
	assert.Equal(int64(1), n, "RebuildFTS rows")

	var count int
	require.NoError(f.Store.DB().QueryRow(
		"SELECT COUNT(*) FROM messages_fts WHERE messages_fts MATCH 'confection'").Scan(&count),
		"FTS MATCH confection")
	assert.Equal(1, count, "match 'confection'")
}

// TestStore_RebuildFTS_ReportsProgress verifies the progress callback is
// invoked with monotonic (done, total) values.
// Runs on both SQLite and PostgreSQL — progress reporting is dialect-agnostic.
func TestStore_RebuildFTS_ReportsProgress(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	f := storetest.New(t)
	if !f.Store.FTS5Available() {
		t.Skip("FTS5 not available")
	}

	ids := f.CreateMessages(3)
	for i, id := range ids {
		require.NoError(f.Store.UpsertMessageBody(id,
			sql.NullString{String: "progress body", Valid: true}, sql.NullString{}),
			"UpsertMessageBody")
		_ = i
	}

	var calls int
	var lastDone, lastTotal int64
	_, err := f.Store.RebuildFTS(func(done, total int64) {
		calls++
		assert.Positive(total, "progress total")
		assert.GreaterOrEqual(done, lastDone, "progress done went backwards: %d -> %d", lastDone, done)
		lastDone, lastTotal = done, total
	})
	require.NoError(err, "RebuildFTS")

	assert.NotZero(calls, "progress callback never invoked")
	assert.Equal(lastTotal, lastDone, "final progress should have done == total")
}
