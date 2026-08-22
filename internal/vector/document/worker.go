package document

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/vector"
)

const (
	maxWorkerRunLimit   = 1000
	maxWorkerRetryDelay = 7 * 24 * time.Hour
)

var (
	errInvalidProviderShape  = errors.New("invalid document embedding provider shape")
	errInvalidProviderVector = errors.New("invalid document embedding provider vector")
	errBackendPut            = errors.New("document vector backend put failed")
)

// Ledger is the authoritative publication surface used by Worker.
// *store.Store satisfies it.
type Ledger interface {
	GetDocumentVectorGeneration(ctx context.Context, id int64) (store.DocumentVectorGeneration, error)
	ListDocumentVectorChunkCandidates(ctx context.Context, generationID, afterChunkID int64, limit int) ([]store.DocumentVectorChunkCandidate, error)
	ClaimDocumentVectorChunk(ctx context.Context, generationID, afterChunkID int64, scanLimit int, owner string, now time.Time, leaseDuration time.Duration) (*store.DocumentVectorChunkClaim, error)
	RenewDocumentVectorChunkClaim(ctx context.Context, generationID int64, token, owner string, fence int64, now time.Time, leaseDuration time.Duration) (time.Time, error)
	CommitDocumentVectorPublication(ctx context.Context, generationID int64, token, owner string, fence int64, now time.Time) error
	FailDocumentVectorChunk(ctx context.Context, generationID int64, token, owner string, fence int64, now time.Time, nextRetryAt *time.Time, terminal bool, errorCode string) error
}

var _ Ledger = (*store.Store)(nil)

// WorkerDeps are the bounded collaborators and policy values for Worker.
type WorkerDeps struct {
	Ledger   Ledger
	Provider Provider
	Backend  Backend

	Owner             string
	Dimension         int
	MaxInputChars     int
	LeaseDuration     time.Duration
	HeartbeatInterval time.Duration
	RetryDelay        time.Duration
	MaxAttempts       int
	// AfterGenerationID and AfterChunkID restore the bounded scan cursor reported
	// by a prior run of the same generation. Task 6b may persist this pair; Worker
	// also carries it across sequential runs and resets it when generations change.
	AfterGenerationID GenerationID
	AfterChunkID      int64
	Now               func() time.Time
}

// RunResult reports only locally observable accounting. Provider token usage
// is deliberately absent because EmbedDocuments does not report it.
type RunResult struct {
	Claimed       int `json:"claimed"`
	Embedded      int `json:"embedded"`
	Published     int `json:"published"`
	Retry         int `json:"retry"`
	Terminal      int `json:"terminal"`
	SourceChanged int `json:"source_changed"`

	ProviderCalls      int `json:"provider_calls"`
	ProviderDocuments  int `json:"provider_documents"`
	ProviderChunks     int `json:"provider_chunks"`
	ProviderInputChars int `json:"provider_input_chars"`

	AfterGenerationID GenerationID `json:"after_generation_id,omitempty"`
	AfterChunkID      int64        `json:"after_chunk_id,omitempty"`
	Exhausted         bool         `json:"exhausted"`
}

// Worker publishes one bounded page of a building document-vector generation.
// A Worker is safe for sequential use.
type Worker struct {
	deps               WorkerDeps
	cursorGenerationID GenerationID
	afterChunkID       int64
}

// workerClaimHeartbeat owns every live claim from provider dispatch through
// backend publication. releaseForTransition serializes the final renewal and
// ownership removal against periodic renewal before Commit or Fail begins.
type workerClaimHeartbeat struct {
	worker       *Worker
	generationID GenerationID
	ctx          context.Context
	cancel       context.CancelFunc
	stopCh       chan struct{}
	doneCh       chan struct{}
	stopOnce     sync.Once

	mu     sync.Mutex
	active map[string]*store.DocumentVectorChunkClaim
	runErr error
}

