package store_test

import (
	"context"
	"database/sql"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/operations"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

// TestScanSource_NullLastSyncAt_Valid verifies that a new source with NULL
// last_sync_at is handled correctly (Valid=false).
func TestScanSource_NullLastSyncAt_Valid(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
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
	require := require.New(t)
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
	assert.True(t, run.StartedAt.IsZero(), "StartedAt = %v, expected zero time", run.StartedAt)
}

func TestSyncRunRecoveryTerminalizesOnlyRunningRows(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	usefulSource, err := st.GetOrCreateSource("gmail", "useful-recovery@example.test")
	require.NoError(err)
	emptySource, err := st.GetOrCreateSource("gmail", "empty-recovery@example.test")
	require.NoError(err)
	terminalSource, err := st.GetOrCreateSource("gmail", "terminal-recovery@example.test")
	require.NoError(err)

	usefulID, err := st.StartSync(usefulSource.ID, "incremental")
	require.NoError(err)
	require.NoError(st.UpdateSyncCheckpoint(usefulID, &store.Checkpoint{
		MessagesProcessed: 3, MessagesAdded: 1, MessagesUpdated: 1,
	}))
	emptyID, err := st.StartSync(emptySource.ID, "incremental")
	require.NoError(err)
	terminalID, err := st.StartSync(terminalSource.ID, "incremental")
	require.NoError(err)
	require.NoError(st.CompleteSync(terminalID, "terminal-cursor"))

	recoveredAt := time.Now().UTC().Add(time.Second).Truncate(time.Millisecond)
	recovered, err := st.RecoverSyncRunsContext(t.Context(), recoveredAt)
	require.NoError(err)
	assert.Equal(int64(2), recovered)
	recovered, err = st.RecoverSyncRunsContext(t.Context(), recoveredAt.Add(time.Hour))
	require.NoError(err)
	assert.Zero(recovered, "recovery must be idempotent")

	var status, message string
	var completed time.Time
	require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`
		SELECT status, error_message, completed_at FROM sync_runs WHERE id = ?`), emptyID).
		Scan(&status, &message, &completed))
	assert.Equal(store.SyncStatusFailed, status)
	assert.Equal("daemon_restarted", message)
	assert.Equal(recoveredAt, completed.UTC())

	var terminalStatus string
	require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(
		`SELECT status FROM sync_runs WHERE id = ?`), terminalID).Scan(&terminalStatus))
	assert.Equal(store.SyncStatusCompleted, terminalStatus)
	var added, updated int64
	require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(
		`SELECT messages_added, messages_updated FROM sync_runs WHERE id = ?`), usefulID).
		Scan(&added, &updated))
	assert.Equal(int64(1), added)
	assert.Equal(int64(1), updated)
	snapshot, err := st.ListRuns(t.Context(), operations.Query{
		Kinds: []operations.Kind{operations.KindSourceSync}, Limit: 10,
	})
	require.NoError(err)
	states := make(map[int64]operations.State, len(snapshot.Runs))
	for _, run := range snapshot.Runs {
		id, ok := run.ID.Int64()
		require.True(ok)
		states[id] = run.State
	}
	assert.Equal(operations.StatePartial, states[usefulID])
	assert.Equal(operations.StateFailed, states[emptyID])
	assert.Equal(operations.StateSucceeded, states[terminalID])
}

// TestScanSource_ZeroTime verifies that sources with timestamps that the driver
// normalizes to zero time are handled correctly.
func TestScanSource_ZeroTime(t *testing.T) {
	require := require.New(t)
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
	assert.True(t, retrieved.CreatedAt.IsZero(), "CreatedAt = %v, expected zero time", retrieved.CreatedAt)
}

// TestParseDBTime_MultipleFormats verifies that the timestamp parser accepts
// both SQLite datetime('now') format and RFC3339 format from go-sqlite3.
func TestParseDBTime_MultipleFormats(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
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

func TestStore_GetLatestSync(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)

	_, err := f.Store.GetLatestSync(f.Source.ID)
	require.ErrorIs(err, store.ErrSyncRunNotFound, "GetLatestSync before any runs")

	firstID := f.StartSync()
	require.NoError(f.Store.CompleteSync(firstID, "history-1"), "CompleteSync first")

	secondID := f.StartSync()

	run, err := f.Store.GetLatestSync(f.Source.ID)
	require.NoError(err, "GetLatestSync")
	require.NotNil(run, "expected sync run")
	assert.Equal(secondID, run.ID, "ID")
	assert.Equal(store.SyncStatusRunning, run.Status, "Status")
}

func TestStore_StartSyncRejectsConcurrentRun(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)

	activeID := f.StartSync()
	_, err := f.Store.StartSync(f.Source.ID, "full")
	require.ErrorIs(err, store.ErrSyncAlreadyActive)

	run, err := f.Store.GetActiveSync(f.Source.ID)
	require.NoError(err)
	assert.Equal(activeID, run.ID)

	require.NoError(f.Store.CompleteSync(activeID, "cursor"))
	_, err = f.Store.StartSync(f.Source.ID, "full")
	require.NoError(err)
}

func TestStore_CompleteSyncWriteFailureReleasesExecution(t *testing.T) {
	requirements := require.New(t)
	checks := assert.New(t)
	f := storetest.New(t)

	firstID, err := f.Store.StartSync(f.Source.ID, "full")
	requirements.NoError(err)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	err = f.Store.CompleteSyncContext(ctx, firstID, "cursor")
	requirements.ErrorIs(err, context.Canceled)

	secondID, err := f.Store.StartSync(f.Source.ID, "full")
	requirements.NoError(err)
	t.Cleanup(func() { _ = f.Store.FailSync(secondID, "test complete") })
	var firstStatus string
	requirements.NoError(f.Store.DB().QueryRow(
		f.Store.Rebind(`SELECT status FROM sync_runs WHERE id = ?`), firstID,
	).Scan(&firstStatus))
	checks.Equal(store.SyncStatusFailed, firstStatus)
}

