package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// Backfill only per-message keys; automatic links are evaluated by readers.
func (s *Store) backfillMeetingLinkKeysContext(ctx context.Context) error {
	for {
		rows, err := s.db.QueryContext(ctx, `SELECT m.id FROM messages m WHERE m.message_type = 'meeting_transcript' AND NOT EXISTS (SELECT 1 FROM meeting_link_key_state k WHERE k.message_id = m.id AND k.key_version >= ?) ORDER BY m.id LIMIT 100`, meetingLinkKeyVersion)
		if err != nil {
			return fmt.Errorf("scan meeting key backfill: %w", err)
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
			return ctx.Err()
		}
		for _, id := range ids {
			if err := s.withTxContext(ctx, func(tx *loggedTx) error {
				if err := s.lockMeetingEvidenceWith(ctx, tx, id); err != nil {
					return err
				}
				q := boundQuerier{ctx: ctx, q: tx}
				var messageType, metadata sql.NullString
				err := q.QueryRow(`SELECT message_type,metadata FROM messages WHERE id = ?`, id).Scan(&messageType, &metadata)
				if errors.Is(err, sql.ErrNoRows) {
					return nil
				}
				if err != nil {
					return err
				}
				var stored []byte
				var format, compression sql.NullString
				err = q.QueryRow(`SELECT raw_data,raw_format,compression FROM message_raw WHERE message_id = ?`, id).Scan(&stored, &format, &compression)
				if err != nil && !errors.Is(err, sql.ErrNoRows) {
					return err
				}
				raw, _, _ := decodeMessageRawBounded(stored, compression, maxMeetingRawBytes, errMeetingRawTooLarge)
				return s.refreshMeetingLinkKeysWith(ctx, tx, id, messageType.String, format.String, raw, []byte(metadata.String))
			}); err != nil {
				return fmt.Errorf("backfill meeting keys %d: %w", id, err)
			}
		}
	}
}
