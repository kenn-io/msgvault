package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
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
	q := boundQuerier{ctx: ctx, q: tx}
	var messageType, metadata sql.NullString
	var sourceID int64
	err := q.QueryRow(`SELECT message_type, metadata, source_id FROM messages WHERE id = ?`, messageID).Scan(&messageType, &metadata, &sourceID)
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
	canonical, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("hash calendar projection: %w", err)
	}
	sum := sha256.Sum256(canonical)
	hash := hex.EncodeToString(sum[:])
	if exists && oldVersion == calendarEventProjectionVersion && oldHash == hash {
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
	query := "INSERT INTO calendar_events (" + strings.Join(columns, ",") + ") VALUES (" + placeholders(len(columns)) + ") ON CONFLICT (message_id) DO UPDATE SET " + strings.Join(updates, ",")
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
