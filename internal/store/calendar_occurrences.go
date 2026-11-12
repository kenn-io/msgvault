package store

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"go.kenn.io/msgvault/internal/calevent"
	"go.kenn.io/msgvault/internal/calrecur"
)

type CalendarOccurrence struct {
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
type CalendarUnexpandable struct {
	MessageID int64  `json:"message_id"`
	Reason    string `json:"reason"`
}
type CalendarOccurrencePage struct {
	Occurrences  []CalendarOccurrence   `json:"occurrences"`
	Unexpandable []CalendarUnexpandable `json:"unexpandable"`
}

// ListCalendarOccurrencesContext expands archived series in a half-open window.
func (s *Store) ListCalendarOccurrencesContext(ctx context.Context, from, to time.Time) (CalendarOccurrencePage, error) {
	page := CalendarOccurrencePage{Occurrences: []CalendarOccurrence{}, Unexpandable: []CalendarUnexpandable{}}
	ctx, release, err := s.BeginReadSnapshotContext(ctx)
	if err != nil {
		return page, err
	}
	defer release()
	rows, err := calendarEventRowsInWindow(ctx, s.db, s.dialect, from, to)
	if err != nil {
		return page, err
	}
	type seriesKey struct {
		source int64
		id     string
	}
	exceptions := map[seriesKey][]CalendarEventRow{}
	masters := map[seriesKey]bool{}
	byID := make(map[int64]CalendarEventRow, len(rows))
	for _, row := range rows {
		byID[row.MessageID] = row
		if row.Event.Kind == "series" || row.Event.Kind == "single" {
			masters[seriesKey{row.SourceID, row.Event.ProviderEventID}] = true
		}
		if row.Event.Kind == "exception" {
			key := seriesKey{row.SourceID, row.Event.SeriesProviderID}
			exceptions[key] = append(exceptions[key], row)
		}
	}
	remaining := calrecur.RequestReadBudget - len(rows)
	if remaining < 0 {
		return page, calendarRecurrenceBudgetError()
	}
	add := func(row CalendarEventRow, seriesID int64, o calrecur.Occurrence) {
		e := row.Event
		if !calendarOccurrenceOverlaps(o, from, to) {
			return
		}
		status := e.Status
		if o.Cancelled {
			status = "cancelled"
		}
		attendees := e.Attendees
		if attendees == nil {
			attendees = []calevent.Attendee{}
		}
		page.Occurrences = append(page.Occurrences, CalendarOccurrence{MessageID: row.MessageID, SeriesMessageID: seriesID, SourceID: row.SourceID, Subject: row.Subject, Start: o.Start, End: o.End, AllDay: o.AllDay, StartDate: o.StartDate, EndDate: o.EndDate, Status: status, DeletedFromSource: row.DeletedFromSource, Attendees: attendees})
	}
	addSeriesOccurrence := func(row CalendarEventRow, o calrecur.Occurrence) error {
		effective := row
		if o.HasException {
			effective = byID[o.ExceptionRef]
			if effective.Event.Status == "cancelled" {
				effective, err = inheritCalendarCancellation(ctx, s.db, effective, row)
				if err != nil {
					return err
				}
			}
		}
		if row.Event.Status == "cancelled" {
			o.Cancelled = true
		}
		add(effective, row.MessageID, o)
		return nil
	}
	reported := map[int64]bool{}
	report := func(id int64, reason string) {
		if !reported[id] {
			page.Unexpandable = append(page.Unexpandable, CalendarUnexpandable{MessageID: id, Reason: reason})
			reported[id] = true
		}
	}
	for _, row := range rows {
		if err := ctx.Err(); err != nil {
			return page, err
		}
		e := row.Event
		boundary, shift, skip := calendarRangeScope(row, masters[seriesKey{row.SourceID, e.SeriesProviderID}], exceptions[seriesKey{row.SourceID, e.ProviderEventID}], report)
		if skip {
			continue
		}
		if e.Kind == "exception" && e.Original == nil {
			report(row.MessageID, "exception has no original start")
		}
		switch e.Kind {
		case "exception", "single":
			if e.Kind == "exception" && masters[seriesKey{row.SourceID, e.SeriesProviderID}] {
				continue
			}
			switch {
			case boundary != nil && !calendarIdentityBefore(calendarEventIdentity(e), boundary):
				// The master itself lies in the omitted part of the series.
			case e.Start != nil && e.End != nil:
				add(row, 0, calrecur.Occurrence{Start: *e.Start, End: *e.End, AllDay: e.AllDay, StartDate: e.StartDate, EndDate: e.EndDate, Cancelled: e.Status == "cancelled"})
			default:
				report(row.MessageID, calendarMissingBoundsReason(e))
			}
			if e.Kind == "exception" || len(exceptions[seriesKey{row.SourceID, e.ProviderEventID}]) == 0 {
				continue
			}
			fallthrough
		case "series":
			for _, exception := range exceptions[seriesKey{row.SourceID, e.ProviderEventID}] {
				edit := exception.Event
				if edit.Original != nil && edit.Original.AllDay != e.AllDay && (edit.Start == nil || edit.End == nil) {
					report(exception.MessageID, "exception original type differs from series and event bounds are missing")
				}
			}
			occurrences, count, reason, err := expandCalendarSeries(ctx, row, exceptions[seriesKey{row.SourceID, e.ProviderEventID}], from.Add(-shift.pad), to.Add(shift.pad), remaining)
			remaining -= count
			if err != nil {
				return page, err
			}
			if reason != "" {
				report(row.MessageID, reason)
			}
			for _, o := range occurrences {
				if !calendarKeepBeforeBoundary(&o, byID, boundary, shift, report) {
					continue
				}
				if err := addSeriesOccurrence(row, o); err != nil {
					return page, err
				}
			}
		}
	}
	slices.SortFunc(page.Occurrences, func(a, b CalendarOccurrence) int {
		if c := a.Start.Compare(b.Start); c != 0 {
			return c
		}
		return cmp.Compare(a.MessageID, b.MessageID)
	})
	return page, nil
}

func calendarIdentityBefore(a, b *calrecur.Identity) bool {
	if a == nil || b == nil || a.AllDay != b.AllDay || a.Key() == "" || b.Key() == "" {
		return false
	}
	if a.AllDay {
		return a.Date < b.Date
	}
	return !a.Instant.IsZero() && a.Instant.Before(b.Instant)
}

// calendarIdentityComparable reports whether id can be ordered against identities of the given type.
func calendarIdentityComparable(id *calrecur.Identity, allDay bool) bool {
	return id != nil && id.AllDay == allDay && id.Key() != "" && (allDay || !id.Instant.IsZero())
}

func calendarEventIdentity(e calevent.Event) *calrecur.Identity {
	id := &calrecur.Identity{AllDay: e.AllDay, Date: e.StartDate}
	if e.Start != nil {
		id.Instant = *e.Start
	}
	return id
}

// calendarShift moves the occurrences a RANGE edit covers; pad is how far a moved occurrence can lie
// from where the series generates it. The zero value cannot move them.
type calendarShift struct {
	move func(*calrecur.Occurrence)
	pad  time.Duration
}

// calendarRangeScope reports unsupported RANGE edits. An edit without a master is skipped. A series
// keeps the occurrences before its earliest edit, cancels or moves later ones when every edit cancels or
// sets a new start, and omits them otherwise; it is skipped only when an edit cannot be placed.
func calendarRangeScope(row CalendarEventRow, hasMaster bool, seriesExceptions []CalendarEventRow, report func(int64, string)) (*calrecur.Identity, calendarShift, bool) {
	e := row.Event
	if e.Kind == "exception" {
		orphan := slices.Contains(e.Recurrence, calevent.ICSRangeRecurrence) && !hasMaster
		if orphan {
			report(row.MessageID, "unsupported recurrence range")
		}
		return nil, calendarShift{}, orphan
	}
	boundary, shift, id := calendarRangeBoundary(e, seriesExceptions)
	if id != 0 && shift.move == nil {
		reason := "unsupported recurrence range: later occurrences omitted"
		if boundary == nil {
			reason = "unsupported recurrence range"
		}
		report(id, reason)
	}
	return boundary, shift, id != 0 && boundary == nil
}

// calendarKeepBeforeBoundary keeps occurrences whose original start precedes or equals the range boundary,
// whatever their effective start, and moves or drops later ones; occurrences with no comparable
// original start are dropped and reported.
func calendarKeepBeforeBoundary(o *calrecur.Occurrence, byID map[int64]CalendarEventRow, boundary *calrecur.Identity, shift calendarShift, report func(int64, string)) bool {
	if boundary == nil {
		return true
	}
	original := &o.Original
	if o.HasException {
		original = byID[o.ExceptionRef].Event.Original
	}
	if !calendarIdentityComparable(original, boundary.AllDay) {
		report(o.ExceptionRef, "exception original start cannot be compared to recurrence range")
		return false
	}
	return calendarMoveLater(o, calendarIdentityBefore(boundary, original), shift, byID[o.ExceptionRef].Event.Start != nil)
}

// calendarMoveLater moves a later occurrence, or drops it when the range cannot move it. An exception
// that sets its own start keeps it, being more specific than the range; generated occurrences have no
// exception row (reference 0), so explicit is false for them.
func calendarMoveLater(o *calrecur.Occurrence, later bool, shift calendarShift, explicit bool) bool {
	if later && !explicit && shift.move != nil {
		shift.move(o)
	}
	return !later || explicit || shift.move != nil
}

func calendarRangeBoundary(master calevent.Event, exceptions []CalendarEventRow) (*calrecur.Identity, calendarShift, int64) {
	var boundary *calrecur.Identity
	var id int64
	edits := slices.DeleteFunc(slices.Clone(exceptions), func(row CalendarEventRow) bool {
		return !slices.Contains(row.Event.Recurrence, calevent.ICSRangeRecurrence)
	})
	for _, row := range edits {
		original := row.Event.Original
		if !calendarIdentityComparable(original, master.AllDay) {
			return nil, calendarShift{}, row.MessageID
		}
		if boundary == nil || calendarIdentityBefore(original, boundary) {
			boundary, id = original, row.MessageID
		}
	}
	return boundary, calendarRangeShift(master, edits), id
}

// calendarRangeShift follows each RANGE edit, in original-start order, and applies to a later occurrence
// the latest edit that does not start after it. The edits are sorted in place. One edit that cannot be
// followed leaves the whole series unfollowed.
func calendarRangeShift(master calevent.Event, edits []CalendarEventRow) calendarShift {
	zone, err := calendarEventZone(master)
	slices.SortStableFunc(edits, func(a, b CalendarEventRow) int { return cmp.Compare(a.Event.Original.Key(), b.Event.Original.Key()) })
	shifts := make([]calendarShift, len(edits))
	var pad time.Duration
	for i, row := range edits {
		shifts[i] = calendarEditShift(master, row.Event, zone)
		if row.Event.Status == "cancelled" && row.Event.Start == nil && i > 0 {
			shifts[i] = calendarCancelShift(row.Event, shifts[i-1])
		}
		if shifts[i].move == nil || err != nil {
			return calendarShift{}
		}
		pad = max(pad, shifts[i].pad)
	}
	return calendarShift{pad: pad, move: func(o *calrecur.Occurrence) {
		for i, edit := range slices.Backward(edits) {
			if edit.Event.Original.Key() <= o.Original.Key() {
				shifts[i].move(o)
				return
			}
		}
	}}
}

// calendarEditShift follows one RANGE edit: a new start moves the later occurrences by its change in the
// series zone's wall-clock time (civil days for an all-day series), and a cancellation also cancels them.
// An edit that sets neither cannot be followed.
func calendarEditShift(master, edit calevent.Event, zone *time.Location) calendarShift {
	var shift calendarShift
	switch {
	case master.AllDay && edit.StartDate != "":
		shift = calendarAllDayShift(edit, zone)
	case !master.AllDay && edit.Start != nil:
		shift = calendarTimedShift(master, edit, zone)
	}
	return calendarCancelShift(edit, shift)
}

// calendarCancelShift makes a cancellation also cancel the later occurrences its edit moves. A
// cancellation that sets no start has nothing to move, so it only cancels.
func calendarCancelShift(edit calevent.Event, shift calendarShift) calendarShift {
	if edit.Status != "cancelled" {
		return shift
	}
	move := shift.move
	shift.move = func(o *calrecur.Occurrence) {
		if move != nil {
			move(o)
		}
		o.Cancelled = true
	}
	return shift
}

// calendarTimedShift moves later occurrences so they keep the zone's local time across daylight saving
// changes, and gives them the edit's duration.
func calendarTimedShift(master, edit calevent.Event, zone *time.Location) calendarShift {
	wall := func(t time.Time) time.Time {
		t = t.In(zone)
		return time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), 0, time.UTC)
	}
	delta := wall(*edit.Start).Sub(wall(edit.Original.Instant))
	pad := max(delta, -delta)
	if edit.End != nil && master.Start != nil && master.End != nil {
		pad += max(0, edit.End.Sub(*edit.Start)-master.End.Sub(*master.Start))
	}
	return calendarShift{pad: pad, move: func(o *calrecur.Occurrence) {
		length := o.End.Sub(o.Start)
		if edit.End != nil {
			length = edit.End.Sub(*edit.Start)
		}
		w := wall(o.Start).Add(delta)
		o.Start = time.Date(w.Year(), w.Month(), w.Day(), w.Hour(), w.Minute(), w.Second(), 0, zone).UTC()
		o.End = o.Start.Add(length)
	}}
}

