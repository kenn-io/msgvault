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
	"go.kenn.io/msgvault/internal/gcal/gcalmodel"
)

var ErrQueryTooBroad = errors.New("calendar query is too broad")

type CalendarEventRow struct {
	MessageID, SourceID int64
	Subject             string
	ICalUID             string
	DeletedFromSource   bool
	Event               calevent.Event
}

const maxCalendarSeries = 2000
const maxCalendarExceptions = 20000
const calendarCandidateMargin = 28 * time.Hour

var calendarEventSelect = `SELECT e.message_id, m.source_id, COALESCE(m.subject, ''),
 m.deleted_from_source_at IS NOT NULL,
 %s, e.provider_event_id, e.series_provider_id, e.kind, e.original_start_at, e.original_start_date,
 e.status, e.all_day, e.start_at, e.end_at, e.start_date, e.end_date,
 e.event_time_zone, e.calendar_time_zone, e.recurrence_json
 `

func calendarEventFromContext(ctx context.Context, event, message string) (string, []any) {
	competingScope, args := sourceScopeClause(ctx, "om.source_id")
	scope, scopeArgs := sourceScopeClause(ctx, message+".source_id")
	return fmt.Sprintf(`FROM calendar_events %[1]s JOIN messages %[2]s ON %[2]s.id = %[1]s.message_id
 WHERE %[2]s.deleted_at IS NULL
 AND `, event, message) + calendarEventVisibility(event, message, competingScope) + scope + `
 AND `, append(args, scopeArgs...)
}

func calendarUIDExpression(d Dialect, column string) string {
	if d.DriverName() == postgresDriverName {
		return column + " ->> 'ical_uid'"
	}
	return "CASE WHEN json_valid(" + column + ") THEN json_extract(" + column + ", '$.ical_uid') END"
}

// Keep unique archived events. A live copy wins over archived copies; among
// archived copies, the lowest message ID wins within the merged source.
func calendarEventVisibility(event, message, competingScope string) string {
	return fmt.Sprintf(`NOT EXISTS (
 SELECT 1 FROM source_merge_archive_only_messages am
 WHERE am.message_id = %[2]s.id AND am.original_source_message_id <> '' AND EXISTS (
  SELECT 1 FROM calendar_events other JOIN messages om ON om.id = other.message_id
  WHERE other.provider_event_id = %[1]s.provider_event_id
  AND om.source_id = %[2]s.source_id AND om.deleted_at IS NULL %[3]s
  AND (NOT EXISTS (SELECT 1 FROM source_merge_archive_only_messages oa WHERE oa.message_id = om.id AND oa.original_source_message_id <> '')
   OR other.message_id < %[1]s.message_id)))`, event, message, competingScope)
}

func queryCalendarEventRowsWithinBudget(ctx context.Context, r reboundReader, predicate string, args []any, budget int) ([]CalendarEventRow, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	from, scopeArgs := calendarEventFromContext(ctx, "e", "m")
	// Public callers supply a snapshot: probe only IDs before decoding event
	// payloads or loading attendees, and stop once the budget is exceeded.
	probe := `SELECT COUNT(*) FROM (SELECT e.message_id ` + from + "(" + predicate + ") LIMIT ?) AS candidates"
	probeArgs := scopeArgs
	probeArgs = append(probeArgs, args...)
	probeArgs = append(probeArgs, budget+1)
	var count int
	if err := r.QueryRowContext(ctx, probe, probeArgs...).Scan(&count); err != nil {
		return nil, fmt.Errorf("probe calendar event breadth: %w", err)
	}
	if count > budget {
		return nil, fmt.Errorf("%w; narrow the window", ErrQueryTooBroad)
	}
	return queryCalendarEventRows(ctx, r, predicate, args, budget+1)
}

