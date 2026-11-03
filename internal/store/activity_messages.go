package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"go.kenn.io/msgvault/internal/sqliteutil"
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

func (s *Store) ListActivityMessagesContext(ctx context.Context, q MessageQuery) ([]ActivityMessage, error) {
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
	rows, err := s.db.QueryContext(ctx, s.Rebind(cte+activityBurstGroupsCTE+`
 SELECT r.message_id,r.source_id,COALESCE(r.conversation_id,0),r.channel,r.direction,r.occurred_at,r.local_date,r.message_type,r.list_id,r.encrypted,r.large_inbound,r.contact_eligible
 FROM representatives r JOIN burst_groups b ON b.source_id=r.source_id AND b.conversation_id=COALESCE(r.conversation_id,0) AND b.local_date=r.local_date
 WHERE `+where+` ORDER BY r.occurred_at `+order+`,r.message_id `+order+` LIMIT ?`), args...)
	if err != nil {
		return nil, fmt.Errorf("read activity messages: %w", err)
	}
	result := []ActivityMessage{}
	facts := []activityReadMessage{}
	for rows.Next() {
		var m ActivityMessage
		var at nullableTimestamp
		var kind string
		var list sql.NullString
		var encrypted, large, eligible int
		if err = rows.Scan(&m.MessageID, &m.SourceID, &m.ConversationID, &m.Channel, &m.Direction, &at, &m.LocalDate, &kind, &list, &encrypted, &large, &eligible); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("scan activity message: %w", err)
		}
		m.OccurredAt = at.Time
		result = append(result, m)
		facts = append(facts, activityReadMessage{ID: m.MessageID, Direction: m.Direction, Meeting: IsMeetingMessageType(kind), List: list.Valid, Encrypted: encrypted != 0, LargeInbound: large != 0, ContactEligible: eligible != 0})
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
	return s.latestEligibleMessageContext(ctx, conversationID, false)
}

// LatestEligibleMessageIncludingColdContext locates a live conversation anchor
// from message metadata when the caller has requested cold activity. It keeps
// source scope and live-message eligibility checks, but does not require an
// activity projection event that cold messages may no longer have.
func (s *Store) LatestEligibleMessageIncludingColdContext(ctx context.Context, conversationID int64) (int64, error) {
	return s.latestEligibleMessageContext(ctx, conversationID, true)
}

func (s *Store) latestEligibleMessageContext(ctx context.Context, conversationID int64, includeCold bool) (int64, error) {
	if conversationID <= 0 {
		return 0, fmt.Errorf("%w: conversation is required", ErrInvalidActivityRequest)
	}
	scope, args := sourceScopeClause(ctx, "m.source_id")
	args = append([]any{conversationID}, args...)
	var id int64
	instantExpr := `COALESCE(m.sent_at,m.received_at,m.internal_date)`
	if !s.IsPostgreSQL() {
		instantExpr = sqliteutil.TimestampKeyFunction + `(` + instantExpr + `)`
	}
	query := `SELECT m.id FROM messages m JOIN sources src ON src.id=m.source_id
 WHERE m.conversation_id=? AND COALESCE(m.message_type,'')<>'call_record' AND ` + s.activityLiveEligibleSQL() + scope + `
 ORDER BY ` + instantExpr + ` DESC,m.id DESC LIMIT 1`
	if !includeCold {
		query = `SELECT m.id FROM messages m JOIN sources src ON src.id=m.source_id JOIN activity_events ae ON ae.message_id=m.id
 WHERE m.conversation_id=? AND COALESCE(m.message_type,'')<>'call_record' AND ` + s.activityLiveEligibleSQL() + ` AND ` + FullActivityEventWhere("ae") + scope + `
 ORDER BY ae.occurred_at DESC,m.id DESC LIMIT 1`
	}
	err := s.db.QueryRowContext(ctx, s.Rebind(query), args...).Scan(&id)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read latest eligible message: %w", err)
	}
	return id, nil
}
