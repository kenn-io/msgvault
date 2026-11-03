package store

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/calevent"
	"go.kenn.io/msgvault/internal/calrecur"
	"go.kenn.io/msgvault/internal/sourcescope"
)

var (
	ErrCalendarOccurrenceNotFound = errors.New("calendar occurrence not found")
	ErrCalendarOccurrenceInvalid  = errors.New("invalid calendar occurrence query")
)

type CalendarOccurrenceQuery struct {
	From, To time.Time
	Limit    *int
	Cursor   string
}

type CalendarOccurrence struct {
	Ref               string              `json:"ref"`
	MessageID         int64               `json:"message_id"`
	SeriesMessageID   int64               `json:"series_message_id"`
	SourceID          int64               `json:"source_id"`
	Subject           string              `json:"subject"`
	Start             time.Time           `json:"start"`
	End               time.Time           `json:"end"`
	AllDay            bool                `json:"all_day"`
	StartDate         string              `json:"start_date"`
	EndDate           string              `json:"end_date"`
	Status            string              `json:"status"`
	DeletedFromSource bool                `json:"deleted_from_source"`
	Attendees         []calevent.Attendee `json:"attendees"`
}
type CalendarOccurrenceDetail struct {
	CalendarOccurrence
	calevent.Content
}

type CalendarUnexpandable struct {
	MessageID int64  `json:"message_id"`
	Reason    string `json:"reason"`
}
type CalendarOccurrencePage struct {
	NextCursor   string                 `json:"next_cursor,omitempty"`
	Occurrences  []CalendarOccurrence   `json:"occurrences"`
	Unexpandable []CalendarUnexpandable `json:"unexpandable"`
}

// ListCalendarOccurrencesContext expands archived series in a half-open window.
func (s *Store) ListCalendarOccurrencesContext(ctx context.Context, query CalendarOccurrenceQuery) (CalendarOccurrencePage, error) {
	from, to := query.From, query.To
	if from.IsZero() || to.IsZero() || to.Before(from) || to.Equal(from) && to.Format(time.DateOnly) <= from.Format(time.DateOnly) || to.Sub(from) > 400*24*time.Hour {
		return CalendarOccurrencePage{}, fmt.Errorf("%w: window must be positive and at most 400 days", ErrCalendarOccurrenceInvalid)
	}
	limit := 100
	if query.Limit != nil {
		limit = *query.Limit
	}
	if limit < 1 || limit > 500 {
		return CalendarOccurrencePage{}, fmt.Errorf("%w: limit must be between 1 and 500", ErrCalendarOccurrenceInvalid)
	}
	page := CalendarOccurrencePage{Occurrences: []CalendarOccurrence{}, Unexpandable: []CalendarUnexpandable{}}
	ctx, release, err := s.BeginReadSnapshotContext(ctx)
	if err != nil {
		return page, err
	}
	defer release()
	uid, err := s.ArchiveUIDContext(ctx)
	if err != nil {
		return page, err
	}
	ids := sourcescope.IDs(ctx)
	slices.Sort(ids)
	ids = slices.Compact(ids)
	hash, err := readFilterHash(struct {
		From       string  `json:"from"`
		To         string  `json:"to"`
		Restricted bool    `json:"restricted"`
		SourceIDs  []int64 `json:"source_ids"`
	}{from.Format(time.RFC3339Nano), to.Format(time.RFC3339Nano), sourcescope.Restricted(ctx), ids})
	if err != nil {
		return page, err
	}
	cursor, err := decodeCalendarOccurrenceCursor(query.Cursor, uid, hash)
	if err != nil {
		return page, err
	}
	rows, remaining, err := s.calendarOccurrenceRows(ctx, from, to)
	if err != nil {
		return page, err
	}
	exceptions := map[calendarSeriesKey]map[calrecur.Identity]bool{}
	masters := map[calendarSeriesKey]*calendarSeries{}
	for _, row := range rows {
		if row.Event.ProviderEventID != "" && (row.Event.Kind == "series" || row.Event.Kind == "single") {
			masters[calendarSeriesKey{row.SourceID, row.Event.ProviderEventID}] = newCalendarSeries(row)
		}
	}
	for _, row := range rows {
		// A removed CalDAV override restores the original occurrence. Overrides
		// deleted with their resource remain part of its final archived snapshot.
		if row.Event.Kind != "exception" || row.Event.Original == nil {
			continue
		}
		if row.SourceType == calevent.CalDAVSourceType {
			master := masters[calendarSeriesKey{row.SourceID, row.Event.SeriesProviderID}]
			if row.OverrideRemoved || master != nil && row.DeletedFromSource && !master.row.DeletedFromSource {
				continue
			}
		}
		key := calendarSeriesKey{row.SourceID, row.Event.SeriesProviderID}
		if exceptions[key] == nil {
			exceptions[key] = map[calrecur.Identity]bool{}
		}
		exceptions[key][*row.Event.Original] = true
	}
	tombstones, err := queryICSSparseCancellationTombstones(ctx, s.db, rows)
	if err != nil {
		return page, err
	}
	for i := range rows {
		row := &rows[i]
		if row.Event.Kind == "exception" && tombstones[calendarSeriesKey{row.SourceID, row.Event.SeriesProviderID}] && masters[calendarSeriesKey{row.SourceID, row.Event.SeriesProviderID}] == nil {
			row.Event.Status = "cancelled"
		}
	}
	page.Occurrences, page.Unexpandable, err = collectCalendarOccurrences(ctx, s, rows, masters, exceptions, from, to, cursor, remaining)
	if err != nil {
		return page, err
	}
	slices.SortFunc(page.Occurrences, func(a, b CalendarOccurrence) int {
		if c := a.Start.Compare(b.Start); c != 0 {
			return c
		}
		return strings.Compare(a.Ref, b.Ref)
	})
	if query.Cursor != "" {
		page.Unexpandable = []CalendarUnexpandable{}
	}
	if len(page.Occurrences) > limit {
		page.Occurrences = page.Occurrences[:limit]
		last := page.Occurrences[len(page.Occurrences)-1]
		page.NextCursor, err = encodeReadCursor(calendarOccurrenceCursor{Version: 1, ArchiveUID: uid, FilterHash: hash, Start: last.Start, Ref: last.Ref})
		if err != nil {
			return page, err
		}
	}
	return page, nil
}