func TestStore_StartSyncRejectsConcurrentRunAcrossSQLiteStores(t *testing.T) {
	requirements := require.New(t)
	testutil.SkipIfPostgres(t, "exercises the cross-process SQLite file lock")
	dbPath := filepath.Join(t.TempDir(), "archive.db")
	first, err := store.OpenForTest(dbPath)
	requirements.NoError(err)
	t.Cleanup(func() { _ = first.Close() })
	requirements.NoError(first.InitSchema())
	source, err := first.GetOrCreateSource("gmail", "lock-owner@example.com")
	requirements.NoError(err)

	second, err := store.OpenForTest(dbPath)
	requirements.NoError(err)
	t.Cleanup(func() { _ = second.Close() })

	firstRun, err := first.StartSync(source.ID, "full")
	requirements.NoError(err)
	_, err = second.StartSync(source.ID, "full")
	requirements.ErrorIs(err, store.ErrSyncAlreadyActive)

	requirements.NoError(first.CompleteSync(firstRun, "cursor"))
	secondRun, err := second.StartSync(source.ID, "full")
	requirements.NoError(err)
	requirements.NoError(second.FailSync(secondRun, "test complete"))
}

func TestStore_StartSyncUsesFilesystemPathForSQLiteFileURI(t *testing.T) {
	requirements := require.New(t)
	testutil.SkipIfPostgres(t, "exercises SQLite file URI lock resolution")
	dbPath := filepath.Join(t.TempDir(), "archive.db")
	uriPath := filepath.ToSlash(dbPath)
	if filepath.VolumeName(dbPath) != "" && !strings.HasPrefix(uriPath, "/") {
		uriPath = "/" + uriPath
	}
	dsn := (&url.URL{Scheme: "file", Path: uriPath}).String()

	first, err := store.OpenForTest(dsn)
	requirements.NoError(err)
	t.Cleanup(func() { _ = first.Close() })
	requirements.NoError(first.InitSchema())
	source, err := first.GetOrCreateSource("gmail", "uri-lock-owner@example.com")
	requirements.NoError(err)

	second, err := store.OpenForTest(dbPath)
	requirements.NoError(err)
	t.Cleanup(func() { _ = second.Close() })

	firstRun, err := first.StartSync(source.ID, "full")
	requirements.NoError(err)
	_, err = second.StartSync(source.ID, "full")
	requirements.ErrorIs(err, store.ErrSyncAlreadyActive)
	requirements.NoError(first.FailSync(firstRun, "test complete"))
}

func TestStore_StartSyncRecoversRunWhoseOwnerClosed(t *testing.T) {
	requirements := require.New(t)
	checks := assert.New(t)
	first := testutil.NewTestStore(t)
	source, err := first.GetOrCreateSource("gmail", "recovery@example.com")
	requirements.NoError(err)
	_, err = first.CreateSyncOperation(source.ID, "abandoned-operation")
	requirements.NoError(err)
	abandonedRun, err := first.StartSyncOperation(source.ID, "abandoned-operation")
	requirements.NoError(err)
	requirements.NoError(first.UpdateSyncCheckpoint(abandonedRun, &store.Checkpoint{
		PageToken:         "resume-token",
		MessagesProcessed: 17,
		MessagesAdded:     11,
	}))
	requirements.NoError(first.Close())

	second, err := store.OpenForTest(store.DBPathForTest(first))
	requirements.NoError(err)
	t.Cleanup(func() { _ = second.Close() })
	active, err := second.GetActiveSync(source.ID)
	requirements.ErrorIs(err, store.ErrSyncRunNotFound)
	requirements.Nil(active)
	recoveryRun, err := second.StartSync(source.ID, "full")
	requirements.NoError(err)

	op, err := second.GetSyncOperation("abandoned-operation")
	requirements.NoError(err)
	checks.Equal("failed", op.Status)
	checks.True(op.FinishedAt.Valid)
	requirements.Len(op.Runs, 1)
	checks.Equal(store.SyncStatusFailed, op.Runs[0].Status)
	checks.Equal("resume-token", op.Runs[0].CursorBefore.String)
	checks.Equal(int64(17), op.Runs[0].MessagesProcessed)
	checks.Equal(int64(11), op.Runs[0].MessagesAdded)
	checks.Equal("sync worker exited before recording completion", op.Runs[0].ErrorMessage.String)

	active, err = second.GetActiveSync(source.ID)
	requirements.NoError(err)
	checks.Equal(recoveryRun, active.ID)
	requirements.NoError(second.FailSync(recoveryRun, "test complete"))
}

func TestStore_GetActiveSyncOnReadOnlyStoreDoesNotRecover(t *testing.T) {
	requirements := require.New(t)
	dbPath := filepath.Join(t.TempDir(), "read-only-active.db")
	writable, err := store.OpenForTest(dbPath)
	requirements.NoError(err)
	requirements.NoError(writable.InitSchema())
	source, err := writable.GetOrCreateSource("gmail", "read-only-active@example.test")
	requirements.NoError(err)
	runID, err := writable.StartSync(source.ID, "full")
	requirements.NoError(err)
	requirements.NoError(writable.Close())

	readOnly, err := store.OpenReadOnly(dbPath)
	requirements.NoError(err)
	t.Cleanup(func() { _ = readOnly.Close() })
	active, err := readOnly.GetActiveSync(source.ID)
	requirements.NoError(err)
	requirements.Equal(runID, active.ID)
}

