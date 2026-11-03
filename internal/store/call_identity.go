package store

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/callhistory"
)

// LogicalCallQuery selects complete call identities in [After, Before).
type LogicalCallQuery struct {
	After, Before time.Time
	// DateAfter and DateBefore bound date-precision evidence by civil date.
	DateAfter      string
	DateBefore     string
	ParticipantIDs []int64
	Limit          int
	AfterKey       string
}

// CallParticipant reports stored participation and whole-observation credit.
type CallParticipant struct {
	ParticipantID, PersonID int64
	Participation           string
	Credited                bool
}

// LogicalCall combines active observations of one canonical call.
type LogicalCall struct {
	IdentityKey     string
	MessageID       int64
	Kind            string
	OccurredAt      time.Time
	Precision       string
	Outcome         string
	DurationSeconds *float64
	Participants    []CallParticipant
}

// CallIdentityForMessageContext returns the active logical-call key for one
// observation. An empty key means the message is not an active call.
func (s *Store) CallIdentityForMessageContext(ctx context.Context, messageID int64) (string, error) {
	if messageID <= 0 {
		return "", fmt.Errorf("invalid call message ID %d", messageID)
	}
	scope, scopeArgs := sourceScopeClause(ctx, "m.source_id")
	query := `SELECT ` + callIdentitySQL("c") + ` FROM call_details c
 JOIN messages m ON m.id = c.message_id
 WHERE c.message_id = ? AND NOT c.withdrawn AND m.deleted_at IS NULL AND m.deleted_from_source_at IS NULL` + scope
	args := append([]any{messageID}, scopeArgs...)
	var key string
	err := ReadDBContext(ctx, s.db.DB).QueryRowContext(ctx, s.Rebind(query), args...).Scan(&key)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("resolve call identity for message %d: %w", messageID, err)
	}
	return key, nil
}

// ListLogicalCallsContext pages whole identities, ordered by time then key.
// Candidate selection is window-bounded; all active observations of a candidate
// are then resolved before applying the identity's earliest timestamp.
func (s *Store) ListLogicalCallsContext(ctx context.Context, q LogicalCallQuery) ([]LogicalCall, string, error) {
	if q.Limit < 1 || q.Limit > 500 || !validLogicalCallTime(q.After) || !validLogicalCallTime(q.Before) || !q.After.Before(q.Before) {
		return nil, "", fmt.Errorf("%w: logical call bounds", callhistory.ErrInvalid)
	}
	dateAfter, dateBefore, hasDateBounds, err := logicalCallDateBounds(q)
	if err != nil {
		return nil, "", err
	}
	for _, id := range q.ParticipantIDs {
		if id <= 0 {
			return nil, "", fmt.Errorf("%w: call participant", callhistory.ErrInvalid)
		}
	}
	instantWindow := "i.occurred_at >= ? AND i.occurred_at < ?"
	candidateWindow := "c.occurred_at >= ? AND c.occurred_at < ?"
	where := "(" + instantWindow + ")"
	candidateWhere := "(" + candidateWindow + ")"
	bounds := []any{q.After.UTC(), q.Before.UTC()}
	candidateBounds := []any{q.After.UTC(), q.Before.UTC()}
	if hasDateBounds {
		candidatePrecision := "m.metadata ->> 'precision'"
		if s.dialect.DriverName() != postgresDriverName {
			candidatePrecision = "CASE WHEN json_valid(m.metadata) THEN m.metadata ->> 'precision' END"
		}
		metadata := `(SELECT m.metadata FROM active evidence JOIN messages m ON m.id = evidence.message_id
  WHERE evidence.identity_key = i.identity_key
  ORDER BY evidence.occurred_at, evidence.message_id LIMIT 1)`
		identityPrecision := "(" + metadata + ") ->> 'precision'"
		if s.dialect.DriverName() != postgresDriverName {
			identityPrecision = "CASE WHEN json_valid(" + metadata + ") THEN " + metadata + " ->> 'precision' END"
		}
		where = "((" + instantWindow + " AND COALESCE(" + identityPrecision + ", 'instant') = 'instant') OR (i.occurred_at >= ? AND i.occurred_at < ? AND " + identityPrecision + " = 'day'))"
		candidateWhere = "((" + candidateWindow + " AND COALESCE(" + candidatePrecision + ", 'instant') = 'instant') OR (c.occurred_at >= ? AND c.occurred_at < ? AND " + candidatePrecision + " = 'day'))"
		bounds = append(bounds, dateAfter, dateBefore)
		candidateBounds = append(candidateBounds, dateAfter, dateBefore)
	}
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
	ctes, args := callScopedIdentityCTEs(ctx, candidateWhere, candidateBounds...)
	args = append(args, bounds...)
	args = append(args, q.Limit+1)
	rows, err := s.db.QueryContext(ctx, s.Rebind(`WITH `+ctes+`, selected AS (
 SELECT i.*, `+callCategorySQL("i.owner_answered")+` AS category
 FROM identities i WHERE `+where+`
 ORDER BY i.occurred_at, i.identity_key LIMIT ?)
 SELECT i.identity_key, i.message_id, i.kind, i.occurred_at, i.category, i.duration_seconds,
 (SELECT m.metadata FROM active evidence JOIN messages m ON m.id = evidence.message_id
  WHERE evidence.identity_key = i.identity_key
  ORDER BY evidence.occurred_at, evidence.message_id LIMIT 1),
 p.participant_id, p.person_id, p.participation, `+callCategorySQL("p.confirmed")+` = 'answered'
 FROM selected i LEFT JOIN call_participants p ON p.identity_key = i.identity_key
 ORDER BY i.occurred_at, i.identity_key, p.participant_id`), args...)
	if err != nil {
		return nil, "", fmt.Errorf("list logical calls: %w", err)
	}
	defer func() { _ = rows.Close() }()
	calls := []LogicalCall{}
	for rows.Next() {
		var c LogicalCall
		var at requiredTimestamp
		var duration sql.NullFloat64
		var metadata sql.NullString
		var participantID, personID sql.NullInt64
		var participation sql.NullString
		var credited sql.NullBool
		if err := rows.Scan(&c.IdentityKey, &c.MessageID, &c.Kind, &at, &c.Outcome, &duration, &metadata, &participantID, &personID, &participation, &credited); err != nil {
			return nil, "", fmt.Errorf("scan logical call: %w", err)
		}
		if !metadata.Valid {
			return nil, "", fmt.Errorf("logical call %q has no evidence metadata", c.IdentityKey)
		}
		var evidence callhistory.Evidence
		if err := json.Unmarshal([]byte(metadata.String), &evidence); err != nil {
			return nil, "", fmt.Errorf("decode logical call evidence: %w", err)
		}
		c.OccurredAt = at.Time.UTC()
		c.Precision = evidence.Precision
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
		return nil, "", fmt.Errorf("read logical calls: %w", err)
	}
	next := ""
	if len(calls) > q.Limit {
		calls = calls[:q.Limit]
		last := calls[len(calls)-1]
		next = last.OccurredAt.UTC().Format(time.RFC3339Nano) + "|" + last.IdentityKey
	}
	return calls, next, nil
}