func newWorkerClaimHeartbeat(
	ctx context.Context, worker *Worker, generationID GenerationID, claims []*store.DocumentVectorChunkClaim,
) *workerClaimHeartbeat {
	workCtx, cancel := context.WithCancel(ctx)
	heartbeat := &workerClaimHeartbeat{
		worker: worker, generationID: generationID, ctx: workCtx, cancel: cancel,
		stopCh: make(chan struct{}), doneCh: make(chan struct{}),
		active: make(map[string]*store.DocumentVectorChunkClaim, len(claims)),
	}
	for _, claim := range claims {
		heartbeat.active[claim.Token] = claim
	}
	go heartbeat.run(ctx)
	return heartbeat
}

func (h *workerClaimHeartbeat) context() context.Context { return h.ctx }

func (h *workerClaimHeartbeat) err() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.runErr
}

func (h *workerClaimHeartbeat) releaseForTransition(claim *store.DocumentVectorChunkClaim) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.runErr != nil {
		return h.runErr
	}
	if err := h.ctx.Err(); err != nil {
		return err
	}
	if _, ok := h.active[claim.Token]; !ok {
		return fmt.Errorf("document vector claim %q is no longer heartbeat-owned", claim.Token)
	}
	if _, err := h.worker.deps.Ledger.RenewDocumentVectorChunkClaim(
		h.ctx, int64(h.generationID), claim.Token, claim.LeaseOwner, claim.LeaseFence,
		h.worker.deps.Now(), h.worker.deps.LeaseDuration,
	); err != nil {
		h.failLocked(claim.Token, err)
		return h.runErr
	}
	delete(h.active, claim.Token)
	return nil
}

func (h *workerClaimHeartbeat) run(parent context.Context) {
	defer close(h.doneCh)
	ticker := time.NewTicker(h.worker.deps.HeartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-parent.Done():
			h.mu.Lock()
			if h.runErr == nil {
				h.runErr = parent.Err()
			}
			h.cancel()
			h.mu.Unlock()
			return
		case <-h.stopCh:
			return
		case <-ticker.C:
			h.mu.Lock()
			for _, claim := range h.active {
				_, err := h.worker.deps.Ledger.RenewDocumentVectorChunkClaim(
					h.ctx, int64(h.generationID), claim.Token, claim.LeaseOwner,
					claim.LeaseFence, h.worker.deps.Now(), h.worker.deps.LeaseDuration,
				)
				if err != nil {
					h.failLocked(claim.Token, err)
					h.mu.Unlock()
					return
				}
			}
			h.mu.Unlock()
		}
	}
}

func (h *workerClaimHeartbeat) failLocked(token string, err error) {
	if h.runErr == nil {
		h.runErr = fmt.Errorf("renew document vector claim %q: %w", token, err)
	}
	h.cancel()
}

func (h *workerClaimHeartbeat) stop() {
	h.stopOnce.Do(func() { close(h.stopCh) })
	<-h.doneCh
	h.cancel()
}

func NewWorker(deps WorkerDeps) *Worker {
	if deps.Now == nil {
		deps.Now = func() time.Time { return time.Now().UTC() }
	}
	return &Worker{
		deps:               deps,
		cursorGenerationID: deps.AfterGenerationID,
		afterChunkID:       deps.AfterChunkID,
	}
}

