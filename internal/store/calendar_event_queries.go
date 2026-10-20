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
	SnapshotUpdatedAt   time.Time
	Event               calevent.Event
}

const maxCalendarSeries = 2000
const maxCalendarExceptions = 20000
const calendarCandidateMargin = 28 * time.Hour

const calendarEventSelect = `SELECT e.message_id, m.source_id, COALESCE(m.subject, ''),
 m.deleted_from_source_at IS NOT NULL,
 %s, e.provider_event_id, e.series_provider_id, e.kind, e.original_start_at, e.original_start_date,
 e.status, e.all_day, e.start_at, e.end_at, e.start_date, e.end_date,
 e.event_time_zone, e.calendar_time_zone, e.recurrence_json, e.snapshot_updated_at
 ` + calendarEventFrom

const calendarEventFrom = `FROM calendar_events e JOIN messages m ON m.id = e.message_id
 WHERE m.deleted_at IS NULL
 AND NOT EXISTS (SELECT 1 FROM source_merge_archive_only_messages am WHERE am.message_id = m.id)
 AND `

func calendarUIDExpression(d Dialect, column string) string {
	if d.DriverName() == postgresDriverName {
		return column + " ->> 'ical_uid'"
	}
	return "CASE WHEN json_valid(" + column + ") THEN json_extract(" + column + ", '$.ical_uid') END"
}

