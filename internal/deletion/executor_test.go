package deletion

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"go.kenn.io/msgvault/internal/gmail"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

// trackingProgress records progress events for testing
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
	if err != nil {
		t.Fatalf("NewManager() error = %v", err)
	}

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
	if err != nil {
		c.t.Fatalf("CreateManifest(%q) error = %v", name, err)
	}
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
	if c.Progress.finalSucc != wantSucc {
		c.t.Errorf("finalSucc = %d, want %d", c.Progress.finalSucc, wantSucc)
	}
	if c.Progress.finalFail != wantFail {
		c.t.Errorf("finalFail = %d, want %d", c.Progress.finalFail, wantFail)
	}
}

// AssertCompleted verifies that OnComplete was called.
func (c *TestContext) AssertCompleted() {
	c.t.Helper()
	if !c.Progress.completed {
		c.t.Error("OnComplete was not called")
	}
}

// AssertNotCompleted verifies that OnComplete was not called.
func (c *TestContext) AssertNotCompleted() {
	c.t.Helper()
	if c.Progress.completed {
		c.t.Error("OnComplete was called unexpectedly")
	}
}

// AssertTrashCalls verifies the number of TrashMessage calls.
func (c *TestContext) AssertTrashCalls(want int) {
	c.t.Helper()
	if len(c.MockAPI.TrashCalls) != want {
		c.t.Errorf("TrashCalls = %d, want %d", len(c.MockAPI.TrashCalls), want)
	}
}

// AssertDeleteCalls verifies the number of DeleteMessage calls.
func (c *TestContext) AssertDeleteCalls(want int) {
	c.t.Helper()
	if len(c.MockAPI.DeleteCalls) != want {
		c.t.Errorf("DeleteCalls = %d, want %d", len(c.MockAPI.DeleteCalls), want)
	}
}

// AssertCompletedCount verifies the number of completed manifests.
func (c *TestContext) AssertCompletedCount(want int) {
	c.t.Helper()
	completed, err := c.Mgr.ListCompleted()
	if err != nil {
		c.t.Fatalf("ListCompleted() error = %v", err)
	}
	if len(completed) != want {
		c.t.Errorf("ListCompleted() = %d, want %d", len(completed), want)
	}
}

// AssertFailedCount verifies the number of failed manifests.
func (c *TestContext) AssertFailedCount(want int) {
	c.t.Helper()
	failed, err := c.Mgr.ListFailed()
	if err != nil {
		c.t.Fatalf("ListFailed() error = %v", err)
	}
	if len(failed) != want {
		c.t.Errorf("ListFailed() = %d, want %d", len(failed), want)
	}
}

