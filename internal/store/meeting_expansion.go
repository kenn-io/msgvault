package store

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"time"

	"go.kenn.io/msgvault/internal/calrecur"
)

type meetingExpansion struct {
	from, to time.Time
	page     CalendarOccurrencePage
}

// meetingExpansions holds one expansion per archived series for a graph load.
type meetingExpansions map[string]meetingExpansion

type meetingExpansionsKey struct{}

func withMeetingExpansions(ctx context.Context) context.Context {
	if ctx.Value(meetingExpansionsKey{}) != nil {
		return ctx
	}
	return context.WithValue(ctx, meetingExpansionsKey{}, meetingExpansions{})
}

// meetingSeriesGroup names the archived series a calendar row belongs to.
func meetingSeriesGroup(row CalendarEventRow) string {
	id := row.Event.ProviderEventID
	if row.Event.Kind == calendarExceptionKind {
		id = row.Event.SeriesProviderID
	}
	if id == "" {
		return fmt.Sprintf("row:%d", row.MessageID)
	}
	return fmt.Sprintf("%d:%s", row.SourceID, id)
}

// meetingSeriesWindow gathers the rows and instants requested of one series.
type meetingSeriesWindow struct {
	rows     map[int64]CalendarEventRow
	from, to time.Time
}

type meetingSeriesWindows map[string]*meetingSeriesWindow

func (w meetingSeriesWindows) add(row CalendarEventRow, at time.Time) {
	group := meetingSeriesGroup(row)
	if w[group] == nil {
		w[group] = &meetingSeriesWindow{rows: map[int64]CalendarEventRow{}, from: at, to: at.Add(time.Second)}
	}
	window := w[group]
	window.rows[row.MessageID] = row
	if at.Before(window.from) {
		window.from = at
	}
	if end := at.Add(time.Second); end.After(window.to) {
		window.to = end
	}
}

// expand expands each series once over every instant requested of it.
func (w meetingSeriesWindows) expand(ctx context.Context, s *Store, budget *int, visit func(CalendarOccurrencePage)) error {
	for _, window := range w {
		page, err := s.meetingExpandWindow(ctx, slices.Collect(maps.Values(window.rows)), window.from, window.to, budget)
		if err != nil {
			return err
		}
		visit(page)
	}
	return nil
}

// meetingExpand expands the series of rows around one instant.
func (s *Store) meetingExpand(ctx context.Context, rows []CalendarEventRow, at time.Time, budget *int) (CalendarOccurrencePage, error) {
	return s.meetingExpandWindow(ctx, rows, at, at.Add(time.Second), budget)
}

// meetingExpandWindow expands the one series that rows belong to over [from,to).
// Within a graph load, a later window the series already covers reuses the
// earlier expansion and spends no budget; an uncovered one widens it.
func (s *Store) meetingExpandWindow(ctx context.Context, rows []CalendarEventRow, from, to time.Time, budget *int) (CalendarOccurrencePage, error) {
	cache, _ := ctx.Value(meetingExpansionsKey{}).(meetingExpansions)
	group := meetingSeriesGroup(rows[0])
	if hit, ok := cache[group]; ok {
		if !from.Before(hit.from) && !to.After(hit.to) {
			return hit.page, nil
		}
		if hit.from.Before(from) {
			from = hit.from
		}
		if hit.to.After(to) {
			to = hit.to
		}
	}
	rows, err := s.meetingRelatedRows(ctx, rows, from, to, budget)
	if err != nil {
		return CalendarOccurrencePage{}, err
	}
	page, err := s.expandCalendarRows(ctx, rows, from, to, false, budget)
	if err != nil {
		return page, err
	}
	if *budget < 0 {
		return CalendarOccurrencePage{}, calendarRecurrenceBudgetError()
	}
	if cache != nil {
		cache[group] = meetingExpansion{from, to, page}
	}
	return page, nil
}

// meetingWarmExpansions expands each queued occurrence's series once, so the
// per-occurrence reads that follow reuse it instead of repeating the work.
func (s *Store) meetingWarmExpansions(ctx context.Context, queue []meetingDecisionEndpoint, budget *int) error {
	windows := meetingSeriesWindows{}
	for _, endpoint := range queue {
		ref, err := endpoint.meetingRef()
		if err != nil || ref.Kind != meetingRefEvent || endpoint.Key == "" {
			continue
		}
		rows, err := s.meetingEventRows(ctx, `e.message_id = ?`, []any{endpoint.ID}, budget)
		if err != nil {
			return err
		}
		if len(rows) == 0 {
			continue
		}
		if rows[0].Event.Kind != calendarSeriesKind {
			ref.Identity = calrecur.Identity{}
		}
		if at, err := meetingReferenceTime(rows[0], ref.Identity); err == nil && !at.IsZero() {
			windows.add(rows[0], at)
		}
	}
	return windows.expand(ctx, s, budget, func(CalendarOccurrencePage) {})
}

// meetingReferenceTime returns the instant a reference to row expands at.
func meetingReferenceTime(row CalendarEventRow, identity calrecur.Identity) (time.Time, error) {
	switch {
	case identity != (calrecur.Identity{}):
		return meetingIdentityTime(identity)
	case row.Event.Original != nil:
		return meetingIdentityTime(*row.Event.Original)
	case row.Event.Start != nil:
		return *row.Event.Start, nil
	case row.Event.End != nil:
		return *row.Event.End, nil
	}
	return time.Time{}, nil
}
