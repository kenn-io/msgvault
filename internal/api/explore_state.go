package api

import (
	"crypto/rand"
	"encoding/base64"
	"maps"
	"sync"
	"time"
)

const (
	exploreOperationTokenTTL    = 5 * time.Minute
	exploreCandidateSnapshotTTL = 5 * time.Minute
	exploreMatchCountTTL        = 2 * time.Minute
	exploreCoverageTTL          = 30 * time.Second
	exploreStateMaxEntries      = 128
)

type exploreCandidateHit struct {
	MessageID int64
	Score     float64
	Excerpt   string
}

type exploreCandidateSnapshot struct {
	RequestHash     string
	IDs             []int64
	LexicalIDs      []int64
	Hits            []exploreCandidateHit
	Generation      int64
	LexicalRevision string
	PoolSaturated   bool
	ExpiresAt       time.Time
}

type exploreMatchCountEntry struct {
	Counts    map[string]int64
	ExpiresAt time.Time
}

// exploreCoverageEntry caches one computed semantic-coverage readout. The
// cache key folds the canonical coverage context, the analytical cache
// revision, and the vector generation identity, so an entry can only be
// served while both sides of the intersection are unchanged; the TTL bounds
// staleness of in-place index fills that do not touch the generation row.
type exploreCoverageEntry struct {
	EligibleCount int64
	EmbeddedCount int64
	ExpiresAt     time.Time
}

type exploreOperationGrant struct {
	SelectionHash string
	Count         int64
	Revision      string
	ExpiresAt     time.Time
	// Reserved marks the grant as claimed by an in-flight operation:
	// concurrent requests with the same token fail immediately, while a
	// rollback after a failed operation restores the grant for retry.
	Reserved bool
}

type exploreServerState struct {
	mu          sync.Mutex
	now         func() time.Time
	operations  map[string]exploreOperationGrant
	snapshots   map[string]exploreCandidateSnapshot
	matchCounts map[string]exploreMatchCountEntry
	coverage    map[string]exploreCoverageEntry
}

func newExploreServerState(now func() time.Time) *exploreServerState {
	if now == nil {
		now = time.Now
	}
	return &exploreServerState{
		now: now, operations: make(map[string]exploreOperationGrant),
		snapshots: make(map[string]exploreCandidateSnapshot), matchCounts: make(map[string]exploreMatchCountEntry),
		coverage: make(map[string]exploreCoverageEntry),
	}
}

func cloneExploreIDs(ids []int64) []int64 {
	if ids == nil {
		return nil
	}
	return append(make([]int64, 0, len(ids)), ids...)
}

func (s *exploreServerState) issueSnapshot(snapshot exploreCandidateSnapshot) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	s.pruneLocked(now)
	s.evictSnapshotsLocked()
	token := randomExploreToken()
	snapshot.IDs = cloneExploreIDs(snapshot.IDs)
	snapshot.LexicalIDs = cloneExploreIDs(snapshot.LexicalIDs)
	snapshot.Hits = append([]exploreCandidateHit(nil), snapshot.Hits...)
	snapshot.ExpiresAt = now.Add(exploreCandidateSnapshotTTL)
	s.snapshots[token] = snapshot
	return token
}

func (s *exploreServerState) snapshot(token, requestHash string) (exploreCandidateSnapshot, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	s.pruneLocked(now)
	snapshot, ok := s.snapshots[token]
	if !ok || snapshot.RequestHash != requestHash {
		return exploreCandidateSnapshot{}, false
	}
	snapshot.IDs = cloneExploreIDs(snapshot.IDs)
	snapshot.LexicalIDs = cloneExploreIDs(snapshot.LexicalIDs)
	snapshot.Hits = append([]exploreCandidateHit(nil), snapshot.Hits...)
	return snapshot, true
}

func (s *exploreServerState) getMatchCounts(key string) (map[string]int64, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked(s.now())
	entry, ok := s.matchCounts[key]
	if !ok {
		return nil, false
	}
	return cloneExploreCounts(entry.Counts), true
}

func (s *exploreServerState) putMatchCounts(key string, counts map[string]int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	s.pruneLocked(now)
	for len(s.matchCounts) >= exploreStateMaxEntries {
		var oldestKey string
		var oldest time.Time
		for candidate, entry := range s.matchCounts {
			if oldestKey == "" || entry.ExpiresAt.Before(oldest) {
				oldestKey, oldest = candidate, entry.ExpiresAt
			}
		}
		delete(s.matchCounts, oldestKey)
	}
	s.matchCounts[key] = exploreMatchCountEntry{Counts: cloneExploreCounts(counts), ExpiresAt: now.Add(exploreMatchCountTTL)}
}