func TestStore_SyncExecutionRetainsOwnershipAcrossRuns(t *testing.T) {
	requirements := require.New(t)
	first := testutil.NewTestStore(t)
	source, err := first.GetOrCreateSource("gmail", "multi-phase-owner@example.com")
	requirements.NoError(err)
	second, err := store.OpenForTest(store.DBPathForTest(first))
	requirements.NoError(err)
	t.Cleanup(func() { _ = second.Close() })

	_, err = first.CreateSyncOperation(source.ID, "multi-phase-operation")
	requirements.NoError(err)
	execution, err := first.AcquireSyncExecutionContext(t.Context(), source.ID)
	requirements.NoError(err)
	t.Cleanup(func() { _ = execution.Release() })
	fullRun, err := execution.StartSyncContext(t.Context(), "full", "multi-phase-operation")
	requirements.NoError(err)
	requirements.NoError(first.CompleteSync(fullRun, "full-cursor"))

	_, err = second.StartSync(source.ID, "incremental")
	requirements.ErrorIs(err, store.ErrSyncAlreadyActive)

	catchupRun, err := execution.StartSyncContext(t.Context(), "incremental", "multi-phase-operation")
	requirements.NoError(err)
	requirements.NoError(first.CompleteSync(catchupRun, "catchup-cursor"))
	requirements.NoError(execution.Release())
	requirements.NoError(first.FinishSyncOperation("multi-phase-operation", "done"))

	nextRun, err := second.StartSync(source.ID, "incremental")
	requirements.NoError(err)
	requirements.NoError(second.FailSync(nextRun, "test complete"))
}

func TestStore_UnfinishedSyncOperationIsRecoveredAtDaemonStartup(t *testing.T) {
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("gmail", "operation-recovery@example.com")
	requirements.NoError(err)
	_, err = st.CreateSyncOperation(source.ID, "abandoned-terminal-operation")
	requirements.NoError(err)
	execution, err := st.AcquireSyncExecutionContext(t.Context(), source.ID)
	requirements.NoError(err)
	runID, err := execution.StartSyncContext(t.Context(), "full", "abandoned-terminal-operation")
	requirements.NoError(err)
	requirements.NoError(st.CompleteSync(runID, "final-cursor"))
	requirements.NoError(execution.Release())

	op, err := st.GetSyncOperation("abandoned-terminal-operation")
	requirements.NoError(err)
	requirements.Equal("running", op.Status)
	requirements.False(op.FinishedAt.Valid)

	failed, err := st.FailUnfinishedSyncOperationsContext(t.Context())
	requirements.NoError(err)
	requirements.Equal(int64(1), failed)
	op, err = st.GetSyncOperation("abandoned-terminal-operation")
	requirements.NoError(err)
	requirements.Equal("failed", op.Status)
	requirements.True(op.FinishedAt.Valid)
	requirements.Len(op.Runs, 1)
	requirements.Equal(store.SyncStatusCompleted, op.Runs[0].Status)
}

func TestStore_UnfinishedSyncOperationRecoveryFailsRunningRun(t *testing.T) {
	requirements := require.New(t)
	checks := assert.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("gmail", "running-operation-recovery@example.com")
	requirements.NoError(err)
	_, err = st.CreateSyncOperation(source.ID, "abandoned-running-operation")
	requirements.NoError(err)
	execution, err := st.AcquireSyncExecutionContext(t.Context(), source.ID)
	requirements.NoError(err)
	runID, err := execution.StartSyncContext(t.Context(), "full", "abandoned-running-operation")
	requirements.NoError(err)
	requirements.NoError(st.UpdateSyncCheckpoint(runID, &store.Checkpoint{
		PageToken:         "resume-token",
		MessagesProcessed: 9,
	}))
	requirements.NoError(execution.Release())

	failed, err := st.FailUnfinishedSyncOperationsContext(t.Context())
	requirements.NoError(err)
	checks.Equal(int64(1), failed)
	op, err := st.GetSyncOperation("abandoned-running-operation")
	requirements.NoError(err)
	checks.Equal(store.SyncStatusFailed, op.Status)
	requirements.Len(op.Runs, 1)
	checks.Equal(runID, op.Runs[0].ID)
	checks.Equal(store.SyncStatusFailed, op.Runs[0].Status)
	checks.True(op.Runs[0].CompletedAt.Valid)
	checks.Equal("sync worker exited before recording completion", op.Runs[0].ErrorMessage.String)
	checks.Equal("resume-token", op.Runs[0].CursorBefore.String)
	checks.Equal(int64(9), op.Runs[0].MessagesProcessed)
	_, err = st.GetActiveSync(source.ID)
	requirements.ErrorIs(err, store.ErrSyncRunNotFound)
}

func TestStore_StartSyncRejectsConcurrentRunAcrossPostgresStores(t *testing.T) {
	requirements := require.New(t)
	if !store.IsPostgresURL(os.Getenv("MSGVAULT_TEST_DB")) {
		t.Skip("PostgreSQL integration test")
	}
	first := testutil.NewTestStore(t)
	source, err := first.GetOrCreateSource("gmail", "lock-owner@example.com")
	requirements.NoError(err)
	second, err := store.OpenForTest(store.DBPathForTest(first))
	requirements.NoError(err)
	t.Cleanup(func() { _ = second.Close() })

	firstRun, err := first.StartSync(source.ID, "full")
	requirements.NoError(err)
	_, err = second.StartSync(source.ID, "full")
	requirements.ErrorIs(err, store.ErrSyncAlreadyActive)

	requirements.NoError(first.CompleteSync(firstRun, "cursor"))
	secondRun, err := second.StartSync(source.ID, "full")
	requirements.NoError(err)
	requirements.NoError(second.FailSync(secondRun, "test complete"))
}

