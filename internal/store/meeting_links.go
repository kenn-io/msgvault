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
	Exceptions []CalendarEventRow
	Identity   meetingIdentity
	Start, End time.Time
}

type reboundReader struct {
	loggedReader

	dialect     Dialect
	expansions  map[meetingExpansionKey]meetingReadResult[[]linkOccurrence]
	occurrences map[OccurrenceRef]meetingReadResult[linkOccurrence]
}

func meetingEventRows(ctx context.Context, r reboundReader, predicate string, args []any, budget *int) ([]CalendarEventRow, error) {
	rows, err := queryCalendarEventRowsWithinBudget(ctx, r, predicate, args, min(200, *budget))
	if err != nil {
		return nil, err
	}
	*budget -= len(rows)
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
		// Unidentified exceptions use their own message reference.
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

func expandMeetingSeries(ctx context.Context, r reboundReader, master CalendarEventRow, from, to time.Time, budget *int) (out []linkOccurrence, err error) {
	if r.expansions != nil {
		key := meetingExpansionKey{master.MessageID, master.ICalUID, master.Event.Status, from.UTC(), to.UTC()}
		if cached, ok := r.expansions[key]; ok {
			return slices.Clone(cached.value), cached.err
		}
		defer func() { r.expansions[key] = meetingReadResult[[]linkOccurrence]{slices.Clone(out), err} }()
	}
	predicate, args := calendarExceptionWindow(r.dialect, from, to)
	predicate += ` AND m.source_id = ? AND e.series_provider_id = ?`
	args = append(args, master.SourceID, master.Event.ProviderEventID)
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
	out = make([]linkOccurrence, 0, len(expanded))
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

func resolveMeetingOccurrence(ctx context.Context, r reboundReader, ref OccurrenceRef, budget *int) (out linkOccurrence, err error) {
	if r.occurrences != nil {
		if cached, ok := r.occurrences[ref]; ok {
			return cached.value, cached.err
		}
		defer func(ref OccurrenceRef) { r.occurrences[ref] = meetingReadResult[linkOccurrence]{out, err} }(ref)
	}
	rows, err := meetingEventRowsByID(ctx, r, ref.MessageID, budget)
	if err != nil {
		return linkOccurrence{}, err
	}
	if len(rows) != 1 {
		return linkOccurrence{}, sql.ErrNoRows
	}
	row := rows[0]
	if row.Event.Kind == meetingExceptionKind && ref.Key == "" {
		masters, err := meetingEventRows(ctx, r, `e.kind IN ('series', 'single') AND m.source_id = ? AND e.provider_event_id = ?`, []any{row.SourceID, row.Event.SeriesProviderID}, budget)
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
	at := id.Instant
	if id.AllDay {
		zone, err := calendarEventZone(row.Event)
		if err != nil {
			return linkOccurrence{}, err
		}
		at, _, err = calrecur.CivilDateBoundary(id.Date, zone)
		if err != nil {
			return linkOccurrence{}, err
		}
	}
	// A moved exception is expanded at its effective start, retaining Original.
	originalColumn, originalValue := "e.original_start_at", any(id.Instant)
	if id.AllDay {
		originalColumn, originalValue = "e.original_start_date", id.Date
	}
	moved, err := meetingEventRows(ctx, r, `e.kind = 'exception' AND m.source_id = ? AND e.series_provider_id = ? AND `+originalColumn+` = ?`, []any{row.SourceID, row.Event.ProviderEventID, originalValue}, budget)
	if err != nil {
		return linkOccurrence{}, err
	}
	if len(moved) == 0 && row.Event.Kind == "single" && storedOccurrenceKey(row, ref.Key) == "" {
		if row.Event.Start == nil || row.Event.End == nil {
			return linkOccurrence{}, fmt.Errorf("%s: %w", calendarMissingBoundsReason(row.Event), sql.ErrNoRows)
		}
		return occurrenceFromRow(row, ""), nil
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
		o.Exceptions = moved
		if row.Event.Status == meetingCancelledStatus {
			o.Row.Event.Status = meetingCancelledStatus
		}
		return o, nil
	}
	if x.Event.Start != nil {
		at = *x.Event.Start
	}
	master := row
	master.Event.Status = ""
	occurrences, err := expandMeetingSeries(ctx, r, master, at.Add(-calendarCandidateMargin), at.Add(calendarCandidateMargin), budget)
	if err != nil && !errors.Is(err, calrecur.ErrUnsupportedRecurrence) {
		return linkOccurrence{}, err
	}
	for _, o := range occurrences {
		if o.Ref.Key == ref.Key {
			o.Exceptions = moved
			if row.Event.Status == meetingCancelledStatus {
				o.Row.Event.Status = meetingCancelledStatus
			}
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
	var key meetingLinkKey
	var at sql.NullTime
	err := r.QueryRowContext(ctx, `SELECT kind,value,scheduled_at FROM meeting_link_key WHERE message_id = ?`, id).Scan(&key.Kind, &key.Value, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return key, nil
	}
	if at.Valid {
		key.ScheduledAt = &at.Time
	}
	return key, err
}

func (s *Store) calendarUIDPredicate() string {
	return calendarUIDExpression(s.dialect, "m.metadata") + ` = ? AND m.message_type = 'calendar_event'`
}

func conflictingGoogleMeetingRows(rows []CalendarEventRow) bool {
	var identity *meetingIdentity
	for _, row := range rows {
		if row.Event.Kind != "single" && row.Event.Kind != meetingSeriesKind {
			continue
		}
		current := occurrenceFromRow(row, "").Identity
		if identity != nil && *identity != current {
			return true
		}
		identity = &current
	}
	return false
}

func (s *Store) automaticMeetingOccurrences(ctx context.Context, r reboundReader, key meetingLinkKey, budget *int) ([]linkOccurrence, error) {
	if key.Kind != "google_event_id" && key.Kind != "ical_uid" {
		return nil, nil
	}
	predicate, args := s.calendarUIDPredicate(), []any{key.Value}
	base, instance, _ := calrecur.ParseInstanceID(key.Value)
	if key.Kind == "google_event_id" {
		predicate = `e.provider_event_id IN (?, ?)`
		args = []any{key.Value, base}
	} else if key.ScheduledAt != nil {
		window, windowArgs := calendarExceptionWindow(r.dialect, key.ScheduledAt.Add(-calendarCandidateMargin), key.ScheduledAt.Add(calendarCandidateMargin))
		predicate += ` AND (e.kind != 'exception' OR (` + window + `))`
		args = append(args, windowArgs...)
	} else {
		predicate += ` AND e.kind != 'exception'`
	}
	rows, err := meetingEventRows(ctx, r, predicate, args, budget)
	if err != nil {
		return nil, err
	}
	if key.Kind == "google_event_id" && conflictingGoogleMeetingRows(rows) {
		return nil, nil
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
			o, err := resolveMeetingOccurrence(ctx, r, ref, budget)
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
			// PostgreSQL rounds a nanosecond window to zero; compare exact starts below.
			candidates, err = expandMeetingSeries(ctx, r, row, *key.ScheduledAt, key.ScheduledAt.Add(time.Second), budget)
			if err != nil && !errors.Is(err, calrecur.ErrUnsupportedRecurrence) {
				return nil, err
			}
			original := calrecur.Identity{Instant: *key.ScheduledAt}
			if row.Event.AllDay {
				zone, err := calendarEventZone(row.Event)
				if err != nil {
					return nil, err
				}
				original = calrecur.Identity{AllDay: true, Date: key.ScheduledAt.In(zone).Format(time.DateOnly)}
			}
			o, err := resolveMeetingOccurrence(ctx, r, OccurrenceRef{MessageID: row.MessageID, Key: original.Key()}, budget)
			if err == nil {
				candidates = slices.DeleteFunc(candidates, func(candidate linkOccurrence) bool { return candidate.Ref == o.Ref })
				candidates = append(candidates, o)
			} else if !errors.Is(err, sql.ErrNoRows) && !errors.Is(err, calrecur.ErrUnsupportedRecurrence) {
				return nil, err
			}
		} else {
			o, err := resolveMeetingOccurrence(ctx, r, OccurrenceRef{MessageID: row.MessageID}, budget)
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
				starts, _, err := meetingCopyStarts(ctx, r, o, nil, budget)
				if err != nil {
					return nil, err
				}
				if key.ScheduledAt == nil || !slices.ContainsFunc(starts, func(at any) bool {
					instant, ok := at.(time.Time)
					return ok && instant.Equal(*key.ScheduledAt)
				}) {
					continue
				}
			}
			out = append(out, o)
		}
	}
	if key.ScheduledAt != nil {
		matching := slices.Clone(out)
		matching = slices.DeleteFunc(matching, func(o linkOccurrence) bool { return !o.Start.Equal(*key.ScheduledAt) })
		if len(matching) > 0 {
			out = matching
		}
	}
	for _, o := range out {
		if o.Identity != out[0].Identity {
			return nil, nil
		}
	}
	return out, nil
}

type meetingLinkDecision struct {
	Ref      OccurrenceRef
	Decision string
}

// savedOccurrence keeps a stale identity when the calendar no longer generates
// a saved link. The transcript still proves attendance for meeting counts.
func savedOccurrence(ctx context.Context, r reboundReader, ref OccurrenceRef, budget *int) (linkOccurrence, bool, error) {
	rows, err := meetingEventRowsByID(ctx, r, ref.MessageID, budget)
	if err != nil {
		return linkOccurrence{}, false, err
	}
	if len(rows) != 1 {
		return linkOccurrence{}, false, sql.ErrNoRows
	}
	if rows[0].Event.Kind == meetingSeriesKind {
		ref.Key = storedOccurrenceKey(rows[0], ref.Key)
	}
	o, err := resolveMeetingOccurrence(ctx, r, ref, budget)
	if errors.Is(err, sql.ErrNoRows) || errors.Is(err, calrecur.ErrUnsupportedRecurrence) {
		return occurrenceFromRow(rows[0], ref.Key), false, nil
	}
	return o, err == nil, err
}

func (s *Store) meetingMemberOccurrences(ctx context.Context, r reboundReader, memberID int64, budget *int) ([]linkOccurrence, []meetingIdentity, map[meetingIdentity]bool, bool, error) {
	decisions, err := readMeetingLinkDecisions(ctx, r, memberID)
	if err != nil {
		return nil, nil, nil, false, err
	}
	var out []linkOccurrence
	var stale []meetingIdentity
	hasLink := false
	splits := map[meetingIdentity]bool{}
	for _, d := range decisions {
		if d.Decision == "link" {
			hasLink = true
		}
		o, current, err := savedOccurrence(ctx, r, d.Ref, budget)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, nil, nil, false, err
		}
		if d.Decision == "split" {
			splits[o.Identity] = true
		} else if current {
			out = append(out, o)
		} else {
			stale = append(stale, o.Identity)
		}
	}
	matches, err := s.memberAutomaticOccurrences(ctx, r, memberID, budget)
	if err != nil {
		return nil, nil, nil, false, err
	}
	out = slices.DeleteFunc(append(out, matches...), func(o linkOccurrence) bool { return splits[o.Identity] })
	stale = slices.DeleteFunc(stale, func(id meetingIdentity) bool { return splits[id] })
	return uniqueMeetingOccurrences(out), stale, splits, hasLink || len(matches) > 0, nil
}

func (s *Store) memberAutomaticOccurrences(ctx context.Context, r reboundReader, memberID int64, budget *int) ([]linkOccurrence, error) {
	key, err := memberLinkKey(ctx, r, memberID)
	if err != nil {
		return nil, err
	}
	return s.automaticMeetingOccurrences(ctx, r, key, budget)
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

func (s *Store) meetingIdentityRows(ctx context.Context, r reboundReader, identity meetingIdentity, budget *int) ([]CalendarEventRow, error) {
	predicate, args := s.calendarUIDPredicate(), []any{identity.ICalUID}
	if identity.OccurrenceKey == "" {
		predicate += ` AND (e.kind = 'single' OR (e.kind = 'exception' AND e.original_start_at IS NULL AND e.original_start_date = ''))`
	} else {
		_, id, ok := calrecur.ParseInstanceID("event_" + identity.OccurrenceKey)
		if !ok {
			return nil, nil
		}
		column, value := "e.original_start_at", any(id.Instant)
		if id.AllDay {
			column, value = "e.original_start_date", id.Date
		}
		predicate += ` AND (e.kind IN ('series', 'single') OR (e.kind = 'exception' AND ` + column + ` = ?))`
		args = append(args, value)
	}
	return meetingEventRows(ctx, r, predicate, args, budget)
}
