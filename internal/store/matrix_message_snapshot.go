package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// PersistMatrixMessageContext commits optional same-room reply context before
// the message occurrence. A missing target never prevents the child snapshot.
func (s *Store) PersistMatrixMessageContext(ctx context.Context, data *MessagePersistData, roomID, parentID string) (int64, bool, error) {
	linked := false
	messageID, err := s.persistMessageWithParticipantsTransaction(ctx, nil, nil, func([]int64) *MessagePersistData { return data }, nil, func(ctx context.Context, tx *loggedTx, data *MessagePersistData, _ int64) error {
		if parentID == "" {
			return nil
		}
		var err error
		linked, err = setMatrixReplyWith(boundQuerier{ctx: ctx, q: tx}, data.Message.SourceID, roomID, data.Message.SourceMessageID, parentID)
		return err
	})
	return messageID, linked, err
}

// SetMatrixReplyContext resolves both native identities under the source writer
// fence. Deleted, absent and other-room targets leave optional context empty.
func (s *Store) SetMatrixReplyContext(ctx context.Context, sourceID int64, roomID, childID, parentID string) (bool, error) {
	var linked bool
	err := s.withSyncSourceWriteContext(ctx, sourceID, func(q querier) error {
		var err error
		linked, err = setMatrixReplyWith(q, sourceID, roomID, childID, parentID)
		return err
	})
	return linked, err
}

func setMatrixReplyWith(q querier, sourceID int64, roomID, childID, parentID string) (bool, error) {
	var parent sql.NullInt64
	err := q.QueryRow(`UPDATE messages SET reply_to_message_id=(
  SELECT parent.id FROM messages parent WHERE parent.source_id=messages.source_id
  AND parent.conversation_id=messages.conversation_id AND parent.source_message_id=?
  AND parent.deleted_from_source_at IS NULL
 ) WHERE source_id=? AND source_message_id=? AND deleted_from_source_at IS NULL
 AND EXISTS (SELECT 1 FROM conversations c WHERE c.id=messages.conversation_id
  AND c.source_id=messages.source_id AND c.source_conversation_id=?)
 RETURNING reply_to_message_id`, parentID, sourceID, childID, roomID).Scan(&parent)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("persist Matrix reply snapshot: %w", err)
	}
	return parent.Valid, nil
}
