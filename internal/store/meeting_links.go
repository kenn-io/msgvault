package store

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"time"

	"go.kenn.io/msgvault/internal/calrecur"
)

const (
	meetingSeriesKind      = "series"
	meetingExceptionKind   = "exception"
	meetingCancelledStatus = "cancelled"
)

// OccurrenceRef identifies an event, or one original occurrence of a series.
type OccurrenceRef struct {
	MessageID int64
	Key       string
}

type meetingIdentity struct {
	ICalUID        string
	EventMessageID int64
	OccurrenceKey  string
}

type linkOccurrence struct {
	Ref        OccurrenceRef
	Row        CalendarEventRow
	Identity   meetingIdentity
	Start, End time.Time
}

type reboundReader struct {
	loggedReader

	dialect Dialect
	batch   *meetingIdentityBatch
}

func meetingEventRows(ctx context.Context, r reboundReader, predicate string, args []any, budget *int) ([]CalendarEventRow, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if r.batch != nil {
		if rows, ok := r.batch.events[meetingQueryKey(predicate, args)]; ok {
			if len(rows) > *budget {
				return nil, calendarRecurrenceBudgetError()
			}
			*budget -= len(rows)
			return rows, nil
		}
	}
	rows, err := queryCalendarEventRowsWithinBudget(ctx, r, predicate, args, min(200, *budget))
	if err != nil {
		return nil, err
	}
	*budget -= len(rows)
	if r.batch != nil {
		r.batch.events[meetingQueryKey(predicate, args)] = rows
		for _, row := range rows {
			r.batch.events[meetingQueryKey("e.message_id = ?", []any{row.MessageID})] = []CalendarEventRow{row}
		}
	}
	return rows, nil
}

// The visible event also represents archived copies hidden by source merges.
func meetingEventAliasPredicate(id string) string {
	return `e.message_id = ` + id + ` OR EXISTS (
 SELECT 1 FROM calendar_events h JOIN messages hm ON hm.id = h.message_id
 JOIN source_merge_archive_only_messages ha ON ha.message_id = h.message_id
 WHERE h.message_id = ` + id + ` AND h.provider_event_id = e.provider_event_id AND hm.source_id = m.source_id)`
}

func meetingEventRowsByID(ctx context.Context, r reboundReader, id int64, budget *int) ([]CalendarEventRow, error) {
	if r.batch != nil {
		if rows, ok := r.batch.events[meetingQueryKey("e.message_id = ?", []any{id})]; ok && len(rows) > 0 {
			*budget -= len(rows)
			if *budget < 0 {
				return nil, calendarRecurrenceBudgetError()
			}
			return rows, nil
		}
	}
	return meetingEventRows(ctx, r, meetingEventAliasPredicate("?"), []any{id, id}, budget)
}

func occurrenceFromRow(row CalendarEventRow, key string) linkOccurrence {
	refKey := key
	if key == "" && row.Event.Kind == meetingExceptionKind && row.Event.Original != nil {
		key = row.Event.Original.Key()
	}
	id := meetingIdentity{ICalUID: row.ICalUID, OccurrenceKey: key}
	unidentifiedException := row.Event.Kind == meetingExceptionKind && row.Event.Original == nil
	// An unidentified exception has no occurrence identity to share with other
	// rows, even when the provider gave the series an iCalUID.
	if unidentifiedException {
		id = meetingIdentity{EventMessageID: row.MessageID}
	}
	if id.ICalUID == "" {
		id.EventMessageID = row.MessageID
	}
	if unidentifiedException {
		refKey = ""
	}
	o := linkOccurrence{Ref: OccurrenceRef{MessageID: row.MessageID, Key: refKey}, Row: row, Identity: id}
	if row.Event.Start != nil {
		o.Start = *row.Event.Start
	}
	if row.Event.End != nil {
		o.End = *row.Event.End
	}
	return o
}

func meetingSeriesExceptionsPredicate(ctx context.Context, dialect Dialect, master CalendarEventRow, from, to time.Time) (string, []any) {
	predicate, args := calendarExceptionWindow(ctx, dialect, from, to)
	return predicate + ` AND m.source_id = ? AND e.series_provider_id = ?`, append(args, master.SourceID, master.Event.ProviderEventID)
}

func meetingMasterPredicate(row CalendarEventRow) (string, []any) {
	return `e.kind IN ('series', 'single') AND m.source_id = ? AND e.provider_event_id = ?`, []any{row.SourceID, row.Event.SeriesProviderID}
}

