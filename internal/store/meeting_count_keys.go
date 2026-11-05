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
		occurrences, err := s.meetingMemberOccurrences(ctx, r, id, &budget)
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

func unresolvedMeetingCount(err error) bool {
	return errors.Is(err, ErrQueryTooBroad) || errors.Is(err, calrecur.ErrUnsupportedRecurrence) || errors.Is(err, sql.ErrNoRows)
}

// countScopedMeetings counts scoped transcripts by the occurrence keys People
// counts use, so transcripts linked to one occurrence count once.
func (s *Store) countScopedMeetings(ctx context.Context, tx *loggedTx, statement meetingScopeStatement) (int64, error) {
	rows, err := tx.QueryContext(ctx, statement.cte+`
		SELECT m.id, CASE WHEN `+meetingCountLinkedSQL+` THEN 1 ELSE 0 END
		FROM (SELECT message_id AS id FROM scoped_meetings) m`, statement.args...)
	if err != nil {
		return 0, fmt.Errorf("read meeting count members: %w", err)
	}
	defer func() { _ = rows.Close() }()
	linked := make(map[int64]bool)
	for rows.Next() {
		var id int64
		var isLinked bool
		if err := rows.Scan(&id, &isLinked); err != nil {
			return 0, fmt.Errorf("scan meeting count member: %w", err)
		}
		linked[id] = isLinked
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("iterate meeting count members: %w", err)
	}
	if err := rows.Close(); err != nil {
		return 0, fmt.Errorf("close meeting count members: %w", err)
	}
	r := reboundReader{loggedReader: tx, dialect: s.dialect}
	keys := make(map[string]bool, len(linked))
	for id, isLinked := range linked {
		var occurrences []linkOccurrence
		if isLinked {
			budget := calrecur.RequestReadBudget
			occurrences, err = s.meetingMemberOccurrences(ctx, r, id, &budget)
			if err != nil && !unresolvedMeetingCount(err) {
				return 0, fmt.Errorf("resolve meeting count member %d: %w", id, err)
			}
		}
		if len(occurrences) == 0 {
			keys[fmt.Sprintf("msg:%d", id)] = true
		}
		for _, occurrence := range occurrences {
			keys[occurrence.Identity.countKey()] = true
		}
	}
	return int64(len(keys)), nil
}
