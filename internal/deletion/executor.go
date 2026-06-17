package deletion

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/gmail"
	"go.kenn.io/msgvault/internal/store"
)

// isNotFoundError checks if an error indicates the message was already deleted.
// Treating 404 as success makes deletion idempotent.
func isNotFoundError(err error) bool {
	var notFound *gmail.NotFoundError
	return errors.As(err, &notFound)
}

// isInsufficientScopeError checks if an error is due to missing OAuth scopes.
func isInsufficientScopeError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "ACCESS_TOKEN_SCOPE_INSUFFICIENT") ||
		strings.Contains(msg, "insufficient authentication scopes") ||
		strings.Contains(msg, "Insufficient Permission")
}

// Progress reports deletion progress.
type Progress interface {
	OnStart(total, alreadyProcessed int)
	OnProgress(processed, succeeded, failed int)
	OnComplete(succeeded, failed int)
}

// NullProgress is a no-op progress reporter.
type NullProgress struct{}

func (NullProgress) OnStart(total, alreadyProcessed int)         {}
func (NullProgress) OnProgress(processed, succeeded, failed int) {}
func (NullProgress) OnComplete(succeeded, failed int)            {}

// Executor performs deletion operations.
type Executor struct {
	manager  *Manager
	store    *store.Store
	client   gmail.API
	logger   *slog.Logger
	progress Progress
}

// NewExecutor creates a deletion executor.
func NewExecutor(manager *Manager, store *store.Store, client gmail.API) *Executor {
	return &Executor{
		manager:  manager,
		store:    store,
		client:   client,
		logger:   slog.Default(),
		progress: NullProgress{},
	}
}

// WithLogger sets the logger.
func (e *Executor) WithLogger(logger *slog.Logger) *Executor {
	e.logger = logger
	return e
}

// WithProgress sets the progress reporter.
func (e *Executor) WithProgress(p Progress) *Executor {
	e.progress = p
	return e
}

// ExecuteOptions configures deletion execution.
type ExecuteOptions struct {
	Method    Method // Trash or permanent delete
	BatchSize int    // Messages per batch for batch delete API
	Resume    bool   // Resume from last checkpoint
}

// DefaultExecuteOptions returns sensible defaults.
func DefaultExecuteOptions() *ExecuteOptions {
	return &ExecuteOptions{
		Method:    MethodTrash,
		BatchSize: 100, // Gmail batch delete supports up to 1000
		Resume:    true,
	}
}

// deleteResult classifies the outcome of a single message deletion attempt.
type deleteResult int

const (
	resultSuccess deleteResult = iota
	resultFailed
	resultFatal
)

// deleteOne attempts to delete a single message and updates the local database on success.
// Returns resultSuccess (including 404/already-deleted), resultFailed for transient errors,
// or resultFatal for scope errors that should halt execution.
func (e *Executor) deleteOne(ctx context.Context, gmailID string, method Method) (deleteResult, error) {
	var err error
	if method == MethodTrash {
		err = e.client.TrashMessage(ctx, gmailID)
	} else {
		err = e.client.DeleteMessage(ctx, gmailID)
	}

	if err == nil || isNotFoundError(err) {
		if err != nil {
			e.logger.Debug("message already deleted", "gmail_id", gmailID)
		}
		if markErr := e.store.MarkMessageDeletedByGmailID(method == MethodDelete, gmailID); markErr != nil {
			e.logger.Warn("failed to mark deleted in DB", "gmail_id", gmailID, "error", markErr)
		}
		return resultSuccess, nil
	}

	if isInsufficientScopeError(err) {
		return resultFatal, err
	}

	e.logger.Warn("failed to delete message", "gmail_id", gmailID, "error", err)
	return resultFailed, err
}

// saveCheckpoint persists the current execution progress to disk.
func (e *Executor) saveCheckpoint(manifest *Manifest, path string, index, succeeded, failed int, failedIDs []string) {
	manifest.Execution.LastProcessedIndex = index
	manifest.Execution.Succeeded = succeeded
	manifest.Execution.Failed = failed
	manifest.Execution.FailedIDs = failedIDs
	if err := manifest.Save(path); err != nil {
		e.logger.Error("failed to save checkpoint", "error", err)
	}
}