func meetingMovedExceptionPredicate(row CalendarEventRow, id calrecur.Identity) (string, []any) {
	column, value := "e.original_start_at", any(id.Instant)
	if id.AllDay {
		column, value = "e.original_start_date", id.Date
	}
	return `e.kind = 'exception' AND m.source_id = ? AND e.series_provider_id = ? AND ` + column + ` = ?`, []any{row.SourceID, row.Event.ProviderEventID, value}
}

func meetingOccurrenceStart(row CalendarEventRow, id calrecur.Identity) (time.Time, error) {
	if !id.AllDay {
		return id.Instant, nil
	}
	zone, err := calendarEventZone(row.Event)
	if err != nil {
		return time.Time{}, err
	}
	at, _, err := calrecur.CivilDateBoundary(id.Date, zone)
	return at, err
}

func expandMeetingSeries(ctx context.Context, r reboundReader, master CalendarEventRow, from, to time.Time, budget *int) ([]linkOccurrence, error) {
	predicate, args := meetingSeriesExceptionsPredicate(ctx, r.dialect, master, from, to)
	related, err := meetingEventRows(ctx, r, predicate, args, budget)
	if err != nil {
		return nil, err
	}
	expanded, count, reason, err := expandCalendarSeries(ctx, master, related, from, to, *budget)
	*budget -= count
	if err != nil {
		return nil, err
	}
	var expansionErr error
	if reason != "" {
		if *budget <= 0 {
			return nil, calendarRecurrenceBudgetError()
		}
		expansionErr = fmt.Errorf("%w: %s", calrecur.ErrUnsupportedRecurrence, reason)
	}
	byID := make(map[int64]CalendarEventRow, len(related))
	for _, row := range related {
		byID[row.MessageID] = row
	}
	out := make([]linkOccurrence, 0, len(expanded))
	for _, instance := range expanded {
		effective, instance, err := materializeCalendarOccurrence(ctx, r, master, byID, instance)
		if err != nil {
			return nil, err
		}
		refRow := master
		if instance.HasException && instance.Original == (calrecur.Identity{}) {
			refRow = effective
		}
		o := occurrenceFromRow(refRow, instance.Original.Key())
		o.Row = effective
		o.Start, o.End = instance.Start, instance.End
		out = append(out, o)
	}
	return out, expansionErr
}

func resolveMeetingOccurrence(ctx context.Context, r reboundReader, ref OccurrenceRef, budget *int) (linkOccurrence, error) {
	rows, err := meetingEventRowsByID(ctx, r, ref.MessageID, budget)
	if err != nil {
		return linkOccurrence{}, err
	}
	if len(rows) != 1 {
		return linkOccurrence{}, sql.ErrNoRows
	}
	row := rows[0]
	if row.Event.Kind == meetingExceptionKind && ref.Key == "" {
		predicate, args := meetingMasterPredicate(row)
		masters, err := meetingEventRows(ctx, r, predicate, args, budget)
		if err != nil {
			return linkOccurrence{}, err
		}
		if len(masters) == 1 && masters[0].Event.Status == meetingCancelledStatus {
			row.Event.Status = meetingCancelledStatus
		}
		if row.Event.Original != nil && len(masters) == 1 {
			row, ref = masters[0], OccurrenceRef{MessageID: masters[0].MessageID, Key: row.Event.Original.Key()}
		} else if row.Event.Original != nil {
			if row.Event.Start == nil || row.Event.End == nil {
				return linkOccurrence{}, fmt.Errorf("%s: %w", calendarMissingBoundsReason(row.Event), sql.ErrNoRows)
			}
			o := occurrenceFromRow(row, row.Event.Original.Key())
			o.Ref.Key = ""
			return o, nil
		}
	}
	if ref.Key == "" {
		if row.Event.Kind == meetingSeriesKind {
			return linkOccurrence{}, fmt.Errorf("series reference needs @<occurrence key>: %w", sql.ErrNoRows)
		}
		if row.Event.Start == nil || row.Event.End == nil {
			return linkOccurrence{}, fmt.Errorf("%s: %w", calendarMissingBoundsReason(row.Event), sql.ErrNoRows)
		}
		return occurrenceFromRow(row, ""), nil
	}
	if row.Event.Kind != meetingSeriesKind && row.Event.Kind != "single" {
		return linkOccurrence{}, sql.ErrNoRows
	}
	_, id, ok := calrecur.ParseInstanceID("event_" + ref.Key)
	if !ok {
		return linkOccurrence{}, fmt.Errorf("invalid occurrence key %q", ref.Key)
	}
	at, err := meetingOccurrenceStart(row, id)
	if err != nil {
		return linkOccurrence{}, err
	}
	// A moved exception is expanded at its effective start, retaining Original.
	predicate, args := meetingMovedExceptionPredicate(row, id)
	moved, err := meetingEventRows(ctx, r, predicate, args, budget)
	if err != nil {
		return linkOccurrence{}, err
	}
	// Every copy supplies the UID; the latest copy supplies the bounds.
	var x CalendarEventRow
	for _, x = range moved {
		row.ICalUID = cmp.Or(row.ICalUID, x.ICalUID)
	}
	if x.Event.Start != nil && x.Event.End != nil {
		o := occurrenceFromRow(row, ref.Key)
		o.Start, o.End, err = seriesExceptionBounds(row, x)
		if err != nil {
			return linkOccurrence{}, err
		}
		o.Row = x
		if row.Event.Status == meetingCancelledStatus {
			o.Row.Event.Status = meetingCancelledStatus
		}
		return o, nil
	}
	if x.Event.Start != nil {
		at = *x.Event.Start
	}
	occurrences, err := expandMeetingSeries(ctx, r, row, at.Add(-calendarCandidateMargin), at.Add(calendarCandidateMargin), budget)
	if err != nil && !errors.Is(err, calrecur.ErrUnsupportedRecurrence) {
		return linkOccurrence{}, err
	}
	for _, o := range occurrences {
		if o.Ref.Key == ref.Key {
			return o, nil
		}
	}
	if err != nil {
		return linkOccurrence{}, err
	}
	return linkOccurrence{}, sql.ErrNoRows
}

