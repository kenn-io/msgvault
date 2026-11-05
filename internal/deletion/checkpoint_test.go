package deletion

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExecutor_CheckpointWritesGrowLinearly(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprintf("fail=%v", fail), func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			ns := []int{10_000, 100_000}
			if testing.Short() {
				ns = ns[:1]
			}
			var sizes []int64
			for _, n := range ns {
				tc := NewTestContext(t)
				m := tc.CreateManifest("linear checkpoints", msgIDs(n))
				path := filepath.Join(tc.Mgr.InProgressDir(), m.ID+".json")
				var claimed os.FileInfo
				var journalSize int64
				tc.MockAPI.BeforeTrash = func(string) error {
					info, err := os.Stat(path)
					require.NoError(err)
					if claimed == nil {
						claimed = info
					}
					assert.True(os.SameFile(claimed, info) && claimed.Size() == info.Size(), "checkpoint rewrote the manifest")
					if journal, err := os.Stat(journalPath(path)); err == nil {
						journalSize = journal.Size()
					}
					if fail {
						return errors.New("provider failure")
					}
					return nil
				}
				require.NoError(tc.ExecuteWithOpts(m.ID, trashOpts(100)))
				require.Positive(journalSize)
				sizes = append(sizes, journalSize)
			}
			if len(sizes) == 2 {
				assert.Greater(sizes[1], 9*sizes[0])
				assert.Less(sizes[1], 12*sizes[0])
			}
		})
	}
}

func TestExecutor_JournalResume(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	tc := NewTestContext(t)
	m := tc.CreateManifest("interrupted", msgIDs(6))
	path := filepath.Join(tc.Mgr.InProgressDir(), m.ID+".json")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	tc.MockAPI.BeforeTrash = func(id string) error {
		if id == "msg2" {
			cancel()
		}
		if id == "msg1" {
			return errors.New("provider failure")
		}
		return nil
	}
	require.ErrorIs(tc.Exec.Execute(ctx, m.ID, trashOpts(2)), context.Canceled)
	loaded, err := LoadManifest(path)
	require.NoError(err)
	assert.Equal(3, loaded.Execution.LastProcessedIndex)
	assert.Equal(2, loaded.Execution.Succeeded)
	assert.Equal([]string{"msg1"}, loaded.Execution.FailedIDs)

	// The retry checkpoint replaces the previous failure list.
	tc.MockAPI.BeforeTrash = func(id string) error {
		switch id {
		case "msg5":
			loaded, err := LoadManifest(path)
			require.NoError(err)
			assert.Equal(4, loaded.Execution.LastProcessedIndex)
			assert.Equal([]string{"msg3"}, loaded.Execution.FailedIDs)
		case "msg3", "msg4":
			return errors.New("provider failure")
		}
		return nil
	}
	require.NoError(tc.ExecuteWithOpts(m.ID, trashOpts(2)))
	assert.Equal([]string{"msg0", "msg2", "msg1", "msg5"}, tc.MockAPI.TrashCalls)
	tc.AssertManifestExecution(m.ID, 4, 2, "msg3", "msg4")
}

