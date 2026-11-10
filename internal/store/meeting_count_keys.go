package store

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"time"

	"go.kenn.io/msgvault/internal/calrecur"
)

// meetingCountLinkedSQL selects members m with exact evidence or saved decisions.
const meetingCountLinkedSQL = `(EXISTS (SELECT 1 FROM meeting_link_key k WHERE k.message_id=m.id AND k.value<>'')
 OR EXISTS (SELECT 1 FROM meeting_link_decisions d WHERE d.member_message_id=m.id))`

func (i meetingIdentity) countKey() string {
	if i.ICalUID != "" {
		return "uid:" + i.ICalUID + "/" + i.OccurrenceKey
	}
	return fmt.Sprintf("evt:%d/%s", i.EventMessageID, i.OccurrenceKey)
}

// MeetingCountOccurrence carries the occurrence time and, for all-day meetings, its civil date.
type MeetingCountOccurrence struct {
	Key   string
	Start time.Time
	Date  string
}

// MeetingCountKeysContext maps linked meeting messages to occurrences in one read snapshot.
func (s *Store) MeetingCountKeysContext(ctx context.Context) (map[int64][]MeetingCountOccurrence, error) {
	return s.meetingCountKeysContext(ctx, calrecur.RequestReadBudget)
}

func (s *Store) meetingCountKeysContext(ctx context.Context, readBudget int) (map[int64][]MeetingCountOccurrence, error) {
	ctx, release, err := s.BeginReadSnapshotContext(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	r := s.meetingReader()
	r.countOnly = true

	rows, err := r.QueryContext(ctx, `SELECT m.id FROM messages m
 WHERE m.message_type = 'meeting_transcript' AND m.deleted_at IS NULL
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
	keys := make(map[int64][]MeetingCountOccurrence)
	for _, id := range members {
		budget := readBudget
		memberKeys, err := s.meetingCountOccurrences(ctx, r, id, &budget)
		if unresolvedMeetingCount(err) || errors.Is(err, ErrQueryTooBroad) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("resolve meeting count member %d: %w", id, err)
		}
		if len(memberKeys) > 0 {
			keys[id] = memberKeys
		}
	}
	return keys, nil
}

// meetingCountOccurrences combines saved choices and independently proved source evidence.
func (s *Store) meetingCountOccurrences(ctx context.Context, r reboundReader, memberID int64, budget *int) ([]MeetingCountOccurrence, error) {
	r = r.withMeetingCache()
	matches, stale, splits, source, err := s.meetingMemberOccurrences(ctx, r, memberID, budget)
	if err != nil {
		return nil, err
	}
	all := slices.Concat(matches, stale)
	if source != nil && !splits[source.Identity] {
		all = append(all, *source)
	}
	var out []MeetingCountOccurrence
	for _, o := range all {
		value := MeetingCountOccurrence{Key: o.Identity.countKey(), Start: o.Start}
		if o.Row.Event.AllDay {
			value.Date = o.Row.Event.StartDate
			if o.Identity.OccurrenceKey != "" && (o.Row.Event.Kind != meetingExceptionKind || value.Date == "") {
				if _, identity, ok := calrecur.ParseInstanceID("event_" + o.Identity.OccurrenceKey); ok && identity.AllDay {
					value.Date = identity.Date
				}
			}
		}
		out = append(out, value)
	}
	slices.SortFunc(out, func(a, b MeetingCountOccurrence) int { return cmp.Compare(a.Key, b.Key) })
	return slices.CompactFunc(out, func(a, b MeetingCountOccurrence) bool { return a.Key == b.Key }), nil
}

// sourceMeetingCountOccurrence uses an archived series to prove source evidence
// even after the recurrence no longer generates that occurrence.
//
//nolint:nilnil // Missing archived evidence is a valid result.
func sourceMeetingCountOccurrence(key meetingLinkKey, rows []CalendarEventRow) (*linkOccurrence, error) {
	var instance calrecur.Identity
	switch key.Kind {
	case "google_event_id":
		_, original, ok := calrecur.ParseInstanceID(key.Value)
		if !ok {
			if key.Value == "" || key.ScheduledAt == nil {
				return nil, nil
			}
			original = calrecur.Identity{Instant: *key.ScheduledAt}
		}
		instance = original
	case "ical_uid":
		if key.Value == "" || key.ScheduledAt == nil {
			return nil, nil
		}
		instance = calrecur.Identity{Instant: *key.ScheduledAt}
	default:
		return nil, nil
	}
	var found *linkOccurrence
	for _, row := range rows {
		if row.Event.Kind != meetingSeriesKind {
			continue
		}
		original := instance
		if key.Kind == "ical_uid" && row.Event.AllDay {
			zone, err := calendarEventZone(row.Event)
			if err != nil {
				return nil, err
			}
			original = calrecur.Identity{AllDay: true, Date: key.ScheduledAt.In(zone).Format(time.DateOnly)}
		}
		o := occurrenceFromRow(row, original.Key())
		if !original.AllDay {
			o.Start = original.Instant
		}
		if found != nil && found.Identity != o.Identity {
			return nil, nil
		}
		found = &o
	}
	return found, nil
}

func unresolvedMeetingCount(err error) bool {
	return errors.Is(err, calrecur.ErrUnsupportedRecurrence) || errors.Is(err, sql.ErrNoRows)
}