func (w *Worker) Run(ctx context.Context, generationID GenerationID, limit int) (RunResult, error) {
	var result RunResult
	if limit < 1 || limit > maxWorkerRunLimit {
		return result, fmt.Errorf("document vector worker limit must be between 1 and %d", maxWorkerRunLimit)
	}
	if err := w.validate(generationID); err != nil {
		return result, err
	}
	generation, err := w.deps.Ledger.GetDocumentVectorGeneration(ctx, int64(generationID))
	if err != nil {
		return result, fmt.Errorf("read document vector generation: %w", err)
	}
	if generation.ID != int64(generationID) || generation.State != store.DocumentVectorGenerationBuilding {
		return result, store.ErrDocumentVectorInvalidGenerationState
	}
	if generation.Dimension != w.deps.Dimension {
		return result, fmt.Errorf("document vector worker dimension %d does not match generation dimension %d", w.deps.Dimension, generation.Dimension)
	}
	w.bindCursor(generationID)
	result.AfterGenerationID = generationID

	claims, err := w.collectClaims(ctx, generationID, limit, &result)
	if err != nil || len(claims) == 0 {
		return result, err
	}
	groups, inputs := groupWorkerClaims(claims, w.deps.MaxInputChars)
	result.ProviderCalls = 1
	result.ProviderDocuments = len(inputs)
	for _, input := range inputs {
		result.ProviderChunks += len(input.Chunks)
		for _, text := range input.Chunks {
			result.ProviderInputChars += utf8.RuneCountInString(text)
		}
	}

	heartbeat := newWorkerClaimHeartbeat(ctx, w, generationID, claims)
	defer heartbeat.stop()
	vectors, providerErr := w.deps.Provider.EmbedDocuments(heartbeat.context(), inputs)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return result, ctxErr
	}
	if heartbeatErr := heartbeat.err(); heartbeatErr != nil {
		return result, heartbeatErr
	}
	completed, unfinishedErr := validateWorkerProviderPrefix(inputs, vectors, providerErr, w.deps.Dimension)
	var embeddings []Embedding
	var completedClaims []*store.DocumentVectorChunkClaim
	for documentIndex := range completed {
		for chunkIndex, vector := range completed[documentIndex] {
			claim := groups[documentIndex][chunkIndex]
			embeddings = append(embeddings, Embedding{Token: claim.Token, Vector: vector})
			completedClaims = append(completedClaims, claim)
		}
	}
	result.Embedded = len(completedClaims)
	var runErr error
	if len(embeddings) > 0 {
		if err := w.deps.Backend.PutUnpublished(heartbeat.context(), generationID, w.deps.Dimension, embeddings); err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return result, ctxErr
			}
			if heartbeatErr := heartbeat.err(); heartbeatErr != nil {
				return result, heartbeatErr
			}
			putErr := fmt.Errorf("%w: %w", errBackendPut, err)
			failureErr := w.failClaims(ctx, generationID, completedClaims, putErr, heartbeat, &result)
			runErr = errors.Join(runErr, fmt.Errorf("put unpublished document vectors: %w", err), failureErr)
		} else {
			for _, claim := range completedClaims {
				if ctxErr := ctx.Err(); ctxErr != nil {
					return result, errors.Join(runErr, ctxErr)
				}
				if err := heartbeat.releaseForTransition(claim); err != nil {
					runErr = errors.Join(runErr, err)
					break
				}
				err := w.deps.Ledger.CommitDocumentVectorPublication(heartbeat.context(), int64(generationID), claim.Token, claim.LeaseOwner, claim.LeaseFence, w.deps.Now())
				switch {
				case err == nil:
					result.Published++
				case errors.Is(err, store.ErrDocumentVectorSourceChanged):
					result.SourceChanged++
					if deleteErr := w.deps.Backend.DeleteTokens(heartbeat.context(), generationID, []string{claim.Token}); deleteErr != nil {
						runErr = errors.Join(runErr, fmt.Errorf("delete changed document vector token: %w", deleteErr))
					}
				default:
					runErr = errors.Join(runErr, err)
				}
			}
		}
	}
	if unfinishedErr != nil {
		unfinishedClaims := flattenWorkerClaims(groups[len(completed):])
		failureErr := w.failClaims(ctx, generationID, unfinishedClaims, unfinishedErr, heartbeat, &result)
		runErr = errors.Join(runErr, unfinishedErr, failureErr)
	}
	runErr = errors.Join(runErr, heartbeat.err())
	return result, runErr
}

