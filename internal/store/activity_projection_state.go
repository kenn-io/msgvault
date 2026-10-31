package store

import (
	"context"
	"fmt"
	"time"

	"go.kenn.io/msgvault/internal/sourcescope"
)

type ActivityProjectionState struct {
	Queued         int
	OldestQueuedAt *time.Time
	Reconciling    bool
}

func (s *Store) ActivityProjectionStateContext(ctx context.Context) (ActivityProjectionState, error) {
	var result ActivityProjectionState
	var oldest nullableTimestamp
	scope, args := sourceScopeClause(ctx, "m.source_id")
	err := s.db.QueryRowContext(ctx, s.Rebind(`SELECT COUNT(*),MIN(q.queued_at) FROM activity_projection_queue q JOIN messages m ON m.id=q.message_id WHERE q.revision>q.processed_revision`+scope), args...).Scan(&result.Queued, &oldest)
	if err != nil {
		return result, fmt.Errorf("read activity projection queue state: %w", err)
	}
	result.OldestQueuedAt = nullableTimePointer(oldest)
	zone, err := s.ActivityTimezoneTransitionContext(ctx)
	if err != nil {
		return result, err
	}
	direct, err := s.ActivityDirectLimitTransitionContext(ctx)
	if err != nil {
		return result, err
	}
	if zone.Active || direct.Active {
		var visible bool
		err = s.db.QueryRowContext(ctx, s.Rebind(`SELECT EXISTS(SELECT 1 FROM messages m WHERE TRUE`+scope+`)`), args...).Scan(&visible)
		if err != nil {
			return result, fmt.Errorf("scope activity reconciliation state: %w", err)
		}
		result.Reconciling = visible
	}
	// A global identity epoch is meaningful for the owner only. A hidden source
	// must not change a delegated projection report through global revision keys.
	if sourcescope.IDs(ctx) == nil {
		current, err := s.ContactRevisionsContext(ctx)
		if err != nil {
			return result, err
		}
		reconciled, known, err := s.ActivityReconciledRevisionsContext(ctx)
		if err != nil {
			return result, err
		}
		result.Reconciling = result.Reconciling || !known || current != reconciled
	}
	return result, nil
}
