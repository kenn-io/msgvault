package deletion

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/gmail"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

// trackingProgress records progress events for testing.
type trackingProgress struct {
	mu             sync.Mutex
	startTotal     int
	startProcessed int
	progressLog    []struct{ processed, succeeded, failed int }
	completed      bool
	finalSucc      int
	finalFail      int
}

func (p *trackingProgress) OnStart(total, alreadyProcessed int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.startTotal = total
	p.startProcessed = alreadyProcessed
}

func (p *trackingProgress) OnProgress(processed, succeeded, failed int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.progressLog = append(p.progressLog, struct{ processed, succeeded, failed int }{processed, succeeded, failed})
}

func (p *trackingProgress) OnComplete(succeeded, failed int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.completed = true
	p.finalSucc = succeeded
	p.finalFail = failed
}

// TestContext encapsulates common test dependencies for executor tests.
type TestContext struct {
	Mgr      *Manager
	Store    *store.Store
	MockAPI  *gmail.DeletionMockAPI
	Exec     *Executor
	Progress *trackingProgress
	Dir      string
	t        *testing.T
}

// NewTestContext creates a new test context with all dependencies initialized.
func NewTestContext(t *testing.T) *TestContext {
	t.Helper()
	tmpDir := t.TempDir()
	mgr, err := NewManager(tmpDir)
	require.NoError(t, err, "NewManager()")

	st := testutil.NewTestStore(t)
	mockAPI := gmail.NewDeletionMockAPI()
	progress := &trackingProgress{}

	exec := NewExecutor(mgr, st, mockAPI).WithProgress(progress)

	return &TestContext{
		Mgr:      mgr,
		Store:    st,
		MockAPI:  mockAPI,
		Exec:     exec,
		Progress: progress,
		Dir:      tmpDir,
		t:        t,
	}
}

// CreateManifest creates a manifest with the given name and Gmail IDs.
func (c *TestContext) CreateManifest(name string, ids []string) *Manifest {
	c.t.Helper()
	manifest, err := c.Mgr.CreateManifest(name, ids, Filters{})
	require.NoError(c.t, err, "CreateManifest(%q)", name)
	return manifest
}

// Execute runs the executor with default options.
func (c *TestContext) Execute(manifestID string) error {
	return c.Exec.Execute(context.Background(), manifestID, nil)
}

// ExecuteWithOpts runs the executor with custom options.
func (c *TestContext) ExecuteWithOpts(manifestID string, opts *ExecuteOptions) error {
	return c.Exec.Execute(context.Background(), manifestID, opts)
}

// ExecuteBatch runs the batch executor.
func (c *TestContext) ExecuteBatch(manifestID string) error {
	return c.Exec.ExecuteBatch(context.Background(), manifestID)
}

// AssertResult verifies the final success and failure counts.
func (c *TestContext) AssertResult(wantSucc, wantFail int) {
	c.t.Helper()
	assert.Equal(c.t, wantSucc, c.Progress.finalSucc, "finalSucc")
	assert.Equal(c.t, wantFail, c.Progress.finalFail, "finalFail")
}

// AssertCompleted verifies that OnComplete was called.
func (c *TestContext) AssertCompleted() {
	c.t.Helper()
	assert.True(c.t, c.Progress.completed, "OnComplete was not called")
}

// AssertNotCompleted verifies that OnComplete was not called.
func (c *TestContext) AssertNotCompleted() {
	c.t.Helper()
	assert.False(c.t, c.Progress.completed, "OnComplete was called unexpectedly")
}

// AssertTrashCalls verifies the number of TrashMessage calls.
func (c *TestContext) AssertTrashCalls(want int) {
	c.t.Helper()
	assert.Len(c.t, c.MockAPI.TrashCalls, want, "TrashCalls")
}

// AssertDeleteCalls verifies the number of DeleteMessage calls.
func (c *TestContext) AssertDeleteCalls(want int) {
	c.t.Helper()
	assert.Len(c.t, c.MockAPI.DeleteCalls, want, "DeleteCalls")
}

// AssertCompletedCount verifies the number of completed manifests.
func (c *TestContext) AssertCompletedCount(want int) {
	c.t.Helper()
	completed, err := c.Mgr.ListCompleted()
	require.NoError(c.t, err, "ListCompleted()")
	assert.Len(c.t, completed, want, "ListCompleted()")
}

// AssertFailedCount verifies the number of failed manifests.
func (c *TestContext) AssertFailedCount(want int) {
	c.t.Helper()
	failed, err := c.Mgr.ListFailed()
	require.NoError(c.t, err, "ListFailed()")
	assert.Len(c.t, failed, want, "ListFailed()")
}

// AssertCancelledCount verifies the number of cancelled manifests.
func (c *TestContext) AssertCancelledCount(want int) {
	c.t.Helper()
	cancelled, err := c.Mgr.ListCancelled()
	require.NoError(c.t, err, "ListCancelled()")
	assert.Len(c.t, cancelled, want, "ListCancelled()")
}

// AssertManifestExecution verifies the persisted execution state of a manifest.
func (c *TestContext) AssertManifestExecution(id string, wantSucc, wantFail int, wantFailedIDs ...string) {
	c.t.Helper()
	m, _, err := c.Mgr.GetManifest(id)
	require.NoError(c.t, err, "GetManifest(%q)", id)
	assert.Equal(c.t, wantSucc, m.Execution.Succeeded, "Persisted Succeeded")
	assert.Equal(c.t, wantFail, m.Execution.Failed, "Persisted Failed")
	if len(m.Execution.FailedIDs) != len(wantFailedIDs) {
		assert.Len(c.t, m.Execution.FailedIDs, len(wantFailedIDs), "FailedIDs count")
	} else {
		for i, id := range wantFailedIDs {
			assert.Equal(c.t, id, m.Execution.FailedIDs[i], "FailedIDs[%d]", i)
		}
	}
}

// SimulateTrashError injects a trash error for a specific message ID.
func (c *TestContext) SimulateTrashError(msgID string) {
	c.MockAPI.TrashErrors[msgID] = errors.New("simulated trash error")
}

// SimulateDeleteError injects a delete error for a specific message ID.
func (c *TestContext) SimulateDeleteError(msgID string) {
	c.MockAPI.DeleteErrors[msgID] = errors.New("simulated delete error")
}

// SimulateNotFound injects a 404 not-found error for a specific message ID.
func (c *TestContext) SimulateNotFound(msgID string) {
	c.MockAPI.SetNotFoundError(msgID)
}

// SimulateBatchDeleteError sets the batch delete operation to fail.
func (c *TestContext) SimulateBatchDeleteError() {
	c.MockAPI.BatchDeleteError = errors.New("simulated batch error")
}

// AssertBatchDeleteCalls verifies the number of BatchDeleteMessages calls.
func (c *TestContext) AssertBatchDeleteCalls(want int) {
	c.t.Helper()
	assert.Len(c.t, c.MockAPI.BatchDeleteCalls, want, "BatchDeleteCalls")
}

