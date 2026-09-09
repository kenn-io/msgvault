package sync

import (
	"context"
	"errors"

	"go.kenn.io/msgvault/internal/gmail"
	"go.kenn.io/msgvault/internal/store"
)

// failedRelocationGuards is run-scoped protection for forced relocation
// targets whose adoption failed. A failed target stays a retryable per-item
// failure so unrelated mail keeps ingesting, but ordinary routing in the same
// run must not consume the target's discovery provenance: the old composite
// key is what rediscovers the candidate on the next attempt, and the guarded
// row keeps its archived snapshot until the relocation succeeds. The guards
// live only on the scoped Syncer copy a single full() run uses; nothing is
// persisted, blacklisted, or acknowledged.
type failedRelocationGuards struct {
	// byInternalID guards the archived row itself: no dedup adoption may
	// rekey, relocate, or otherwise mutate it this run.
	byInternalID map[int64]gmail.MessageRelocationTarget
	// bySourceMessageID guards the target's composite keys — the expected
	// old archived key and the failed candidate key — from UID-reuse
	// invalidation and alias routing.
	bySourceMessageID map[string]int64
}

func newFailedRelocationGuards() *failedRelocationGuards {
	return &failedRelocationGuards{
		byInternalID:      map[int64]gmail.MessageRelocationTarget{},
		bySourceMessageID: map[string]int64{},
	}
}

func (g *failedRelocationGuards) empty() bool {
	return len(g.byInternalID) == 0
}

func (g *failedRelocationGuards) add(target gmail.MessageRelocationTarget) {
	g.byInternalID[target.InternalID] = target
	g.bySourceMessageID[target.SourceMessageID] = target.InternalID
	if target.NewSourceMessageID != "" && target.NewSourceMessageID != target.SourceMessageID {
		g.bySourceMessageID[target.NewSourceMessageID] = target.InternalID
	}
}

// guardFailedRelocation records a failed forced target for this run. The
// caller has already recorded the per-item error; the guards only keep
// ordinary routing from destroying the retry provenance.
func (s *Syncer) guardFailedRelocation(target gmail.MessageRelocationTarget) {
	if s.failedRelocationGuards == nil {
		return
	}
	s.failedRelocationGuards.add(target)
}

// relocationGuardsActive reports whether any forced target failed this run.
func (s *Syncer) relocationGuardsActive() bool {
	return s.failedRelocationGuards != nil && !s.failedRelocationGuards.empty()
}

// relocationRowProtected reports whether internalID belongs to a failed
// relocation target, optionally matching one of its composite keys.
func (s *Syncer) relocationRowProtected(internalID int64, sourceMessageID string) bool {
	if s.failedRelocationGuards == nil || internalID <= 0 {
		return false
	}
	if _, guarded := s.failedRelocationGuards.byInternalID[internalID]; guarded {
		return true
	}
	return sourceMessageID != "" &&
		s.failedRelocationGuards.bySourceMessageID[sourceMessageID] == internalID
}

// relocationRowProtectedByID reports whether internalID belongs to a failed
// relocation target regardless of which composite key or RFC822 identity
// routed to it.
func (s *Syncer) relocationRowProtectedByID(internalID int64) bool {
	return s.relocationRowProtected(internalID, "")
}

// relocationCompositeProtected reports whether sourceMessageID is a composite
// key held by a failed relocation target, independently of which archived
// row ordinary routing resolves behind it. Keys stay protected on their own:
// the old key is the provenance the next retry rediscovers the candidate
// with, and the failed candidate key must not be adopted by anything else.
func (s *Syncer) relocationCompositeProtected(sourceMessageID string) bool {
	if s.failedRelocationGuards == nil || sourceMessageID == "" {
		return false
	}
	_, guarded := s.failedRelocationGuards.bySourceMessageID[sourceMessageID]
	return guarded
}

// fatalRelocationError reports whether err must stop the run instead of
// deferring the relocation target: cancellation and sync-generation fencing
// invalidate the run itself, so neither the target nor unrelated mail can
// commit afterwards. Persistence-level SQL failures stay per-item deferrals
// — the run records them as errors, publishes no topology, and retries —
// matching the SQL-failure retry contract.
func fatalRelocationError(err error) bool {
	return errors.Is(err, context.Canceled) ||
		errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, store.ErrSyncRunSuperseded)
}
