package store

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"fmt"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/callhistory"
)

// CallQuery uses an inclusive After and exclusive Before. Evidence pages use
// AfterID; ranked person summaries use Offset. Limits default to 50, at most 100.
type CallQuery struct {
	PersonID int64
	After    *time.Time
	Before   *time.Time
	AfterID  int64
	Limit    int
	Offset   int
}

func (q CallQuery) limit() (int, error) {
	for _, bound := range []*time.Time{q.After, q.Before} {
		if bound != nil && (bound.IsZero() || bound.Year() < 1 || bound.Year() > 9999) {
			return 0, fmt.Errorf("%w: query timestamp", callhistory.ErrInvalid)
		}
	}
	if q.PersonID < 0 || q.AfterID < 0 || q.Offset < 0 || q.Offset > 1000000 ||
		q.Limit < 0 || q.Limit > 100 ||
		(q.After != nil && q.Before != nil && !q.After.Before(*q.Before)) {
		return 0, fmt.Errorf("%w: query bounds", callhistory.ErrInvalid)
	}
	if q.Limit == 0 {
		return 50, nil
	}
	return q.Limit, nil
}

type CallObservationView struct {
	callhistory.Evidence

	MessageID    int64                     `json:"message_id"`
	SourceID     int64                     `json:"source_id"`
	RecordID     string                    `json:"record_id"`
	Participants []callhistory.Participant `json:"participants"`
}

type PersonCallSummary struct {
	PersonID         int64 `json:"person_id"`
	Answered         int   `json:"answered"`
	Missed           int   `json:"missed"`
	Declined         int   `json:"declined"`
	Cancelled        int   `json:"cancelled"`
	Unknown          int   `json:"unknown"`
	ExampleMessageID int64 `json:"example_message_id"`
}

// callRowsSQL counts any connected seconds as answered in the summary.
// MinCallSeconds is the tier's contact threshold. Calls without connected time
// stay unknown, and undated legacy calls are excluded from both call readers.
func (s *Store) callRowsSQL() string {
	seconds := s.callConnectedSecondsSQL()
	return `SELECT message_id, canonical_key, '' AS direction,
		outcome, owner_participation, occurred_at, NULL AS duration_seconds FROM call_details
		WHERE EXISTS (SELECT 1 FROM messages m WHERE m.id = message_id AND m.message_type = '` + callhistory.MessageType + `')
		UNION ALL
		SELECT m.id, '',
			CASE WHEN m.is_from_me THEN 'outgoing' ELSE 'incoming' END,
			CASE WHEN ` + seconds + ` > 0 THEN 'answered' ELSE 'unknown' END,
			CASE WHEN ` + seconds + ` > 0 THEN 'joined' ELSE 'unknown' END,
			m.sent_at, NULLIF(` + seconds + `, 0)
		FROM messages m WHERE m.sent_at IS NOT NULL
		AND m.message_type IN ('` + MessageTypeSynctechCall + `','` + MessageTypeGoogleVoiceCall + `')`
}

// callRolesSQL credits only the counterparty, whoever dialed, as personTierRows does.
func (s *Store) callRolesSQL() string {
	return `SELECT r.message_id, r.participant_id,
		CASE WHEN r.recipient_type IN ('call_joined', 'call_invited', 'call_unknown') THEN r.recipient_type
			WHEN ` + s.callConnectedSecondsSQL() + ` > 0 THEN 'call_joined' ELSE 'call_unknown' END AS recipient_type
		FROM message_recipients r JOIN messages m ON m.id = r.message_id
		WHERE (r.recipient_type IN ('call_joined', 'call_invited', 'call_unknown') OR
			(m.message_type IN ('` + MessageTypeSynctechCall + `','` + MessageTypeGoogleVoiceCall + `')
			AND r.recipient_type = CASE WHEN m.is_from_me THEN 'to' ELSE 'from' END))
		AND r.participant_id NOT IN (` + ownerParticipantIDsSQL + `)
		AND NOT EXISTS (SELECT 1 FROM person_participants pp
			WHERE pp.participant_id = r.participant_id AND pp.person_id IN (` + fmt.Sprintf(personIDsForParticipantsSQL, ownerParticipantIDsSQL) + `))`
}

