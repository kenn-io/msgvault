package deletion

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/gmail"
	"go.kenn.io/msgvault/internal/store"
)

// ErrManifestCancelled reports that a deletion manifest was cancelled
// (its file was moved out of in_progress/ by a concurrent daemon cancel)
// while the executor was running. Callers use errors.Is to distinguish a
// cooperative cancellation from context cancellation and from real failures,
// and treat it as a clean stop rather than an error to surface.
var ErrManifestCancelled = errors.New("deletion manifest cancelled")

var errLocalTombstone = errors.New("local tombstone write failed")

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
	sourceID int64
}

// WithSourceID scopes legacy version-1 manifests to the source selected by
// the caller's ambiguity-checked planning step.
func (e *Executor) WithSourceID(sourceID int64) *Executor {
	e.sourceID = sourceID
	return e
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
func (e *Executor) deleteOne(ctx context.Context, sourceID int64, gmailID string, method Method) (deleteResult, error) {
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
		if markErr := e.store.MarkMessageDeletedBySourceMessageID(sourceID, method == MethodDelete, gmailID); markErr != nil {
			e.logger.Warn("failed to mark deleted in DB", "gmail_id", gmailID, "error", markErr)
			return resultFailed, fmt.Errorf("%w: %v", errLocalTombstone, markErr)
		}
		return resultSuccess, nil
	}

	if isInsufficientScopeError(err) {
		return resultFatal, err
	}

	e.logger.Warn("failed to delete message", "gmail_id", gmailID, "error", err)
	return resultFailed, err
}

func (e *Executor) manifestSourceID(manifest *Manifest) (int64, error) {
	if err := manifest.ValidateVersion(); err != nil {
		return 0, err
	}
	if manifest.Version == 1 {
		return e.sourceID, nil
	}
	source, err := e.store.GetSourceByTypeAndIdentifier(manifest.Source.Type, manifest.Source.Identifier)
	if err != nil {
		return 0, fmt.Errorf("resolve manifest source: %w", err)
	}
	return source.ID, nil
}

// manifestCancelled reports whether the manifest was cancelled by a concurrent
// daemon cancel. The executor polls this cheaply between deletions so a
// cross-process cancel stops it promptly. Two independent signals are checked:
//
//   - the durable cancelled/<id>.json marker (Manager.ManifestCancelled). A
//     cancel leaves this in place permanently, so it stays authoritative even
//     if a stray write had recreated the in_progress file.
//   - the absence of in_progress/<id>.json (a cancel renames it away).
//
// Either condition means "stop". The durable marker is listed first so a
// resurrected in_progress file can never mask a cancellation.
func (e *Executor) manifestCancelled(manifestID string) bool {
	return e.manager.ManifestCancelled(manifestID) ||
		!e.manager.InProgressManifestExists(manifestID)
}

// saveCheckpoint persists the current execution progress through
// Manager.WriteInProgressCheckpoint, which holds the per-manifest lock and
// writes atomically. If a concurrent daemon cancel already moved
// in_progress/<id>.json to cancelled/, the write is skipped (the lock
// serializes it with the cancel rename and it returns ErrManifestCancelled)
// rather than resurrecting the cancelled deletion. finalizeExecution remains
// the authoritative crash-safe commit (it claims the file with a locked atomic
// rename before completing).
func (e *Executor) saveCheckpoint(manifest *Manifest, manifestID string, index, succeeded, failed int, failedIDs []string) {
	manifest.Execution.LastProcessedIndex = index
	manifest.Execution.Succeeded = succeeded
	manifest.Execution.Failed = failed
	manifest.Execution.FailedIDs = failedIDs
	if err := e.manager.WriteInProgressCheckpoint(manifest, manifestID); err != nil {
		if errors.Is(err, ErrManifestCancelled) {
			e.logger.Info("manifest no longer in progress; skipping checkpoint", "manifest", manifestID)
			return
		}
		e.logger.Error("failed to save checkpoint", "error", err)
	}
}

