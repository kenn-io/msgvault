package store

import (
	"context"
	"errors"
)

// PersonMatchConsentEgressContext serializes one provider request with consent
// changes in the owning daemon. Callers must invoke it again for each retry.
func (s *Store) PersonMatchConsentEgressContext(ctx context.Context, fingerprint string, dispatch func() error) (bool, error) {
	if !personMatchFingerprint.MatchString(fingerprint) {
		return false, errors.New("person match disclosure fingerprint is invalid")
	}
	if dispatch == nil {
		return false, errors.New("person match provider dispatch is required")
	}
	mu := &s.withoutSyncScope().personMatchConsentMu
	mu.RLock()
	defer mu.RUnlock()
	active, err := s.HasPersonMatchConsentContext(ctx, fingerprint)
	if err != nil {
		return false, err
	}
	if !active {
		return false, nil
	}
	return true, dispatch()
}
