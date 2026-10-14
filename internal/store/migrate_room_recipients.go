package store

import "context"

const migrationRoomRecipientsV1 = "room_recipients_v1"

func (s *Store) repairRoomRecipientsContext(ctx context.Context) error {
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		// Captured envelope addresses identify an edge more precisely than the
		// participant's later aliases. Addressless mixed legacy edges are retained.
		roomParticipant := `(trim(coalesce(message_recipients.email_address,'')) <> '' AND ` + RoomAddressSQL("message_recipients.email_address") + `) OR (trim(coalesce(message_recipients.email_address,'')) = '' AND ` + participantObservedRoomOnlySQL("message_recipients.participant_id") + `)`
		result, err := tx.ExecContext(ctx, `DELETE FROM message_recipients WHERE recipient_type IN ('from','to') AND EXISTS (SELECT 1 FROM messages room_m WHERE room_m.id = message_recipients.message_id AND room_m.message_type = 'calendar_event') AND (`+roomParticipant+`)`)
		if err != nil {
			return err
		}
		removed, err := result.RowsAffected()
		if err != nil {
			return err
		}
		roomSender := `EXISTS (SELECT 1 FROM calendar_events room_event WHERE room_event.message_id=messages.id AND trim(room_event.organizer_email) <> '' AND ` + RoomAddressSQL("room_event.organizer_email") + `) OR (NOT EXISTS (SELECT 1 FROM calendar_events room_event WHERE room_event.message_id=messages.id AND trim(room_event.organizer_email) <> '') AND ` + participantObservedRoomOnlySQL("messages.sender_id") + `)`
		result, err = tx.ExecContext(ctx, `UPDATE messages SET sender_id = NULL WHERE message_type = 'calendar_event' AND sender_id IS NOT NULL AND (`+roomSender+`)`)
		if err != nil {
			return err
		}
		cleared, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if removed+cleared > 0 {
			if err := s.bumpDerivedDataRevisionContext(ctx, tx); err != nil {
				return err
			}
		}
		return s.markMigrationAppliedContext(ctx, tx, migrationRoomRecipientsV1, 1)
	})
}

func (s *Store) backfillRoomDirectoryContext(ctx context.Context) error {
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO directory_projection_dirty(person_id) SELECT id FROM persons WHERE true ON CONFLICT(person_id) DO NOTHING`); err != nil {
			return err
		}
		return s.markMigrationAppliedContext(ctx, tx, "room_directory_v1", 1)
	})
}
