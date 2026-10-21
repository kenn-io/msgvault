package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/calevent"
	"go.kenn.io/msgvault/internal/calrecur"
	"go.kenn.io/msgvault/internal/gcal/gcalmodel"
)

const calendarEventProjectionVersion = 1

// Decode failures have no provider status. Keep a marker while retaining the
// single kind so unknown bounds remain candidates for the occurrence reader.
const calendarEventDecodeFailureStatus = "decode_error"

func (s *Store) refreshCalendarEventProjectionWith(ctx context.Context, tx *loggedTx, messageID int64) error {
	return s.refreshCalendarEventProjectionModeWith(ctx, tx, messageID, false, false)
}

// refreshCalendarEventProjectionFromProviderWith records that an importer has
// observed provider evidence for this message. Evidence freshness can change
// even when the normalized event projection is unchanged.
func (s *Store) refreshCalendarEventProjectionFromProviderWith(ctx context.Context, tx *loggedTx, messageID int64) error {
	return s.refreshCalendarEventProjectionModeWith(ctx, tx, messageID, true, false)
}

func (s *Store) refreshCalendarEventProjectionFromCancellationMetadataWith(ctx context.Context, tx *loggedTx, messageID int64) error {
	return s.refreshCalendarEventProjectionModeWith(ctx, tx, messageID, true, true)
}

