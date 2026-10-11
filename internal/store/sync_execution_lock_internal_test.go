package store

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

type failingSyncExecutionLock struct {
	released bool
	err      error
}

func (l *failingSyncExecutionLock) release() (bool, error) { return l.released, l.err }

func TestSyncExecutionReleaseRetriesFailedCleanup(t *testing.T) {
	require := require.New(t)
	st, err := OpenForTest(":memory:")
	require.NoError(err)
	t.Cleanup(func() { _ = st.Close() })
	require.NoError(st.InitSchema())
	source, err := st.GetOrCreateSource("gmail", "owner@example.test")
	require.NoError(err)
	// Model a backend that still owns its lock after an unsuccessful unlock.
	cleanupErr := errors.New("backend unlock failed")
	lock := &failingSyncExecutionLock{err: cleanupErr}
	st.syncExecutionLocks.bySource[source.ID] = lock
	execution := &SyncExecution{store: st, sourceID: source.ID, lock: lock}
	require.ErrorIs(execution.Release(), cleanupErr)
	_, err = st.AcquireSyncExecutionContext(t.Context(), source.ID)
	require.ErrorIs(err, ErrSyncAlreadyActive)

	lock.released = true
	require.ErrorIs(execution.Release(), cleanupErr)
	require.NoError(execution.Release(), "completed cleanup must be idempotent")
	next, err := st.AcquireSyncExecutionContext(t.Context(), source.ID)
	require.NoError(err, "retrying cleanup must release the source")
	require.NoError(next.Release())
}

func TestSyncExecutionCleanupRetainsOnlyUnreleasedSources(t *testing.T) {
	for _, cleanup := range []string{"run", "abandon", "all"} {
		t.Run(cleanup, func(t *testing.T) {
			require := require.New(t)
			st, err := OpenForTest(":memory:")
			require.NoError(err)
			t.Cleanup(func() { _ = st.Close() })
			require.NoError(st.InitSchema())
			source, err := st.GetOrCreateSource("gmail", "owner@example.test")
			require.NoError(err)
			cleanupErr := errors.New("backend cleanup failed")
			lock := &failingSyncExecutionLock{err: cleanupErr}
			st.registerSyncExecutionLock(source.ID, 101, lock, true)
			release := func() error {
				switch cleanup {
				case "run":
					return st.releaseSyncExecutionLock(101)
				case "abandon":
					return st.abandonSyncExecutionLock(source.ID, lock)
				default:
					return st.releaseAllSyncExecutionLocks()
				}
			}
			require.ErrorIs(release(), cleanupErr)
			_, err = st.AcquireSyncExecutionContext(t.Context(), source.ID)
			require.ErrorIs(err, ErrSyncAlreadyActive)

			lock.released = true
			require.ErrorIs(release(), cleanupErr)
			next, err := st.AcquireSyncExecutionContext(t.Context(), source.ID)
			require.NoError(err, "completed cleanup must release ownership despite its diagnostic error")
			require.NoError(next.Release())
			require.NoError(st.releaseSyncExecutionLock(101), "completed cleanup must also remove the run")
		})
	}
}

func TestReleaseOwnedNoOpSyncExecutionLockPreservesOtherSource(t *testing.T) {
	requirements := require.New(t)
	st, err := OpenForTest(":memory:")
	requirements.NoError(err)
	t.Cleanup(func() { _ = st.Close() })

	firstLock, err := st.acquireBackendSyncExecutionLock(t.Context(), 1)
	requirements.NoError(err)
	secondLock, err := st.acquireBackendSyncExecutionLock(t.Context(), 2)
	requirements.NoError(err)

	st.syncExecutionLocks.bySource[1] = firstLock
	st.syncExecutionLocks.bySource[2] = secondLock
	st.registerSyncExecutionLock(1, 101, firstLock, false)
	st.registerSyncExecutionLock(2, 202, secondLock, false)

	released, err := st.releaseOwnedSyncExecutionLock(1, firstLock)
	requirements.NoError(err)
	requirements.True(released)

	st.syncExecutionLocks.mu.Lock()
	defer st.syncExecutionLocks.mu.Unlock()
	_, firstSourceExists := st.syncExecutionLocks.bySource[1]
	_, secondSourceExists := st.syncExecutionLocks.bySource[2]
	_, firstRunExists := st.syncExecutionLocks.byRun[101]
	_, secondRunExists := st.syncExecutionLocks.byRun[202]
	requirements.False(firstSourceExists)
	requirements.True(secondSourceExists)
	requirements.False(firstRunExists)
	requirements.True(secondRunExists)
}
