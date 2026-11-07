package cmd

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	"go.kenn.io/msgvault/internal/operations"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/vector"
	"golang.org/x/sync/singleflight"
)

// embeddingCoverageCacheTTL is how long counts are served without a refresh.
// Diagnostics are read fresh on every request.
const embeddingCoverageCacheTTL = vector.EmbeddingCoverageMaxAge

// embeddingCoverageStaleLimit is how long older counts are served while a
// slow refresh runs, so a slow archive still answers every poll.
const embeddingCoverageStaleLimit = 5 * time.Minute

// embeddingCoverageRefreshWait is how long a poll holding older counts waits
// for the refresh before serving them, so a fast archive answers fresh.
const embeddingCoverageRefreshWait = 2 * time.Second

// embeddingCoverageCountTimeout bounds one count. It outlives a status
// request, so a count slower than the request still serves later polls.
const embeddingCoverageCountTimeout = 2 * time.Minute

type embeddingCoverage struct {
	eligible, current, pending int64
	countedAt                  time.Time
}

// embeddingCoverageCache keeps recent counts per scope and runs at most one
// count per scope at a time, so frequent status polls do not each scan every
// live message. Counts end with base, the daemon's lifetime.
type embeddingCoverageCache struct {
	base    context.Context
	log     *slog.Logger
	group   singleflight.Group
	mu      sync.Mutex
	entries map[string]embeddingCoverage
}

func newEmbeddingCoverageCache(base context.Context, log *slog.Logger) *embeddingCoverageCache {
	if log == nil {
		log = slog.Default()
	}
	return &embeddingCoverageCache{base: base, log: log, entries: make(map[string]embeddingCoverage)}
}

func (c *embeddingCoverageCache) counts(ctx context.Context, st *store.Store, gen int64, messageTypes []string, sourceIDs []int64) (embeddingCoverage, error) {
	key := fmt.Sprint(gen, messageTypes, sourceIDs)
	c.mu.Lock()
	entry, cached := c.entries[key]
	c.mu.Unlock()
	age := time.Since(entry.countedAt)
	if cached && age < embeddingCoverageCacheTTL {
		return entry, nil
	}
	result := c.group.DoChan(key, func() (any, error) {
		return c.count(key, st, gen, messageTypes, sourceIDs)
	})
	if err := ctx.Err(); err != nil {
		return embeddingCoverage{}, fmt.Errorf("wait for embedding coverage count: %w", err)
	}
	var stale <-chan time.Time
	if cached && age < embeddingCoverageStaleLimit {
		timer := time.NewTimer(embeddingCoverageRefreshWait)
		defer timer.Stop()
		stale = timer.C
	}
	select {
	case counted := <-result:
		if counted.Err == nil {
			coverage, _ := counted.Val.(embeddingCoverage)
			return coverage, nil
		}
		if stale != nil {
			return entry, nil
		}
		return embeddingCoverage{}, counted.Err
	case <-stale:
		return entry, nil
	case <-ctx.Done():
		if stale != nil {
			return entry, nil
		}
		return embeddingCoverage{}, fmt.Errorf("wait for embedding coverage count: %w", ctx.Err())
	}
}

func (c *embeddingCoverageCache) count(key string, st *store.Store, gen int64, messageTypes []string, sourceIDs []int64) (embeddingCoverage, error) {
	ctx, cancel := context.WithTimeout(c.base, embeddingCoverageCountTimeout)
	defer cancel()
	// The counts describe the archive when the count began. The count is
	// bounded by ctx, not PostgreSQL's shorter pool-wide statement timeout.
	started := time.Now().UTC()
	eligible, current, _, pending, err := st.CoverageCountsScopedUnbounded(ctx, gen, messageTypes, sourceIDs)
	if err != nil {
		// A poll serving older counts does not see this error.
		c.log.Warn("embedding status coverage count failed", "error", err)
		return embeddingCoverage{}, fmt.Errorf("count embedding coverage: %w", err)
	}
	coverage := embeddingCoverage{eligible, current, pending, started}
	now := started
	c.mu.Lock()
	defer c.mu.Unlock()
	for k, old := range c.entries {
		if now.Sub(old.countedAt) > embeddingCoverageStaleLimit {
			delete(c.entries, k)
		}
	}
	c.entries[key] = coverage
	return coverage, nil
}

// embeddingStatusSource reads archive coverage and generation diagnostics.
type embeddingStatusSource struct {
	st       *store.Store
	backend  vector.Backend
	cfg      vector.Config
	coverage *embeddingCoverageCache
}

// read returns coverage and the latest batch diagnostics for the generation
// being built, or the active one. The API handler adds liveness.
func (src embeddingStatusSource) read(ctx context.Context, sourceID int64) (vector.EmbeddingStatus, error) {
	st, backend, cfg := src.st, src.backend, src.cfg
	status := vector.EmbeddingStatus{Generation: vector.EmbeddingGenerationStatus{State: "not_initialized"}, CoverageSampledAt: time.Now().UTC()}
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
	if sourceID > 0 {
		if _, err := st.GetSourceByIDContext(ctx, sourceID); err != nil {
			if errors.Is(err, store.ErrSourceNotFound) {
				return status, vector.ErrEmbeddingStatusUnknownSource
			}
			return status, err
		}
		if len(scope.SourceIDs) != 0 && !slices.Contains(scope.SourceIDs, sourceID) {
			return status, vector.ErrEmbeddingStatusSourceOutOfScope
		}
		scope.SourceIDs = []int64{sourceID}
	}
	coverage, err := src.coverage.counts(ctx, st, int64(target.ID), scope.MessageTypes, scope.SourceIDs)
	if err != nil {
		return status, err
	}
	status.Eligible, status.Current, status.Pending = coverage.eligible, coverage.current, coverage.pending
	status.CoverageSampledAt = coverage.countedAt
	if err := setEmbeddingRunStatus(ctx, st, &status); err != nil {
		return status, err
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
	status.Diagnostics, err = st.ReadEmbeddingDiagnostics(ctx, int64(target.ID))
	if err != nil {
		src.coverage.log.Warn("read embedding diagnostics", "error", err)
		status.Diagnostics = nil
	}
	if status.Diagnostics != nil && status.Diagnostics.GenerationFingerprint != target.Fingerprint {
		status.Diagnostics = nil
	}
	return status, nil
}

// setEmbeddingRunStatus adds the active and latest message-embedding runs,
// which status uses to decide liveness and which failures to report.
func setEmbeddingRunStatus(ctx context.Context, st *store.Store, status *vector.EmbeddingStatus) error {
	lane, err := st.LaneStatus(ctx, operations.KindMessageEmbedding)
	if err != nil {
		return err
	}
	if lane.Active != nil {
		id, ok := lane.Active.ID.Int64()
		if !ok {
			return fmt.Errorf("active message embedding run %v has no integer id", lane.Active.ID)
		}
		status.ActiveRunID, status.ActiveRunStartedAt = id, &lane.Active.StartedAt
	}
	if run := lane.Latest; run != nil {
		status.LatestRunStartedAt = &run.StartedAt
		for _, counter := range run.Counters {
			if counter.Name == operations.CounterFailed {
				status.Failed = counter.Value
			}
		}
	}
	return nil
}
