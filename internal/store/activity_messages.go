package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

type ActivityMessage struct {
	MessageID, SourceID, ConversationID int64
	Channel                             ActivityChannel
	Direction                           ActivityDirection
	OccurredAt                          time.Time
	LocalDate                           string
	Counterparts                        []ActivityMessageCounterpart
}
type MessageQuery struct {
	ConversationID int64
	After, Before  *time.Time
	Descending     bool
	AfterKey       *ActivityKey
	Limit          int
	Filters        ActivityFilters
}
type ActivityKey struct {
	OccurredAt time.Time
	MessageID  int64
}

// ActivityMessageRepresentativeContext resolves an anchor through the same
// scoped copy selection used by the burst reader. Zero means no projected copy.
func (s *Store) ActivityMessageRepresentativeContext(ctx context.Context, messageID int64, q BurstQuery) (int64, error) {
	filters, err := s.normalizedActivityFilters(ctx, q.Filters)
	if err != nil {
		return 0, err
	}
	cte, args, err := s.activityReadCTE(ctx, activityReadWindow{FromDate: q.FromDate, ToDate: q.ToDate, After: q.After, Before: q.Before, Subject: q.Subject}, filters)
	if err != nil {
		return 0, err
	}
	args = append(args, messageID)
	var id int64
	err = s.db.QueryRowContext(ctx, s.Rebind(cte+` SELECT r.message_id FROM representatives r JOIN copies anchor ON anchor.copy_key=r.copy_key WHERE anchor.message_id=?`), args...).Scan(&id)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("resolve activity message representative: %w", err)
	}
	return id, nil
}

func (s *Store) ListActivityMessagesContext(ctx context.Context, q MessageQuery) ([]ActivityMessage, error) {
	var result []ActivityMessage
	err := s.withReadSnapshotContext(ctx, func(tx *loggedTx) error {
		snapshotCtx := context.WithValue(ctx, readSnapshotKey{}, &readSnapshot{db: s.db.DB, tx: tx})
		var err error
		result, err = s.listActivityMessagesContext(snapshotCtx, q)
		return err
	})
	return result, err
}

func (s *Store) listActivityMessagesContext(ctx context.Context, q MessageQuery) ([]ActivityMessage, error) {
	if q.ConversationID <= 0 || q.Limit < 1 || q.Limit > 500 {
		return nil, fmt.Errorf("%w: messages require a conversation and limit 1..500", ErrInvalidActivityRequest)
	}
	filters, err := s.normalizedActivityFilters(ctx, q.Filters)
	if err != nil {
		return nil, err
	}
	cte, args, err := s.activityReadCTE(ctx, activityReadWindow{ConversationID: q.ConversationID, After: q.After, Before: q.Before}, filters)
	if err != nil {
		return nil, err
	}
	order, compare := "ASC", ">"
	if q.Descending {
		order, compare = "DESC", "<"
	}
	where := "TRUE"
	if !filters.IncludeNoise {
		where = "b.noise=0"
	}
	if q.AfterKey != nil {
		where += ` AND (r.occurred_at ` + compare + ` ? OR (r.occurred_at=? AND r.message_id ` + compare + ` ?))`
		args = append(args, q.AfterKey.OccurredAt.UTC(), q.AfterKey.OccurredAt.UTC(), q.AfterKey.MessageID)
	}
	args = append(args, q.Limit)
	rows, err := s.db.QueryContext(ctx, s.Rebind(cte+activityBurstGroupsCTE(filters)+`
 SELECT r.message_id,r.source_id,r.conversation_id,r.channel,r.direction,r.occurred_at,r.local_date
 FROM displayed r JOIN burst_groups b ON b.source_id=r.source_id AND b.conversation_id=r.conversation_id AND b.local_date=r.local_date
 WHERE `+where+` ORDER BY r.occurred_at `+order+`,r.message_id `+order+` LIMIT ?`), args...)
	if err != nil {
		return nil, fmt.Errorf("read activity messages: %w", err)
	}
	result := []ActivityMessage{}
	facts := []activityReadMessage{}
	for rows.Next() {
		var m ActivityMessage
		var at nullableTimestamp
		if err = rows.Scan(&m.MessageID, &m.SourceID, &m.ConversationID, &m.Channel, &m.Direction, &at, &m.LocalDate); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("scan activity message: %w", err)
		}
		m.OccurredAt = at.Time
		result = append(result, m)
		facts = append(facts, activityReadMessage{ID: m.MessageID})
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return nil, fmt.Errorf("iterate activity messages: %w", err)
	}
	if err = s.hydrateActivityCounterpartsContext(ctx, facts, filters); err != nil {
		return nil, err
	}
	for i := range result {
		result[i].Counterparts = facts[i].Counterparts
	}
	return result, nil
}

// LatestEligibleMessageContext locates a conversation anchor without applying
// the optional noise filter. Anchors still obey live eligibility and scope.
func (s *Store) LatestEligibleMessageContext(ctx context.Context, conversationID int64) (int64, error) {
	if conversationID <= 0 {
		return 0, fmt.Errorf("%w: conversation is required", ErrInvalidActivityRequest)
	}
	scope, args := sourceScopeClause(ctx, "m.source_id")
	args = append([]any{conversationID}, args...)
	var id int64
	err := s.db.QueryRowContext(ctx, s.Rebind(`SELECT m.id FROM messages m JOIN sources src ON src.id=m.source_id JOIN activity_events ae ON ae.message_id=m.id
 WHERE m.conversation_id=? AND m.message_type<>'call_record' AND `+s.activityLiveEligibleSQL()+scope+`
 ORDER BY ae.occurred_at DESC,m.id DESC LIMIT 1`), args...).Scan(&id)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read latest eligible message: %w", err)
	}
	return id, nil
}