// GetBatchDeleteCall safely retrieves a batch delete call by index.
func (c *TestContext) GetBatchDeleteCall(index int) []string {
	c.t.Helper()
	require.Less(c.t, index, len(c.MockAPI.BatchDeleteCalls), "BatchDeleteCalls index %d out of range (len=%d)", index, len(c.MockAPI.BatchDeleteCalls))
	return c.MockAPI.BatchDeleteCalls[index]
}

// AssertIsScopeError verifies that the error is an insufficient scope error.
func (c *TestContext) AssertIsScopeError(err error) {
	c.t.Helper()
	if err == nil {
		assert.Fail(c.t, "expected scope insufficient error", "got nil")
		return
	}
	assert.Contains(c.t, err.Error(), "ACCESS_TOKEN_SCOPE_INSUFFICIENT", "want scope insufficient error")
}

// msgIDs generates sequential message IDs like "msg0", "msg1", ..., "msg(n-1)".
func msgIDs(n int) []string {
	ids := make([]string, n)
	for i := range ids {
		ids[i] = fmt.Sprintf("msg%d", i)
	}
	return ids
}

// deleteOpts returns ExecuteOptions configured for permanent delete.
func deleteOpts(batchSize int) *ExecuteOptions {
	return &ExecuteOptions{
		Method:    MethodDelete,
		BatchSize: batchSize,
		Resume:    true,
	}
}

// trashOpts returns ExecuteOptions configured for trash with a custom batch size.
func trashOpts(batchSize int) *ExecuteOptions {
	return &ExecuteOptions{
		Method:    MethodTrash,
		BatchSize: batchSize,
		Resume:    true,
	}
}

// SimulateScopeError injects an insufficient scope error for a specific message ID.
func (c *TestContext) SimulateScopeError(msgID string) {
	scopeErr := errors.New("googleapi: Error 403: Insufficient Permission: ACCESS_TOKEN_SCOPE_INSUFFICIENT")
	c.MockAPI.TrashErrors[msgID] = scopeErr
	c.MockAPI.DeleteErrors[msgID] = scopeErr
}

// SimulateBatchScopeError sets the batch delete operation to fail with a scope error.
func (c *TestContext) SimulateBatchScopeError() {
	c.MockAPI.BatchDeleteError = errors.New("googleapi: Error 403: Insufficient Permission: ACCESS_TOKEN_SCOPE_INSUFFICIENT")
}

// AssertInProgressCount verifies the number of in-progress manifests.
func (c *TestContext) AssertInProgressCount(want int) {
	c.t.Helper()
	inProgress, err := c.Mgr.ListInProgress()
	require.NoError(c.t, err, "ListInProgress()")
	assert.Len(c.t, inProgress, want, "ListInProgress()")
}

// AssertManifestLastProcessedIndex verifies the persisted LastProcessedIndex.
func (c *TestContext) AssertManifestLastProcessedIndex(id string, want int) {
	c.t.Helper()
	m, _, err := c.Mgr.GetManifest(id)
	require.NoError(c.t, err, "GetManifest(%q)", id)
	require.NotNil(c.t, m.Execution, "manifest %q has nil Execution", id)
	assert.Equal(c.t, want, m.Execution.LastProcessedIndex, "LastProcessedIndex")
}

func TestNullProgress(t *testing.T) {
	// NullProgress should not panic
	p := NullProgress{}
	p.OnStart(10, 0)
	p.OnProgress(5, 4, 1)
	p.OnComplete(9, 1)
}

func TestDefaultExecuteOptions(t *testing.T) {
	opts := DefaultExecuteOptions()

	assert.Equal(t, MethodTrash, opts.Method)
	assert.Equal(t, 100, opts.BatchSize)
	assert.True(t, opts.Resume, "Resume should be true")
}

func TestNewExecutor(t *testing.T) {
	tmpDir := t.TempDir()
	mgr, err := NewManager(tmpDir)
	require.NoError(t, err, "NewManager()")

	store := testutil.NewTestStore(t)
	mockAPI := gmail.NewDeletionMockAPI()

	exec := NewExecutor(mgr, store, mockAPI)
	assert.NotNil(t, exec, "NewExecutor returned nil")
}

func TestExecutor_WithLogger(t *testing.T) {
	tmpDir := t.TempDir()
	mgr, err := NewManager(tmpDir)
	require.NoError(t, err, "NewManager()")
	store := testutil.NewTestStore(t)

	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	exec := NewExecutor(mgr, store, gmail.NewDeletionMockAPI()).WithLogger(logger)

	assert.Same(t, logger, exec.logger, "WithLogger did not set logger")
}

func TestExecutor_WithProgress(t *testing.T) {
	tmpDir := t.TempDir()
	mgr, err := NewManager(tmpDir)
	require.NoError(t, err, "NewManager()")
	store := testutil.NewTestStore(t)

	progress := &trackingProgress{}
	exec := NewExecutor(mgr, store, gmail.NewDeletionMockAPI()).WithProgress(progress)

	assert.Same(t, progress, exec.progress, "WithProgress did not set progress")
}

func TestExecutor_Execute_Scenarios(t *testing.T) {
	tests := []struct {
		name       string
		ids        []string
		setup      func(*TestContext)
		opts       *ExecuteOptions
		wantSucc   int
		wantFail   int
		wantErr    bool
		scopeError bool
		assertions func(*testing.T, *TestContext, *Manifest)
	}{
		{
			name:     "Success",
			ids:      msgIDs(3),
			wantSucc: 3, wantFail: 0,
			assertions: func(t *testing.T, ctx *TestContext, m *Manifest) {
				t.Helper()
				ctx.AssertTrashCalls(3)
				ctx.AssertCompleted()
				ctx.AssertCompletedCount(1)
			},
		},
		{
			name:     "WithDeleteMethod",
			ids:      msgIDs(2),
			opts:     deleteOpts(100),
			wantSucc: 2, wantFail: 0,
			assertions: func(t *testing.T, ctx *TestContext, m *Manifest) {
				t.Helper()
				ctx.AssertDeleteCalls(2)
				ctx.AssertTrashCalls(0)
			},
		},
		{
			name:     "WithFailures",
			ids:      msgIDs(3),
			setup:    func(c *TestContext) { c.SimulateTrashError("msg1") },
			wantSucc: 2, wantFail: 1,
			assertions: func(t *testing.T, ctx *TestContext, m *Manifest) {
				t.Helper()
				ctx.AssertCompletedCount(1)
				ctx.AssertManifestExecution(m.ID, 2, 1, "msg1")
			},
		},
		{
			name: "AllFail",
			ids:  msgIDs(2),
			setup: func(c *TestContext) {
				c.SimulateTrashError("msg0")
				c.SimulateTrashError("msg1")
			},
			wantSucc: 0, wantFail: 2,
			assertions: func(t *testing.T, ctx *TestContext, m *Manifest) {
				t.Helper()
				ctx.AssertFailedCount(1)
			},
		},
		{
			name:     "SmallBatchSize",
			ids:      msgIDs(5),
			opts:     trashOpts(2),
			wantSucc: 5, wantFail: 0,
			assertions: func(t *testing.T, ctx *TestContext, m *Manifest) {
				t.Helper()
				ctx.AssertTrashCalls(5)
			},
		},
		{
			name:     "NotFoundTreatedAsSuccess",
			ids:      msgIDs(3),
			setup:    func(c *TestContext) { c.SimulateNotFound("msg1") },
			wantSucc: 3, wantFail: 0,
			assertions: func(t *testing.T, ctx *TestContext, m *Manifest) {
				t.Helper()
				ctx.AssertCompletedCount(1)
				ctx.AssertManifestExecution(m.ID, 3, 0)
			},
		},
		{
			name: "MixedErrors",
			ids:  msgIDs(5),
			setup: func(c *TestContext) {
				c.SimulateNotFound("msg2")
				c.SimulateTrashError("msg4")
			},
			wantSucc: 4, wantFail: 1,
		},
		{
			name:     "WithDeleteMethod404",
			ids:      msgIDs(3),
			opts:     deleteOpts(100),
			setup:    func(c *TestContext) { c.SimulateNotFound("msg1") },
			wantSucc: 3, wantFail: 0,
		},
		{
			name:       "ScopeError",
			ids:        []string{"msg0", "msg1", "msg2"},
			setup:      func(c *TestContext) { c.SimulateScopeError("msg1") },
			wantErr:    true,
			scopeError: true,
			assertions: func(t *testing.T, ctx *TestContext, m *Manifest) {
				t.Helper()
				ctx.AssertNotCompleted()
				ctx.AssertInProgressCount(1)
				ctx.AssertManifestLastProcessedIndex(m.ID, 1)
				ctx.AssertManifestExecution(m.ID, 1, 0)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := NewTestContext(t)
			if tt.setup != nil {
				tt.setup(ctx)
			}
			manifest := ctx.CreateManifest(tt.name, tt.ids)

			var err error
			if tt.opts != nil {
				err = ctx.ExecuteWithOpts(manifest.ID, tt.opts)
			} else {
				err = ctx.Execute(manifest.ID)
			}

			if tt.wantErr {
				require.Error(t, err, "expected error")
				if tt.scopeError {
					ctx.AssertIsScopeError(err)
				}
			} else {
				require.NoError(t, err, "unexpected error")
				ctx.AssertResult(tt.wantSucc, tt.wantFail)
			}

			if tt.assertions != nil {
				tt.assertions(t, ctx, manifest)
			}
		})
	}
}

