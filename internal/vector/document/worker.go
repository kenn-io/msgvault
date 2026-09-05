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

	docbankdocument "go.kenn.io/docbank/document"
	docembedding "go.kenn.io/docbank/document/embedding"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/vector"
)

const (
	maxWorkerRunLimit   = 1000
	maxWorkerRetryDelay = 7 * 24 * time.Hour
)

var (
	errInvalidProviderShape  = vector.ErrInvalidProviderShape
	errInvalidProviderVector = vector.ErrInvalidProviderVector
	errInputPreparation      = errors.New("document embedding input preparation failed")
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
	Recipe            docembedding.Recipe
	// ContextualDocuments keeps each durable extraction chunk in its own stable
	// provider document. A worker batch must never redefine contextual scope.
	ContextualDocuments bool
	// AfterGenerationID and AfterChunkID restore the bounded scan cursor reported
	// by a prior run of the same generation. Task 6b may persist this pair; Worker
	// also carries it across sequential runs and resets it when generations change.
	AfterGenerationID GenerationID
	AfterChunkID      int64
	Now               func() time.Time
	prepareInputs     func(context.Context, Ledger, docembedding.Recipe, []*store.DocumentVectorChunkClaim) (map[string]string, error)
}

// RunResult reports only locally observable accounting. Provider token usage
// is deliberately absent because EmbedDocuments does not report it.
type RunResult struct {
	Attempted int `json:"attempted"`
	Succeeded int `json:"succeeded"`
	Failed    int `json:"failed"`

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
	if deps.Recipe.Fingerprint() == "" && deps.MaxInputChars > 0 {
		deps.Recipe, _ = docembedding.NewRecipe(docembedding.RecipeConfig{
			Mode: docembedding.RepresentationRaw, MaxInputRunes: deps.MaxInputChars,
		})
	}
	if deps.prepareInputs == nil {
		deps.prepareInputs = prepareDocbankClaimInputs
	}
	return &Worker{
		deps:               deps,
		cursorGenerationID: deps.AfterGenerationID,
		afterChunkID:       deps.AfterChunkID,
	}
}

func (w *Worker) Run(ctx context.Context, generationID GenerationID, limit int) (result RunResult, runErr error) {
	defer result.finalizeOutcomes()
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
	heartbeat := newWorkerClaimHeartbeat(ctx, w, generationID, claims)
	defer heartbeat.stop()
	preparedTexts := make(map[string]string, len(claims))
	preparedClaims := make([]*store.DocumentVectorChunkClaim, 0, len(claims))
	for _, claimGroup := range groupWorkerClaimsByExtraction(claims) {
		groupTexts, prepareErr := w.deps.prepareInputs(
			heartbeat.context(), w.deps.Ledger, w.deps.Recipe, claimGroup,
		)
		if prepareErr != nil {
			cause := fmt.Errorf("%w: extraction %q: %w", errInputPreparation, claimGroup[0].ExtractionID, prepareErr)
			failureErr := w.failClaims(ctx, generationID, claimGroup, cause, heartbeat, &result)
			runErr = errors.Join(runErr, cause, failureErr)
			continue
		}
		for _, claim := range claimGroup {
			text, ok := groupTexts[claim.Token]
			if !ok {
				cause := fmt.Errorf("%w: extraction %q omitted token %q", errInputPreparation, claim.ExtractionID, claim.Token)
				failureErr := w.failClaims(ctx, generationID, []*store.DocumentVectorChunkClaim{claim}, cause, heartbeat, &result)
				runErr = errors.Join(runErr, cause, failureErr)
				continue
			}
			preparedTexts[claim.Token] = text
			preparedClaims = append(preparedClaims, claim)
		}
	}
	if len(preparedClaims) == 0 {
		return result, errors.Join(runErr, heartbeat.err())
	}
	groups, inputs := groupWorkerClaims(preparedClaims, preparedTexts, w.deps.ContextualDocuments)
	result.ProviderCalls = 1
	result.ProviderDocuments = len(inputs)
	for _, input := range inputs {
		result.ProviderChunks += len(input.Chunks)
		for _, text := range input.Chunks {
			result.ProviderInputChars += utf8.RuneCountInString(text)
		}
	}

	vectors, providerErr := w.deps.Provider.EmbedDocuments(heartbeat.context(), inputs)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return result, ctxErr
	}
	if heartbeatErr := heartbeat.err(); heartbeatErr != nil {
		return result, heartbeatErr
	}
	outcomes, responseErr := validateWorkerProviderDocuments(inputs, vectors, providerErr, w.deps.Dimension)
	var embeddings []Embedding
	var completedClaims []*store.DocumentVectorChunkClaim
	for documentIndex, outcome := range outcomes {
		if outcome.err != nil {
			continue
		}
		for chunkIndex, vector := range outcome.vectors {
			claim := groups[documentIndex][chunkIndex]
			embeddings = append(embeddings, Embedding{Token: claim.Token, Vector: vector})
			completedClaims = append(completedClaims, claim)
		}
	}
	result.Embedded = len(completedClaims)
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
	for documentIndex, outcome := range outcomes {
		if outcome.err == nil {
			continue
		}
		failureErr := w.failClaims(ctx, generationID, groups[documentIndex], outcome.err, heartbeat, &result)
		runErr = errors.Join(runErr, outcome.err, failureErr)
	}
	runErr = errors.Join(runErr, responseErr)
	runErr = errors.Join(runErr, heartbeat.err())
	return result, runErr
}

