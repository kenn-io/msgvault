package store

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"slices"
	"time"

	"go.kenn.io/msgvault/internal/calevent"
	"go.kenn.io/msgvault/internal/calrecur"
)

var ErrQueryTooBroad = errors.New("calendar query is too broad; narrow the window or sources")

type SourceScope struct {
	All bool
	IDs []int64
}

func OwnerScope() SourceScope { return SourceScope{All: true} }
func (sc SourceScope) Allows(sourceID int64) bool {
	return sc.All || (sourceID > 0 && slices.Contains(sc.IDs, sourceID))
}

type CalendarEventRow struct {
	MessageID, SourceID int64
	Subject             string
	DeletedFromSource   bool
	Event               calevent.Event
	RecurrenceEndAt     *time.Time
	RecurrenceError     string
}

const maxCalendarSeries = 2000
const maxCalendarExceptions = 20000
const calendarCandidateMargin = 28 * time.Hour

const calendarEventSelect = `SELECT e.message_id, m.source_id, COALESCE(m.subject, ''),
 m.deleted_from_source_at IS NOT NULL,
 e.provider, e.namespace, e.provider_event_id, e.series_provider_id, e.kind,
 e.original_start_at, e.original_start_date, e.ical_uid, e.sequence, e.status,
 e.has_bounds, e.all_day, e.start_at, e.end_at, e.start_date, e.end_date,
 e.event_time_zone, e.calendar_time_zone, e.recurrence_json, e.recurrence_end_at,
 e.recurrence_error, e.join_url, e.join_key, e.organizer_email, e.calendar_id,
 e.transparency, e.visibility, e.event_type, e.location, e.copy_self_rsvp,
 e.description, e.description_truncated
 ` + calendarEventFrom

const calendarEventFrom = `FROM calendar_events e JOIN messages m ON m.id = e.message_id
 WHERE m.deleted_at IS NULL AND `

func calendarScopeSQL(scope SourceScope) (string, []any) {
	if scope.All {
		return "1 = 1", nil
	}
	ids := slices.DeleteFunc(normalizedSignedIDs(scope.IDs), func(id int64) bool { return id <= 0 })
	if len(ids) == 0 {
		return "1 = 0", nil
	}
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	return "m.source_id IN (" + placeholders(len(ids)) + ")", args
}

func queryCalendarEventRowsWithinBudget(ctx context.Context, r rowReader, scope SourceScope, predicate string, args []any, budget int) ([]CalendarEventRow, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// Public callers supply a snapshot: probe only IDs before decoding event
	// payloads or loading attendees, and stop once the budget is exceeded.
	scoped, scopeArgs := calendarScopeSQL(scope)
	probe := `SELECT COUNT(*) FROM (SELECT e.message_id ` + calendarEventFrom + scoped + " AND (" + predicate + ") LIMIT ?) AS candidates"
	probeArgs := scopeArgs
	probeArgs = append(probeArgs, args...)
	probeArgs = append(probeArgs, budget+1)
	var count int
	if err := r.QueryRowContext(ctx, probe, probeArgs...).Scan(&count); err != nil {
		return nil, fmt.Errorf("probe calendar event breadth: %w", err)
	}
	if count > budget {
		return nil, ErrQueryTooBroad
	}
	rows, err := queryCalendarEventRows(ctx, r, scope, predicate, args, budget+1)
	if err != nil {
		return nil, err
	}
	if len(rows) > budget {
		return nil, ErrQueryTooBroad
	}
	return rows, nil
}