func TestStore_CompleteSyncAndUpdateSourceCursorRejectsSupersededRunAtomically(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	require.NoError(f.Store.UpdateSourceSyncCursor(f.Source.ID, "baseline-cursor"))

	oldID := f.StartSync()
	require.NoError(f.Store.FailSync(oldID, "worker stopped"))
	newID := f.StartSync()

	err := f.Store.CompleteSyncAndUpdateSourceCursorContext(
		t.Context(), oldID, f.Source.ID, "stale-cursor",
	)
	require.ErrorIs(err, store.ErrSyncRunSuperseded)

	source, err := f.Store.GetSourceByID(f.Source.ID)
	require.NoError(err)
	assert.Equal("baseline-cursor", source.SyncCursor.String)

	require.NoError(f.Store.CompleteSyncAndUpdateSourceCursorContext(
		t.Context(), newID, f.Source.ID, "fresh-cursor",
	))
	source, err = f.Store.GetSourceByID(f.Source.ID)
	require.NoError(err)
	assert.Equal("fresh-cursor", source.SyncCursor.String)
	run, err := f.Store.GetLatestSync(f.Source.ID)
	require.NoError(err)
	assert.Equal(store.SyncStatusCompleted, run.Status)
	assert.Equal("fresh-cursor", run.CursorAfter.String)
}

func TestStore_FailSyncAndClearSourceCursorRejectsSupersededRunAtomically(t *testing.T) {
	requirements := require.New(t)
	checks := assert.New(t)
	f := storetest.New(t)
	requirements.NoError(f.Store.UpdateSourceSyncCursor(f.Source.ID, "baseline-cursor"))

	oldID := f.StartSync()
	requirements.NoError(f.Store.FailSync(oldID, "worker stopped"))
	newID := f.StartSync()
	err := f.Store.FailSyncAndClearSourceCursorContext(
		t.Context(), oldID, f.Source.ID, "expired cursor",
	)
	requirements.ErrorIs(err, store.ErrSyncRunSuperseded)
	source, err := f.Store.GetSourceByID(f.Source.ID)
	requirements.NoError(err)
	checks.Equal("baseline-cursor", source.SyncCursor.String)

	requirements.NoError(f.Store.FailSyncAndClearSourceCursorContext(
		t.Context(), newID, f.Source.ID, "expired cursor",
	))
	source, err = f.Store.GetSourceByID(f.Source.ID)
	requirements.NoError(err)
	checks.Empty(source.SyncCursor.String)
	run, err := f.Store.GetLatestSync(f.Source.ID)
	requirements.NoError(err)
	checks.Equal(store.SyncStatusFailed, run.Status)
	checks.Equal("expired cursor", run.ErrorMessage.String)
}