func queryCalendarEventRows(ctx context.Context, r reboundReader, predicate string, args []any, limit int) ([]CalendarEventRow, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	from, scopeArgs := calendarEventFromContext(ctx, "e", "m")
	query := fmt.Sprintf(calendarEventSelect,
		calendarUIDExpression(r.dialect, "m.metadata")) + from + "(" + predicate + ") ORDER BY e.message_id"
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
		var original, start, end sql.NullTime
		var originalDate, recurrence string
		var uid sql.NullString
		err := result.Scan(&row.MessageID, &row.SourceID, &row.Subject, &row.DeletedFromSource,
			&uid, &e.ProviderEventID, &e.SeriesProviderID, &e.Kind, &original, &originalDate, &e.Status, &e.AllDay,
			&start, &end, &e.StartDate, &e.EndDate, &e.EventTimeZone, &e.CalendarTimeZone, &recurrence)
		if err != nil {
			_ = result.Close()
			return nil, fmt.Errorf("scan calendar event: %w", err)
		}
		row.ICalUID = uid.String
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

func loadCalendarAttendees(ctx context.Context, r loggedReader, events []CalendarEventRow) error {
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

func calendarEventRowsInWindow(ctx context.Context, r reboundReader, from, to time.Time) ([]CalendarEventRow, error) {
	lower, upper := from.UTC().Add(-calendarCandidateMargin), to.UTC().Add(calendarCandidateMargin)
	// RDATE can precede DTSTART by any amount. Retain those masters for exact
	// expansion rather than treating their stored start as the earliest instance.
	// A cancelled master generates nothing; its archived exceptions load it below.
	masters, err := queryCalendarSeriesWithinBudget(ctx, r, `e.kind = 'series' AND e.status <> 'cancelled' AND (e.start_at IS NULL OR e.start_at < ? OR e.recurrence_json LIKE '%RDATE%') AND (e.recurrence_end_at IS NULL OR e.recurrence_end_at >= ?)`, []any{upper, lower})
	if err != nil {
		return nil, err
	}
	// Standalones count toward the request budget; cap them before materializing
	// more candidates than any occurrence reader could consider. Unknown bounds
	// remain candidates so the reader can report them.
	singles, err := queryCalendarEventRowsWithinBudget(ctx, r, `e.kind = 'single' AND ((e.start_at < ? AND e.end_at >= ?) OR (e.status <> 'cancelled' AND (e.start_at IS NULL OR e.end_at IS NULL)))`, []any{upper, lower}, calrecur.RequestReadBudget)
	if err != nil {
		return nil, err
	}
	exceptions, err := queryCalendarExceptionsInWindow(ctx, r, lower, upper)
	if err != nil {
		return nil, err
	}
	byID := make(map[int64]CalendarEventRow, len(masters)+len(singles)+len(exceptions))
	for _, rows := range [][]CalendarEventRow{masters, singles, exceptions} {
		for _, row := range rows {
			byID[row.MessageID] = row
		}
	}
	seriesCount := len(masters)
	// A moved exception brings its old master into the candidate set, restricted
	// to the same source.
	for start := 0; start < len(exceptions); start += 500 {
		end := min(start+500, len(exceptions))
		ids := make([]any, 0, end-start)
		var seriesIDs []any
		seen := make(map[string]bool, end-start)
		for _, exception := range exceptions[start:end] {
			ids = append(ids, exception.MessageID)
			if id := exception.Event.SeriesProviderID; !seen[id] {
				seen[id] = true
				seriesIDs = append(seriesIDs, id)
			}
		}
		// Name the parent IDs directly so the provider index finds the few
		// candidates even before the planner has statistics; otherwise the
		// EXISTS runs once for every archived event.
		predicate := `e.kind IN ('series', 'single') AND e.provider_event_id IN (` + placeholders(len(seriesIDs)) + `)
   AND EXISTS (SELECT 1 FROM calendar_events x JOIN messages mx ON mx.id = x.message_id
   WHERE x.message_id IN (` + placeholders(len(ids)) + `)
   AND x.series_provider_id = e.provider_event_id AND mx.source_id = m.source_id)`
		parents, err := queryCalendarSeriesWithinBudget(ctx, r, predicate, append(seriesIDs, ids...))
		if err != nil {
			return nil, err
		}
		for _, row := range parents {
			if _, exists := byID[row.MessageID]; !exists {
				byID[row.MessageID] = row
				if row.Event.Kind == "series" {
					seriesCount++
				}
				if seriesCount > maxCalendarSeries {
					return nil, calendarSeriesLimitError()
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

func calendarSeriesLimitError() error {
	return fmt.Errorf("%w: more than %d recurring series overlap the window; a narrower window will not reduce open-ended series", ErrQueryTooBroad, maxCalendarSeries)
}

func queryCalendarSeriesWithinBudget(ctx context.Context, r reboundReader, predicate string, args []any) ([]CalendarEventRow, error) {
	rows, err := queryCalendarEventRowsWithinBudget(ctx, r, predicate, args, maxCalendarSeries)
	if errors.Is(err, ErrQueryTooBroad) {
		return nil, calendarSeriesLimitError()
	}
	return rows, err
}

func queryCalendarExceptionsInWindow(ctx context.Context, r reboundReader, lower, upper time.Time) ([]CalendarEventRow, error) {
	predicate, args := calendarExceptionWindow(ctx, r.dialect, lower, upper)
	return queryCalendarEventRowsWithinBudget(ctx, r, predicate, args, maxCalendarExceptions)
}

func calendarExceptionWindow(ctx context.Context, dialect Dialect, lower, upper time.Time) (string, []any) {
	// Calendar projections store UTC instants; SQLite compares these bounds as text.
	lower, upper = lower.UTC(), upper.UTC()
	// A shortened series may end before the window. Look up each exception's
	// own visible master to retain its full original interval without loading
	// unrelated series or charging them against the series budget.
	// Apply the same source scope and copy visibility to the nested master read.
	timedEnd := `julianday(e.original_start_at) + (julianday(p.end_at) - julianday(p.start_at)) >= julianday(?)`
	dateEnd := `julianday(e.original_start_date) + (julianday(p.end_date) - julianday(p.start_date)) >= julianday(?)`
	if dialect.DriverName() == postgresDriverName {
		timedEnd = `e.original_start_at + (p.end_at - p.start_at) >= ?`
		dateEnd = `NULLIF(e.original_start_date, '')::date + (NULLIF(p.end_date, '')::date - NULLIF(p.start_date, '')::date) >= ?::date`
	}
	from, scopeArgs := calendarEventFromContext(ctx, "p", "pm")
	predicate := `e.kind = 'exception' AND ((e.start_at < ? AND e.end_at >= ?) OR
  (e.start_at IS NULL AND e.end_at >= ?) OR
  (e.end_at IS NULL AND e.start_at < ?) OR
  (e.original_start_at >= ? AND e.original_start_at < ?) OR
   (e.original_start_date >= ? AND e.original_start_date < ?) OR
   EXISTS (SELECT 1 ` + from + `p.kind IN ('series', 'single') AND p.provider_event_id = e.series_provider_id
    AND pm.source_id = m.source_id
    AND ((e.original_start_at < ? AND ` + timedEnd + `) OR
    (e.original_start_date < ? AND ` + dateEnd + `))) OR
   (e.original_start_at IS NULL AND e.original_start_date = ''))`
	dateLower, dateUpper := lower.Format(time.DateOnly), upper.AddDate(0, 0, 1).Format(time.DateOnly)
	args := []any{upper, lower, lower, upper, lower, upper, dateLower, dateUpper}
	args = append(args, scopeArgs...)
	return predicate, append(args, upper, dialect.TimestampParam(lower), dateUpper, dateLower)
}

// Only sparse cancellations inherit omitted details from the master.
func inheritCalendarCancellation(ctx context.Context, r loggedReader, row, master CalendarEventRow) (CalendarEventRow, error) {
	var stored []byte
	var compression, format sql.NullString
	if err := r.QueryRowContext(ctx, `SELECT raw_data, compression, raw_format FROM message_raw WHERE message_id = ?`, row.MessageID).Scan(&stored, &compression, &format); err != nil {
		return row, fmt.Errorf("read cancellation evidence: %w", err)
	}
	if format.String == rawFormatICS {
		// An ICS cancellation keeps its own details unless it omits the times.
		if row.Event.Start != nil {
			return row, nil
		}
		return inheritSparseCalendarCancellation(row, master, nil), nil
	}
	raw, _, err := decodeMessageRawBounded(stored, compression, maxMeetingRawBytes, errMeetingRawTooLarge)
	if err != nil {
		return row, err
	}
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		return row, fmt.Errorf("decode cancellation evidence: %w", err)
	}
	if fields["start"] != nil || fields["end"] != nil {
		return row, nil
	}
	return inheritSparseCalendarCancellation(row, master, fields), nil
}

func inheritSparseCalendarCancellation(row, master CalendarEventRow, fields map[string]any) CalendarEventRow {
	if _, supplied := fields["summary"]; !supplied {
		row.Subject = master.Subject
	}
	row.Event.Attendees = inheritSparseCalendarAttendees(row.Event.Attendees, master.Event.Attendees, fields)
	return row
}

func inheritSparseCalendarAttendees(own, master []calevent.Attendee, fields map[string]any) []calevent.Attendee {
	attendees := master
	if _, supplied := fields["attendees"]; supplied {
		attendees = own
	}
	organizers := master
	if _, supplied := fields["organizer"]; supplied || slices.ContainsFunc(own, func(a calevent.Attendee) bool { return a.Role == "organizer" }) {
		organizers = own
	}
	attendees = slices.Clone(attendees)
	for _, a := range organizers {
		if a.Role != "organizer" {
			continue
		}
		email := gcalmodel.NormalizeParticipantEmail(a.Email)
		i := slices.IndexFunc(attendees, func(guest calevent.Attendee) bool {
			return email != "" && gcalmodel.NormalizeParticipantEmail(guest.Email) == email
		})
		if i >= 0 {
			attendees[i].Role = "organizer"
		} else {
			attendees = append(attendees, a)
		}
	}
	return slices.DeleteFunc(attendees, func(a calevent.Attendee) bool {
		return a.Role == "organizer" && !slices.ContainsFunc(organizers, func(organizer calevent.Attendee) bool {
			return organizer.Role == "organizer" && gcalmodel.NormalizeParticipantEmail(organizer.Email) == gcalmodel.NormalizeParticipantEmail(a.Email)
		})
	})
}