func queryCalendarEventRows(ctx context.Context, r rowReader, scope SourceScope, predicate string, args []any, limit int) ([]CalendarEventRow, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !scope.All && len(scope.IDs) == 0 {
		return nil, nil
	}
	scoped, scopeArgs := calendarScopeSQL(scope)
	query := calendarEventSelect + scoped + " AND (" + predicate + ") ORDER BY e.message_id"
	args = append(scopeArgs, args...)
	if limit > 0 {
		query += " LIMIT ?"
		args = append(args, limit)
	}
	result, err := r.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query calendar events: %w", err)
	}
	defer func() { _ = result.Close() }()
	var out []CalendarEventRow
	for result.Next() {
		var row CalendarEventRow
		e := &row.Event
		var original, start, end, recurrenceEnd sql.NullTime
		var originalDate, recurrence string
		err := result.Scan(&row.MessageID, &row.SourceID, &row.Subject, &row.DeletedFromSource,
			&e.Provider, &e.Namespace, &e.ProviderEventID, &e.SeriesProviderID, &e.Kind,
			&original, &originalDate, &e.ICalUID, &e.Sequence, &e.Status, &e.HasBounds, &e.AllDay,
			&start, &end, &e.StartDate, &e.EndDate, &e.EventTimeZone, &e.CalendarTimeZone,
			&recurrence, &recurrenceEnd, &row.RecurrenceError, &e.JoinURL, &e.JoinKey, &e.OrganizerEmail,
			&e.CalendarID, &e.Transparency, &e.Visibility, &e.EventType, &e.Location, &e.CopySelfRSVP,
			&e.Description, &e.DescriptionTruncated)
		if err != nil {
			_ = result.Close()
			return nil, fmt.Errorf("scan calendar event: %w", err)
		}
		if originalDate != "" {
			e.Original = &calrecur.Identity{Date: originalDate, AllDay: true}
		} else if original.Valid {
			e.Original = &calrecur.Identity{Instant: original.Time.UTC()}
		}
		if start.Valid {
			value := start.Time.UTC()
			e.Start = &value
		}
		if end.Valid {
			value := end.Time.UTC()
			e.End = &value
		}
		if recurrenceEnd.Valid {
			value := recurrenceEnd.Time.UTC()
			row.RecurrenceEndAt = &value
		}
		if err := json.Unmarshal([]byte(recurrence), &e.Recurrence); err != nil {
			_ = result.Close()
			return nil, fmt.Errorf("decode calendar recurrence %d: %w", row.MessageID, err)
		}
		out = append(out, row)
	}
	iterationErr, closeErr := result.Err(), result.Close()
	if iterationErr != nil {
		return nil, fmt.Errorf("iterate calendar events: %w", iterationErr)
	}
	if closeErr != nil {
		return nil, fmt.Errorf("close calendar events: %w", closeErr)
	}
	if err := loadCalendarAttendees(ctx, r, out); err != nil {
		return nil, err
	}
	return out, nil
}

func loadCalendarAttendees(ctx context.Context, r rowReader, events []CalendarEventRow) error {
	byID := make(map[int64]int, len(events))
	for i, event := range events {
		byID[event.MessageID] = i
	}
	for start := 0; start < len(events); start += 500 {
		end := min(start+500, len(events))
		args := make([]any, end-start)
		for i := start; i < end; i++ {
			args[i-start] = events[i].MessageID
		}
		rows, err := r.QueryContext(ctx, `SELECT message_id, email, display_name, role, rsvp, is_copy_self FROM calendar_event_attendees WHERE message_id IN (`+placeholders(len(args))+`) ORDER BY message_id, ordinal`, args...)
		if err != nil {
			return fmt.Errorf("read calendar attendees: %w", err)
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var id int64
			var attendee calevent.Attendee
			if err := rows.Scan(&id, &attendee.Email, &attendee.DisplayName, &attendee.Role, &attendee.RSVP, &attendee.IsCopySelf); err != nil {
				_ = rows.Close()
				return fmt.Errorf("scan calendar attendee: %w", err)
			}
			i := byID[id]
			events[i].Event.Attendees = append(events[i].Event.Attendees, attendee)
		}
		iterationErr, closeErr := rows.Err(), rows.Close()
		if iterationErr != nil {
			return fmt.Errorf("iterate calendar attendees: %w", iterationErr)
		}
		if closeErr != nil {
			return fmt.Errorf("close calendar attendees: %w", closeErr)
		}
	}
	return nil
}

