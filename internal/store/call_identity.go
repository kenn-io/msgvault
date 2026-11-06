package store

import (
	"context"
	"database/sql"
	"fmt"
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

// LogicalCall combines active records of one call and all its participants.
type LogicalCall struct {
	IdentityKey     string
	MessageID       int64
	Kind            string
	OccurredAt      time.Time
	Outcome         string
	DurationSeconds *float64
	Participants    []CallParticipant
}

// ListLogicalCallsContext pages whole identities, ordered by time then key.
// Candidate selection is window-bounded; all active records of a candidate are
// then resolved before applying the identity's earliest timestamp.
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
		where += " AND EXISTS (SELECT 1 FROM call_participants p WHERE p.identity_key = i.identity_key AND " + clause + ")"
		bounds = append(bounds, args...)
	}
	if q.AfterKey != "" {
		at, key, ok := strings.Cut(q.AfterKey, "|")
		timestamp, err := time.Parse(time.RFC3339Nano, at)
		if !ok || err != nil || !validLogicalCallTime(timestamp) || (!strings.HasPrefix(key, "n:") && !strings.HasPrefix(key, "m:")) || len(key) <= 2 {
			return nil, "", fmt.Errorf("%w: logical call cursor", callhistory.ErrInvalid)
		}
		where += " AND (i.occurred_at > ? OR (i.occurred_at = ? AND i.identity_key > ?))"
		bounds = append(bounds, timestamp.UTC(), timestamp.UTC(), key)
	}
	ctes, args := s.callScopedIdentityCTEs(ctx, "c.occurred_at >= ? AND c.occurred_at < ?", q.After.UTC(), q.Before.UTC())
	args = append(args, bounds...)
	args = append(args, q.Limit+1)
	calls := []LogicalCall{}
	next := ""
	err := s.withReadSnapshotContext(ctx, func(tx *loggedTx) error {
		rows, err := tx.QueryContext(ctx, `WITH `+ctes+`, selected AS (
 SELECT i.*, `+callCategorySQL("CASE WHEN EXISTS (SELECT 1 FROM active a WHERE a.identity_key = i.identity_key AND a.outcome = 'answered' AND a.owner_participation = 'joined') THEN 1 ELSE 0 END")+` AS category
 FROM identities i WHERE `+where+`
 ORDER BY i.occurred_at, i.identity_key LIMIT ?)
 SELECT i.identity_key, i.message_id, i.kind, i.occurred_at, i.category, i.duration_seconds,
 p.participant_id, p.person_id, p.participation, `+callCategorySQL("p.confirmed")+` = 'answered'
 FROM selected i LEFT JOIN call_participants p ON p.identity_key = i.identity_key
 ORDER BY i.occurred_at, i.identity_key, p.participant_id`, args...)
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
			if err := rows.Scan(&c.IdentityKey, &c.MessageID, &c.Kind, &at, &c.Outcome, &duration, &participantID, &personID, &participation, &credited); err != nil {
				return fmt.Errorf("scan logical call: %w", err)
			}
			c.OccurredAt = at.Time.UTC()
			if len(calls) == 0 || calls[len(calls)-1].IdentityKey != c.IdentityKey {
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
			next = last.OccurredAt.UTC().Format(time.RFC3339Nano) + "|" + last.IdentityKey
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
 SELECT a.identity_key, r.participant_id, COALESCE(MAX(pp.person_id),0) AS person_id,
 CASE WHEN MAX(CASE WHEN r.recipient_type = 'call_joined' THEN 1 ELSE 0 END) = 1 THEN 'joined'
 WHEN MAX(CASE WHEN r.recipient_type = 'call_invited' THEN 1 ELSE 0 END) = 1 THEN 'invited'
 ELSE 'unknown' END AS participation,
 MAX(` + callConfirmedSQL() + `) AS confirmed,
 MAX(CASE WHEN (` + callConfirmedSQL() + `) = 1 AND a.direction = 'incoming' THEN 1 ELSE 0 END) AS incoming,
 MAX(CASE WHEN (` + callConfirmedSQL() + `) = 1 AND a.direction = 'outgoing' THEN 1 ELSE 0 END) AS outgoing
 FROM active a JOIN (` + s.callRolesSQL() + `) r ON r.message_id = a.message_id
 LEFT JOIN person_participants pp ON pp.participant_id = r.participant_id
 WHERE r.recipient_type IN ('call_joined','call_invited','call_unknown')
 GROUP BY a.identity_key, r.participant_id)`
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
	// Select this subject's identities, then resolve every participant's credit
	// from all their active records.
	ctes, args := s.callScopedIdentityCTEs(ctx, `c.message_id IN (SELECT r.message_id FROM (`+s.callRolesSQL()+`) r
 WHERE r.recipient_type IN ('call_joined','call_invited','call_unknown')
 AND `+candidateSubject+`)`, subjectArgs...)
	args = append(args, subjectArgs...)
	args = append(args, q.Now.UTC())
	err := s.withReadSnapshotContext(ctx, func(tx *loggedTx) error {
		rows, err := tx.QueryContext(ctx, `WITH `+ctes+`
 SELECT i.identity_key, i.occurred_at, MAX(p.incoming), MAX(p.outgoing)
 FROM identities i JOIN call_participants p ON p.identity_key = i.identity_key
 WHERE `+subject+` AND `+callCategorySQL("p.confirmed")+` = 'answered' AND i.occurred_at <= ?
 GROUP BY i.identity_key, i.occurred_at ORDER BY i.occurred_at, i.identity_key`, args...)
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
			var identity string
			var timestamp requiredTimestamp
			var incoming, outgoing int
			if err := rows.Scan(&identity, &timestamp, &incoming, &outgoing); err != nil {
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

// A window or participant predicate bounds candidates. Native identities then
// use the canonical-key index; records without one use primary-key lookups.
// Authorization applies to every evidence row.
func (s *Store) callScopedIdentityCTEs(ctx context.Context, candidateWhere string, candidateArgs ...any) (string, []any) {
	scope, scopeArgs := sourceScopeClause(ctx, "m.source_id")
	active := s.callActiveSQL(scope+` AND c.canonical_key IN (SELECT canonical_key FROM candidates WHERE canonical_key <> '')`) +
		" UNION ALL " + s.callActiveSQL(scope+` AND c.message_id IN (SELECT message_id FROM candidates WHERE canonical_key = '')`)
	ctes := `candidates AS (
 SELECT c.canonical_key, c.message_id FROM (` + s.callRowsSQL() + `) c JOIN messages m ON m.id = c.message_id
 WHERE NOT c.withdrawn AND m.deleted_at IS NULL AND m.deleted_from_source_at IS NULL` + scope + `
 AND (` + candidateWhere + `)), ` + callIdentityCTEs(active) + `, ` + s.callParticipantsCTE()
	args := append([]any{}, scopeArgs...)
	args = append(args, candidateArgs...)
	args = append(args, scopeArgs...)
	args = append(args, scopeArgs...)
	return ctes, args
}