func collectCalendarOccurrences(ctx context.Context, s *Store, rows []CalendarEventRow, masters map[calendarSeriesKey]*calendarSeries, exceptions map[calendarSeriesKey]map[calrecur.Identity]bool, from, to time.Time, after calendarOccurrenceCursor, remaining int) ([]CalendarOccurrence, []CalendarUnexpandable, error) {
	page := CalendarOccurrencePage{Occurrences: []CalendarOccurrence{}, Unexpandable: []CalendarUnexpandable{}}
	// Steps still run from DTSTART, so budgets match a full read; only earlier output is skipped.
	floor := from
	if !after.Start.IsZero() && after.Start.After(from) {
		floor = after.Start
	}
	reported := map[int64]bool{}
	report := func(id int64, reason string) {
		if reason != "" && !reported[id] {
			page.Unexpandable = append(page.Unexpandable, CalendarUnexpandable{MessageID: id, Reason: reason})
			reported[id] = true
		}
	}
	add := func(o CalendarOccurrence) {
		if calendarOccurrenceOverlaps(o, from, to) && (after.Start.IsZero() || o.Start.After(after.Start) || (o.Start.Equal(after.Start) && o.Ref > after.Ref)) {
			page.Occurrences = append(page.Occurrences, o)
		}
	}
	boundedOnly := map[calendarSeriesKey]bool{}
	for _, row := range rows {
		if row.Event.Kind != "series" {
			continue
		}
		e := row.Event
		report(row.MessageID, row.evidenceError)
		series := masters[calendarSeriesKey{row.SourceID, e.ProviderEventID}]
		occurrences, count, reason, bounded, err := series.expand(ctx, floor, to, remaining)
		remaining -= count
		if err != nil {
			return nil, nil, err
		}
		boundedOnly[calendarSeriesKey{row.SourceID, e.ProviderEventID}] = bounded
		if reason != "" {
			report(row.MessageID, reason)
		}
		for _, o := range occurrences {
			if !exceptions[calendarSeriesKey{row.SourceID, e.ProviderEventID}][o.Original] {
				add(calendarOccurrence(row, row.MessageID, calendarOccurrenceRef(row.MessageID, o.Original.Key()), o))
			}
		}
	}
	for _, row := range rows {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		e := row.Event
		if e.Kind == "series" {
			continue
		}
		if row.SourceType == calevent.CalDAVSourceType && row.OverrideRemoved {
			// A removed CalDAV override is represented by the master's generated instance.
			continue
		}
		report(row.MessageID, row.evidenceError)
		if e.Kind == "exception" && e.Original == nil {
			report(row.MessageID, "exception has no original start")
		}
		if e.Kind == "exception" && e.Original != nil && !e.Original.AllDay && e.Original.Instant.Nanosecond() != 0 {
			report(row.MessageID, "exception original start must have whole-second precision")
		}
		key := calendarSeriesKey{row.SourceID, e.SeriesProviderID}
		if master := masters[key]; e.Kind == "exception" && master != nil && e.Original != nil && e.Original.AllDay != master.row.Event.AllDay && (e.Start == nil || e.End == nil) {
			report(row.MessageID, "exception original type differs from series and event bounds are missing")
			continue
		}
		if boundedOnly[key] && (e.Start == nil || e.End == nil) {
			continue
		}
		occurrence, evidence, err := masters[key].exception(ctx, s, row)
		if errors.Is(err, ErrCalendarOccurrenceInvalid) {
			report(row.MessageID, calendarMissingBoundsReason(e))
		} else if err != nil {
			return nil, nil, err
		} else {
			if evidence != nil {
				report(row.MessageID, evidence.diagnostic)
			}
			add(occurrence)
		}
	}

	return page.Occurrences, page.Unexpandable, nil
}