func calendarEventRowsByMessageIDs(ctx context.Context, r rowReader, scope SourceScope, ids []int64) ([]CalendarEventRow, error) {
	ids = normalizedSignedIDs(ids)
	var out []CalendarEventRow
	for start := 0; start < len(ids); start += 500 {
		end := min(start+500, len(ids))
		args := make([]any, end-start)
		for i, id := range ids[start:end] {
			args[i] = id
		}
		rows, err := queryCalendarEventRows(ctx, r, scope, "e.message_id IN ("+placeholders(len(args))+")", args, 0)
		if err != nil {
			return nil, err
		}
		out = append(out, rows...)
	}
	return out, nil
}

func calendarEventRowsByProviderID(ctx context.Context, r rowReader, scope SourceScope, provider, namespace, id string) ([]CalendarEventRow, error) {
	if id == "" {
		return nil, nil
	}
	predicate := "e.provider = ? AND e.provider_event_id = ?"
	args := []any{provider, id}
	if namespace != "" {
		predicate += " AND e.namespace = ?"
		args = append(args, namespace)
	}
	return queryCalendarEventRows(ctx, r, scope, predicate, args, 0)
}
func calendarEventRowsBySeriesProviderID(ctx context.Context, r rowReader, scope SourceScope, provider, namespace, id string) ([]CalendarEventRow, error) {
	if id == "" {
		return nil, nil
	}
	predicate := "e.provider = ? AND e.series_provider_id = ?"
	args := []any{provider, id}
	if namespace != "" {
		predicate += " AND e.namespace = ?"
		args = append(args, namespace)
	}
	return queryCalendarEventRows(ctx, r, scope, predicate, args, 0)
}
func calendarEventRowsByICalUID(ctx context.Context, r rowReader, scope SourceScope, uid string) ([]CalendarEventRow, error) {
	if uid == "" {
		return nil, nil
	}
	return queryCalendarEventRows(ctx, r, scope, "e.ical_uid = ?", []any{uid}, 0)
}
func calendarEventRowsByJoinKey(ctx context.Context, r rowReader, scope SourceScope, key string) ([]CalendarEventRow, error) {
	if key == "" {
		return nil, nil
	}
	return queryCalendarEventRows(ctx, r, scope, "e.join_key = ?", []any{key}, 0)
}

func currentCalendarZone(ctx context.Context, r rowReader, sourceID int64) (*time.Location, error) {
	var config sql.NullString
	err := r.QueryRowContext(ctx, `SELECT sync_config FROM sources WHERE id = ?`, sourceID).Scan(&config)
	if errors.Is(err, sql.ErrNoRows) {
		return time.UTC, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read calendar time zone: %w", err)
	}
	var sc struct {
		TimeZone string `json:"time_zone"`
	}
	if json.Unmarshal([]byte(config.String), &sc) == nil && sc.TimeZone != "" {
		if zone, err := time.LoadLocation(sc.TimeZone); err == nil {
			return zone, nil
		}
	}
	return time.UTC, nil
}