func TestScopedStoreRejectsEveryImporterMutationAfterSupersession(t *testing.T) {
	requirements := require.New(t)
	checks := assert.New(t)
	f := storetest.New(t)
	messageID := f.CreateMessage("generation-fence-message")
	var conversationID int64
	requirements.NoError(f.Store.DB().QueryRow(f.Store.Rebind(
		`SELECT conversation_id FROM messages WHERE id = ?`), messageID,
	).Scan(&conversationID))
	participantID := f.EnsureParticipant("reactor@example.test", "Reactor", "example.test")
	oldID := f.StartSync()
	stale := f.Store.ScopedToSync(f.Source.ID, oldID)
	requirements.NoError(f.Store.FailSync(oldID, "worker stopped"))
	_ = f.StartSync()

	tests := []struct {
		name  string
		write func() error
	}{
		{name: "email conversation", write: func() error {
			_, err := stale.EnsureConversation(f.Source.ID, "stale-email-thread", "Stale")
			return err
		}},
		{name: "conversation", write: func() error {
			_, err := stale.EnsureConversationWithType(f.Source.ID, "stale-thread", "chat", "Stale")
			return err
		}},
		{name: "raw message", write: func() error {
			return stale.UpsertMessageRawWithFormat(messageID, []byte("stale"), "json")
		}},
		{name: "attachment", write: func() error {
			return stale.UpsertAttachmentRecord(t.Context(), messageID, store.AttachmentWrite{
				Filename: "stale.txt", MIMEType: "text/plain", Size: 5,
			})
		}},
		{name: "reaction", write: func() error {
			return stale.UpsertReaction(messageID, participantID, "emoji", "stale", time.Now().UTC())
		}},
		{name: "FTS", write: func() error {
			return stale.UpsertFTS(messageID, "stale", "stale", "", "", "")
		}},
		{name: "conversation stats", write: func() error {
			return stale.RecomputeConversationStats(f.Source.ID)
		}},
		{name: "checkpoint", write: func() error {
			return stale.UpdateSyncCheckpoint(oldID, &store.Checkpoint{PageToken: "stale"})
		}},
		{name: "history recovery handoff", write: func() error {
			return stale.PinSyncHandoffCursorContext(t.Context(), oldID, "12345")
		}},
		{name: "attachment metadata", write: func() error {
			return stale.UpdateAttachmentMediaMetadataContext(
				t.Context(), messageID, "hash", "image", sql.NullInt64{}, sql.NullInt64{}, sql.NullInt64{})
		}},
		{name: "reply link", write: func() error {
			return stale.SetMessageReplyContext(t.Context(), messageID, messageID)
		}},
		{name: "edited flag", write: func() error {
			return stale.SetMessageEdited(messageID)
		}},
		{name: "conversation metadata", write: func() error {
			return stale.SetConversationMetadata(conversationID, sql.NullString{String: `{}`, Valid: true})
		}},
		{name: "conversation member count", write: func() error {
			return stale.SetConversationMemberCount(conversationID, 2)
		}},
		{name: "remove message labels", write: func() error {
			return stale.RemoveMessageLabels(messageID, []int64{1})
		}},
		{name: "legacy reply link", write: func() error {
			return stale.SetReplyTo(f.Source.ID, "generation-fence-message", "generation-fence-message")
		}},
		{name: "legacy attachment cleanup", write: func() error {
			return stale.DeleteLegacyHashlessAttachmentsContext(t.Context(), messageID)
		}},
		{name: "hash attachment cleanup", write: func() error {
			return stale.DeleteUnstoredAttachmentByHashContext(t.Context(), messageID, "hash")
		}},
		{name: "metadata attachment cleanup", write: func() error {
			return stale.DeleteUnstoredAttachmentByMetadataContext(t.Context(), messageID, "stale", "text/plain")
		}},
		{name: "source import item", write: func() error {
			return stale.UpsertSourceImportItem(store.SourceImportItem{
				SourceID: f.Source.ID, Provider: "test", ProviderID: "stale-item",
				Name: "stale-item", Status: "imported",
			})
		}},
		{name: "source last sync", write: func() error {
			return stale.TouchSourceLastSyncAt(f.Source.ID)
		}},
		{name: "source display name", write: func() error {
			return stale.UpdateSourceDisplayName(f.Source.ID, "Stale Name")
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			require.New(t).ErrorIs(test.write(), store.ErrSyncRunSuperseded)
		})
	}
	var staleEmailThreads int
	requirements.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`SELECT COUNT(*) FROM conversations
		WHERE source_id = ? AND source_conversation_id = 'stale-email-thread'`),
		f.Source.ID).Scan(&staleEmailThreads))
	checks.Zero(staleEmailThreads)
}

func TestScopedSourceWriteMatchesStartSyncLockOrder(t *testing.T) {
	f := storetest.New(t)
	if !f.Store.IsPostgreSQL() {
		t.Skip("PostgreSQL-only sync source lock-order regression")
	}
	syncID := f.StartSync()
	scoped := f.Store.ScopedToSync(f.Source.ID, syncID)

	writeErr := forcePostgreSQLDeadlock(t.Context(), t, f.Store,
		postgreSQLRowLock{table: "sources", id: f.Source.ID},
		postgreSQLRowLock{table: "sync_runs", id: syncID},
		func(ctx context.Context) error {
			return scoped.UpdateSourceDisplayNameContext(ctx, f.Source.ID, "Updated Source")
		})
	require.NoError(t, writeErr,
		"a scoped source write must not reverse StartSync's source-then-run lock order")

	source, err := f.Store.GetSourceByID(f.Source.ID)
	require.NoError(t, err)
	assert.Equal(t, "Updated Source", source.DisplayName.String)
}

func TestSuccessfulSyncCoalescesTrackedPeopleOnce(t *testing.T) {
	tests := []struct {
		name     string
		complete func(t *testing.T, f personSweepJournalFixture, syncID int64)
	}{
		{
			name: "run completion",
			complete: func(t *testing.T, f personSweepJournalFixture, syncID int64) {
				t.Helper()
				require.NoError(t, f.store.CompleteSyncContext(
					t.Context(), syncID, "published-run-cursor"))
			},
		},
		{
			name: "source cursor publication",
			complete: func(t *testing.T, f personSweepJournalFixture, syncID int64) {
				t.Helper()
				require.NoError(t, f.store.CompleteSyncAndUpdateSourceCursorContext(
					t.Context(), syncID, f.sourceID, "published-source-cursor"))
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			checks := assert.New(t)
			requirements := require.New(t)
			f := newPersonSweepJournalFixture(t, true, false)
			deletePersonSweepWork(t, f.store, f.alicePersonID)
			syncID, err := f.store.StartSync(f.sourceID, "incremental")
			requirements.NoError(err)

			f.insertMessage(t, "successful-sync-first", "email", f.aliceID,
				time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC))
			f.insertMessage(t, "successful-sync-second", "email", f.aliceID,
				time.Date(2026, 8, 23, 12, 1, 0, 0, time.UTC))
			want := latestPersonSweepSequence(t, f.store)
			rows, _ := personSweepWorkState(t, f.store, f.alicePersonID)
			requirements.Zero(rows, "archive mutations must wait for a publication boundary")

			test.complete(t, f, syncID)
			rows, dirtyThrough := personSweepWorkState(t, f.store, f.alicePersonID)
			checks.Equal(1, rows)
			checks.Equal(want, dirtyThrough)
			lower, upper := personSweepSyncPublicationBounds(t, f.store, syncID)
			requirements.True(upper.Valid)
			checks.Less(lower, upper.Int64)
			checks.Equal(want, upper.Int64)

			deletePersonSweepWork(t, f.store, f.alicePersonID)
			noChangeSyncID, err := f.store.StartSync(f.sourceID, "incremental")
			requirements.NoError(err)
			test.complete(t, f, noChangeSyncID)
			rows, _ = personSweepWorkState(t, f.store, f.alicePersonID)
			checks.Zero(rows,
				"a later successful no-change sync must not replay historical journal debt")
			lower, upper = personSweepSyncPublicationBounds(t, f.store, noChangeSyncID)
			requirements.True(upper.Valid)
			checks.Equal(want, lower)
			checks.Equal(want, upper.Int64)
		})
	}
}

