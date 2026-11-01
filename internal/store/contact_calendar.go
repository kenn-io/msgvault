package store

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"
	"time"
)

// calendarAttendedTranscriptIDs bounds candidate recordings before resolving
// exact occurrences. It never expands a calendar history or reads message bodies.
func (s *Store) calendarAttendedTranscriptIDs(ctx context.Context, q ContactQuery, filters ActivityFilters, limit int) ([]int64, bool, error) {
	emails, err := s.contactSubjectEmails(ctx, q.Subject)
	if err != nil {
		return nil, false, err
	}
	emails = slices.DeleteFunc(emails, func(email string) bool { return filters.Exclusions.ExcludesAddress(email) != "" })
	if len(emails) == 0 {
		return nil, false, nil
	}
	marks := placeholders(len(emails))
	args := make([]any, 0, len(emails)+1)
	for _, email := range emails {
		args = append(args, email)
	}
	calendarScope, calendarArgs := sourceScopeClause(ctx, "cm.source_id")
	args = append(args, calendarArgs...)
	transcriptScope, transcriptArgs := sourceScopeClause(ctx, "m.source_id")
	args = append(args, transcriptArgs...)
	args = append(args, limit+1)
	uid := calendarUIDExpression(s.dialect, "cm.metadata")
	// UID/provider keys and manual targets are only candidates: the forward
	// resolver below still applies splits, ambiguity, current copies and scope.
	query := `SELECT m.id FROM messages m JOIN sources src ON src.id=m.source_id WHERE m.message_type='meeting_transcript' AND m.list_id IS NULL
 AND ` + strings.Replace(s.activityLiveEligibleSQL(), "COALESCE(m.sent_at,m.received_at,m.internal_date) IS NOT NULL", "TRUE", 1) + `
 AND EXISTS(SELECT 1 FROM calendar_events e JOIN messages cm ON cm.id=e.message_id
 WHERE cm.deleted_at IS NULL AND ` + calendarEventVisibility("e", "cm") + `
 AND EXISTS(SELECT 1 FROM calendar_event_attendees a WHERE a.message_id=e.message_id AND a.is_copy_self=FALSE AND a.role<>'room' AND LOWER(TRIM(a.email)) IN (` + marks + `))` + calendarScope + `
 AND (EXISTS(SELECT 1 FROM meeting_link_key k WHERE k.message_id=m.id AND
 ((k.kind='ical_uid' AND k.value<>'' AND k.value=` + uid + `) OR
 (k.kind='google_event_id' AND e.provider_event_id<>'' AND (k.value=e.provider_event_id OR SUBSTR(k.value,1,LENGTH(e.provider_event_id)+1)=e.provider_event_id||'_'))))
 OR EXISTS(SELECT 1 FROM meeting_link_decisions d WHERE d.member_message_id=m.id AND d.decision='link'
 AND (d.target_message_id=cm.id OR (d.target_ical_uid<>'' AND d.target_ical_uid=` + uid + `)))))` + transcriptScope + ` ORDER BY m.id DESC LIMIT ?`
	rows, err := s.db.QueryContext(ctx, s.Rebind(query), args...)
	if err != nil {
		return nil, false, fmt.Errorf("discover calendar attended transcripts: %w", err)
	}
	candidates := []int64{}
	for rows.Next() {
		var id int64
		if err = rows.Scan(&id); err != nil {
			_ = rows.Close()
			return nil, false, fmt.Errorf("scan calendar transcript: %w", err)
		}
		candidates = append(candidates, id)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return nil, false, fmt.Errorf("iterate calendar transcripts: %w", err)
	}
	more := len(candidates) > limit
	if more {
		candidates = candidates[:limit]
	}
	identity, err := s.ResolveMeetingIdentitiesContext(ctx, MeetingIdentityRequest{TranscriptIDs: candidates})
	if err != nil {
		return nil, more, err
	}
	occurrences := map[string]bool{}
	result := []int64{}
	for _, id := range candidates {
		for _, ref := range identity.ByTranscript[id].ExactOccurrenceRefs {
			matches, known := occurrences[ref]
			if !known {
				detail, err := s.GetCalendarOccurrenceContext(ctx, ref)
				if err != nil {
					return nil, more, err
				}
				o := detail.CalendarOccurrence
				at := o.Start
				if o.AllDay {
					loc := q.Location
					if loc == nil {
						loc = time.UTC
					}
					at, _ = time.ParseInLocation(time.DateOnly, o.StartDate, loc)
				}
				if !at.IsZero() && !at.After(q.Now) {
					for _, a := range o.Attendees {
						if !a.IsCopySelf && a.Role != "room" && slices.Contains(emails, strings.ToLower(strings.TrimSpace(a.Email))) {
							matches = true
							break
						}
					}
				}
				occurrences[ref] = matches
			}
			if matches {
				result = append(result, id)
				break
			}
		}
	}
	return result, more, nil
}

func (s *Store) contactSubjectEmails(ctx context.Context, subject ActivitySubject) ([]string, error) {
	predicate, args, err := activityFactSubjectClause(subject)
	if err != nil {
		return nil, err
	}
	predicate = strings.ReplaceAll(predicate, "f.person_id", "pp.person_id")
	predicate = strings.ReplaceAll(predicate, "f.participant_id", "p.id")
	// Match the person reader's scoped population before using linked addresses.
	// A hidden/unobserved participant must not become calendar evidence merely
	// because it is linked to the same saved person.
	scope, scopeArgs := sourceScopeClause(ctx, "observed.source_id")
	if scope != "" {
		predicate += ` AND EXISTS(SELECT 1 FROM messages observed WHERE ` + activityParticipantMatchSQL("observed", "p.id") + scope + `)`
		args = append(args, scopeArgs...)
	}
	query := `SELECT LOWER(TRIM(p.email_address)) FROM participants p LEFT JOIN person_participants pp ON pp.participant_id=p.id WHERE ` + predicate + `
 UNION SELECT LOWER(TRIM(pi.identifier_value)) FROM participant_identifiers pi JOIN participants p ON p.id=pi.participant_id LEFT JOIN person_participants pp ON pp.participant_id=p.id WHERE pi.identifier_type='email' AND ` + predicate
	args = append(args, slices.Clone(args)...)
	rows, err := s.db.QueryContext(ctx, s.Rebind(query), args...)
	if err != nil {
		return nil, fmt.Errorf("read contact addresses: %w", err)
	}
	defer func() { _ = rows.Close() }()
	emails := []string{}
	for rows.Next() {
		var email sql.NullString
		if err = rows.Scan(&email); err != nil {
			return nil, fmt.Errorf("scan contact address: %w", err)
		}
		if email.Valid && email.String != "" {
			emails = append(emails, email.String)
		}
	}
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate contact addresses: %w", err)
	}
	slices.Sort(emails)
	return slices.Compact(emails), nil
}