// calendarEventRowsInWindow returns the full candidate closure or a breadth
// error. Exact all-day overlap uses currentCalendarZone in the caller's snapshot.
func calendarEventRowsInWindow(ctx context.Context, r rowReader, scope SourceScope, from, to time.Time) ([]CalendarEventRow, error) {
	lower, upper := from.UTC().Add(-calendarCandidateMargin), to.UTC().Add(calendarCandidateMargin)
	// RDATE can precede DTSTART by any amount. Retain those masters for exact
	// expansion rather than treating their stored start as the earliest instance.
	masters, err := queryCalendarEventRows(ctx, r, scope, `e.kind = 'series' AND (e.start_at IS NULL OR e.start_at < ? OR e.recurrence_json LIKE '%RDATE%') AND (e.recurrence_end_at IS NULL OR e.recurrence_end_at >= ?)`, []any{upper, lower}, maxCalendarSeries+1)
	if err != nil {
		return nil, err
	}
	if len(masters) > maxCalendarSeries {
		return nil, ErrQueryTooBroad
	}
	// Standalones count toward the request budget; cap them before materializing
	// more candidates than any occurrence reader could consider.
	singles, err := queryCalendarEventRowsWithinBudget(ctx, r, scope, `e.kind = 'single' AND e.start_at < ? AND e.end_at >= ?`, []any{upper, lower}, calrecur.RequestReadBudget)
	if err != nil {
		return nil, err
	}
	predicate := `e.kind = 'exception' AND ((e.start_at < ? AND e.end_at >= ?) OR
  (e.has_bounds = FALSE AND ((e.original_start_at >= ? AND e.original_start_at < ?) OR
   (e.original_start_date >= ? AND e.original_start_date < ?))))`
	exceptions, err := queryCalendarEventRows(ctx, r, scope, predicate, []any{upper, lower, lower, upper, lower.Format(time.DateOnly), upper.AddDate(0, 0, 1).Format(time.DateOnly)}, maxCalendarExceptions+1)
	if err != nil {
		return nil, err
	}
	if len(exceptions) > maxCalendarExceptions {
		return nil, ErrQueryTooBroad
	}
	byID := make(map[int64]CalendarEventRow, len(masters)+len(singles)+len(exceptions))
	for _, rows := range [][]CalendarEventRow{masters, singles, exceptions} {
		for _, row := range rows {
			byID[row.MessageID] = row
		}
	}
	seriesCount := len(masters)
	// A moved exception brings its old master into the candidate set, restricted
	// to the same provider, namespace and source.
	for start := 0; start < len(exceptions); start += 500 {
		end := min(start+500, len(exceptions))
		args := make([]any, end-start)
		for i := start; i < end; i++ {
			args[i-start] = exceptions[i].MessageID
		}
		predicate := `e.kind = 'series' AND EXISTS (SELECT 1 FROM calendar_events x JOIN messages mx ON mx.id = x.message_id
   WHERE x.message_id IN (` + placeholders(len(args)) + `) AND x.provider = e.provider AND x.namespace = e.namespace
   AND x.series_provider_id = e.provider_event_id AND mx.source_id = m.source_id)`
		parents, err := queryCalendarEventRows(ctx, r, scope, predicate, args, maxCalendarSeries+1)
		if err != nil {
			return nil, err
		}
		for _, row := range parents {
			if _, exists := byID[row.MessageID]; !exists {
				byID[row.MessageID] = row
				masters = append(masters, row)
				seriesCount++
				if seriesCount > maxCalendarSeries {
					return nil, ErrQueryTooBroad
				}
			}
		}
	}
	exceptionCount := len(exceptions)
	for start := 0; start < len(masters); start += 500 {
		end := min(start+500, len(masters))
		args := make([]any, end-start)
		for i := start; i < end; i++ {
			args[i-start] = masters[i].MessageID
		}
		predicate := `e.kind = 'exception' AND EXISTS (SELECT 1 FROM calendar_events x JOIN messages mx ON mx.id = x.message_id
   WHERE x.message_id IN (` + placeholders(len(args)) + `) AND x.provider = e.provider AND x.namespace = e.namespace
   AND x.provider_event_id = e.series_provider_id AND mx.source_id = m.source_id)`
		children, err := queryCalendarEventRows(ctx, r, scope, predicate, args, maxCalendarExceptions+1)
		if err != nil {
			return nil, err
		}
		for _, row := range children {
			if _, exists := byID[row.MessageID]; !exists {
				byID[row.MessageID] = row
				exceptionCount++
				if exceptionCount > maxCalendarExceptions {
					return nil, ErrQueryTooBroad
				}
			}
		}
	}
	out := make([]CalendarEventRow, 0, len(byID))
	for _, row := range byID {
		out = append(out, row)
	}
	slices.SortFunc(out, func(a, b CalendarEventRow) int { return cmp.Compare(a.MessageID, b.MessageID) })
	return out, nil
}
