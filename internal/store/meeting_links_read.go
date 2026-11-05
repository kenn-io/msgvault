package store

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/calrecur"
)

// MeetingLink is a resolved calendar occurrence linked to a meeting note.
type MeetingLink struct {
	Ref     string    `json:"ref"`
	Subject string    `json:"subject,omitempty"`
	Start   time.Time `json:"start"`
	End     time.Time `json:"end"`
}

// MeetingRecord joins an archived occurrence with its meeting notes.
type MeetingRecord struct {
	Links      []MeetingLink `json:"links"`
	MessageIDs []int64       `json:"message_ids"`
}

// MeetingRecordContext reads a record from either a note or an event reference.
func (s *Store) MeetingRecordContext(ctx context.Context, ref MeetingLinkRef) (MeetingRecord, error) {
	record := MeetingRecord{Links: []MeetingLink{}, MessageIDs: []int64{}}
	ctx, release, err := s.BeginReadSnapshotContext(ctx)
	if err != nil {
		return record, err
	}
	defer release()
	r := s.meetingReader()
	budget := calrecur.RequestReadBudget
	if ref.Kind == "msg" {
		var found int64
		if err := r.QueryRowContext(ctx, `SELECT id FROM messages WHERE id = ? AND message_type = 'meeting_transcript' AND deleted_at IS NULL`, ref.MessageID).Scan(&found); err != nil {
			return record, err
		}
		record.MessageIDs = append(record.MessageIDs, ref.MessageID)
		occurrences, err := s.meetingMemberOccurrences(ctx, r, ref.MessageID, &budget)
		if err != nil {
			return record, err
		}
		for _, occurrence := range occurrences {
			record.Links = append(record.Links, MeetingLink{Ref: formatMeetingOccurrence(occurrence.Ref), Subject: occurrence.Row.Subject, Start: occurrence.Start, End: occurrence.End})
			members, err := s.meetingOccurrenceMembers(ctx, r, occurrence, &budget)
			if err != nil {
				return record, err
			}
			record.MessageIDs = append(record.MessageIDs, members...)
		}
		slices.Sort(record.MessageIDs)
		record.MessageIDs = slices.Compact(record.MessageIDs)
		return record, nil
	}
	target, err := resolveMeetingOccurrence(ctx, r, ref.OccurrenceRef, &budget)
	if err != nil {
		return record, err
	}
	record.Links = append(record.Links, MeetingLink{Ref: formatMeetingOccurrence(target.Ref), Subject: target.Row.Subject, Start: target.Start, End: target.End})
	record.MessageIDs, err = s.meetingOccurrenceMembers(ctx, r, target, &budget)
	return record, err
}

func (s *Store) meetingOccurrenceMembers(ctx context.Context, r reboundReader, target linkOccurrence, budget *int) ([]int64, error) {
	membersOut := []int64{}
	query, args, err := s.meetingIdentityMemberQuery(ctx, r, target, budget)
	if err != nil {
		return membersOut, err
	}
	rows, err := r.QueryContext(ctx, query+" ORDER BY m.id LIMIT ?", append(args, *budget+1)...)
	if err != nil {
		return membersOut, err
	}
	defer func() { _ = rows.Close() }()
	var members []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return membersOut, err
		}
		members = append(members, id)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return membersOut, err
	}
	*budget -= len(members)
	if *budget < 0 {
		return membersOut, calendarRecurrenceBudgetError()
	}
	for _, id := range members {
		occurrences, err := s.meetingMemberOccurrences(ctx, r, id, budget)
		if err != nil {
			return membersOut, err
		}
		for _, occurrence := range occurrences {
			if occurrence.Identity == target.Identity {
				membersOut = append(membersOut, id)
				break
			}
		}
	}
	return membersOut, nil
}

// Include source-local orphan targets saved before their master arrived.
func meetingIdentityAliasPredicate(o linkOccurrence, copies []CalendarEventRow) (string, []any) {
	if o.Identity.OccurrenceKey != "" {
		_, original, ok := calrecur.ParseInstanceID("event_" + o.Identity.OccurrenceKey)
		if o.Row.Event.Original != nil {
			original, ok = *o.Row.Event.Original, true
		}
		if ok {
			column, value := "e.original_start_at", any(original.Instant)
			if original.AllDay {
				column, value = "e.original_start_date", original.Date
			}
			var terms []string
			var aliasArgs []any
			for _, copy := range copies {
				provider := copy.Event.ProviderEventID
				if copy.Event.Kind == meetingExceptionKind {
					provider = copy.Event.SeriesProviderID
				}
				if provider == "" {
					continue
				}
				terms = append(terms, `(m.source_id=? AND e.series_provider_id=?)`)
				aliasArgs = append(aliasArgs, copy.SourceID, provider)
			}
			if len(terms) > 0 {
				aliasArgs = append(aliasArgs, value)
				return `e.kind='exception' AND (` + strings.Join(terms, " OR ") + `) AND ` + column + `=?`, aliasArgs
			}
		}
	}
	return "", nil
}