func TestExecutor_Execute_ContextCancelled(t *testing.T) {
	ctx := NewTestContext(t)

	manifest := ctx.CreateManifest("interrupt test", msgIDs(100))

	// Cancel context immediately
	execCtx, cancel := context.WithCancel(context.Background())
	cancel()

	err := ctx.Exec.Execute(execCtx, manifest.ID, nil)
	require.ErrorIs(t, err, context.Canceled, "Execute() error")

	ctx.AssertNotCompleted()

	// Manifest should remain in in_progress (for resume)
	ctx.AssertInProgressCount(1)
}

func TestExecutor_Execute_ManifestNotFound(t *testing.T) {
	ctx := NewTestContext(t)

	err := ctx.Execute("nonexistent-id")
	assert.Error(t, err, "Execute() should error for nonexistent manifest")
}

func TestExecutor_Execute_InvalidStatus(t *testing.T) {
	ctx := NewTestContext(t)
	manifest := ctx.CreateManifest("completed test", msgIDs(1))

	// Execute to completion
	require.NoError(t, ctx.Execute(manifest.ID), "Execute()")

	// Try to execute again
	err := ctx.Execute(manifest.ID)
	assert.Error(t, err, "Execute() should error for completed manifest")
}

func TestExecutor_Execute_ResumeFromInProgress(t *testing.T) {
	tc := NewTestContext(t)

	// Create a manifest that's already in_progress with some progress
	gmailIDs := msgIDs(5)
	manifest := NewManifest("in-progress resume", gmailIDs)
	manifest.Status = StatusInProgress
	manifest.Execution = &Execution{
		StartedAt:          time.Now().Add(-time.Hour),
		Method:             MethodTrash,
		Succeeded:          2,
		Failed:             0,
		LastProcessedIndex: 2, // Already processed msg1 and msg2
	}
	require.NoError(t, tc.Mgr.SaveManifest(manifest), "SaveManifest()")

	require.NoError(t, tc.ExecuteWithOpts(manifest.ID, trashOpts(100)), "Execute()")

	// Should only process msg3, msg4, msg5 (skipping msg1, msg2)
	tc.AssertTrashCalls(3)

	// Verify final counts include all 5
	tc.AssertManifestExecution(manifest.ID, 5, 0)
}

// TestExecutor_Execute_RejectsMethodMismatchOnResume verifies that a manifest
// claimed and checkpointed with one method cannot be resumed with another:
// resuming a permanent-delete batch through the trash path (or the reverse)
// must fail before any message is touched, leaving the manifest resumable.
func TestExecutor_Execute_RejectsMethodMismatchOnResume(t *testing.T) {
	require := require.New(t)
	tc := NewTestContext(t)

	manifest := NewManifest("method mismatch", msgIDs(5))
	manifest.Status = StatusInProgress
	manifest.Execution = &Execution{
		StartedAt:          time.Now().Add(-time.Hour),
		Method:             MethodDelete,
		Succeeded:          2,
		LastProcessedIndex: 2,
	}
	require.NoError(tc.Mgr.SaveManifest(manifest), "SaveManifest()")

	err := tc.ExecuteWithOpts(manifest.ID, trashOpts(100))
	require.ErrorContains(err, "cannot be resumed with method", "trash resume of a delete batch")
	tc.AssertTrashCalls(0)
	tc.AssertDeleteCalls(0)
	tc.AssertInProgressCount(1)
}

// TestExecutor_ExecuteBatch_RejectsTrashResume verifies the reverse and more
// dangerous direction: ExecuteBatch always deletes permanently, so resuming a
// recoverable trash batch through it must be rejected rather than silently
// switching the remaining messages to permanent deletion. The batch then
// resumes cleanly through the trash path it was started with.
func TestExecutor_ExecuteBatch_RejectsTrashResume(t *testing.T) {
	require := require.New(t)
	tc := NewTestContext(t)

	manifest := NewManifest("trash resumed as batch", msgIDs(5))
	manifest.Status = StatusInProgress
	manifest.Execution = &Execution{
		StartedAt:          time.Now().Add(-time.Hour),
		Method:             MethodTrash,
		Succeeded:          2,
		LastProcessedIndex: 2,
	}
	require.NoError(tc.Mgr.SaveManifest(manifest), "SaveManifest()")

	err := tc.ExecuteBatch(manifest.ID)
	require.ErrorContains(err, "cannot be resumed with method", "batch resume of a trash batch")
	tc.AssertBatchDeleteCalls(0)
	tc.AssertDeleteCalls(0)
	tc.AssertInProgressCount(1)

	require.NoError(tc.ExecuteWithOpts(manifest.ID, trashOpts(100)), "trash resume after rejection")
	tc.AssertTrashCalls(3)
	tc.AssertManifestExecution(manifest.ID, 5, 0)
}