// calendarAllDayShift moves later all-day occurrences by the edit's change in civil days and gives them
// the edit's length when it sets an end date. An edit that keeps its start day moves them by nothing.
func calendarAllDayShift(edit calevent.Event, zone *time.Location) calendarShift {
	start, startErr := time.Parse(time.DateOnly, edit.StartDate)
	was, wasErr := time.Parse(time.DateOnly, edit.Original.Date)
	end, endErr := time.Parse(time.DateOnly, edit.EndDate)
	if startErr != nil || wasErr != nil {
		return calendarShift{}
	}
	delta, span := start.Sub(was), end.Sub(start)
	return calendarShift{pad: max(delta, -delta) + span, move: func(o *calrecur.Occurrence) {
		first, firstErr := time.Parse(time.DateOnly, o.StartDate)
		last, lastErr := time.Parse(time.DateOnly, o.EndDate)
		if !o.AllDay || firstErr != nil || lastErr != nil {
			return
		}
		if first = first.Add(delta); endErr == nil {
			last = first.Add(span)
		} else {
			last = last.Add(delta)
		}
		o.StartDate, o.EndDate = first.Format(time.DateOnly), last.Format(time.DateOnly)
		o.Start, _, _ = calrecur.CivilDateBoundary(o.StartDate, zone)
		o.End, _, _ = calrecur.CivilDateBoundary(o.EndDate, zone)
	}}
}