func (w *Worker) validate(generationID GenerationID) error {
	if generationID <= 0 {
		return errors.New("document vector worker generation must be positive")
	}
	if w.deps.Ledger == nil || w.deps.Provider == nil || w.deps.Backend == nil {
		return errors.New("document vector worker ledger, provider, and backend are required")
	}
	if strings.TrimSpace(w.deps.Owner) == "" || w.deps.Dimension <= 0 || w.deps.MaxInputChars <= 0 ||
		w.deps.LeaseDuration <= 0 || w.deps.HeartbeatInterval <= 0 ||
		w.deps.HeartbeatInterval >= w.deps.LeaseDuration || w.deps.RetryDelay <= 0 ||
		w.deps.RetryDelay > maxWorkerRetryDelay ||
		w.deps.MaxAttempts <= 0 || w.deps.AfterGenerationID < 0 || w.deps.AfterChunkID < 0 ||
		(w.deps.AfterGenerationID == 0) != (w.deps.AfterChunkID == 0) {
		return errors.New("document vector worker policy is invalid")
	}
	return nil
}

func (w *Worker) bindCursor(generationID GenerationID) {
	if w.cursorGenerationID != 0 && w.cursorGenerationID != generationID {
		w.afterChunkID = 0
	}
	w.cursorGenerationID = generationID
}

func (w *Worker) collectClaims(ctx context.Context, generationID GenerationID, limit int, result *RunResult) ([]*store.DocumentVectorChunkClaim, error) {
	claims := make([]*store.DocumentVectorChunkClaim, 0, limit)
	after := w.afterChunkID
	result.AfterChunkID = after
	candidates, err := w.deps.Ledger.ListDocumentVectorChunkCandidates(ctx, int64(generationID), after, maxWorkerRunLimit)
	if err != nil {
		return nil, fmt.Errorf("list document vector candidates: %w", err)
	}
	if len(candidates) == 0 {
		result.Exhausted = true
		w.resetCursor(result)
		return claims, nil
	}
	processed := 0
	for _, candidate := range candidates {
		processed++
		if candidate.ChunkID <= after {
			continue
		}
		claim, err := w.deps.Ledger.ClaimDocumentVectorChunk(ctx, int64(generationID), after, 1, w.deps.Owner, w.deps.Now(), w.deps.LeaseDuration)
		if err != nil {
			return nil, fmt.Errorf("claim document vector chunk: %w", err)
		}
		after = candidate.ChunkID
		if claim != nil {
			claims = append(claims, claim)
			result.Claimed++
			if claim.ChunkID > after {
				after = claim.ChunkID
			}
		}
		result.AfterChunkID = after
		if len(claims) == limit {
			break
		}
	}
	if processed == len(candidates) && len(candidates) < maxWorkerRunLimit {
		result.Exhausted = true
		w.resetCursor(result)
	} else {
		w.afterChunkID = after
	}
	return claims, nil
}

func (w *Worker) resetCursor(result *RunResult) {
	w.cursorGenerationID = 0
	w.afterChunkID = 0
	result.AfterGenerationID = 0
	result.AfterChunkID = 0
}

func groupWorkerClaims(claims []*store.DocumentVectorChunkClaim, maxInputChars int) ([][]*store.DocumentVectorChunkClaim, []vector.DocumentInput) {
	var groups [][]*store.DocumentVectorChunkClaim
	var inputs []vector.DocumentInput
	indices := make(map[string]int)
	for _, claim := range claims {
		index, ok := indices[claim.ExtractionID]
		if !ok {
			index = len(groups)
			indices[claim.ExtractionID] = index
			groups = append(groups, nil)
			inputs = append(inputs, vector.DocumentInput{})
		}
		groups[index] = append(groups[index], claim)
		inputs[index].Chunks = append(inputs[index].Chunks, documentEmbeddingInput(claim.Text, maxInputChars))
	}
	return groups, inputs
}