// AssertManifestExecution verifies the persisted execution state of a manifest.
func (c *TestContext) AssertManifestExecution(id string, wantSucc, wantFail int, wantFailedIDs ...string) {
	c.t.Helper()
	m, _, err := c.Mgr.GetManifest(id)
	if err != nil {
		c.t.Fatalf("GetManifest(%q) failed: %v", id, err)
	}
	if m.Execution.Succeeded != wantSucc {
		c.t.Errorf("Persisted Succeeded = %d, want %d", m.Execution.Succeeded, wantSucc)
	}
	if m.Execution.Failed != wantFail {
		c.t.Errorf("Persisted Failed = %d, want %d", m.Execution.Failed, wantFail)
	}
	if len(m.Execution.FailedIDs) != len(wantFailedIDs) {
		c.t.Errorf("FailedIDs count = %d, want %d", len(m.Execution.FailedIDs), len(wantFailedIDs))
	} else {
		for i, id := range wantFailedIDs {
			if m.Execution.FailedIDs[i] != id {
				c.t.Errorf("FailedIDs[%d] = %q, want %q", i, m.Execution.FailedIDs[i], id)
			}
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
	if len(c.MockAPI.BatchDeleteCalls) != want {
		c.t.Errorf("BatchDeleteCalls = %d, want %d", len(c.MockAPI.BatchDeleteCalls), want)
	}
}

// GetBatchDeleteCall safely retrieves a batch delete call by index.
func (c *TestContext) GetBatchDeleteCall(index int) []string {
	c.t.Helper()
	if index >= len(c.MockAPI.BatchDeleteCalls) {
		c.t.Fatalf("BatchDeleteCalls index %d out of range (len=%d)", index, len(c.MockAPI.BatchDeleteCalls))
		return nil
	}
	return c.MockAPI.BatchDeleteCalls[index]
}

// AssertIsScopeError verifies that the error is an insufficient scope error.
func (c *TestContext) AssertIsScopeError(err error) {
	c.t.Helper()
	if err == nil || !strings.Contains(err.Error(), "ACCESS_TOKEN_SCOPE_INSUFFICIENT") {
		c.t.Errorf("error = %v, want scope insufficient error", err)
	}
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
	scopeErr := fmt.Errorf("googleapi: Error 403: Insufficient Permission: ACCESS_TOKEN_SCOPE_INSUFFICIENT")
	c.MockAPI.TrashErrors[msgID] = scopeErr
	c.MockAPI.DeleteErrors[msgID] = scopeErr
}

// SimulateBatchScopeError sets the batch delete operation to fail with a scope error.
func (c *TestContext) SimulateBatchScopeError() {
	c.MockAPI.BatchDeleteError = fmt.Errorf("googleapi: Error 403: Insufficient Permission: ACCESS_TOKEN_SCOPE_INSUFFICIENT")
}

// AssertInProgressCount verifies the number of in-progress manifests.
func (c *TestContext) AssertInProgressCount(want int) {
	c.t.Helper()
	inProgress, err := c.Mgr.ListInProgress()
	if err != nil {
		c.t.Fatalf("ListInProgress() error = %v", err)
	}
	if len(inProgress) != want {
		c.t.Errorf("ListInProgress() = %d, want %d", len(inProgress), want)
	}
}

// AssertManifestLastProcessedIndex verifies the persisted LastProcessedIndex.
func (c *TestContext) AssertManifestLastProcessedIndex(id string, want int) {
	c.t.Helper()
	m, _, err := c.Mgr.GetManifest(id)
	if err != nil {
		c.t.Fatalf("GetManifest(%q) failed: %v", id, err)
	}
	if m.Execution == nil {
		c.t.Fatalf("manifest %q has nil Execution", id)
	}
	if m.Execution.LastProcessedIndex != want {
		c.t.Errorf("LastProcessedIndex = %d, want %d", m.Execution.LastProcessedIndex, want)
	}
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

	if opts.Method != MethodTrash {
		t.Errorf("Method = %q, want %q", opts.Method, MethodTrash)
	}
	if opts.BatchSize != 100 {
		t.Errorf("BatchSize = %d, want 100", opts.BatchSize)
	}
	if !opts.Resume {
		t.Error("Resume = false, want true")
	}
}

func TestNewExecutor(t *testing.T) {
	tmpDir := t.TempDir()
	mgr, err := NewManager(tmpDir)
	if err != nil {
		t.Fatalf("NewManager() error = %v", err)
	}

	store := testutil.NewTestStore(t)
	mockAPI := gmail.NewDeletionMockAPI()

	exec := NewExecutor(mgr, store, mockAPI)
	if exec == nil {
		t.Fatal("NewExecutor returned nil")
	}
}

func TestExecutor_WithLogger(t *testing.T) {
	tmpDir := t.TempDir()
	mgr, err := NewManager(tmpDir)
	if err != nil {
		t.Fatalf("NewManager() error = %v", err)
	}
	store := testutil.NewTestStore(t)

	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	exec := NewExecutor(mgr, store, gmail.NewDeletionMockAPI()).WithLogger(logger)

	if exec.logger != logger {
		t.Error("WithLogger did not set logger")
	}
}

func TestExecutor_WithProgress(t *testing.T) {
	tmpDir := t.TempDir()
	mgr, err := NewManager(tmpDir)
	if err != nil {
		t.Fatalf("NewManager() error = %v", err)
	}
	store := testutil.NewTestStore(t)

	progress := &trackingProgress{}
	exec := NewExecutor(mgr, store, gmail.NewDeletionMockAPI()).WithProgress(progress)

	if exec.progress != progress {
		t.Error("WithProgress did not set progress")
	}
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
				ctx.AssertFailedCount(1)
			},
		},
		{
			name:     "SmallBatchSize",
			ids:      msgIDs(5),
			opts:     trashOpts(2),
			wantSucc: 5, wantFail: 0,
			assertions: func(t *testing.T, ctx *TestContext, m *Manifest) {
				ctx.AssertTrashCalls(5)
			},
		},
		{
			name:     "NotFoundTreatedAsSuccess",
			ids:      msgIDs(3),
			setup:    func(c *TestContext) { c.SimulateNotFound("msg1") },
			wantSucc: 3, wantFail: 0,
			assertions: func(t *testing.T, ctx *TestContext, m *Manifest) {
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
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				if tt.scopeError {
					ctx.AssertIsScopeError(err)
				}
			} else {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
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
	if err != context.Canceled {
		t.Errorf("Execute() error = %v, want context.Canceled", err)
	}

	ctx.AssertNotCompleted()

	// Manifest should remain in in_progress (for resume)
	ctx.AssertInProgressCount(1)
}

func TestExecutor_Execute_ManifestNotFound(t *testing.T) {
	ctx := NewTestContext(t)

	err := ctx.Execute("nonexistent-id")
	if err == nil {
		t.Error("Execute() should error for nonexistent manifest")
	}
}

func TestExecutor_Execute_InvalidStatus(t *testing.T) {
	ctx := NewTestContext(t)
	manifest := ctx.CreateManifest("completed test", msgIDs(1))

	// Execute to completion
	if err := ctx.Execute(manifest.ID); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	// Try to execute again
	err := ctx.Execute(manifest.ID)
	if err == nil {
		t.Error("Execute() should error for completed manifest")
	}
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
	if err := tc.Mgr.SaveManifest(manifest); err != nil {
		t.Fatalf("SaveManifest() error = %v", err)
	}

	if err := tc.ExecuteWithOpts(manifest.ID, trashOpts(100)); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	// Should only process msg3, msg4, msg5 (skipping msg1, msg2)
	tc.AssertTrashCalls(3)

	// Verify final counts include all 5
	tc.AssertManifestExecution(manifest.ID, 5, 0)
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
				ctx.AssertBatchDeleteCalls(1)
				if len(ctx.GetBatchDeleteCall(0)) != 3 {
					t.Errorf("BatchDeleteCalls[0] length = %d, want 3", len(ctx.GetBatchDeleteCall(0)))
				}
				ctx.AssertCompleted()
				ctx.AssertCompletedCount(1)
			},
		},
		{
			name:     "LargeBatch",
			ids:      msgIDs(1500),
			wantSucc: 1500, wantFail: 0,
			assertions: func(t *testing.T, ctx *TestContext, m *Manifest) {
				ctx.AssertBatchDeleteCalls(2)
				if len(ctx.GetBatchDeleteCall(0)) != 1000 {
					t.Errorf("BatchDeleteCalls[0] length = %d, want 1000", len(ctx.GetBatchDeleteCall(0)))
				}
				if len(ctx.GetBatchDeleteCall(1)) != 500 {
					t.Errorf("BatchDeleteCalls[1] length = %d, want 500", len(ctx.GetBatchDeleteCall(1)))
				}
			},
		},
		{
			name:     "WithBatchError",
			ids:      msgIDs(3),
			setup:    func(c *TestContext) { c.SimulateBatchDeleteError() },
			wantSucc: 3, wantFail: 0,
			assertions: func(t *testing.T, ctx *TestContext, m *Manifest) {
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
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				if tt.scopeError {
					ctx.AssertIsScopeError(err)
				}
			} else {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				ctx.AssertResult(tt.wantSucc, tt.wantFail)
			}

			if tt.assertions != nil {
				tt.assertions(t, ctx, manifest)
			}
		})
	}
}

