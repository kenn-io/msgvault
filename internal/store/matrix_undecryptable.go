package store

import (
	"context"
	"database/sql"
	"errors"
)

// MatrixUndecryptableEvent is an encrypted Matrix event waiting for a key.
// RawEvent is empty for entries migrated from state that never recorded the
// ciphertext.
type MatrixUndecryptableEvent struct {
	EventID  string
	RoomID   string
	RawEvent []byte
}

const putMatrixUndecryptableSQL = `INSERT INTO matrix_undecryptable_events
	(source_id, event_id, room_id, raw_event) VALUES (?, ?, ?, ?)
	ON CONFLICT(source_id, event_id) DO UPDATE SET
		room_id = excluded.room_id,
		raw_event = CASE WHEN length(excluded.raw_event) > 0
			THEN excluded.raw_event ELSE matrix_undecryptable_events.raw_event END`

// PutMatrixUndecryptableEvents records events as pending decryption. An empty
// RawEvent never replaces ciphertext that is already recorded.
func (s *Store) PutMatrixUndecryptableEvents(sourceID int64, events []MatrixUndecryptableEvent) error {
	if len(events) == 0 {
		return nil
	}
	if err := s.requireSyncSource(sourceID); err != nil {
		return err
	}
	return s.withSyncSourceWriteContext(context.Background(), sourceID, func(q querier) error {
		for _, evt := range events {
			raw := evt.RawEvent
			if raw == nil {
				raw = []byte{}
			}
			if _, err := q.Exec(putMatrixUndecryptableSQL, sourceID, evt.EventID, evt.RoomID, raw); err != nil {
				return err
			}
		}
		return nil
	})
}

// DeleteMatrixUndecryptableEvent removes an event from the pending set.
func (s *Store) DeleteMatrixUndecryptableEvent(sourceID int64, eventID string) error {
	if err := s.requireSyncSource(sourceID); err != nil {
		return err
	}
	return s.withSyncSourceWriteContext(context.Background(), sourceID, func(q querier) error {
		_, err := q.Exec(`DELETE FROM matrix_undecryptable_events
			WHERE source_id = ? AND event_id = ?`, sourceID, eventID)
		return err
	})
}

// MatrixUndecryptableEventsPage returns up to limit pending events with an
// event ID greater than afterEventID, in event ID order.
func (s *Store) MatrixUndecryptableEventsPage(sourceID int64, afterEventID string, limit int) ([]MatrixUndecryptableEvent, error) {
	rows, err := s.db.Query(`SELECT event_id, room_id, raw_event
		FROM matrix_undecryptable_events
		WHERE source_id = ? AND event_id > ?
		ORDER BY event_id LIMIT ?`, sourceID, afterEventID, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []MatrixUndecryptableEvent
	for rows.Next() {
		var evt MatrixUndecryptableEvent
		if err := rows.Scan(&evt.EventID, &evt.RoomID, &evt.RawEvent); err != nil {
			return nil, err
		}
		out = append(out, evt)
	}
	return out, rows.Err()
}

// MatrixUndecryptableEvent loads one pending event.
func (s *Store) MatrixUndecryptableEvent(sourceID int64, eventID string) (MatrixUndecryptableEvent, bool, error) {
	evt := MatrixUndecryptableEvent{EventID: eventID}
	err := s.db.QueryRow(`SELECT room_id, raw_event FROM matrix_undecryptable_events
		WHERE source_id = ? AND event_id = ?`, sourceID, eventID).Scan(&evt.RoomID, &evt.RawEvent)
	if errors.Is(err, sql.ErrNoRows) {
		return MatrixUndecryptableEvent{}, false, nil
	}
	return evt, err == nil, err
}
