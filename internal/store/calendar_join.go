package store

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
)

// CalendarJoinEvent keeps the archive location with the original Calendar JSON.
type CalendarJoinEvent struct {
	MessageID  int64
	SourceID   int64
	CalendarID string
	Raw        []byte
}

// CalendarJoinEventsContext reads synced events for one account. It enumerates
// message IDs without bodies, closes each result set, then reads raw evidence by
// primary key. Callers cache this once per import run, including unmatched events.
func (s *Store) CalendarJoinEventsContext(ctx context.Context, accountEmail string) ([]CalendarJoinEvent, error) {
	sources, err := s.GetSourcesByTypeAndAccountContext(ctx, "gcal", accountEmail)
	if err != nil {
		return nil, err
	}
	var events []CalendarJoinEvent
	skipped := 0
	for _, source := range sources {
		var cfg struct {
			CalendarID string `json:"calendar_id"`
		}
		if err := json.Unmarshal([]byte(source.SyncConfig.String), &cfg); err != nil {
			return nil, fmt.Errorf("decode calendar source config: %w", err)
		}
		rows, err := s.db.QueryContext(ctx, s.Rebind(`SELECT id, metadata FROM messages WHERE source_id = ? AND message_type = 'calendar_event' AND deleted_at IS NULL AND deleted_from_source_at IS NULL ORDER BY id`), source.ID)
		if err != nil {
			return nil, fmt.Errorf("list archived calendar events: %w", err)
		}
		var ids []int64
		for rows.Next() {
			var id int64
			var metadata sql.NullString
			if err := rows.Scan(&id, &metadata); err != nil {
				_ = rows.Close()
				return nil, fmt.Errorf("read archived calendar event ID: %w", err)
			}
			var meta struct {
				Status string `json:"status"`
			}
			if metadata.Valid {
				if err := json.Unmarshal([]byte(metadata.String), &meta); err != nil {
					skipped++
					continue
				}
			}
			if meta.Status == "cancelled" {
				continue
			}
			ids = append(ids, id)
		}
		readErr := rows.Err()
		closeErr := rows.Close()
		if err := errors.Join(readErr, closeErr); err != nil {
			return nil, fmt.Errorf("list archived calendar event IDs: %w", err)
		}
		for _, id := range ids {
			raw, err := s.GetMessageRawContext(ctx, id)
			if errors.Is(err, sql.ErrNoRows) {
				skipped++
				continue
			} // Missing raw evidence cannot support a join.
			if errors.Is(err, ErrInvalidMessageRaw) {
				skipped++
				continue
			}
			if err != nil {
				return nil, fmt.Errorf("read archived calendar event: %w", err)
			}
			events = append(events, CalendarJoinEvent{MessageID: id, SourceID: source.ID, CalendarID: cfg.CalendarID, Raw: raw})
		}
	}
	if skipped > 0 {
		return events, fmt.Errorf("skipped %d unreadable archived calendar events", skipped)
	}
	return events, nil
}
