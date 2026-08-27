package store_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

func TestStore_CompleteSyncRejectsSupersededRun(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)

	oldID := f.StartSync()
	newID := f.StartSync()

	err := f.Store.CompleteSync(oldID, "stale-cursor")
	require.ErrorIs(err, store.ErrSyncRunSuperseded)

	var status string
	var cursorAfter sql.NullString
	require.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`
		SELECT status, cursor_after FROM sync_runs WHERE id = ?
	`), oldID).Scan(&status, &cursorAfter))
	assert.Equal(store.SyncStatusFailed, status)
	assert.False(cursorAfter.Valid)

	run, err := f.Store.GetActiveSync(f.Source.ID)
	require.NoError(err)
	assert.Equal(newID, run.ID)
}

func TestStore_CompleteSyncAndUpdateSourceCursorRejectsSupersededRunAtomically(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	require.NoError(f.Store.UpdateSourceSyncCursor(f.Source.ID, "baseline-cursor"))

	oldID := f.StartSync()
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
	f := storetest.New(t)
	messageID := f.CreateMessage("generation-fence-message")
	var conversationID int64
	require.NoError(t, f.Store.DB().QueryRow(f.Store.Rebind(
		`SELECT conversation_id FROM messages WHERE id = ?`), messageID,
	).Scan(&conversationID))
	participantID := f.EnsureParticipant("reactor@example.test", "Reactor", "example.test")
	oldID := f.StartSync()
	stale := f.Store.ScopedToSync(f.Source.ID, oldID)
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
			require.ErrorIs(t, test.write(), store.ErrSyncRunSuperseded)
		})
	}
	var staleEmailThreads int
	require.NoError(t, f.Store.DB().QueryRow(f.Store.Rebind(`SELECT COUNT(*) FROM conversations
		WHERE source_id = ? AND source_conversation_id = 'stale-email-thread'`),
		f.Source.ID).Scan(&staleEmailThreads))
	assert.Zero(t, staleEmailThreads)
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
