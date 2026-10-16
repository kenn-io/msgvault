package store

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func waitForSourceOperation(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(20 * time.Second):
		require.FailNow(t, "source operation did not finish")
		return context.DeadlineExceeded
	}
}

func TestMergeSourcesExecutionLockOwnership(t *testing.T) {
	for _, reverse := range []bool{false, true} {
		t.Run(fmt.Sprintf("reverse_%t", reverse), func(t *testing.T) {
			require := require.New(t)
			var st *Store
			if dbURL := os.Getenv("MSGVAULT_TEST_DB"); IsPostgresURL(dbURL) {
				st = newPGStoreInternal(t, dbURL)
			} else {
				st = openTestStore(t)
			}
			first, err := st.GetOrCreateSource("gmail", "first@example.test")
			require.NoError(err)
			second, err := st.GetOrCreateSource("gmail", "second@example.test")
			require.NoError(err)
			peer, err := OpenForTest(st.dbPath)
			require.NoError(err)
			t.Cleanup(func() { _ = peer.Close() })
			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
			defer cancel()
			req := MergeSourcesRequest{FromSourceID: first.ID, IntoSourceID: second.ID, DryRun: true}
			if reverse {
				req.FromSourceID, req.IntoSourceID = req.IntoSourceID, req.FromSourceID
			}
			blocker, err := peer.AcquireSyncExecutionContext(ctx, second.ID)
			require.NoError(err)
			defer func() { _ = blocker.Release() }()
			_, err = st.MergeSourcesContext(ctx, req)
			require.ErrorIs(err, ErrSyncAlreadyActive)
			// Failure on the second lock must release the first physical lock
			// and its reservation in the merging store.
			for _, owner := range []*Store{peer, st} {
				execution, err := owner.AcquireSyncExecutionContext(ctx, first.ID)
				require.NoError(err)
				require.NoError(execution.Release())
			}
			require.NoError(blocker.Release())

			locked := make(chan struct{})
			resume := make(chan struct{})
			var once sync.Once
			release := func() { once.Do(func() { close(resume) }) }
			defer release()
			st.sourceMergeBeforeIdentityLockHook = func() { close(locked); <-resume }
			done := make(chan error, 1)
			go func() { _, err := st.MergeSourcesContext(ctx, req); done <- err }()
			select {
			case <-locked:
			case <-ctx.Done():
				require.FailNow("merge did not acquire both execution locks")
			}
			for _, owner := range []*Store{peer, st} {
				for _, id := range []int64{first.ID, second.ID} {
					_, err := owner.AcquireSyncExecutionContext(ctx, id)
					require.ErrorIs(err, ErrSyncAlreadyActive)
				}
			}
			release()
			require.NoError(waitForSourceOperation(t, done))
			st.sourceMergeBeforeIdentityLockHook = nil
			for _, owner := range []*Store{peer, st} {
				for _, id := range []int64{first.ID, second.ID} {
					execution, err := owner.AcquireSyncExecutionContext(ctx, id)
					require.NoError(err)
					require.NoError(execution.Release())
					require.NoError(execution.Release())
				}
			}
		})
	}
}

func TestSourceRemovalLockSerializesWithMerge(t *testing.T) {
	require := require.New(t)
	var st *Store
	if dbURL := os.Getenv("MSGVAULT_TEST_DB"); IsPostgresURL(dbURL) {
		st = newPGStoreInternal(t, dbURL)
	} else {
		st = openTestStore(t)
	}
	from, err := st.GetOrCreateSource("matrix", "@history:example.org")
	require.NoError(err)
	into, err := st.GetOrCreateSource("matrix", "@archive:example.org")
	require.NoError(err)
	peer, err := OpenForTest(st.dbPath)
	require.NoError(err)
	t.Cleanup(func() { require.NoError(peer.Close()) })
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	req := MergeSourcesRequest{FromSourceID: from.ID, IntoSourceID: into.ID}

	releaseRemoval, err := peer.AcquireSourceRemovalLockContext(ctx, into.ID)
	require.NoError(err)
	defer func() { require.NoError(releaseRemoval()) }()
	_, err = st.MergeSourcesContext(ctx, req)
	require.ErrorIs(err, ErrSyncAlreadyActive)
	require.NoError(releaseRemoval())

	mergeLocked := make(chan struct{})
	resumeMerge := make(chan struct{})
	releaseMerge := sync.OnceFunc(func() { close(resumeMerge) })
	defer releaseMerge()
	st.sourceMergeBeforeIdentityLockHook = func() { close(mergeLocked); <-resumeMerge }
	merged := make(chan error, 1)
	go func() { _, err := st.MergeSourcesContext(ctx, req); merged <- err }()
	select {
	case <-mergeLocked:
	case <-ctx.Done():
		require.FailNow("merge did not acquire source ownership")
	}
	_, err = peer.AcquireSourceRemovalLockContext(ctx, into.ID)
	releaseMerge()
	mergeErr := waitForSourceOperation(t, merged)
	require.ErrorIs(err, ErrSyncAlreadyActive)
	require.NoError(mergeErr)
	_, err = peer.AcquireSourceRemovalLockContext(ctx, into.ID)
	require.ErrorIs(err, ErrSourceMergeParticipant)

	// Refusing removal must release the physical lock as well as its local
	// reservation; the retained destination remains available to sync.
	execution, err := st.AcquireSyncExecutionContext(ctx, into.ID)
	require.NoError(err)
	require.NoError(execution.Release())
}

func TestSourceRemovalLockDoesNotRecoverRunningSync(t *testing.T) {
	require := require.New(t)
	var st *Store
	if dbURL := os.Getenv("MSGVAULT_TEST_DB"); IsPostgresURL(dbURL) {
		st = newPGStoreInternal(t, dbURL)
	} else {
		st = openTestStore(t)
	}
	source, err := st.GetOrCreateSource("matrix", "@archive:example.org")
	require.NoError(err)
	execution, err := st.AcquireSyncExecutionContext(t.Context(), source.ID)
	require.NoError(err)
	syncID, err := execution.StartSyncContext(t.Context(), "matrix", "")
	require.NoError(err)
	peer, err := OpenForTest(st.dbPath)
	require.NoError(err)
	t.Cleanup(func() { require.NoError(peer.Close()) })
	require.NoError(execution.Release(), "release worker ownership while leaving its running row")

	release, err := peer.AcquireSourceRemovalLockContext(t.Context(), source.ID)
	require.NoError(err)
	defer func() { require.NoError(release()) }()
	run, err := peer.GetActiveSync(source.ID)
	require.NoError(err)
	assert.Equal(t, syncID, run.ID, "removal must not recover the running row before the cascade observes it")
	hadActiveSync, _, err := peer.RemoveSourceSerialized(t.Context(), source.ID)
	require.NoError(err)
	assert.True(t, hadActiveSync, "forced removal must retain the signal used to preserve attachment files")
}
