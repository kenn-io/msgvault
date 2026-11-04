package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/calrecur"
)

const (
	contactCalendarICSEventPageSize = 200
	maxContactCalendarICSEvents     = 2000
)

// calendarAttendedTranscriptIDs bounds candidate recordings before resolving
// exact occurrences. It never expands calendar history and reads only bounded
// raw ICS records to recover UIDs absent from their metadata.
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
	directMore := len(candidates) > limit
	if directMore {
		candidates = candidates[:limit]
	}
	icsUIDs, icsMore, err := s.calendarAttendedICSUIDs(ctx, emails)
	if err != nil {
		return nil, false, err
	}
	icsIDs, icsIDsMore, err := s.calendarTranscriptIDsForUIDs(ctx, icsUIDs, limit)
	if err != nil {
		return nil, false, err
	}
	candidates = append(candidates, icsIDs...)
	slices.Sort(candidates)
	candidates = slices.Compact(candidates)
	slices.Reverse(candidates)
	more := directMore || icsMore || icsIDsMore || len(candidates) > limit
	if len(candidates) > limit {
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
					resolvedAt, exists, boundaryErr := calrecur.CivilDateBoundary(o.StartDate, loc)
					if boundaryErr != nil {
						return nil, more, fmt.Errorf("resolve attended all-day event boundary: %w", boundaryErr)
					}
					if !exists {
						occurrences[ref] = false
						continue
					}
					at = resolvedAt
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

func (s *Store) calendarAttendedICSUIDs(ctx context.Context, emails []string) ([]string, bool, error) {
	reader := s.meetingReader(ctx)
	marks := placeholders(len(emails))
	predicate := `r.raw_format='ics' AND e.message_id > ? AND EXISTS(
 SELECT 1 FROM calendar_event_attendees a WHERE a.message_id=e.message_id
 AND a.is_copy_self=FALSE AND a.role<>'room' AND LOWER(TRIM(a.email)) IN (` + marks + `))`
	argsFor := func(after int64) []any {
		args := make([]any, 0, len(emails)+1)
		args = append(args, after)
		for _, email := range emails {
			args = append(args, email)
		}
		return args
	}
	uids := map[string]struct{}{}
	var after int64
	scanned := 0
	verificationBudget := maxCalendarExceptions
	for {
		rows, err := queryCalendarEventRows(ctx, reader, predicate, argsFor(after), contactCalendarICSEventPageSize)
		if err != nil {
			return nil, false, fmt.Errorf("read attended ICS calendar events: %w", err)
		}
		if len(rows) == 0 {
			return sortedCalendarUIDs(uids), false, nil
		}
		if len(rows) > verificationBudget {
			return sortedCalendarUIDs(uids), true, nil
		}
		verificationBudget -= len(rows)
		for _, row := range rows {
			component, err := meetingICSComponent(ctx, reader, row, &verificationBudget)
			if errors.Is(err, calrecur.ErrBudgetExceeded) {
				return sortedCalendarUIDs(uids), true, nil
			}
			if err != nil {
				return nil, false, fmt.Errorf("read attended ICS calendar UID: %w", err)
			}
			if component.UID != "" {
				uids[component.UID] = struct{}{}
			}
			after = row.MessageID
		}
		scanned += len(rows)
		if len(rows) < contactCalendarICSEventPageSize {
			return sortedCalendarUIDs(uids), false, nil
		}
		if scanned >= maxContactCalendarICSEvents {
			more, err := queryCalendarEventRows(ctx, reader, predicate, argsFor(after), 1)
			if err != nil {
				return nil, false, fmt.Errorf("check remaining attended ICS calendar events: %w", err)
			}
			return sortedCalendarUIDs(uids), len(more) > 0, nil
		}
	}
}

func sortedCalendarUIDs(uids map[string]struct{}) []string {
	result := make([]string, 0, len(uids))
	for uid := range uids {
		result = append(result, uid)
	}
	slices.Sort(result)
	return result
}

func (s *Store) calendarTranscriptIDsForUIDs(ctx context.Context, uids []string, limit int) ([]int64, bool, error) {
	if len(uids) == 0 {
		return nil, false, nil
	}
	const uidBatchSize = 500
	transcriptScope, transcriptArgs := sourceScopeClause(ctx, "m.source_id")
	eligible := strings.Replace(s.activityLiveEligibleSQL(), "COALESCE(m.sent_at,m.received_at,m.internal_date) IS NOT NULL", "TRUE", 1)
	candidates := []int64{}
	more := false
	for start := 0; start < len(uids); start += uidBatchSize {
		end := min(start+uidBatchSize, len(uids))
		batch := uids[start:end]
		marks := placeholders(len(batch))
		query := `SELECT m.id FROM messages m JOIN sources src ON src.id=m.source_id WHERE m.message_type='meeting_transcript' AND m.list_id IS NULL
 AND ` + eligible + ` AND (
 EXISTS(SELECT 1 FROM meeting_link_key k WHERE k.message_id=m.id AND k.kind='ical_uid' AND k.value IN (` + marks + `))
 OR EXISTS(SELECT 1 FROM meeting_link_decisions d WHERE d.member_message_id=m.id AND d.decision='link' AND d.target_ical_uid IN (` + marks + `)))` + transcriptScope + ` ORDER BY m.id DESC LIMIT ?`
		args := make([]any, 0, len(batch)*2+len(transcriptArgs)+1)
		for _, uid := range batch {
			args = append(args, uid)
		}
		for _, uid := range batch {
			args = append(args, uid)
		}
		args = append(args, transcriptArgs...)
		args = append(args, limit+1)
		rows, err := s.db.QueryContext(ctx, s.Rebind(query), args...)
		if err != nil {
			return nil, false, fmt.Errorf("discover ICS-linked transcripts: %w", err)
		}
		batchIDs := []int64{}
		for rows.Next() {
			var id int64
			if err = rows.Scan(&id); err != nil {
				_ = rows.Close()
				return nil, false, fmt.Errorf("scan ICS-linked transcript: %w", err)
			}
			batchIDs = append(batchIDs, id)
		}
		iterationErr, closeErr := rows.Err(), rows.Close()
		if iterationErr != nil {
			return nil, false, fmt.Errorf("iterate ICS-linked transcripts: %w", iterationErr)
		}
		if closeErr != nil {
			return nil, false, fmt.Errorf("close ICS-linked transcripts: %w", closeErr)
		}
		if len(batchIDs) > limit {
			more = true
			batchIDs = batchIDs[:limit]
		}
		candidates = append(candidates, batchIDs...)
	}
	slices.Sort(candidates)
	candidates = slices.Compact(candidates)
	slices.Reverse(candidates)
	if len(candidates) > limit {
		more = true
		candidates = candidates[:limit]
	}
	return candidates, more, nil
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