func (s *Store) calendarOccurrenceRows(ctx context.Context, from, to time.Time) ([]CalendarEventRow, int, error) {
	rows, err := calendarEventRowsInWindow(ctx, s.meetingReader(ctx), from, to)
	if err != nil {
		return nil, 0, err
	}
	remaining := calrecur.RequestReadBudget - len(rows)
	if remaining < 0 {
		return nil, 0, calendarRecurrenceBudgetError()
	}
	if err := loadCalendarICSRecurrenceErrors(ctx, s.db, rows, &remaining); err != nil {
		return nil, 0, err
	}
	return rows, remaining, nil
}

func calendarMissingBoundsReason(event calevent.Event) string {
	if event.Status == calendarEventDecodeFailureStatus {
		return "event data could not be decoded"
	}
	if event.Kind == meetingExceptionKind {
		return "missing series master and event bounds"
	}
	return "missing event bounds"
}

type calendarSeries struct {
	row       CalendarEventRow
	set       calrecur.Set
	duration  time.Duration
	days      int
	boundsErr error
	err       error
}

func newCalendarSeries(row CalendarEventRow) *calendarSeries {
	set, duration, days, err := prepareCalendarBounds(row.Event)
	series := &calendarSeries{row: row, set: set, duration: duration, days: days, boundsErr: err, err: err}
	if err == nil {
		parsed, parseErr := parseCalendarSeriesSet(row.Event, set)
		series.err = parseErr
		if parseErr == nil {
			series.set = parsed
		}
	}
	return series
}

// Typed rows keep damaged recurrence evidence out of expansion; ICS wall
// starts preserve the provider's civil DTSTART across offset gaps.
func parseCalendarSeriesSet(e calevent.Event, bounds calrecur.Set) (calrecur.Set, error) {
	if e.RecurrenceError != "" {
		return calrecur.Set{}, errors.New(e.RecurrenceError)
	}
	if e.StartWall != nil {
		return calrecur.ParseSetWithCivilStart(bounds.DTStart, *e.StartWall, bounds.AllDay, bounds.StartDate, bounds.Zone, e.Recurrence)
	}
	return calrecur.ParseSet(bounds.DTStart, bounds.AllDay, bounds.StartDate, bounds.Zone, e.Recurrence)
}

// Both readers resolve rows independently of expansion windows and budgets.
func (series *calendarSeries) exception(ctx context.Context, s *Store, row CalendarEventRow) (CalendarOccurrence, *calendarCancellationEvidence, error) {
	e := row.Event
	var o calrecur.Occurrence
	var evidence *calendarCancellationEvidence
	seriesID := int64(0)
	resolved := false
	if series != nil {
		seriesID = series.row.MessageID
		if series.boundsErr == nil {
			exceptions, err := calendarRecurrenceExceptions([]CalendarEventRow{row})
			if err == nil {
				o, err = calrecur.EffectiveException(series.set, exceptions[0], series.duration, series.days)
				resolved = err == nil
			}
		}
		if row.Event.Status == "cancelled" {
			var err error
			row, evidence, err = inheritCalendarCancellation(ctx, s.db, row, series.row)
			if err != nil {
				return CalendarOccurrence{}, nil, err
			}
		}
		if series.row.Event.Status == "cancelled" {
			row.Event.Status = "cancelled"
		}
	}
	if !resolved {
		if e.Start == nil || e.End == nil {
			return CalendarOccurrence{}, nil, fmt.Errorf("%w: %s", ErrCalendarOccurrenceInvalid, calendarMissingBoundsReason(e))
		}
		o = calrecur.Occurrence{Start: *e.Start, End: *e.End, AllDay: e.AllDay, StartDate: e.StartDate, EndDate: e.EndDate, Cancelled: e.Status == "cancelled"}
	}
	return calendarOccurrence(row, seriesID, calendarOccurrenceRef(row.MessageID, ""), o), evidence, nil
}

func (series *calendarSeries) expand(ctx context.Context, from, to time.Time, remaining int) ([]calrecur.Occurrence, int, string, bool, error) {
	if series.err != nil {
		return nil, 0, "unsupported recurrence", true, nil //nolint:nilerr // Report an unsupported series alongside readable rows.
	}
	const requestLimit = "request work limit reached; long-running series count their steps from their start"
	if remaining <= 0 {
		return nil, 0, requestLimit, false, nil
	}
	occurrences, count, err := calrecur.Expand(ctx, series.set, series.duration, series.days, from.Add(-calendarCandidateMargin), to.Add(calendarCandidateMargin), min(remaining, calrecur.SeriesReadBudget))
	switch {
	case ctx.Err() != nil:
		return nil, count, "", false, ctx.Err()
	case errors.Is(err, calrecur.ErrBudgetExceeded) && count > remaining:
		return nil, count, requestLimit, false, nil
	case errors.Is(err, calrecur.ErrBudgetExceeded):
		return nil, count, err.Error(), true, nil
	case err != nil:
		return nil, count, "unsupported recurrence", true, nil //nolint:nilerr // Report an unsupported series alongside readable rows.
	}
	return occurrences, count, "", false, nil
}

