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

const calendarEventProjectionVersion = 2

// Decode failures have no provider status. Keep a marker while retaining the
// single kind so unknown bounds remain candidates for the occurrence reader.
const calendarEventDecodeFailureStatus = "decode_error"

func (s *Store) clearCalendarEventProjectionWith(ctx context.Context, tx *loggedTx, messageID int64) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM calendar_events WHERE message_id = ?`, messageID); err != nil {
		return fmt.Errorf("clear calendar projection: %w", err)
	}
	return nil
}

// Reuse verified invitation input instead of spending ingestion work to decode it again.
func (s *Store) persistCalendarEventProjectionWith(ctx context.Context, tx *loggedTx, messageID int64, invite *preparedICS) error {
	if invite == nil {
		return s.refreshCalendarEventProjectionWith(ctx, tx, messageID)
	}
	if invite.meta.Superseded {
		return s.clearCalendarEventProjectionWith(ctx, tx, messageID)
	}
	event := invite.component.Event
	if invite.meta.Status != "" {
		event.Status = invite.meta.Status
	}
	return s.writeCalendarEventProjectionWith(ctx, tx, messageID, event)
}

// Metadata changes leave the accepted event details intact. Reusing them avoids
// spending the shared recurrence budget to reparse unchanged evidence.
func (s *Store) refreshICSMetadataProjectionWith(ctx context.Context, tx *loggedTx, messageID int64, meta calevent.ICSMetadata) error {
	if meta.Superseded {
		return s.clearCalendarEventProjectionWith(ctx, tx, messageID)
	}
	if meta.Status != "" {
		// Invalidate the hash so a later active revision with the same event
		// details restores its status instead of skipping the projection write.
		result, err := tx.ExecContext(ctx, `UPDATE calendar_events SET status = ?, content_hash = '' WHERE message_id = ? AND projection_version = ? AND status <> ?`, meta.Status, messageID, calendarEventProjectionVersion, calendarEventDecodeFailureStatus)
		if err != nil {
			return fmt.Errorf("update ICS projection metadata: %w", err)
		}
		n, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("count updated ICS projections: %w", err)
		}
		if n > 0 {
			return nil
		}
	}
	return s.refreshCalendarEventProjectionWith(ctx, tx, messageID)
}

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
	if messageType.String != "calendar_event" {
		return s.clearCalendarEventProjectionWith(ctx, tx, messageID)
	}
	var stored []byte
	var format, compression sql.NullString
	err = q.QueryRow(`SELECT raw_data, raw_format, compression FROM message_raw WHERE message_id = ?`, messageID).Scan(&stored, &format, &compression)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("read calendar projection raw: %w", err)
	}
	if format.String != gcalmodel.RawFormat && format.String != rawFormatICS {
		return s.clearCalendarEventProjectionWith(ctx, tx, messageID)
	}
	var config sql.NullString
	if err := q.QueryRow(`SELECT sync_config FROM sources WHERE id = ?`, sourceID).Scan(&config); err != nil {
		return fmt.Errorf("read calendar projection source: %w", err)
	}
	// Rebuilding retained evidence has its own bounded verification budget.
	// Earlier ingestion work must not turn a readable event into decode_error.
	event, ok, err := decodeCalendarProjection(ctx, format.String, stored, compression, []byte(metadata.String), calendarConfigZone(config), calevent.NewICSParseBudget())
	if err != nil {
		return fmt.Errorf("decode calendar projection: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if !ok {
		return s.clearCalendarEventProjectionWith(ctx, tx, messageID)
	}
	if format.String == rawFormatICS && event.Status == calendarEventDecodeFailureStatus {
		// A rebuild cannot replace accepted typed state with damaged evidence.
		var accepted bool
		if err := q.QueryRow(`SELECT EXISTS(SELECT 1 FROM calendar_events WHERE message_id = ? AND status <> ?)`, messageID, calendarEventDecodeFailureStatus).Scan(&accepted); err != nil {
			return fmt.Errorf("read accepted calendar projection: %w", err)
		}
		if accepted {
			return nil
		}
	}
	if format.String == rawFormatICS && event.Status != calendarEventDecodeFailureStatus {
		if err := s.refreshICSMessageTimeWith(q, messageID, event); err != nil {
			return err
		}
	}
	return s.writeCalendarEventProjectionWith(ctx, tx, messageID, event)
}

func (s *Store) writeCalendarEventProjectionWith(ctx context.Context, tx *loggedTx, messageID int64, event calevent.Event) error {
	q := boundQuerier{ctx: ctx, q: tx}
	var oldVersion int
	var oldHash string
	err := q.QueryRow(`SELECT projection_version, content_hash FROM calendar_events WHERE message_id = ?`, messageID).Scan(&oldVersion, &oldHash)
	exists := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("read calendar projection version: %w", err)
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
	recurrenceEnd := calendarRecurrenceEnd(ctx, event, tx.icsParseBudget().VerificationRemaining)
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
	startWall := ""
	if event.StartWall != nil {
		startWall = event.StartWall.UTC().Format("2006-01-02T15:04:05")
	}
	columns := []string{"message_id", "projection_version", "content_hash", "provider_event_id", "series_provider_id", "kind", "original_start_at", "original_start_date", "status", "all_day", "start_at", "start_wall", "end_at", "start_date", "end_date", "event_time_zone", "calendar_time_zone", "recurrence_json", "recurrence_end_at"}
	values := []any{messageID, calendarEventProjectionVersion, hash, event.ProviderEventID, event.SeriesProviderID, event.Kind, originalAt, originalDate, event.Status, event.AllDay, event.Start, startWall, event.End, event.StartDate, event.EndDate, event.EventTimeZone, event.CalendarTimeZone, string(recurrence), recurrenceEnd}
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

func decodeCalendarProjection(ctx context.Context, format string, stored []byte, compression sql.NullString, metadata []byte, zone string, budget *calevent.ICSParseBudget) (calevent.Event, bool, error) {
	raw, _, err := decodeMessageRawBounded(stored, compression, maxMeetingRawBytes, errMeetingRawTooLarge)
	var event calevent.Event
	var ok bool
	if err == nil {
		if format == rawFormatICS {
			event, ok, err = calevent.DecodeICS(ctx, raw, metadata, zone, budget)
		} else {
			event, ok, err = calevent.Decode(format, raw, metadata, zone)
		}
	}
	if errors.Is(err, calrecur.ErrBudgetExceeded) {
		return calevent.Event{}, false, err
	}
	if err != nil {
		return calevent.Event{Kind: "single", Status: calendarEventDecodeFailureStatus}, true, nil //nolint:nilerr // Report damaged evidence per message while keeping readable events.
	}
	return event, ok, nil
}

func calendarRecurrenceEnd(ctx context.Context, event calevent.Event, remaining *int) *time.Time {
	if event.Kind != "series" || *remaining <= 0 {
		return nil
	}
	set, duration, days, err := prepareCalendarRecurrence(event)
	if err != nil {
		return nil
	}
	end, bounded, used := calrecur.ConservativeEnd(ctx, set, duration, days, min(*remaining, calrecur.SeriesReadBudget))
	*remaining -= min(used, *remaining)
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
	var value struct {
		Zone string `json:"time_zone"`
	}
	if json.Unmarshal([]byte(config.String), &value) != nil {
		return ""
	}
	return value.Zone
}

// refreshICSMessageTimeWith keeps search and conversation dates aligned with
// projections reinterpreted after a source time zone change.
func (s *Store) refreshICSMessageTimeWith(q querier, id int64, event calevent.Event) error {
	sent, err := icsEventSentAt(event)
	if err != nil {
		return err
	}
	var old sql.NullTime
	var conversationID int64
	if err := q.QueryRow(`SELECT sent_at,conversation_id FROM messages WHERE id=?`, id).Scan(&old, &conversationID); err != nil {
		return fmt.Errorf("read ICS message time: %w", err)
	}
	if old.Valid == sent.Valid && old.Time.Equal(sent.Time) {
		return nil
	}
	if _, err := q.Exec(`UPDATE messages SET sent_at=? WHERE id=?`, sent, id); err != nil {
		return fmt.Errorf("refresh ICS message time: %w", err)
	}
	return s.recomputeConversationStatsWith(q, "id = ?", conversationID)
}
