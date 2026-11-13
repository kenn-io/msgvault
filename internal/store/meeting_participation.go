package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"go.kenn.io/msgvault/internal/meetingcontent"
)

type meetingParticipationReader interface {
	contextRowQuerier
	contextRowsQuerier
}

// loadMeetingParticipationWith reads only the selected event. Raw roles must
// match its explicit recipients unambiguously; the conversation roster and
// display decoder's owner/invitee fallbacks cannot supply positive evidence.
func (s *Store) loadMeetingParticipationWith(ctx context.Context, q meetingParticipationReader, id int64, authorize ChatDraftAuthorizer) ([]ActivityCounterpart, error) {
	var typ, sourceType, identifier string
	var retired sql.NullInt64
	var deleted, upstream, sent, received, internal nullableTimestamp
	err := q.QueryRowContext(ctx, `SELECT m.message_type, src.source_type, src.identifier, ss.merged_into_source_id,
 m.deleted_at, m.deleted_from_source_at, m.sent_at, m.received_at, m.internal_date
 FROM messages m JOIN sources src ON src.id=m.source_id LEFT JOIN source_settings ss ON ss.source_id=src.id WHERE m.id=?`, id).Scan(&typ, &sourceType, &identifier, &retired, &deleted, &upstream, &sent, &received, &internal)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrMeetingNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("load meeting participation source: %w", err)
	}
	if authorize != nil {
		if err := authorize(sourceType, identifier); err != nil {
			return nil, err
		}
	}
	if typ != "meeting_transcript" || retired.Int64 != 0 || deleted.Valid || upstream.Valid || (!sent.Valid && !received.Valid && !internal.Valid) {
		return nil, nil
	}
	var stored []byte
	var format string
	var compression sql.NullString
	err = q.QueryRowContext(ctx, `SELECT raw_data, raw_format, compression FROM message_raw WHERE message_id=?`, id).Scan(&stored, &format, &compression)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("load meeting participation raw: %w", err)
	}
	raw, _, err := decodeMessageRawBounded(stored, compression, maxMeetingRawBytes, errMeetingRawTooLarge)
	if err != nil {
		return nil, nil
	}
	explicit := meetingcontent.ExplicitParticipants(format, raw)
	if len(explicit) == 0 {
		return nil, nil
	}
	rows, err := q.QueryContext(ctx, `SELECT mr.participant_id, pp.person_id, mr.recipient_type,
 COALESCE(mr.email_address,''), COALESCE(p.phone_number,'')
 FROM message_recipients mr JOIN participants p ON p.id=mr.participant_id
 LEFT JOIN person_participants pp ON pp.participant_id=mr.participant_id
 WHERE mr.message_id=? AND mr.recipient_type IN ('from','to')`, id)
	if err != nil {
		return nil, fmt.Errorf("load meeting participation recipients: %w", err)
	}
	type recipient struct {
		counterpart  ActivityCounterpart
		email, phone string
	}
	var recipients []recipient
	for rows.Next() {
		var r recipient
		if err := rows.Scan(&r.counterpart.ParticipantID, &r.counterpart.PersonID, &r.counterpart.RecipientType, &r.email, &r.phone); err != nil {
			_ = rows.Close()
			return nil, err
		}
		r.email = strings.ToLower(strings.TrimSpace(r.email))
		r.phone = strings.TrimSpace(r.phone)
		recipients = append(recipients, r)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return nil, err
	}
	result := []ActivityCounterpart{}
	seen := map[ActivityCounterpart]bool{}
	for _, person := range explicit {
		matches := map[int64]ActivityCounterpart{}
		for _, r := range recipients {
			if r.counterpart.RecipientType != person.Role {
				continue
			}
			if (person.Email != "" && person.Email == r.email) || (person.Phone != "" && person.Phone == r.phone) {
				matches[r.counterpart.ParticipantID] = r.counterpart
			}
		}
		if len(matches) != 1 {
			return nil, nil
		}
		for _, c := range matches {
			// Deduplicate by participant/role, not pointer identity.
			key := c
			key.PersonID = nil
			if !seen[key] {
				seen[key] = true
				result = append(result, c)
			}
		}
	}
	return result, nil
}

// PersonIntroductionMeetingValidContext revalidates a context reference in the
// caller's read snapshot. Authorization runs before raw or participant reads.
func (s *Store) PersonIntroductionMeetingValidContext(ctx context.Context, id, a, b int64, authorize ChatDraftAuthorizer) (bool, error) {
	people, err := s.loadMeetingParticipationWith(ctx, s.db, id, authorize)
	if err != nil {
		return false, err
	}
	return meetingIncludesPersons(people, a, b), nil
}
func meetingIncludesPersons(people []ActivityCounterpart, a, b int64) bool {
	hasA, hasB := false, false
	for _, p := range people {
		if p.PersonID != nil {
			hasA = hasA || *p.PersonID == a
			hasB = hasB || *p.PersonID == b
		}
	}
	return hasA && hasB
}