// Resolve archived exceptions once, then suppress their original generated instances.
func expandCalendarSeries(ctx context.Context, row CalendarEventRow, exceptionRows []CalendarEventRow, from, to time.Time, remaining int) ([]calrecur.Occurrence, int, string, error) {
	exceptionRows = eligibleCalendarExceptionRows(row, exceptionRows)
	set, duration, days, prepareErr := prepareCalendarRecurrence(row.Event)
	var prepared *calrecur.Set
	if prepareErr == nil {
		prepared = &set
	}
	resolved, bounded, err := resolveCalendarExceptions(ctx, prepared, exceptionRows, duration, days)
	if ctx.Err() != nil {
		return nil, 0, "", ctx.Err()
	}
	if prepared == nil || err != nil {
		return bounded, 0, "unsupported recurrence", nil //nolint:nilerr // Report an unsupported series alongside readable occurrences.
	}
	if row.Event.Kind == "single" {
		return resolved, 0, "", nil
	}
	const requestLimit = "request work limit reached; long-running series count their steps from their start"
	if remaining <= 0 {
		return resolved, 0, requestLimit, nil
	}
	occurrences, count, err := calrecur.Expand(ctx, set, duration, days, from.Add(-calendarCandidateMargin), to.Add(calendarCandidateMargin), min(remaining, calrecur.SeriesReadBudget))
	if err != nil {
		if ctx.Err() != nil {
			return nil, count, "", ctx.Err()
		}
		if errors.Is(err, calrecur.ErrBudgetExceeded) {
			if count > remaining {
				return resolved, count, requestLimit, nil
			}
			return bounded, count, err.Error(), nil
		}
		return bounded, count, "unsupported recurrence", nil
	}
	return append(occurrences, resolved...), count, "", nil
}

// Keep unidentified edits and edits of a former series type alongside resolved overrides.
// Preserve all bounded edits for generation failures.
func resolveCalendarExceptions(ctx context.Context, set *calrecur.Set, rows []CalendarEventRow, duration time.Duration, days int) (resolved, bounded []calrecur.Occurrence, err error) {
	exceptions := calendarExceptionRows(rows)
	for _, row := range exceptions {
		if err := ctx.Err(); err != nil {
			return nil, bounded, err
		}
		e := row.Event
		if e.Start != nil && e.End != nil {
			o, err := exceptionBoundsFor(set, duration, days, row)
			if err != nil {
				return nil, bounded, err
			}
			bounded = append(bounded, o)
			if e.Original == nil || set != nil && e.Original.AllDay != set.AllDay || !e.Original.AllDay && e.Original.Instant.Nanosecond() != 0 {
				resolved = append(resolved, bounded[len(bounded)-1])
			}
		}
	}
	if set == nil {
		return nil, bounded, nil
	}
	for _, exception := range exceptions {
		if err := ctx.Err(); err != nil {
			return nil, bounded, err
		}
		if exception.Event.Original == nil {
			continue
		}
		// A retained edit's old value type or fractional precision cannot identify a current instance.
		// Its recorded bounds were already kept above.
		if exception.Event.Original.AllDay != set.AllDay || !exception.Event.Original.AllDay && exception.Event.Original.Instant.Nanosecond() != 0 {
			continue
		}
		o, err := exceptionBoundsFor(set, duration, days, exception)
		if err != nil {
			return nil, bounded, err
		}
		resolved = append(resolved, o)
		set.ExDates = append(set.ExDates, *exception.Event.Original)
	}
	return resolved, bounded, nil
}

// Freshness follows provider-observation order. Message IDs break ties when
// multiple snapshots share the same migrated order.
func calendarExceptionOverrides(master CalendarEventRow, rows []CalendarEventRow) map[string]CalendarEventRow {
	rows = eligibleCalendarExceptionRows(master, rows)
	overrides := make(map[string]CalendarEventRow, len(rows))
	for _, row := range rows {
		if row.Event.Original == nil {
			continue
		}
		key := row.Event.Original.Key()
		current, exists := overrides[key]
		if !exists || calendarEventSnapshotNewer(row, current) {
			overrides[key] = row
		}
	}
	return overrides
}

// Provider instance keys have whole-second precision. Alias freshness must
// preserve the retained instant so a fractional edit cannot replace its sibling.
func calendarExceptionAliasKey(original calrecur.Identity) string {
	if original.AllDay {
		return original.Date
	}
	return original.Instant.UTC().Format(time.RFC3339Nano)
}

// Identified snapshots collapse to their freshest alias. Exceptions without
// an original identity remain independent bounded occurrences.
func calendarExceptionRows(rows []CalendarEventRow) []CalendarEventRow {
	type identity struct {
		sourceID int64
		seriesID string
		key      string
	}
	identified := make(map[identity]CalendarEventRow, len(rows))
	byID := make(map[int64]CalendarEventRow, len(rows))
	for _, row := range rows {
		if row.SourceType == calevent.CalDAVSourceType && row.OverrideRemoved {
			continue
		}
		if row.Event.Original == nil {
			byID[row.MessageID] = row
			continue
		}
		key := identity{sourceID: row.SourceID, seriesID: row.Event.SeriesProviderID, key: calendarExceptionAliasKey(*row.Event.Original)}
		current, exists := identified[key]
		if !exists || calendarEventSnapshotNewer(row, current) {
			identified[key] = row
		}
	}
	for _, row := range identified {
		byID[row.MessageID] = row
	}
	exceptions := make([]CalendarEventRow, 0, len(byID))
	for _, row := range byID {
		exceptions = append(exceptions, row)
	}
	slices.SortFunc(exceptions, func(a, b CalendarEventRow) int { return cmp.Compare(a.MessageID, b.MessageID) })
	return exceptions
}

