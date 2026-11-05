package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

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

// MeetingCountKeysContext maps calendar events and linked transcripts to occurrence keys in one read snapshot.
func (s *Store) MeetingCountKeysContext(ctx context.Context) (map[int64][]string, error) {
	ctx, release, err := s.BeginReadSnapshotContext(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	r := s.meetingReader()

	rows, err := r.QueryContext(ctx, `SELECT m.id, m.message_type = 'calendar_event' FROM messages m
 WHERE m.deleted_at IS NULL AND (m.message_type = 'calendar_event'
 OR (m.message_type IN ('meeting_transcript','meeting_note','meeting_minutes') AND `+meetingCountLinkedSQL+`))`)
	if err != nil {
		return nil, fmt.Errorf("read meeting count members: %w", err)
	}
	defer func() { _ = rows.Close() }()
	type member struct {
		id       int64
		calendar bool
	}
	var members []member
	for rows.Next() {
		var m member
		if err := rows.Scan(&m.id, &m.calendar); err != nil {
			return nil, err
		}
		members = append(members, m)
	}
	err = rows.Err()
	closeErr := rows.Close()
	if err != nil {
		return nil, err
	}
	if closeErr != nil {
		return nil, closeErr
	}
	keys := make(map[int64][]string)
	for _, m := range members {
		budget := calrecur.RequestReadBudget
		var occurrences []linkOccurrence
		if m.calendar {
			ref := OccurrenceRef{MessageID: m.id}
			events, readErr := meetingEventRowsByID(ctx, r, m.id, &budget)
			if readErr != nil {
				return nil, fmt.Errorf("read calendar count member %d: %w", m.id, readErr)
			}
			if len(events) != 1 {
				continue
			}
			event := events[0].Event
			if event.Kind == meetingSeriesKind && event.Start != nil {
				ref.Key = (calrecur.Identity{Instant: *event.Start, Date: event.StartDate, AllDay: event.AllDay}).Key()
			}
			var occurrence linkOccurrence
			occurrence, err = resolveMeetingOccurrence(ctx, r, ref, &budget)
			occurrences = []linkOccurrence{occurrence}
		} else {
			occurrences, err = s.meetingMemberOccurrences(ctx, r, m.id, &budget)
		}
		if unresolvedMeetingCount(err) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("resolve meeting count member %d: %w", m.id, err)
		}
		for _, occurrence := range occurrences {
			keys[m.id] = append(keys[m.id], occurrence.Identity.countKey())
		}
	}
	return keys, nil
}

func unresolvedMeetingCount(err error) bool {
	return errors.Is(err, ErrQueryTooBroad) || errors.Is(err, calrecur.ErrUnsupportedRecurrence) || errors.Is(err, sql.ErrNoRows)
}
