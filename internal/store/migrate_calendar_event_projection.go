package store

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"

	"go.kenn.io/msgvault/internal/gcal/gcalmodel"
)

func (s *Store) backfillCalendarEventProjectionsContext(ctx context.Context) error {
	isCancellation := func(ctx context.Context, tx *loggedTx, messageID int64) (bool, error) {
		return s.calendarCancellationExceptionWith(ctx, tx, messageID)
	}
	isNotCancellation := func(ctx context.Context, tx *loggedTx, messageID int64) (bool, error) {
		cancelled, err := isCancellation(ctx, tx, messageID)
		return !cancelled, err
	}
	// Project all non-cancelled evidence before reconstructing cancellations.
	// A cancellation's message ID can be older than a complete alias whose
	// provider snapshot was updated later, so ID order alone is not sufficient.
	if err := s.backfillMessageProjectionsMatchingContext(ctx, "calendar_event", "calendar_events", calendarEventProjectionVersion, isNotCancellation, s.refreshCalendarEventProjectionWith); err != nil {
		return fmt.Errorf("backfill calendar evidence before cancellations: %w", err)
	}
	return s.backfillMessageProjectionsMatchingContext(ctx, "calendar_event", "calendar_events", calendarEventProjectionVersion, isCancellation, s.refreshCalendarEventProjectionWith)
}

func (s *Store) calendarCancellationExceptionWith(ctx context.Context, tx *loggedTx, messageID int64) (bool, error) {
	q := boundQuerier{ctx: ctx, q: tx}
	var metadata sql.NullString
	var raw []byte
	var format, compression sql.NullString
	err := q.QueryRow(`
		SELECT m.metadata, r.raw_data, r.raw_format, r.compression
		FROM messages m
		LEFT JOIN message_raw r ON r.message_id = m.id
		WHERE m.id = ? AND m.message_type = 'calendar_event'`, messageID).Scan(&metadata, &raw, &format, &compression)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("read calendar event for migration ordering: %w", err)
	}
	if format.String != gcalmodel.RawFormat {
		return false, nil
	}
	fields := calendarRawFields(raw, compression)
	if len(fields) == 0 {
		return false, nil
	}
	var status string
	if err := json.Unmarshal(fields["status"], &status); err != nil {
		status = ""
	}
	if metadata.Valid {
		var values struct {
			Status string `json:"status"`
		}
		if err := json.Unmarshal([]byte(metadata.String), &values); err == nil && values.Status != "" {
			status = values.Status
		}
	}
	if status != meetingCancelledStatus {
		return false, nil
	}
	var seriesID string
	_ = json.Unmarshal(fields["recurringEventId"], &seriesID)
	if seriesID == "" {
		return false, nil
	}
	var original struct {
		Date     string `json:"date"`
		DateTime string `json:"dateTime"`
	}
	_ = json.Unmarshal(fields["originalStartTime"], &original)
	return original.Date != "" || original.DateTime != "", nil
}