func (s *Store) refreshCalendarEventProjectionModeWith(ctx context.Context, tx *loggedTx, messageID int64, providerEvidence, cancellationMetadata bool) error {
	q := boundQuerier{ctx: ctx, q: tx}
	var messageType, metadata sql.NullString
	var subject sql.NullString
	var sourceID int64
	var lastModified time.Time
	err := q.QueryRow(`SELECT message_type, metadata, source_id, last_modified, subject FROM messages WHERE id = ?`, messageID).Scan(&messageType, &metadata, &sourceID, &lastModified, &subject)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read calendar projection message: %w", err)
	}
	var oldVersion int
	var oldHash string
	err = q.QueryRow(`SELECT projection_version, content_hash FROM calendar_events WHERE message_id = ?`, messageID).Scan(&oldVersion, &oldHash)
	exists := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("read calendar projection version: %w", err)
	}
	clearProjection := func() error {
		if !exists {
			return nil
		}
		_, err := q.Exec(`DELETE FROM calendar_events WHERE message_id = ?`, messageID)
		if err != nil {
			return fmt.Errorf("clear calendar projection: %w", err)
		}
		return nil
	}
	if messageType.String != "calendar_event" {
		return clearProjection()
	}
	var stored []byte
	var format, compression sql.NullString
	err = q.QueryRow(`SELECT raw_data, raw_format, compression FROM message_raw WHERE message_id = ?`, messageID).Scan(&stored, &format, &compression)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("read calendar projection raw: %w", err)
	}
	if format.String != gcalmodel.RawFormat {
		return clearProjection()
	}
	var config sql.NullString
	if err := q.QueryRow(`SELECT sync_config FROM sources WHERE id = ?`, sourceID).Scan(&config); err != nil {
		return fmt.Errorf("read calendar projection source: %w", err)
	}
	event, ok := decodeCalendarProjection(stored, compression, []byte(metadata.String), calendarConfigZone(config))
	if err := ctx.Err(); err != nil {
		return err
	}
	if !ok {
		return clearProjection()
	}
	// A cancellation alias can supply some fields and omit others. Always select
	// the freshest complete alias, then overlay only fields present in newly
	// observed provider JSON. Metadata changes and projection rebuilds have no
	// new provider fields to overlay.
	if event.Kind == "exception" && event.Status == meetingCancelledStatus && event.Original != nil {
		if eventUIDMissing(metadata) {
			uid, err := s.freshestCalendarExceptionUIDWith(ctx, tx, sourceID, event)
			if err != nil {
				return err
			}
			if uid != "" {
				updatedMetadata, changed, err := mergeCalendarEventUID(metadata, uid)
				if err != nil {
					return fmt.Errorf("preserve cancellation event iCalUID: %w", err)
				}
				if changed {
					if err := setMessageMetadataWith(q, s.dialect, messageID, sql.NullString{String: string(updatedMetadata), Valid: true}); err != nil {
						return fmt.Errorf("preserve cancellation event iCalUID: %w", err)
					}
				}
			}
		}
		latest, found, err := s.freshestCalendarExceptionDetailsWith(ctx, tx, sourceID, event)
		if err != nil {
			return err
		}
		if found {
			status, providerID, seriesID, original := event.Status, event.ProviderEventID, event.SeriesProviderID, event.Original
			rawFields := calendarRawFields(stored, compression)
			event, err = mergeCancelledCalendarException(latest.Event, event, rawFields, providerEvidence && !cancellationMetadata)
			if err != nil {
				return err
			}
			event.Status, event.ProviderEventID, event.SeriesProviderID, event.Original = status, providerID, seriesID, original
			if cancellationMetadata || rawFields["summary"] == nil && latest.Subject != "" && (!subject.Valid || subject.String == "") {
				if _, err := q.Exec(`UPDATE messages SET subject = ? WHERE id = ?`, latest.Subject, messageID); err != nil {
					return fmt.Errorf("preserve cancellation event subject: %w", err)
				}
				subject = sql.NullString{String: latest.Subject, Valid: true}
			}
		}
	}
	canonical, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("hash calendar projection: %w", err)
	}
	sum := sha256.Sum256(canonical)
	hash := hex.EncodeToString(sum[:])
	if exists && oldVersion == calendarEventProjectionVersion && oldHash == hash {
		if providerEvidence {
			if _, err := q.Exec(`UPDATE calendar_events SET snapshot_updated_at = `+s.dialect.ContentChangedNow()+` WHERE message_id = ?`, messageID); err != nil {
				return fmt.Errorf("advance calendar snapshot freshness: %w", err)
			}
		}
		return nil
	}
	recurrenceEnd := calendarRecurrenceEnd(ctx, event)
	if err := ctx.Err(); err != nil {
		return err
	}
	recurrence, err := json.Marshal(event.Recurrence)
	if err != nil {
		return fmt.Errorf("encode calendar recurrence: %w", err)
	}
	var originalAt *time.Time
	var originalDate string
	if event.Original != nil {
		if event.Original.AllDay {
			originalDate = event.Original.Date
		} else {
			originalAt = &event.Original.Instant
		}
	}
	columns := []string{"message_id", "projection_version", "content_hash", "provider_event_id", "series_provider_id", "kind", "original_start_at", "original_start_date", "status", "all_day", "start_at", "end_at", "start_date", "end_date", "event_time_zone", "calendar_time_zone", "recurrence_json", "recurrence_end_at"}
	values := []any{messageID, calendarEventProjectionVersion, hash, event.ProviderEventID, event.SeriesProviderID, event.Kind, originalAt, originalDate, event.Status, event.AllDay, event.Start, event.End, event.StartDate, event.EndDate, event.EventTimeZone, event.CalendarTimeZone, string(recurrence), recurrenceEnd}
	updates := make([]string, 0, len(columns)-1)
	for _, column := range columns[1:] {
		updates = append(updates, column+" = excluded."+column)
	}
	columns = append(columns, "snapshot_updated_at")
	var insertValues string
	if providerEvidence {
		insertValues = placeholders(len(values)) + "," + s.dialect.ContentChangedNow()
		updates = append(updates, "snapshot_updated_at = "+s.dialect.ContentChangedNow())
	} else {
		// Projection rebuilds and source-configuration refreshes change derived
		// fields without observing newer provider evidence. Keep an existing
		// watermark, or seed a newly created projection from the message's
		// archived write time until the freshness migration can do so.
		values = append(values, s.dialect.TimestampParam(lastModified))
		insertValues = placeholders(len(values))
		updates = append(updates, "snapshot_updated_at = calendar_events.snapshot_updated_at")
	}
	query := "INSERT INTO calendar_events (" + strings.Join(columns, ",") + ") VALUES (" + insertValues + ") ON CONFLICT (message_id) DO UPDATE SET " + strings.Join(updates, ",")
	if _, err := q.Exec(query, values...); err != nil {
		return fmt.Errorf("write calendar projection: %w", err)
	}
	if _, err := q.Exec(`DELETE FROM calendar_event_attendees WHERE message_id = ?`, messageID); err != nil {
		return fmt.Errorf("replace calendar attendees: %w", err)
	}
	for ordinal, attendee := range event.Attendees {
		if _, err := q.Exec(`INSERT INTO calendar_event_attendees (message_id, ordinal, email, display_name, role, rsvp, is_copy_self) VALUES (?, ?, ?, ?, ?, ?, ?)`, messageID, ordinal, attendee.Email, attendee.DisplayName, attendee.Role, attendee.RSVP, attendee.IsCopySelf); err != nil {
			return fmt.Errorf("write calendar attendee %d: %w", ordinal, err)
		}
	}
	return nil
}

