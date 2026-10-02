package store

import "time"

// NoteProviderIdentityCheck keeps successful no-op polls fresh without writing
// archive rows. Sync-scoped views share the base Store's memory. Restarting the
// daemon may repeat one poll. Failed checks override an earlier durable success
// until the next successful check, including failures to record an outcome.
func (s *Store) NoteProviderIdentityCheck(sourceID int64, success bool) {
	if s.syncBase != nil {
		s.syncBase.NoteProviderIdentityCheck(sourceID, success)
		return
	}
	s.providerIdentityChecksMu.Lock()
	defer s.providerIdentityChecksMu.Unlock()
	if s.providerIdentityChecks == nil {
		s.providerIdentityChecks = make(map[int64]time.Time)
	}
	checked := time.Time{}
	if success {
		checked = time.Now()
	}
	s.providerIdentityChecks[sourceID] = checked
}

func (s *Store) ProviderIdentityCheckFresh(sourceID int64, maxAge time.Duration) bool {
	if s.syncBase != nil {
		return s.syncBase.ProviderIdentityCheckFresh(sourceID, maxAge)
	}
	s.providerIdentityChecksMu.Lock()
	defer s.providerIdentityChecksMu.Unlock()
	checked, ok := s.providerIdentityChecks[sourceID]
	return ok && !checked.IsZero() && time.Since(checked) < maxAge
}

func (s *Store) ProviderIdentityCheckFailed(sourceID int64) bool {
	if s.syncBase != nil {
		return s.syncBase.ProviderIdentityCheckFailed(sourceID)
	}
	s.providerIdentityChecksMu.Lock()
	defer s.providerIdentityChecksMu.Unlock()
	checked, ok := s.providerIdentityChecks[sourceID]
	return ok && checked.IsZero()
}
