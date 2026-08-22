package document

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.kenn.io/msgvault/internal/store"
)

const maxReconcileLimit = 1000

type WorkerRunner interface {
	Run(ctx context.Context, generationID GenerationID, limit int) (RunResult, error)
}

type ReconcileLedger interface {
	GetDocumentVectorGeneration(ctx context.Context, id int64) (store.DocumentVectorGeneration, error)
	GetDocumentVectorGenerationStatus(ctx context.Context, generationID int64, afterToken string, limit int) (store.DocumentVectorGenerationStatus, error)
	ParkObsoleteDocumentVectorTokens(ctx context.Context, generationID int64, afterToken string, limit int, now time.Time) (store.DocumentVectorCleanupPage, error)
	FinalizeObsoleteDocumentVectorToken(ctx context.Context, generationID int64, token string, now time.Time) (bool, error)
	GetDocumentVectorCoverage(ctx context.Context, generationID int64) (store.DocumentVectorCoverage, error)
	ActivateDocumentVectorGeneration(ctx context.Context, generationID int64, now time.Time) error
	PurgeRetiredDocumentVectorGeneration(ctx context.Context, generationID int64) (bool, error)
}

var (
	_ WorkerRunner    = (*Worker)(nil)
	_ ReconcileLedger = (*store.Store)(nil)
)

type ReconcilerDeps struct {
	Ledger  ReconcileLedger
	Worker  WorkerRunner
	Backend Backend
	Now     func() time.Time

	AfterCleanupGenerationID GenerationID
	AfterCleanupToken        string
}

type ReconcileResult struct {
	WorkerRan bool      `json:"worker_ran"`
	Worker    RunResult `json:"worker"`

	CleanupListed            int          `json:"cleanup_listed"`
	CleanupDeleted           int          `json:"cleanup_deleted"`
	CleanupFinalized         int          `json:"cleanup_finalized"`
	CleanupAfterGenerationID GenerationID `json:"cleanup_after_generation_id,omitempty"`
	CleanupAfterToken        string       `json:"cleanup_after_token,omitempty"`
	CleanupExhausted         bool         `json:"cleanup_exhausted"`

	Status    GenerationStatus             `json:"status"`
	Coverage  store.DocumentVectorCoverage `json:"coverage"`
	Activated bool                         `json:"activated"`
	Purged    bool                         `json:"purged"`
	Converged bool                         `json:"converged"`
}

type Reconciler struct {
	deps                      ReconcilerDeps
	cleanupCursorGenerationID GenerationID
	cleanupAfterToken         string
}

func NewReconciler(deps ReconcilerDeps) *Reconciler {
	if deps.Now == nil {
		deps.Now = func() time.Time { return time.Now().UTC() }
	}
	return &Reconciler{
		deps: deps, cleanupCursorGenerationID: deps.AfterCleanupGenerationID,
		cleanupAfterToken: deps.AfterCleanupToken,
	}
}

func (r *Reconciler) Run(ctx context.Context, generationID GenerationID, limit int) (ReconcileResult, error) {
	var result ReconcileResult
	if err := r.validate(generationID, limit); err != nil {
		return result, err
	}
	generation, err := r.deps.Ledger.GetDocumentVectorGeneration(ctx, int64(generationID))
	if err != nil {
		return result, fmt.Errorf("read document vector generation for reconciliation: %w", err)
	}
	if generation.ID != int64(generationID) {
		return result, store.ErrDocumentVectorInvalidGenerationState
	}
	if generation.State == store.DocumentVectorGenerationBuilding && r.deps.Worker == nil {
		return result, errors.New("document vector reconciliation worker is required for a building generation")
	}
	r.bindCleanupCursor(generationID)

	var reconcileErr error
	if generation.State == store.DocumentVectorGenerationBuilding {
		result.WorkerRan = true
		result.Worker, err = r.deps.Worker.Run(ctx, generationID, limit)
		if ctxErr := ctx.Err(); ctxErr != nil {
			return result, ctxErr
		}
		reconcileErr = errors.Join(reconcileErr, err)
	}

	cleanupErr := r.cleanup(ctx, generationID, limit, &result)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return result, errors.Join(reconcileErr, ctxErr)
	}
	reconcileErr = errors.Join(reconcileErr, cleanupErr)

	status, statusErr := r.deps.Ledger.GetDocumentVectorGenerationStatus(ctx, int64(generationID), "", limit)
	if statusErr == nil {
		result.Status = status
		if status.CleanupPending == 0 {
			r.resetCleanupCursor(&result)
		}
	}
	reconcileErr = errors.Join(reconcileErr, statusErr)

	switch generation.State {
	case store.DocumentVectorGenerationBuilding, store.DocumentVectorGenerationActive:
		coverage, coverageErr := r.deps.Ledger.GetDocumentVectorCoverage(ctx, int64(generationID))
		if coverageErr == nil {
			result.Coverage = coverage
		}
		reconcileErr = errors.Join(reconcileErr, coverageErr)
		if generation.State == store.DocumentVectorGenerationBuilding && coverageErr == nil &&
			coverage.Complete() && reconcileErr == nil {
			activationErr := r.deps.Ledger.ActivateDocumentVectorGeneration(ctx, int64(generationID), r.now())
			if activationErr == nil {
				result.Activated = true
				if refreshed, refreshErr := r.deps.Ledger.GetDocumentVectorGenerationStatus(ctx, int64(generationID), "", limit); refreshErr == nil {
					result.Status = refreshed
				} else {
					reconcileErr = refreshErr
				}
			} else {
				reconcileErr = activationErr
			}
		}
		result.Converged = (generation.State == store.DocumentVectorGenerationActive || result.Activated) &&
			result.Coverage.Complete() && result.Status.CleanupPending == 0 && reconcileErr == nil
	case store.DocumentVectorGenerationRetired:
		if result.Status.CleanupPending == 0 && reconcileErr == nil {
			result.Purged, err = r.deps.Ledger.PurgeRetiredDocumentVectorGeneration(ctx, int64(generationID))
			reconcileErr = errors.Join(reconcileErr, err)
		}
		result.Converged = result.Purged && reconcileErr == nil
	default:
		reconcileErr = errors.Join(reconcileErr, store.ErrDocumentVectorInvalidGenerationState)
	}
	return result, reconcileErr
}

