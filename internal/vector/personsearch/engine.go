// Package personsearch resolves semantic queries against the person-owned
// vector corpus and returns current durable person roots.
package personsearch

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/vector"
)

// ErrPersonCoverageIncomplete reports that the active generation exists but
// does not yet cover the current curated person corpus. It retains
// ErrIndexBuilding compatibility for existing API and CLI error handling.
var ErrPersonCoverageIncomplete = fmt.Errorf(
	"person corpus coverage incomplete: %w", vector.ErrIndexBuilding,
)

// Coverage is exact person-corpus coverage for one vector generation.
type Coverage struct {
	Mismatched int64
	Rejected   int64
}

// Complete reports whether every current semantic person document has the
// exact revision in the generation and no terminal rejection remains.
func (c Coverage) Complete() bool {
	return c.Mismatched == 0 && c.Rejected == 0
}

// CoverageChecker reads person-only coverage without scanning message or
// contextual-document convergence state.
type CoverageChecker interface {
	CheckPersonCoverage(ctx context.Context, generation vector.GenerationID) (Coverage, error)
}

// CoverageIncompleteError preserves the reason an active generation cannot
// yet serve semantic person search so callers can give correct recovery help.
type CoverageIncompleteError struct {
	Generation vector.GenerationID
	Mismatched int64
	Rejected   int64
}

func (e *CoverageIncompleteError) Error() string {
	return fmt.Sprintf(
		"person corpus coverage incomplete for generation %d (mismatched=%d, rejected=%d): %v",
		e.Generation, e.Mismatched, e.Rejected, ErrPersonCoverageIncomplete,
	)
}

// Unwrap retains errors.Is compatibility with ErrPersonCoverageIncomplete
// and, through it, vector.ErrIndexBuilding.
func (e *CoverageIncompleteError) Unwrap() error { return ErrPersonCoverageIncomplete }

// Backend combines generation lifecycle reads with the separate person-owned
// ANN capability. Search never calls the message-owned Search method.
type Backend interface {
	vector.Backend
	vector.PersonBackend
}

// Store supplies the current semantic revision and durable roots used to
// revalidate and hydrate ANN hits.
type Store interface {
	ResolvePersonSemanticCandidatesContext(
		ctx context.Context, candidates []store.PersonSemanticCandidate,
	) ([]store.Person, error)
}

// QueryEmbedder embeds one free-text query.
type QueryEmbedder interface {
	EmbedQuery(ctx context.Context, text string) ([]float32, error)
}

// Config controls generation compatibility.
type Config struct {
	ExpectedFingerprint string
	// Gate rechecks default-off enablement and exact current consent before
	// generation lookup or query embedding.
	Gate vector.SemanticPersonEmbeddingGate
	// PersonCoverage reports exact active-generation coverage for the current
	// curated person corpus without consulting unrelated message coverage.
	// Nil preserves the standalone engine contract for callers that do not own
	// embedding maintenance.
	PersonCoverage func(context.Context, vector.GenerationID) (Coverage, error)
}

// Result is one ranked, durable person match. Semantic projection text and
// revision details never leave the engine.
type Result struct {
	Person store.Person
	Score  float64
}

// Engine searches the person-owned corpus and fences results against current
// source revisions before returning durable roots.
type Engine struct {
	backend  Backend
	store    Store
	embedder QueryEmbedder
	config   Config
}

// NewEngine constructs a semantic person search engine.
func NewEngine(backend Backend, data Store, embedder QueryEmbedder, config Config) *Engine {
	return &Engine{backend: backend, store: data, embedder: embedder, config: config}
}

