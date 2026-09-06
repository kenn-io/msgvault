package store

import (
	"testing"

	"github.com/stretchr/testify/require"
)

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

	requirements.NoError(st.releaseOwnedSyncExecutionLock(1, firstLock))

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
