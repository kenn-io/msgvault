package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"

	"go.kenn.io/msgvault/internal/calrecur"
)

func readMeetingLinkDecisions(ctx context.Context, r loggedReader, memberID int64) (out []meetingLinkDecision, err error) {
	rows, err := r.QueryContext(ctx, `SELECT target_message_id,occurrence_key,decision FROM meeting_link_decisions WHERE member_message_id = ? ORDER BY target_message_id,occurrence_key`, memberID)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, rows.Close()) }()
	for rows.Next() {
		var d meetingLinkDecision
		if err := rows.Scan(&d.Ref.MessageID, &d.Ref.Key, &d.Decision); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// LinkMeetingContext adds the owner's chosen link to automatic evidence.
func (s *Store) LinkMeetingContext(ctx context.Context, memberID int64, occurrence OccurrenceRef) error {
	return s.writeMeetingLinkDecision(ctx, memberID, occurrence, "link")
}

// UnlinkMeetingContext splits an occurrence and copies with the same identity.
func (s *Store) UnlinkMeetingContext(ctx context.Context, memberID int64, occurrence OccurrenceRef) error {
	return s.writeMeetingLinkDecision(ctx, memberID, occurrence, "split")
}

func (s *Store) writeMeetingLinkDecision(ctx context.Context, memberID int64, occurrence OccurrenceRef, decision string) error {
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		first, second := memberID, occurrence.MessageID
		if first > second {
			first, second = second, first
		}
		for _, id := range []int64{first, second} {
			if s.IsPostgreSQL() {
				var found int64
				if err := tx.QueryRowContext(ctx, `SELECT id FROM messages WHERE id = ? FOR NO KEY UPDATE`, id).Scan(&found); err != nil && !errors.Is(err, sql.ErrNoRows) {
					return err
				}
			} else if err := s.lockMeetingEvidenceWith(ctx, tx, id); err != nil {
				return err
			}
		}
		var found int64
		if err := tx.QueryRowContext(ctx, `SELECT id FROM messages WHERE id = ? AND message_type = 'meeting_transcript' AND deleted_at IS NULL`, memberID).Scan(&found); err != nil {
			return err
		}
		r := (reboundReader{loggedReader: tx, dialect: s.dialect}).withMeetingCache()
		budget := calrecur.RequestReadBudget
		decisions, err := readMeetingLinkDecisions(ctx, r, memberID)
		if err != nil {
			return err
		}
		target, err := resolveMeetingOccurrence(ctx, r, occurrence, &budget)
		if decision == "split" && (errors.Is(err, sql.ErrNoRows) || errors.Is(err, calrecur.ErrUnsupportedRecurrence)) && slices.ContainsFunc(decisions, func(d meetingLinkDecision) bool { return d.Ref == occurrence }) {
			target, _, err = savedOccurrence(ctx, r, occurrence, &budget)
		}
		if err != nil {
			return fmt.Errorf("resolve meeting decision target: %w", err)
		}
		for _, d := range decisions {
			previous, _, err := savedOccurrence(ctx, r, d.Ref, &budget)
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			if err != nil {
				return err
			}
			if previous.Identity == target.Identity {
				if _, err := tx.ExecContext(ctx, `DELETE FROM meeting_link_decisions WHERE member_message_id = ? AND target_message_id = ? AND occurrence_key = ?`, memberID, d.Ref.MessageID, d.Ref.Key); err != nil {
					return err
				}
			}
		}
		_, err = tx.ExecContext(ctx, meetingDecisionUpsert, memberID, target.Ref.MessageID, target.Ref.Key, decision)
		if err != nil {
			return err
		}
		budget = calrecur.RequestReadBudget
		_, err = s.meetingNoteRecord(ctx, r.withMeetingCache(), memberID, &budget)
		return err
	})
}

// A bare reference saved before a standalone event became a series names its first occurrence.
func storedOccurrenceKey(row CalendarEventRow, key string) string {
	first := firstOccurrenceKey(row)
	if row.Event.Kind == meetingSeriesKind && key == "" {
		return first
	}
	if row.Event.Kind == calendarSingleKind && key != "" && key == first {
		return ""
	}
	return key
}

func firstOccurrenceKey(row CalendarEventRow) string {
	if row.Event.Start == nil {
		return ""
	}
	return calrecur.Identity{Instant: *row.Event.Start, Date: row.Event.StartDate, AllDay: row.Event.AllDay}.Key()
}

// Retain the original decisions for undo; purge removes only that hidden copy.
func copyMeetingLinkDecisionsTx(ctx context.Context, tx *loggedTx, from, to int64) error {
	if from == to {
		return nil
	}
	rows, err := tx.QueryContext(ctx, `SELECT member_message_id,target_message_id,occurrence_key,decision FROM meeting_link_decisions WHERE member_message_id = ? OR target_message_id = ?`, from, from)
	if err != nil {
		return err
	}
	type copyDecision struct {
		member        int64
		target        int64
		key, decision string
	}
	var decisions []copyDecision
	for rows.Next() {
		var d copyDecision
		if err := rows.Scan(&d.member, &d.target, &d.key, &d.decision); err != nil {
			_ = rows.Close()
			return err
		}
		decisions = append(decisions, d)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	for _, d := range decisions {
		if d.member == from {
			d.member = to
		}
		if d.target == from {
			d.target = to
		}
		_, err := tx.ExecContext(ctx, meetingDecisionCopy, d.member, d.target, d.key, d.decision)
		if err != nil {
			return fmt.Errorf("copy meeting decision: %w", err)
		}
	}
	return nil
}

const meetingDecisionUpsert = `INSERT INTO meeting_link_decisions(member_message_id,target_message_id,occurrence_key,decision) VALUES (?,?,?,?) ON CONFLICT(member_message_id,target_message_id,occurrence_key) DO UPDATE SET decision = excluded.decision`
const meetingDecisionCopy = `INSERT INTO meeting_link_decisions(member_message_id,target_message_id,occurrence_key,decision) VALUES (?,?,?,?) ON CONFLICT(member_message_id,target_message_id,occurrence_key) DO NOTHING`