func TestSupersededSyncDoesNotCoalescePersonSweep(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	f := newPersonSweepJournalFixture(t, true, false)
	deletePersonSweepWork(t, f.store, f.alicePersonID)
	oldSyncID, err := f.store.StartSync(f.sourceID, "incremental")
	requirements.NoError(err)
	f.insertMessage(t, "superseded-sync-change", "email", f.aliceID,
		time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC))
	requirements.NoError(f.store.FailSync(oldSyncID, "worker stopped"))
	newSyncID, err := f.store.StartSync(f.sourceID, "incremental")
	requirements.NoError(err)

	err = f.store.CompleteSyncAndUpdateSourceCursorContext(
		t.Context(), oldSyncID, f.sourceID, "stale-cursor")
	requirements.ErrorIs(err, store.ErrSyncRunSuperseded)
	rows, _ := personSweepWorkState(t, f.store, f.alicePersonID)
	checks.Zero(rows)
	checks.NotEmpty(personSweepChangesAfter(t, f.store, f.alicePersonID, 0),
		"the committed partial import remains available to gap recovery")
	f.insertMessage(t, "failed-sync-partial-change", "email", f.aliceID,
		time.Date(2026, 8, 23, 12, 1, 0, 0, time.UTC))
	requirements.NoError(f.store.FailSync(newSyncID, "synthetic failed import"))
	rows, _ = personSweepWorkState(t, f.store, f.alicePersonID)
	checks.Zero(rows, "failed sync completion must leave committed mutations journal-only")
	_, failedUpper := personSweepSyncPublicationBounds(t, f.store, newSyncID)
	checks.False(failedUpper.Valid)

	recoverySyncID, err := f.store.StartSync(f.sourceID, "incremental")
	requirements.NoError(err)
	requirements.NoError(f.store.CompleteSyncAndUpdateSourceCursorContext(
		t.Context(), recoverySyncID, f.sourceID, "post-failure-no-change"))
	rows, _ = personSweepWorkState(t, f.store, f.alicePersonID)
	checks.Zero(rows,
		"a later no-change sync must not publish a failed run's partial mutations")
}

func personSweepSyncPublicationBounds(
	t *testing.T, st *store.Store, syncRunID int64,
) (int64, sql.NullInt64) {
	t.Helper()
	var (
		lower int64
		upper sql.NullInt64
	)
	require.NoError(t, st.DB().QueryRowContext(t.Context(), st.Rebind(`
		SELECT lower_sequence, upper_sequence
		FROM person_sweep_sync_publications
		WHERE sync_run_id = ?`), syncRunID).Scan(&lower, &upper))
	return lower, upper
}

func TestStore_GetLatestCheckpointedSyncFallsBackPastUncheckpointedRun(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	checkpointedID := f.StartSync()
	require.NoError(f.Store.UpdateSyncCheckpoint(checkpointedID, &store.Checkpoint{PageToken: "cursor-1"}))
	require.NoError(f.Store.FailSync(checkpointedID, "interrupted after checkpoint"))
	newerID := f.StartSync()

	run, err := f.Store.GetLatestCheckpointedSync(f.Source.ID)
	require.NoError(err)
	assert.Equal(checkpointedID, run.ID)
	assert.Equal("cursor-1", run.CursorBefore.String)

	require.NoError(f.Store.FailSync(newerID, "interrupted before checkpoint"))
	run, err = f.Store.GetLatestCheckpointedSync(f.Source.ID)
	require.NoError(err)
	assert.Equal(checkpointedID, run.ID, "an uncheckpointed failed run must not hide recoverable state")
}

func TestStore_GetLatestCheckpointedSyncNeverFallsBackPastCompletion(t *testing.T) {
	require := require.New(t)
	f := storetest.New(t)
	checkpointedID := f.StartSync()
	require.NoError(f.Store.UpdateSyncCheckpoint(checkpointedID, &store.Checkpoint{PageToken: "stale-cursor"}))
	require.NoError(f.Store.FailSync(checkpointedID, "interrupted"))
	completedID := f.StartSync()
	require.NoError(f.Store.CompleteSync(completedID, "completed-cursor"))
	_ = f.StartSync()

	_, err := f.Store.GetLatestCheckpointedSync(f.Source.ID)
	require.ErrorIs(err, store.ErrSyncRunNotFound)
}

func TestStore_CreateSyncOperationRejectsActiveSource(t *testing.T) {
	requirements := require.New(t)
	fixture := storetest.New(t)
	runID := fixture.StartSync()
	t.Cleanup(func() { _ = fixture.Store.FailSync(runID, "test complete") })

	_, err := fixture.Store.CreateSyncOperation(fixture.Source.ID, "competing-operation")
	requirements.ErrorIs(err, store.ErrSyncAlreadyActive)
}

