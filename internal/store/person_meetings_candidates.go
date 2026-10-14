package store

import (
	"context"
	"fmt"
	"strings"

	"go.kenn.io/msgvault/internal/calevent"
)

func personMeetingEmailMatches(attendees []calevent.Attendee, emails map[string]bool) bool {
	for _, a := range attendees {
		if a.Role != "room" && emails[strings.ToLower(strings.TrimSpace(a.Email))] {
			return true
		}
	}
	return false
}

// Retain the whole master/exception closure when any effective row can match.
func personMeetingInvitationRows(rows []CalendarEventRow, emails map[string]bool) []CalendarEventRow {
	groups := map[calendarSeriesIdentity]bool{}
	for _, row := range rows {
		if !personMeetingEmailMatches(row.Event.Attendees, emails) {
			continue
		}
		id := row.Event.ProviderEventID
		if row.Event.Kind == calendarExceptionKind {
			id = row.Event.SeriesProviderID
		}
		groups[calendarSeriesGroup(row, id)] = true
	}
	out := []CalendarEventRow{}
	for _, row := range rows {
		id := row.Event.ProviderEventID
		if row.Event.Kind == calendarExceptionKind {
			id = row.Event.SeriesProviderID
		}
		if groups[calendarSeriesGroup(row, id)] {
			out = append(out, row)
		}
	}
	return out
}

// Match identity before applying the effective occurrence's time window. An
// imported transcript can have a sent time months after its linked meeting.
func (s *Store) personMeetingNoteCandidates(ctx context.Context, r rowReader, scope SourceScope, q PersonMeetingsQuery) ([]int64, error) {
	predicates := []string{}
	args := []any{}
	if len(q.ParticipantIDs) > 0 {
		pred, values, err := s.meetingIDsMembership("p.id", q.ParticipantIDs)
		if err != nil {
			return nil, err
		}
		predicates = append(predicates, `EXISTS (SELECT 1 FROM participants p WHERE `+pred+` AND (p.id = m.sender_id OR EXISTS (SELECT 1 FROM message_recipients mr WHERE mr.message_id = m.id AND mr.participant_id = p.id)))`)
		args = append(args, values...)
	}
	if len(q.Emails) > 0 {
		marks := placeholders(len(q.Emails))
		predicates = append(predicates, `EXISTS (SELECT 1 FROM participants p WHERE LOWER(TRIM(COALESCE(p.email_address,''))) IN (`+marks+`) AND (p.id = m.sender_id OR EXISTS (SELECT 1 FROM message_recipients mr WHERE mr.message_id = m.id AND mr.participant_id = p.id)))`)
		for _, email := range q.Emails {
			args = append(args, email)
		}
		sourceEmails := `SELECT 1 FROM meeting_details md, json_each(md.content_json,'$.source_participants') sp WHERE md.message_id = m.id AND LOWER(TRIM(COALESCE(json_extract(sp.value,'$.email'),''))) IN (` + marks + `)`
		if s.IsPostgreSQL() {
			sourceEmails = `SELECT 1 FROM meeting_details md, jsonb_array_elements(COALESCE(CAST(md.content_json AS jsonb)->'source_participants','[]'::jsonb)) sp WHERE md.message_id = m.id AND LOWER(TRIM(COALESCE(sp->>'email',''))) IN (` + marks + `)`
		}
		predicates = append(predicates, `EXISTS (`+sourceEmails+`)`)
		for _, email := range q.Emails {
			args = append(args, email)
		}
	}
	clause, scopeArgs := linkScopeSQL(scope)
	args = append(args, scopeArgs...)
	rows, err := r.QueryContext(ctx, `SELECT m.id FROM messages m WHERE m.message_type = 'meeting_transcript' AND m.deleted_at IS NULL AND (`+strings.Join(predicates, " OR ")+`)`+clause+` ORDER BY m.id LIMIT 2001`, args...)
	if err != nil {
		return nil, fmt.Errorf("read person meeting notes: %w", err)
	}
	defer func() { _ = rows.Close() }()
	ids := []int64{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if len(ids) > 2000 {
		return nil, ErrQueryTooBroad
	}
	return ids, rows.Err()
}