func TestExecutor_FinalizeIgnoresStaleJournal(t *testing.T) {
	if runtime.GOOS == "windows" || os.Getuid() == 0 {
		t.Skip("requires Unix file permissions enforced for the current user")
	}
	require := require.New(t)
	assert := assert.New(t)
	tc := NewTestContext(t)
	m := tc.CreateManifest("stale final journal", msgIDs(3))
	baseDir := tc.Mgr.baseDir
	t.Cleanup(func() { require.NoError(os.Chmod(baseDir, 0o700)) })
	tc.MockAPI.BeforeTrash = func(id string) error {
		if id == "msg2" {
			// Publication in in_progress still succeeds, but unlinking the
			// index-2 journal record in its parent must fail.
			require.NoError(os.Chmod(baseDir, 0o500))
		}
		return nil
	}
	require.NoError(tc.ExecuteWithOpts(m.ID, trashOpts(2)))
	path := filepath.Join(tc.Mgr.CompletedDir(), m.ID+".json")
	_, err := os.Stat(journalPath(path))
	require.NoError(err)
	loaded, err := LoadManifest(path)
	require.NoError(err)
	assert.Equal(StatusCompleted, loaded.Status)
	require.NotNil(loaded.Execution)
	assert.Equal(3, loaded.Execution.LastProcessedIndex)
	assert.Equal(3, loaded.Execution.Succeeded)
	assert.Zero(loaded.Execution.Failed)
	assert.Empty(loaded.Execution.FailedIDs)
	require.NotNil(loaded.Execution.CompletedAt)

	// Simulate a crash after publishing the terminal file but before moving it.
	require.NoError(os.Chmod(baseDir, 0o700))
	require.NoError(os.Rename(path, filepath.Join(tc.Mgr.InProgressDir(), m.ID+".json")))
	tc.MockAPI.BeforeTrash = nil
	require.NoError(tc.ExecuteWithOpts(m.ID, trashOpts(2)))
	assert.Equal([]string{"msg0", "msg1", "msg2"}, tc.MockAPI.TrashCalls)
	tc.AssertManifestExecution(m.ID, 3, 0)
}

func TestManager_ResumeTerminalManifestRequiresJournalCleanup(t *testing.T) {
	if runtime.GOOS == "windows" || os.Getuid() == 0 {
		t.Skip("requires Unix file permissions enforced for the current user")
	}
	for _, status := range []Status{StatusCompleted, StatusFailed} {
		t.Run(string(status), func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			mgr := testManager(t)
			m := setupInProgress(t, mgr, "terminal resume")
			path := filepath.Join(mgr.InProgressDir(), m.ID+".json")
			m.Execution.LastProcessedIndex = 1
			m.Execution.Failed = 1
			m.Execution.FailedIDs = []string{"a"}
			require.NoError(mgr.WriteInProgressCheckpoint(m, m.ID, 0))
			m.Status = status
			m.Execution.LastProcessedIndex = 2
			m.Execution.Succeeded = 2
			m.Execution.Failed = 0
			m.Execution.FailedIDs = nil
			m.Execution.CompletedAt = new(time.Now())
			require.NoError(os.Chmod(mgr.baseDir, 0o500))
			t.Cleanup(func() { require.NoError(os.Chmod(mgr.baseDir, 0o700)) })
			require.NoError(mgr.WriteInProgressManifest(m, m.ID))
			targetDir := mgr.dirForStatus(status)
			require.NoError(os.Chmod(targetDir, 0o500))
			t.Cleanup(func() { require.NoError(os.Chmod(targetDir, 0o700)) })
			require.ErrorIs(mgr.FinalizeInProgress(m.ID, status), os.ErrPermission)

			for range 3 {
				_, err := mgr.ClaimManifest(m.ID, MethodTrash)
				require.ErrorIs(err, os.ErrPermission)
				loaded, err := LoadManifest(path)
				require.NoError(err)
				assert.Equal(status, loaded.Status)
				assert.Equal(2, loaded.Execution.LastProcessedIndex)
				assert.Equal(2, loaded.Execution.Succeeded)
				assert.Zero(loaded.Execution.Failed)
				assert.Empty(loaded.Execution.FailedIDs)
				_, err = os.Stat(journalPath(path))
				require.NoError(err)
			}

			require.NoError(os.Chmod(mgr.baseDir, 0o700))
			for range 2 {
				_, err := mgr.ClaimManifest(m.ID, MethodTrash)
				require.NoError(err)
				loaded, err := LoadManifest(path)
				require.NoError(err)
				assert.Equal(StatusInProgress, loaded.Status)
				assert.Equal(2, loaded.Execution.LastProcessedIndex)
				assert.Equal(2, loaded.Execution.Succeeded)
				assert.Zero(loaded.Execution.Failed)
				assert.Empty(loaded.Execution.FailedIDs)
				_, err = os.Stat(journalPath(path))
				require.ErrorIs(err, os.ErrNotExist)
			}
		})
	}
}