// Discover only keys or decisions that could resolve to this occurrence. The
// forward resolver applies splits, ambiguity and provider ordering afterwards.
func (s *Store) meetingIdentityMemberQuery(ctx context.Context, r reboundReader, o linkOccurrence, budget *int) (string, []any, error) {
	copies, err := s.meetingIdentityRows(ctx, r, o.Identity, budget)
	if err != nil {
		return "", nil, err
	}
	predicate, aliasArgs := meetingIdentityAliasPredicate(o, copies)
	if predicate != "" {
		aliases, err := meetingEventRows(ctx, r, predicate, aliasArgs, budget)
		if err != nil {
			return "", nil, err
		}
		copies = append(copies, aliases...)
	}
	clauses := []string{"1=0"}
	args := []any{}
	keyClauses, keyArgs, err := meetingIdentityUIDMemberClauses(ctx, r, o, copies, budget)
	if err != nil {
		return "", nil, err
	}
	for _, copy := range copies {
		clauses = append(clauses, `(d.target_message_id=? AND (d.occurrence_key=? OR d.occurrence_key=''))`)
		args = append(args, copy.MessageID, o.Identity.OccurrenceKey)
		// Saved decisions keep the historical target ID after a source merge.
		// Resolve its source-local provider alias just as forward reads do.
		clauses = append(clauses, `(EXISTS (SELECT 1 FROM calendar_events h
 JOIN messages hm ON hm.id=h.message_id
 JOIN source_merge_archive_only_messages ha ON ha.message_id=h.message_id
 WHERE h.message_id=d.target_message_id AND h.provider_event_id=? AND hm.source_id=?)
 AND (d.occurrence_key=? OR d.occurrence_key=''))`)
		args = append(args, copy.Event.ProviderEventID, copy.SourceID, o.Identity.OccurrenceKey)
		if provider := copy.Event.ProviderEventID; provider != "" {
			keyClauses = append(keyClauses, `(k.kind='google_event_id' AND k.value IN (?,?))`)
			instance := provider
			if o.Identity.OccurrenceKey != "" {
				instance += "_" + o.Identity.OccurrenceKey
			}
			keyArgs = append(keyArgs, provider, instance)
		}
	}
	query := `SELECT m.id FROM messages m WHERE m.message_type='meeting_transcript' AND m.deleted_at IS NULL
 AND m.id IN (SELECT d.member_message_id FROM meeting_link_decisions d WHERE ` + strings.Join(clauses, " OR ") + `
 UNION SELECT k.message_id FROM meeting_link_key k WHERE ` + strings.Join(keyClauses, " OR ") + `)`
	args = append(args, keyArgs...)
	scope, scopeArgs := sourceScopeClause(ctx, "m.source_id")
	args = append(args, scopeArgs...)
	return query + scope, args, nil
}

// Each calendar copy can move the same occurrence to a different start.
// Discovery admits all of those starts; forward resolution decides the link.
func meetingIdentityUIDMemberClauses(ctx context.Context, r reboundReader, o linkOccurrence, rows []CalendarEventRow, budget *int) ([]string, []any, error) {
	uids := []any{o.Identity.ICalUID}
	if o.Row.ICalUID != "" && o.Row.ICalUID != o.Identity.ICalUID {
		uids = append(uids, o.Row.ICalUID)
	}
	clause := `(k.kind='ical_uid' AND k.value IN (` + placeholders(len(uids)) + `) AND k.value<>'')`
	if o.Identity.OccurrenceKey == "" {
		return []string{clause}, uids, nil
	}
	copies, err := meetingIdentityCopyOccurrences(ctx, r, o.Identity, rows, budget)
	if err != nil {
		return nil, nil, err
	}
	starts := []any{o.Start.UTC()}
	if _, original, ok := calrecur.ParseInstanceID("event_" + o.Identity.OccurrenceKey); ok {
		if original.AllDay {
			zone, err := calendarEventZone(o.Row.Event)
			if err != nil {
				return nil, nil, err
			}
			original.Instant, _, err = calrecur.CivilDateBoundary(original.Date, zone)
			if err != nil {
				return nil, nil, err
			}
		}
		starts = append(starts, original.Instant)
	}
	for _, copy := range copies {
		uids = append(uids, copy.Row.ICalUID)
		starts = append(starts, copy.Start.UTC())
	}
	column, values := "k.scheduled_at", placeholders(len(starts))
	if r.dialect.DriverName() != postgresDriverName {
		column, values = "julianday(k.scheduled_at)", strings.ReplaceAll(values, "?", "julianday(?)")
	}
	clause = `(k.kind='ical_uid' AND k.value IN (` + placeholders(len(uids)) + `) AND k.value<>'' AND ` + column + ` IN (` + values + `))`
	return []string{clause}, append(uids, starts...), nil
}