// prepareExecution loads a manifest, validates its status, transitions it to
// InProgress if pending, and returns the manifest with its file path.
func (e *Executor) prepareExecution(manifestID string, method Method) (*Manifest, string, error) {
	manifest, _, err := e.manager.GetManifest(manifestID)
	if err != nil {
		return nil, "", fmt.Errorf("load manifest: %w", err)
	}

	if manifest.Status != StatusPending && manifest.Status != StatusInProgress {
		return nil, "", fmt.Errorf("manifest %s is %s, cannot execute", manifestID, manifest.Status)
	}

	if manifest.Status == StatusPending {
		if err := e.manager.MoveManifest(manifestID, StatusPending, StatusInProgress); err != nil {
			return nil, "", fmt.Errorf("move to in_progress: %w", err)
		}
		manifest.Status = StatusInProgress
		manifest.Execution = &Execution{
			StartedAt: time.Now(),
			Method:    method,
		}
	} else if manifest.Execution == nil {
		manifest.Execution = &Execution{
			StartedAt: time.Now(),
			Method:    method,
		}
	}

	path := e.manager.InProgressDir() + "/" + manifestID + ".json"
	return manifest, path, nil
}

// finalizeExecution marks the manifest as completed or failed and moves it.
// When failOnAllErrors is true, the manifest is marked as Failed if all deletions
// failed (succeeded == 0). When false (batch mode), it is always marked Completed
// even with failures, preserving the batch semantics where partial progress is expected.
func (e *Executor) finalizeExecution(manifestID string, manifest *Manifest, path string, succeeded, failed int, failedIDs []string, failOnAllErrors bool) {
	if manifest.Execution == nil {
		manifest.Execution = &Execution{StartedAt: time.Now(), Method: "unknown"}
	}
	now := time.Now()
	manifest.Execution.CompletedAt = &now
	manifest.Execution.LastProcessedIndex = len(manifest.GmailIDs)
	manifest.Execution.Succeeded = succeeded
	manifest.Execution.Failed = failed
	manifest.Execution.FailedIDs = failedIDs

	var targetStatus Status
	if failed == 0 || succeeded > 0 || !failOnAllErrors {
		targetStatus = StatusCompleted
	} else {
		targetStatus = StatusFailed
	}

	manifest.Status = targetStatus
	if err := manifest.Save(path); err != nil {
		e.logger.Warn("failed to save final state", "error", err)
	}

	if err := e.manager.MoveManifest(manifestID, StatusInProgress, targetStatus); err != nil {
		e.logger.Warn("failed to move manifest", "error", err)
	}

	e.progress.OnComplete(succeeded, failed)

	e.logger.Debug("deletion complete",
		"manifest", manifestID,
		"succeeded", succeeded,
		"failed", failed,
	)
}

// Execute performs the deletion for a manifest.
func (e *Executor) Execute(ctx context.Context, manifestID string, opts *ExecuteOptions) error {
	if opts == nil {
		opts = DefaultExecuteOptions()
	}

	manifest, path, err := e.prepareExecution(manifestID, opts.Method)
	if err != nil {
		return err
	}

	// Determine starting point
	startIndex := 0
	if opts.Resume && manifest.Execution != nil {
		startIndex = manifest.Execution.LastProcessedIndex
	}

	e.logger.Debug("executing deletion",
		"manifest", manifestID,
		"total", len(manifest.GmailIDs),
		"start_index", startIndex,
		"method", opts.Method,
	)

	e.progress.OnStart(len(manifest.GmailIDs), startIndex)

	// Execute deletions
	succeeded := manifest.Execution.Succeeded
	failed := manifest.Execution.Failed
	failedIDs := manifest.Execution.FailedIDs

	for i := startIndex; i < len(manifest.GmailIDs); i++ {
		select {
		case <-ctx.Done():
			e.saveCheckpoint(manifest, path, i, succeeded, failed, failedIDs)
			return ctx.Err()
		default:
		}

		result, delErr := e.deleteOne(ctx, manifest.GmailIDs[i], opts.Method)
		switch result {
		case resultSuccess:
			succeeded++
		case resultFatal:
			e.saveCheckpoint(manifest, path, i, succeeded, failed, failedIDs)
			return fmt.Errorf("delete message: %w", delErr)
		case resultFailed:
			failed++
			failedIDs = append(failedIDs, manifest.GmailIDs[i])
		}

		// Save checkpoint periodically
		if (i+1)%opts.BatchSize == 0 {
			e.saveCheckpoint(manifest, path, i+1, succeeded, failed, failedIDs)
			e.progress.OnProgress(i+1, succeeded, failed)
		}
	}

	e.finalizeExecution(manifestID, manifest, path, succeeded, failed, failedIDs, true)
	return nil
}