func TestExecutor_JournalClearsTombstonesBeforeCrash(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	tc := NewTestContext(t)
	m := tc.CreateManifest("tombstone resume", msgIDs(3))
	tc.SeedMessages(msgIDs(3))
	m.Status = StatusInProgress
	m.Execution = &Execution{StartedAt: time.Now(), Method: MethodTrash, LastProcessedIndex: 1, TombstoneIDs: []string{"msg0"}}
	require.NoError(tc.Mgr.SaveManifest(m))
	tc.MockAPI.BeforeTrash = func(id string) error {
		if id == "msg2" {
			panic("simulated crash after checkpoint")
		}
		return nil
	}
	require.PanicsWithValue("simulated crash after checkpoint", func() {
		_ = tc.ExecuteWithOpts(m.ID, trashOpts(1))
	})
	loaded, _, err := tc.Mgr.GetManifest(m.ID)
	require.NoError(err)
	assert.Empty(loaded.Execution.TombstoneIDs)
	assert.Equal(2, loaded.Execution.Succeeded)
	tc.MockAPI.BeforeTrash = nil
	require.NoError(tc.ExecuteWithOpts(m.ID, trashOpts(1)))
	tc.AssertManifestExecution(m.ID, 3, 0)
	assert.Equal([]string{"msg1", "msg2"}, tc.MockAPI.TrashCalls)
}

func TestExecutor_CancelAfterCheckpoint(t *testing.T) {
	for _, readOnly := range []bool{false, true} {
		t.Run(fmt.Sprintf("readOnly=%v", readOnly), func(t *testing.T) {
			if readOnly && (runtime.GOOS == "windows" || os.Getuid() == 0) {
				t.Skip("requires Unix file permissions enforced for the current user")
			}
			require := require.New(t)
			assert := assert.New(t)
			tc := NewTestContext(t)
			m := tc.CreateManifest("cancel progress", msgIDs(3))
			path := filepath.Join(tc.Mgr.InProgressDir(), m.ID+".json")
			tc.MockAPI.BeforeTrash = func(id string) error {
				if id == "msg1" {
					if readOnly {
						// Atomic publication replaces even a read-only manifest.
						require.NoError(os.Chmod(path, 0o400))
					}
					require.NoError(tc.Mgr.CancelManifest(m.ID))
				}
				return nil
			}
			require.ErrorIs(tc.ExecuteWithOpts(m.ID, trashOpts(1)), ErrManifestCancelled)
			assert.True(tc.Mgr.ManifestCancelled(m.ID))
			assert.Equal([]string{"msg0", "msg1"}, tc.MockAPI.TrashCalls)
			cancelled, status, err := tc.Mgr.GetManifestWithStatus(m.ID)
			require.NoError(err)
			assert.Equal(StatusCancelled, status)
			require.NotNil(cancelled.Execution)
			assert.Equal(1, cancelled.Execution.Succeeded)
			assert.Equal(1, cancelled.Execution.LastProcessedIndex)
		})
	}
}