func documentEmbeddingInput(text string, maxInputChars int) string {
	if utf8.RuneCountInString(text) <= maxInputChars {
		return text
	}
	seen := 0
	for offset := range text {
		if seen == maxInputChars {
			return text[:offset]
		}
		seen++
	}
	return text
}

func flattenWorkerClaims(groups [][]*store.DocumentVectorChunkClaim) []*store.DocumentVectorChunkClaim {
	var claims []*store.DocumentVectorChunkClaim
	for _, group := range groups {
		claims = append(claims, group...)
	}
	return claims
}

func (w *Worker) failClaims(
	ctx context.Context,
	generationID GenerationID,
	claims []*store.DocumentVectorChunkClaim,
	cause error,
	heartbeat *workerClaimHeartbeat,
	result *RunResult,
) error {
	var failureErr error
	for _, claim := range claims {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return errors.Join(failureErr, ctxErr)
		}
		if err := heartbeat.releaseForTransition(claim); err != nil {
			return errors.Join(failureErr, err)
		}
		now := w.deps.Now()
		terminal, code := workerFailureDisposition(cause, claim.AttemptCount, w.deps.MaxAttempts)
		var retryAt *time.Time
		if !terminal {
			deadline := now.Add(w.deps.RetryDelay)
			retryAt = &deadline
		}
		err := w.deps.Ledger.FailDocumentVectorChunk(
			heartbeat.context(), int64(generationID), claim.Token, claim.LeaseOwner, claim.LeaseFence,
			now, retryAt, terminal, code,
		)
		if err != nil {
			failureErr = errors.Join(failureErr, err)
			continue
		}
		if terminal {
			result.Terminal++
		} else {
			result.Retry++
		}
	}
	return failureErr
}

func workerFailureDisposition(cause error, attemptCount, maxAttempts int) (bool, string) {
	if attemptCount >= maxAttempts {
		return true, "attempt_limit"
	}
	switch {
	case errors.Is(cause, errInvalidProviderShape):
		return true, "invalid_provider_shape"
	case errors.Is(cause, errInvalidProviderVector):
		return true, "invalid_provider_vector"
	case errors.Is(cause, vector.ErrPermanent4xx):
		return true, "provider_rejected"
	case errors.Is(cause, errBackendPut):
		return false, "backend_transient"
	default:
		return false, "provider_transient"
	}
}

func validateWorkerProviderPrefix(inputs []vector.DocumentInput, vectors [][][]float32, providerErr error, dimension int) ([][][]float32, error) {
	if len(vectors) > len(inputs) || (providerErr == nil && len(vectors) != len(inputs)) {
		return nil, fmt.Errorf("%w: document count got %d, expected %d", errInvalidProviderShape, len(vectors), len(inputs))
	}
	for documentIndex, documentVectors := range vectors {
		if len(documentVectors) != len(inputs[documentIndex].Chunks) {
			return vectors[:documentIndex], fmt.Errorf("%w: document %d chunk count got %d, expected %d", errInvalidProviderShape, documentIndex, len(documentVectors), len(inputs[documentIndex].Chunks))
		}
		for _, vector := range documentVectors {
			if len(vector) != dimension {
				return vectors[:documentIndex], fmt.Errorf("%w: dimension got %d, expected %d", errInvalidProviderVector, len(vector), dimension)
			}
			var squaredNorm float64
			for _, value := range vector {
				if math.IsNaN(float64(value)) || math.IsInf(float64(value), 0) {
					return vectors[:documentIndex], fmt.Errorf("%w: nonfinite component", errInvalidProviderVector)
				}
				squaredNorm += float64(value) * float64(value)
			}
			if squaredNorm == 0 {
				return vectors[:documentIndex], fmt.Errorf("%w: zero norm", errInvalidProviderVector)
			}
		}
	}
	return vectors, providerErr
}
