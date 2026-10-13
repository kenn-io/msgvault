package api

import (
	"context"
	"time"

	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/vector"
	"go.kenn.io/msgvault/internal/vector/hybrid"
)

type searchMetadataProvider interface {
	GetSearchMetadata(ctx context.Context, ids []int64, q *search.Query, includeSnippet bool) (map[int64]store.SearchMetadata, error)
}

func (s *Server) searchMetadata(ctx context.Context, ids []int64, q *search.Query, includeSnippet bool) (map[int64]store.SearchMetadata, error) {
	provider, ok := s.store.(searchMetadataProvider)
	if !ok || len(ids) == 0 {
		return map[int64]store.SearchMetadata{}, nil
	}
	return provider.GetSearchMetadata(ctx, ids, q, includeSnippet)
}

func resultIDs(results []query.MessageSummary) []int64 {
	ids := make([]int64, len(results))
	for i, msg := range results {
		ids[i] = msg.ID
	}
	return ids
}

// A suggestion must never make an otherwise successful search fail or embed text.
func (s *Server) hybridHintAvailable(ctx context.Context, messageTypes []string) bool {
	// Read the cached status; refreshing here would spend the shared revalidation throttle on a hint.
	ctx, cancel := context.WithTimeout(ctx, 250*time.Millisecond)
	defer cancel()
	s.vectorMu.RLock()
	ready := s.vectorStatus == VectorStatusReady && !s.vectorStaleLatch
	engine, backend, cfg := s.hybridEngine, s.backend, s.vectorCfg
	s.vectorMu.RUnlock()
	if !ready || engine == nil || backend == nil {
		return false
	}
	if hybrid.ValidateBuildScope(cfg.Embed.Scope.BuildScope(), vector.Filter{MessageTypes: messageTypes}) != nil {
		return false
	}
	_, err := vector.ResolveActiveForFingerprint(ctx, backend, cfg.GenerationFingerprint())
	return err == nil
}
