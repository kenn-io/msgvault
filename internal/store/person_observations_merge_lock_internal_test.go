package store

import (
	"context"
	"go.kenn.io/msgvault/internal/personfacts"
)

// Pause after the real generation has locked its person, before support events.
func (s *Store) ApplyObservationStatusPausedForTest(ctx context.Context, input personfacts.GenerationInput, paused chan<- int, release <-chan struct{}) error {
	prepared, err := personfacts.PreparePersonFactGeneration(ctx, input, nil)
	if err != nil {
		return err
	}
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		var pid int
		if err := tx.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
			return err
		}
		_, _, err := s.applyPreparedPersonFactGenerationDetailedTx(ctx, tx, prepared, func(stage string) error {
			if stage != "claim" {
				return nil
			}
			select {
			case paused <- pid:
			case <-ctx.Done():
				return ctx.Err()
			}
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
		return err
	})
}

// Use the real first merge attempt so retry cannot hide a lock-order defect.
func (s *Store) MergeObservationPersonOnceForTest(ctx context.Context, request PersonMergeRequest) (*PersonMergeResult, error) {
	return s.mergePersonsOnce(ctx, request)
}