// Search embeds query once, searches only the person corpus, drops stale or
// deleted hits, and returns durable person roots in ANN rank order.
func (e *Engine) Search(ctx context.Context, query string, limit int) ([]Result, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, errors.New("empty person search query")
	}
	if limit <= 0 {
		return nil, errors.New("person search limit must be positive")
	}
	if e.config.Gate == nil {
		return nil, errors.New("semantic person search gate is not configured")
	}
	if err := e.config.Gate.Check(ctx); err != nil {
		return nil, err
	}

	active, err := vector.ResolveActiveForFingerprint(
		ctx, e.backend, e.config.ExpectedFingerprint,
	)
	if err != nil {
		return nil, err
	}
	if e.config.PersonCoverage != nil {
		coverage, err := e.config.PersonCoverage(ctx, active.ID)
		if err != nil {
			return nil, fmt.Errorf("check person corpus coverage: %w", err)
		}
		if !coverage.Complete() {
			return nil, &CoverageIncompleteError{
				Generation: active.ID,
				Mismatched: coverage.Mismatched,
				Rejected:   coverage.Rejected,
			}
		}
	}
	queryVector, err := e.embedder.EmbedQuery(ctx, query)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, fmt.Errorf("embed person query: %w: %w", vector.ErrEmbeddingTimeout, err)
		}
		return nil, fmt.Errorf("embed person query: %w", err)
	}
	searchLimit := limit
	seen := make(map[store.PersonSemanticCandidate]struct{})
	valid := make(map[store.PersonSemanticCandidate]struct{})
	for {
		hits, err := e.backend.SearchPeople(ctx, active.ID, queryVector, searchLimit)
		if err != nil {
			return nil, fmt.Errorf("search person corpus: %w", err)
		}

		newCandidates := make([]store.PersonSemanticCandidate, 0, len(hits))
		for _, hit := range hits {
			candidate := store.PersonSemanticCandidate{
				PersonID: hit.PersonID,
				Revision: hit.Revision,
			}
			if _, ok := seen[candidate]; ok {
				continue
			}
			seen[candidate] = struct{}{}
			newCandidates = append(newCandidates, candidate)
		}
		exhausted := len(hits) < searchLimit
		if exhausted {
			newSet := make(map[store.PersonSemanticCandidate]struct{}, len(newCandidates))
			for _, candidate := range newCandidates {
				newSet[candidate] = struct{}{}
			}
			finalCandidates := make([]store.PersonSemanticCandidate, 0, len(valid)+len(newCandidates))
			for _, hit := range hits {
				candidate := store.PersonSemanticCandidate{PersonID: hit.PersonID, Revision: hit.Revision}
				_, wasValid := valid[candidate]
				_, isNew := newSet[candidate]
				if wasValid || isNew {
					finalCandidates = append(finalCandidates, candidate)
				}
			}
			people, err := e.store.ResolvePersonSemanticCandidatesContext(ctx, finalCandidates)
			if err != nil {
				return nil, fmt.Errorf("resolve final person search hits: %w", err)
			}
			return personResultsInHitOrder(hits, people, limit), nil
		}
		hadValidCandidates := len(valid) > 0
		var newlyResolved []store.Person
		if len(newCandidates) > 0 {
			people, err := e.store.ResolvePersonSemanticCandidatesContext(ctx, newCandidates)
			if err != nil {
				return nil, fmt.Errorf("resolve current person search hits: %w", err)
			}
			candidateByID := make(map[int64]store.PersonSemanticCandidate, len(newCandidates))
			for _, candidate := range newCandidates {
				candidateByID[candidate.PersonID] = candidate
			}
			for _, person := range people {
				if candidate, ok := candidateByID[person.ID]; ok {
					valid[candidate] = struct{}{}
				}
			}
			newlyResolved = people
		}

		currentCandidates := make([]store.PersonSemanticCandidate, 0, min(limit, len(valid)))
		for _, hit := range hits {
			candidate := store.PersonSemanticCandidate{PersonID: hit.PersonID, Revision: hit.Revision}
			if _, ok := valid[candidate]; ok {
				currentCandidates = append(currentCandidates, candidate)
			}
		}
		if len(currentCandidates) >= limit {
			if !hadValidCandidates {
				return personResultsInHitOrder(hits, newlyResolved, limit), nil
			}
			people, err := e.store.ResolvePersonSemanticCandidatesContext(ctx, currentCandidates)
			if err != nil {
				return nil, fmt.Errorf("revalidate final person search hits: %w", err)
			}
			results := personResultsInHitOrder(hits, people, limit)
			if len(results) == limit {
				return results, nil
			}
			peopleByID := make(map[int64]struct{}, len(people))
			for _, person := range people {
				peopleByID[person.ID] = struct{}{}
			}
			valid = make(map[store.PersonSemanticCandidate]struct{}, len(people))
			for _, candidate := range currentCandidates {
				if _, ok := peopleByID[candidate.PersonID]; ok {
					valid[candidate] = struct{}{}
				}
			}
		}
		next, ok := widenSearchLimit(searchLimit)
		if !ok {
			return nil, nil
		}
		searchLimit = next
	}
}

func personResultsInHitOrder(hits []vector.PersonHit, people []store.Person, limit int) []Result {
	peopleByID := make(map[int64]store.Person, len(people))
	for _, person := range people {
		peopleByID[person.ID] = person
	}
	results := make([]Result, 0, min(limit, len(people)))
	for _, hit := range hits {
		person, ok := peopleByID[hit.PersonID]
		if !ok {
			continue
		}
		results = append(results, Result{Person: person, Score: hit.Score})
		if len(results) == limit {
			break
		}
	}
	return results
}

func widenSearchLimit(current int) (int, bool) {
	maxInt := int(^uint(0) >> 1)
	if current >= maxInt {
		return current, false
	}
	if current > maxInt/2 {
		return maxInt, true
	}
	return current * 2, true
}
