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

// TestStore_RebuildFTS_HappyPath verifies RebuildFTS on a healthy database
// recreates the FTS index with correct searchable content.
func TestStore_RebuildFTS_HappyPath(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	testutil.SkipIfPostgres(t, "RebuildFTS is SQLite-specific (drop/recreate messages_fts vtable); PG RebuildFTS is not yet implemented (PR4 scope)")
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

	var count int
	require.NoError(f.Store.DB().QueryRow(
		"SELECT COUNT(*) FROM messages_fts WHERE messages_fts MATCH 'banana'").Scan(&count),
		"FTS MATCH banana")
	assert.Equal(1, count, "match 'banana'")

	require.NoError(f.Store.DB().QueryRow(
		"SELECT COUNT(*) FROM messages_fts WHERE messages_fts MATCH 'alice'").Scan(&count),
		"FTS MATCH alice")
	assert.Equal(1, count, "match 'alice'")
}

// TestStore_RebuildFTS_BypassesAvailabilityFlag verifies the critical
// guarantee that RebuildFTS ignores the cached fts5Available flag. A corrupt
// FTS5 shadow table causes the availability probe to fail, which is exactly
// when the rebuild is needed — BackfillFTS would short-circuit here, but
// RebuildFTS must not.
func TestStore_RebuildFTS_BypassesAvailabilityFlag(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	testutil.SkipIfPostgres(t, "RebuildFTS is SQLite-specific (drop/recreate messages_fts vtable); PG RebuildFTS is not yet implemented (PR4 scope)")
	f := storetest.New(t)
	if !f.Store.FTS5Available() {
		t.Skip("FTS5 not available")
	}

	msgID := f.CreateMessage("rebuild-bypass")
	require.NoError(f.Store.UpsertMessageBody(msgID,
		sql.NullString{String: "cherry tart dessert", Valid: true}, sql.NullString{}),
		"UpsertMessageBody")

	// Force the cached flag false to simulate a probe that saw a corrupt
	// shadow table and returned false at InitSchema time.
	store.SetFTS5AvailableForTest(f.Store, false)

	n, err := f.Store.RebuildFTS(nil)
	require.NoError(err, "RebuildFTS")
	assert.Equal(int64(1), n, "RebuildFTS rows")

	assert.True(f.Store.FTS5Available(), "FTS5Available() after rebuild")

	var count int
	require.NoError(f.Store.DB().QueryRow(
		"SELECT COUNT(*) FROM messages_fts WHERE messages_fts MATCH 'cherry'").Scan(&count),
		"FTS MATCH cherry")
	assert.Equal(1, count, "match 'cherry'")
}

// TestStore_RebuildFTS_AfterTableDropped verifies that RebuildFTS recreates
// messages_fts from scratch when the table is missing entirely — the
// post-DROP state from the manual recovery procedure in issue #287.
func TestStore_RebuildFTS_AfterTableDropped(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	testutil.SkipIfPostgres(t, "RebuildFTS is SQLite-specific (drop/recreate messages_fts vtable); PG RebuildFTS is not yet implemented (PR4 scope)")
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
func TestStore_RebuildFTS_ReportsProgress(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	testutil.SkipIfPostgres(t, "RebuildFTS is SQLite-specific (drop/recreate messages_fts vtable); PG RebuildFTS is not yet implemented (PR4 scope)")
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
