package hybrid

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.kenn.io/msgvault/internal/vector"
	"go.kenn.io/msgvault/internal/vector/rerank"
)

// ErrRerankNotConfigured is returned when a request asks for reranking but
// the engine has no rerank stage ([vector.rerank] is disabled).
var ErrRerankNotConfigured = errors.New("reranking is not configured")

// FailureCandidateText is the fallback category used when the candidate
// text could not be loaded from the archive.
const FailureCandidateText = "candidate_text"

// CandidateTexts returns the text a reranker scores for each message ID, in
// the order of ids. It returns "" for a message that has no text or no
// longer exists.
type CandidateTexts func(ctx context.Context, ids []int64) ([]string, error)

// RerankStage reorders the head of a retrieval result with a reranker.
// Message text leaves the daemon only through this stage, and only when a
// request asks for it.
type RerankStage struct {
	Reranker rerank.Reranker
	Texts    CandidateTexts
	// Model names the provider model for response metadata.
	Model string
	// Candidates is how many top retrieval hits are rescored, at most
	// rerank.MaxCandidates. Hits below this window keep their retrieval
	// order after the reranked ones.
	Candidates int
}

// RerankMeta describes what the rerank stage did for one search.
type RerankMeta struct {
	Requested bool
	// Applied is true when the returned order came from the reranker.
	Applied    bool
	Model      string
	Candidates int
	Duration   time.Duration
	// Fallback is the caller-safe failure category when reranking failed and
	// the retrieval order was returned instead. Empty on success.
	Fallback string
	// Err is the underlying failure for the daemon log. It may contain the
	// provider's own error message, so it must not be sent to a caller.
	Err error
}

// RerankAvailable reports whether the engine has a rerank stage.
func (e *Engine) RerankAvailable() bool { return e != nil && e.cfg.Rerank != nil }

// applyRerank rescores the first stage.Candidates hits. A provider failure
// leaves the retrieval order in place and records the fallback in meta; only
// cancellation of the caller's own context is returned as an error, because
// then there is no caller left to answer.
func (e *Engine) applyRerank(
	ctx context.Context, query string, hits []vector.FusedHit, limit int, meta ResultMeta,
) ([]vector.FusedHit, ResultMeta, error) {
	stage := e.cfg.Rerank
	meta.Rerank = RerankMeta{Requested: true, Model: stage.Model}
	started := time.Now()
	reordered, applied, err := stage.rerank(ctx, query, hits)
	meta.Rerank.Duration = time.Since(started)
	switch {
	case err != nil && ctx.Err() != nil:
		return nil, ResultMeta{}, fmt.Errorf("rerank: %w", ctx.Err())
	case err != nil:
		meta.Rerank.Fallback = failureCategory(err)
		meta.Rerank.Err = err
	default:
		hits = reordered
		meta.Rerank.Applied = true
		meta.Rerank.Candidates = applied
	}
	if len(hits) > limit {
		hits = hits[:limit]
		// The engine fetched a wider window for the reranker, so hits exist
		// past the page the caller asked for.
		meta.PoolSaturated = true
	}
	meta.ReturnedCount = len(hits)
	return hits, meta, nil
}

func failureCategory(err error) string {
	if _, ok := errors.AsType[candidateTextError](err); ok {
		return FailureCandidateText
	}
	return rerank.FailureCategory(err)
}

type candidateTextError struct{ err error }

func (e candidateTextError) Error() string { return "load candidate text: " + e.err.Error() }
func (e candidateTextError) Unwrap() error { return e.err }

// rerank returns the reordered hit list and how many hits the provider
// scored. Candidates with no text are not sent; they follow the scored ones
// in retrieval order, ahead of the hits outside the window.
func (s *RerankStage) rerank(ctx context.Context, query string, hits []vector.FusedHit) ([]vector.FusedHit, int, error) {
	window := min(len(hits), s.Candidates, rerank.MaxCandidates)
	if window == 0 {
		return hits, 0, nil
	}
	ids := make([]int64, window)
	for i := range window {
		ids[i] = hits[i].MessageID
	}
	texts, err := s.Texts(ctx, ids)
	if err != nil {
		return nil, 0, candidateTextError{err: err}
	}
	if len(texts) != window {
		return nil, 0, candidateTextError{err: fmt.Errorf("loaded %d texts for %d candidates", len(texts), window)}
	}
	scored := make([]int, 0, window)
	candidates := make([]string, 0, window)
	var unscored []int
	for i, text := range texts {
		if text == "" {
			unscored = append(unscored, i)
			continue
		}
		scored = append(scored, i)
		candidates = append(candidates, text)
	}
	if len(candidates) == 0 {
		return hits, 0, nil
	}
	result, err := s.Reranker.Rerank(ctx, rerank.Request{Query: query, Candidates: candidates})
	if err != nil {
		return nil, 0, err
	}
	if len(result.Scores) != len(candidates) {
		return nil, 0, &rerank.Error{Category: rerank.FailureInvalidResponse}
	}
	order, err := rerank.Order(result.Scores)
	if err != nil {
		return nil, 0, &rerank.Error{Category: rerank.FailureInvalidResponse}
	}
	out := make([]vector.FusedHit, 0, len(hits))
	for _, position := range order {
		hit := hits[scored[position]]
		score := result.Scores[position]
		hit.RerankScore = &score
		out = append(out, hit)
	}
	for _, i := range unscored {
		out = append(out, hits[i])
	}
	out = append(out, hits[window:]...)
	return out, len(candidates), nil
}