func seriesExceptionBounds(master, exception CalendarEventRow) (time.Time, time.Time, error) {
	set, duration, days, err := prepareCalendarRecurrence(master.Event)
	var prepared *calrecur.Set
	if err == nil {
		prepared = &set
	}
	o, err := exceptionBoundsFor(prepared, duration, days, exception)
	return o.Start, o.End, err
}

// Bounded edits share one interpretation across listings and meeting links,
// including when series generation fails. Sparse edits still inherit bounds.
func exceptionBoundsFor(set *calrecur.Set, duration time.Duration, days int, row CalendarEventRow) (calrecur.Occurrence, error) {
	e := row.Event
	o := calrecur.Occurrence{AllDay: e.AllDay, StartDate: e.StartDate, EndDate: e.EndDate, Cancelled: e.Status == meetingCancelledStatus, ExceptionRef: row.MessageID, HasException: true}
	if e.Original != nil {
		o.Original = *e.Original
	}
	bounded := e.Start != nil && e.End != nil
	if bounded {
		o.Start, o.End = *e.Start, *e.End
	}
	if set == nil || e.Original == nil || e.Original.AllDay != set.AllDay || bounded && !e.AllDay {
		return o, nil
	}
	exceptions, err := calendarRecurrenceExceptions([]CalendarEventRow{row})
	if err != nil {
		if bounded {
			return o, nil
		}
		return o, err
	}
	resolved, err := calrecur.EffectiveException(*set, exceptions[0], duration, days)
	if bounded && errors.Is(err, calrecur.ErrUnsupportedRecurrence) {
		return o, nil
	}
	return resolved, err
}

func memberLinkKey(ctx context.Context, r loggedReader, id int64) (meetingLinkKey, error) {
	if rr, ok := r.(reboundReader); ok && rr.batch != nil {
		if key, ok := rr.batch.keys[id]; ok {
			return key, nil
		}
	}
	var key meetingLinkKey
	var at sql.NullTime
	var kind, value, calendar sql.NullString
	scope, args := sourceScopeClause(ctx, "m.source_id")
	args = append([]any{id}, args...)
	err := r.QueryRowContext(ctx, `SELECT k.kind,k.value,k.calendar_id,k.scheduled_at FROM messages m LEFT JOIN meeting_link_key k ON k.message_id=m.id WHERE m.id = ? AND m.deleted_at IS NULL`+scope, args...).Scan(&kind, &value, &calendar, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return key, nil
	}
	if err != nil {
		return key, err
	}
	key.Kind, key.Value, key.CalendarID = kind.String, value.String, calendar.String
	if at.Valid {
		key.ScheduledAt = &at.Time
	}
	return key, nil
}

