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
	PersonID           int64 `json:"person_id"`
	Answered           int   `json:"answered"`
	Missed             int   `json:"missed"`
	Declined           int   `json:"declined"`
	Cancelled          int   `json:"cancelled"`
	Unknown            int   `json:"unknown"`
	Conflicting        int   `json:"conflicting"`
	Scheduled          int   `json:"scheduled"`
	ScheduledCancelled int   `json:"scheduled_cancelled"`
	ExampleMessageID   int64 `json:"example_message_id"`
}

// callRowsSQL counts any connected seconds as answered in the summary.
// MinCallSeconds is the tier's contact threshold. Calls without connected time
// stay unknown, and undated legacy calls are excluded from both call readers.
func (s *Store) callRowsSQL() string {
	seconds := s.callConnectedSecondsSQL()
	return `SELECT message_id, canonical_key, kind, direction,
		outcome, owner_participation, occurred_at, duration_seconds, withdrawn FROM call_details
		UNION ALL
		SELECT m.id, '', 'call',
			CASE WHEN m.is_from_me THEN 'outgoing' ELSE 'incoming' END,
			CASE WHEN ` + seconds + ` > 0 THEN 'answered' ELSE 'unknown' END,
			CASE WHEN ` + seconds + ` > 0 THEN 'joined' ELSE 'unknown' END,
			m.sent_at, NULLIF(` + seconds + `, 0), FALSE
		FROM messages m WHERE m.sent_at IS NOT NULL
		AND m.message_type IN ('` + MessageTypeSynctechCall + `','` + MessageTypeGoogleVoiceCall + `')`
}

// callRolesSQL credits only the counterparty, whoever dialed, as personTierRows does.
func (s *Store) callRolesSQL() string {
	return `SELECT r.message_id, r.participant_id, r.recipient_type FROM message_recipients r
		JOIN participants p ON p.id = r.participant_id
		WHERE r.recipient_type IN ('call_joined', 'call_invited', 'call_unknown')
		AND NOT EXISTS (SELECT 1 FROM account_identities ai
			WHERE (` + ownerEmailMatch("p.email_address", "ai.address") + `) OR EXISTS (
				SELECT 1 FROM participant_identifiers pi WHERE pi.participant_id = p.id AND ` +
		ownerIdentifierMatch("pi.identifier_type", "pi.identifier_value", "ai.address", identifierWithoutPrimaryEmail) + `))
		UNION ALL
		SELECT m.id, r.participant_id,
			CASE WHEN ` + s.callConnectedSecondsSQL() + ` > 0 THEN 'call_joined' ELSE 'call_unknown' END
		FROM messages m JOIN message_recipients r ON r.message_id = m.id
		WHERE m.message_type IN ('` + MessageTypeSynctechCall + `','` + MessageTypeGoogleVoiceCall + `')
		AND r.recipient_type = CASE WHEN m.is_from_me THEN 'to' ELSE 'from' END`
}

// ListCallObservationsContext includes withdrawn evidence for inspection but
// respects archive and source deletion. It never reads raw payloads or bodies.
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
			src.source_type, src.identifier, c.kind, c.direction, c.outcome,
			c.owner_participation, c.occurred_at, c.duration_seconds, c.withdrawn
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
				&view.Provider, &view.Account, &view.Kind, &view.Direction, &view.Outcome,
				&view.OwnerParticipation, &view.OccurredAt, &view.DurationSeconds, &view.Withdrawn); err != nil {
				_ = rows.Close()
				return err
			}
			if messageType == callhistory.MessageType {
				if err := json.Unmarshal([]byte(metadata.String), &view.Evidence); err != nil {
					_ = rows.Close()
					return fmt.Errorf("decode call evidence: %w", err)
				}
			} else {
				view.Version, view.Precision = 1, "instant"
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
		for i := range out {
			rows, err := tx.QueryContext(ctx, `SELECT participant_id,
				CASE WHEN MAX(CASE WHEN recipient_type = 'call_joined' THEN 1 ELSE 0 END) = 1 THEN 'joined'
				     WHEN MAX(CASE WHEN recipient_type = 'call_invited' THEN 1 ELSE 0 END) = 1 THEN 'invited'
				     ELSE 'unknown' END
				FROM (`+s.callRolesSQL()+`) r WHERE message_id = ?
				GROUP BY participant_id ORDER BY participant_id`, out[i].MessageID)
			if err != nil {
				return err
			}
			for rows.Next() {
				var p callhistory.Participant
				if err := rows.Scan(&p.ID, &p.Participation); err != nil {
					_ = rows.Close()
					return err
				}
				out[i].Participants = append(out[i].Participants, p)
			}
			err = rows.Err()
			closeErr := rows.Close()
			if err != nil {
				return err
			}
			if closeErr != nil {
				return closeErr
			}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("list call observations: %w", err)
	}
	return out, nil
}

