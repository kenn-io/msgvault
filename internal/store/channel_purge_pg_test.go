package store

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExclusiveMaintenanceLetsScopedConversationFinish(t *testing.T) {
	for _, maintenance := range []string{"channel purge", "source removal", "exclusive lock"} {
		t.Run(maintenance, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			st := newPGStoreInternal(t, skipUnlessPostgresInternal(t))
			removed, err := st.GetOrCreateSource("discord", "removed-guild")
			require.NoError(err)
			retained, err := st.GetOrCreateSource("discord", "retained-guild")
			require.NoError(err)
			removedConversation, err := st.EnsureConversation(removed.ID, "removed-channel", "Removed")
			require.NoError(err)
			runID, err := st.StartSync(retained.ID, "full")
			require.NoError(err)
			scoped := st.ScopedToSync(retained.ID, runID)

			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			blocker, err := st.DB().BeginTx(ctx, nil)
			require.NoError(err)
			defer func() { _ = blocker.Rollback() }()
			var id int64
			require.NoError(blocker.QueryRowContext(ctx,
				"SELECT id FROM sync_runs WHERE id=$1 FOR UPDATE", runID).Scan(&id))
			var blockerPID int
			require.NoError(blocker.QueryRowContext(ctx, "SELECT pg_backend_pid()").Scan(&blockerPID))

			// Park the real conversation write inside its generation fence. It
			// holds sync_runs but has not reached the sources foreign-key check.
			writeDone := make(chan error, 1)
			go func() {
				_, err := scoped.EnsureConversationWithTypeContext(ctx, retained.ID, "retained-channel", "channel", "Retained")
				writeDone <- err
			}()
			writerPID := waitForPGBlockedQuery(ctx, t, st, blockerPID, "%UPDATE sync_runs%")
			maintenanceDone := make(chan error, 1)
			go func() {
				var err error
				switch maintenance {
				case "channel purge":
					err = st.PurgeChannelContext(ctx, removed.ID, "removed-channel")
				case "source removal":
					_, _, err = st.RemoveSourceSerialized(ctx, removed.ID)
				case "exclusive lock":
					err = st.WithExclusiveLock(ctx, func() error { return nil })
				}
				maintenanceDone <- err
			}()
			waitForPGBlockedQuery(ctx, t, st, writerPID, "LOCK TABLE %")
			require.NoError(blocker.Commit())
			assert.NoError(<-writeDone, "scoped conversation creation")
			assert.NoError(<-maintenanceDone, "maintenance")

			var title string
			require.NoError(st.DB().QueryRowContext(ctx,
				"SELECT title FROM conversations WHERE source_id=$1 AND source_conversation_id='retained-channel'",
				retained.ID).Scan(&title))
			assert.Equal("Retained", title)
			var count int
			require.NoError(st.DB().QueryRowContext(ctx,
				"SELECT count(*) FROM conversations WHERE id=$1", removedConversation).Scan(&count))
			if maintenance == "exclusive lock" {
				assert.Equal(1, count)
			} else {
				assert.Zero(count)
			}
		})
	}
}

func waitForPGBlockedQuery(ctx context.Context, t *testing.T, st *Store, blockerPID int, query string) int {
	t.Helper()
	var pid int
	require.Eventually(t, func() bool {
		return st.DB().QueryRowContext(ctx, `SELECT pid FROM pg_stat_activity
			WHERE datname=current_database() AND $1=ANY(pg_blocking_pids(pid))
			AND query LIKE $2 LIMIT 1`, blockerPID, query).Scan(&pid) == nil
	}, 15*time.Second, 10*time.Millisecond, "query %q did not wait for backend %d", query, blockerPID)
	return pid
}

