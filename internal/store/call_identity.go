package store

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/callhistory"
)

// LogicalCallQuery selects complete call identities in [After, Before).
type LogicalCallQuery struct {
	After, Before  time.Time
	ParticipantIDs []int64
	Limit          int
	AfterKey       string
}

// CallContactQuery supplies the identity, clock and zone for a call aggregate.
type CallContactQuery struct {
	Subject  ActivitySubject
	Now      time.Time
	Location *time.Location
}

// CallParticipant reports stored participation and whole-observation credit.
type CallParticipant struct {
	ParticipantID, PersonID int64
	Participation           string
	Credited                bool
}

// LogicalCall reports one call record and all its participants.
type LogicalCall struct {
	MessageID       int64
	OccurredAt      time.Time
	Outcome         string
	DurationSeconds *float64
	Participants    []CallParticipant
}

// ListLogicalCallsContext pages call records, ordered by time then message ID.
func (s *Store) ListLogicalCallsContext(ctx context.Context, q LogicalCallQuery) ([]LogicalCall, string, error) {
	if q.Limit < 1 || q.Limit > 500 || !validLogicalCallTime(q.After) || !validLogicalCallTime(q.Before) || !q.After.Before(q.Before) {
		return nil, "", fmt.Errorf("%w: logical call bounds", callhistory.ErrInvalid)
	}
	for _, id := range q.ParticipantIDs {
		if id <= 0 {
			return nil, "", fmt.Errorf("%w: call participant", callhistory.ErrInvalid)
		}
	}
	where := "i.occurred_at >= ? AND i.occurred_at < ?"
	bounds := []any{q.After.UTC(), q.Before.UTC()}
	if len(q.ParticipantIDs) != 0 {
		clause, args := messageExportInClause("p.participant_id", q.ParticipantIDs)
		where += " AND EXISTS (SELECT 1 FROM call_participants p WHERE p.message_id = i.message_id AND " + clause + ")"
		bounds = append(bounds, args...)
	}
	if q.AfterKey != "" {
		at, key, ok := strings.Cut(q.AfterKey, "|")
		timestamp, timeErr := time.Parse(time.RFC3339Nano, at)
		messageID, idErr := strconv.ParseInt(key, 10, 64)
		if !ok || timeErr != nil || idErr != nil || !validLogicalCallTime(timestamp) || messageID <= 0 {
			return nil, "", fmt.Errorf("%w: logical call cursor", callhistory.ErrInvalid)
		}
		where += " AND (i.occurred_at > ? OR (i.occurred_at = ? AND i.message_id > ?))"
		bounds = append(bounds, timestamp.UTC(), timestamp.UTC(), messageID)
	}
	ctes, args := s.callScopedIdentityCTEs(ctx, "c.occurred_at >= ? AND c.occurred_at < ?", q.After.UTC(), q.Before.UTC())
	args = append(args, bounds...)
	args = append(args, q.Limit+1)
	calls := []LogicalCall{}
	next := ""
	err := s.withReadSnapshotContext(ctx, func(tx *loggedTx) error {
		rows, err := tx.QueryContext(ctx, `WITH `+ctes+`, selected AS (
 SELECT i.*, CASE WHEN i.outcome = 'answered' AND i.owner_participation = 'joined' THEN 'answered'
 WHEN i.outcome IN ('missed','declined','cancelled') THEN i.outcome
 ELSE 'unknown' END AS category
 FROM active i WHERE `+where+`
 ORDER BY i.occurred_at, i.message_id LIMIT ?)
 SELECT i.message_id, i.occurred_at, i.category, i.duration_seconds,
 p.participant_id, p.person_id, p.participation, p.confirmed = 1
 FROM selected i LEFT JOIN call_participants p ON p.message_id = i.message_id
 ORDER BY i.occurred_at, i.message_id, p.participant_id`, args...)
		if err != nil {
			return fmt.Errorf("list logical calls: %w", err)
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var c LogicalCall
			var at requiredTimestamp
			var duration sql.NullFloat64
			var participantID, personID sql.NullInt64
			var participation sql.NullString
			var credited sql.NullBool
			if err := rows.Scan(&c.MessageID, &at, &c.Outcome, &duration, &participantID, &personID, &participation, &credited); err != nil {
				return fmt.Errorf("scan logical call: %w", err)
			}
			c.OccurredAt = at.Time.UTC()
			if len(calls) == 0 || calls[len(calls)-1].MessageID != c.MessageID {
				c.Participants = []CallParticipant{}
				if duration.Valid {
					c.DurationSeconds = &duration.Float64
				}
				calls = append(calls, c)
			}
			if participantID.Valid {
				last := &calls[len(calls)-1]
				last.Participants = append(last.Participants, CallParticipant{ParticipantID: participantID.Int64, PersonID: personID.Int64, Participation: participation.String, Credited: credited.Bool})
			}
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("read logical calls: %w", err)
		}
		if len(calls) > q.Limit {
			calls = calls[:q.Limit]
			last := calls[len(calls)-1]
			next = last.OccurredAt.UTC().Format(time.RFC3339Nano) + "|" + strconv.FormatInt(last.MessageID, 10)
		}
		return nil
	})
	if err != nil {
		return nil, "", err
	}
	return calls, next, nil
}

func validLogicalCallTime(at time.Time) bool {
	return !at.IsZero() && at.Year() >= 1 && at.Year() <= 9999
}

