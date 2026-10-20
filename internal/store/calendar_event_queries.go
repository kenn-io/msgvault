package store

import (
	"bytes"
	"cmp"
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"slices"
	"time"

	ical "github.com/emersion/go-ical"

	"go.kenn.io/msgvault/internal/calevent"
	"go.kenn.io/msgvault/internal/calrecur"
	"go.kenn.io/msgvault/internal/gcal/gcalmodel"
)

var ErrQueryTooBroad = errors.New("calendar query is too broad")

type CalendarEventRow struct {
	MessageID, SourceID int64
	Subject             string
	rawFormat           string
	evidenceError       string
	DeletedFromSource   bool
	Event               calevent.Event
}

type calendarSeriesKey struct {
	source int64
	id     string
}

const maxCalendarSeries = 2000
const maxCalendarExceptions = 20000
const calendarCandidateMargin = 28 * time.Hour

const calendarEventSelect = `SELECT e.message_id, m.source_id, COALESCE(m.subject, ''),
	m.deleted_from_source_at IS NOT NULL, COALESCE(r.raw_format,''), COALESCE(CAST(m.metadata AS TEXT),''),
 e.provider_event_id, e.series_provider_id, e.kind, e.original_start_at, e.original_start_date,
 e.status, e.all_day, e.start_at, e.end_at, e.start_date, e.end_date,
 e.event_time_zone, e.calendar_time_zone, e.recurrence_json, e.start_wall
 ` + calendarEventFrom

const calendarEventFrom = `FROM calendar_events e JOIN messages m ON m.id = e.message_id
 LEFT JOIN message_raw r ON r.message_id = m.id
 WHERE m.deleted_at IS NULL
 AND NOT EXISTS (SELECT 1 FROM source_merge_archive_only_messages am WHERE am.message_id = m.id)
 AND `

