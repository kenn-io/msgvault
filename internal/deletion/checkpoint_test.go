package deletion

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExecutor_CheckpointWritesGrowLinearly(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprintf("fail=%v", fail), func(t *testing.T) {
			ns := []int{10_000, 100_000}
			if testing.Short() {
				ns = ns[:1]
			}
			var sizes []int64
			for _, n := range ns {
				tc := NewTestContext(t)
				ids := make([]string, n)
				for i := range ids {
					ids[i] = fmt.Sprintf("msg-%06d", i)
				}
				m := tc.CreateManifest("linear checkpoints", ids)
				path := filepath.Join(tc.Mgr.InProgressDir(), m.ID+".json")
				var claimed os.FileInfo
				var journalSize int64
				tc.MockAPI.BeforeTrash = func(string) error {
					info, err := os.Stat(path)
					require.NoError(t, err)
					if claimed == nil {
						claimed = info
					}
					assert.True(t, os.SameFile(claimed, info) && claimed.Size() == info.Size(), "checkpoint rewrote the manifest")
					if journal, err := os.Stat(journalPath(path)); err == nil {
						journalSize = journal.Size()
					}
					if fail {
						return errors.New("provider failure")
					}
					return nil
				}
				require.NoError(t, tc.ExecuteWithOpts(m.ID, trashOpts(100)))
				require.Positive(t, journalSize)
				sizes = append(sizes, journalSize)
			}
			if len(sizes) == 2 {
				assert.Greater(t, sizes[1], 9*sizes[0])
				assert.Less(t, sizes[1], 11*sizes[0])
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

	// A torn tail must not hide the next record, and the retry pass's record
	// replaces the failure list rather than extending it.
	f, err := os.OpenFile(journalPath(path), os.O_APPEND|os.O_WRONLY, 0)
	require.NoError(err)
	_, err = f.WriteString(`{"last_processed_index":`)
	require.NoError(err)
	require.NoError(f.Close())
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

func TestExecutor_FinalizeJournalCleanupFailurePreservesProgress(t *testing.T) {
	if runtime.GOOS == "windows" || os.Getuid() == 0 {
		t.Skip("requires Unix file permissions enforced for the current user")
	}
	require := require.New(t)
	assert := assert.New(t)
	tc := NewTestContext(t)
	m := tc.CreateManifest("final journal cleanup failure", msgIDs(3))
	baseDir := tc.Mgr.baseDir
	t.Cleanup(func() { require.NoError(os.Chmod(baseDir, 0o700)) })
	tc.MockAPI.BeforeTrash = func(id string) error {
		if id == "msg2" {
			// Existing journal appends and the full write in in_progress still
			// succeed, but unlinking the journal in its parent must fail.
			require.NoError(os.Chmod(baseDir, 0o500))
		}
		return nil
	}
	err := tc.ExecuteWithOpts(m.ID, trashOpts(2))
	require.NoError(err)
	path := filepath.Join(tc.Mgr.CompletedDir(), m.ID+".json")
	_, err = os.Stat(journalPath(path))
	require.NoError(err)
	loaded, err := LoadManifest(path)
	require.NoError(err)
	assert.Equal(StatusCompleted, loaded.Status)
	assert.Equal(3, loaded.Execution.LastProcessedIndex)
	assert.Equal(3, loaded.Execution.Succeeded)
	assert.Zero(loaded.Execution.Failed)
	require.NotNil(loaded.Execution.CompletedAt)

	require.NoError(os.Chmod(baseDir, 0o700))
	tc.MockAPI.BeforeTrash = nil
	require.ErrorContains(tc.ExecuteWithOpts(m.ID, trashOpts(2)), "is completed, cannot execute")
	assert.Equal([]string{"msg0", "msg1", "msg2"}, tc.MockAPI.TrashCalls)
	tc.AssertManifestExecution(m.ID, 3, 0)
}

func TestExecutor_JournalClearsTombstonesBeforeCrash(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	tc := NewTestContext(t)
	m := tc.CreateManifest("tombstone resume", msgIDs(3))
	tc.SeedMessages(msgIDs(3))
	m.Status = StatusInProgress
	m.Execution = &Execution{Method: MethodTrash, LastProcessedIndex: 1, TombstoneIDs: []string{"msg0"}}
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

func TestExecutor_CancelFoldsJournal(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	tc := NewTestContext(t)
	m := tc.CreateManifest("cancel progress", msgIDs(3))
	tc.MockAPI.BeforeTrash = func(id string) error {
		if id == "msg1" {
			require.NoError(tc.Mgr.CancelManifest(m.ID))
		}
		return nil
	}
	require.ErrorIs(tc.ExecuteWithOpts(m.ID, trashOpts(1)), ErrManifestCancelled)
	cancelled, status, err := tc.Mgr.GetManifestWithStatus(m.ID)
	require.NoError(err)
	assert.Equal(StatusCancelled, status)
	assert.Equal(1, cancelled.Execution.Succeeded)
	assert.Equal(1, cancelled.Execution.LastProcessedIndex)
	_, err = os.Stat(journalPath(filepath.Join(tc.Mgr.InProgressDir(), m.ID+".json")))
	assert.ErrorIs(err, os.ErrNotExist)
}

func TestExecutor_CancelMovesBeforeFallibleRewrite(t *testing.T) {
	if runtime.GOOS == "windows" || os.Getuid() == 0 {
		t.Skip("requires Unix file permissions enforced for the current user")
	}
	require := require.New(t)
	assert := assert.New(t)
	tc := NewTestContext(t)
	m := tc.CreateManifest("cancel rewrite failure", msgIDs(3))
	tc.MockAPI.BeforeTrash = func(id string) error {
		if id == "msg1" {
			path := filepath.Join(tc.Mgr.InProgressDir(), m.ID+".json")
			// The rename succeeds, but saving the moved file must fail.
			require.NoError(os.Chmod(path, 0o400))
			require.ErrorIs(tc.Mgr.CancelManifest(m.ID), os.ErrPermission)
		}
		return nil
	}
	require.ErrorIs(tc.ExecuteWithOpts(m.ID, trashOpts(1)), ErrManifestCancelled)
	assert.True(tc.Mgr.ManifestCancelled(m.ID))
	assert.Equal([]string{"msg0", "msg1"}, tc.MockAPI.TrashCalls)
	cancelled, err := LoadManifest(filepath.Join(tc.Mgr.dirForStatus(StatusCancelled), m.ID+".json"))
	require.NoError(err)
	assert.Equal(1, cancelled.Execution.LastProcessedIndex)
	assert.Equal(1, cancelled.Execution.Succeeded)
}

func TestExecutor_TombstoneStopReplacesFailures(t *testing.T) {
	tc := NewTestContext(t)
	m := tc.CreateManifest("tombstone restart", msgIDs(3))
	m.Status = StatusInProgress
	m.Execution = &Execution{
		Method: MethodTrash, LastProcessedIndex: 2,
		Failed: 1, FailedIDs: []string{"msg0"}, TombstoneIDs: []string{"msg1"},
	}
	require.NoError(t, tc.Mgr.SaveManifest(m))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	tc.Exec.WithProgress(&onStartProgress{hook: cancel})
	opts := trashOpts(1)
	opts.Resume = false
	require.ErrorIs(t, tc.Exec.Execute(ctx, m.ID, opts), context.Canceled)
	loaded, _, err := tc.Mgr.GetManifest(m.ID)
	require.NoError(t, err)
	assert.Equal(t, []string{"msg1"}, loaded.Execution.TombstoneIDs)
	assert.Empty(t, loaded.Execution.FailedIDs)
}
