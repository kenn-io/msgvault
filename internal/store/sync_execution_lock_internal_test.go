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
	requirements := require.New(t)
	st, err := OpenForTest(":memory:")
	requirements.NoError(err)
	t.Cleanup(func() { _ = st.Close() })
	requirements.NoError(st.InitSchema())
	source, err := st.GetOrCreateSource("gmail", "owner@example.test")
	requirements.NoError(err)
	// Model a backend that still owns its lock after an unsuccessful unlock.
	cleanupErr := errors.New("backend unlock failed")
	lock := &failingSyncExecutionLock{err: cleanupErr}
	st.syncExecutionLocks.bySource[source.ID] = lock
	execution := &SyncExecution{store: st, sourceID: source.ID, lock: lock}
	requirements.ErrorIs(execution.Release(), cleanupErr)
	_, err = st.AcquireSyncExecutionContext(t.Context(), source.ID)
	requirements.ErrorIs(err, ErrSyncAlreadyActive)

	lock.released = true
	requirements.ErrorIs(execution.Release(), cleanupErr)
	requirements.NoError(execution.Release(), "completed cleanup must be idempotent")
	next, err := st.AcquireSyncExecutionContext(t.Context(), source.ID)
	requirements.NoError(err, "retrying cleanup must release the source")
	requirements.NoError(next.Release())
}

func TestSyncExecutionCleanupRetainsOnlyUnreleasedSources(t *testing.T) {
	for _, cleanup := range []string{"run", "abandon", "all"} {
		t.Run(cleanup, func(t *testing.T) {
			requirements := require.New(t)
			st, err := OpenForTest(":memory:")
			requirements.NoError(err)
			t.Cleanup(func() { _ = st.Close() })
			requirements.NoError(st.InitSchema())
			source, err := st.GetOrCreateSource("gmail", "owner@example.test")
			requirements.NoError(err)
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
			requirements.ErrorIs(release(), cleanupErr)
			_, err = st.AcquireSyncExecutionContext(t.Context(), source.ID)
			requirements.ErrorIs(err, ErrSyncAlreadyActive)

			lock.released = true
			requirements.ErrorIs(release(), cleanupErr)
			next, err := st.AcquireSyncExecutionContext(t.Context(), source.ID)
			requirements.NoError(err, "completed cleanup must release ownership despite its diagnostic error")
			requirements.NoError(next.Release())
			requirements.NoError(st.releaseSyncExecutionLock(101), "completed cleanup must also remove the run")
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

func TestInboxExecutionReleasesFenceAfterCompletedSyncCleanupError(t *testing.T) {
	requirements := require.New(t)

	st, err := OpenForTest(":memory:")
	requirements.NoError(err)
	defer func() { requirements.NoError(st.Close()) }()
	requirements.NoError(st.InitSchema())
	source, err := st.GetOrCreateSource("gmail", "cleanup@example.test")
	requirements.NoError(err)
	cleanupErr := errors.New("synthetic completed cleanup diagnostic")
	lock := &failingSyncExecutionLock{released: true, err: cleanupErr}
	st.syncExecutionLocks.bySource[source.ID] = lock
	fence := &failingSyncExecutionLock{released: true}
	execution := &InboxExecution{execution: &SyncExecution{store: st, sourceID: source.ID, lock: lock}, fence: fence}
	requirements.ErrorIs(execution.Release(), cleanupErr)
	requirements.Nil(execution.fence, "ended sync ownership must not strand the independent removal fence")
	requirements.NoError(execution.Release())
}
