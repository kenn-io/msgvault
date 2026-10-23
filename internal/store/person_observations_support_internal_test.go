package store

import "context"

func (s *Store) ObservationTraversalContextForTest(ctx context.Context, limit int) context.Context {
	return context.WithValue(ctx, observationTraversalLimitKey{}, limit)
}
func (s *Store) RefreshObservationForTest(ctx context.Context, personID, id int64) error {
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		if err := s.lockPersonMutationTx(ctx, tx, personID); err != nil {
			return err
		}
		return s.notifyObservationSupportChangedTx(ctx, tx, personID, []int64{id}, "continuation")
	})
}
