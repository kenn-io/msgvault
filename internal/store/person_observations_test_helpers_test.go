package store

import (
	"context"
	"strings"
)

// CancelObservationMessageBackfillForTest interrupts the second real chunk
// query, after the first transaction has committed. Rebinding still delegates
// to the actual dialect; no query or database behavior is stubbed.
func (s *Store) CancelObservationMessageBackfillForTest(cancel context.CancelFunc) func() {
	original := s.db.rebind
	chunks := 0
	s.db.rebind = func(query string) string {
		if strings.Contains(query, "SELECT id, source_ref FROM person_fact_evidence") {
			chunks++
			if chunks == 2 {
				cancel()
			}
		}
		return original(query)
	}
	return func() { s.db.rebind = original }
}
