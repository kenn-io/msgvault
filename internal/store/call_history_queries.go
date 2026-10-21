package store

import (
	"context"
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
		where = append(where, `EXISTS (SELECT 1 FROM message_recipients r
			JOIN person_participants pp ON pp.participant_id = r.participant_id
			WHERE r.message_id = m.id AND pp.person_id = ?
			AND r.recipient_type IN ('call_joined', 'call_invited', 'call_unknown'))`)
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
		rows, err := tx.QueryContext(ctx, `SELECT m.id, m.source_id, m.source_message_id, m.metadata
			FROM call_details c JOIN messages m ON m.id = c.message_id
			WHERE `+strings.Join(where, " AND ")+` ORDER BY m.id LIMIT ?`, args...)
		if err != nil {
			return err
		}
		for rows.Next() {
			var view CallObservationView
			var metadata string
			if err := rows.Scan(&view.MessageID, &view.SourceID, &view.RecordID, &metadata); err != nil {
				_ = rows.Close()
				return err
			}
			if err := json.Unmarshal([]byte(metadata), &view.Evidence); err != nil {
				_ = rows.Close()
				return fmt.Errorf("decode call evidence: %w", err)
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
				FROM message_recipients WHERE message_id = ?
				AND recipient_type IN ('call_joined', 'call_invited', 'call_unknown')
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
			FROM call_details c JOIN messages m ON m.id = c.message_id
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
			FROM active a JOIN message_recipients r ON r.message_id = a.message_id
			JOIN person_participants pp ON pp.participant_id = r.participant_id
			WHERE r.recipient_type IN ('call_joined','call_invited','call_unknown')
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