func (s *exploreServerState) pruneLocked(now time.Time) {
	for token, grant := range s.operations {
		if !grant.ExpiresAt.After(now) {
			delete(s.operations, token)
		}
	}
	for token, snapshot := range s.snapshots {
		if !snapshot.ExpiresAt.After(now) {
			delete(s.snapshots, token)
		}
	}
	for key, entry := range s.matchCounts {
		if !entry.ExpiresAt.After(now) {
			delete(s.matchCounts, key)
		}
	}
	for key, entry := range s.coverage {
		if !entry.ExpiresAt.After(now) {
			delete(s.coverage, key)
		}
	}
}

func (s *exploreServerState) getCoverage(key string) (exploreCoverageEntry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked(s.now())
	entry, ok := s.coverage[key]
	return entry, ok
}

func (s *exploreServerState) putCoverage(key string, eligible, embedded int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	s.pruneLocked(now)
	for len(s.coverage) >= exploreStateMaxEntries {
		var oldestKey string
		var oldest time.Time
		for candidate, entry := range s.coverage {
			if oldestKey == "" || entry.ExpiresAt.Before(oldest) {
				oldestKey, oldest = candidate, entry.ExpiresAt
			}
		}
		delete(s.coverage, oldestKey)
	}
	s.coverage[key] = exploreCoverageEntry{
		EligibleCount: eligible, EmbeddedCount: embedded, ExpiresAt: now.Add(exploreCoverageTTL),
	}
}

func (s *exploreServerState) evictSnapshotsLocked() {
	for len(s.snapshots) >= exploreStateMaxEntries {
		var oldestToken string
		var oldest time.Time
		for token, snapshot := range s.snapshots {
			if oldestToken == "" || snapshot.ExpiresAt.Before(oldest) {
				oldestToken, oldest = token, snapshot.ExpiresAt
			}
		}
		delete(s.snapshots, oldestToken)
	}
}

func cloneExploreCounts(input map[string]int64) map[string]int64 {
	result := make(map[string]int64, len(input))
	maps.Copy(result, input)
	return result
}

func randomExploreToken() string {
	var raw [24]byte
	if _, err := rand.Read(raw[:]); err != nil {
		panic("crypto/rand unavailable: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(raw[:])
}

func (s *exploreServerState) issueOperation(selectionHash string, count int64, revision string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	for token, grant := range s.operations {
		if !grant.ExpiresAt.After(now) {
			delete(s.operations, token)
		}
	}
	for len(s.operations) >= exploreStateMaxEntries {
		var oldestToken string
		var oldest time.Time
		for token, grant := range s.operations {
			if oldestToken == "" || grant.ExpiresAt.Before(oldest) {
				oldestToken, oldest = token, grant.ExpiresAt
			}
		}
		delete(s.operations, oldestToken)
	}
	token := randomExploreToken()
	s.operations[token] = exploreOperationGrant{
		SelectionHash: selectionHash, Count: count, Revision: revision,
		ExpiresAt: now.Add(exploreOperationTokenTTL),
	}
	return token
}

func (s *exploreServerState) operation(token, selectionHash string) (exploreOperationGrant, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked(s.now())
	grant, ok := s.operations[token]
	if !ok || grant.SelectionHash != selectionHash {
		return exploreOperationGrant{}, false
	}
	return grant, true
}

// reserveOperation atomically claims the grant for one in-flight
// operation. A concurrent request with the same token fails here, so
// the one-shot guarantee holds while consumption is deferred. The
// caller must finish with commitOperation once the operation has
// durably succeeded, or rollbackOperation so the client can retry.
func (s *exploreServerState) reserveOperation(token, selectionHash string) (exploreOperationGrant, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked(s.now())
	grant, ok := s.operations[token]
	if !ok || grant.SelectionHash != selectionHash || grant.Reserved {
		return exploreOperationGrant{}, false
	}
	grant.Reserved = true
	s.operations[token] = grant
	return grant, true
}

// commitOperation permanently invalidates a reserved grant after the
// operation it authorized succeeded.
func (s *exploreServerState) commitOperation(token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.operations, token)
}

// rollbackOperation releases a reservation after the authorized
// operation failed, restoring the grant so the same token can be
// retried before it expires.
func (s *exploreServerState) rollbackOperation(token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	grant, ok := s.operations[token]
	if !ok {
		return
	}
	grant.Reserved = false
	s.operations[token] = grant
}