func TestStore_PendingSyncOperationReservesSource(t *testing.T) {
	requirements := require.New(t)
	fixture := storetest.New(t)
	const operationID = "reserved-operation"
	_, err := fixture.Store.CreateSyncOperation(fixture.Source.ID, operationID)
	requirements.NoError(err)

	_, err = fixture.Store.StartSync(fixture.Source.ID, "full")
	requirements.ErrorIs(err, store.ErrSyncAlreadyActive)

	execution, err := fixture.Store.AcquireSyncExecutionContext(t.Context(), fixture.Source.ID)
	requirements.NoError(err)
	t.Cleanup(func() { _ = execution.Release() })
	runID, err := execution.StartSyncContext(t.Context(), "full", operationID)
	requirements.NoError(err)
	requirements.NoError(fixture.Store.CompleteSync(runID, "complete"))
	requirements.NoError(execution.Release())
	requirements.NoError(fixture.Store.FinishSyncOperation(operationID, "done"))

	nextRunID, err := fixture.Store.StartSync(fixture.Source.ID, "full")
	requirements.NoError(err)
	requirements.NoError(fixture.Store.FailSync(nextRunID, "test complete"))
}

func TestStore_SyncOperationGroupsRunsAndPublishesFinalState(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)

	pending, err := f.Store.CreateSyncOperation(f.Source.ID, "operation-1")
	require.NoError(err)
	assert.Equal("pending", pending.Status)
	assert.Equal(f.Source.ID, pending.SourceID)
	assert.False(pending.CreatedAt.IsZero())
	assert.False(pending.StartedAt.Valid)
	assert.Empty(pending.Runs)
	pending, err = f.Store.GetSyncOperation("operation-1")
	require.NoError(err)
	assert.Equal("pending", pending.Status)
	assert.Equal(f.Source.ID, pending.SourceID)
	assert.Empty(pending.Runs)

	execution, err := f.Store.AcquireSyncExecutionContext(t.Context(), f.Source.ID)
	require.NoError(err)
	t.Cleanup(func() { _ = execution.Release() })
	firstID, err := execution.StartSyncContext(t.Context(), "full", "operation-1")
	require.NoError(err)
	require.NoError(f.Store.CompleteSync(firstID, "first"))
	secondID, err := execution.StartSyncContext(t.Context(), "incremental", "operation-1")
	require.NoError(err)
	require.NoError(f.Store.CompleteSync(secondID, "second"))
	require.NoError(execution.Release())
	require.NoError(f.Store.FinishSyncOperation("operation-1", "done"))

	op, err := f.Store.GetSyncOperation("operation-1")
	require.NoError(err)
	assert.Equal("done", op.Status)
	assert.True(op.StartedAt.Valid)
	assert.True(op.FinishedAt.Valid)
	require.Len(op.Runs, 2)
	assert.Equal(firstID, op.Runs[0].ID)
	assert.Equal(secondID, op.Runs[1].ID)
}

func TestStore_FinishSyncOperationPreservesFirstTerminalStatus(t *testing.T) {
	f := storetest.New(t)

	for _, first := range []string{"done", store.SyncStatusFailed} {
		t.Run(first, func(t *testing.T) {
			requirements := require.New(t)
			checks := assert.New(t)
			operationID := "terminal-" + first
			_, err := f.Store.CreateSyncOperation(f.Source.ID, operationID)
			requirements.NoError(err)
			requirements.NoError(f.Store.FinishSyncOperation(operationID, first))

			second := "done"
			if first == "done" {
				second = store.SyncStatusFailed
			}
			requirements.NoError(f.Store.FinishSyncOperation(operationID, second))

			op, err := f.Store.GetSyncOperation(operationID)
			requirements.NoError(err)
			checks.Equal(first, op.Status)
			checks.True(op.FinishedAt.Valid)
		})
	}
}

func TestStore_FinishFailedSyncOperationFailsRunningRuns(t *testing.T) {
	requirements := require.New(t)
	checks := assert.New(t)
	f := storetest.New(t)
	const operationID = "failed-with-running-run"

	_, err := f.Store.CreateSyncOperation(f.Source.ID, operationID)
	requirements.NoError(err)
	runID, err := f.Store.StartSyncOperation(f.Source.ID, operationID)
	requirements.NoError(err)
	requirements.NoError(f.Store.FinishSyncOperation(operationID, store.SyncStatusFailed))

	op, err := f.Store.GetSyncOperation(operationID)
	requirements.NoError(err)
	checks.Equal(store.SyncStatusFailed, op.Status)
	requirements.Len(op.Runs, 1)
	checks.Equal(runID, op.Runs[0].ID)
	checks.Equal(store.SyncStatusFailed, op.Runs[0].Status)
	checks.True(op.Runs[0].CompletedAt.Valid)
	checks.Equal("sync worker exited before recording completion", op.Runs[0].ErrorMessage.String)
	active, err := f.Store.HasAnyActiveSync()
	requirements.NoError(err)
	checks.False(active)
}

func TestStore_FailUnfinishedSyncOperations(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)

	_, err := f.Store.CreateSyncOperation(f.Source.ID, "orphaned-operation")
	require.NoError(err)
	failed, err := f.Store.FailUnfinishedSyncOperationsContext(t.Context())
	require.NoError(err)
	assert.Equal(int64(1), failed)

	op, err := f.Store.GetSyncOperation("orphaned-operation")
	require.NoError(err)
	assert.Equal("failed", op.Status)
	assert.False(op.StartedAt.Valid)
	assert.True(op.FinishedAt.Valid)
	assert.Empty(op.Runs)
}

