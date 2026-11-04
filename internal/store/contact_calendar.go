package store

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/calevent"
	"go.kenn.io/msgvault/internal/calrecur"
)

const calendarAttendedCandidateBatch = 100
const calendarAttendedCandidateBudget = 10000

type calendarAttendedIdentity struct {
	kind, value, providerID string
}

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
	identityMap, identityMapMore, err := s.calendarAttendedIdentityMap(ctx)
	if err != nil {
		return nil, false, err
	}
	marks := placeholders(len(emails))
	args := make([]any, 0, len(emails))
	for _, email := range emails {
		args = append(args, email)
	}
	calendarScope, calendarArgs := sourceScopeClause(ctx, "cm.source_id")
	args = append(args, calendarArgs...)
	targetCalendarScope, targetCalendarArgs := sourceScopeClause(ctx, "target_cm.source_id")
	args = append(args, targetCalendarArgs...)
	transcriptScope, transcriptArgs := sourceScopeClause(ctx, "m.source_id")
	args = append(args, transcriptArgs...)
	calendarUID := calendarEventUIDExpression(s.dialect, "cm.metadata", "e.provider_event_id", "cal_src.source_type")
	targetCalendarUID := calendarEventUIDExpression(s.dialect, "target_cm.metadata", "target_e.provider_event_id", "target_cal_src.source_type")
	// Correlate each transcript's link evidence to the attendee-matching event
	// before the candidate budget. The forward resolver remains authoritative for
	// occurrence identity, splits, ambiguity, current copies and source scope.
	identityArgs := []any{}
	identityValues := make([]string, 0, len(identityMap))
	for _, identity := range identityMap {
		identityValues = append(identityValues, "(?,?)")
		identityArgs = append(identityArgs, identity.kind+":"+identity.value, identity.providerID)
	}
	identityCTE := `WITH calendar_attended_identity_map(identity_key,provider_id) AS (SELECT NULL,NULL WHERE 1=0) `
	if len(identityValues) > 0 {
		identityCTE = `WITH calendar_attended_identity_map(identity_key,provider_id) AS (VALUES ` + strings.Join(identityValues, ",") + `) `
	}
	icsUIDIdentity := func(key string) string {
		return `EXISTS(SELECT 1 FROM calendar_attended_identity_map identity_map
 JOIN message_raw ics_raw ON ics_raw.message_id=e.message_id AND ics_raw.raw_format='ics'
 WHERE identity_map.identity_key=` + key + `
  AND (identity_map.provider_id=e.provider_event_id OR identity_map.provider_id=e.series_provider_id))`
	}
	calendarLinkKeyMatch := `EXISTS(SELECT 1 FROM meeting_link_key k WHERE k.message_id=m.id AND (
 (k.kind='ical_uid' AND k.value<>'' AND ` + calendarUID + `=k.value)
 OR (k.kind='ical_uid' AND ` + icsUIDIdentity(`'ical_uid:'||k.value`) + `)
 OR (k.kind='google_event_id' AND k.value<>'' AND (
  e.provider_event_id=k.value OR e.series_provider_id=k.value OR EXISTS(
   SELECT 1 FROM calendar_attended_identity_map identity_map
   WHERE identity_map.identity_key='google_event_id:'||k.value
    AND (identity_map.provider_id=e.provider_event_id OR identity_map.provider_id=e.series_provider_id)))))` + `)`
	calendarDecisionMatch := `EXISTS(SELECT 1 FROM meeting_link_decisions d WHERE d.member_message_id=m.id AND d.decision='link' AND (
 d.target_message_id=e.message_id OR EXISTS(SELECT 1 FROM calendar_events target_e
  JOIN messages target_cm ON target_cm.id=target_e.message_id JOIN sources target_cal_src ON target_cal_src.id=target_cm.source_id
  WHERE target_e.message_id=d.target_message_id AND target_cm.deleted_at IS NULL AND ` + calendarEventVisibility("target_e", "target_cm") + `
   AND ((` + targetCalendarUID + `<>'' AND ` + targetCalendarUID + `=` + calendarUID + `)
    OR (target_e.provider_event_id<>'' AND (target_e.provider_event_id=e.provider_event_id OR target_e.provider_event_id=e.series_provider_id))
    OR (target_e.series_provider_id<>'' AND (target_e.series_provider_id=e.provider_event_id OR target_e.series_provider_id=e.series_provider_id)))` + targetCalendarScope + `)
 OR (d.target_ical_uid<>'' AND (` + calendarUID + `=d.target_ical_uid OR ` + icsUIDIdentity(`'ical_uid:'||d.target_ical_uid`) + `))))`
	query := identityCTE + `SELECT m.id FROM messages m JOIN sources src ON src.id=m.source_id WHERE m.message_type='meeting_transcript' AND m.list_id IS NULL
 AND ` + strings.Replace(s.activityLiveEligibleSQL(), "COALESCE(m.sent_at,m.received_at,m.internal_date) IS NOT NULL", "TRUE", 1) + `
 AND EXISTS(SELECT 1 FROM calendar_events e JOIN messages cm ON cm.id=e.message_id JOIN sources cal_src ON cal_src.id=cm.source_id
 WHERE cm.deleted_at IS NULL AND ` + calendarEventVisibility("e", "cm") + `
 AND EXISTS(SELECT 1 FROM calendar_event_attendees a WHERE a.message_id=e.message_id AND a.is_copy_self=FALSE AND a.role<>'room' AND LOWER(TRIM(a.email)) IN (` + marks + `))` + calendarScope + `
 AND (` + calendarLinkKeyMatch + ` OR ` + calendarDecisionMatch + `))` + transcriptScope + ` ORDER BY m.id DESC LIMIT ? OFFSET ?`
	occurrences := map[string]bool{}
	result := []int64{}
	more := identityMapMore
	offset := 0
	reachedEnd := false
	for offset < calendarAttendedCandidateBudget && len(result) <= limit {
		pageArgs := append(slices.Clone(identityArgs), args...)
		pageArgs = append(pageArgs, calendarAttendedCandidateBatch, offset)
		rows, err := s.db.QueryContext(ctx, s.Rebind(query), pageArgs...)
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
		if len(candidates) == 0 {
			reachedEnd = true
			break
		}
		identity, err := s.ResolveMeetingIdentitiesContext(ctx, MeetingIdentityRequest{TranscriptIDs: candidates})
		if err != nil {
			return nil, false, err
		}
		for _, id := range candidates {
			for _, ref := range identity.ByTranscript[id].ExactOccurrenceRefs {
				matches, known := occurrences[ref]
				if !known {
					detail, err := s.GetCalendarOccurrenceContext(ctx, ref)
					if err != nil {
						return nil, false, err
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
		offset += len(candidates)
		if len(result) > limit {
			more = true
			break
		}
		if len(candidates) < calendarAttendedCandidateBatch {
			reachedEnd = true
			break
		}
	}
	if !reachedEnd && offset >= calendarAttendedCandidateBudget {
		more = true
	}
	if len(result) > limit {
		result = result[:limit]
	}
	return result, more, nil
}

// calendarAttendedIdentityMap mirrors the provider identity forms used by
// calendarUIDPredicate without reading ICS content. The deterministic ICS IDs
// and parsed Google instance bases are joined to attendee-matching events in
// the candidate query before its result budget is applied.
func (s *Store) calendarAttendedIdentityMap(ctx context.Context) ([]calendarAttendedIdentity, bool, error) {
	scope, scopeArgs := sourceScopeClause(ctx, "m.source_id")
	live := strings.Replace(s.activityLiveEligibleSQL(), "COALESCE(m.sent_at,m.received_at,m.internal_date) IS NOT NULL", "TRUE", 1)
	query := `SELECT kind,value FROM (
 SELECT k.kind AS kind,k.value AS value,m.id AS message_id
 FROM meeting_link_key k JOIN messages m ON m.id=k.message_id JOIN sources src ON src.id=m.source_id
 WHERE m.message_type='meeting_transcript' AND m.list_id IS NULL AND ` + live + `
  AND k.kind IN ('ical_uid','google_event_id') AND k.value<>''` + scope + `
 UNION ALL
 SELECT 'ical_uid' AS kind,d.target_ical_uid AS value,m.id AS message_id
 FROM meeting_link_decisions d JOIN messages m ON m.id=d.member_message_id JOIN sources src ON src.id=m.source_id
 WHERE m.message_type='meeting_transcript' AND m.list_id IS NULL AND ` + live + `
  AND d.decision='link' AND d.target_ical_uid<>''` + scope + `
) linked GROUP BY kind,value ORDER BY MAX(message_id) DESC,kind,value LIMIT ?`
	args := append(slices.Clone(scopeArgs), scopeArgs...)
	args = append(args, calendarAttendedCandidateBudget+1)
	rows, err := s.db.QueryContext(ctx, s.Rebind(query), args...)
	if err != nil {
		return nil, false, fmt.Errorf("read calendar attendance identities: %w", err)
	}
	defer func() { _ = rows.Close() }()
	identities := []calendarAttendedIdentity{}
	more := false
	fetched := 0
	for rows.Next() {
		if fetched == calendarAttendedCandidateBudget {
			more = true
			break
		}
		fetched++
		var kind, value string
		if err := rows.Scan(&kind, &value); err != nil {
			return nil, false, fmt.Errorf("scan calendar attendance identity: %w", err)
		}
		identity := calendarAttendedIdentity{kind: kind, value: value}
		switch kind {
		case "ical_uid":
			identity.providerID = calevent.ICSMessageID(value, "")
		case "google_event_id":
			base, _, ok := calrecur.ParseInstanceID(value)
			if !ok {
				continue
			}
			identity.providerID = base
		default:
			continue
		}
		identities = append(identities, identity)
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("iterate calendar attendance identities: %w", err)
	}
	return identities, more, nil
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
