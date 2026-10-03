package store

import (
	"context"
)

// StoreMatrixEncryptedEvent retains the original m.room.encrypted event after
// decryption. The message raw keeps the decrypted event, which edits, replies,
// and repairs read; this table keeps the ciphertext the homeserver delivered.
func (s *Store) StoreMatrixEncryptedEvent(sourceID int64, eventID, roomID string, raw []byte) error {
	if err := s.requireSyncSource(sourceID); err != nil {
		return err
	}
	return s.withSyncSourceWriteContext(context.Background(), sourceID, func(q querier) error {
		_, err := q.Exec(`INSERT INTO matrix_encrypted_events
			(source_id, event_id, room_id, raw_event) VALUES (?, ?, ?, ?)
			ON CONFLICT(source_id, event_id) DO UPDATE SET
				room_id = excluded.room_id, raw_event = excluded.raw_event`,
			sourceID, eventID, roomID, raw)
		return err
	})
}

// MatrixEncryptedEvent loads the retained original encrypted event.
func (s *Store) MatrixEncryptedEvent(sourceID int64, eventID string) ([]byte, string, error) {
	var raw []byte
	var roomID string
	err := s.db.QueryRow(`SELECT raw_event, room_id FROM matrix_encrypted_events
		WHERE source_id = ? AND event_id = ?`, sourceID, eventID).Scan(&raw, &roomID)
	return raw, roomID, err
}