func calendarMissingBoundsReason(event calevent.Event) string {
	if event.Status == calendarEventDecodeFailureStatus {
		return "event data could not be decoded"
	}
	if event.Kind == "exception" {
		return "missing series master and event bounds"
	}
	return "missing event bounds"
}

// Resolve archived exceptions once, then suppress their original generated instances.
func expandCalendarSeries(ctx context.Context, row CalendarEventRow, exceptionRows []CalendarEventRow, from, to time.Time, remaining int) ([]calrecur.Occurrence, int, string, error) {
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
	// A deleted series keeps only its archived exceptions.
	if row.Event.Kind == "single" || row.Event.Status == "cancelled" {
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
	for _, row := range rows {
		e := row.Event
		if e.Start != nil && e.End != nil {
			bounded = append(bounded, calrecur.Occurrence{Start: *e.Start, End: *e.End, AllDay: e.AllDay, StartDate: e.StartDate, EndDate: e.EndDate, Cancelled: e.Status == "cancelled", ExceptionRef: row.MessageID, HasException: true})
			if e.Original == nil || set != nil && e.Original.AllDay != set.AllDay {
				resolved = append(resolved, bounded[len(bounded)-1])
			}
		}
	}
	if set == nil {
		return nil, bounded, nil
	}
	exceptions, err := calendarRecurrenceExceptions(rows)
	if err != nil {
		return nil, bounded, err
	}
	overrides := make(map[string]calrecur.Exception, len(exceptions))
	for _, exception := range exceptions {
		overrides[exception.Original.Key()] = exception
	}
	for _, exception := range overrides {
		if err := ctx.Err(); err != nil {
			return nil, bounded, err
		}
		// A retained edit's old value type cannot identify a current instance.
		// Its recorded bounds were already kept above.
		if exception.Original.AllDay != set.AllDay {
			continue
		}
		o, err := calrecur.EffectiveException(*set, exception, duration, days)
		if err != nil {
			return nil, bounded, err
		}
		resolved = append(resolved, o)
		set.ExDates = append(set.ExDates, exception.Original)
	}
	return resolved, bounded, nil
}

func calendarRecurrenceBudgetError() error {
	return fmt.Errorf("%w; long-running series count steps from their start; a narrower window may not reduce the work", ErrQueryTooBroad)
}

func calendarRecurrenceExceptions(rows []CalendarEventRow) ([]calrecur.Exception, error) {
	exceptions := make([]calrecur.Exception, 0, len(rows))
	for _, row := range rows {
		e := row.Event
		if e.Original == nil {
			continue
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
	zone, err := calevent.LoadZone(name)
	if err != nil {
		return nil, fmt.Errorf("calendar time zone: %w", err)
	}
	return zone, nil
}

// All-day dates belong to the query's civil days, independent of the calendar zone.
func calendarOccurrenceOverlaps(o calrecur.Occurrence, from, to time.Time) bool {
	if o.AllDay {
		endDate := to.Format(time.DateOnly)
		if to.Hour() != 0 || to.Minute() != 0 || to.Second() != 0 || to.Nanosecond() != 0 {
			endDate = time.Date(to.Year(), to.Month(), to.Day()+1, 0, 0, 0, 0, time.UTC).Format(time.DateOnly)
		}
		return o.StartDate < endDate && o.EndDate > from.Format(time.DateOnly)
	}
	return from.Before(to) && calrecur.Overlaps(o.Start, o.End, from, to)
}

func prepareCalendarRecurrence(e calevent.Event) (calrecur.Set, time.Duration, int, error) {
	if e.Start == nil || e.End == nil {
		return calrecur.Set{}, 0, 0, errors.New("missing event bounds")
	}
	zone, err := calendarEventZone(e)
	if err != nil {
		return calrecur.Set{}, 0, 0, err
	}
	set, err := calrecur.ParseSet(*e.Start, e.AllDay, e.StartDate, zone, e.Recurrence)
	if err != nil {
		return set, 0, 0, err
	}
	days := 0
	if e.AllDay {
		first, err := time.Parse(time.DateOnly, e.StartDate)
		if err != nil {
			return set, 0, 0, fmt.Errorf("calendar start date: %w", err)
		}
		last, err := time.Parse(time.DateOnly, e.EndDate)
		if err != nil {
			return set, 0, 0, fmt.Errorf("calendar end date: %w", err)
		}
		days = int(last.Sub(first) / (24 * time.Hour))
	}
	return set, e.End.Sub(*e.Start), days, nil
}