func (r *Reconciler) cleanup(
	ctx context.Context, generationID GenerationID, limit int, result *ReconcileResult,
) error {
	page, err := r.deps.Ledger.ParkObsoleteDocumentVectorTokens(
		ctx, int64(generationID), r.cleanupAfterToken, limit, r.now(),
	)
	if err != nil {
		return fmt.Errorf("park obsolete document vector tokens: %w", err)
	}
	result.CleanupListed = len(page.Tokens)
	if len(page.Tokens) == 0 {
		r.resetCleanupCursor(result)
		return nil
	}
	opaque := make([]string, len(page.Tokens))
	for index := range page.Tokens {
		opaque[index] = page.Tokens[index].Token
	}
	if err := r.deps.Backend.DeleteTokens(ctx, generationID, opaque); err != nil {
		r.invalidateCleanupCursor(result)
		return fmt.Errorf("delete obsolete document vector tokens: %w", err)
	}
	result.CleanupDeleted = len(opaque)
	var finalizeErr error
	for _, token := range opaque {
		finalized, err := r.deps.Ledger.FinalizeObsoleteDocumentVectorToken(
			ctx, int64(generationID), token, r.now(),
		)
		if err != nil {
			finalizeErr = errors.Join(finalizeErr, fmt.Errorf("finalize obsolete document vector token %q: %w", token, err))
			continue
		}
		if finalized {
			result.CleanupFinalized++
		}
	}
	if finalizeErr != nil {
		r.invalidateCleanupCursor(result)
		return finalizeErr
	}
	if page.Exhausted {
		r.resetCleanupCursor(result)
	} else {
		r.cleanupCursorGenerationID = GenerationID(page.AfterGenerationID)
		r.cleanupAfterToken = page.AfterToken
		result.CleanupAfterGenerationID = GenerationID(page.AfterGenerationID)
		result.CleanupAfterToken = page.AfterToken
	}
	return nil
}

func (r *Reconciler) validate(generationID GenerationID, limit int) error {
	if generationID <= 0 {
		return errors.New("document vector reconciliation generation must be positive")
	}
	if limit < 1 || limit > maxReconcileLimit {
		return fmt.Errorf("document vector reconciliation limit must be between 1 and %d", maxReconcileLimit)
	}
	if r == nil || r.deps.Ledger == nil || r.deps.Backend == nil || r.deps.Now == nil {
		return errors.New("document vector reconciliation dependencies are required")
	}
	if r.deps.AfterCleanupGenerationID < 0 ||
		(r.deps.AfterCleanupGenerationID == 0) != (r.deps.AfterCleanupToken == "") {
		return errors.New("document vector reconciliation cleanup cursor is invalid")
	}
	return nil
}

func (r *Reconciler) now() time.Time {
	return r.deps.Now().UTC().Truncate(time.Millisecond)
}

func (r *Reconciler) bindCleanupCursor(generationID GenerationID) {
	if r.cleanupCursorGenerationID != 0 && r.cleanupCursorGenerationID != generationID {
		r.cleanupAfterToken = ""
	}
	r.cleanupCursorGenerationID = generationID
}

func (r *Reconciler) resetCleanupCursor(result *ReconcileResult) {
	r.cleanupCursorGenerationID = 0
	r.cleanupAfterToken = ""
	result.CleanupAfterGenerationID = 0
	result.CleanupAfterToken = ""
	result.CleanupExhausted = true
}

func (r *Reconciler) invalidateCleanupCursor(result *ReconcileResult) {
	r.cleanupCursorGenerationID = 0
	r.cleanupAfterToken = ""
	result.CleanupAfterGenerationID = 0
	result.CleanupAfterToken = ""
	result.CleanupExhausted = false
}