// A removed CalDAV override restores its generated occurrence. Exceptions
// removed with the master remain part of the final archived snapshot.
func eligibleCalendarExceptionRows(master CalendarEventRow, rows []CalendarEventRow) []CalendarEventRow {
	if master.SourceType == calevent.CalDAVSourceType {
		rows = slices.DeleteFunc(slices.Clone(rows), func(row CalendarEventRow) bool {
			return row.OverrideRemoved || row.DeletedFromSource && !master.DeletedFromSource
		})
	}
	return calendarExceptionRows(rows)
}

func calendarRecurrenceBudgetError() error {
	return fmt.Errorf("%w; long-running series count steps from their start; a narrower window may not reduce the work", ErrQueryTooBroad)
}

func calendarRecurrenceExceptions(rows []CalendarEventRow) ([]calrecur.Exception, error) {
	exceptions := make([]calrecur.Exception, 0, len(rows))
	for _, row := range rows {
		e := row.Event
		if e.Original == nil {
			return nil, errors.New("missing exception original start")
		}
		// Explicit dates use the event or calendar zone; omitted bounds inherit the master.
		var zone *time.Location
		if e.EventTimeZone != "" || e.StartDate != "" || e.EndDate != "" {
			var err error
			zone, err = calendarEventZone(e)
			if err != nil {
				return nil, err
			}
		}
		exceptions = append(exceptions, calrecur.Exception{Original: *e.Original, Start: e.Start, End: e.End, StartDate: e.StartDate, EndDate: e.EndDate, Zone: zone, Cancelled: e.Status == "cancelled", Ref: row.MessageID})
	}
	return exceptions, nil
}

func calendarEventZone(e calevent.Event) (*time.Location, error) {
	name := e.EventTimeZone
	if name == "" {
		name = e.CalendarTimeZone
	}
	if name == "" {
		return time.UTC, nil
	}
	zone, err := time.LoadLocation(name)
	if err != nil {
		return nil, fmt.Errorf("calendar time zone: %w", err)
	}
	return zone, nil
}

// All-day dates belong to the query's civil days, independent of the calendar zone.
func calendarOccurrenceOverlaps(o CalendarOccurrence, from, to time.Time) bool {
	if o.AllDay {
		endDate := to.Format(time.DateOnly)
		if to.Hour() != 0 || to.Minute() != 0 || to.Second() != 0 || to.Nanosecond() != 0 {
			endDate = time.Date(to.Year(), to.Month(), to.Day()+1, 0, 0, 0, 0, time.UTC).Format(time.DateOnly)
		}
		return o.StartDate < endDate && o.EndDate > from.Format(time.DateOnly)
	}
	return from.Before(to) && o.Start.Before(to) && (o.End.After(from) || o.Start.Equal(o.End) && !o.Start.Before(from))
}

func prepareCalendarRecurrence(e calevent.Event) (calrecur.Set, time.Duration, int, error) {
	if e.RecurrenceError != "" {
		return calrecur.Set{}, 0, 0, errors.New(e.RecurrenceError)
	}
	if e.Start == nil || e.End == nil {
		return calrecur.Set{}, 0, 0, errors.New("missing event bounds")
	}
	zone, err := calendarEventZone(e)
	if err != nil {
		return calrecur.Set{}, 0, 0, err
	}
	var set calrecur.Set
	if e.StartWall != nil {
		set, err = calrecur.ParseSetWithCivilStart(*e.Start, *e.StartWall, e.AllDay, e.StartDate, zone, e.Recurrence)
	} else {
		set, err = calrecur.ParseSet(*e.Start, e.AllDay, e.StartDate, zone, e.Recurrence)
	}
	if err != nil {
		return set, 0, 0, err
	}
	duration, days := e.End.Sub(*e.Start), e.RecurrenceDurationDays
	if e.AllDay {
		days, err = calendarAllDayDurationDays(e.StartDate, e.EndDate)
		if err != nil {
			return set, 0, 0, err
		}
	} else if days > 0 {
		// Timed civil days are measured in the series zone; the rest is elapsed.
		start := e.Start.In(zone)
		end, err := calrecur.AddCivilDays(start, days)
		if err != nil {
			return set, 0, 0, err
		}
		civil := end.Sub(start)
		if civil > duration {
			return set, 0, 0, errors.New("calendar civil duration exceeds event bounds")
		}
		duration -= civil
	}
	return set, duration, days, nil
}