// freshestCalendarExceptionDetailsWith finds the most recently archived
// readable alias before a cancellation update is recorded. The current row is
// eligible: a metadata-only cancellation leaves its previous projection in
// place until this refresh writes the cancelled version.
func (s *Store) freshestCalendarExceptionDetailsWith(ctx context.Context, tx *loggedTx, sourceID int64, event calevent.Event) (CalendarEventRow, bool, error) {
	originalColumn, originalValue := "e.original_start_at", any(event.Original.Instant)
	if event.Original.AllDay {
		originalColumn, originalValue = "e.original_start_date", event.Original.Date
	}
	predicate := `e.kind = 'exception' AND m.source_id = ? AND e.series_provider_id = ? AND ` + originalColumn + ` = ? AND
		((e.start_at IS NOT NULL AND e.end_at IS NOT NULL) OR (e.start_date <> '' AND e.end_date <> ''))`
	r := reboundReader{reader: tx.Tx, rebind: s.dialect.Rebind, dialect: s.dialect}
	query := `SELECT e.message_id ` + calendarEventFrom + `(` + predicate + `) ORDER BY e.snapshot_updated_at DESC, e.message_id DESC LIMIT 1`
	var selectedID int64
	err := r.QueryRowContext(ctx, query, sourceID, event.SeriesProviderID, originalValue).Scan(&selectedID)
	if errors.Is(err, sql.ErrNoRows) {
		return CalendarEventRow{}, false, nil
	}
	if err != nil {
		return CalendarEventRow{}, false, fmt.Errorf("find freshest calendar exception details: %w", err)
	}
	rows, err := queryCalendarEventRows(ctx, r, `e.message_id = ?`, []any{selectedID}, 1)
	if err != nil {
		return CalendarEventRow{}, false, err
	}
	if len(rows) != 1 {
		return CalendarEventRow{}, false, nil
	}
	return rows[0], true, nil
}

func (s *Store) freshestCalendarExceptionUIDWith(ctx context.Context, tx *loggedTx, sourceID int64, event calevent.Event) (string, error) {
	if event.Original == nil {
		return "", nil
	}
	originalColumn, originalValue := "e.original_start_at", any(event.Original.Instant)
	if event.Original.AllDay {
		originalColumn, originalValue = "e.original_start_date", event.Original.Date
	}
	uid := calendarUIDExpression(s.dialect, "m.metadata")
	predicate := `e.kind = 'exception' AND m.source_id = ? AND e.series_provider_id = ? AND ` + originalColumn + ` = ? AND ` + uid + ` IS NOT NULL AND ` + uid + ` <> ''`
	query := `SELECT ` + uid + ` ` + calendarEventFrom + `(` + predicate + `) ORDER BY e.snapshot_updated_at DESC, e.message_id DESC LIMIT 1`
	r := reboundReader{reader: tx.Tx, rebind: s.dialect.Rebind, dialect: s.dialect}
	var value sql.NullString
	err := r.QueryRowContext(ctx, query, sourceID, event.SeriesProviderID, originalValue).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("find freshest calendar exception iCalUID: %w", err)
	}
	return value.String, nil
}

func eventUIDMissing(metadata sql.NullString) bool {
	if !metadata.Valid || metadata.String == "" {
		return true
	}
	var fields map[string]jsontext.Value
	if err := json.Unmarshal([]byte(metadata.String), &fields); err != nil {
		return false
	}
	var value string
	if err := json.Unmarshal(fields["ical_uid"], &value); err != nil {
		return true
	}
	return value == ""
}

func mergeCalendarEventUID(metadata sql.NullString, uid string) ([]byte, bool, error) {
	if uid == "" || metadata.Valid && !eventUIDMissing(metadata) {
		return nil, false, nil
	}
	fields := make(map[string]jsontext.Value)
	if metadata.Valid && metadata.String != "" {
		if err := json.Unmarshal([]byte(metadata.String), &fields); err != nil {
			return nil, false, fmt.Errorf("decode calendar metadata for iCalUID: %w", err)
		}
	}
	if fields == nil {
		fields = make(map[string]jsontext.Value)
	}
	value, err := json.Marshal(uid)
	if err != nil {
		return nil, false, err
	}
	fields["ical_uid"] = jsontext.Value(value)
	encoded, err := json.Marshal(fields, json.Deterministic(true))
	if err != nil {
		return nil, false, err
	}
	return encoded, true, nil
}

func calendarRawFields(stored []byte, compression sql.NullString) map[string]jsontext.Value {
	raw, _, err := decodeMessageRawBounded(stored, compression, maxMeetingRawBytes, errMeetingRawTooLarge)
	if err != nil {
		return nil
	}
	var fields map[string]jsontext.Value
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil
	}
	return fields
}