func TestExecutor_DeleteOne_LocalTombstoneFailureIsRetryable(t *testing.T) {
	tc := NewTestContext(t)
	require.NoError(t, tc.Store.Close(), "close store")

	result, err := tc.Exec.deleteOne(context.Background(), 1, "msg-1", MethodDelete)

	assert.Equal(t, resultFailed, result)
	assert.Error(t, err)
	assert.Len(t, tc.MockAPI.DeleteCalls, 1)
}

func TestExecutor_Execute_LocalTombstoneFailureLeavesManifestInProgress(t *testing.T) {
	tc := NewTestContext(t)
	manifest := tc.CreateManifest("local tombstone retry", []string{"msg-1"})
	require.NoError(t, tc.Store.Close(), "close store")

	err := tc.ExecuteWithOpts(manifest.ID, &ExecuteOptions{Method: MethodDelete, BatchSize: 1, Resume: true})

	assert.Error(t, err)
	inProgress, listErr := tc.Mgr.ListInProgress()
	require.NoError(t, listErr)
	if assert.Len(t, inProgress, 1) {
		assert.Equal(t, []string{"msg-1"}, inProgress[0].Execution.FailedIDs)
	}
}

func TestExecutor_ExecuteBatch_LocalTombstoneFailureDuringRetryLeavesManifestInProgress(t *testing.T) {
	tc := NewTestContext(t)
	manifest := NewManifest("batch tombstone retry", []string{"msg-1"})
	manifest.Status = StatusInProgress
	manifest.Execution = &Execution{
		StartedAt:          time.Now().Add(-time.Hour),
		Method:             MethodDelete,
		FailedIDs:          []string{"msg-1"},
		LastProcessedIndex: 1,
	}
	require.NoError(t, tc.Mgr.SaveManifest(manifest), "SaveManifest")
	require.NoError(t, tc.Store.Close(), "close store")

	err := tc.ExecuteBatch(manifest.ID)

	assert.Error(t, err)
	inProgress, listErr := tc.Mgr.ListInProgress()
	require.NoError(t, listErr)
	if assert.Len(t, inProgress, 1) {
		assert.Equal(t, []string{"msg-1"}, inProgress[0].Execution.FailedIDs)
	}
}

// TestExecutor_Execute_ResumeRetriesFailedIDs verifies that resuming a trash
// manifest retries checkpointed transient failures before continuing from
// LastProcessedIndex, mirroring ExecuteBatch — instead of skipping them and
// finalizing as completed while the messages remain undeleted.
func TestExecutor_Execute_ResumeRetriesFailedIDs(t *testing.T) {
	tc := NewTestContext(t)

	manifest := NewManifest("trash retry", msgIDs(5))
	manifest.Status = StatusInProgress
	manifest.Execution = &Execution{
		StartedAt:          time.Now().Add(-time.Hour),
		Method:             MethodTrash,
		Succeeded:          2,
		Failed:             1,
		FailedIDs:          []string{"msg1"},
		LastProcessedIndex: 3, // msg0..msg2 processed; msg1 failed transiently
	}
	require.NoError(t, tc.Mgr.SaveManifest(manifest), "SaveManifest()")

	require.NoError(t, tc.ExecuteWithOpts(manifest.ID, trashOpts(100)), "Execute()")

	// msg1 retried, then msg3 and msg4 continued.
	tc.AssertTrashCalls(3)
	tc.AssertManifestExecution(manifest.ID, 5, 0)
	tc.AssertCompletedCount(1)
}

// TestExecutor_Execute_ResumeRetryStillFailing verifies that a checkpointed
// failure that fails again on the resume retry stays recorded in FailedIDs
// rather than being double-counted or dropped.
func TestExecutor_Execute_ResumeRetryStillFailing(t *testing.T) {
	tc := NewTestContext(t)
	tc.SimulateTrashError("msg1")

	manifest := NewManifest("trash retry still failing", msgIDs(5))
	manifest.Status = StatusInProgress
	manifest.Execution = &Execution{
		StartedAt:          time.Now().Add(-time.Hour),
		Method:             MethodTrash,
		Succeeded:          2,
		Failed:             1,
		FailedIDs:          []string{"msg1"},
		LastProcessedIndex: 3,
	}
	require.NoError(t, tc.Mgr.SaveManifest(manifest), "SaveManifest()")

	require.NoError(t, tc.ExecuteWithOpts(manifest.ID, trashOpts(100)), "Execute()")

	tc.AssertManifestExecution(manifest.ID, 4, 1, "msg1")
	tc.AssertCompletedCount(1)
}

// TestExecutor_Finalize_TerminalFileCarriesFinalState verifies that the final
// status and counters are durable BEFORE the manifest is moved into its
// terminal directory, so the file that lands there already carries them
// instead of depending on a post-rename save.
//
// The crash window is simulated by occupying the manifest's destination path
// in completed/ with a directory, which fails the rename on every OS at
// exactly the point a crash would interrupt it (unlike chmod, which does not
// make a directory unwritable on Windows). The durable record must still hold
// the final state: with the write ordered after the move, the interrupted
// manifest would be left serialized as in_progress with no CompletedAt.
func TestExecutor_Finalize_TerminalFileCarriesFinalState(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	tc := NewTestContext(t)

	manifest := tc.CreateManifest("final state", msgIDs(3))
	blocker := filepath.Join(tc.Mgr.CompletedDir(), manifest.ID+".json")
	// Occupied only once execution is under way: a terminal file present
	// beforehand is (correctly) refused by ClaimManifest as an already
	// finished batch, which would never reach finalization.
	tc.Exec.WithProgress(&onStartProgress{hook: func() {
		require.NoError(os.Mkdir(blocker, 0o755), "occupy the terminal destination path")
	}})

	err := tc.ExecuteWithOpts(manifest.ID, trashOpts(100))
	require.Error(err, "the interrupted move must surface")
	require.NoError(os.Remove(blocker), "clear the blocking directory")

	loaded, err := LoadManifest(filepath.Join(tc.Mgr.InProgressDir(), manifest.ID+".json"))
	require.NoError(err, "load the durable record left by the interrupted finalize")
	assert.Equal(StatusCompleted, loaded.Status, "final status persisted before the move")
	require.NotNil(loaded.Execution, "execution state")
	assert.Equal(3, loaded.Execution.Succeeded)
	assert.Equal(3, loaded.Execution.LastProcessedIndex)
	require.NotNil(loaded.Execution.CompletedAt, "CompletedAt persisted before the move")
}

// TestExecutor_Finalize_CompletedFileIsSelfConsistent verifies the ordinary
// path: the file in completed/ carries its own final status and counters, so
// readers never depend on the directory to correct a stale inline status.
func TestExecutor_Finalize_CompletedFileIsSelfConsistent(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	tc := NewTestContext(t)

	manifest := tc.CreateManifest("self consistent", msgIDs(3))
	require.NoError(tc.ExecuteWithOpts(manifest.ID, trashOpts(100)), "Execute()")

	loaded, err := LoadManifest(filepath.Join(tc.Mgr.CompletedDir(), manifest.ID+".json"))
	require.NoError(err, "load completed manifest")
	assert.Equal(StatusCompleted, loaded.Status)
	require.NotNil(loaded.Execution)
	assert.Equal(3, loaded.Execution.Succeeded)
	assert.Equal(0, loaded.Execution.Failed)
	require.NotNil(loaded.Execution.CompletedAt)

	_, statErr := os.Stat(filepath.Join(tc.Mgr.InProgressDir(), manifest.ID+".json"))
	assert.True(os.IsNotExist(statErr), "in_progress file removed by the finalize rename")
}

