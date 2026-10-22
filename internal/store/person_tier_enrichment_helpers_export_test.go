package store

import (
	"context"
	"go.kenn.io/msgvault/internal/personenrichment"
	"time"
)

func PublishTierEnrichmentForTest(ctx context.Context, s *Store, personID int64) error {
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		return s.publishPersonEnrichmentTx(ctx, tx, personID, personenrichment.TriggerTracked, "scheduled:test", s.personEnrichmentTime())
	})
}
func CancelScheduledTierEnrichmentForTest(ctx context.Context, s *Store, personID int64) error {
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		if err := s.lockPersonEnrichmentAuthorityMutationTx(ctx, tx); err != nil {
			return err
		}
		return s.cancelScheduledPersonEnrichmentTx(ctx, tx, personID)
	})
}
func TierEnrichmentAuthorityForTest(ctx context.Context, s *Store, personID int64) (bool, error) {
	var eligible bool
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		var err error
		eligible, err = s.personEnrichmentScheduledEligibleTx(ctx, tx, personID)
		return err
	})
	return eligible, err
}
func OpenManualTierEnrichmentForTest(ctx context.Context, s *Store, personID int64) (string, error) {
	var id string
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		var err error
		id, err = s.openPersonManualEnrichmentRunTx(ctx, tx, personID)
		return err
	})
	return id, err
}

func SetTierEnrichmentClockForTest(s *Store, clock func() time.Time) { s.personEnrichmentClock = clock }
