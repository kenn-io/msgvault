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

// meetingCountLinkedSQL selects members m with exact evidence or saved decisions.
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
		memberKeys, err := s.meetingCountOccurrences(ctx, r, id, &budget)
		if unresolvedMeetingCount(err) {
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

// meetingCountOccurrences preserves current matches and saved choices, with
// exact source evidence as a fallback when no occurrence or saved link matches.
func (s *Store) meetingCountOccurrences(ctx context.Context, r reboundReader, memberID int64, budget *int) ([]string, error) {
	r = r.withMeetingCache()
	matches, err := s.memberAutomaticOccurrences(ctx, r, memberID, budget)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, o := range matches {
		out = append(out, o.Identity.countKey())
	}
	decisions, err := readMeetingLinkDecisions(ctx, r, memberID)
	if err != nil {
		return nil, err
	}
	splits := map[string]bool{}
	hasLink := false
	for _, d := range decisions {
		hasLink = hasLink || d.Decision == "link"
		o, err := resolveMeetingOccurrence(ctx, r, d.Ref, budget)
		identity := o.Identity
		if errors.Is(err, sql.ErrNoRows) || errors.Is(err, calrecur.ErrUnsupportedRecurrence) {
			identity, err = storedMeetingIdentity(ctx, r, d.Ref, budget)
		}
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if d.Decision == "split" {
			splits[identity.countKey()] = true
		} else {
			out = append(out, identity.countKey())
		}
	}
	if len(matches) == 0 && !hasLink {
		key, err := memberLinkKey(ctx, r, memberID)
		if err != nil {
			return nil, err
		}
		fallback, err := s.unmatchedMeetingCountKey(ctx, r, key, budget)
		if err != nil {
			return nil, err
		}
		if fallback != "" {
			out = append(out, fallback)
		}
	}
	out = slices.DeleteFunc(out, func(key string) bool { return splits[key] })
	slices.Sort(out)
	return slices.Compact(out), nil
}

// unmatchedMeetingCountKey uses a series only to canonicalize exact evidence.
// The transcript proves attendance even when recurrence no longer includes it.
func (s *Store) unmatchedMeetingCountKey(ctx context.Context, r reboundReader, key meetingLinkKey, budget *int) (string, error) {
	var predicate, fallback string
	var args []any
	var instance calrecur.Identity
	switch key.Kind {
	case "google_event_id":
		if key.CalendarID == "" || key.Value == "" {
			return "", nil
		}
		base, original, ok := calrecur.ParseInstanceID(key.Value)
		if !ok {
			if key.ScheduledAt == nil {
				return "", nil
			}
			base, original = key.Value, calrecur.Identity{Instant: *key.ScheduledAt}
		}
		instance = original
		fallback = fmt.Sprintf("gcal:%q/%q/%s", key.CalendarID, base, instance.Key())
		predicate, args = `e.provider_event_id = ?`, []any{base}
	case "ical_uid":
		if key.Value == "" || key.ScheduledAt == nil {
			return "", nil
		}
		instance = calrecur.Identity{Instant: *key.ScheduledAt}
		fallback = (meetingIdentity{ICalUID: key.Value, OccurrenceKey: instance.Key()}).countKey()
		predicate, args = s.calendarUIDPredicate(), []any{key.Value}
	default:
		return "", nil
	}
	rows, err := meetingEventRows(ctx, r, predicate+` AND e.kind = 'series'`, args, budget)
	if err != nil {
		return "", err
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
				return "", err
			}
		} else if row.Event.AllDay {
			zone, err := calendarEventZone(row.Event)
			if err != nil {
				return "", err
			}
			original = calrecur.Identity{AllDay: true, Date: key.ScheduledAt.In(zone).Format(time.DateOnly)}
		}
		identity := occurrenceFromRow(row, original.Key()).Identity
		if len(found) > 0 && identity != found[0] {
			return fallback, nil
		}
		found = append(found, identity)
	}
	if len(found) == 0 {
		return fallback, nil
	}
	return found[0].countKey(), nil
}

func unresolvedMeetingCount(err error) bool {
	return errors.Is(err, ErrQueryTooBroad) || errors.Is(err, calrecur.ErrUnsupportedRecurrence) || errors.Is(err, sql.ErrNoRows)
}