// finalizeOutcomes derives public pass accounting only from durable chunk
// decisions. Claims released or lost before a publication/failure transition
// do not count as attempts, and provider retry mechanics cannot multiply one
// chunk into several public failures.
func (r *RunResult) finalizeOutcomes() {
	r.Succeeded = r.Published
	r.Failed = r.Retry + r.Terminal + r.SourceChanged
	r.Attempted = r.Succeeded + r.Failed
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
	if w.deps.Recipe.Fingerprint() == "" || w.deps.Recipe.Values().Mode != docembedding.RepresentationRaw {
		return errors.New("document vector worker requires a valid raw Docbank embedding recipe")
	}
	return nil
}

type normalizedDocumentLedger interface {
	LoadNormalizedDocument(ctx context.Context, extractionID string) (docbankdocument.NormalizedDocument, error)
}

func prepareDocbankClaimInputs(
	ctx context.Context, ledger Ledger, recipe docembedding.Recipe,
	claims []*store.DocumentVectorChunkClaim,
) (map[string]string, error) {
	source, ok := ledger.(normalizedDocumentLedger)
	if !ok {
		return nil, errors.New("document vector ledger cannot load normalized documents")
	}
	inputsByToken := make(map[string]string, len(claims))
	plans := make(map[string]map[string]docembedding.EmbeddingInput)
	for _, claim := range claims {
		byChunk := plans[claim.ExtractionID]
		if byChunk == nil {
			normalized, err := source.LoadNormalizedDocument(ctx, claim.ExtractionID)
			if err != nil {
				return nil, fmt.Errorf("load normalized document for embedding: %w", err)
			}
			plan, err := docembedding.BuildEmbeddingPlan(
				normalized, docembedding.DocumentContext{}, recipe, nil,
			)
			if err != nil {
				return nil, fmt.Errorf("build document embedding plan: %w", err)
			}
			byChunk = make(map[string]docembedding.EmbeddingInput, len(plan.Inputs))
			for _, input := range plan.Inputs {
				if input.Kind != docembedding.RepresentationKindRaw || len(input.SourceRefs) != 1 {
					return nil, errors.New("raw document embedding plan returned an invalid input")
				}
				byChunk[input.SourceRefs[0].ChunkKey] = input
			}
			plans[claim.ExtractionID] = byChunk
		}
		input, ok := byChunk[claim.ChunkKey]
		if !ok || len(input.SourceRefs) != 1 || input.SourceRefs[0].ChunkChecksum != claim.ChunkChecksum {
			return nil, fmt.Errorf("document embedding plan does not match claimed chunk %q", claim.ChunkKey)
		}
		inputsByToken[claim.Token] = input.Text
	}
	return inputsByToken, nil
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

func groupWorkerClaims(
	claims []*store.DocumentVectorChunkClaim, preparedTexts map[string]string, contextualDocuments bool,
) ([][]*store.DocumentVectorChunkClaim, []vector.DocumentInput) {
	if contextualDocuments {
		groups := make([][]*store.DocumentVectorChunkClaim, len(claims))
		inputs := make([]vector.DocumentInput, len(claims))
		for index, claim := range claims {
			groups[index] = []*store.DocumentVectorChunkClaim{claim}
			inputs[index] = vector.DocumentInput{Chunks: []string{preparedTexts[claim.Token]}}
		}
		return groups, inputs
	}
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
		inputs[index].Chunks = append(inputs[index].Chunks, preparedTexts[claim.Token])
	}
	return groups, inputs
}