// onStartProgress runs a hook when execution starts — after the manifest is
// claimed into in_progress/, before any message is deleted — so a test can
// disturb the manifest's on-disk state mid-execution.
type onStartProgress struct {
	hook func()
}

func (p *onStartProgress) OnStart(total, alreadyProcessed int)         { p.hook() }
func (p *onStartProgress) OnProgress(processed, succeeded, failed int) {}
func (p *onStartProgress) OnComplete(succeeded, failed int)            {}

// TestExecutor_Finalize_PropagatesPersistFailure verifies that a failure to
// persist the final state is returned rather than logged and swallowed, so
// the caller never reports success over a manifest whose durable record was
// never updated.
//
// The write is failed by replacing the claimed in_progress file with a
// directory once execution is under way, which defeats the atomic writer's
// final rename on every OS (chmod does not make a directory unwritable on
// Windows).
func TestExecutor_Finalize_PropagatesPersistFailure(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	tc := NewTestContext(t)

	manifest := tc.CreateManifest("persist failure", msgIDs(2))
	inProgressPath := filepath.Join(tc.Mgr.InProgressDir(), manifest.ID+".json")
	tc.Exec.WithProgress(&onStartProgress{hook: func() {
		require.NoError(os.Remove(inProgressPath), "remove the claimed manifest file")
		require.NoError(os.Mkdir(inProgressPath, 0o755), "occupy its path with a directory")
	}})

	err := tc.ExecuteWithOpts(manifest.ID, trashOpts(100))
	require.Error(err, "a final-state write failure must surface")
	assert.Contains(err.Error(), "persist final state")

	// The manifest was not moved into a terminal directory, and no stray temp
	// file was left behind by the failed atomic write.
	tc.AssertCompletedCount(0)
	tc.AssertFailedCount(0)
	require.NoError(os.Remove(inProgressPath), "clear the blocking directory")
	entries, err := os.ReadDir(tc.Mgr.InProgressDir())
	require.NoError(err, "ReadDir in_progress")
	assert.Empty(entries, "no leftover temp files from the failed write")
}

func TestExecutor_ExecuteBatch_Scenarios(t *testing.T) {
	tests := []struct {
		name       string
		ids        []string
		setup      func(*TestContext)
		wantSucc   int
		wantFail   int
		wantErr    bool
		scopeError bool
		assertions func(*testing.T, *TestContext, *Manifest)
	}{
		{
			name:     "Success",
			ids:      msgIDs(3),
			wantSucc: 3, wantFail: 0,
			assertions: func(t *testing.T, ctx *TestContext, m *Manifest) {
				t.Helper()
				ctx.AssertBatchDeleteCalls(1)
				assert.Len(t, ctx.GetBatchDeleteCall(0), 3, "BatchDeleteCalls[0] length")
				ctx.AssertCompleted()
				ctx.AssertCompletedCount(1)
			},
		},
		{
			name:     "LargeBatch",
			ids:      msgIDs(1500),
			wantSucc: 1500, wantFail: 0,
			assertions: func(t *testing.T, ctx *TestContext, m *Manifest) {
				t.Helper()
				ctx.AssertBatchDeleteCalls(2)
				assert.Len(t, ctx.GetBatchDeleteCall(0), 1000, "BatchDeleteCalls[0] length")
				assert.Len(t, ctx.GetBatchDeleteCall(1), 500, "BatchDeleteCalls[1] length")
			},
		},
		{
			name:     "WithBatchError",
			ids:      msgIDs(3),
			setup:    func(c *TestContext) { c.SimulateBatchDeleteError() },
			wantSucc: 3, wantFail: 0,
			assertions: func(t *testing.T, ctx *TestContext, m *Manifest) {
				t.Helper()
				ctx.AssertBatchDeleteCalls(1)
				ctx.AssertDeleteCalls(3)
			},
		},
		{
			name:     "FallbackNotFoundTreatedAsSuccess",
			ids:      msgIDs(3),
			setup:    func(c *TestContext) { c.SimulateBatchDeleteError(); c.SimulateNotFound("msg1") },
			wantSucc: 3, wantFail: 0,
		},
		{
			name:     "FallbackWithNon404Failures",
			ids:      msgIDs(3),
			setup:    func(c *TestContext) { c.SimulateBatchDeleteError(); c.SimulateDeleteError("msg1") },
			wantSucc: 2, wantFail: 1,
		},
		{
			name: "FallbackMixed",
			ids:  msgIDs(4),
			setup: func(c *TestContext) {
				c.SimulateBatchDeleteError()
				c.SimulateNotFound("msg2")
				c.SimulateDeleteError("msg3")
			},
			wantSucc: 3, wantFail: 1,
			assertions: func(t *testing.T, ctx *TestContext, m *Manifest) {
				t.Helper()
				ctx.AssertBatchDeleteCalls(1)
				ctx.AssertDeleteCalls(4)
			},
		},
		{
			name: "AllFail",
			ids:  msgIDs(2),
			setup: func(c *TestContext) {
				c.SimulateBatchDeleteError()
				c.SimulateDeleteError("msg0")
				c.SimulateDeleteError("msg1")
			},
			wantSucc: 0, wantFail: 2,
			assertions: func(t *testing.T, ctx *TestContext, m *Manifest) {
				t.Helper()
				// Batch mode always marks as Completed even when all fail
				ctx.AssertCompletedCount(1)
				ctx.AssertFailedCount(0)
			},
		},
		{
			name:       "ScopeError",
			ids:        msgIDs(3),
			setup:      func(c *TestContext) { c.SimulateBatchScopeError() },
			wantErr:    true,
			scopeError: true,
			assertions: func(t *testing.T, ctx *TestContext, m *Manifest) {
				t.Helper()
				ctx.AssertNotCompleted()
				ctx.AssertInProgressCount(1)
				ctx.AssertManifestLastProcessedIndex(m.ID, 0)
			},
		},
		{
			name: "FallbackScopeError",
			ids:  []string{"msg0", "msg1", "msg2", "msg3"},
			setup: func(c *TestContext) {
				c.SimulateBatchDeleteError()
				c.SimulateScopeError("msg2")
			},
			wantErr:    true,
			scopeError: true,
			assertions: func(t *testing.T, ctx *TestContext, m *Manifest) {
				t.Helper()
				ctx.AssertNotCompleted()
				ctx.AssertInProgressCount(1)
				ctx.AssertManifestLastProcessedIndex(m.ID, 2)
				ctx.AssertManifestExecution(m.ID, 2, 0)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := NewTestContext(t)
			if tt.setup != nil {
				tt.setup(ctx)
			}
			manifest := ctx.CreateManifest(tt.name, tt.ids)

			err := ctx.ExecuteBatch(manifest.ID)

			if tt.wantErr {
				require.Error(t, err, "expected error")
				if tt.scopeError {
					ctx.AssertIsScopeError(err)
				}
			} else {
				require.NoError(t, err, "unexpected error")
				ctx.AssertResult(tt.wantSucc, tt.wantFail)
			}

			if tt.assertions != nil {
				tt.assertions(t, ctx, manifest)
			}
		})
	}
}

