package store

import (
	"context"
	"fmt"
)

// MeetingArchivedParticipantIDsContext returns the persisted sender and attendee
// identity projection for providers that resolve people independently of emails.
func (s *Store) MeetingArchivedParticipantIDsContext(ctx context.Context, messageID int64) (int64, []int64, error) {
	var senderID int64
	if err := s.db.QueryRowContext(ctx, s.Rebind(`SELECT COALESCE(sender_id, 0) FROM messages WHERE id = ?`), messageID).Scan(&senderID); err != nil {
		return 0, nil, fmt.Errorf("load meeting sender identity: %w", err)
	}
	rows, err := s.db.QueryContext(ctx, s.Rebind(`SELECT participant_id FROM message_recipients WHERE message_id = ? AND recipient_type = 'to' ORDER BY participant_id`), messageID)
	if err != nil {
		return 0, nil, fmt.Errorf("load meeting attendee identities: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return 0, nil, fmt.Errorf("scan meeting attendee identity: %w", err)
		}
		ids = append(ids, id)
	}
	return senderID, ids, rows.Err()
}
