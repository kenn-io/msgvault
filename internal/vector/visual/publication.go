package visual

import (
	"context"
	"errors"

	"go.kenn.io/msgvault/internal/store"
)

// preparePublication reserves the pending publication BEFORE the provider
// request, so the in-place source-invalidation triggers cover the entire
// provider window deterministically: any attachment or context change after
// this point clears the pending token and the commit misses. The claim-time
// content stamp guards the remaining claim-to-prepare gap. It returns false
// (with no error) when the claim or owner is already obsolete.
func (w *Worker) preparePublication(
	ctx context.Context,
	work WorkItem,
	result *WorkerResult,
) (token string, previousToken string, ok bool, err error) {
	if publication, getErr := w.archive.GetVisualPublication(
		ctx, work.Claim.GenerationID, work.Candidate.Owner,
	); getErr == nil {
		previousToken = publication.CurrentVectorToken
	}
	token, err = w.archive.PrepareVisualPublication(ctx, store.PreparedVisualPublication{
		Claim:                      work.Claim,
		RepresentativeAttachmentID: work.Candidate.RepresentativeAttachmentID,
		Role:                       work.Candidate.Role, RoleSource: work.Candidate.RoleSource,
	})
	if err != nil {
		if errors.Is(err, store.ErrVisualClaimLost) ||
			errors.Is(err, store.ErrVisualSourceChanged) ||
			errors.Is(err, store.ErrVisualOwnerMissing) {
			result.Obsolete++
			return "", "", false, nil
		}
		return "", "", false, err
	}
	return token, previousToken, true, nil
}

// publish stores the vector under the reserved token and atomically promotes
// it, discarding the paid result when the source moved underneath the claim.
func (w *Worker) publish(
	ctx context.Context,
	work WorkItem,
	token string,
	previousToken string,
	vector []float32,
	result *WorkerResult,
) error {
	vectorToken := VectorToken(token)
	if err := w.backend.PutUnpublished(ctx, vectorToken, vector); err != nil {
		w.discardToken(ctx, work, token, result)
		_ = w.archive.ReleaseVisualWork(ctx, work.Claim)
		return err
	}
	if err := w.archive.CommitVisualPublication(ctx, work.Claim, token); err != nil {
		w.discardToken(ctx, work, token, result)
		if errors.Is(err, store.ErrVisualSourceChanged) ||
			errors.Is(err, store.ErrVisualClaimLost) ||
			errors.Is(err, store.ErrVisualOwnerMissing) {
			result.Obsolete++
			return nil
		}
		return err
	}
	result.Published++
	// Best-effort prompt delete of the replaced vector. The commit already
	// parked previousToken in superseded_vector_token, so a failure here is
	// retried by the obsolete-token sweep rather than orphaning the vector.
	if previousToken != "" && previousToken != token {
		if cleanupErr := w.backend.DeleteTokens(ctx, []VectorToken{VectorToken(previousToken)}); cleanupErr != nil {
			result.CleanupFailures++
		}
	}
	return nil
}

// discardToken deletes an unpublished vector the archive will never expose.
// If the backend delete fails, the token is parked on the owner's row so the
// obsolete-token sweep retries the delete instead of orphaning the vector.
func (w *Worker) discardToken(ctx context.Context, work WorkItem, token string, result *WorkerResult) {
	if err := w.backend.DeleteTokens(ctx, []VectorToken{VectorToken(token)}); err == nil {
		return
	}
	result.CleanupFailures++
	// Best-effort: if parking also fails the counter still records the leak,
	// matching the prior behavior for a doubly-unavailable backend/archive.
	_ = w.archive.ParkObsoleteVisualToken(ctx, work.Claim.GenerationID, work.Claim.Owner, token)
}
