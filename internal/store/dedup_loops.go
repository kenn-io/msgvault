package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
)

type duplicateLoopRow struct {
	id                     int64
	turnKey                string
	cycle                  int64
	conversationID         int64
	sourceID               int64
	kind, detector, status string
	anchor, activity, end  sql.NullInt64
	evidence               []byte
}

// retargetDuplicateLoopReferences keeps loop rows attached to the surviving
// copy before duplicate messages are hidden. When an anchor moves between
// conversations, it also resolves any live turn collision transactionally.
func (s *Store) retargetDuplicateLoopReferences(
	ctx context.Context,
	tx *loggedTx,
	survivorID int64,
	duplicateIDs []int64,
) error {
	var destinationConversationID, destinationSourceID int64
	if err := tx.QueryRowContext(ctx, `SELECT conversation_id, source_id FROM messages WHERE id = ?`, survivorID).
		Scan(&destinationConversationID, &destinationSourceID); err != nil {
		return fmt.Errorf("load duplicate survivor %d conversation: %w", survivorID, err)
	}
	remaps := make(map[int64]map[int64]int64)
	for _, duplicateID := range duplicateIDs {
		var conversationID int64
		if err := tx.QueryRowContext(ctx, `SELECT conversation_id FROM messages WHERE id = ?`, duplicateID).Scan(&conversationID); err != nil {
			return fmt.Errorf("load duplicate %d conversation: %w", duplicateID, err)
		}
		if remaps[conversationID] == nil {
			remaps[conversationID] = make(map[int64]int64)
		}
		remaps[conversationID][duplicateID] = survivorID
	}
	conversationIDs := make([]int64, 0, len(remaps))
	for id := range remaps {
		conversationIDs = append(conversationIDs, id)
	}
	sort.Slice(conversationIDs, func(i, j int) bool { return conversationIDs[i] < conversationIDs[j] })
	for _, sourceConversationID := range conversationIDs {
		rows, err := tx.QueryContext(ctx, `SELECT id, turn_key, cycle, conversation_id, source_id, kind,
			detector, status, anchor_message_id, last_activity_message_id, closed_message_id, evidence_json
			FROM open_loops WHERE conversation_id = ? ORDER BY id`, sourceConversationID)
		if err != nil {
			return fmt.Errorf("load loops for duplicate conversation %d: %w", sourceConversationID, err)
		}
		var loops []duplicateLoopRow
		for rows.Next() {
			var loop duplicateLoopRow
			if err := rows.Scan(&loop.id, &loop.turnKey, &loop.cycle, &loop.conversationID, &loop.sourceID,
				&loop.kind, &loop.detector, &loop.status, &loop.anchor, &loop.activity, &loop.end, &loop.evidence); err != nil {
				_ = rows.Close()
				return err
			}
			loops = append(loops, loop)
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			return fmt.Errorf("read loops for duplicate conversation %d: %w", sourceConversationID, err)
		}
		for _, loop := range loops {
			anchor := remapSourceMergeLoopMessageID(loop.anchor, remaps[sourceConversationID])
			activity := remapSourceMergeLoopMessageID(loop.activity, remaps[sourceConversationID])
			closedMessage := remapSourceMergeLoopMessageID(loop.end, remaps[sourceConversationID])
			anchorMoved := loop.anchor.Valid && anchor.Int64 != loop.anchor.Int64
			if sourceConversationID != destinationConversationID && !anchorMoved {
				continue
			}
			conversationID, sourceID := loop.conversationID, loop.sourceID
			if anchorMoved {
				conversationID, sourceID = destinationConversationID, destinationSourceID
				if loop.status != "closed" && loop.detector == "turn_state" && loop.conversationID != conversationID {
					if err := s.resolveSourceMergeLiveLoopCollision(ctx, tx, loop.id, conversationID); err != nil {
						return fmt.Errorf("resolve duplicate loop %d collision in conversation %d: %w", loop.id, conversationID, err)
					}
					if err := tx.QueryRowContext(ctx, `SELECT evidence_json FROM open_loops WHERE id = ?`, loop.id).Scan(&loop.evidence); err != nil {
						return fmt.Errorf("reload duplicate loop %d evidence after collision: %w", loop.id, err)
					}
				}
			}
			evidence, err := remapSourceMergeLoopEvidence(loop.evidence, sourceConversationID, conversationID, remaps[sourceConversationID])
			if err != nil {
				return fmt.Errorf("remap duplicate loop %d evidence: %w", loop.id, err)
			}
			if anchor == loop.anchor && activity == loop.activity && closedMessage == loop.end && string(evidence) == string(loop.evidence) {
				continue
			}
			turnKey, cycle := loop.turnKey, loop.cycle
			if loop.detector == "turn_state" && anchorMoved {
				turnKey = fmt.Sprintf("turn:%d:%s:%d", conversationID, loop.kind, anchor.Int64)
				if turnKey != loop.turnKey {
					var maxCycle int64
					if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(cycle), 0) FROM open_loops WHERE turn_key = ? AND id <> ?`, turnKey, loop.id).Scan(&maxCycle); err != nil {
						return fmt.Errorf("find duplicate loop %d cycle: %w", loop.id, err)
					}
					if maxCycle >= cycle {
						cycle = maxCycle + 1
					}
				}
			}
			if _, err := tx.ExecContext(ctx, `UPDATE open_loops SET turn_key = ?, cycle = ?, conversation_id = ?, source_id = ?,
				anchor_message_id = ?, last_activity_message_id = ?, closed_message_id = ?, evidence_json = ? WHERE id = ?`,
				turnKey, cycle, conversationID, sourceID, anchor, activity, closedMessage, string(evidence), loop.id); err != nil {
				return fmt.Errorf("retarget duplicate loop %d references: %w", loop.id, err)
			}
		}
	}
	return nil
}
