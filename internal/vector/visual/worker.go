package visual

import (
	"go.kenn.io/docbank/document/voyage"

	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"go.kenn.io/msgvault/internal/store"
)

type WorkerConfig struct {
	Dimension       int
	ProviderTimeout time.Duration
	LeaseDuration   time.Duration
	RenewInterval   time.Duration
	MaxBatchItems   int
	Now             func() time.Time
}

type WorkerResult struct {
	Attempted int64
	Succeeded int64
	Failed    int64
	Skipped   int64

	ProviderRequests int64
	Published        int64
	Retryable        int64
	Terminal         int64
	Obsolete         int64
	CleanupFailures  int64
	Usage            Usage
}

// Durable outcome kinds recorded with rejections. Terminal decisions are
// converged until their revision changes or an operator retries; retryable
// ones re-enter reconciliation on the next pass.
const (
	OutcomeTerminal  = "terminal"
	OutcomeRetryable = "retryable"
)

type Worker struct {
	archive  *store.Store
	provider Provider
	backend  Backend
	config   WorkerConfig
}

func NewWorker(
	archive *store.Store,
	provider Provider,
	backend Backend,
	config WorkerConfig,
) (*Worker, error) {
	if archive == nil || provider == nil || backend == nil {
		return nil, errors.New("visual worker requires archive, provider, and backend")
	}
	if config.Dimension <= 0 || config.ProviderTimeout <= 0 ||
		config.LeaseDuration <= config.ProviderTimeout {
		return nil, errors.New("visual worker requires a positive dimension and a provider timeout shorter than the lease")
	}
	if config.RenewInterval == 0 {
		config.RenewInterval = config.ProviderTimeout / 3
	}
	if config.RenewInterval <= 0 || config.RenewInterval >= config.LeaseDuration {
		return nil, errors.New("visual worker renewal interval must be shorter than its lease")
	}
	if config.MaxBatchItems == 0 {
		config.MaxBatchItems = voyage.MaxBatchItems
	}
	if config.MaxBatchItems < 1 {
		return nil, errors.New("visual worker batch size must be positive")
	}
	if config.Now == nil {
		config.Now = func() time.Time { return time.Now().UTC() }
	}
	return &Worker{archive: archive, provider: provider, backend: backend, config: config}, nil
}

// Run publishes already-claimed reconciliation work. Every provider and vector
// operation happens outside archive transactions. Exact claim and source-fence
// checks remain in the archive prepare/commit operations.
func (w *Worker) Run(ctx context.Context, work []WorkItem) (result WorkerResult, runErr error) {
	defer result.finalizeOutcomes()
	for start := 0; start < len(work); start += w.config.MaxBatchItems {
		end := min(start+w.config.MaxBatchItems, len(work))
		if err := w.runBatch(ctx, work[start:end], &result); err != nil {
			return result, err
		}
	}
	return result, nil
}

// finalizeOutcomes projects durable attachment decisions, not provider calls
// or batch splits. Obsolete work is a skipped final decision and participates
// in the visual attempted invariant.
func (r *WorkerResult) finalizeOutcomes() {
	r.Succeeded = r.Published
	r.Failed = r.Retryable + r.Terminal
	r.Skipped = r.Obsolete
	r.Attempted = r.Succeeded + r.Failed + r.Skipped
}

func (w *Worker) runBatch(
	ctx context.Context,
	work []WorkItem,
	result *WorkerResult,
) error {
	if len(work) == 0 {
		return nil
	}
	// Reserve every publication before the provider request so source
	// changes during the request deterministically invalidate the pending
	// row; already-obsolete claims drop out here without a paid call.
	live := make([]WorkItem, 0, len(work))
	tokens := make([]string, 0, len(work))
	previousTokens := make([]string, 0, len(work))
	for index := range work {
		token, previousToken, ok, err := w.preparePublication(ctx, work[index], result)
		if err != nil {
			// A hard failure here (including caller cancellation) must not
			// strand the batch's claims until lease expiry: nothing was sent
			// to the provider yet, so releasing is always safe.
			releaseCtx := context.WithoutCancel(ctx)
			var releaseErr error
			for _, item := range work {
				releaseErr = errors.Join(releaseErr, w.archive.ReleaseVisualWork(releaseCtx, item.Claim))
			}
			return errors.Join(err, releaseErr)
		}
		if !ok {
			continue
		}
		live = append(live, work[index])
		tokens = append(tokens, token)
		previousTokens = append(previousTokens, previousToken)
	}
	if len(live) == 0 {
		return nil
	}
	work = live
	documents := make([]DocumentInput, len(work))
	claims := make([]store.VisualWorkClaim, len(work))
	for index := range work {
		documents[index] = work[index].Document
		claims[index] = work[index].Claim
	}

	result.ProviderRequests++
	providerCtx, cancel := context.WithTimeout(ctx, w.config.ProviderTimeout)
	renewErr := make(chan error, 1)
	stopRenewal := make(chan struct{})
	go w.renewClaims(providerCtx, claims, stopRenewal, cancel, renewErr)
	embeddings, err := w.provider.EmbedDocuments(providerCtx, documents)
	close(stopRenewal)
	renewalErr := <-renewErr
	cancel()
	if renewalErr != nil {
		return fmt.Errorf("renew visual work claims: %w", renewalErr)
	}
	if err != nil {
		// Rejections bisect like oversized batches: a multi-item rejection
		// does not identify the offending document, and recording terminal
		// outcomes for the whole batch would permanently exclude valid
		// neighbors. Only a rejected SINGLETON is a durable decision.
		if (errors.Is(err, ErrProviderBatchTooLarge) || errors.Is(err, ErrProviderRejected)) && len(work) > 1 {
			middle := len(work) / 2
			if err := w.runBatch(ctx, work[:middle], result); err != nil {
				return err
			}
			return w.runBatch(ctx, work[middle:], result)
		}
		return w.recordProviderFailure(ctx, work, err, result)
	}
	ordered, usage, err := w.validateEmbeddings(work, embeddings)
	if err != nil {
		return w.recordProviderFailure(ctx, work, err, result)
	}
	mergeUsage(&result.Usage, usage)
	for index := range work {
		if err := w.publish(ctx, work[index], tokens[index], previousTokens[index], ordered[index], result); err != nil {
			return err
		}
	}
	return nil
}

