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
		if e.Kind == "exception" && e.Original == nil {
			report(row.MessageID, "exception has no original start")
		}
		switch e.Kind {
		case "exception", "single":
			if e.Kind == "exception" && masters[seriesKey{row.SourceID, e.SeriesProviderID}] {
				continue
			}
			if e.Start != nil && e.End != nil {
				add(row, 0, calrecur.Occurrence{Start: *e.Start, End: *e.End, AllDay: e.AllDay, StartDate: e.StartDate, EndDate: e.EndDate, Cancelled: e.Status == "cancelled"})
			} else {
				report(row.MessageID, calendarMissingBoundsReason(e))
			}
			if e.Kind == "exception" || len(exceptions[seriesKey{row.SourceID, e.ProviderEventID}]) == 0 {
				continue
			}
			fallthrough
		case "series":
			occurrences, count, reason, err := expandCalendarSeries(ctx, row, exceptions[seriesKey{row.SourceID, e.ProviderEventID}], from, to, remaining)
			remaining -= count
			if err != nil {
				return page, err
			}
			if reason != "" {
				report(row.MessageID, reason)
			}
			for _, o := range occurrences {
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
		if exception.Original.AllDay != set.AllDay && exception.Start != nil && exception.End != nil {
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
		// An omitted event zone inherits the master's zone during expansion.
		var zone *time.Location
		if e.EventTimeZone != "" {
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
func calendarOccurrenceOverlaps(o calrecur.Occurrence, from, to time.Time) bool {
	if o.AllDay {
		endDate := to.Format(time.DateOnly)
		if to.Hour() != 0 || to.Minute() != 0 || to.Second() != 0 || to.Nanosecond() != 0 {
			endDate = time.Date(to.Year(), to.Month(), to.Day()+1, 0, 0, 0, 0, time.UTC).Format(time.DateOnly)
		}
		return o.StartDate < endDate && o.EndDate > from.Format(time.DateOnly)
	}
	return from.Before(to) && o.Start.Before(to) && o.End.After(from)
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
