package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"go.kenn.io/msgvault/internal/calrecur"
)

func (i meetingIdentity) countKey() string {
	if i.ICalUID != "" {
		return "uid:" + i.ICalUID + "/" + i.OccurrenceKey
	}
	return fmt.Sprintf("evt:%d/%s", i.EventMessageID, i.OccurrenceKey)
}

// MeetingCountKeysContext maps linked transcripts and calendar copies to exact
// meeting targets in one read snapshot. Series masters have no count key.
func (s *Store) MeetingCountKeysContext(ctx context.Context) (map[int64]string, error) {
	ctx, release, err := s.BeginReadSnapshotContext(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	r := s.meetingReader(ctx)

	rows, err := r.QueryContext(ctx, `SELECT m.id FROM messages m
 WHERE m.message_type='meeting_transcript' AND m.deleted_at IS NULL
 AND (EXISTS (SELECT 1 FROM meeting_link_key k WHERE k.message_id=m.id AND k.value<>'')
 OR EXISTS (SELECT 1 FROM meeting_link_decisions d WHERE d.member_message_id=m.id))`)
	if err != nil {
		return nil, fmt.Errorf("read meeting count members: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var members []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		members = append(members, id)
	}
	err = rows.Err()
	closeErr := rows.Close()
	if err != nil {
		return nil, err
	}
	if closeErr != nil {
		return nil, closeErr
	}
	keys := make(map[int64]string)
	seen := make(map[meetingIdentity]bool)
	for _, id := range members {
		budget := calrecur.RequestReadBudget
		occurrences, err := s.meetingMemberOccurrences(ctx, r, id, &budget)
		if unresolvedMeetingCount(err) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("resolve meeting count member %d: %w", id, err)
		}
		if len(occurrences) != 1 {
			continue
		}
		identity := occurrences[0].Identity
		keys[id] = identity.countKey()
		if seen[identity] {
			continue
		}
		seen[identity] = true
		copies, err := s.meetingCountCopies(ctx, r, identity)
		if unresolvedMeetingCount(err) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("resolve meeting count copies: %w", err)
		}
		for _, copyID := range copies {
			keys[copyID] = identity.countKey()
		}
	}
	return keys, nil
}

func unresolvedMeetingCount(err error) bool {
	return errors.Is(err, ErrQueryTooBroad) || errors.Is(err, calrecur.ErrUnsupportedRecurrence) || errors.Is(err, sql.ErrNoRows)
}

func (s *Store) meetingCountCopies(ctx context.Context, r reboundReader, identity meetingIdentity) ([]int64, error) {
	const singleKind = "single"
	budget := calrecur.RequestReadBudget
	rows, err := s.meetingIdentityRows(ctx, r, identity, &budget)
	if err != nil {
		return nil, err
	}
	var copies []int64
	for _, row := range rows {
		ref := OccurrenceRef{MessageID: row.MessageID}
		if row.Event.Kind == meetingSeriesKind || row.Event.Kind == singleKind && identity.OccurrenceKey != "" {
			ref.Key = identity.OccurrenceKey
		}
		occurrence, err := resolveMeetingOccurrence(ctx, r, ref, &budget)
		if errors.Is(err, sql.ErrNoRows) || errors.Is(err, calrecur.ErrUnsupportedRecurrence) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if occurrence.Identity == identity {
			copyID := row.MessageID
			if row.Event.Kind == meetingSeriesKind || row.Event.Kind == singleKind && identity.OccurrenceKey != "" {
				if occurrence.Row.Event.Kind == meetingSeriesKind {
					continue
				}
				copyID = occurrence.Row.MessageID
			}
			copies = append(copies, copyID)
		}
	}
	return copies, nil
}
