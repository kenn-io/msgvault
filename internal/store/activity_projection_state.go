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
	conversationScope, conversationArgs := sourceScopeClause(ctx, "c.source_id")
	queueArgs := append(append([]any{}, args...), conversationArgs...)
	// Roster markers have no timestamp until expansion queues their messages.
	err := s.db.QueryRowContext(ctx, s.Rebind(`SELECT COUNT(*),MIN(queued_at) FROM (
 SELECT q.queued_at FROM activity_projection_queue q JOIN messages m ON m.id=q.message_id WHERE m.message_type<>'call_record' AND q.revision>q.processed_revision`+scope+`
 UNION ALL SELECT NULL AS queued_at FROM activity_projection_conversation_queue q JOIN conversations c ON c.id=q.conversation_id WHERE TRUE`+conversationScope+`
 ) pending`), queueArgs...).Scan(&result.Queued, &oldest)
	if err != nil {
		return result, fmt.Errorf("read activity projection queue state: %w", err)
	}
	result.OldestQueuedAt = nullableTimePointer(oldest)
	result.Reconciling = result.Queued > 0
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
		err = s.db.QueryRowContext(ctx, s.Rebind(`SELECT EXISTS(SELECT 1 FROM messages m WHERE m.message_type<>'call_record'`+scope+`)`), args...).Scan(&visible)
		if err != nil {
			return result, fmt.Errorf("scope activity reconciliation state: %w", err)
		}
		result.Reconciling = result.Reconciling || visible
	}
	// Only the owner reports global epoch lag directly. Scoped reports compare
	// visible classifications so hidden-source changes cannot alter their state.
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
	} else if !result.Reconciling {
		result.Reconciling, err = s.scopedActivityIdentityPendingContext(ctx)
		if err != nil {
			return result, err
		}
	}
	return result, nil
}

// Global epochs only select candidates. A scoped report becomes pending when
// current inputs would actually change a visible event's classification.
func (s *Store) scopedActivityIdentityPendingContext(ctx context.Context) (bool, error) {
	var pending bool
	err := s.withReadSnapshotContext(ctx, func(tx *loggedTx) error {
		scope, scopeArgs := sourceScopeClause(ctx, "m.source_id")
		var after int64
		for {
			args := append([]any{after}, scopeArgs...)
			args = append(args, activityCandidateIDChunk)
			rows, err := tx.QueryContext(ctx, activityCandidateStateCTE+`
 SELECT m.id FROM messages m JOIN sources src ON src.id=m.source_id
 JOIN activity_events ae ON ae.message_id=m.id CROSS JOIN activity_current_state r
 WHERE m.id>? AND `+s.activityLiveEligibleSQL()+scope+`
 AND (ae.projected_identity_revision<>r.identity_revision OR ae.projected_account_identity_revision<>r.account_identity_revision)
 ORDER BY m.id LIMIT ?`, args...)
			if err != nil {
				return fmt.Errorf("read scoped activity identity candidates: %w", err)
			}
			ids := []int64{}
			for rows.Next() {
				var id int64
				if err := rows.Scan(&id); err != nil {
					_ = rows.Close()
					return err
				}
				ids = append(ids, id)
			}
			err = rows.Err()
			_ = rows.Close()
			if err != nil {
				return err
			}
			if len(ids) == 0 {
				return nil
			}
			candidates, err := s.loadActivityCandidatesByIDQueryerContext(ctx, tx, ids)
			if err != nil {
				return err
			}
			for _, candidate := range candidates {
				old, err := s.loadActivityEventTx(ctx, tx, candidate.MessageID)
				if err != nil {
					return err
				}
				classification := ClassifyActivityCandidate(candidate, candidate.DirectLimitTransition.Target)
				next := &ActivityEvent{RefKind: classification.RefKind, Channel: classification.Channel, Direction: classification.Direction, OwnerSourceID: classification.OwnerSourceID, OwnerAddress: classification.OwnerAddress, Persons: classification.Persons}
				if !activityEventClassificationEqual(old, next) {
					pending = true
					return nil
				}
			}
			after = ids[len(ids)-1]
		}
	})
	return pending, err
}
