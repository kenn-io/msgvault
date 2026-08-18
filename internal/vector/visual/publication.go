package visual

import (
	"context"
	"errors"

	"go.kenn.io/msgvault/internal/store"
)

func (w *Worker) publish(
	ctx context.Context,
	work WorkItem,
	vector []float32,
	result *WorkerResult,
) error {
	previousToken := ""
	publication, err := w.archive.GetVisualPublication(
		ctx, work.Claim.GenerationID, work.Candidate.Owner,
	)
	if err == nil {
		previousToken = publication.CurrentVectorToken
	}

	token, err := w.archive.PrepareVisualPublication(ctx, store.PreparedVisualPublication{
		Claim:                      work.Claim,
		RepresentativeAttachmentID: work.Candidate.RepresentativeAttachmentID,
		Role:                       work.Candidate.Role, RoleSource: work.Candidate.RoleSource,
	})
	if err != nil {
		if errors.Is(err, store.ErrVisualClaimLost) ||
			errors.Is(err, store.ErrVisualOwnerMissing) {
			result.Obsolete++
			return nil
		}
		return err
	}
	vectorToken := VectorToken(token)
	if err := w.backend.PutUnpublished(ctx, vectorToken, vector); err != nil {
		if cleanupErr := w.backend.DeleteTokens(ctx, []VectorToken{vectorToken}); cleanupErr != nil {
			result.CleanupFailures++
		}
		_ = w.archive.ReleaseVisualWork(ctx, work.Claim)
		return err
	}
	if err := w.archive.CommitVisualPublication(ctx, work.Claim, token); err != nil {
		if cleanupErr := w.backend.DeleteTokens(ctx, []VectorToken{vectorToken}); cleanupErr != nil {
			result.CleanupFailures++
		}
		if errors.Is(err, store.ErrVisualSourceChanged) ||
			errors.Is(err, store.ErrVisualClaimLost) ||
			errors.Is(err, store.ErrVisualOwnerMissing) {
			result.Obsolete++
			return nil
		}
		return err
	}
	result.Published++
	if previousToken != "" && previousToken != token {
		if cleanupErr := w.backend.DeleteTokens(ctx, []VectorToken{VectorToken(previousToken)}); cleanupErr != nil {
			result.CleanupFailures++
		}
	}
	return nil
}
