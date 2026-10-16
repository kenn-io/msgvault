package store

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMergeSourcesWithTwoPostgresConnections(t *testing.T) {
	for _, dryRun := range []bool{true, false} {
		t.Run(fmt.Sprintf("dry_run_%t", dryRun), func(t *testing.T) {
			require := require.New(t)
			st := newPGStoreInternal(t, skipUnlessPostgresInternal(t))
			from, err := st.GetOrCreateSource("gmail", "history@example.test")
			require.NoError(err)
			into, err := st.GetOrCreateSource("gmail", "live@example.test")
			require.NoError(err)
			st.DB().SetMaxOpenConns(2)
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			done := make(chan error, 1)
			go func() {
				_, err := st.MergeSourcesContext(ctx, MergeSourcesRequest{FromSourceID: from.ID, IntoSourceID: into.ID, DryRun: dryRun})
				done <- err
			}()
			select {
			case err = <-done:
			case <-ctx.Done():
				// Release a broken implementation's context-free pool wait so
				// the regression reports a failure without leaking a goroutine.
				st.DB().SetMaxOpenConns(3)
				require.NoError(st.DB().PingContext(t.Context()))
				err = waitForSourceOperation(t, done)
			}
			require.NoError(err)
			settings, err := st.GetSourceSettingsContext(t.Context(), from.ID)
			require.NoError(err)
			if dryRun {
				assert.Zero(t, settings.MergedIntoSourceID)
			} else {
				assert.Equal(t, into.ID, settings.MergedIntoSourceID)
			}
		})
	}
}

// waitForPostgresBlocker observes a real lock wait, not a scheduling delay.
// Both cancellation and removal regressions use it to establish their race.
func waitForPostgresBlocker(t *testing.T, db *sql.DB, blocker int) int {
	t.Helper()
	require := require.New(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		var pid int
		err := db.QueryRowContext(ctx, `SELECT COALESCE((SELECT pid FROM pg_stat_activity
 WHERE $1 = ANY(pg_blocking_pids(pid)) LIMIT 1), 0)`, blocker).Scan(&pid)
		require.NoError(err)
		if pid != 0 {
			return pid
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			require.FailNow("operation did not reach its database lock wait")
		}
	}
}

func TestSourceExecutionLifecycleCheckHonorsCancellation(t *testing.T) {
	for _, merge := range []bool{false, true} {
		t.Run(fmt.Sprintf("merge_%t", merge), func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			st := newPGStoreInternal(t, skipUnlessPostgresInternal(t))
			from, err := st.GetOrCreateSource("gmail", "history@example.test")
			require.NoError(err)
			into, err := st.GetOrCreateSource("gmail", "live@example.test")
			require.NoError(err)
			blocker, err := st.DB().BeginTx(t.Context(), nil)
			require.NoError(err)
			defer func() { _ = blocker.Rollback() }()
			var blockerPID int
			require.NoError(blocker.QueryRow(`SELECT pg_backend_pid()`).Scan(&blockerPID))
			_, err = blocker.Exec(`LOCK TABLE source_settings IN ACCESS EXCLUSIVE MODE`)
			require.NoError(err)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				if merge {
					_, err := st.MergeSourcesContext(ctx, MergeSourcesRequest{FromSourceID: from.ID, IntoSourceID: into.ID, DryRun: true})
					done <- err
					return
				}
				execution, err := st.AcquireSyncExecutionContext(ctx, from.ID)
				if execution != nil {
					_ = execution.Release()
				}
				done <- err
			}()
			waitForPostgresBlocker(t, st.DB(), blockerPID)
			cancel()
			select {
			case err = <-done:
			case <-time.After(5 * time.Second):
				assert.Fail("canceled lifecycle check remained blocked")
				require.NoError(blocker.Rollback())
				err = waitForSourceOperation(t, done)
			}
			require.ErrorIs(err, context.Canceled)
			_ = blocker.Rollback()
			// A canceled acquisition must release every source reservation and
			// backend lock so the operation can be retried immediately.
			_, err = st.MergeSourcesContext(t.Context(), MergeSourcesRequest{FromSourceID: from.ID, IntoSourceID: into.ID, DryRun: true})
			require.NoError(err)
		})
	}
}

func TestRemoveSourceRechecksMergeHistoryAfterIdentityLock(t *testing.T) {
	for _, removeDestination := range []bool{false, true} {
		t.Run(fmt.Sprintf("destination_%t", removeDestination), func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			st := newPGStoreInternal(t, skipUnlessPostgresInternal(t))
			from, err := st.GetOrCreateSource("gmail", "history@example.test")
			require.NoError(err)
			into, err := st.GetOrCreateSource("gmail", "live@example.test")
			require.NoError(err)
			blocker, err := st.DB().BeginTx(t.Context(), nil)
			require.NoError(err)
			defer func() { _ = blocker.Rollback() }()
			var blockerPID int
			require.NoError(blocker.QueryRow(`SELECT pg_backend_pid()`).Scan(&blockerPID))
			_, err = blocker.Exec(`UPDATE source_maintenance_lock SET singleton = singleton WHERE singleton = 1`)
			require.NoError(err)
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			defer cancel()
			mergeDone := make(chan error, 1)
			req := MergeSourcesRequest{FromSourceID: from.ID, IntoSourceID: into.ID}
			go func() { _, err := st.MergeSourcesContext(ctx, req); mergeDone <- err }()
			// The merge now owns the identity lock but cannot yet transfer data.
			mergePID := waitForPostgresBlocker(t, st.DB(), blockerPID)
			sourceID := from.ID
			if removeDestination {
				sourceID = into.ID
			}
			removeDone := make(chan error, 1)
			go func() { removeDone <- st.RemoveSource(sourceID) }()
			waitForPostgresBlocker(t, st.DB(), mergePID)
			require.NoError(blocker.Rollback())
			require.NoError(waitForSourceOperation(t, mergeDone))
			require.ErrorIs(waitForSourceOperation(t, removeDone), ErrSourceMergeParticipant)
			for _, id := range []int64{from.ID, into.ID} {
				_, err := st.GetSourceByID(id)
				require.NoError(err, "merge participants remain in the archive")
			}
			retry, err := st.MergeSourcesContext(t.Context(), req)
			require.NoError(err)
			assert.True(retry.AlreadyMerged, "removal must preserve the merge journal")
		})
	}
}

func TestExistingSourceLookupDoesNotWaitForMaintenance(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := newPGStoreInternal(t, skipUnlessPostgresInternal(t))
	source, err := st.GetOrCreateSource("gmail", "existing@example.test")
	require.NoError(err)
	blocker, err := st.DB().BeginTx(t.Context(), nil)
	require.NoError(err)
	defer func() { _ = blocker.Rollback() }()
	_, err = blocker.Exec(`UPDATE source_maintenance_lock SET singleton = singleton WHERE singleton = 1`)
	require.NoError(err)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	found, err := st.GetOrCreateSourceContext(ctx, source.SourceType, source.Identifier)
	require.NoError(err)
	assert.Equal(source.ID, found.ID)
}