func queryCalendarEventRowsWithinBudget(ctx context.Context, r reboundReader, predicate string, args []any, budget int) ([]CalendarEventRow, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// Public callers supply a snapshot: probe only IDs before decoding event
	// payloads or loading attendees, and stop once the budget is exceeded.
	probe := `SELECT COUNT(*) FROM (SELECT e.message_id ` + calendarEventFrom + "(" + predicate + ") LIMIT ?) AS candidates"
	probeArgs := append([]any(nil), args...)
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
	query := fmt.Sprintf(calendarEventSelect, calendarUIDExpression(r.dialect, "m.metadata")) + "(" + predicate + ") ORDER BY e.message_id"
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
		var original, start, end, snapshotUpdatedAt sql.NullTime
		var originalDate, recurrence string
		var uid sql.NullString
		err := result.Scan(&row.MessageID, &row.SourceID, &row.Subject, &row.DeletedFromSource,
			&uid, &e.ProviderEventID, &e.SeriesProviderID, &e.Kind, &original, &originalDate, &e.Status, &e.AllDay,
			&start, &end, &e.StartDate, &e.EndDate, &e.EventTimeZone, &e.CalendarTimeZone, &recurrence, &snapshotUpdatedAt)
		if err != nil {
			_ = result.Close()
			return nil, fmt.Errorf("scan calendar event: %w", err)
		}
		row.ICalUID = uid.String
		if snapshotUpdatedAt.Valid {
			row.SnapshotUpdatedAt = snapshotUpdatedAt.Time.UTC()
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

func loadCalendarAttendees(ctx context.Context, r SQLReader, events []CalendarEventRow) error {
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
	masters, err := queryCalendarSeriesWithinBudget(ctx, r, `e.kind = 'series' AND (e.start_at IS NULL OR e.start_at < ? OR e.recurrence_json LIKE '%RDATE%') AND (e.recurrence_end_at IS NULL OR e.recurrence_end_at >= ?)`, []any{upper, lower})
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
	exceptions, err := queryCalendarExceptionsInWindow(ctx, r, lower, upper, "", nil, maxCalendarExceptions)
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
		args := make([]any, end-start)
		for i := start; i < end; i++ {
			args[i-start] = exceptions[i].MessageID
		}
		predicate := `e.kind IN ('series', 'single') AND EXISTS (SELECT 1 FROM calendar_events x JOIN messages mx ON mx.id = x.message_id
   WHERE x.message_id IN (` + placeholders(len(args)) + `)
   AND x.series_provider_id = e.provider_event_id AND mx.source_id = m.source_id)`
		parents, err := queryCalendarSeriesWithinBudget(ctx, r, predicate, args)
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

func queryCalendarExceptionsInWindow(ctx context.Context, r reboundReader, lower, upper time.Time, extraPredicate string, extraArgs []any, budget int) ([]CalendarEventRow, error) {
	windowPredicate, args := calendarExceptionWindowFor(r.dialect, lower, upper, "e", "m")
	visible := func(messageAlias string) string {
		return messageAlias + `.deleted_at IS NULL AND NOT EXISTS (
   SELECT 1 FROM source_merge_archive_only_messages am WHERE am.message_id = ` + messageAlias + `.id)`
	}
	keyMatch := `newer.series_provider_id = e.series_provider_id AND (
	  ((e.series_provider_id = '' OR (e.original_start_at IS NULL AND e.original_start_date = '')) AND newer.message_id = e.message_id) OR
	  (e.series_provider_id <> '' AND (e.original_start_at IS NOT NULL OR e.original_start_date <> '') AND
	   (newer.original_start_at = e.original_start_at OR (newer.original_start_at IS NULL AND e.original_start_at IS NULL)) AND newer.original_start_date = e.original_start_date))`
	predicate := windowPredicate + ` AND NOT EXISTS (SELECT 1 FROM calendar_events newer JOIN messages nm ON nm.id = newer.message_id
	   WHERE newer.kind = 'exception' AND ` + visible("nm") + ` AND nm.source_id = m.source_id AND ` + keyMatch + `
     AND (newer.snapshot_updated_at > e.snapshot_updated_at OR
       (newer.snapshot_updated_at = e.snapshot_updated_at AND newer.message_id > e.message_id)))`
	if extraPredicate != "" {
		predicate += " AND (" + extraPredicate + ")"
		args = append(args, extraArgs...)
	}
	return queryCalendarEventRowsWithinBudget(ctx, r, predicate, args, budget)
}

func calendarExceptionWindowFor(dialect Dialect, lower, upper time.Time, eventAlias, messageAlias string) (string, []any) {
	// Calendar projections store UTC instants; SQLite compares these bounds as text.
	lower, upper = lower.UTC(), upper.UTC()
	// A shortened series may end before the window. Look up each exception's
	// own visible master to retain its full original interval without loading
	// unrelated series or charging them against the series budget.
	// Resolve source visibility by message ID so PostgreSQL can use the provider
	// index first instead of scanning the source's messages for every exception.
	timedEnd := `julianday(` + eventAlias + `.original_start_at) + (julianday(p.end_at) - julianday(p.start_at)) >= julianday(?)`
	dateEnd := `julianday(` + eventAlias + `.original_start_date) + (julianday(p.end_date) - julianday(p.start_date)) >= julianday(?)`
	if dialect.DriverName() == postgresDriverName {
		timedEnd = eventAlias + `.original_start_at + (p.end_at - p.start_at) >= ?`
		dateEnd = `NULLIF(` + eventAlias + `.original_start_date, '')::date + (NULLIF(p.end_date, '')::date - NULLIF(p.start_date, '')::date) >= ?::date`
	}
	predicate := eventAlias + `.kind = 'exception' AND ((` + eventAlias + `.start_at < ? AND ` + eventAlias + `.end_at >= ?) OR
  (` + eventAlias + `.start_at IS NULL AND ` + eventAlias + `.end_at >= ?) OR
  (` + eventAlias + `.end_at IS NULL AND ` + eventAlias + `.start_at < ?) OR
  (` + eventAlias + `.original_start_at >= ? AND ` + eventAlias + `.original_start_at < ?) OR
   (` + eventAlias + `.original_start_date >= ? AND ` + eventAlias + `.original_start_date < ?) OR
   EXISTS (SELECT 1 FROM calendar_events p
    WHERE p.kind IN ('series', 'single') AND p.provider_event_id = ` + eventAlias + `.series_provider_id
    AND (SELECT pm.source_id FROM messages pm WHERE pm.id = p.message_id AND pm.deleted_at IS NULL
     AND NOT EXISTS (SELECT 1 FROM source_merge_archive_only_messages am WHERE am.message_id = pm.id)) = ` + messageAlias + `.source_id
    AND ((` + eventAlias + `.original_start_at < ? AND ` + timedEnd + `) OR
    (` + eventAlias + `.original_start_date < ? AND ` + dateEnd + `))) OR
   (` + eventAlias + `.original_start_at IS NULL AND ` + eventAlias + `.original_start_date = ''))`
	dateLower, dateUpper := lower.Format(time.DateOnly), upper.AddDate(0, 0, 1).Format(time.DateOnly)
	args := []any{
		upper, lower, lower, upper, lower, upper, dateLower, dateUpper,
		upper, dialect.TimestampParam(lower), dateUpper, dateLower,
	}
	return predicate, args
}

// Only sparse cancellations inherit omitted details from the master.
func inheritCalendarCancellation(ctx context.Context, r SQLReader, row, master CalendarEventRow) (CalendarEventRow, error) {
	var stored []byte
	var compression sql.NullString
	if err := r.QueryRowContext(ctx, `SELECT raw_data, compression FROM message_raw WHERE message_id = ?`, row.MessageID).Scan(&stored, &compression); err != nil {
		return row, fmt.Errorf("read cancellation evidence: %w", err)
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