func (s *Store) calendarUIDPredicate(uid string) (string, []any) {
	return `e.message_id IN (SELECT uid_message.id FROM messages uid_message WHERE uid_message.message_type='calendar_event' AND ` + calendarUIDExpression(s.dialect, "uid_message.metadata") + ` = ?)`, []any{uid}
}

func (s *Store) automaticMeetingPredicate(ctx context.Context, r reboundReader, key meetingLinkKey) (string, []any, error) {
	if key.Kind != "google_event_id" && key.Kind != "ical_uid" {
		return "1=0", nil, nil
	}
	predicate, args := s.calendarUIDPredicate(key.Value)
	base, _, _ := calrecur.ParseInstanceID(key.Value)
	if key.Kind == "google_event_id" {
		sources, err := meetingCalendarSources(ctx, r)
		if err != nil {
			return "", nil, err
		}
		sourceIDs := slices.Clone(sources[key.CalendarID])
		if len(sourceIDs) == 0 {
			return "1=0", nil, nil
		}
		predicate = `e.provider_event_id IN (?, ?) AND EXISTS (SELECT 1 FROM calendar_events candidate JOIN messages cm ON cm.id = candidate.message_id
 WHERE (` + meetingEventAliasPredicate("candidate.message_id") + `)
 AND COALESCE((SELECT sm.original_source_id FROM source_merge_messages sm WHERE sm.message_id = cm.id), cm.source_id) IN (` + placeholders(len(sourceIDs)) + `)
 AND (candidate.provider_event_id = ? OR (candidate.kind = 'series' AND candidate.provider_event_id = ?)))`
		args = append([]any{key.Value, base}, sourceIDs...)
		args = append(args, key.Value, base)
	} else if key.ScheduledAt != nil {
		window, windowArgs := calendarExceptionWindow(ctx, r.dialect, key.ScheduledAt.Add(-calendarCandidateMargin), key.ScheduledAt.Add(calendarCandidateMargin))
		predicate += ` AND (e.kind != 'exception' OR (` + window + `))`
		args = append(args, windowArgs...)
	} else {
		predicate += ` AND e.kind != 'exception'`
	}
	return predicate, args, nil
}