// prepareExecution claims a manifest into in_progress/ (or resumes one
// already there) via Manager.ClaimManifest, which persists the initialized
// in-progress state to disk under the per-manifest lock before returning. See
// ClaimManifest for why that lock and disk-before-return ordering matter: it
// serializes the claim against a concurrent daemon cancel and closes the
// crash window where an in_progress file could still say "pending".
func (e *Executor) prepareExecution(manifestID string, method Method) (*Manifest, error) {
	manifest, err := e.manager.ClaimManifest(manifestID, method)
	if err != nil {
		if errors.Is(err, ErrManifestCancelled) {
			return nil, fmt.Errorf("manifest %s: %w", manifestID, err)
		}
		return nil, fmt.Errorf("claim manifest: %w", err)
	}
	// A resumed manifest keeps the method it was started with (ClaimManifest
	// preserves Execution.Method); executing the remainder with a different
	// one would silently switch a recoverable trash batch to permanent
	// deletion (or the reverse). Fresh claims always match — ClaimManifest
	// initializes Execution.Method from the argument — so a mismatch is
	// always a resume through the wrong entry point or flag.
	if manifest.Execution != nil && manifest.Execution.Method != method {
		return nil, fmt.Errorf(
			"manifest %s was started with method %q and cannot be resumed with method %q; rerun with the original method",
			manifestID, manifest.Execution.Method, method)
	}
	return manifest, nil
}

// finalizeExecution marks the manifest as completed or failed and moves it.
// When failOnAllErrors is true, the manifest is marked as Failed if all deletions
// failed (succeeded == 0). When false (batch mode), it is always marked Completed
// even with failures, preserving the batch semantics where partial progress is expected.
//
// Ordering: the final state is written into in_progress/<id>.json through the
// locked atomic checkpoint writer FIRST, then that already-final file is
// claimed into the terminal directory with an atomic rename. The rename is
// the authoritative anti-resurrection guard: a concurrent daemon cancel is
// also a rename of the same source path, so exactly one of the two wins. If
// the cancel won, the checkpoint write returns ErrManifestCancelled (or the
// rename fails with ENOENT) and we stop without recreating the file or
// force-completing.
//
// Writing before the rename — rather than saving into the terminal directory
// afterwards — means the file that lands in completed/ or failed/ already
// carries its final status and counters. A crash in between leaves the
// manifest in in_progress/ holding that final state, which the
// directory-authoritative resume path re-finalizes idempotently (its
// LastProcessedIndex covers every ID, so no message is deleted twice). The
// reverse order could report success while leaving a completed manifest
// serialized as in_progress.
func (e *Executor) finalizeExecution(manifestID string, manifest *Manifest, succeeded, failed int, failedIDs []string, failOnAllErrors bool) error {
	// A durable cancelled/ marker is authoritative: even in the pathological
	// case where a stray write recreated in_progress/<id>.json, completing here
	// would leave the manifest in both cancelled/ and completed/ (a double
	// copy). Refuse to finalize when cancellation is on record.
	if e.manager.ManifestCancelled(manifestID) {
		e.logger.Info("manifest cancelled; not finalizing", "manifest", manifestID)
		return ErrManifestCancelled
	}

	var targetStatus Status
	if failed == 0 || succeeded > 0 || !failOnAllErrors {
		targetStatus = StatusCompleted
	} else {
		targetStatus = StatusFailed
	}

	if manifest.Execution == nil {
		manifest.Execution = &Execution{StartedAt: time.Now(), Method: "unknown"}
	}
	now := time.Now()
	manifest.Execution.CompletedAt = &now
	manifest.Execution.LastProcessedIndex = len(manifest.GmailIDs)
	manifest.Execution.Succeeded = succeeded
	manifest.Execution.Failed = failed
	manifest.Execution.FailedIDs = failedIDs
	manifest.Status = targetStatus
	if err := e.manager.WriteInProgressCheckpoint(manifest, manifestID); err != nil {
		if errors.Is(err, ErrManifestCancelled) {
			e.logger.Info("manifest cancelled during finalize; not completing", "manifest", manifestID)
			return ErrManifestCancelled
		}
		return fmt.Errorf("persist final state for manifest %s: %w", manifestID, err)
	}

	if err := e.manager.FinalizeInProgress(manifestID, targetStatus); err != nil {
		if errors.Is(err, ErrManifestCancelled) || errors.Is(err, os.ErrNotExist) {
			e.logger.Info("manifest cancelled during finalize; not completing", "manifest", manifestID)
			return ErrManifestCancelled
		}
		return fmt.Errorf("finalize manifest %s: %w", manifestID, err)
	}

	e.progress.OnComplete(succeeded, failed)

	e.logger.Debug("deletion complete",
		"manifest", manifestID,
		"succeeded", succeeded,
		"failed", failed,
	)
	return nil
}