func groupWorkerClaimsByExtraction(claims []*store.DocumentVectorChunkClaim) [][]*store.DocumentVectorChunkClaim {
	groups := make([][]*store.DocumentVectorChunkClaim, 0)
	indices := make(map[string]int)
	for _, claim := range claims {
		index, ok := indices[claim.ExtractionID]
		if !ok {
			index = len(groups)
			indices[claim.ExtractionID] = index
			groups = append(groups, nil)
		}
		groups[index] = append(groups[index], claim)
	}
	return groups
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
	case errors.Is(cause, errInputPreparation):
		return false, "input_preparation"
	case errors.Is(cause, errBackendPut):
		return false, "backend_transient"
	default:
		return false, "provider_transient"
	}
}

type workerProviderDocumentOutcome struct {
	vectors [][]float32
	err     error
}

func validateWorkerProviderDocuments(
	inputs []vector.DocumentInput, vectors [][][]float32, providerErr error, dimension int,
) ([]workerProviderDocumentOutcome, error) {
	outcomes := make([]workerProviderDocumentOutcome, len(inputs))
	var responseErr error
	if len(vectors) > len(inputs) {
		responseErr = fmt.Errorf("%w: document count got %d, expected at most %d", errInvalidProviderShape, len(vectors), len(inputs))
	}
	for documentIndex := range inputs {
		if documentIndex >= len(vectors) {
			if providerErr != nil {
				outcomes[documentIndex].err = providerErr
			} else {
				outcomes[documentIndex].err = fmt.Errorf(
					"%w: document count got %d, expected %d", errInvalidProviderShape, len(vectors), len(inputs),
				)
			}
			continue
		}
		documentVectors := vectors[documentIndex]
		if len(documentVectors) != len(inputs[documentIndex].Chunks) {
			outcomes[documentIndex].err = fmt.Errorf(
				"%w: document %d chunk count got %d, expected %d",
				errInvalidProviderShape, documentIndex, len(documentVectors), len(inputs[documentIndex].Chunks),
			)
			continue
		}
		for _, embedding := range documentVectors {
			if vectorErr := validateWorkerProviderVector(embedding, dimension); vectorErr != nil {
				outcomes[documentIndex].err = vectorErr
				break
			}
		}
		if outcomes[documentIndex].err == nil {
			outcomes[documentIndex].vectors = documentVectors
		}
	}
	return outcomes, errors.Join(responseErr, providerErr)
}

func validateWorkerProviderVector(embedding []float32, dimension int) error {
	if len(embedding) != dimension {
		return fmt.Errorf("%w: dimension got %d, expected %d", errInvalidProviderVector, len(embedding), dimension)
	}
	var squaredNorm float64
	for _, value := range embedding {
		if math.IsNaN(float64(value)) || math.IsInf(float64(value), 0) {
			return fmt.Errorf("%w: nonfinite component", errInvalidProviderVector)
		}
		squaredNorm += float64(value) * float64(value)
	}
	if squaredNorm == 0 {
		return fmt.Errorf("%w: zero norm", errInvalidProviderVector)
	}
	return nil
}