func logicalCallDateBounds(q LogicalCallQuery) (time.Time, time.Time, bool, error) {
	if q.DateAfter == "" && q.DateBefore == "" {
		return time.Time{}, time.Time{}, false, nil
	}
	if q.DateAfter == "" || q.DateBefore == "" {
		return time.Time{}, time.Time{}, false, fmt.Errorf("%w: logical call date bounds", callhistory.ErrInvalid)
	}
	after, err := time.Parse(time.DateOnly, q.DateAfter)
	if err != nil {
		return time.Time{}, time.Time{}, false, fmt.Errorf("%w: logical call date bounds", callhistory.ErrInvalid)
	}
	before, err := time.Parse(time.DateOnly, q.DateBefore)
	if err != nil || !after.Before(before) {
		return time.Time{}, time.Time{}, false, fmt.Errorf("%w: logical call date bounds", callhistory.ErrInvalid)
	}
	return after, before, true, nil
}

func validLogicalCallTime(at time.Time) bool {
	return !at.IsZero() && at.Year() >= 1 && at.Year() <= 9999
}

func callParticipantsCTE() string {
	return `call_participants AS (
 SELECT a.identity_key, r.participant_id, COALESCE(MAX(pp.person_id),0) AS person_id,
 CASE WHEN MAX(CASE WHEN r.recipient_type = 'call_joined' THEN 1 ELSE 0 END) = 1 THEN 'joined'
 WHEN MAX(CASE WHEN r.recipient_type = 'call_invited' THEN 1 ELSE 0 END) = 1 THEN 'invited'
 ELSE 'unknown' END AS participation,
 MAX(` + callConfirmedSQL() + `) AS confirmed,
 MAX(CASE WHEN (` + callConfirmedSQL() + `) = 1 AND a.direction = 'incoming' THEN 1 ELSE 0 END) AS incoming,
 MAX(CASE WHEN (` + callConfirmedSQL() + `) = 1 AND a.direction = 'outgoing' THEN 1 ELSE 0 END) AS outgoing
 FROM active a JOIN message_recipients r ON r.message_id = a.message_id
 LEFT JOIN person_participants pp ON pp.participant_id = r.participant_id
 WHERE r.recipient_type IN ('call_joined','call_invited','call_unknown')
 GROUP BY a.identity_key, r.participant_id)`
}