func prepareCalendarBounds(e calevent.Event) (calrecur.Set, time.Duration, int, error) {
	if e.Start == nil || e.End == nil {
		return calrecur.Set{}, 0, 0, errors.New("missing event bounds")
	}
	zone, err := calendarEventZone(e)
	if err != nil {
		return calrecur.Set{}, 0, 0, err
	}
	set, err := calrecur.ParseSet(*e.Start, e.AllDay, e.StartDate, zone, nil)
	if err != nil {
		return set, 0, 0, err
	}
	days := 0
	if e.AllDay {
		days, err = calendarAllDayDurationDays(e.StartDate, e.EndDate)
		if err != nil {
			return set, 0, 0, err
		}
	} else if e.RecurrenceDurationDays > 0 {
		// Timed civil days are measured in the series zone; the rest is elapsed.
		start := e.Start.In(zone)
		end, err := calrecur.AddCivilDays(start, e.RecurrenceDurationDays)
		if err != nil {
			return set, 0, 0, err
		}
		duration := e.End.Sub(*e.Start) - end.Sub(start)
		return set, duration, e.RecurrenceDurationDays, nil
	}
	return set, e.End.Sub(*e.Start), days, nil
}

func materializeCalendarOccurrence(ctx context.Context, r SQLReader, master CalendarEventRow, byID map[int64]CalendarEventRow, o calrecur.Occurrence) (CalendarEventRow, calrecur.Occurrence, error) {
	effective := master
	if o.HasException {
		effective = byID[o.ExceptionRef]
		if effective.Event.Status == meetingCancelledStatus {
			var err error
			effective, _, err = inheritCalendarCancellation(ctx, r, effective, master)
			if err != nil {
				return effective, o, err
			}
		}
	}
	if master.Event.Status == meetingCancelledStatus {
		o.Cancelled = true
	}
	if o.Cancelled {
		effective.Event.Status = meetingCancelledStatus
	}
	return effective, o, nil
}

type calendarOccurrenceCursor struct {
	Version    int       `json:"version"`
	ArchiveUID string    `json:"archive_uid"`
	FilterHash string    `json:"filter_hash"`
	Start      time.Time `json:"start"`
	Ref        string    `json:"ref"`
}

func decodeCalendarOccurrenceCursor(value, uid, hash string) (calendarOccurrenceCursor, error) {
	var cursor calendarOccurrenceCursor
	if value == "" {
		return cursor, nil
	}
	var header struct {
		Version int `json:"version"`
	}
	err := decodeReadCursor(value, &header, json.RejectUnknownMembers(false))
	if err == nil && header.Version != 1 {
		return cursor, fmt.Errorf("%w: unsupported cursor version %d", ErrCalendarOccurrenceInvalid, header.Version)
	}
	if err == nil {
		err = decodeReadCursor(value, &cursor)
	}
	if err != nil || cursor.ArchiveUID != uid || cursor.FilterHash != hash || cursor.Start.IsZero() || cursor.Ref == "" {
		return cursor, fmt.Errorf("%w: cursor does not match this archive, window, or scope", ErrCalendarOccurrenceInvalid)
	}
	return cursor, nil
}

func calendarOccurrenceRef(id int64, key string) string {
	ref := "evt:" + strconv.FormatInt(id, 10)
	if key != "" {
		ref += "@" + key
	}
	return ref
}

func calendarOccurrence(row CalendarEventRow, seriesID int64, ref string, o calrecur.Occurrence) CalendarOccurrence {
	status := row.Event.Status
	if o.Cancelled {
		status = "cancelled"
	}
	attendees := row.Event.Attendees
	if attendees == nil {
		attendees = []calevent.Attendee{}
	}
	return CalendarOccurrence{Ref: ref, MessageID: row.MessageID, SeriesMessageID: seriesID, SourceID: row.SourceID, Subject: row.Subject, Start: o.Start, End: o.End, AllDay: o.AllDay, StartDate: o.StartDate, EndDate: o.EndDate, Status: status, DeletedFromSource: row.DeletedFromSource, Attendees: attendees}
}

func calendarAllDayDurationDays(startDate, endDate string) (int, error) {
	start, err := time.Parse(time.DateOnly, startDate)
	if err != nil {
		return 0, fmt.Errorf("calendar occurrence date: %w", err)
	}
	end, err := time.Parse(time.DateOnly, endDate)
	if err != nil {
		return 0, fmt.Errorf("calendar occurrence date: %w", err)
	}
	return int(end.Sub(start) / (24 * time.Hour)), nil
}

func readCalendarContent(ctx context.Context, r contextRowQuerier, messageID int64, calendarZone string) (calevent.Content, error) {
	raw, format, metadata, err := readCalendarRaw(ctx, r, messageID)
	if errors.Is(err, sql.ErrNoRows) {
		return calevent.Content{}, nil
	}
	if err != nil {
		return calevent.Content{}, err
	}
	return decodeCalendarContent(ctx, format, raw, metadata, calendarZone)
}