func TestExecutor_ExecuteBatch_InvalidStatus(t *testing.T) {
	ctx := NewTestContext(t)
	manifest := ctx.CreateManifest("wrong status", msgIDs(1))

	// Move to in_progress
	if err := ctx.Mgr.MoveManifest(manifest.ID, StatusPending, StatusInProgress); err != nil {
		t.Fatalf("MoveManifest() error = %v", err)
	}

	err := ctx.ExecuteBatch(manifest.ID)
	if err == nil {
		t.Error("ExecuteBatch() should error for non-pending manifest")
	}
}

func TestExecutor_ExecuteBatch_ContextCancelled(t *testing.T) {
	ctx := NewTestContext(t)

	manifest := ctx.CreateManifest("cancel batch", msgIDs(2500))

	// Cancel context immediately
	execCtx, cancel := context.WithCancel(context.Background())
	cancel()

	err := ctx.Exec.ExecuteBatch(execCtx, manifest.ID)
	if err != context.Canceled {
		t.Errorf("ExecuteBatch() error = %v, want context.Canceled", err)
	}

	ctx.AssertNotCompleted()
}

func TestExecutor_ExecuteBatch_ManifestNotFound(t *testing.T) {
	ctx := NewTestContext(t)

	err := ctx.ExecuteBatch("nonexistent-id")
	if err == nil {
		t.Error("ExecuteBatch() should error for nonexistent manifest")
	}
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
	if err := tc.Mgr.SaveManifest(manifest); err != nil {
		t.Fatalf("SaveManifest() error = %v", err)
	}

	if err := tc.ExecuteBatch(manifest.ID); err != nil {
		t.Fatalf("ExecuteBatch() error = %v", err)
	}

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
	if err := tc.Mgr.SaveManifest(manifest); err != nil {
		t.Fatalf("SaveManifest() error = %v", err)
	}

	if err := tc.ExecuteBatch(manifest.ID); err != nil {
		t.Fatalf("ExecuteBatch() error = %v", err)
	}

	// msg2, msg4 succeed on retry; msg3 still fails
	tc.AssertResult(4, 1)
	tc.AssertCompletedCount(1)
}

