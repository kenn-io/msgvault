package store

import (
	"context"
	"database/sql"
	"errors"
	"slices"
)

// markConversationPolicyActivityDirtyWith runs in the metadata mutation's
// transaction. An unconditional conflict update is deliberate: it waits for a
// concurrent refresh's key deletion, then leaves a new hint after that commits.
func markConversationPolicyActivityDirtyWith(q querier, conversationID int64) error {
	if conversationID <= 0 {
		return nil
	}
	return markConversationPolicyActivitiesDirtyWith(q, []int64{conversationID})
}

func markConversationPolicyActivitiesDirtyWith(q querier, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	ids = slices.Clone(ids)
	slices.Sort(ids)
	ids = slices.Compact(ids)
	for start := 0; start < len(ids); start += 500 {
		chunk := ids[start:min(start+500, len(ids))]
		args := []any{conversationPolicyActivityDirtyPrefix}
		for _, id := range chunk {
			args = append(args, id)
		}
		_, err := q.Exec(`INSERT INTO archive_metadata(key,value)
 SELECT ? || CAST(c.id AS TEXT),'1' FROM conversations c JOIN sources src ON src.id=c.source_id
 WHERE c.id IN (`+sqlPlaceholders(len(chunk))+`) AND src.source_type IN ('beeper','slack','slackdump','discord','teams') ORDER BY c.id
 ON CONFLICT(key) DO UPDATE SET value=excluded.value`, args...)
		if err != nil {
			return err
		}
	}
	return nil
}

func markMessageConversationPolicyActivityDirtyWith(q querier, messageID int64) error {
	var room sql.NullInt64
	err := q.QueryRow(`SELECT conversation_id FROM messages WHERE id=?`, messageID).Scan(&room)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	return markConversationPolicyActivityDirtyWith(q, room.Int64)
}

// Metadata mutations only record hints. Tier changes belong to the guarded
// batch refresh, after the provider has finished roster and reply repair.
func (s *Store) withConversationPolicyActivityWriteContext(ctx context.Context, conversationID int64, write func(querier) error) error {
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		q := boundQuerier{ctx: ctx, q: tx}
		if err := s.requireSyncConversationSourceTx(q, conversationID); err != nil {
			return err
		}
		if err := write(q); err != nil {
			return err
		}
		return markConversationPolicyActivityDirtyWith(q, conversationID)
	})
}

func (s *Store) markSourceConversationPolicyActivityDirtyTx(ctx context.Context, tx *loggedTx, sourceID int64) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO archive_metadata(key,value)
 SELECT ? || CAST(c.id AS TEXT),'1' FROM conversations c JOIN sources src ON src.id=c.source_id
 WHERE c.source_id=? AND src.source_type IN ('beeper','slack','slackdump','discord','teams') ORDER BY c.id
 ON CONFLICT(key) DO UPDATE SET value=excluded.value`, conversationPolicyActivityDirtyPrefix, sourceID)
	return err
}