func readCalendarRaw(ctx context.Context, r contextRowQuerier, messageID int64) ([]byte, string, []byte, error) {
	var stored []byte
	var format, compression, metadata sql.NullString
	if err := r.QueryRowContext(ctx, `SELECT r.raw_data, r.raw_format, r.compression, CAST(m.metadata AS TEXT)
FROM message_raw r JOIN messages m ON m.id = r.message_id WHERE r.message_id = ?`, messageID).Scan(&stored, &format, &compression, &metadata); err != nil {
		return nil, "", nil, fmt.Errorf("read calendar content: %w", err)
	}
	raw, _, err := decodeMessageRawBounded(stored, compression, maxMeetingRawBytes, errMeetingRawTooLarge)
	if err != nil {
		err = fmt.Errorf("%w: %w", ErrInvalidMessageRaw, err)
	}
	return raw, format.String, []byte(metadata.String), err
}

func decodeCalendarContent(ctx context.Context, format string, raw, metadata []byte, calendarZone string) (calevent.Content, error) {
	content, err := calevent.DecodeContent(ctx, format, raw, metadata, calendarZone)
	if err != nil {
		return calevent.Content{}, fmt.Errorf("decode calendar content: %w", err)
	}
	return content, nil
}

func (s *Store) GetCalendarOccurrenceContext(ctx context.Context, ref string) (CalendarOccurrenceDetail, error) {
	ctx, release, err := s.BeginReadSnapshotContext(ctx)
	if err != nil {
		return CalendarOccurrenceDetail{}, err
	}
	defer release()
	row, original, recurring, err := s.calendarOccurrenceAnchor(ctx, ref)
	if err != nil {
		return CalendarOccurrenceDetail{}, err
	}
	var series *calendarSeries
	var occurrence CalendarOccurrence
	var cancellation *calendarCancellationEvidence
	if !recurring && row.Event.Kind == "exception" && row.Event.Original != nil {
		series, err = s.seriesMasterFor(ctx, row)
		if err != nil {
			return CalendarOccurrenceDetail{}, err
		}
		master := row
		if series != nil {
			master = series.row
		}
		effective, found, lookupErr := s.freshestCalendarExceptionAt(ctx, row, master, *row.Event.Original)
		if lookupErr != nil {
			return CalendarOccurrenceDetail{}, lookupErr
		}
		if found {
			row = effective
			occurrence, cancellation, err = series.exception(ctx, s, row)
		} else if series == nil {
			return CalendarOccurrenceDetail{}, ErrCalendarOccurrenceNotFound
		} else {
			original = *row.Event.Original
			row = series.row
			ref = calendarOccurrenceRef(row.MessageID, original.Key())
			recurring = true
		}
	} else if !recurring {
		series, err = s.seriesMasterFor(ctx, row)
		if err == nil {
			occurrence, cancellation, err = series.exception(ctx, s, row)
		}
	}
	if recurring && err == nil {
		if series == nil {
			series, err = s.calendarSeriesContext(ctx, row)
		}
		if err == nil {
			related, readErr := s.calendarExceptionsAt(ctx, row, original)
			if readErr != nil {
				return CalendarOccurrenceDetail{}, readErr
			}
			if effective, found := freshestCalendarException(row, related); found {
				row = effective
				occurrence, cancellation, err = series.exception(ctx, s, row)
			} else if series.err != nil {
				err = errors.Join(calrecur.ErrUnsupportedRecurrence, series.err)
			} else {
				var o calrecur.Occurrence
				var found bool
				o, found, err = calrecur.Resolve(ctx, series.set, series.duration, series.days, original, calrecur.SeriesReadBudget)
				if errors.Is(err, calrecur.ErrBudgetExceeded) {
					err = ErrQueryTooBroad
				} else if err == nil && !found {
					err = ErrCalendarOccurrenceNotFound
				}
				occurrence = calendarOccurrence(row, row.MessageID, ref, o)
			}
		}
	}
	if err != nil {
		return CalendarOccurrenceDetail{}, err
	}
	masterID := row.MessageID
	masterZone := row.Event.CalendarTimeZone
	if series != nil {
		masterID = series.row.MessageID
		masterZone = series.row.Event.CalendarTimeZone
	}
	row.Event.Status = occurrence.Status
	content, err := s.calendarOccurrenceContent(ctx, row, masterID, masterZone, cancellation)
	return CalendarOccurrenceDetail{CalendarOccurrence: occurrence, Content: content}, err
}

func (s *Store) calendarExceptionsAt(ctx context.Context, row CalendarEventRow, original calrecur.Identity) ([]CalendarEventRow, error) {
	identityPredicate, identityValue := "e.original_start_at = ?", any(original.Instant.UTC())
	if original.AllDay {
		identityPredicate, identityValue = "e.original_start_date = ?", original.Date
	}
	seriesID := row.Event.ProviderEventID
	if row.Event.Kind == "exception" {
		seriesID = row.Event.SeriesProviderID
	}
	return queryCalendarEventRowsWithinBudget(ctx, s.meetingReader(ctx), "e.kind = 'exception' AND m.source_id = ? AND e.series_provider_id = ? AND "+identityPredicate, []any{row.SourceID, seriesID, identityValue}, calrecur.SeriesReadBudget)
}

