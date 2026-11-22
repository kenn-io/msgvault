package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
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

// scheduleConversationPolicyMoveCleanupWith starts derived-data cleanup when a
// message leaves a full room for one that is not full. Neither room's tier
// changes, so no transition schedules it, and the moved text would otherwise
// stay in the vectors of its old contextual window.
func scheduleConversationPolicyMoveCleanupWith(q querier, from sql.NullInt64, to int64) error {
	if !from.Valid || from.Int64 == to {
		return nil
	}
	full := `EXISTS(SELECT 1 FROM conversations c WHERE c.id=? AND ` + FullConversationWhere("c") + `)`
	var leftFull bool
	if err := q.QueryRow(`SELECT `+full+` AND NOT `+full, from.Int64, to).Scan(&leftFull); err != nil {
		return fmt.Errorf("check moved message eligibility: %w", err)
	}
	if !leftFull {
		return nil
	}
	// Contextual vectors of the old room's messages can carry the moved text.
	// Revoke their stamps now so search stops accepting them before cleanup
	// runs. Windows are day-scoped, but the whole room is cheap to cover and
	// a move out of a full room is rare.
	if _, err := q.Exec(`UPDATE messages SET embed_gen=NULL, embedding_revision=embedding_revision+1
 WHERE conversation_id=? AND message_type='beeper' AND `+FullMessageWhere("messages"), from.Int64); err != nil {
		return fmt.Errorf("invalidate moved message context: %w", err)
	}
	// Advance the policy revision too. Cleanup resets discovery watermarks and
	// cursors; without a new revision, an in-flight worker could overwrite
	// that reset with progress from before the move and skip the siblings
	// revoked above until the daily backstop.
	if _, err := q.Exec(`INSERT INTO archive_metadata(key,value) VALUES (?,'1')
 ON CONFLICT(key) DO UPDATE SET value=CAST(CAST(archive_metadata.value AS BIGINT)+1 AS TEXT)`, conversationPolicyRevisionKey); err != nil {
		return fmt.Errorf("advance policy revision for moved message: %w", err)
	}
	if _, err := q.Exec(advanceConversationPolicyCleanupSQL); err != nil {
		return fmt.Errorf("schedule cleanup for moved message: %w", err)
	}
	return nil
}