// Resolve source configuration once for this batch's read snapshot.
func meetingCalendarSources(ctx context.Context, r reboundReader) (map[string][]any, error) {
	if r.batch != nil && r.batch.calendarSources != nil {
		return r.batch.calendarSources, nil
	}
	scope, args := sourceScopeClause(ctx, "m.source_id")
	rows, err := r.QueryContext(ctx, `SELECT id,sync_config FROM sources WHERE source_type = 'gcal' AND EXISTS (SELECT 1 FROM messages m WHERE m.deleted_at IS NULL AND COALESCE((SELECT sm.original_source_id FROM source_merge_messages sm WHERE sm.message_id=m.id),m.source_id)=sources.id`+scope+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	sources := map[string][]any{}
	for rows.Next() {
		var id int64
		var config sql.NullString
		if err := rows.Scan(&id, &config); err != nil {
			return nil, err
		}
		mailbox := calendarMailbox(config)
		sources[mailbox] = append(sources[mailbox], id)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	if r.batch != nil {
		r.batch.calendarSources = sources
	}
	return sources, nil
}

func (s *Store) automaticMeetingCandidates(ctx context.Context, r reboundReader, key meetingLinkKey, budget *int) ([]linkOccurrence, error) {
	predicate, args, err := s.automaticMeetingPredicate(ctx, r, key)
	if err != nil {
		return nil, err
	}
	base, instance, _ := calrecur.ParseInstanceID(key.Value)
	rows, err := meetingEventRows(ctx, r, predicate, args, budget)
	if err != nil {
		return nil, err
	}
	ownerRows := rows
	if key.Kind == "google_event_id" {
		if len(ownerRows) == 0 {
			return nil, nil
		}
		// Other calendars can expose conflicting identities for a calendar-scoped ID.
		base, _, _ := calrecur.ParseInstanceID(key.Value)
		rows, err = meetingEventRows(ctx, r, `e.provider_event_id IN (?, ?)`, []any{key.Value, base}, budget)
		if err != nil {
			return nil, err
		}
	}
	var out []linkOccurrence
candidatesLoop:
	for _, row := range rows {
		if row.Event.Kind == meetingExceptionKind {
			for _, master := range rows {
				if master.Event.Kind == meetingSeriesKind && master.SourceID == row.SourceID && master.Event.ProviderEventID == row.Event.SeriesProviderID {
					continue candidatesLoop
				}
			}
		}
		var candidates []linkOccurrence
		if key.Kind == "google_event_id" {
			ref := OccurrenceRef{MessageID: row.MessageID}
			if base != "" && row.Event.ProviderEventID == base {
				ref.Key = instance.Key()
			}
			o, err := resolvedMeetingOccurrence(ctx, r, ref, budget)
			if errors.Is(err, sql.ErrNoRows) || errors.Is(err, calrecur.ErrUnsupportedRecurrence) {
				continue
			}
			if err != nil {
				return nil, err
			}
			candidates = append(candidates, o)
		} else if row.Event.Kind == meetingSeriesKind {
			if key.ScheduledAt == nil {
				continue
			}
			cached := false
			if r.batch != nil {
				// A moved exception can put another occurrence at the same start.
				predicate, args := meetingSeriesExceptionsPredicate(ctx, r.dialect, row, *key.ScheduledAt, key.ScheduledAt.Add(time.Second))
				related, loaded := r.batch.events[meetingQueryKey(predicate, args)]
				ref := OccurrenceRef{MessageID: row.MessageID, Key: (calrecur.Identity{Instant: *key.ScheduledAt}).Key()}
				if o, ok := r.batch.occurrences[ref]; loaded && len(related) == 0 && ok && o.Start.Equal(*key.ScheduledAt) {
					candidates = append(candidates, o)
					cached = true
				}
			}
			if !cached {
				// PostgreSQL rounds a nanosecond window to zero; compare exact starts below.
				candidates, err = expandMeetingSeries(ctx, r, row, *key.ScheduledAt, key.ScheduledAt.Add(time.Second), budget)
				if err != nil && !errors.Is(err, calrecur.ErrUnsupportedRecurrence) {
					return nil, err
				}
				if err == nil && r.batch != nil {
					if r.batch.occurrences == nil {
						r.batch.occurrences = map[OccurrenceRef]linkOccurrence{}
					}
					for _, o := range candidates {
						r.batch.occurrences[o.Ref] = o
					}
				}
			}
		} else {
			o, err := resolvedMeetingOccurrence(ctx, r, OccurrenceRef{MessageID: row.MessageID}, budget)
			if errors.Is(err, sql.ErrNoRows) || errors.Is(err, calrecur.ErrUnsupportedRecurrence) {
				continue
			}
			if err != nil {
				return nil, err
			}
			candidates = append(candidates, o)
		}
		for _, o := range candidates {
			if key.Kind == "google_event_id" && len(out) > 0 && o.Identity != out[0].Identity {
				return nil, nil
			}
			if key.Kind == "ical_uid" && row.Event.Kind != "single" {
				if key.ScheduledAt == nil || !o.Start.Equal(*key.ScheduledAt) {
					continue
				}
			}
			out = append(out, o)
		}
	}
	if key.Kind == "google_event_id" {
		out = slices.DeleteFunc(out, func(o linkOccurrence) bool {
			return !slices.ContainsFunc(ownerRows, func(row CalendarEventRow) bool {
				return row.SourceID == o.Row.SourceID
			})
		})
	}
	if key.ScheduledAt != nil {
		matching := slices.Clone(out)
		matching = slices.DeleteFunc(matching, func(o linkOccurrence) bool { return !o.Start.Equal(*key.ScheduledAt) })
		if len(matching) > 0 {
			out = matching
		}
	}
	return out, nil
}

type meetingLinkDecision struct {
	Ref      OccurrenceRef
	Decision string
}

func (s *Store) meetingMemberOccurrences(ctx context.Context, r reboundReader, memberID int64, budget *int) ([]linkOccurrence, error) {
	decisions, err := readMeetingLinkDecisions(ctx, r, memberID)
	if err != nil {
		return nil, err
	}
	var out []linkOccurrence
	splits := map[meetingIdentity]bool{}
	for _, d := range decisions {
		if d.Decision == "split" {
			resolved, err := resolvedMeetingOccurrence(ctx, r, d.Ref, budget)
			if err == nil {
				splits[resolved.Identity] = true
				continue
			}
			if !errors.Is(err, sql.ErrNoRows) && !errors.Is(err, calrecur.ErrUnsupportedRecurrence) {
				return nil, err
			}
			rows, err := meetingEventRowsByID(ctx, r, d.Ref.MessageID, budget)
			if err != nil {
				return nil, err
			}
			if len(rows) == 1 {
				splits[occurrenceFromRow(rows[0], d.Ref.Key).Identity] = true
			}
			continue
		}
		o, err := resolvedMeetingOccurrence(ctx, r, d.Ref, budget)
		if errors.Is(err, sql.ErrNoRows) || errors.Is(err, calrecur.ErrUnsupportedRecurrence) {
			continue
		}
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	key, err := memberLinkKey(ctx, r, memberID)
	if err != nil {
		return nil, err
	}
	matches, err := s.automaticMeetingCandidates(ctx, r, key, budget)
	if err != nil {
		return nil, err
	}
	for _, o := range matches {
		if o.Identity != matches[0].Identity {
			matches = nil
			break
		}
	}
	out = slices.DeleteFunc(append(out, matches...), func(o linkOccurrence) bool {
		return splits[o.Identity]
	})
	return uniqueMeetingOccurrences(out), nil
}

func (s *Store) meetingReader() reboundReader {
	return reboundReader{loggedReader: s.db, dialect: s.dialect}
}

// Choose the lowest canonical reference across copies of the same occurrence.
func uniqueMeetingOccurrences(out []linkOccurrence) []linkOccurrence {
	slices.SortFunc(out, func(a, b linkOccurrence) int {
		if c := cmp.Compare(a.Ref.MessageID, b.Ref.MessageID); c != 0 {
			return c
		}
		return cmp.Compare(a.Ref.Key, b.Ref.Key)
	})
	seen := make(map[meetingIdentity]bool, len(out))
	return slices.DeleteFunc(out, func(o linkOccurrence) bool {
		if seen[o.Identity] {
			return true
		}
		seen[o.Identity] = true
		return false
	})
}

func (s *Store) meetingIdentityPredicate(identity meetingIdentity) (string, []any) {
	predicate, args := s.calendarUIDPredicate(identity.ICalUID)
	if identity.ICalUID == "" {
		predicate, args = "e.message_id = ?", []any{identity.EventMessageID}
	}
	if identity.ICalUID != "" {
		if identity.OccurrenceKey == "" {
			predicate += ` AND (e.kind = 'single' OR (e.kind = 'exception' AND e.original_start_at IS NULL AND e.original_start_date = ''))`
		} else {
			_, id, ok := calrecur.ParseInstanceID("event_" + identity.OccurrenceKey)
			if !ok {
				return "1=0", nil
			}
			column, value := "e.original_start_at", any(id.Instant)
			if id.AllDay {
				column, value = "e.original_start_date", id.Date
			}
			predicate += ` AND (e.kind IN ('series', 'single') OR (e.kind = 'exception' AND ` + column + ` = ?))`
			args = append(args, value)
		}
	}

	return predicate, args
}

func (s *Store) meetingIdentityRows(ctx context.Context, r reboundReader, identity meetingIdentity, budget *int) ([]CalendarEventRow, error) {
	predicate, args := s.meetingIdentityPredicate(identity)
	rows, err := meetingEventRows(ctx, r, predicate, args, budget)
	if err != nil {
		return nil, err
	}
	return rows, nil
}

func (s *Store) resolveMeetingIdentity(ctx context.Context, r reboundReader, identity meetingIdentity, budget *int) ([]linkOccurrence, error) {
	rows, err := s.meetingIdentityRows(ctx, r, identity, budget)
	if err != nil {
		return nil, err
	}
	out, err := meetingIdentityCopyOccurrences(ctx, r, identity, rows, budget)
	return uniqueMeetingOccurrences(out), err
}

func meetingIdentityCopyRefs(identity meetingIdentity, rows []CalendarEventRow) []OccurrenceRef {
	refs := make([]OccurrenceRef, 0, len(rows))
	for _, row := range rows {
		ref := OccurrenceRef{MessageID: row.MessageID, Key: identity.OccurrenceKey}
		if row.Event.Kind == meetingExceptionKind {
			ref.Key = ""
		}
		refs = append(refs, ref)
	}
	return refs
}

func meetingIdentityCopyOccurrences(ctx context.Context, r reboundReader, identity meetingIdentity, rows []CalendarEventRow, budget *int) ([]linkOccurrence, error) {
	var out []linkOccurrence
	for _, ref := range meetingIdentityCopyRefs(identity, rows) {
		o, err := resolvedMeetingOccurrence(ctx, r, ref, budget)
		if errors.Is(err, sql.ErrNoRows) || errors.Is(err, calrecur.ErrUnsupportedRecurrence) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if o.Identity == identity {
			out = append(out, o)
		}
	}
	return out, nil
}