func TestManager_CancelManifest_JournalCleanupFailure(t *testing.T) {
	for _, failure := range []string{"none", "readonly", "cleanup"} {
		t.Run(failure, func(t *testing.T) {
			if failure != "none" && (runtime.GOOS == "windows" || os.Getuid() == 0) {
				t.Skip("requires Unix file permissions enforced for the current user")
			}
			require := require.New(t)
			assert := assert.New(t)
			mgr := testManager(t)
			m := setupInProgress(t, mgr, "cancel journal progress")
			m.Execution.LastProcessedIndex = 2
			m.Execution.Succeeded = 1
			m.Execution.Failed = 1
			m.Execution.FailedIDs = []string{"b"}
			require.NoError(mgr.WriteInProgressCheckpoint(m, m.ID, 0))
			path := filepath.Join(mgr.InProgressDir(), m.ID+".json")
			cancelledPath := filepath.Join(mgr.dirForStatus(StatusCancelled), m.ID+".json")
			original, err := os.Stat(path)
			require.NoError(err)
			// Prove progress is journaled before exercising the fold.
			_, err = os.Stat(journalPath(path))
			require.NoError(err)
			switch failure {
			case "readonly":
				// Atomic publication replaces even a read-only manifest.
				require.NoError(os.Chmod(path, 0o400))
				t.Cleanup(func() { require.NoError(os.Chmod(cancelledPath, 0o600)) })
			case "cleanup":
				// Status directories stay writable, so only journal removal fails.
				require.NoError(os.Chmod(mgr.baseDir, 0o500))
				t.Cleanup(func() { require.NoError(os.Chmod(mgr.baseDir, 0o700)) })
			}

			require.NoError(mgr.CancelManifest(m.ID))
			if failure == "none" {
				published, err := os.Stat(cancelledPath)
				require.NoError(err)
				assert.False(os.SameFile(original, published), "cancellation must replace the file instead of rewriting it in place")
			}
			assert.True(mgr.ManifestCancelled(m.ID))
			_, err = os.Stat(journalPath(path))
			if failure != "cleanup" {
				require.ErrorIs(err, os.ErrNotExist)
			} else {
				require.NoError(err, "journal remains when cleanup fails")
			}
			if failure == "cleanup" {
				// Reload without the journal to prove the fold saved progress.
				require.NoError(os.Chmod(mgr.baseDir, 0o700))
				require.NoError(os.Remove(journalPath(path)))
			}
			loaded, status, err := mgr.GetManifestWithStatus(m.ID)
			require.NoError(err)
			assert.Equal(StatusCancelled, status)
			assert.Equal(StatusCancelled, loaded.Status)
			require.NotNil(loaded.Execution)
			assert.Equal(2, loaded.Execution.LastProcessedIndex)
			assert.Equal(1, loaded.Execution.Succeeded)
			assert.Equal(1, loaded.Execution.Failed)
			assert.Equal([]string{"b"}, loaded.Execution.FailedIDs)
		})
	}
}

func TestExecutor_TombstoneStopReplacesFailures(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	tc := NewTestContext(t)
	m := tc.CreateManifest("tombstone restart", msgIDs(3))
	// A provider failure followed by a local tombstone failure leaves both
	// retry lists populated. Restarting without Resume must replace failures.
	tc.MockAPI.BeforeTrash = func(id string) error {
		if id == "msg0" {
			return errors.New("provider failure")
		}
		require.NoError(tc.Store.Close())
		return nil
	}
	require.ErrorIs(tc.ExecuteWithOpts(m.ID, trashOpts(1)), errLocalTombstone)
	loaded, _, err := tc.Mgr.GetManifest(m.ID)
	require.NoError(err)
	require.NotNil(loaded.Execution)
	assert.Equal(2, loaded.Execution.LastProcessedIndex)
	assert.Equal([]string{"msg0"}, loaded.Execution.FailedIDs)
	assert.Equal([]string{"msg1"}, loaded.Execution.TombstoneIDs)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	tc.Exec.WithProgress(&onStartProgress{hook: cancel})
	opts := trashOpts(1)
	opts.Resume = false
	require.ErrorIs(tc.Exec.Execute(ctx, m.ID, opts), context.Canceled)
	loaded, _, err = tc.Mgr.GetManifest(m.ID)
	require.NoError(err)
	assert.Equal([]string{"msg1"}, loaded.Execution.TombstoneIDs)
	assert.Empty(loaded.Execution.FailedIDs)
}