// Execute performs the deletion for a manifest.
func (e *Executor) Execute(ctx context.Context, manifestID string, opts *ExecuteOptions) error {
	if opts == nil {
		opts = DefaultExecuteOptions()
	}

	manifest, err := e.prepareExecution(manifestID, opts.Method)
	if err != nil {
		return err
	}
	sourceID, err := e.manifestSourceID(manifest)
	if err != nil {
		return fmt.Errorf("validate manifest source: %w", err)
	}

	// Determine starting point
	startIndex := 0
	succeeded := manifest.Execution.Succeeded
	failed := manifest.Execution.Failed
	failedIDs := manifest.Execution.FailedIDs
	var retryIDs []string
	if opts.Resume {
		startIndex = manifest.Execution.LastProcessedIndex
		// Retry checkpointed transient failures before continuing, mirroring
		// ExecuteBatch: a resume that only continued from LastProcessedIndex
		// would permanently skip messages that failed before the interruption
		// and still finalize as completed.
		if len(failedIDs) > 0 {
			retryIDs = failedIDs
			failedIDs = nil
			failed = 0
		}
	}

	e.logger.Debug("executing deletion",
		"manifest", manifestID,
		"total", len(manifest.GmailIDs),
		"start_index", startIndex,
		"retry_ids", len(retryIDs),
		"method", opts.Method,
	)

	// When retries are pending, report succeeded count (not startIndex)
	// to avoid showing 100% while retry work is still running.
	alreadyProcessed := startIndex
	if len(retryIDs) > 0 {
		alreadyProcessed = succeeded
	}
	e.progress.OnStart(len(manifest.GmailIDs), alreadyProcessed)

	// Retry previously failed IDs before continuing with remaining messages
	for ri, gmailID := range retryIDs {
		select {
		case <-ctx.Done():
			remaining := slices.Concat(failedIDs, retryIDs[ri:])
			e.saveCheckpoint(manifest, manifestID, startIndex, succeeded, len(remaining), remaining)
			return ctx.Err()
		default:
		}

		if e.manifestCancelled(manifestID) {
			e.logger.Info("deletion cancelled during retry; stopping", "manifest", manifestID, "retried", ri)
			return ErrManifestCancelled
		}

		result, delErr := e.deleteOne(ctx, sourceID, gmailID, opts.Method)
		switch result {
		case resultSuccess:
			succeeded++
		case resultFatal:
			remaining := slices.Concat(failedIDs, retryIDs[ri:])
			e.saveCheckpoint(manifest, manifestID, startIndex, succeeded, len(remaining), remaining)
			return fmt.Errorf("delete message: %w", delErr)
		case resultFailed:
			if errors.Is(delErr, errLocalTombstone) {
				remaining := slices.Concat(failedIDs, []string{gmailID}, retryIDs[ri+1:])
				e.saveCheckpoint(manifest, manifestID, startIndex, succeeded, len(remaining), remaining)
				return fmt.Errorf("delete message: %w", delErr)
			}
			failed++
			failedIDs = append(failedIDs, gmailID)
		}
	}

	for i := startIndex; i < len(manifest.GmailIDs); i++ {
		select {
		case <-ctx.Done():
			e.saveCheckpoint(manifest, manifestID, i, succeeded, failed, failedIDs)
			return ctx.Err()
		default:
		}

		// Cooperative cross-process cancellation: a daemon cancel moved the
		// manifest out of in_progress/. Stop before deleting the next message
		// and do not checkpoint (which would resurrect the file).
		if e.manifestCancelled(manifestID) {
			e.logger.Info("deletion cancelled; stopping", "manifest", manifestID, "processed", i)
			return ErrManifestCancelled
		}

		result, delErr := e.deleteOne(ctx, sourceID, manifest.GmailIDs[i], opts.Method)
		switch result {
		case resultSuccess:
			succeeded++
		case resultFatal:
			e.saveCheckpoint(manifest, manifestID, i, succeeded, failed, failedIDs)
			return fmt.Errorf("delete message: %w", delErr)
		case resultFailed:
			if errors.Is(delErr, errLocalTombstone) {
				remaining := append(slices.Clone(failedIDs), manifest.GmailIDs[i])
				e.saveCheckpoint(manifest, manifestID, i+1, succeeded, len(remaining), remaining)
				return fmt.Errorf("delete message: %w", delErr)
			}
			failed++
			failedIDs = append(failedIDs, manifest.GmailIDs[i])
		}

		// Save checkpoint periodically
		if (i+1)%opts.BatchSize == 0 {
			e.saveCheckpoint(manifest, manifestID, i+1, succeeded, failed, failedIDs)
			e.progress.OnProgress(i+1, succeeded, failed)
		}
	}

	return e.finalizeExecution(manifestID, manifest, succeeded, failed, failedIDs, true)
}

