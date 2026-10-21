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
		cancelled, _, err := s.calendarCancellationEvidenceWith(ctx, tx, messageID)
		return cancelled, err
	}
	isCompleteEvidence := func(ctx context.Context, tx *loggedTx, messageID int64) (bool, error) {
		cancelled, complete, err := s.calendarCancellationEvidenceWith(ctx, tx, messageID)
		return !cancelled || complete, err
	}
	// Seed non-cancelled rows and complete cancellation aliases before
	// reconstructing cancellation snapshots.
	// A cancellation's message ID can be older than a complete alias whose
	// provider snapshot was updated later, so ID order alone is not sufficient.
	if err := s.backfillMessageProjectionsMatchingContext(ctx, "calendar_event", "calendar_events", calendarEventProjectionVersion, isCompleteEvidence, s.refreshCalendarEventProjectionMigrationSeedWith); err != nil {
		return fmt.Errorf("backfill calendar evidence before cancellations: %w", err)
	}
	return s.backfillMessageProjectionsMatchingContext(ctx, "calendar_event", "calendar_events", calendarEventProjectionVersion, isCancellation, s.refreshCalendarEventProjectionWith)
}

func (s *Store) calendarCancellationEvidenceWith(ctx context.Context, tx *loggedTx, messageID int64) (bool, bool, error) {
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
			return false, false, nil
		}
		return false, false, fmt.Errorf("read calendar event for migration ordering: %w", err)
	}
	if format.String != gcalmodel.RawFormat {
		return false, false, nil
	}
	fields := calendarRawFields(raw, compression)
	if len(fields) == 0 {
		return false, false, nil
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
		return false, false, nil
	}
	var seriesID string
	_ = json.Unmarshal(fields["recurringEventId"], &seriesID)
	if seriesID == "" {
		return false, false, nil
	}
	var original struct {
		Date     string `json:"date"`
		DateTime string `json:"dateTime"`
	}
	_ = json.Unmarshal(fields["originalStartTime"], &original)
	if original.Date == "" && original.DateTime == "" {
		return false, false, nil
	}
	var bounds struct {
		Start struct {
			Date     string `json:"date"`
			DateTime string `json:"dateTime"`
		} `json:"start"`
		End struct {
			Date     string `json:"date"`
			DateTime string `json:"dateTime"`
		} `json:"end"`
	}
	_ = json.Unmarshal(fields["start"], &bounds.Start)
	_ = json.Unmarshal(fields["end"], &bounds.End)
	complete := bounds.Start.Date != "" && bounds.End.Date != "" || bounds.Start.DateTime != "" && bounds.End.DateTime != ""
	return true, complete, nil
}