// TestExecutor_ExecuteBatch_InvalidStatus verifies that a manifest already in
// a terminal state cannot be executed again. A manifest whose file merely
// lives in in_progress/ (e.g. moved there directly, without an inline status
// update) is a valid claim target, not an invalid one — see
// TestManager_ClaimManifest_ResumeIgnoresStaleInlineStatus — so this test
// exercises the genuinely invalid case: completed.
func TestExecutor_ExecuteBatch_InvalidStatus(t *testing.T) {
	ctx := NewTestContext(t)
	manifest := ctx.CreateManifest("wrong status", msgIDs(1))

	require.NoError(t, ctx.ExecuteBatch(manifest.ID), "ExecuteBatch() first run")

	err := ctx.ExecuteBatch(manifest.ID)
	assert.Error(t, err, "ExecuteBatch() should error for a completed manifest")
}

func TestExecutor_ExecuteBatch_ContextCancelled(t *testing.T) {
	ctx := NewTestContext(t)

	manifest := ctx.CreateManifest("cancel batch", msgIDs(2500))

	// Cancel context immediately
	execCtx, cancel := context.WithCancel(context.Background())
	cancel()

	err := ctx.Exec.ExecuteBatch(execCtx, manifest.ID)
	require.ErrorIs(t, err, context.Canceled, "ExecuteBatch() error")

	ctx.AssertNotCompleted()
}

func TestExecutor_ExecuteBatch_ManifestNotFound(t *testing.T) {
	ctx := NewTestContext(t)

	err := ctx.ExecuteBatch("nonexistent-id")
	assert.Error(t, err, "ExecuteBatch() should error for nonexistent manifest")
}

// TestExecutor_ExecuteBatch_RetriesFailedIDs verifies that resuming a batch
// execution retries previously failed message IDs.
func TestExecutor_ExecuteBatch_RetriesFailedIDs(t *testing.T) {
	tc := NewTestContext(t)

	// Create a manifest that's already in_progress with failed IDs
	gmailIDs := msgIDs(5)
	manifest := NewManifest("retry test", gmailIDs)
	manifest.Status = StatusInProgress
	manifest.Execution = &Execution{
		StartedAt:          time.Now().Add(-time.Hour),
		Method:             MethodDelete,
		Succeeded:          2,
		Failed:             3,
		FailedIDs:          []string{"msg2", "msg3", "msg4"},
		LastProcessedIndex: 5, // All processed, but 3 failed
	}
	require.NoError(t, tc.Mgr.SaveManifest(manifest), "SaveManifest()")

	require.NoError(t, tc.ExecuteBatch(manifest.ID), "ExecuteBatch()")

	// The 3 previously failed IDs should be retried via individual delete
	tc.AssertDeleteCalls(3)
	// All should succeed now (no errors injected)
	tc.AssertResult(5, 0)
	tc.AssertCompletedCount(1)
}

// TestExecutor_ExecuteBatch_RetryPartialSuccess verifies that retried IDs that
// still fail are tracked correctly.
func TestExecutor_ExecuteBatch_RetryPartialSuccess(t *testing.T) {
	tc := NewTestContext(t)
	tc.SimulateDeleteError("msg3") // msg3 still fails on retry

	gmailIDs := msgIDs(5)
	manifest := NewManifest("retry partial", gmailIDs)
	manifest.Status = StatusInProgress
	manifest.Execution = &Execution{
		StartedAt:          time.Now().Add(-time.Hour),
		Method:             MethodDelete,
		Succeeded:          2,
		Failed:             3,
		FailedIDs:          []string{"msg2", "msg3", "msg4"},
		LastProcessedIndex: 5,
	}
	require.NoError(t, tc.Mgr.SaveManifest(manifest), "SaveManifest()")

	require.NoError(t, tc.ExecuteBatch(manifest.ID), "ExecuteBatch()")

	// msg2, msg4 succeed on retry; msg3 still fails
	tc.AssertResult(4, 1)
	tc.AssertCompletedCount(1)
}

// TestExecutor_ExecuteBatch_RetryScopeErrorAfterPartialSuccess verifies that
// a scope error during retry only preserves unattempted+failed IDs, not
// already-succeeded ones.
func TestExecutor_ExecuteBatch_RetryScopeErrorAfterPartialSuccess(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	tc := NewTestContext(t)
	// msg3 hits scope error; msg2 succeeds before it, msg4 is unattempted
	tc.SimulateScopeError("msg3")

	gmailIDs := msgIDs(5)
	manifest := NewManifest("retry scope partial", gmailIDs)
	manifest.Status = StatusInProgress
	manifest.Execution = &Execution{
		StartedAt:          time.Now().Add(-time.Hour),
		Method:             MethodDelete,
		Succeeded:          2,
		Failed:             3,
		FailedIDs:          []string{"msg2", "msg3", "msg4"},
		LastProcessedIndex: 5,
	}
	require.NoError(tc.Mgr.SaveManifest(manifest), "SaveManifest()")

	err := tc.ExecuteBatch(manifest.ID)
	require.Error(err, "ExecuteBatch() should return error for scope error during retry")
	tc.AssertIsScopeError(err)

	// msg2 succeeded before the scope error on msg3
	// Checkpoint should have: FailedIDs = [msg3, msg4] (current + unattempted)
	m, _, err := tc.Mgr.GetManifest(manifest.ID)
	require.NoError(err, "GetManifest()")
	assert.Equal(3, m.Execution.Succeeded, "Succeeded (original 2 + msg2 retry)")
	assert.Equal(2, m.Execution.Failed, "Failed (msg3 + msg4)")
	require.Len(m.Execution.FailedIDs, 2)
	assert.True(m.Execution.FailedIDs[0] == "msg3" && m.Execution.FailedIDs[1] == "msg4", "FailedIDs = %v, want [msg3, msg4]", m.Execution.FailedIDs)
}

// SeedMessages creates messages in the DB with source_message_id matching the
// executor's msgIDs convention (msg0, msg1, ...).
func (c *TestContext) SeedMessages(gmailIDs []string) {
	c.t.Helper()
	source, err := c.Store.GetOrCreateSource("gmail", "test@example.com")
	require.NoError(c.t, err, "GetOrCreateSource")
	convID, err := c.Store.EnsureConversation(source.ID, "thread-1", "Thread")
	require.NoError(c.t, err, "EnsureConversation")
	for _, id := range gmailIDs {
		_, err := c.Store.UpsertMessage(&store.Message{
			ConversationID:  convID,
			SourceID:        source.ID,
			SourceMessageID: id,
			MessageType:     "email",
			SizeEstimate:    100,
		})
		require.NoError(c.t, err, "UpsertMessage(%s)", id)
	}
}

// CountDeleted returns the count of messages with deleted_from_source_at set.
func (c *TestContext) CountDeleted() int {
	c.t.Helper()
	var count int
	err := c.Store.DB().QueryRow(
		`SELECT COUNT(*) FROM messages WHERE deleted_from_source_at IS NOT NULL`,
	).Scan(&count)
	require.NoError(c.t, err, "CountDeleted")
	return count
}

