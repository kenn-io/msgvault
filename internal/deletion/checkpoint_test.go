package deletion

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExecutor_CheckpointWritesGrowLinearly(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprintf("fail=%v", fail), func(t *testing.T) {
			var sizes []int64
			for _, n := range []int{10_000, 100_000} {
				t.Run(strconv.Itoa(n), func(t *testing.T) {
					if n == 100_000 && testing.Short() {
						t.Skip("large checkpoint run")
					}
					require := require.New(t)
					assert := assert.New(t)
					tc := NewTestContext(t)
					ids := make([]string, n)
					for i := range ids {
						ids[i] = fmt.Sprintf("msg-%06d", i)
					}
					m := tc.CreateManifest("linear checkpoints", ids)
					path := filepath.Join(tc.Mgr.InProgressDir(), m.ID+".json")
					var claimed os.FileInfo
					var journalSize int64
					calls := 0
					tc.MockAPI.BeforeTrash = func(string) error {
						info, err := os.Stat(path)
						require.NoError(err)
						if claimed == nil {
							claimed = info
						}
						assert.True(os.SameFile(claimed, info), "checkpoint replaced the full manifest")
						assert.Equal(claimed.Size(), info.Size())
						if calls >= 100 {
							journal, err := os.Stat(journalPath(path))
							require.NoError(err)
							journalSize = journal.Size()
						}
						calls++
						if fail {
							return errors.New("provider failure")
						}
						return nil
					}
					require.NoError(tc.ExecuteWithOpts(m.ID, trashOpts(100)))
					assert.Equal(n, calls)
					assert.Positive(journalSize)
					sizes = append(sizes, journalSize)
					t.Logf("%d IDs: %d checkpoint bytes", n, journalSize)
				})
			}
			if len(sizes) == 2 {
				assert.Greater(t, sizes[1], 9*sizes[0])
				assert.Less(t, sizes[1], 11*sizes[0])
			}
		})
	}
}

func TestExecutor_JournalResumeMatchesUninterrupted(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	tc := NewTestContext(t)
	m := tc.CreateManifest("interrupted", msgIDs(6))
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
	path := filepath.Join(tc.Mgr.InProgressDir(), m.ID+".json")
	loaded, err := LoadManifest(path)
	require.NoError(err)
	assert.Equal(3, loaded.Execution.LastProcessedIndex)
	assert.Equal(2, loaded.Execution.Succeeded)
	assert.Equal([]string{"msg1"}, loaded.Execution.FailedIDs)
	// An interrupted append must not hide the next complete checkpoint.
	f, err := os.OpenFile(journalPath(path), os.O_APPEND|os.O_WRONLY, 0)
	require.NoError(err)
	_, err = f.WriteString(`{"last_processed_index":`)
	require.NoError(err)
	require.NoError(f.Close())
	// A retry rebuilds the failure list, then ordinary checkpoints append to it.
	tc.MockAPI.BeforeTrash = func(id string) error {
		if id == "msg1" || id == "msg4" {
			return errors.New("provider failure")
		}
		return nil
	}
	require.NoError(tc.ExecuteWithOpts(m.ID, trashOpts(2)))
	resumed, _, err := tc.Mgr.GetManifest(m.ID)
	require.NoError(err)
	assert.Equal([]string{"msg0", "msg2", "msg3", "msg5"}, tc.MockAPI.TrashCalls)

	other := NewTestContext(t)
	whole := other.CreateManifest("uninterrupted", msgIDs(6))
	other.MockAPI.BeforeTrash = tc.MockAPI.BeforeTrash
	require.NoError(other.ExecuteWithOpts(whole.ID, trashOpts(2)))
	uninterrupted, _, err := other.Mgr.GetManifest(whole.ID)
	require.NoError(err)
	assert.Equal(uninterrupted.Execution.LastProcessedIndex, resumed.Execution.LastProcessedIndex)
	assert.Equal(uninterrupted.Execution.Succeeded, resumed.Execution.Succeeded)
	assert.Equal(uninterrupted.Execution.Failed, resumed.Execution.Failed)
	assert.Equal(uninterrupted.Execution.FailedIDs, resumed.Execution.FailedIDs)
}

func TestManager_JournalTornTailAndCompletedManifest(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	mgr := testManager(t)
	m := setupInProgress(t, mgr, "torn tail")
	path := filepath.Join(mgr.InProgressDir(), m.ID+".json")
	m.Execution.Succeeded = 1
	m.Execution.LastProcessedIndex = 1
	require.NoError(mgr.WriteInProgressCheckpoint(m, m.ID, 0))
	f, err := os.OpenFile(journalPath(path), os.O_APPEND|os.O_WRONLY, 0)
	require.NoError(err)
	_, err = f.WriteString(`{"succeeded":`)
	require.NoError(err)
	require.NoError(f.Close())
	loaded, err := LoadManifest(path)
	require.NoError(err)
	assert.Equal(1, loaded.Execution.Succeeded)
	m.Execution.Succeeded = 2
	m.Execution.LastProcessedIndex = 2
	require.NoError(mgr.WriteInProgressCheckpoint(m, m.ID, 0))
	loaded, err = LoadManifest(path)
	require.NoError(err)
	assert.Equal(2, loaded.Execution.Succeeded)
	m.Execution.CompletedAt = new(time.Now())
	m.Execution.Succeeded = 3
	require.NoError(m.Save(path)) // Simulate finalize publishing before journal cleanup.
	loaded, err = LoadManifest(path)
	require.NoError(err)
	assert.Equal(3, loaded.Execution.Succeeded)
	resumed, err := mgr.ClaimManifest(m.ID, MethodTrash)
	require.NoError(err)
	assert.Nil(resumed.Execution.CompletedAt)
	resumed.Execution.Succeeded = 4
	require.NoError(mgr.WriteInProgressCheckpoint(resumed, m.ID, 0))
	loaded, err = LoadManifest(path)
	require.NoError(err)
	assert.Equal(4, loaded.Execution.Succeeded)
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
	require.ErrorIs(tc.Mgr.WriteInProgressCheckpoint(cancelled, m.ID, 0), ErrManifestCancelled)
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
}