// ExecuteBatch performs batch deletion (more efficient but permanent).
func (e *Executor) ExecuteBatch(ctx context.Context, manifestID string) error {
	manifest, path, err := e.prepareExecution(manifestID, MethodDelete)
	if err != nil {
		return err
	}

	if err := manifest.Save(path); err != nil {
		return fmt.Errorf("save manifest: %w", err)
	}

	// Resume from checkpoint if available
	startIndex := 0
	succeeded := 0
	failed := 0
	var retryIDs []string
	if manifest.Execution != nil {
		startIndex = manifest.Execution.LastProcessedIndex
		succeeded = manifest.Execution.Succeeded
		// Retry previously failed IDs instead of carrying forward the count
		if len(manifest.Execution.FailedIDs) > 0 {
			retryIDs = manifest.Execution.FailedIDs
			failed = 0
			succeeded = manifest.Execution.Succeeded
		} else {
			failed = manifest.Execution.Failed
		}
	}

	// Bounds check to handle corrupted manifests
	if startIndex < 0 {
		startIndex = 0
	}
	if startIndex > len(manifest.GmailIDs) {
		startIndex = len(manifest.GmailIDs)
	}

	e.logger.Debug("executing batch deletion",
		"manifest", manifestID,
		"total", len(manifest.GmailIDs),
		"start_index", startIndex,
		"retry_ids", len(retryIDs),
	)

	// When retries are pending, report succeeded count (not startIndex)
	// to avoid showing 100% while retry work is still running.
	alreadyProcessed := startIndex
	if len(retryIDs) > 0 {
		alreadyProcessed = succeeded
	}
	e.progress.OnStart(len(manifest.GmailIDs), alreadyProcessed)

	var failedIDs []string

	// Retry previously failed IDs before continuing with remaining messages
	if len(retryIDs) > 0 {
		e.logger.Debug("retrying previously failed messages", "count", len(retryIDs))
		for ri, gmailID := range retryIDs {
			select {
			case <-ctx.Done():
				remaining := slices.Concat(failedIDs, retryIDs[ri:])
				e.saveCheckpoint(manifest, path, startIndex, succeeded, len(remaining), remaining)
				return ctx.Err()
			default:
			}

			result, delErr := e.deleteOne(ctx, gmailID, MethodDelete)
			switch result {
			case resultSuccess:
				succeeded++
			case resultFatal:
				remaining := slices.Concat(failedIDs, retryIDs[ri:])
				e.saveCheckpoint(manifest, path, startIndex, succeeded, len(remaining), remaining)
				return fmt.Errorf("delete message: %w", delErr)
			case resultFailed:
				failed++
				failedIDs = append(failedIDs, gmailID)
			}
		}
		e.logger.Debug("retry complete", "succeeded_now", succeeded-manifest.Execution.Succeeded, "still_failed", len(failedIDs))
	}

	// Execute in batches of 1000 (Gmail API limit)
	const batchSize = 1000

	for i := startIndex; i < len(manifest.GmailIDs); i += batchSize {
		select {
		case <-ctx.Done():
			e.saveCheckpoint(manifest, path, i, succeeded, failed, failedIDs)
			return ctx.Err()
		default:
		}

		end := min(i+batchSize, len(manifest.GmailIDs))

		batch := manifest.GmailIDs[i:end]

		e.logger.Debug("deleting batch", "start", i, "end", end, "size", len(batch))

		if err := e.client.BatchDeleteMessages(ctx, batch); err != nil {
			if isInsufficientScopeError(err) {
				e.saveCheckpoint(manifest, path, i, succeeded, failed, failedIDs)
				return fmt.Errorf("batch delete: %w", err)
			}
			e.logger.Warn("batch delete failed, falling back to individual deletes", "start_index", i, "error", err)
			// Fall back to individual deletes
			for j, gmailID := range batch {
				select {
				case <-ctx.Done():
					e.saveCheckpoint(manifest, path, i+j, succeeded, failed, failedIDs)
					return ctx.Err()
				default:
				}

				result, delErr := e.deleteOne(ctx, gmailID, MethodDelete)
				switch result {
				case resultSuccess:
					succeeded++
				case resultFatal:
					e.saveCheckpoint(manifest, path, i+j, succeeded, failed, failedIDs)
					return fmt.Errorf("delete message: %w", delErr)
				case resultFailed:
					failed++
					failedIDs = append(failedIDs, gmailID)
				}
				e.progress.OnProgress(i+j+1, succeeded, failed)
			}
		} else {
			succeeded += len(batch)
			// Mark all as deleted in DB using batch update
			if markErr := e.store.MarkMessagesDeletedByGmailIDBatch(batch); markErr != nil {
				e.logger.Warn("failed to mark batch as deleted in DB", "count", len(batch), "error", markErr)
			}
		}

		e.progress.OnProgress(end, succeeded, failed)
	}

	e.finalizeExecution(manifestID, manifest, path, succeeded, failed, failedIDs, false)
	return nil
}