// ListCallObservationsContext excludes archive-deleted and source-deleted calls.
// It never reads raw payloads or bodies.
func (s *Store) ListCallObservationsContext(ctx context.Context, q CallQuery) ([]CallObservationView, error) {
	if q.Offset != 0 {
		return nil, fmt.Errorf("%w: evidence pages require after_id", callhistory.ErrInvalid)
	}
	limit, err := q.limit()
	if err != nil {
		return nil, err
	}
	where := []string{"m.deleted_at IS NULL", "m.deleted_from_source_at IS NULL", "m.id > ?"}
	args := []any{q.AfterID}
	if q.PersonID != 0 {
		where = append(where, `EXISTS (SELECT 1 FROM (`+s.callRolesSQL()+`) r
			JOIN person_participants pp ON pp.participant_id = r.participant_id
			WHERE r.message_id = m.id AND pp.person_id = ?)`)
		args = append(args, q.PersonID)
	}
	if q.After != nil {
		where, args = append(where, "c.occurred_at >= ?"), append(args, q.After.UTC())
	}
	if q.Before != nil {
		where, args = append(where, "c.occurred_at < ?"), append(args, q.Before.UTC())
	}
	args = append(args, limit)
	out := []CallObservationView{}
	err = s.withReadSnapshotContext(ctx, func(tx *loggedTx) error {
		rows, err := tx.QueryContext(ctx, `SELECT m.id, m.source_id, m.source_message_id, m.metadata, m.message_type,
			src.source_type, src.identifier, c.direction, c.outcome,
			c.owner_participation, c.occurred_at, c.duration_seconds
			FROM (`+s.callRowsSQL()+`) c JOIN messages m ON m.id = c.message_id
			JOIN sources src ON src.id = m.source_id
			WHERE `+strings.Join(where, " AND ")+` ORDER BY m.id LIMIT ?`, args...)
		if err != nil {
			return err
		}
		for rows.Next() {
			var view CallObservationView
			var metadata sql.NullString
			var messageType string
			if err := rows.Scan(&view.MessageID, &view.SourceID, &view.RecordID, &metadata, &messageType,
				&view.Provider, &view.Account, &view.Direction, &view.Outcome,
				&view.OwnerParticipation, &view.OccurredAt, &view.DurationSeconds); err != nil {
				_ = rows.Close()
				return err
			}
			if messageType == callhistory.MessageType {
				if err := json.Unmarshal([]byte(metadata.String), &view.Evidence); err != nil {
					_ = rows.Close()
					return fmt.Errorf("decode call evidence: %w", err)
				}
			} else {
				view.Version = 1
			}
			view.Participants = []callhistory.Participant{}
			out = append(out, view)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return err
		}
		if err := rows.Close(); err != nil {
			return err
		}
		// A single snapshot keeps roles consistent with the scalar evidence,
		// including during concurrent replacement or participant merges.
		ids := make([]int64, len(out))
		indices := make(map[int64]int, len(out))
		for i := range out {
			ids[i], indices[out[i].MessageID] = out[i].MessageID, i
		}
		return queryInChunksContext(ctx, tx, ids, nil, `SELECT message_id, participant_id,
			CASE WHEN MAX(CASE WHEN recipient_type = 'call_joined' THEN 1 ELSE 0 END) = 1 THEN 'joined'
			     WHEN MAX(CASE WHEN recipient_type = 'call_invited' THEN 1 ELSE 0 END) = 1 THEN 'invited'
			     ELSE 'unknown' END
			FROM (`+s.callRolesSQL()+`) r WHERE message_id IN (%s)
			GROUP BY message_id, participant_id ORDER BY message_id, participant_id`, func(rows *loggedRows) error {
			var messageID int64
			var p callhistory.Participant
			if err := rows.Scan(&messageID, &p.ID, &p.Participation); err != nil {
				return err
			}
			i := indices[messageID]
			out[i].Participants = append(out[i].Participants, p)
			return nil
		})
	})
	if err != nil {
		return nil, fmt.Errorf("list call observations: %w", err)
	}
	return out, nil
}