// CallContactAggregateContext summarizes credited logical calls for one subject.
// Calls use whole-observation credit, independently of message noise filters.
// Initiation remains message-only. The composer labels this aggregate as calls;
// the store channel stays Other to preserve the activity channel contract.
func (s *Store) CallContactAggregateContext(ctx context.Context, q ContactQuery) (ContactAggregate, error) {
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
	// Select only this subject's identities, then include all their active evidence
	// so another observation can override credit without changing the call date.
	ctes, args := callScopedIdentityCTEs(ctx, `c.message_id IN (SELECT r.message_id FROM message_recipients r
 WHERE r.recipient_type IN ('call_joined','call_invited','call_unknown')
 AND `+candidateSubject+`)`, subjectArgs...)
	args = append(args, subjectArgs...)
	args = append(args, q.Now.UTC())
	rows, err := s.db.QueryContext(ctx, s.Rebind(`WITH `+ctes+`
	 SELECT i.identity_key, i.occurred_at, MAX(p.incoming), MAX(p.outgoing),
	 (SELECT m.metadata FROM active evidence JOIN messages m ON m.id = evidence.message_id
	  WHERE evidence.identity_key = i.identity_key
	  ORDER BY evidence.occurred_at, evidence.message_id LIMIT 1)
 FROM identities i JOIN call_participants p ON p.identity_key = i.identity_key
 WHERE `+subject+` AND `+callCategorySQL("p.confirmed")+` = 'answered' AND i.occurred_at <= ?
 GROUP BY i.identity_key, i.occurred_at ORDER BY i.occurred_at, i.identity_key`), args...)
	if err != nil {
		return result, fmt.Errorf("aggregate call contact: %w", err)
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
		var metadata sql.NullString
		if err := rows.Scan(&identity, &timestamp, &incoming, &outgoing, &metadata); err != nil {
			return ContactAggregate{}, fmt.Errorf("scan call contact: %w", err)
		}
		if !metadata.Valid {
			return ContactAggregate{}, fmt.Errorf("call identity %q has no evidence metadata", identity)
		}
		var evidence callhistory.Evidence
		if err := json.Unmarshal([]byte(metadata.String), &evidence); err != nil {
			return ContactAggregate{}, fmt.Errorf("decode call contact evidence: %w", err)
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
		if evidence.Precision == "day" {
			local = at
		}
		monthsAgo := (now.Year()-local.Year())*12 + int(now.Month()-local.Month())
		if monthsAgo >= 0 && monthsAgo < len(result.Monthly) {
			result.Monthly[len(result.Monthly)-1-monthsAgo]++
		}
	}
	if err := rows.Err(); err != nil {
		return ContactAggregate{}, fmt.Errorf("read call contact: %w", err)
	}
	if result.Count > 0 {
		result.Channels = []ActivityChannel{ChannelOther}
	}
	return result, nil
}

// A window or participant predicate bounds candidates. Canonical identities then
// use the canonical-key index; empty keys use message primary-key lookups, never
// all other empty-key observations. Authorization applies to every evidence row.
func callScopedIdentityCTEs(ctx context.Context, candidateWhere string, candidateArgs ...any) (string, []any) {
	scope, scopeArgs := sourceScopeClause(ctx, "m.source_id")
	active := callActiveSQL(scope+` AND c.canonical_key IN (SELECT canonical_key FROM candidates WHERE canonical_key <> '')`) +
		" UNION ALL " + callActiveSQL(scope+` AND c.message_id IN (SELECT message_id FROM candidates WHERE canonical_key = '')`)
	ctes := `candidates AS (
 SELECT c.canonical_key, c.message_id FROM call_details c JOIN messages m ON m.id = c.message_id
 WHERE NOT c.withdrawn AND m.deleted_at IS NULL AND m.deleted_from_source_at IS NULL` + scope + `
 AND (` + candidateWhere + `)), ` + callIdentityCTEs(active) + `, ` + callParticipantsCTE()
	args := append([]any{}, scopeArgs...)
	args = append(args, candidateArgs...)
	args = append(args, scopeArgs...)
	args = append(args, scopeArgs...)
	return ctes, args
}