// TestExecutor_ExecuteBatch_RetryScopeErrorAfterPartialSuccess verifies that
// a scope error during retry only preserves unattempted+failed IDs, not
// already-succeeded ones.
func TestExecutor_ExecuteBatch_RetryScopeErrorAfterPartialSuccess(t *testing.T) {
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
	if err := tc.Mgr.SaveManifest(manifest); err != nil {
		t.Fatalf("SaveManifest() error = %v", err)
	}

	err := tc.ExecuteBatch(manifest.ID)
	if err == nil {
		t.Fatal("ExecuteBatch() should return error for scope error during retry")
	}
	tc.AssertIsScopeError(err)

	// msg2 succeeded before the scope error on msg3
	// Checkpoint should have: FailedIDs = [msg3, msg4] (current + unattempted)
	m, _, err := tc.Mgr.GetManifest(manifest.ID)
	if err != nil {
		t.Fatalf("GetManifest() error = %v", err)
	}
	if m.Execution.Succeeded != 3 { // original 2 + msg2 retry
		t.Errorf("Succeeded = %d, want 3", m.Execution.Succeeded)
	}
	if m.Execution.Failed != 2 { // msg3 + msg4
		t.Errorf("Failed = %d, want 2", m.Execution.Failed)
	}
	if len(m.Execution.FailedIDs) != 2 {
		t.Fatalf("FailedIDs count = %d, want 2", len(m.Execution.FailedIDs))
	}
	if m.Execution.FailedIDs[0] != "msg3" || m.Execution.FailedIDs[1] != "msg4" {
		t.Errorf("FailedIDs = %v, want [msg3, msg4]", m.Execution.FailedIDs)
	}
}

// SeedMessages creates messages in the DB with source_message_id matching the
// executor's msgIDs convention (msg0, msg1, ...).
func (c *TestContext) SeedMessages(gmailIDs []string) {
	c.t.Helper()
	source, err := c.Store.GetOrCreateSource("gmail", "test@example.com")
	if err != nil {
		c.t.Fatalf("GetOrCreateSource: %v", err)
	}
	convID, err := c.Store.EnsureConversation(source.ID, "thread-1", "Thread")
	if err != nil {
		c.t.Fatalf("EnsureConversation: %v", err)
	}
	for _, id := range gmailIDs {
		if _, err := c.Store.UpsertMessage(&store.Message{
			ConversationID:  convID,
			SourceID:        source.ID,
			SourceMessageID: id,
			MessageType:     "email",
			SizeEstimate:    100,
		}); err != nil {
			c.t.Fatalf("UpsertMessage(%s): %v", id, err)
		}
	}
}

