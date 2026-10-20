package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"go.kenn.io/msgvault/internal/calrecur"
)

func readMeetingLinkDecisions(ctx context.Context, r SQLReader, memberID int64) (out []meetingLinkDecision, err error) {
	rows, err := r.QueryContext(ctx, `SELECT target_ical_uid,COALESCE(target_message_id,0),occurrence_key,decision,created_at FROM meeting_link_decisions WHERE member_message_id = ? ORDER BY target_ical_uid,target_message_id,occurrence_key`, memberID)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, rows.Close()) }()
	for rows.Next() {
		var d meetingLinkDecision
		if err := rows.Scan(&d.Identity.ICalUID, &d.Identity.EventMessageID, &d.Identity.OccurrenceKey, &d.Decision, &d.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// LinkMeetingContext replaces automatic evidence with the owner's chosen links.
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
				if err := tx.QueryRowContext(ctx, `SELECT id FROM messages WHERE id = ? FOR NO KEY UPDATE`, id).Scan(&found); err != nil {
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
		target, err := resolveMeetingDecisionTarget(ctx, reboundReader{reader: tx.Tx, rebind: s.dialect.Rebind, dialect: s.dialect}, memberID, occurrence, decision)
		if err != nil {
			return fmt.Errorf("resolve meeting decision target: %w", err)
		}
		_, err = tx.ExecContext(ctx, meetingDecisionUpsert, memberID, target.Identity.ICalUID, sql.NullInt64{Int64: target.Identity.EventMessageID, Valid: target.Identity.ICalUID == ""}, target.Identity.OccurrenceKey, decision, time.Now().UTC())
		return err
	})
}

func resolveMeetingDecisionTarget(ctx context.Context, r reboundReader, memberID int64, ref OccurrenceRef, decision string) (linkOccurrence, error) {
	budget := calrecur.RequestReadBudget
	target, err := resolveMeetingOccurrence(ctx, r, ref, &budget)
	if decision != "split" || !errors.Is(err, sql.ErrNoRows) && !errors.Is(err, calrecur.ErrUnsupportedRecurrence) && !errors.Is(err, ErrQueryTooBroad) {
		return target, err
	}
	// A series edit can remove an occurrence while its saved link still
	// suppresses automatic matches. Only supersede a saved identity here.
	rows, err := meetingEventRows(ctx, r, `e.message_id = ?`, []any{ref.MessageID}, &budget)
	if err != nil {
		return linkOccurrence{}, err
	}
	if len(rows) != 1 {
		return linkOccurrence{}, sql.ErrNoRows
	}
	target = occurrenceFromRow(rows[0], ref.Key)
	var found int
	err = r.QueryRowContext(ctx, `SELECT 1 FROM meeting_link_decisions WHERE member_message_id = ? AND target_ical_uid = ? AND COALESCE(target_message_id,0) = ? AND occurrence_key = ?`, memberID, target.Identity.ICalUID, target.Identity.EventMessageID, target.Identity.OccurrenceKey).Scan(&found)
	return target, err
}

// Retain the original decisions for undo; purge removes only that hidden copy.
func copyMeetingLinkDecisionsTx(ctx context.Context, tx *loggedTx, from, to int64) error {
	if from == to {
		return nil
	}
	rows, err := tx.QueryContext(ctx, `SELECT member_message_id,target_ical_uid,target_message_id,occurrence_key,decision,created_at FROM meeting_link_decisions WHERE member_message_id = ? OR target_message_id = ?`, from, from)
	if err != nil {
		return err
	}
	type copyDecision struct {
		member             int64
		target             sql.NullInt64
		uid, key, decision string
		created            time.Time
	}
	var decisions []copyDecision
	for rows.Next() {
		var d copyDecision
		if err := rows.Scan(&d.member, &d.uid, &d.target, &d.key, &d.decision, &d.created); err != nil {
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
		if d.target.Valid && d.target.Int64 == from {
			d.target.Int64 = to
		}
		_, err := tx.ExecContext(ctx, meetingDecisionUpsert+` WHERE meeting_link_decisions.created_at < excluded.created_at OR (meeting_link_decisions.created_at = excluded.created_at AND excluded.decision = 'split')`, d.member, d.uid, d.target, d.key, d.decision, d.created)
		if err != nil {
			return fmt.Errorf("copy meeting decision: %w", err)
		}
	}
	return nil
}

const meetingDecisionUpsert = `INSERT INTO meeting_link_decisions(member_message_id,target_ical_uid,target_message_id,occurrence_key,decision,created_at) VALUES (?,?,?,?,?,?) ON CONFLICT(member_message_id,target_ical_uid,(COALESCE(target_message_id,0)),occurrence_key) DO UPDATE SET decision = excluded.decision,created_at = excluded.created_at`