// ExecuteBatch performs batch deletion (more efficient but permanent).
func (e *Executor) ExecuteBatch(ctx context.Context, manifestID string) error {
	manifest, err := e.prepareExecution(manifestID, MethodDelete)
	if err != nil {
		return err
	}
	sourceID, err := e.manifestSourceID(manifest)
	if err != nil {
		return fmt.Errorf("validate manifest source: %w", err)
	}

	if e.manifestCancelled(manifestID) {
		e.logger.Info("deletion cancelled before batch start; stopping", "manifest", manifestID)
		return ErrManifestCancelled
	}
	// The manifest is already claimed into in_progress/ by prepareExecution,
	// so this initial checkpoint normally has a file to overwrite. Route it
	// through the locked writer anyway: a daemon cancel could land in the
	// window between the check above and this write, and the write must not
	// recreate a manifest that cancel just moved to cancelled/.
	if err := e.manager.WriteInProgressCheckpoint(manifest, manifestID); err != nil {
		if errors.Is(err, ErrManifestCancelled) {
			e.logger.Info("deletion cancelled before batch start; stopping", "manifest", manifestID)
			return ErrManifestCancelled
		}
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
				e.saveCheckpoint(manifest, manifestID, startIndex, succeeded, len(remaining), remaining)
				return ctx.Err()
			default:
			}

			if e.manifestCancelled(manifestID) {
				e.logger.Info("deletion cancelled during retry; stopping", "manifest", manifestID, "retried", ri)
				return ErrManifestCancelled
			}

			result, delErr := e.deleteOne(ctx, sourceID, gmailID, MethodDelete)
			switch result {
			case resultSuccess:
				succeeded++
			case resultFatal:
				remaining := slices.Concat(failedIDs, retryIDs[ri:])
				e.saveCheckpoint(manifest, manifestID, startIndex, succeeded, len(remaining), remaining)
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
			e.saveCheckpoint(manifest, manifestID, i, succeeded, failed, failedIDs)
			return ctx.Err()
		default:
		}

		if e.manifestCancelled(manifestID) {
			e.logger.Info("deletion cancelled; stopping", "manifest", manifestID, "processed", i)
			return ErrManifestCancelled
		}

		end := min(i+batchSize, len(manifest.GmailIDs))

		batch := manifest.GmailIDs[i:end]

		e.logger.Debug("deleting batch", "start", i, "end", end, "size", len(batch))

		if err := e.client.BatchDeleteMessages(ctx, batch); err != nil {
			if isInsufficientScopeError(err) {
				e.saveCheckpoint(manifest, manifestID, i, succeeded, failed, failedIDs)
				return fmt.Errorf("batch delete: %w", err)
			}
			e.logger.Warn("batch delete failed, falling back to individual deletes", "start_index", i, "error", err)
			// Fall back to individual deletes
			for j, gmailID := range batch {
				select {
				case <-ctx.Done():
					e.saveCheckpoint(manifest, manifestID, i+j, succeeded, failed, failedIDs)
					return ctx.Err()
				default:
				}

				if e.manifestCancelled(manifestID) {
					e.logger.Info("deletion cancelled during fallback; stopping", "manifest", manifestID, "processed", i+j)
					return ErrManifestCancelled
				}

				result, delErr := e.deleteOne(ctx, sourceID, gmailID, MethodDelete)
				switch result {
				case resultSuccess:
					succeeded++
				case resultFatal:
					e.saveCheckpoint(manifest, manifestID, i+j, succeeded, failed, failedIDs)
					return fmt.Errorf("delete message: %w", delErr)
				case resultFailed:
					if errors.Is(delErr, errLocalTombstone) {
						remaining := slices.Concat(failedIDs, batch[j:])
						remaining = append(remaining, manifest.GmailIDs[end:]...)
						e.saveCheckpoint(manifest, manifestID, len(manifest.GmailIDs), succeeded, len(remaining), remaining)
						return fmt.Errorf("delete message: %w", delErr)
					}
					failed++
					failedIDs = append(failedIDs, gmailID)
				}
				e.progress.OnProgress(i+j+1, succeeded, failed)
			}
		} else {
			// Mark all as deleted in DB using batch update
			if markErr := e.store.MarkMessagesDeletedBySourceMessageIDBatch(sourceID, batch); markErr != nil {
				e.logger.Warn("failed to mark batch as deleted in DB", "count", len(batch), "error", markErr)
				remaining := append(slices.Clone(failedIDs), batch...)
				remaining = append(remaining, manifest.GmailIDs[end:]...)
				e.saveCheckpoint(manifest, manifestID, len(manifest.GmailIDs), succeeded, len(remaining), remaining)
				return fmt.Errorf("mark batch deleted in DB: %w", markErr)
			}
			succeeded += len(batch)
		}

		e.progress.OnProgress(end, succeeded, failed)
	}

	return e.finalizeExecution(manifestID, manifest, succeeded, failed, failedIDs, false)
}
