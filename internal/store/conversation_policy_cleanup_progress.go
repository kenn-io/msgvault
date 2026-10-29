package store

import (
	"context"
)

// advanceConversationPolicyCleanupSQL moves the cleanup target past anything
// already acknowledged. The target is its own counter, not the policy
// revision: a message leaving a full room needs cleanup without a revision
// change that would invalidate unrelated in-flight work.
const advanceConversationPolicyCleanupSQL = `INSERT INTO archive_metadata(key,value) VALUES ('conversation_policy_cleanup_revision','1')
 ON CONFLICT(key) DO UPDATE SET value=CAST(CAST(archive_metadata.value AS BIGINT)+1 AS TEXT)`

// seedConversationPolicyCleanup gives an archive with existing non-full rooms
// a pending cleanup target.
func (s *Store) seedConversationPolicyCleanup(ctx context.Context) error {
	return s.withConversationPolicyWrite(ctx, func(tx *loggedTx) error {
		var restricted bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM conversation_policy WHERE effective_tier<>'full')`).Scan(&restricted); err != nil || !restricted {
			return err
		}
		_, err := tx.ExecContext(ctx, advanceConversationPolicyCleanupSQL)
		return err
	})
}

// ConversationPolicyCleanupPending reads the existing target and acknowledged
// revision together. An idle runtime need not reopen native stores or scan lanes.
func (s *Store) ConversationPolicyCleanupPending(ctx context.Context) (target int64, pending bool, err error) {
	var cleaned int64
	err = s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(CASE WHEN key='conversation_policy_cleanup_revision' THEN CAST(value AS BIGINT) END),0),COALESCE(MAX(CASE WHEN key='conversation_policy_cleaned_revision' THEN CAST(value AS BIGINT) END),0) FROM archive_metadata WHERE key IN ('conversation_policy_cleanup_revision','conversation_policy_cleaned_revision')`).Scan(&target, &cleaned)
	return target, target != cleaned, err
}

// ReconcileConversationPolicyMediaProgress repairs durable scan progress and
// claim-only work even when no native backend exists. Each page owns W and
// releases it before the next page; native pruners acquire their own W later.
func (s *Store) ReconcileConversationPolicyMediaProgress(ctx context.Context) error {
	for _, visual := range []bool{false, true} {
		var after int64
		for {
			scanned := 0
			err := s.withConversationPolicyWrite(ctx, func(tx *loggedTx) error {
				table := "document_vector_generations"
				if visual {
					table = "visual_generations"
				}
				rows, err := tx.QueryContext(ctx, `SELECT id,fingerprint FROM `+table+` WHERE state<>'retired' AND id>? ORDER BY id LIMIT 500`, after)
				if err != nil {
					return err
				}
				type generation struct {
					id          int64
					fingerprint string
				}
				var selected []generation
				for rows.Next() {
					var gen generation
					if err := rows.Scan(&gen.id, &gen.fingerprint); err != nil {
						_ = rows.Close()
						return err
					}
					selected = append(selected, gen)
				}
				err = rows.Err()
				closeErr := rows.Close()
				if err != nil {
					return err
				}
				if closeErr != nil {
					return closeErr
				}
				scanned = len(selected)
				for _, gen := range selected {
					after = gen.id
					if !visual {
						if _, err := tx.ExecContext(ctx, `DELETE FROM document_vector_build_progress WHERE generation_id=?`, gen.id); err != nil {
							return err
						}
						continue
					}
					if _, err := tx.ExecContext(ctx, `UPDATE visual_generations SET source_fence=0 WHERE id=?`, gen.id); err != nil {
						return err
					}
					if err := s.reopenVisualReconciliationTx(boundQuerier{ctx: ctx, q: tx}, "visual/"+gen.fingerprint); err != nil {
						return err
					}
				}
				return nil
			})
			if err != nil {
				return err
			}
			if scanned < 500 {
				break
			}
		}
	}
	type claimCursor struct {
		VisualPolicyCleanupCursor

		Revision string
	}
	after := claimCursor{}
	for {
		scanned := 0
		err := s.withConversationPolicyWrite(ctx, func(tx *loggedTx) error {
			rows, err := tx.QueryContext(ctx, `SELECT wc.generation_id,wc.message_id,wc.blob_hash,wc.media_input_key,wc.proposed_revision
    FROM visual_work_claims wc JOIN visual_generations g ON g.id=wc.generation_id
    WHERE (wc.generation_id,wc.message_id,wc.blob_hash,wc.media_input_key,wc.proposed_revision)>(?,?,?,?,?)
    AND (NOT (`+visualPolicyOwnerCurrentSQL("wc")+`) OR NOT EXISTS(SELECT 1 FROM messages m WHERE m.id=wc.message_id AND COALESCE(CAST(m.content_changed_at AS TEXT),'')=wc.claimed_content_stamp)
    OR EXISTS(SELECT 1 FROM attachment_change_log change WHERE change.sequence>wc.source_fence AND (change.old_message_id=wc.message_id OR change.new_message_id=wc.message_id)))
    ORDER BY wc.generation_id,wc.message_id,wc.blob_hash,wc.media_input_key,wc.proposed_revision LIMIT 500`, after.GenerationID, after.Owner.MessageID, after.Owner.BlobHash, after.Owner.MediaInputKey, after.Revision)
			if err != nil {
				return err
			}
			var selected []claimCursor
			for rows.Next() {
				var key claimCursor
				if err := rows.Scan(&key.GenerationID, &key.Owner.MessageID, &key.Owner.BlobHash, &key.Owner.MediaInputKey, &key.Revision); err != nil {
					_ = rows.Close()
					return err
				}
				selected = append(selected, key)
			}
			err = rows.Err()
			closeErr := rows.Close()
			if err != nil {
				return err
			}
			if closeErr != nil {
				return closeErr
			}
			scanned = len(selected)
			for _, key := range selected {
				after = key
				if _, err := tx.ExecContext(ctx, `DELETE FROM visual_work_claims WHERE generation_id=? AND message_id=? AND blob_hash=? AND media_input_key=? AND proposed_revision=?`, key.GenerationID, key.Owner.MessageID, key.Owner.BlobHash, key.Owner.MediaInputKey, key.Revision); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			return err
		}
		if scanned < 500 {
			return nil
		}
	}
}