// SummarizeCallsContext counts logical calls within the time window.
// Each observation must establish attendance on its own.
// Current curated person bindings determine whose evidence is counted.
func (s *Store) SummarizeCallsContext(ctx context.Context, q CallQuery) ([]PersonCallSummary, error) {
	if q.AfterID != 0 {
		return nil, fmt.Errorf("%w: summary pages require offset", callhistory.ErrInvalid)
	}
	limit, err := q.limit()
	if err != nil {
		return nil, err
	}
	where, args := []string{"1 = 1"}, []any{}
	if q.PersonID != 0 {
		where, args = append(where, "p.person_id = ?"), append(args, q.PersonID)
	}
	if q.After != nil {
		where, args = append(where, "i.occurred_at >= ?"), append(args, q.After.UTC())
	}
	if q.Before != nil {
		where, args = append(where, "i.occurred_at < ?"), append(args, q.Before.UTC())
	}
	args = append(args, limit, q.Offset)
	scope, scopeArgs := sourceScopeClause(ctx, "m.source_id")
	args = append(scopeArgs, args...)
	out := []PersonCallSummary{}
	err = s.withReadSnapshotContext(ctx, func(tx *loggedTx) error {
		rows, err := tx.QueryContext(ctx, `
		WITH `+callIdentityCTEs(`SELECT c.*, `+callIdentitySQL("c")+` AS identity_key
			FROM (`+s.callRowsSQL()+`) c JOIN messages m ON m.id = c.message_id
			WHERE m.deleted_at IS NULL AND m.deleted_from_source_at IS NULL`+scope)+`, per_person AS (
			SELECT a.identity_key, MIN(a.message_id) AS message_id, pp.person_id,
				MAX(`+callConfirmedSQL()+`) AS confirmed
			FROM active a JOIN (`+s.callRolesSQL()+`) r ON r.message_id = a.message_id
			JOIN person_participants pp ON pp.participant_id = r.participant_id
			GROUP BY a.identity_key, pp.person_id
		), classified AS (
			SELECT p.person_id, p.message_id AS example_id,
				`+callCategorySQL("p.confirmed")+` AS category
			FROM per_person p JOIN identities i ON i.identity_key = p.identity_key
			WHERE `+strings.Join(where, " AND ")+`
		)
		SELECT person_id,
			SUM(CASE WHEN category = 'answered' THEN 1 ELSE 0 END) AS answered,
			SUM(CASE WHEN category = 'missed' THEN 1 ELSE 0 END),
			SUM(CASE WHEN category = 'declined' THEN 1 ELSE 0 END),
			SUM(CASE WHEN category = 'cancelled' THEN 1 ELSE 0 END),
			SUM(CASE WHEN category = 'unknown' THEN 1 ELSE 0 END),
			MIN(example_id)
		FROM classified GROUP BY person_id ORDER BY answered DESC, person_id
		LIMIT ? OFFSET ?`, args...)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var v PersonCallSummary
			if err := rows.Scan(&v.PersonID, &v.Answered, &v.Missed, &v.Declined, &v.Cancelled,
				&v.Unknown, &v.ExampleMessageID); err != nil {
				return fmt.Errorf("scan call summary: %w", err)
			}
			out = append(out, v)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("summarize calls: %w", err)
	}
	return out, nil
}

// Records sharing a provider/account/native call ID are one call; a record
// without that ID is its own call.
func callIdentitySQL(alias string) string {
	return "CASE WHEN " + alias + ".canonical_key = '' THEN 'm:' || CAST(" + alias + ".message_id AS TEXT) ELSE 'n:' || " + alias + ".canonical_key END"
}

func (s *Store) callActiveSQL(extra string) string {
	// Direction and duration remain in normalized evidence, outside the scalar
	// projection used by the summary. Extract only those fields for this reader.
	direction := "m.metadata ->> 'direction'"
	duration := "m.metadata ->> 'duration_seconds'"
	if s.dialect.DriverName() != postgresDriverName {
		direction = "CASE WHEN json_valid(m.metadata) THEN " + direction + " END"
		duration = "CASE WHEN json_valid(m.metadata) THEN " + duration + " END"
	}
	return `SELECT c.message_id, c.canonical_key, c.kind, c.outcome, c.owner_participation,
 c.occurred_at, CASE WHEN m.message_type = 'call_record' THEN ` + direction + ` ELSE c.direction END AS direction,
 CASE WHEN m.message_type = 'call_record' THEN CAST(` + duration + ` AS DOUBLE PRECISION) ELSE c.duration_seconds END AS duration_seconds,
 ` + callIdentitySQL("c") + ` AS identity_key
 FROM (` + s.callRowsSQL() + `) c JOIN messages m ON m.id = c.message_id
 WHERE m.deleted_at IS NULL AND m.deleted_from_source_at IS NULL` + extra
}

// An identity takes its earliest record's time. Any whole observation can
// establish an answer; negative outcomes count only when its records agree.
func callIdentityCTEs(active string) string {
	negative := "CASE WHEN outcome IN ('missed','declined','cancelled') THEN outcome END"
	return `active AS (` + active + `), identities AS (
 SELECT identity_key, MIN(occurred_at) AS occurred_at, MIN(kind) AS kind,
 MIN(message_id) AS message_id, MAX(duration_seconds) AS duration_seconds,
 CASE WHEN MIN(` + negative + `) = MAX(` + negative + `) THEN MIN(` + negative + `) ELSE 'unknown' END AS outcome
 FROM active GROUP BY identity_key)`
}

func callCategorySQL(confirmed string) string {
	return fmt.Sprintf(`CASE WHEN %s = 1 THEN 'answered'
 WHEN i.outcome IN ('missed','declined','cancelled') THEN i.outcome
 ELSE 'unknown' END`, confirmed)
}

func callConfirmedSQL() string {
	return `CASE WHEN a.outcome = 'answered' AND a.owner_participation = 'joined'
 AND r.recipient_type = 'call_joined' THEN 1 ELSE 0 END`
}