// TestExecutor_ExecuteBatch_MarksDBRows verifies that successful batch
// deletion actually marks messages as deleted in the database.
func TestExecutor_ExecuteBatch_MarksDBRows(t *testing.T) {
	tc := NewTestContext(t)

	ids := msgIDs(5)
	tc.SeedMessages(ids)
	manifest := tc.CreateManifest("db-mark-test", ids)

	require.NoError(t, tc.ExecuteBatch(manifest.ID), "ExecuteBatch")

	assert.Equal(t, 5, tc.CountDeleted(), "deleted count")
}

// TestExecutor_ExecuteBatch_CancelDuringRetry verifies that cancellation
// during the retry-failed-IDs loop checkpoints unattempted retry IDs.
func TestExecutor_ExecuteBatch_CancelDuringRetry(t *testing.T) {
	require := require.New(t)
	tc := NewTestContext(t)

	ids := msgIDs(5)
	manifest := NewManifest("cancel retry", ids)
	manifest.Status = StatusInProgress
	manifest.Execution = &Execution{
		StartedAt:          time.Now().Add(-time.Hour),
		Method:             MethodDelete,
		Succeeded:          0,
		Failed:             5,
		FailedIDs:          ids,
		LastProcessedIndex: 5,
	}
	require.NoError(tc.Mgr.SaveManifest(manifest), "SaveManifest")

	execCtx, cancel := context.WithCancel(context.Background())
	callCount := 0
	tc.MockAPI.BeforeDelete = func(id string) error {
		callCount++
		if callCount >= 2 {
			cancel()
		}
		return nil
	}

	err := tc.Exec.ExecuteBatch(execCtx, manifest.ID)
	require.ErrorIs(err, context.Canceled, "ExecuteBatch error")

	// Load checkpoint
	m, _, err := tc.Mgr.GetManifest(manifest.ID)
	require.NoError(err, "GetManifest")

	// The unattempted retry IDs should be preserved in the checkpoint
	assert.GreaterOrEqual(t, len(m.Execution.FailedIDs), 3, "FailedIDs (unattempted retry IDs preserved)")
}

// TestExecutor_ExecuteBatch_CancelDuringFallback verifies that cancellation
// during the fallback individual-delete loop checkpoints correctly.
func TestExecutor_ExecuteBatch_CancelDuringFallback(t *testing.T) {
	tc := NewTestContext(t)

	ids := msgIDs(5)
	manifest := tc.CreateManifest("cancel fallback", ids)

	// Force batch delete to fail so it falls back to individual
	tc.SimulateBatchDeleteError()

	execCtx, cancel := context.WithCancel(context.Background())
	callCount := 0
	tc.MockAPI.BeforeDelete = func(id string) error {
		callCount++
		if callCount >= 3 {
			cancel()
		}
		return nil
	}

	err := tc.Exec.ExecuteBatch(execCtx, manifest.ID)
	require.ErrorIs(t, err, context.Canceled, "ExecuteBatch error")

	// Should have processed some messages before cancellation
	m, _, err := tc.Mgr.GetManifest(manifest.ID)
	require.NoError(t, err, "GetManifest")
	assert.GreaterOrEqual(t, m.Execution.Succeeded, 2, "Succeeded (processed before cancel)")
}

// TestExecutor_OnStartAlreadyProcessed verifies that OnStart receives the
// correct alreadyProcessed value when resuming.
func TestExecutor_OnStartAlreadyProcessed(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	tc := NewTestContext(t)

	ids := msgIDs(10)
	manifest := NewManifest("resume progress", ids)
	manifest.Status = StatusInProgress
	manifest.Execution = &Execution{
		StartedAt:          time.Now().Add(-time.Hour),
		Method:             MethodDelete,
		Succeeded:          5,
		Failed:             0,
		LastProcessedIndex: 5,
	}
	require.NoError(tc.Mgr.SaveManifest(manifest), "SaveManifest")

	require.NoError(tc.ExecuteBatch(manifest.ID), "ExecuteBatch")

	assert.Equal(5, tc.Progress.startProcessed, "OnStart alreadyProcessed")
	assert.Equal(10, tc.Progress.startTotal, "OnStart total")
}

// TestExecutor_OnStartRetryDoesNotShow100Percent verifies that when resuming
// with retry IDs and LastProcessedIndex == total, progress shows succeeded
// count (not startIndex) to avoid a misleading 100% display.
func TestExecutor_OnStartRetryDoesNotShow100Percent(t *testing.T) {
	tc := NewTestContext(t)

	ids := msgIDs(10)
	manifest := NewManifest("retry progress", ids)
	manifest.Status = StatusInProgress
	manifest.Execution = &Execution{
		StartedAt:          time.Now().Add(-time.Hour),
		Method:             MethodDelete,
		Succeeded:          7,
		Failed:             3,
		FailedIDs:          []string{"msg7", "msg8", "msg9"},
		LastProcessedIndex: 10, // all processed, but 3 failed
	}
	require.NoError(t, tc.Mgr.SaveManifest(manifest), "SaveManifest")

	require.NoError(t, tc.ExecuteBatch(manifest.ID), "ExecuteBatch")

	// Should show 7 (succeeded), not 10 (startIndex == total)
	assert.Equal(t, 7, tc.Progress.startProcessed, "OnStart alreadyProcessed (succeeded, not startIndex)")
}

// TestExecutor_Execute_CooperativeCancel verifies that a cross-process cancel
// (the daemon moving the manifest out of in_progress/ while the CLI executor
// runs) stops the executor promptly: it does not delete every message, returns
// ErrManifestCancelled, and leaves the manifest cancelled rather than resurrected
// or completed.
func TestExecutor_Execute_CooperativeCancel(t *testing.T) {
	tc := NewTestContext(t)
	manifest := tc.CreateManifest("coop cancel", msgIDs(10))

	callCount := 0
	tc.MockAPI.BeforeTrash = func(string) error {
		callCount++
		if callCount == 1 {
			// Simulate the daemon cancelling the in-progress batch mid-run.
			require.NoError(t, tc.Mgr.CancelManifest(manifest.ID), "daemon CancelManifest")
		}
		return nil
	}

	err := tc.Execute(manifest.ID)
	require.ErrorIs(t, err, ErrManifestCancelled, "Execute() error")

	assert.Less(t, len(tc.MockAPI.TrashCalls), 10, "stopped before deleting all messages")
	tc.AssertNotCompleted()
	tc.AssertInProgressCount(0)
	tc.AssertCompletedCount(0)
	tc.AssertFailedCount(0)
	tc.AssertCancelledCount(1)
}

