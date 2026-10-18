package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"go.kenn.io/msgvault/internal/calrecur"
)

// Keep only decisions whose named target, or orphan alias, survives the subset.
// Copied raw is reprojected after this transaction; no automatic link is copied.
func copySubsetMeetingLinkDecisions(tx *sql.Tx, result *CopyResult) error {
	present, err := sourceTableExists(tx, "meeting_link_decisions")
	if err != nil {
		return err
	}
	if !present {
		return nil
	}
	var total int64
	if err := tx.QueryRow(`SELECT count(*) FROM src.meeting_link_decisions`).Scan(&total); err != nil {
		return err
	}
	rows, err := tx.Query(`SELECT member_message_id,target_ref,target_logical,decision,created_at FROM src.meeting_link_decisions WHERE member_message_id IN (SELECT id FROM messages WHERE message_type = 'meeting_transcript') ORDER BY member_message_id,target_ref`)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	type decision struct {
		member             int64
		ref, logical, kind string
		created            time.Time
	}
	var decisions []decision
	for rows.Next() {
		var d decision
		if err := rows.Scan(&d.member, &d.ref, &d.logical, &d.kind, &d.created); err != nil {
			_ = rows.Close()
			return err
		}
		decisions = append(decisions, d)
	}
	err = rows.Err()
	closeErr := rows.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	for _, d := range decisions {
		ref, keep, err := subsetMeetingDecisionTarget(tx, d.ref)
		if err != nil {
			return fmt.Errorf("resolve copied meeting target: %w", err)
		}
		if !keep {
			continue
		}
		_, target, _, err := ParseMeetingRef(ref)
		if err != nil {
			return fmt.Errorf("parse copied meeting target: %w", err)
		}
		if _, err := tx.Exec(`INSERT INTO meeting_link_decisions(member_message_id,target_message_id,target_ref,target_logical,decision,created_at) VALUES (?, ?, ?, ?, ?, ?) ON CONFLICT(member_message_id,target_ref) DO UPDATE SET target_message_id = excluded.target_message_id,target_logical = excluded.target_logical,decision = excluded.decision,created_at = excluded.created_at WHERE meeting_link_decisions.created_at < excluded.created_at`, d.member, target.MessageID, ref, d.logical, d.kind, d.created.UTC()); err != nil {
			return fmt.Errorf("copy meeting decision: %w", err)
		}
	}
	if err := tx.QueryRow(`SELECT count(*) FROM meeting_link_decisions`).Scan(&result.MeetingLinkDecisions); err != nil {
		return err
	}
	result.OmittedMeetingLinkDecisions = total - result.MeetingLinkDecisions
	return nil
}

func subsetMeetingDecisionTarget(tx *sql.Tx, ref string) (string, bool, error) {
	kind, target, _, err := ParseMeetingRef(ref)
	if err != nil {
		return "", false, nil //nolint:nilerr // Invalid historical refs are omitted and counted in the subset report.
	}
	if kind != calendarEventRefKind {
		return "", false, nil
	}
	var typ string
	err = tx.QueryRow(`SELECT message_type FROM messages WHERE id = ?`, target.MessageID).Scan(&typ)
	if err == nil {
		return ref, typ == meetingLinkCalendarType, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", false, err
	}
	if target.Key != "" {
		return "", false, nil
	}
	present, err := sourceTableExists(tx, "calendar_events")
	if err != nil || !present {
		return "", false, err
	}
	var provider, namespace, series string
	var source int64
	var at sql.NullTime
	var date sql.NullString
	err = tx.QueryRow(`SELECT e.provider,e.namespace,e.series_provider_id,m.source_id,e.original_start_at,e.original_start_date FROM src.calendar_events e JOIN src.messages m ON m.id = e.message_id WHERE e.message_id = ? AND e.kind = 'exception' AND m.deleted_at IS NULL`, target.MessageID).Scan(&provider, &namespace, &series, &source, &at, &date)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	var master int64
	err = tx.QueryRow(`SELECT e.message_id FROM src.calendar_events e JOIN src.messages m ON m.id = e.message_id WHERE e.kind = 'series' AND e.provider = ? AND e.namespace = ? AND e.provider_event_id = ? AND m.source_id = ? AND m.deleted_at IS NULL AND EXISTS (SELECT 1 FROM messages kept WHERE kept.id = e.message_id AND kept.message_type = 'calendar_event') ORDER BY e.message_id LIMIT 1`, provider, namespace, series, source).Scan(&master)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	var original calrecur.Identity
	if date.Valid && date.String != "" {
		original = calrecur.Identity{Date: date.String, AllDay: true}
	} else if at.Valid {
		original = calrecur.Identity{Instant: at.Time}
	} else {
		return "", false, nil
	}
	return FormatOccurrenceRef(OccurrenceRef{MessageID: master, Key: original.Key()}), true, nil
}

// Validate after raw has rebuilt the destination's event projection. A selected
// message alone does not prove that the named series occurrence still exists.
func (s *Store) validateSubsetMeetingDecisions(ctx context.Context, result *CopyResult) error {
	rows, err := s.db.QueryContext(ctx, `SELECT member_message_id,target_ref FROM meeting_link_decisions ORDER BY member_message_id,target_ref`)
	if err != nil {
		return err
	}
	type target struct {
		member int64
		ref    string
	}
	var targets []target
	for rows.Next() {
		var item target
		if err := rows.Scan(&item.member, &item.ref); err != nil {
			_ = rows.Close()
			return err
		}
		targets = append(targets, item)
	}
	err = rows.Err()
	closeErr := rows.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	for _, item := range targets {
		_, ref, _, err := ParseMeetingRef(item.ref)
		if err != nil {
			return err
		}
		budget := calrecur.RequestReadBudget
		_, err = resolveLinkOccurrence(ctx, s.snapshotReader(ctx), OwnerScope(), ref, &budget)
		if err == nil {
			continue
		}
		if !errors.Is(err, sql.ErrNoRows) && !errors.Is(err, calrecur.ErrUnsupportedRecurrence) {
			return err
		}
		if _, err := s.db.ExecContext(ctx, `DELETE FROM meeting_link_decisions WHERE member_message_id = ? AND target_ref = ?`, item.member, item.ref); err != nil {
			return err
		}
		result.MeetingLinkDecisions--
		result.OmittedMeetingLinkDecisions++
	}
	return nil
}