// CountDeleted returns the count of messages with deleted_from_source_at set.
func (c *TestContext) CountDeleted() int {
	c.t.Helper()
	var count int
	err := c.Store.DB().QueryRow(
		`SELECT COUNT(*) FROM messages WHERE deleted_from_source_at IS NOT NULL`,
	).Scan(&count)
	if err != nil {
		c.t.Fatalf("CountDeleted: %v", err)
	}
	return count
}

// TestExecutor_ExecuteBatch_MarksDBRows verifies that successful batch
// deletion actually marks messages as deleted in the database.
func TestExecutor_ExecuteBatch_MarksDBRows(t *testing.T) {
	tc := NewTestContext(t)

	ids := msgIDs(5)
	tc.SeedMessages(ids)
	manifest := tc.CreateManifest("db-mark-test", ids)

	if err := tc.ExecuteBatch(manifest.ID); err != nil {
		t.Fatalf("ExecuteBatch: %v", err)
	}

	if got := tc.CountDeleted(); got != 5 {
		t.Errorf("deleted count = %d, want 5", got)
	}
}

// TestExecutor_ExecuteBatch_CancelDuringRetry verifies that cancellation
// during the retry-failed-IDs loop checkpoints unattempted retry IDs.
func TestExecutor_ExecuteBatch_CancelDuringRetry(t *testing.T) {
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
	if err := tc.Mgr.SaveManifest(manifest); err != nil {
		t.Fatalf("SaveManifest: %v", err)
	}

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
	if err != context.Canceled {
		t.Fatalf("ExecuteBatch error = %v, want context.Canceled", err)
	}

	// Load checkpoint
	m, _, err := tc.Mgr.GetManifest(manifest.ID)
	if err != nil {
		t.Fatalf("GetManifest: %v", err)
	}

	// The unattempted retry IDs should be preserved in the checkpoint
	if len(m.Execution.FailedIDs) < 3 {
		t.Errorf("FailedIDs = %d, want >= 3 (unattempted retry IDs preserved)", len(m.Execution.FailedIDs))
	}
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
	if err != context.Canceled {
		t.Fatalf("ExecuteBatch error = %v, want context.Canceled", err)
	}

	// Should have processed some messages before cancellation
	m, _, err := tc.Mgr.GetManifest(manifest.ID)
	if err != nil {
		t.Fatalf("GetManifest: %v", err)
	}
	if m.Execution.Succeeded < 2 {
		t.Errorf("Succeeded = %d, want >= 2 (processed before cancel)", m.Execution.Succeeded)
	}
}

// TestExecutor_OnStartAlreadyProcessed verifies that OnStart receives the
// correct alreadyProcessed value when resuming.
func TestExecutor_OnStartAlreadyProcessed(t *testing.T) {
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
	if err := tc.Mgr.SaveManifest(manifest); err != nil {
		t.Fatalf("SaveManifest: %v", err)
	}

	if err := tc.ExecuteBatch(manifest.ID); err != nil {
		t.Fatalf("ExecuteBatch: %v", err)
	}

	if tc.Progress.startProcessed != 5 {
		t.Errorf("OnStart alreadyProcessed = %d, want 5", tc.Progress.startProcessed)
	}
	if tc.Progress.startTotal != 10 {
		t.Errorf("OnStart total = %d, want 10", tc.Progress.startTotal)
	}
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
	if err := tc.Mgr.SaveManifest(manifest); err != nil {
		t.Fatalf("SaveManifest: %v", err)
	}

	if err := tc.ExecuteBatch(manifest.ID); err != nil {
		t.Fatalf("ExecuteBatch: %v", err)
	}

	// Should show 7 (succeeded), not 10 (startIndex == total)
	if tc.Progress.startProcessed != 7 {
		t.Errorf("OnStart alreadyProcessed = %d, want 7 (succeeded, not startIndex)", tc.Progress.startProcessed)
	}
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
