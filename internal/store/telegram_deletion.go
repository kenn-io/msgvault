package store

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/gotd/td/constant"
)

// MarkTelegramCommonDeletedContext marks an explicit common-box deletion.
// Channel and supergroup IDs use a separate namespace and cannot match it.
func (s *Store) MarkTelegramCommonDeletedContext(ctx context.Context, sourceID int64, nativeID int) error {
	if err := s.requireSyncSource(sourceID); err != nil {
		return err
	}
	if nativeID <= 0 || int64(nativeID) > math.MaxInt32 {
		return errors.New("invalid Telegram common message ID")
	}
	return s.withTxLockedContext(ctx, nil, func(tx *loggedTx) error { return s.lockSyncSourceTx(ctx, tx, sourceID) }, func(tx *loggedTx) error {
		var sourceType string
		if err := tx.QueryRowContext(ctx, `SELECT source_type FROM sources WHERE id = ?`, sourceID).Scan(&sourceType); err != nil {
			return err
		}
		if sourceType != "telegram" {
			return errors.New("Telegram deletion requires a Telegram source")
		}
		rows, err := tx.QueryContext(ctx, `SELECT m.id, m.source_message_id FROM messages m
WHERE m.source_id = ? AND m.message_type = ? AND m.source_message_id LIKE ?
 AND NOT EXISTS (SELECT 1 FROM source_merge_archive_only_messages am WHERE am.message_id = m.id)`+s.dialect.SelectForUpdate(), sourceID, "telegram", fmt.Sprintf("%%:%d", nativeID))
		if err != nil {
			return fmt.Errorf("find Telegram common deletion: %w", err)
		}
		var matches []int64
		for rows.Next() {
			var messageID int64
			var key string
			if err := rows.Scan(&messageID, &key); err != nil {
				_ = rows.Close()
				return err
			}
			peer, suffix, ok := strings.Cut(key, ":")
			id, parseErr := strconv.ParseInt(peer, 10, 64)
			if !ok || parseErr != nil || strconv.FormatInt(id, 10) != peer || suffix != strconv.Itoa(nativeID) {
				_ = rows.Close()
				return errors.New("invalid Telegram message key")
			}
			marked := constant.TDLibPeerID(id)
			if marked.IsUser() || marked.IsChat() {
				matches = append(matches, messageID)
			}
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return err
		}
		if err := rows.Close(); err != nil {
			return err
		}
		if len(matches) > 1 {
			return errors.New("ambiguous Telegram common message ID; deletion was not applied")
		}
		if len(matches) == 0 {
			return nil
		}
		_, err = tx.ExecContext(ctx, fmt.Sprintf(`UPDATE messages SET deleted_from_source_at = %s WHERE id = ? AND source_id = ? AND deleted_from_source_at IS NULL`, s.dialect.Now()), matches[0], sourceID)
		return err
	})
}
