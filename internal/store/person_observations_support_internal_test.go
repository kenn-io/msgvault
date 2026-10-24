package store

import "context"

func (s *Store) ObservationTraversalBatchSizeContextForTest(ctx context.Context, batchSize int) context.Context {
	return context.WithValue(ctx, observationTraversalBatchSizeKey{}, batchSize)
}
