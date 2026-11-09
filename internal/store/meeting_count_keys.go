package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
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

// MeetingCountKeysContext maps linked meeting messages to their occurrence keys in one read snapshot.
func (s *Store) MeetingCountKeysContext(ctx context.Context) (map[int64][]string, error) {
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
	keys := make(map[int64][]string)
	for _, id := range members {
		budget := calrecur.RequestReadBudget
		occurrences, err := s.meetingCountOccurrences(ctx, r, id, &budget)
		if unresolvedMeetingCount(err) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("resolve meeting count member %d: %w", id, err)
		}
		for _, occurrence := range occurrences {
			keys[id] = append(keys[id], occurrence.Identity.countKey())
		}
	}
	return keys, nil
}

// meetingCountOccurrences adds the archived identities of saved links and
// automatic evidence that the current series no longer expands, then removes
// every identity a saved split names.
func (s *Store) meetingCountOccurrences(ctx context.Context, r reboundReader, memberID int64, budget *int) ([]linkOccurrence, error) {
	r = r.withMeetingCache()
	out, err := s.meetingMemberOccurrences(ctx, r, memberID, budget)
	if err != nil {
		return nil, err
	}
	decisions, err := readMeetingLinkDecisions(ctx, r, memberID)
	if err != nil {
		return nil, err
	}
	splits := map[meetingIdentity]bool{}
	for _, d := range decisions {
		o, err := resolveMeetingOccurrence(ctx, r, d.Ref, budget)
		identity, archived := o.Identity, errors.Is(err, sql.ErrNoRows) || errors.Is(err, calrecur.ErrUnsupportedRecurrence)
		if archived {
			identity, err = storedMeetingIdentity(ctx, r, d.Ref, budget)
		}
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if d.Decision == "split" {
			splits[identity] = true
		} else if archived {
			out = append(out, linkOccurrence{Identity: identity})
		}
	}
	identity, ok, err := s.removedMeetingCountIdentity(ctx, r, memberID, budget)
	if err != nil {
		return nil, err
	}
	if ok {
		out = append(out, linkOccurrence{Identity: identity})
	}
	out = slices.DeleteFunc(out, func(o linkOccurrence) bool { return splits[o.Identity] })
	return uniqueMeetingOccurrences(out), nil
}

// removedMeetingCountIdentity returns the identity an automatic link was
// imported with when the archived series no longer expands that occurrence.
func (s *Store) removedMeetingCountIdentity(ctx context.Context, r reboundReader, memberID int64, budget *int) (meetingIdentity, bool, error) {
	key, err := memberLinkKey(ctx, r, memberID)
	if err != nil {
		return meetingIdentity{}, false, err
	}
	var predicate string
	var args []any
	var instance calrecur.Identity
	switch key.Kind {
	case "google_event_id":
		var base string
		var ok bool
		base, instance, ok = calrecur.ParseInstanceID(key.Value)
		if !ok {
			return meetingIdentity{}, false, nil
		}
		predicate, args = `e.provider_event_id = ?`, []any{base}
	case "ical_uid":
		if key.ScheduledAt == nil {
			return meetingIdentity{}, false, nil
		}
		predicate, args = s.calendarUIDPredicate(), []any{key.Value}
	default:
		return meetingIdentity{}, false, nil
	}
	if current, err := s.automaticMeetingOccurrences(ctx, r, key, budget); err != nil || len(current) > 0 {
		return meetingIdentity{}, false, err
	}
	rows, err := meetingEventRows(ctx, r, predicate+` AND e.kind = 'series'`, args, budget)
	if err != nil {
		return meetingIdentity{}, false, err
	}
	var found []meetingIdentity
	for _, row := range rows {
		original := instance
		if key.Kind == "google_event_id" {
			var config sql.NullString
			err := r.QueryRowContext(ctx, `SELECT sync_config FROM sources WHERE source_type = 'gcal'
 AND id = COALESCE((SELECT sm.original_source_id FROM source_merge_messages sm WHERE sm.message_id = ?), ?)`, row.MessageID, row.SourceID).Scan(&config)
			if errors.Is(err, sql.ErrNoRows) || err == nil && calendarMailbox(config) != key.CalendarID {
				continue
			}
			if err != nil {
				return meetingIdentity{}, false, err
			}
		} else if row.Event.AllDay {
			zone, err := calendarEventZone(row.Event)
			if err != nil {
				return meetingIdentity{}, false, err
			}
			original = calrecur.Identity{AllDay: true, Date: key.ScheduledAt.In(zone).Format(time.DateOnly)}
		} else {
			original = calrecur.Identity{Instant: *key.ScheduledAt}
		}
		identity := occurrenceFromRow(row, original.Key()).Identity
		if len(found) > 0 && identity != found[0] {
			return meetingIdentity{}, false, nil
		}
		found = append(found, identity)
	}
	if len(found) == 0 {
		return meetingIdentity{}, false, nil
	}
	return found[0], true, nil
}

func unresolvedMeetingCount(err error) bool {
	return errors.Is(err, ErrQueryTooBroad) || errors.Is(err, calrecur.ErrUnsupportedRecurrence) || errors.Is(err, sql.ErrNoRows)
}