func (w *Worker) renewClaims(
	ctx context.Context,
	claims []store.VisualWorkClaim,
	stop <-chan struct{},
	cancel context.CancelFunc,
	result chan<- error,
) {
	ticker := time.NewTicker(w.config.RenewInterval)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			result <- nil
			return
		case <-ctx.Done():
			result <- nil
			return
		case <-ticker.C:
			for _, claim := range claims {
				if err := w.archive.RenewVisualWork(
					ctx, claim, w.config.Now(), w.config.LeaseDuration,
				); err != nil {
					if ctx.Err() != nil && (errors.Is(err, context.Canceled) ||
						errors.Is(err, context.DeadlineExceeded)) {
						result <- nil
						return
					}
					cancel()
					result <- err
					return
				}
			}
		}
	}
}

func (w *Worker) validateEmbeddings(
	work []WorkItem,
	embeddings []EmbeddingResult,
) ([][]float32, Usage, error) {
	if len(embeddings) != len(work) {
		return nil, Usage{}, providerMalformedError(nil)
	}
	byOwner := make(map[Owner][]float32, len(embeddings))
	usage := Usage{}
	for _, embedding := range embeddings {
		if _, exists := byOwner[embedding.Owner]; exists ||
			len(embedding.Vector) != w.config.Dimension || !finiteVector(embedding.Vector) {
			return nil, Usage{}, providerMalformedError(nil)
		}
		byOwner[embedding.Owner] = embedding.Vector
		mergeUsage(&usage, embedding.Usage)
	}
	ordered := make([][]float32, len(work))
	for index, item := range work {
		vector, exists := byOwner[item.Document.Owner]
		if !exists {
			return nil, Usage{}, providerMalformedError(nil)
		}
		ordered[index] = vector
	}
	return ordered, usage, nil
}

func (w *Worker) recordProviderFailure(
	ctx context.Context,
	work []WorkItem,
	err error,
	result *WorkerResult,
) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		releaseCtx := context.WithoutCancel(ctx)
		var releaseErr error
		for _, item := range work {
			releaseErr = errors.Join(releaseErr, w.archive.ReleaseVisualWork(releaseCtx, item.Claim))
		}
		if releaseErr != nil {
			return errors.Join(err, fmt.Errorf("release visual work: %w", releaseErr))
		}
		return err
	}
	// Authentication and configuration failures affect the complete run, not
	// one media owner. Keep claims retryable and surface the operator error.
	if errors.Is(err, ErrProviderUnauthorized) {
		for _, item := range work {
			_ = w.archive.ReleaseVisualWork(ctx, item.Claim)
		}
		return err
	}
	kind, reason := OutcomeRetryable, "provider_error"
	if errors.Is(err, ErrProviderBatchTooLarge) {
		kind, reason = OutcomeTerminal, string(ReasonProviderLimit)
	} else if errors.Is(err, ErrProviderRejected) {
		kind, reason = OutcomeTerminal, "provider_rejected"
	} else if errors.Is(err, ErrProviderMalformed) {
		reason = "provider_malformed_response"
	}
	for _, item := range work {
		if rejectErr := w.archive.RejectVisualPublication(ctx, item.Claim, store.VisualOutcome{
			Kind: kind, Reason: reason,
		}); rejectErr != nil {
			if errors.Is(rejectErr, store.ErrVisualClaimLost) ||
				errors.Is(rejectErr, store.ErrVisualSourceChanged) ||
				errors.Is(rejectErr, store.ErrVisualOwnerMissing) {
				// The source moved underneath the failed request; the
				// reconciler re-evaluates it rather than recording an
				// outcome for content that no longer exists.
				result.Obsolete++
				continue
			}
			return rejectErr
		}
		if kind == OutcomeTerminal {
			result.Terminal++
		} else {
			result.Retryable++
		}
	}
	return nil
}

func finiteVector(vector []float32) bool {
	if len(vector) == 0 {
		return false
	}
	for _, value := range vector {
		if math.IsNaN(float64(value)) || math.IsInf(float64(value), 0) {
			return false
		}
	}
	return true
}

func mergeUsage(total *Usage, next Usage) {
	if !next.Available {
		return
	}
	total.Available = true
	total.TotalTokens += next.TotalTokens
}
