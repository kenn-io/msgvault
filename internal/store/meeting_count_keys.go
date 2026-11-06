package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"go.kenn.io/msgvault/internal/calrecur"
)

// meetingCountLinkedSQL selects members m that can resolve to an occurrence.
const meetingCountLinkedSQL = `(EXISTS (SELECT 1 FROM meeting_link_key k WHERE k.message_id=m.id AND k.value<>'')
 OR EXISTS (SELECT 1 FROM meeting_link_decisions d WHERE d.member_message_id=m.id))`

func (i meetingIdentity) countKey() string {
	if i.ICalUID != "" {
		return "uid:" + i.ICalUID + "/" + i.OccurrenceKey
	}
	return fmt.Sprintf("evt:%d/%s", i.EventMessageID, i.OccurrenceKey)
}

// MeetingCountKey carries an occurrence's identity and resolved start time into analytics.
type MeetingCountKey struct {
	Key   string
	Start time.Time
}

// MeetingCountKeysContext maps linked meeting messages to their occurrences in one read snapshot.
func (s *Store) MeetingCountKeysContext(ctx context.Context) (map[int64][]MeetingCountKey, error) {
	ctx, release, err := s.BeginReadSnapshotContext(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	r := s.meetingReader()

	rows, err := r.QueryContext(ctx, `SELECT m.id FROM messages m
 WHERE m.message_type IN ('meeting_transcript','meeting_note','meeting_minutes') AND m.deleted_at IS NULL
 AND `+meetingCountLinkedSQL)
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
	keys := make(map[int64][]MeetingCountKey)
	for _, id := range members {
		budget := calrecur.RequestReadBudget
		occurrences, err := s.meetingMemberOccurrences(ctx, r, id, &budget)
		if unresolvedMeetingCount(err) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("resolve meeting count member %d: %w", id, err)
		}
		for _, occurrence := range occurrences {
			keys[id] = append(keys[id], MeetingCountKey{Key: occurrence.Identity.countKey(), Start: occurrence.Start})
		}
	}
	return keys, nil
}

func unresolvedMeetingCount(err error) bool {
	return errors.Is(err, ErrQueryTooBroad) || errors.Is(err, calrecur.ErrUnsupportedRecurrence) || errors.Is(err, sql.ErrNoRows)
}