func queryCalendarEventRowsWithinBudget(ctx context.Context, r *loggedDB, predicate string, args []any, budget int) ([]CalendarEventRow, error) {
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

func queryCalendarEventRows(ctx context.Context, r *loggedDB, predicate string, args []any, limit int) ([]CalendarEventRow, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	query := calendarEventSelect + "(" + predicate + ") ORDER BY e.message_id"
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
		var originalDate, recurrence, startWall, messageMetadata string
		err := result.Scan(&row.MessageID, &row.SourceID, &row.Subject, &row.DeletedFromSource, &row.rawFormat,
			&messageMetadata,
			&e.ProviderEventID, &e.SeriesProviderID, &e.Kind, &original, &originalDate, &e.Status, &e.AllDay,
			&start, &end, &e.StartDate, &e.EndDate, &e.EventTimeZone, &e.CalendarTimeZone, &recurrence, &startWall)
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
		if startWall != "" {
			value, err := time.Parse("2006-01-02T15:04:05", startWall)
			if err != nil {
				_ = result.Close()
				return nil, fmt.Errorf("decode calendar wall DTSTART %d: %w", row.MessageID, err)
			}
			e.StartWall = &value
		}
		if row.rawFormat == rawFormatICS {
			if _, err := calevent.ParseICSMetadata([]byte(messageMetadata)); err != nil {
				row.evidenceError = "event data could not be decoded"
			}
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

func queryICSSparseCancellationTombstones(ctx context.Context, r *loggedDB, events []CalendarEventRow) (map[calendarSeriesKey]bool, error) {
	bySource := make(map[int64][]string)
	seen := make(map[calendarSeriesKey]bool)
	for _, row := range events {
		if row.rawFormat != rawFormatICS || row.Event.Kind != "exception" || row.Event.SeriesProviderID == "" {
			continue
		}
		key := calendarSeriesKey{source: row.SourceID, id: row.Event.SeriesProviderID}
		if !seen[key] {
			seen[key] = true
			bySource[row.SourceID] = append(bySource[row.SourceID], row.Event.SeriesProviderID)
		}
	}
	tombstones := make(map[calendarSeriesKey]bool)
	for sourceID, ids := range bySource {
		for start := 0; start < len(ids); start += 500 {
			end := min(start+500, len(ids))
			args := make([]any, 0, end-start+1)
			args = append(args, sourceID)
			for _, id := range ids[start:end] {
				args = append(args, id)
			}
			// Typed rows retain cancellation evidence even when message metadata
			// is damaged. The event reader reports that damage per message.
			query := `SELECT e.provider_event_id
				FROM calendar_events e JOIN messages m ON m.id=e.message_id
				JOIN message_raw raw ON raw.message_id=m.id
				WHERE m.source_id=? AND raw.raw_format='ics' AND e.kind IN ('single', 'series')
			  AND e.status='cancelled'
				  AND m.deleted_at IS NULL AND m.deleted_from_source_at IS NULL
				  AND NOT EXISTS (SELECT 1 FROM source_merge_archive_only_messages am WHERE am.message_id=m.id)
				  AND e.provider_event_id IN (` + placeholders(end-start) + `)`
			rows, err := r.QueryContext(ctx, query, args...)
			if err != nil {
				return nil, fmt.Errorf("read sparse ICS cancellation tombstones: %w", err)
			}
			for rows.Next() {
				var id string
				if err := rows.Scan(&id); err != nil {
					_ = rows.Close()
					return nil, fmt.Errorf("scan sparse ICS cancellation tombstone: %w", err)
				}
				tombstones[calendarSeriesKey{source: sourceID, id: id}] = true
			}
			iterationErr, closeErr := rows.Err(), rows.Close()
			if iterationErr != nil {
				return nil, fmt.Errorf("iterate sparse ICS cancellation tombstones: %w", iterationErr)
			}
			if closeErr != nil {
				return nil, fmt.Errorf("close sparse ICS cancellation tombstones: %w", closeErr)
			}
		}
	}
	return tombstones, nil
}

func loadCalendarICSRecurrenceErrors(ctx context.Context, r *loggedDB, events []CalendarEventRow, remaining *int) error {
	var ids []any
	for _, row := range events {
		if row.Event.Kind == "series" && row.rawFormat == rawFormatICS {
			ids = append(ids, row.MessageID)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	diagnostics, err := calendarICSRecurrenceErrors(ctx, r, ids, remaining)
	if errors.Is(err, calrecur.ErrBudgetExceeded) {
		return calendarRecurrenceBudgetError()
	}
	if err != nil {
		return err
	}
	for i := range events {
		if events[i].Event.Kind != "series" || events[i].rawFormat != rawFormatICS {
			continue
		}
		events[i].Event.RecurrenceError = diagnostics[events[i].MessageID]
	}
	return nil
}

func calendarICSRecurrenceErrors(ctx context.Context, r *loggedDB, ids []any, remaining *int) (map[int64]string, error) {
	rows, err := r.QueryContext(ctx, `SELECT m.id,r.raw_data,r.compression,m.metadata,s.sync_config
 FROM message_raw r JOIN messages m ON m.id=r.message_id JOIN sources s ON s.id=m.source_id
 WHERE r.raw_format='ics' AND m.id IN (`+placeholders(len(ids))+`)`, ids...)
	if err != nil {
		return nil, fmt.Errorf("read ICS recurrence evidence: %w", err)
	}
	defer func() { _ = rows.Close() }()
	diagnostics := make(map[int64]string, len(ids))
	for rows.Next() {
		var id int64
		var stored []byte
		var metadata, compression, config sql.NullString
		if err := rows.Scan(&id, &stored, &compression, &metadata, &config); err != nil {
			return nil, fmt.Errorf("scan ICS recurrence evidence: %w", err)
		}
		event, _, err := decodeICSRecurrenceEvidence(ctx, stored, compression, metadata.String, config, remaining)
		if errors.Is(err, calrecur.ErrBudgetExceeded) {
			return nil, err
		}
		if err != nil {
			diagnostics[id] = err.Error()
		} else {
			diagnostics[id] = event.RecurrenceError
		}
	}
	return diagnostics, errors.Join(rows.Err(), rows.Close())
}

func loadCalendarAttendees(ctx context.Context, r *loggedDB, events []CalendarEventRow) error {
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

func calendarEventRowsInWindow(ctx context.Context, r *loggedDB, dialect Dialect, from, to time.Time) ([]CalendarEventRow, error) {
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
	exceptions, err := queryCalendarExceptionsInWindow(ctx, r, dialect, lower, upper)
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

func queryCalendarSeriesWithinBudget(ctx context.Context, r *loggedDB, predicate string, args []any) ([]CalendarEventRow, error) {
	rows, err := queryCalendarEventRowsWithinBudget(ctx, r, predicate, args, maxCalendarSeries)
	if errors.Is(err, ErrQueryTooBroad) {
		return nil, calendarSeriesLimitError()
	}
	return rows, err
}

func queryCalendarExceptionsInWindow(ctx context.Context, r *loggedDB, dialect Dialect, lower, upper time.Time) ([]CalendarEventRow, error) {
	// A shortened series may end before the window. Look up each exception's
	// own visible master to retain its full original interval without loading
	// unrelated series or charging them against the series budget.
	// Resolve source visibility by message ID so PostgreSQL can use the provider
	// index first instead of scanning the source's messages for every exception.
	timedEnd := `julianday(e.original_start_at) + (julianday(p.end_at) - julianday(p.start_at)) >= julianday(?)`
	dateEnd := `julianday(e.original_start_date) + (julianday(p.end_date) - julianday(p.start_date)) >= julianday(?)`
	if dialect.DriverName() == postgresDriverName {
		timedEnd = `e.original_start_at + (p.end_at - p.start_at) >= ?`
		dateEnd = `NULLIF(e.original_start_date, '')::date + (NULLIF(p.end_date, '')::date - NULLIF(p.start_date, '')::date) >= ?::date`
	}
	predicate := `e.kind = 'exception' AND ((e.start_at < ? AND e.end_at >= ?) OR
  (e.start_at IS NULL AND e.end_at >= ?) OR
  (e.end_at IS NULL AND e.start_at < ?) OR
  (e.original_start_at >= ? AND e.original_start_at < ?) OR
   (e.original_start_date >= ? AND e.original_start_date < ?) OR
   EXISTS (SELECT 1 FROM calendar_events p
    WHERE p.kind IN ('series', 'single') AND p.provider_event_id = e.series_provider_id
    AND (SELECT pm.source_id FROM messages pm WHERE pm.id = p.message_id AND pm.deleted_at IS NULL
     AND NOT EXISTS (SELECT 1 FROM source_merge_archive_only_messages am WHERE am.message_id = pm.id)) = m.source_id
    AND ((e.original_start_at < ? AND ` + timedEnd + `) OR
    (e.original_start_date < ? AND ` + dateEnd + `))) OR
   (e.original_start_at IS NULL AND e.original_start_date = ''))`
	dateLower, dateUpper := lower.Format(time.DateOnly), upper.AddDate(0, 0, 1).Format(time.DateOnly)
	return queryCalendarEventRowsWithinBudget(ctx, r, predicate, []any{
		upper, lower, lower, upper, lower, upper, dateLower, dateUpper,
		upper, dialect.TimestampParam(lower), dateUpper, dateLower,
	}, maxCalendarExceptions)
}

// Only sparse cancellations inherit omitted details from the master. Evidence
// decode failures are diagnostics; archive reads and context errors abort.
func inheritCalendarCancellation(ctx context.Context, r *loggedDB, row, master CalendarEventRow) (CalendarEventRow, string, error) {
	if err := ctx.Err(); err != nil {
		return row, "", err
	}
	if row.Event.Start != nil && row.Event.End != nil {
		return row, "", nil
	}
	var stored []byte
	var compression sql.NullString
	var format string
	if err := r.QueryRowContext(ctx, `SELECT raw_data, compression, raw_format FROM message_raw WHERE message_id = ?`, row.MessageID).Scan(&stored, &compression, &format); err != nil {
		return row, "", fmt.Errorf("read cancellation evidence: %w", err)
	}
	raw, _, err := decodeMessageRawBounded(stored, compression, maxMeetingRawBytes, errMeetingRawTooLarge)
	if err != nil {
		return row, fmt.Sprintf("decode cancellation evidence: %s", err), nil
	}
	var fields map[string]any
	switch format {
	case rawFormatICS:
		cal, err := ical.NewDecoder(bytes.NewReader(raw)).Decode()
		if err != nil {
			return row, fmt.Sprintf("decode ICS cancellation evidence: %s", err), nil
		}
		fields = make(map[string]any)
		for _, child := range cal.Children {
			if child.Name != ical.CompEvent {
				continue
			}
			for name, key := range map[string]string{"SUMMARY": "summary", "ATTENDEE": "attendees", "ORGANIZER": "organizer", "DTSTART": "start", "DTEND": "end"} {
				if child.Props.Get(name) != nil {
					fields[key] = true
				}
			}
		}
	case "gcal_json":
		if err := json.Unmarshal(raw, &fields); err != nil {
			return row, fmt.Sprintf("decode cancellation evidence: %s", err), nil
		}
	default:
		return row, fmt.Sprintf("unsupported cancellation raw format %q", format), nil
	}
	if fields["start"] != nil || fields["end"] != nil {
		return row, "", nil
	}
	return inheritSparseCalendarCancellation(row, master, fields), "", nil
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