func mergeCancelledCalendarException(latest, current calevent.Event, rawFields map[string]jsontext.Value, overlayProviderFields bool) (calevent.Event, error) {
	merged := latest
	merged.CalendarTimeZone = current.CalendarTimeZone
	if overlayProviderFields {
		if rawFields["start"] != nil {
			merged.AllDay = current.AllDay
			merged.Start, merged.StartDate, merged.EventTimeZone = current.Start, current.StartDate, current.EventTimeZone
		}
		if rawFields["end"] != nil {
			merged.End, merged.EndDate = current.End, current.EndDate
		}
		if rawFields["recurrence"] != nil {
			merged.Recurrence = current.Recurrence
		}
		if rawFields["attendees"] != nil {
			merged.Attendees = current.Attendees
		} else if rawFields["organizer"] != nil {
			merged.Attendees = mergeCalendarOrganizer(latest.Attendees, current.Attendees)
		}
	}
	if merged.AllDay && merged.StartDate != "" && merged.EndDate != "" {
		start, end, err := calendarAllDayBounds(merged.StartDate, merged.EndDate, merged.CalendarTimeZone)
		if err != nil {
			return calevent.Event{}, err
		}
		merged.Start, merged.End = start, end
	}
	return merged, nil
}

func mergeCalendarOrganizer(previous, current []calevent.Attendee) []calevent.Attendee {
	var organizers []calevent.Attendee
	organizerEmails := make(map[string]struct{})
	for _, attendee := range current {
		if attendee.Role == "organizer" {
			organizers = append(organizers, attendee)
			organizerEmails[attendee.Email] = struct{}{}
		}
	}
	merged := make([]calevent.Attendee, 0, len(previous)+len(organizers))
	for _, attendee := range previous {
		if attendee.Role == "organizer" {
			continue
		}
		if _, replaced := organizerEmails[attendee.Email]; replaced {
			continue
		}
		merged = append(merged, attendee)
	}
	return append(merged, organizers...)
}

func calendarAllDayBounds(startDate, endDate, zone string) (*time.Time, *time.Time, error) {
	location := time.UTC
	if zone != "" {
		loaded, err := time.LoadLocation(zone)
		if err != nil {
			return nil, nil, fmt.Errorf("calendar time zone: %w", err)
		}
		location = loaded
	}
	start, _, err := calrecur.CivilDateBoundary(startDate, location)
	if err != nil {
		return nil, nil, fmt.Errorf("calendar all-day start: %w", err)
	}
	end, _, err := calrecur.CivilDateBoundary(endDate, location)
	if err != nil {
		return nil, nil, fmt.Errorf("calendar all-day end: %w", err)
	}
	return &start, &end, nil
}

func decodeCalendarProjection(stored []byte, compression sql.NullString, metadata []byte, zone string) (calevent.Event, bool) {
	raw, _, err := decodeMessageRawBounded(stored, compression, maxMeetingRawBytes, errMeetingRawTooLarge)
	var event calevent.Event
	var ok bool
	if err == nil {
		event, ok, err = calevent.Decode(gcalmodel.RawFormat, raw, metadata, zone)
	}
	if err != nil {
		return calevent.Event{Kind: "single", Status: calendarEventDecodeFailureStatus}, true
	}
	return event, ok
}

func calendarRecurrenceEnd(ctx context.Context, event calevent.Event) *time.Time {
	if event.Kind != "series" {
		return nil
	}
	set, duration, days, err := prepareCalendarRecurrence(event)
	if err != nil {
		return nil
	}
	end, bounded := calrecur.ConservativeEnd(ctx, set, duration, days, calrecur.SeriesReadBudget)
	if !bounded {
		return nil
	}
	return &end
}

func (s *Store) refreshSourceCalendarEventsTx(ctx context.Context, tx *loggedTx, sourceID int64) error {
	var after int64
	haveCursor := false
	for {
		query := `SELECT id FROM messages WHERE source_id = ? AND message_type = 'calendar_event'`
		args := []any{sourceID}
		if haveCursor {
			query += " AND id > ?"
			args = append(args, after)
		}
		query += " ORDER BY id LIMIT 100"
		rows, err := tx.QueryContext(ctx, query, args...)
		if err != nil {
			return err
		}
		var ids []int64
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				_ = rows.Close()
				return err
			}
			ids = append(ids, id)
		}
		err = rows.Err()
		closeErr := rows.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
		if len(ids) == 0 {
			return nil
		}
		for _, id := range ids {
			if err := s.lockMeetingEvidenceWith(ctx, tx, id); err != nil {
				return err
			}
			if err := s.refreshCalendarEventProjectionWith(ctx, tx, id); err != nil {
				return err
			}
			after = id
			haveCursor = true
		}
	}
}
func calendarConfigZone(config sql.NullString) string {
	return decodeCalendarSourceConfig(config).TimeZone
}
