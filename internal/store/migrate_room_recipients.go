package store

import (
	"context"
	"fmt"
)

const migrationRoomRecipientsV1 = "room_recipients_v1"

// Candidates follow stored edges, including roster entries with no recipient row.
// Provider room addresses apply to meetings; resource flags stay event-local.
const roomRecipientEdgesSQL = `WITH candidates AS (
   SELECT m.id AS message_id, r.participant_id FROM message_recipients r
   JOIN messages m ON m.id = r.message_id
   WHERE m.message_type IN ('calendar_event','meeting_transcript')
   UNION
   SELECT id, sender_id FROM messages
   WHERE message_type IN ('calendar_event','meeting_transcript') AND sender_id IS NOT NULL
   UNION
   SELECT m.id, cp.participant_id FROM conversation_participants cp
   JOIN messages m ON m.conversation_id = cp.conversation_id
   WHERE m.message_type IN ('calendar_event','meeting_transcript')
  ), room_edges AS (
   SELECT c.message_id, c.participant_id FROM candidates c
   JOIN participants p ON p.id = c.participant_id
   WHERE lower(trim(p.email_address)) LIKE '%@resource.calendar.google.com'
      OR lower(trim(p.email_address)) LIKE '%@%.resource.calendar.google.com'
      OR EXISTS (SELECT 1 FROM calendar_event_attendees a
       WHERE a.message_id = c.message_id AND a.role = 'room'
        AND lower(trim(a.email)) = lower(trim(p.email_address)))
  ) `

func (s *Store) repairRoomRecipientsContext(ctx context.Context) error {
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		for _, query := range []string{
			roomRecipientEdgesSQL + `DELETE FROM message_recipients WHERE EXISTS (
    SELECT 1 FROM room_edges r WHERE r.message_id = message_recipients.message_id
     AND r.participant_id = message_recipients.participant_id)`,
			roomRecipientEdgesSQL + `UPDATE messages SET sender_id = NULL WHERE EXISTS (
    SELECT 1 FROM room_edges r WHERE r.message_id = messages.id AND r.participant_id = messages.sender_id)`,
			// A recurring conversation can also hold events where the flagged address
			// is a person. Keep the roster entry while one of those events uses it.
			roomRecipientEdgesSQL + `DELETE FROM conversation_participants WHERE EXISTS (
    SELECT 1 FROM room_edges r JOIN messages m ON m.id = r.message_id
    WHERE m.conversation_id = conversation_participants.conversation_id
     AND r.participant_id = conversation_participants.participant_id
   ) AND NOT EXISTS (
    SELECT 1 FROM messages m WHERE m.conversation_id = conversation_participants.conversation_id
     AND (m.sender_id = conversation_participants.participant_id OR EXISTS (
      SELECT 1 FROM message_recipients r WHERE r.message_id = m.id
       AND r.participant_id = conversation_participants.participant_id)))`,
		} {
			if _, err := tx.ExecContext(ctx, query); err != nil {
				return fmt.Errorf("remove room edges: %w", err)
			}
		}
		return nil
	})
}