// TestExecutor_Execute_NoResurrectionOnFinalize cancels the manifest on the
// final item so the per-item loop check does not fire; the move-first finalize
// must still refuse to recreate the in_progress file or mark it completed.
func TestExecutor_Execute_NoResurrectionOnFinalize(t *testing.T) {
	tc := NewTestContext(t)
	manifest := tc.CreateManifest("finalize cancel", msgIDs(1))

	tc.MockAPI.BeforeTrash = func(string) error {
		// Cancel during the only delete, after the loop's pre-delete check.
		require.NoError(t, tc.Mgr.CancelManifest(manifest.ID), "daemon CancelManifest")
		return nil
	}

	err := tc.Execute(manifest.ID)
	require.ErrorIs(t, err, ErrManifestCancelled, "Execute() error")

	tc.AssertNotCompleted()
	tc.AssertInProgressCount(0)
	tc.AssertCompletedCount(0)
	tc.AssertCancelledCount(1)
}

// TestExecutor_Execute_CancelledBeforeClaim verifies that a manifest cancelled
// while still pending cannot be executed: prepareExecution reports it as
// ErrManifestCancelled and nothing is deleted or completed.
func TestExecutor_Execute_CancelledBeforeClaim(t *testing.T) {
	tc := NewTestContext(t)
	manifest := tc.CreateManifest("cancel before claim", msgIDs(3))

	require.NoError(t, tc.Mgr.CancelManifest(manifest.ID), "CancelManifest")

	err := tc.Execute(manifest.ID)
	require.ErrorIs(t, err, ErrManifestCancelled, "Execute() error")

	tc.AssertTrashCalls(0)
	tc.AssertNotCompleted()
	tc.AssertCompletedCount(0)
	tc.AssertInProgressCount(0)
	tc.AssertCancelledCount(1)
}

// TestExecutor_ExecuteBatch_CooperativeCancel verifies the batch path honors a
// cross-process cancel between batches without deleting every batch or
// resurrecting the manifest.
func TestExecutor_ExecuteBatch_CooperativeCancel(t *testing.T) {
	tc := NewTestContext(t)
	manifest := tc.CreateManifest("coop cancel batch", msgIDs(2500))

	batchCount := 0
	tc.MockAPI.BeforeBatchDelete = func([]string) error {
		batchCount++
		if batchCount == 1 {
			require.NoError(t, tc.Mgr.CancelManifest(manifest.ID), "daemon CancelManifest")
		}
		return nil
	}

	err := tc.ExecuteBatch(manifest.ID)
	require.ErrorIs(t, err, ErrManifestCancelled, "ExecuteBatch() error")

	assert.Less(t, len(tc.MockAPI.BatchDeleteCalls), 3, "stopped before processing all batches")
	tc.AssertNotCompleted()
	tc.AssertInProgressCount(0)
	tc.AssertCompletedCount(0)
	tc.AssertCancelledCount(1)
}

// TestExecutor_SaveCheckpoint_DoesNotResurrectCancelledManifest drives the
// exact checkpoint-write race: a manifest is claimed into in_progress/, then a
// daemon cancel moves it to cancelled/ in the window where the executor's
// cancellation stat would previously have already passed. The subsequent
// checkpoint write must NOT recreate in_progress/<id>.json; the manifest must
// remain only in cancelled/.
func TestExecutor_SaveCheckpoint_DoesNotResurrectCancelledManifest(t *testing.T) {
	tc := NewTestContext(t)
	manifest := tc.CreateManifest("checkpoint race", msgIDs(10))

	// Claim into in_progress/ as prepareExecution would.
	require.NoError(t, tc.Mgr.MoveManifest(manifest.ID, StatusPending, StatusInProgress), "MoveManifest")
	manifest.Status = StatusInProgress
	manifest.Execution = &Execution{StartedAt: time.Now(), Method: MethodTrash}
	path := tc.Mgr.InProgressDir() + "/" + manifest.ID + ".json"

	// The daemon cancels right before the checkpoint write lands.
	require.NoError(t, tc.Mgr.CancelManifest(manifest.ID), "daemon CancelManifest")

	// Invoke the checkpoint write directly at the race boundary.
	tc.Exec.saveCheckpoint(manifest, manifest.ID, 5, 5, 0, nil)

	// The in_progress file must not be resurrected; the manifest lives only
	// in cancelled/.
	_, statErr := os.Stat(path)
	assert.True(t, os.IsNotExist(statErr), "in_progress file must not be recreated by checkpoint")
	tc.AssertInProgressCount(0)
	tc.AssertCompletedCount(0)
	tc.AssertCancelledCount(1)
}

// TestExecutor_Finalize_RefusesWhenDurablyCancelled verifies that finalize
// refuses to complete a manifest whose cancelled/ marker is present, even if a
// stray in_progress/<id>.json exists (simulating a resurrected file). This
// guards against a double copy in cancelled/ and completed/.
func TestExecutor_Finalize_RefusesWhenDurablyCancelled(t *testing.T) {
	require := require.New(t)
	tc := NewTestContext(t)
	manifest := tc.CreateManifest("finalize durable cancel", msgIDs(3))

	require.NoError(tc.Mgr.MoveManifest(manifest.ID, StatusPending, StatusInProgress), "MoveManifest")
	manifest.Status = StatusInProgress
	manifest.Execution = &Execution{StartedAt: time.Now(), Method: MethodTrash}

	// Daemon cancels (durable marker in cancelled/).
	require.NoError(tc.Mgr.CancelManifest(manifest.ID), "daemon CancelManifest")

	// Simulate a resurrected in_progress file so MoveManifest alone would
	// otherwise succeed and create a completed/ copy.
	inProgressPath := tc.Mgr.InProgressDir() + "/" + manifest.ID + ".json"
	require.NoError(manifest.Save(inProgressPath), "resurrect in_progress file")

	err := tc.Exec.finalizeExecution(manifest.ID, manifest, 3, 0, nil, true)
	require.ErrorIs(err, ErrManifestCancelled, "finalizeExecution error")

	tc.AssertCompletedCount(0)
	tc.AssertCancelledCount(1)
}

// TestExecutor_PrepareExecution_PersistsInProgressState verifies that
// claiming a pending manifest through the executor's prepareExecution (backed
// by Manager.ClaimManifest) writes Status=InProgress and a non-nil Execution
// to in_progress/<id>.json on disk immediately — not just to the returned
// in-memory manifest. A crash before the first checkpoint must never leave an
// in_progress file that still says "pending".
func TestExecutor_PrepareExecution_PersistsInProgressState(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	tc := NewTestContext(t)
	manifest := tc.CreateManifest("prepare persists", msgIDs(3))

	_, err := tc.Exec.prepareExecution(manifest.ID, MethodTrash)
	require.NoError(err, "prepareExecution")

	inProgressPath := tc.Mgr.InProgressDir() + "/" + manifest.ID + ".json"
	loaded, err := LoadManifest(inProgressPath)
	require.NoError(err, "reload in_progress manifest from disk")
	assert.Equal(StatusInProgress, loaded.Status, "on-disk status must not still say pending")
	require.NotNil(loaded.Execution, "on-disk Execution must be initialized")
}

// TestNullProgress_AllMethods exercises all NullProgress methods for coverage.
func TestNullProgress_AllMethods(t *testing.T) {
	p := NullProgress{}
	// These are no-ops but we need to call them for coverage
	p.OnStart(100, 0)
	p.OnProgress(50, 40, 10)
	p.OnComplete(90, 10)
	// If we get here without panic, the test passes
}
