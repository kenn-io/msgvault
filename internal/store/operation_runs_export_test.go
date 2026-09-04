package store

import (
	"context"

	"go.kenn.io/msgvault/internal/operations"
)

//nolint:revive // Test exports mirror the Store receiver-first API they expose.
func ListSourceOperationRunsForTest(
	s *Store, ctx context.Context, query operations.Query,
) ([]operations.Run, error) {
	return s.listSourceOperationRuns(ctx, query, query.Limit)
}

//nolint:revive // Test exports mirror the Store receiver-first API they expose.
func ListSourceOperationRunsWithFetchLimitForTest(
	s *Store, ctx context.Context, query operations.Query, fetchLimit int,
) ([]operations.Run, error) {
	return s.listSourceOperationRuns(ctx, query, fetchLimit)
}

//nolint:revive // Test exports mirror the Store receiver-first API they expose.
func GetSourceOperationRunForTest(
	s *Store, ctx context.Context, id operations.StableID,
) (operations.Run, error) {
	return s.getSourceOperationRun(ctx, id)
}

//nolint:revive // Test exports mirror the Store receiver-first API they expose.
func SourceOperationLaneStatusForTest(
	s *Store, ctx context.Context,
) (operations.LaneHistoryStatus, error) {
	return s.sourceOperationLaneStatus(ctx)
}

//nolint:revive // Test exports mirror the Store receiver-first API they expose.
func ListPersonSweepOperationRunsForTest(
	s *Store, ctx context.Context, query operations.Query,
) ([]operations.Run, error) {
	return s.listPersonSweepOperationRuns(ctx, query, query.Limit)
}

//nolint:revive // Test exports mirror the Store receiver-first API they expose.
func ListPersonSweepOperationRunsWithFetchLimitForTest(
	s *Store, ctx context.Context, query operations.Query, fetchLimit int,
) ([]operations.Run, error) {
	return s.listPersonSweepOperationRuns(ctx, query, fetchLimit)
}

//nolint:revive // Test exports mirror the Store receiver-first API they expose.
func GetPersonSweepOperationRunForTest(
	s *Store, ctx context.Context, id operations.StableID,
) (operations.Run, error) {
	return s.getPersonSweepOperationRun(ctx, id)
}

//nolint:revive // Test exports mirror the Store receiver-first API they expose.
func PersonSweepOperationLaneStatusForTest(
	s *Store, ctx context.Context,
) (operations.LaneHistoryStatus, error) {
	return s.personSweepOperationLaneStatus(ctx)
}

//nolint:revive // Test exports mirror the Store receiver-first API they expose.
func ListCardDAVOperationRunsForTest(
	s *Store, ctx context.Context, query operations.Query,
) ([]operations.Run, error) {
	return s.listCardDAVOperationRuns(ctx, query, query.Limit)
}

//nolint:revive // Test exports mirror the Store receiver-first API they expose.
func ListCardDAVOperationRunsWithFetchLimitForTest(
	s *Store, ctx context.Context, query operations.Query, fetchLimit int,
) ([]operations.Run, error) {
	return s.listCardDAVOperationRuns(ctx, query, fetchLimit)
}

//nolint:revive // Test exports mirror the Store receiver-first API they expose.
func GetCardDAVOperationRunForTest(
	s *Store, ctx context.Context, id operations.StableID,
) (operations.Run, error) {
	return s.getCardDAVOperationRun(ctx, id)
}

//nolint:revive // Test exports mirror the Store receiver-first API they expose.
func CardDAVOperationLaneStatusForTest(
	s *Store, ctx context.Context,
) (operations.LaneHistoryStatus, error) {
	return s.cardDAVOperationLaneStatus(ctx)
}

func SetOperationHistoryAfterAdapterReadHookForTest(
	s *Store, hook func(operations.Kind),
) {
	if hook == nil {
		s.operationHistoryAfterAdapterReadHook = nil
		return
	}
	s.operationHistoryAfterAdapterReadHook = func(kind string) {
		hook(operations.Kind(kind))
	}
}

func SetOperationHistoryStatusAfterActiveHookForTest(
	s *Store, hook func(operations.Kind),
) {
	if hook == nil {
		s.operationHistoryStatusAfterActiveHook = nil
		return
	}
	s.operationHistoryStatusAfterActiveHook = func(kind string) {
		hook(operations.Kind(kind))
	}
}