func freshestCalendarException(master CalendarEventRow, rows []CalendarEventRow) (CalendarEventRow, bool) {
	rows = eligibleCalendarExceptionRows(master, rows)
	if len(rows) == 0 {
		return CalendarEventRow{}, false
	}
	newest := rows[0]
	for _, row := range rows[1:] {
		if calendarEventSnapshotNewer(row, newest) {
			newest = row
		}
	}
	return newest, true
}

func (s *Store) freshestCalendarExceptionAt(ctx context.Context, row, master CalendarEventRow, original calrecur.Identity) (CalendarEventRow, bool, error) {
	rows, err := s.calendarExceptionsAt(ctx, row, original)
	if err != nil {
		return CalendarEventRow{}, false, err
	}
	effective, found := freshestCalendarException(master, rows)
	return effective, found, nil
}

func (s *Store) calendarOccurrenceContent(ctx context.Context, row CalendarEventRow, masterID int64, masterZone string, cancellation *calendarCancellationEvidence) (calevent.Content, error) {
	var err error
	if cancellation == nil && row.Event.Status == "cancelled" && row.MessageID != masterID {
		cancellation, err = readCalendarCancellationEvidence(ctx, s.db, row.MessageID)
		if err != nil {
			return calevent.Content{}, err
		}
	}
	if cancellation == nil {
		return readCalendarContent(ctx, s.db, row.MessageID, row.Event.CalendarTimeZone)
	}
	var content calevent.Content
	if cancellation.raw != nil {
		content, err = decodeCalendarContent(ctx, cancellation.format, cancellation.raw, cancellation.metadata, row.Event.CalendarTimeZone)
	}
	if err != nil || !cancellation.sparse || (content.Location != "" && content.Description != "" && content.JoinURL != "") {
		return content, err
	}
	inherited, err := readCalendarContent(ctx, s.db, masterID, masterZone)
	return calevent.Content{Location: cmp.Or(content.Location, inherited.Location), Description: cmp.Or(content.Description, inherited.Description), JoinURL: cmp.Or(content.JoinURL, inherited.JoinURL)}, err
}

func (s *Store) calendarOccurrenceAnchor(ctx context.Context, ref string) (CalendarEventRow, calrecur.Identity, bool, error) {
	invalid := func() (CalendarEventRow, calrecur.Identity, bool, error) {
		return CalendarEventRow{}, calrecur.Identity{}, false, fmt.Errorf("%w: invalid occurrence ref", ErrCalendarOccurrenceInvalid)
	}
	if !strings.HasPrefix(ref, "evt:") {
		return invalid()
	}
	anchor, key, recurring := strings.Cut(strings.TrimPrefix(ref, "evt:"), "@")
	id, err := strconv.ParseInt(anchor, 10, 64)
	if err != nil || strconv.FormatInt(id, 10) != anchor {
		return invalid()
	}
	var original calrecur.Identity
	if recurring {
		var ok bool
		original, ok = calrecur.ParseKey(key)
		if !ok {
			return invalid()
		}
	}
	predicate := "e.message_id = ? AND e.kind <> 'series'"
	if recurring {
		predicate = "e.message_id = ? AND e.kind IN ('series', 'single')"
	}
	rows, err := queryCalendarEventRows(ctx, s.meetingReader(ctx), predicate, []any{id}, 1)
	if err != nil {
		return CalendarEventRow{}, calrecur.Identity{}, false, err
	}
	if len(rows) == 0 {
		return CalendarEventRow{}, calrecur.Identity{}, false, ErrCalendarOccurrenceNotFound
	}
	row := rows[0]
	if recurring && row.Event.Kind == "single" {
		if !calendarEventStartsAtIdentity(row.Event, original) {
			return CalendarEventRow{}, calrecur.Identity{}, false, ErrCalendarOccurrenceNotFound
		}
		return row, calrecur.Identity{}, false, nil
	}
	return row, original, recurring, nil
}

func calendarEventStartsAtIdentity(event calevent.Event, original calrecur.Identity) bool {
	if event.AllDay != original.AllDay {
		return false
	}
	if original.AllDay {
		return event.StartDate == original.Date
	}
	return event.Start != nil && event.Start.Equal(original.Instant)
}

func (s *Store) seriesMasterFor(ctx context.Context, row CalendarEventRow) (*calendarSeries, error) {
	if row.Event.Kind != "exception" {
		return nil, nil //nolint:nilnil // A standalone has no series master.
	}
	masters, err := queryCalendarEventRows(ctx, s.meetingReader(ctx), "e.kind IN ('series', 'single') AND m.source_id = ? AND e.provider_event_id = ?", []any{row.SourceID, row.Event.SeriesProviderID}, 1)
	if err != nil || len(masters) == 0 {
		return nil, err
	}
	return s.calendarSeriesContext(ctx, masters[0])
}

func (s *Store) calendarSeriesContext(ctx context.Context, row CalendarEventRow) (*calendarSeries, error) {
	rows := []CalendarEventRow{row}
	remaining := calrecur.RequestReadBudget - len(rows)
	if remaining < 0 {
		return nil, calendarRecurrenceBudgetError()
	}
	if err := loadCalendarICSRecurrenceErrors(ctx, s.db, rows, &remaining); err != nil {
		return nil, err
	}
	return newCalendarSeries(rows[0]), nil
}