func (s *Store) callParticipantsCTE() string {
	return `call_participants AS (
 SELECT a.message_id, r.participant_id, COALESCE(MAX(pp.person_id),0) AS person_id,
 CASE WHEN MAX(CASE WHEN r.recipient_type = 'call_joined' THEN 1 ELSE 0 END) = 1 THEN 'joined'
 WHEN MAX(CASE WHEN r.recipient_type = 'call_invited' THEN 1 ELSE 0 END) = 1 THEN 'invited'
 ELSE 'unknown' END AS participation,
 MAX(` + callConfirmedSQL() + `) AS confirmed,
 MAX(CASE WHEN (` + callConfirmedSQL() + `) = 1 AND a.direction = 'incoming' THEN 1 ELSE 0 END) AS incoming,
 MAX(CASE WHEN (` + callConfirmedSQL() + `) = 1 AND a.direction = 'outgoing' THEN 1 ELSE 0 END) AS outgoing
 FROM active a JOIN (` + s.callRolesSQL() + `) r ON r.message_id = a.message_id
 LEFT JOIN person_participants pp ON pp.participant_id = r.participant_id
 WHERE r.recipient_type IN ('call_joined','call_invited','call_unknown')
 GROUP BY a.message_id, r.participant_id)`
}

// CallContactAggregateContext summarizes credited logical calls for one subject.
// Calls use whole-observation credit, independently of message noise filters.
// Initiation remains message-only. The composer labels this aggregate as calls;
// the store channel stays Other to preserve the activity channel contract.
func (s *Store) CallContactAggregateContext(ctx context.Context, q CallContactQuery) (ContactAggregate, error) {
	var result ContactAggregate
	if q.Subject.PersonID < 0 || (q.Subject.PersonID == 0 && len(q.Subject.ParticipantIDs) == 0) ||
		(q.Subject.PersonID != 0 && len(q.Subject.ParticipantIDs) != 0) || !validLogicalCallTime(q.Now) {
		return result, fmt.Errorf("%w: call contact subject or clock", callhistory.ErrInvalid)
	}
	for _, id := range q.Subject.ParticipantIDs {
		if id <= 0 {
			return result, fmt.Errorf("%w: call participant", callhistory.ErrInvalid)
		}
	}
	var candidateSubject, subject string
	var subjectArgs []any
	if q.Subject.PersonID != 0 {
		candidateSubject = `r.participant_id IN (SELECT pp.participant_id FROM person_participants pp WHERE pp.person_id = ?)`
		subject = "p.person_id = ?"
		subjectArgs = []any{q.Subject.PersonID}
	} else {
		candidateSubject, subjectArgs = messageExportInClause("r.participant_id", q.Subject.ParticipantIDs)
		subject, _ = messageExportInClause("p.participant_id", q.Subject.ParticipantIDs)
	}
	// Select this subject's calls, then resolve each participant's credit.
	ctes, args := s.callScopedIdentityCTEs(ctx, `c.message_id IN (SELECT r.message_id FROM (`+s.callRolesSQL()+`) r
 WHERE r.recipient_type IN ('call_joined','call_invited','call_unknown')
 AND `+candidateSubject+`)`, subjectArgs...)
	args = append(args, subjectArgs...)
	args = append(args, q.Now.UTC())
	err := s.withReadSnapshotContext(ctx, func(tx *loggedTx) error {
		rows, err := tx.QueryContext(ctx, `WITH `+ctes+`
 SELECT i.message_id, i.occurred_at, MAX(p.incoming), MAX(p.outgoing)
 FROM active i JOIN call_participants p ON p.message_id = i.message_id
 WHERE `+subject+` AND p.confirmed = 1 AND i.occurred_at <= ?
 GROUP BY i.message_id, i.occurred_at ORDER BY i.occurred_at, i.message_id`, args...)
		if err != nil {
			return fmt.Errorf("aggregate call contact: %w", err)
		}
		defer func() { _ = rows.Close() }()
		loc := q.Location
		if loc == nil {
			loc = time.UTC
		}
		now := q.Now.In(loc)
		for rows.Next() {
			var messageID int64
			var timestamp requiredTimestamp
			var incoming, outgoing int
			if err := rows.Scan(&messageID, &timestamp, &incoming, &outgoing); err != nil {
				return fmt.Errorf("scan call contact: %w", err)
			}
			at := timestamp.Time.UTC()
			if result.FirstAt == nil {
				result.FirstAt = &at
			}
			result.LastAt = &at
			if incoming != 0 {
				result.LastInboundAt = &at
			}
			if outgoing != 0 {
				result.LastOutboundAt = &at
			}
			result.Count++
			local := at.In(loc)
			monthsAgo := (now.Year()-local.Year())*12 + int(now.Month()-local.Month())
			if monthsAgo >= 0 && monthsAgo < len(result.Monthly) {
				result.Monthly[len(result.Monthly)-1-monthsAgo]++
			}
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("read call contact: %w", err)
		}
		if result.Count > 0 {
			result.Channels = []ActivityChannel{ChannelOther}
		}
		return nil
	})
	if err != nil {
		return ContactAggregate{}, err
	}
	return result, nil
}

// A window or participant predicate bounds records before resolving participants.
// Authorization applies to every evidence row.
func (s *Store) callScopedIdentityCTEs(ctx context.Context, candidateWhere string, candidateArgs ...any) (string, []any) {
	scope, args := sourceScopeClause(ctx, "m.source_id")
	active := s.callActiveSQL(scope + " AND (" + candidateWhere + ")")
	args = append(args, candidateArgs...)
	return "active AS (" + active + "), " + s.callParticipantsCTE(), args
}