func TestStore_FailUnfinishedSyncOperationsRepairsFailedOperationRuns(t *testing.T) {
	requirements := require.New(t)
	checks := assert.New(t)
	first := testutil.NewTestStore(t)
	source, err := first.GetOrCreateSource("gmail", "failed-operation-recovery@example.com")
	requirements.NoError(err)
	const operationID = "failed-operation-with-running-run"
	_, err = first.CreateSyncOperation(source.ID, operationID)
	requirements.NoError(err)
	runID, err := first.StartSyncOperation(source.ID, operationID)
	requirements.NoError(err)
	_, err = first.DB().Exec(first.Rebind(`
		UPDATE sync_operations
		SET status = 'failed', finished_at = CURRENT_TIMESTAMP
		WHERE id = ?
	`), operationID)
	requirements.NoError(err)
	dbPath := store.DBPathForTest(first)
	requirements.NoError(first.Close())

	second, err := store.OpenForTest(dbPath)
	requirements.NoError(err)
	t.Cleanup(func() { _ = second.Close() })
	failed, err := second.FailUnfinishedSyncOperationsContext(t.Context())
	requirements.NoError(err)
	checks.Zero(failed)
	op, err := second.GetSyncOperation(operationID)
	requirements.NoError(err)
	checks.Equal(store.SyncStatusFailed, op.Status)
	requirements.Len(op.Runs, 1)
	checks.Equal(runID, op.Runs[0].ID)
	checks.Equal(store.SyncStatusFailed, op.Runs[0].Status)
	checks.True(op.Runs[0].CompletedAt.Valid)
	checks.Equal("sync worker exited before recording completion", op.Runs[0].ErrorMessage.String)
	active, err := second.HasAnyActiveSync()
	requirements.NoError(err)
	checks.False(active)
}

func TestStore_SyncRunItems(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)

	syncID := f.StartSync()

	require.NoError(f.Store.RecordSyncRunItem(store.SyncRunItem{
		SyncRunID:       syncID,
		SourceMessageID: "msg-skipped",
		Phase:           "fetch",
		Status:          store.SyncRunItemStatusSkipped,
		ErrorKind:       "gmail_not_found",
		ErrorMessage:    "not found: /messages/msg-skipped",
	}), "RecordSyncRunItem skipped")
	require.NoError(f.Store.RecordSyncRunItem(store.SyncRunItem{
		SyncRunID:       syncID,
		SourceMessageID: "msg-error",
		Phase:           "ingest",
		Status:          store.SyncRunItemStatusError,
		ErrorKind:       "ingest_error",
		ErrorMessage:    "parse MIME: malformed header",
	}), "RecordSyncRunItem error")

	errorCount, err := f.Store.CountSyncRunItems(syncID, store.SyncRunItemStatusError)
	require.NoError(err, "CountSyncRunItems error")
	assert.Equal(int64(1), errorCount, "error count")

	skippedCount, err := f.Store.CountSyncRunItems(syncID, store.SyncRunItemStatusSkipped)
	require.NoError(err, "CountSyncRunItems skipped")
	assert.Equal(int64(1), skippedCount, "skipped count")

	items, err := f.Store.ListSyncRunItems(syncID, store.SyncRunItemStatusError, 10)
	require.NoError(err, "ListSyncRunItems")
	require.Len(items, 1, "items")
	assert.Equal("msg-error", items[0].SourceMessageID, "SourceMessageID")
	assert.Equal("ingest", items[0].Phase, "Phase")
	assert.Equal("ingest_error", items[0].ErrorKind, "ErrorKind")
	assert.Equal("parse MIME: malformed header", items[0].ErrorMessage, "ErrorMessage")
	assert.False(items[0].CreatedAt.IsZero(), "CreatedAt")
}

func TestStore_SyncRunItemsCascadeWithSyncRun(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)

	syncID := f.StartSync()
	require.NoError(f.Store.RecordSyncRunItem(store.SyncRunItem{
		SyncRunID:       syncID,
		SourceMessageID: "msg-error",
		Phase:           "fetch",
		Status:          store.SyncRunItemStatusError,
		ErrorKind:       "fetch_error",
		ErrorMessage:    "network unavailable",
	}), "RecordSyncRunItem")

	_, err := f.Store.DB().Exec(f.Store.Rebind(`DELETE FROM sync_runs WHERE id = ?`), syncID)
	require.NoError(err, "delete sync run")

	count, err := f.Store.CountSyncRunItems(syncID, "")
	require.NoError(err, "CountSyncRunItems")
	assert.Equal(int64(0), count, "sync_run_items should cascade with sync_run")
}

// TestListSources_ParsesTimestamps verifies that ListSources correctly parses
// timestamps for all returned sources.
func TestListSources_ParsesTimestamps(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
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
	require.Error(t, err, "expected error for unrecognized timestamp format")

	// Error should include the bad value for debugging
	assert.ErrorContains(t, err, badTimestamp, "error should include the bad value")
}

// TestScanSource_NullRequiredTimestamp verifies that parseRequiredTime returns
// an error when a required timestamp field (created_at/updated_at) is NULL.
func TestScanSource_NullRequiredTimestamp(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
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
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)

	running, err := f.Store.HasAnyActiveSync()
	require.NoError(err, "HasAnyActiveSync (initial)")
	assert.False(running, "expected no active sync on fresh DB")

	syncID := f.StartSync()

	running, err = f.Store.HasAnyActiveSync()
	require.NoError(err, "HasAnyActiveSync (after StartSync)")
	assert.True(running, "expected active sync after StartSync")

	_, err = f.Store.StartSync(f.Source.ID, "full")
	require.ErrorIs(err, store.ErrSyncAlreadyActive)
	running, err = f.Store.HasAnyActiveSync()
	require.NoError(err, "HasAnyActiveSync (after rejected StartSync)")
	assert.True(running, "the original sync remains active")

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