// SummarizeCallsContext groups all active observations before applying the
// time window. Exact identities never combine partial attendance claims.
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
	rows, err := s.db.QueryContext(ctx, s.Rebind(`
		WITH active AS (
			SELECT c.*, CASE WHEN c.canonical_key = '' THEN 'm:' || CAST(c.message_id AS TEXT)
				ELSE 'n:' || c.canonical_key END AS identity_key
			FROM (`+s.callRowsSQL()+`) c JOIN messages m ON m.id = c.message_id
			WHERE NOT c.withdrawn AND m.deleted_at IS NULL AND m.deleted_from_source_at IS NULL
		), identities AS (
			SELECT identity_key, MIN(occurred_at) AS occurred_at, MIN(kind) AS kind,
				MAX(CASE WHEN outcome = 'answered' THEN 1 ELSE 0 END) AS answered,
				MAX(CASE WHEN owner_participation = 'not_joined' OR outcome IN ('missed','declined','cancelled')
					THEN 1 ELSE 0 END) AS negative,
				MAX(CASE WHEN outcome = 'missed' THEN 1 ELSE 0 END) AS missed,
				MAX(CASE WHEN outcome = 'declined' THEN 1 ELSE 0 END) AS declined,
				MAX(CASE WHEN outcome = 'cancelled' THEN 1 ELSE 0 END) AS cancelled
			FROM active GROUP BY identity_key
		), per_person AS (
			SELECT a.identity_key, pp.person_id, MIN(a.message_id) AS example_id,
				MAX(CASE WHEN a.outcome = 'answered' AND a.owner_participation = 'joined'
					AND r.recipient_type = 'call_joined' THEN 1 ELSE 0 END) AS confirmed
			FROM active a JOIN (`+s.callRolesSQL()+`) r ON r.message_id = a.message_id
			JOIN person_participants pp ON pp.participant_id = r.participant_id
			GROUP BY a.identity_key, pp.person_id
		), classified AS (
			SELECT p.person_id, p.example_id,
				CASE WHEN i.kind = 'scheduled_meeting' AND i.cancelled = 1 THEN 'scheduled_cancelled'
					WHEN i.kind = 'scheduled_meeting' THEN 'scheduled'
					WHEN (i.answered = 1 AND i.negative = 1) OR i.missed + i.declined + i.cancelled > 1 THEN 'conflicting'
					WHEN p.confirmed = 1 THEN 'answered'
					WHEN i.missed = 1 THEN 'missed'
					WHEN i.declined = 1 THEN 'declined'
					WHEN i.cancelled = 1 THEN 'cancelled'
					ELSE 'unknown' END AS category
			FROM per_person p JOIN identities i ON i.identity_key = p.identity_key
			WHERE `+strings.Join(where, " AND ")+`
		)
		SELECT person_id,
			SUM(CASE WHEN category = 'answered' THEN 1 ELSE 0 END) AS answered,
			SUM(CASE WHEN category = 'missed' THEN 1 ELSE 0 END),
			SUM(CASE WHEN category = 'declined' THEN 1 ELSE 0 END),
			SUM(CASE WHEN category = 'cancelled' THEN 1 ELSE 0 END),
			SUM(CASE WHEN category = 'unknown' THEN 1 ELSE 0 END),
			SUM(CASE WHEN category = 'conflicting' THEN 1 ELSE 0 END),
			SUM(CASE WHEN category IN ('scheduled', 'scheduled_cancelled') THEN 1 ELSE 0 END),
			SUM(CASE WHEN category = 'scheduled_cancelled' THEN 1 ELSE 0 END),
			MIN(example_id)
		FROM classified GROUP BY person_id ORDER BY answered DESC, person_id
		LIMIT ? OFFSET ?`), args...)
	if err != nil {
		return nil, fmt.Errorf("summarize calls: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []PersonCallSummary{}
	for rows.Next() {
		var v PersonCallSummary
		if err := rows.Scan(&v.PersonID, &v.Answered, &v.Missed, &v.Declined, &v.Cancelled,
			&v.Unknown, &v.Conflicting, &v.Scheduled, &v.ScheduledCancelled, &v.ExampleMessageID); err != nil {
			return nil, fmt.Errorf("scan call summary: %w", err)
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
