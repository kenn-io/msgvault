package store

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"
)

// calendarTranscriptCandidates bounds recordings that may share calendar attendees.
func (s *Store) calendarTranscriptCandidates(ctx context.Context, q ContactQuery, filters ActivityFilters, limit int) ([]int64, []string, bool, error) {
	emails, err := s.contactSubjectEmails(ctx, q.Subject)
	if err != nil {
		return nil, nil, false, err
	}
	emails = slices.DeleteFunc(emails, func(email string) bool { return filters.Exclusions.ExcludesAddress(email) != "" })
	contacts := emails[:0]
	for _, email := range emails {
		owner, err := s.IsAccountIdentityAddressContext(ctx, email)
		if err != nil {
			return nil, nil, false, err
		}
		if !owner {
			contacts = append(contacts, email)
		}
	}
	emails = contacts
	if len(emails) == 0 {
		return nil, nil, false, nil
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
 AND ` + s.activityLiveEligibleWithoutDateSQL() + `
 AND EXISTS(SELECT 1 FROM calendar_events e JOIN messages cm ON cm.id=e.message_id
 WHERE cm.deleted_at IS NULL AND ` + calendarEventVisibility("e", "cm", "") + `
 AND EXISTS(SELECT 1 FROM calendar_event_attendees a WHERE a.message_id=e.message_id AND a.is_copy_self=FALSE AND a.role<>'room' AND LOWER(TRIM(a.email)) IN (` + marks + `))` + calendarScope + `
 AND (EXISTS(SELECT 1 FROM meeting_link_key k WHERE k.message_id=m.id AND
 ((k.kind='ical_uid' AND k.value<>'' AND k.value=` + uid + `) OR
 (k.kind='google_event_id' AND e.provider_event_id<>'' AND (k.value=e.provider_event_id OR SUBSTR(k.value,1,LENGTH(e.provider_event_id)+1)=e.provider_event_id||'_'))))
 OR EXISTS(SELECT 1 FROM meeting_link_decisions d WHERE d.member_message_id=m.id AND d.decision='link'
 AND d.target_message_id=cm.id)))` + transcriptScope + ` ORDER BY m.id DESC LIMIT ?`
	rows, err := s.db.QueryContext(ctx, s.Rebind(query), args...)
	if err != nil {
		return nil, nil, false, fmt.Errorf("discover calendar attended transcripts: %w", err)
	}
	candidates := []int64{}
	for rows.Next() {
		var id int64
		if err = rows.Scan(&id); err != nil {
			_ = rows.Close()
			return nil, nil, false, fmt.Errorf("scan calendar transcript: %w", err)
		}
		candidates = append(candidates, id)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return nil, nil, false, fmt.Errorf("iterate calendar transcripts: %w", err)
	}
	more := len(candidates) > limit
	if more {
		candidates = candidates[:limit]
	}
	return candidates, emails, more, nil
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

// CalendarMessageDisplayContext reads archived event titles within the source scope.
func (s *Store) CalendarMessageDisplayContext(ctx context.Context, ids []int64) (map[int64]MessageDisplay, error) {
	return s.messageDisplaysContext(ctx, ids, "m.message_type='calendar_event' AND m.deleted_at IS NULL")
}
