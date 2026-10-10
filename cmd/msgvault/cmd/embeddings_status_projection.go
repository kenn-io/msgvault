package cmd

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"go.kenn.io/msgvault/internal/operations"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/vector"
)

// embeddingCoverageCacheTTL bounds how stale status coverage counts can be.
// Diagnostics are read fresh on every request.
const embeddingCoverageCacheTTL = 5 * time.Second

// embeddingCoverageCache reuses coverage counts briefly, so frequent status
// polls do not each scan every live message.
type embeddingCoverageCache struct {
	mu                         sync.Mutex
	key                        string
	expires                    time.Time
	eligible, current, pending int64
}

func (c *embeddingCoverageCache) counts(ctx context.Context, st *store.Store, gen int64, messageTypes []string, sourceIDs []int64) (eligible, current, pending int64, err error) {
	if c == nil {
		eligible, current, _, pending, err = st.CoverageCountsScoped(ctx, gen, messageTypes, sourceIDs)
		return eligible, current, pending, err
	}
	key := fmt.Sprint(gen, messageTypes, sourceIDs)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.key == key && time.Now().Before(c.expires) {
		return c.eligible, c.current, c.pending, nil
	}
	eligible, current, _, pending, err = st.CoverageCountsScoped(ctx, gen, messageTypes, sourceIDs)
	if err != nil {
		return 0, 0, 0, err
	}
	c.key, c.expires = key, time.Now().Add(embeddingCoverageCacheTTL)
	c.eligible, c.current, c.pending = eligible, current, pending
	return eligible, current, pending, nil
}

// readEmbeddingStatus reads coverage and the latest batch diagnostics for the
// generation being built, or the active one. The API handler adds liveness.
// A nil cache reads coverage counts on every call.
func readEmbeddingStatus(ctx context.Context, st *store.Store, backend vector.Backend, cfg vector.Config, sourceID int64, cache *embeddingCoverageCache) (vector.EmbeddingStatus, error) {
	status := vector.EmbeddingStatus{Generation: vector.EmbeddingGenerationStatus{State: "not_initialized"}}
	active, activeErr := backend.ActiveGeneration(ctx)
	if activeErr != nil && !errors.Is(activeErr, vector.ErrNoActiveGeneration) {
		return status, activeErr
	}
	building, err := backend.BuildingGeneration(ctx)
	if err != nil {
		return status, err
	}
	target := active
	if building != nil && building.Fingerprint == cfg.GenerationFingerprint() {
		target = *building
	}
	scope := cfg.Embed.Scope.BuildScope()
	if sourceID == 0 || len(scope.SourceIDs) == 0 || slices.Contains(scope.SourceIDs, sourceID) {
		if sourceID > 0 {
			scope.SourceIDs = []int64{sourceID}
		}
		status.Eligible, status.Current, status.Pending, err = cache.counts(ctx, st, int64(target.ID), scope.MessageTypes, scope.SourceIDs)
		if err != nil {
			return status, err
		}
	}
	lane, err := st.LaneStatus(ctx, operations.KindMessageEmbedding)
	if err != nil {
		return status, err
	}
	if lane.Active != nil {
		status.ActiveRunID, _ = lane.Active.ID.Int64()
		status.ActiveRunStartedAt = &lane.Active.StartedAt
	}
	if run := lane.Latest; run != nil {
		status.LatestRunStartedAt = &run.StartedAt
		for _, counter := range run.Counters {
			if counter.Name == operations.CounterFailed {
				status.Failed = counter.Value
			}
		}
	}
	if target.ID == 0 {
		return status, nil
	}
	status.Generation = vector.EmbeddingGenerationView(target)
	if activeErr == nil {
		activeView := vector.EmbeddingGenerationView(active)
		status.ActiveGeneration = &activeView
	}
	if target.Fingerprint != cfg.GenerationFingerprint() {
		return status, nil
	}
	d, err := st.ReadEmbeddingDiagnostics(ctx, int64(target.ID))
	if err != nil {
		return status, err
	}
	if d == nil {
		return status, nil
	}
	status.Diagnostics = d
	return status, nil
}
