package store_test

import (
	"testing"
	"time"

	assertpkg "github.com/stretchr/testify/assert"
	requirepkg "github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

// TestScanSource_NullLastSyncAt_Valid verifies that a new source with NULL
// last_sync_at is handled correctly (Valid=false).
func TestScanSource_NullLastSyncAt_Valid(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	st := testutil.NewTestStore(t)

	// Create a fresh source (should have NULL last_sync_at)
	source, err := st.GetOrCreateSource("gmail", "null-lastsync@example.com")
	require.NoError(err, "GetOrCreateSource")

	// Retrieve it - should work fine with NULL last_sync_at
	retrieved, err := st.GetSourceByIdentifier("null-lastsync@example.com")
	require.NoError(err, "GetSourceByIdentifier")

	require.NotNil(retrieved, "expected source, got nil")
	assert.Equal(source.ID, retrieved.ID, "ID")
	assert.False(retrieved.LastSyncAt.Valid, "LastSyncAt should not be valid for a new source")
}

// TestScanSyncRun_ZeroTime verifies that the scanner handles timestamps that
// the go-sqlite3 driver normalizes to zero time (from invalid input).
// The driver converts unparseable DATETIME values to "0001-01-01T00:00:00Z".
func TestScanSyncRun_ZeroTime(t *testing.T) {
	require := requirepkg.New(t)
	testutil.SkipIfPostgres(t, "tests go-sqlite3 driver normalization of invalid DATETIME strings to zero time; PG TIMESTAMPTZ rejects invalid strings outright")
	f := storetest.New(t)

	syncID := f.StartSync()

	// Corrupt the started_at with an invalid value.
	// go-sqlite3 normalizes this to "0001-01-01T00:00:00Z" for DATETIME columns.
	_, err := f.Store.DB().Exec(`
		UPDATE sync_runs SET started_at = 'invalid-timestamp' WHERE id = ?
	`, syncID)
	require.NoError(err, "corrupt started_at")

	// GetActiveSync should still work - the driver normalizes to zero time
	run, err := f.Store.GetActiveSync(f.Source.ID)
	require.NoError(err, "GetActiveSync")

	require.NotNil(run, "expected sync run, got nil")

	// The driver normalizes invalid timestamps to zero time
	assertpkg.True(t, run.StartedAt.IsZero(), "StartedAt = %v, expected zero time", run.StartedAt)
}

// TestScanSource_ZeroTime verifies that sources with timestamps that the driver
// normalizes to zero time are handled correctly.
func TestScanSource_ZeroTime(t *testing.T) {
	require := requirepkg.New(t)
	testutil.SkipIfPostgres(t, "tests go-sqlite3 driver normalization of invalid DATETIME strings to zero time; PG TIMESTAMPTZ rejects invalid strings outright")
	st := testutil.NewTestStore(t)

	// Create a source
	source, err := st.GetOrCreateSource("gmail", "zerotime@example.com")
	require.NoError(err, "GetOrCreateSource")

	// Corrupt the created_at with an invalid value.
	// go-sqlite3 normalizes this to "0001-01-01T00:00:00Z" for DATETIME columns.
	_, err = st.DB().Exec(`
		UPDATE sources SET created_at = 'garbage' WHERE id = ?
	`, source.ID)
	require.NoError(err, "corrupt created_at")

	// Should still work - the driver normalizes to zero time
	retrieved, err := st.GetSourceByIdentifier("zerotime@example.com")
	require.NoError(err, "GetSourceByIdentifier")

	require.NotNil(retrieved, "expected source, got nil")

	// The driver normalizes invalid timestamps to zero time
	assertpkg.True(t, retrieved.CreatedAt.IsZero(), "CreatedAt = %v, expected zero time", retrieved.CreatedAt)
}

// TestParseDBTime_MultipleFormats verifies that the timestamp parser accepts
// both SQLite datetime('now') format and RFC3339 format from go-sqlite3.
func TestParseDBTime_MultipleFormats(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	f := storetest.New(t)

	// Start a sync (uses datetime('now') which go-sqlite3 normalizes to RFC3339)
	syncID := f.StartSync()

	// GetActiveSync should parse the RFC3339 timestamp successfully
	run, err := f.Store.GetActiveSync(f.Source.ID)
	require.NoError(err, "GetActiveSync")

	require.NotNil(run, "expected sync run, got nil")
	assert.Equal(syncID, run.ID, "ID")

	// StartedAt should be recent (within last minute)
	age := time.Since(run.StartedAt)
	assert.GreaterOrEqual(age, time.Duration(0), "StartedAt age = %v, expected recent time", age)
	assert.LessOrEqual(age, time.Minute, "StartedAt age = %v, expected recent time", age)
}

// TestListSources_ParsesTimestamps verifies that ListSources correctly parses
// timestamps for all returned sources.
func TestListSources_ParsesTimestamps(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	st := testutil.NewTestStore(t)

	// Create a few sources
	emails := []string{"user1@example.com", "user2@example.com", "user3@example.com"}
	for _, email := range emails {
		_, err := st.GetOrCreateSource("gmail", email)
		require.NoError(err, "GetOrCreateSource")
	}

	// ListSources should parse timestamps correctly
	sources, err := st.ListSources("gmail")
	require.NoError(err, "ListSources")

	require.Len(sources, 3)

	for _, src := range sources {
		// CreatedAt should be recent
		age := time.Since(src.CreatedAt)
		assert.GreaterOrEqual(age, time.Duration(0), "source %d: CreatedAt age = %v, expected recent time", src.ID, age)
		assert.LessOrEqual(age, time.Minute, "source %d: CreatedAt age = %v, expected recent time", src.ID, age)
	}
}

// TestScanSource_UnrecognizedFormat verifies that parseDBTime returns an error
// with helpful context when encountering a truly unrecognized timestamp format.
func TestScanSource_UnrecognizedFormat(t *testing.T) {
	badTimestamp := "not-a-date-at-all"

	// Verify that parseDBTime rejects unrecognized formats
	_, err := store.ParseDBTime(badTimestamp)
	requirepkg.Error(t, err, "expected error for unrecognized timestamp format")

	// Error should include the bad value for debugging
	assertpkg.ErrorContains(t, err, badTimestamp, "error should include the bad value")
}

// TestScanSource_NullRequiredTimestamp verifies that parseRequiredTime returns
// an error when a required timestamp field (created_at/updated_at) is NULL.
func TestScanSource_NullRequiredTimestamp(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	st := testutil.NewTestStore(t)

	// Create a source
	source, err := st.GetOrCreateSource("gmail", "nullrequired@example.com")
	require.NoError(err, "GetOrCreateSource")

	// Corrupt created_at to NULL (violates expected schema invariant)
	_, err = st.DB().Exec(st.Rebind(`UPDATE sources SET created_at = NULL WHERE id = ?`), source.ID)
	require.NoError(err, "set created_at to NULL")

	// Attempting to retrieve should fail with a clear error
	_, err = st.GetSourceByIdentifier("nullrequired@example.com")
	require.Error(err, "expected error for NULL required timestamp")

	// Error should mention the field name and that it's NULL
	require.ErrorContains(err, "created_at", "error should mention field")
	assert.ErrorContains(err, "NULL", "error should mention NULL status")
}

func TestStore_HasAnyActiveSync(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	f := storetest.New(t)

	running, err := f.Store.HasAnyActiveSync()
	require.NoError(err, "HasAnyActiveSync (initial)")
	assert.False(running, "expected no active sync on fresh DB")

	syncID := f.StartSync()

	running, err = f.Store.HasAnyActiveSync()
	require.NoError(err, "HasAnyActiveSync (after StartSync)")
	assert.True(running, "expected active sync after StartSync")

	// A second StartSync on the same source marks the prior one failed, but
	// itself is running.
	_ = f.StartSync()
	running, err = f.Store.HasAnyActiveSync()
	require.NoError(err, "HasAnyActiveSync (after second StartSync)")
	assert.True(running, "expected an active sync after second StartSync")

	// Mark the latest sync as completed.
	_, err = f.Store.DB().Exec(
		`UPDATE sync_runs SET status = 'completed' WHERE status = 'running'`,
	)
	require.NoError(err, "mark sync completed")

	running, err = f.Store.HasAnyActiveSync()
	require.NoError(err, "HasAnyActiveSync (after completion)")
	assert.False(running, "expected no active sync after completion")

	_ = syncID
}