// A source-row blocker pauses each real sync path before it can finish its
// transaction. Maintenance must wait without holding locks that path still needs.
func TestChannelPurgeLetsOtherSourceSyncFinish(t *testing.T) {
	for _, operation := range []string{"recovery", "start", "start with ownership", "complete with cursor", "fail and clear cursor", "scoped source write", "fail operation"} {
		t.Run(operation, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			st := newPGStoreInternal(t, skipUnlessPostgresInternal(t))
			removed, err := st.GetOrCreateSource("discord", "removed-guild")
			require.NoError(err)
			retained, err := st.GetOrCreateSource("discord", "retained-guild")
			require.NoError(err)
			conversation, err := st.EnsureConversation(removed.ID, "removed-channel", "Removed")
			require.NoError(err)
			var runID int64
			if operation == "fail operation" {
				_, err = st.CreateSyncOperation(retained.ID, "retained-operation")
				require.NoError(err)
				runID, err = st.StartSyncOperation(retained.ID, "retained-operation")
			} else {
				runID, err = st.StartSync(retained.ID, "full")
			}
			require.NoError(err)
			require.NoError(st.UpdateSourceSyncCursor(retained.ID, "old-cursor"))

			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			var heldLock syncExecutionLock
			switch operation {
			case "recovery":
				// Leave a running row behind as a departed worker would.
				require.NoError(st.releaseSyncExecutionLock(runID))
			case "start", "start with ownership":
				require.NoError(st.CompleteSyncContext(ctx, runID, "old-cursor"))
				if operation == "start with ownership" {
					execution, err := st.AcquireSyncExecutionContext(ctx, retained.ID)
					require.NoError(err)
					defer func() { assert.NoError(execution.Release()) }()
					heldLock = execution.lock
				}
			}

			blocker, err := st.DB().BeginTx(ctx, nil)
			require.NoError(err)
			defer func() { _ = blocker.Rollback() }()
			var id int64
			blockedQuery := "%sources%"
			if operation == "fail operation" {
				blockedQuery = "%UPDATE sync_operations%"
				var operationID string
				require.NoError(blocker.QueryRowContext(ctx,
					"SELECT id FROM sync_operations WHERE id='retained-operation' FOR UPDATE").Scan(&operationID))
			} else {
				require.NoError(blocker.QueryRowContext(ctx,
					"SELECT id FROM sources WHERE id=$1 FOR UPDATE", retained.ID).Scan(&id))
			}
			var blockerPID int
			require.NoError(blocker.QueryRowContext(ctx, "SELECT pg_backend_pid()").Scan(&blockerPID))

			syncDone := make(chan error, 1)
			go func() {
				var err error
				switch operation {
				case "recovery":
					var execution *SyncExecution
					execution, err = st.AcquireSyncExecutionContext(ctx, retained.ID)
					if err == nil {
						err = execution.Release()
					}
				case "start", "start with ownership":
					// Exercise one attempt so retries cannot conceal a deadlock.
					_, err = st.startSyncOnce(ctx, retained.ID, "full", "", "", heldLock)
				case "complete with cursor":
					err = st.CompleteSyncAndUpdateSourceCursorContext(ctx, runID, retained.ID, "new-cursor")
				case "fail and clear cursor":
					err = st.FailSyncAndClearSourceCursorContext(ctx, runID, retained.ID, "expired cursor")
				case "fail operation":
					err = st.FinishSyncOperation("retained-operation", SyncStatusFailed)
				case "scoped source write":
					err = st.ScopedToSync(retained.ID, runID).UpdateSourceDisplayNameContext(ctx, retained.ID, "Renamed")
				}
				syncDone <- err
			}()
			writerPID := waitForPGBlockedQuery(ctx, t, st, blockerPID, blockedQuery)
			purgeDone := make(chan error, 1)
			go func() { purgeDone <- st.PurgeChannelContext(ctx, removed.ID, "removed-channel") }()
			waitForPGBlockedQuery(ctx, t, st, writerPID, "LOCK TABLE %")
			require.NoError(blocker.Commit())
			assert.NoError(<-syncDone, "sync operation")
			assert.NoError(<-purgeDone, "channel purge")

			var count int
			require.NoError(st.DB().QueryRowContext(ctx,
				"SELECT count(*) FROM conversations WHERE id=$1", conversation).Scan(&count))
			assert.Zero(count)
			source, err := st.GetSourceByIDContext(ctx, retained.ID)
			require.NoError(err)
			run, err := st.GetLatestSyncContext(ctx, retained.ID, 0)
			require.NoError(err)
			switch operation {
			case "recovery", "fail and clear cursor", "fail operation":
				assert.Equal(SyncStatusFailed, run.Status)
				if operation == "fail and clear cursor" {
					assert.Empty(source.SyncCursor.String)
				}
			case "complete with cursor":
				assert.Equal(SyncStatusCompleted, run.Status)
				assert.Equal("new-cursor", source.SyncCursor.String)
			case "scoped source write":
				assert.Equal(SyncStatusRunning, run.Status)
				assert.Equal("Renamed", source.DisplayName.String)
			case "start", "start with ownership":
				assert.Equal(SyncStatusRunning, run.Status)
				assert.Greater(run.ID, runID)
			}
		})
	}
}
