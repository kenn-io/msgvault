package store

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strings"
	"time"
)

type sourceMergeLoop struct {
	id                                                      int64
	conversationID                                          int64
	anchorMessageID, lastActivityMessageID, closedMessageID sql.NullInt64
	kind                                                    string
	detector                                                string
	status                                                  string
	lastActivityAt                                          time.Time
	updatedAt                                               time.Time
	evidenceJSON                                            string
	evidence                                                []map[string]jsontext.Value
}

type sourceMergeLoopGroup struct {
	conversationID int64
	kind           string
}

func transferSourceMergeLoops(
	ctx context.Context,
	tx *loggedTx,
	from, into int64,
	conversations map[int64]int64,
	messageSurvivors map[int64]int64,
	nowExpr string,
) error {
	rows, err := tx.QueryContext(ctx, `SELECT id, conversation_id,
		anchor_message_id, last_activity_message_id, closed_message_id,
		kind, detector, status, last_activity_at, updated_at, CAST(evidence_json AS TEXT)
		FROM open_loops WHERE source_id = ? ORDER BY id`, from)
	if err != nil {
		return fmt.Errorf("read source loop state: %w", err)
	}
	var sourceLoops []sourceMergeLoop
	for rows.Next() {
		var loop sourceMergeLoop
		if err := rows.Scan(&loop.id, &loop.conversationID,
			&loop.anchorMessageID, &loop.lastActivityMessageID, &loop.closedMessageID,
			&loop.kind, &loop.detector, &loop.status, &loop.lastActivityAt,
			&loop.updatedAt, &loop.evidenceJSON); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan source loop state: %w", err)
		}
		if err := json.Unmarshal([]byte(loop.evidenceJSON), &loop.evidence); err != nil {
			_ = rows.Close()
			return fmt.Errorf("decode loop %d evidence: %w", loop.id, err)
		}
		sourceLoops = append(sourceLoops, loop)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return fmt.Errorf("read source loop state: %w", err)
	}
	if len(sourceLoops) == 0 {
		return nil
	}

	survivorIDs := make(map[int64]struct{})
	for _, loop := range sourceLoops {
		if loop.anchorMessageID.Valid {
			if survivor, ok := messageSurvivors[loop.anchorMessageID.Int64]; ok && survivor != loop.anchorMessageID.Int64 {
				survivorIDs[survivor] = struct{}{}
			}
		}
		for index, item := range loop.evidence {
			rawMessageID, ok := item["message_id"]
			if !ok {
				continue
			}
			var messageID int64
			if err := json.Unmarshal(rawMessageID, &messageID); err != nil {
				return fmt.Errorf("decode loop %d evidence item %d message ID: %w", loop.id, index, err)
			}
			if survivor, ok := messageSurvivors[messageID]; ok && survivor != messageID {
				survivorIDs[survivor] = struct{}{}
			}
		}
	}
	survivorConversations, err := sourceMergeSurvivorConversations(ctx, tx, survivorIDs)
	if err != nil {
		return err
	}

	groups := make(map[sourceMergeLoopGroup][]sourceMergeLoop)
	for _, loop := range sourceLoops {
		if loop.status == "closed" || loop.detector != "turn_state" {
			continue
		}
		conversationID := sourceMergeLoopConversationID(loop, conversations, messageSurvivors, survivorConversations)
		group := sourceMergeLoopGroup{conversationID: conversationID, kind: loop.kind}
		groups[group] = append(groups[group], loop)
	}

	for group, candidates := range groups {
		conflicts, err := tx.QueryContext(ctx, `SELECT id, last_activity_at, updated_at
			FROM open_loops WHERE conversation_id = ? AND kind = ?
			  AND detector = 'turn_state' AND status <> 'closed' AND source_id <> ?`,
			group.conversationID, group.kind, from)
		if err != nil {
			return fmt.Errorf("find source-merge loop conflicts: %w", err)
		}
		for conflicts.Next() {
			var loop sourceMergeLoop
			if err := conflicts.Scan(&loop.id, &loop.lastActivityAt, &loop.updatedAt); err != nil {
				_ = conflicts.Close()
				return fmt.Errorf("scan conflicting loop state: %w", err)
			}
			candidates = append(candidates, loop)
		}
		if err := errors.Join(conflicts.Err(), conflicts.Close()); err != nil {
			return fmt.Errorf("find source-merge loop conflicts: %w", err)
		}
		if len(candidates) < 2 {
			continue
		}

		winner := candidates[0]
		for _, candidate := range candidates[1:] {
			if sourceMergeLoopIsNewer(candidate, winner) {
				winner = candidate
			}
		}
		for _, candidate := range candidates {
			if candidate.id == winner.id {
				continue
			}
			if _, err := tx.ExecContext(ctx, `UPDATE open_loops
				SET status = 'closed', closed_at = `+nowExpr+`,
				    closed_reason = 'withdrawn', closed_by = 'system', updated_at = `+nowExpr+`
				WHERE id = ? AND status <> 'closed'`, candidate.id); err != nil {
				return fmt.Errorf("withdraw superseded loop %d: %w", candidate.id, err)
			}
		}
	}

	for _, loop := range sourceLoops {
		conversationID := sourceMergeLoopConversationID(loop, conversations, messageSurvivors, survivorConversations)
		evidenceJSON, err := remapSourceMergeLoopEvidence(loop, conversations, messageSurvivors, survivorConversations)
		if err != nil {
			return err
		}
		anchorMessageID := remapSourceMergeLoopMessageID(loop.anchorMessageID, messageSurvivors)
		lastActivityMessageID := remapSourceMergeLoopMessageID(loop.lastActivityMessageID, messageSurvivors)
		closedMessageID := remapSourceMergeLoopMessageID(loop.closedMessageID, messageSurvivors)
		if _, err := tx.ExecContext(ctx, `UPDATE open_loops
			SET source_id = ?, conversation_id = ?, anchor_message_id = ?,
			    last_activity_message_id = ?, closed_message_id = ?, evidence_json = ?,
			    updated_at = `+nowExpr+`
			WHERE id = ?`, into, conversationID, anchorMessageID,
			lastActivityMessageID, closedMessageID, evidenceJSON, loop.id); err != nil {
			return fmt.Errorf("update loop %d references: %w", loop.id, err)
		}
	}
	return nil
}

func sourceMergeLoopConversationID(
	loop sourceMergeLoop,
	conversations map[int64]int64,
	messageSurvivors map[int64]int64,
	survivorConversations map[int64]int64,
) int64 {
	if loop.anchorMessageID.Valid {
		if survivor, ok := messageSurvivors[loop.anchorMessageID.Int64]; ok {
			if conversationID, ok := survivorConversations[survivor]; ok {
				return conversationID
			}
		}
	}
	if destination, ok := conversations[loop.conversationID]; ok {
		return destination
	}
	return loop.conversationID
}

func sourceMergeSurvivorConversations(
	ctx context.Context,
	tx *loggedTx,
	survivorIDs map[int64]struct{},
) (map[int64]int64, error) {
	conversations := make(map[int64]int64, len(survivorIDs))
	ids := make([]int64, 0, len(survivorIDs))
	for id := range survivorIDs {
		ids = append(ids, id)
	}
	for start := 0; start < len(ids); start += 100 {
		batch := ids[start:min(start+100, len(ids))]
		placeholders := make([]string, len(batch))
		args := make([]any, len(batch))
		for i, id := range batch {
			placeholders[i] = "?"
			args[i] = id
		}
		rows, err := tx.QueryContext(ctx, `SELECT id, conversation_id FROM messages WHERE id IN (`+strings.Join(placeholders, ",")+")", args...)
		if err != nil {
			return nil, fmt.Errorf("read source-merge survivor conversations: %w", err)
		}
		for rows.Next() {
			var id, conversationID int64
			if err := rows.Scan(&id, &conversationID); err != nil {
				_ = rows.Close()
				return nil, fmt.Errorf("scan source-merge survivor conversation: %w", err)
			}
			conversations[id] = conversationID
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			return nil, fmt.Errorf("read source-merge survivor conversations: %w", err)
		}
	}
	if len(conversations) != len(survivorIDs) {
		return nil, fmt.Errorf("read source-merge survivor conversations: expected %d, found %d", len(survivorIDs), len(conversations))
	}
	return conversations, nil
}

func sourceMergeLoopIsNewer(left, right sourceMergeLoop) bool {
	if !left.lastActivityAt.Equal(right.lastActivityAt) {
		return left.lastActivityAt.After(right.lastActivityAt)
	}
	if !left.updatedAt.Equal(right.updatedAt) {
		return left.updatedAt.After(right.updatedAt)
	}
	return left.id > right.id
}

func remapSourceMergeLoopEvidence(
	loop sourceMergeLoop,
	conversations map[int64]int64,
	messageSurvivors map[int64]int64,
	survivorConversations map[int64]int64,
) (string, error) {
	evidence := loop.evidence
	changed := false
	for index, item := range evidence {
		var survivorConversationID int64
		messageWasRemapped := false
		if rawMessageID, ok := item["message_id"]; ok {
			var messageID int64
			if err := json.Unmarshal(rawMessageID, &messageID); err != nil {
				return "", fmt.Errorf("decode loop %d evidence item %d message ID: %w", loop.id, index, err)
			}
			if survivor, ok := messageSurvivors[messageID]; ok && survivor != messageID {
				encoded, err := json.Marshal(survivor)
				if err != nil {
					return "", fmt.Errorf("encode loop %d evidence message ID: %w", loop.id, err)
				}
				item["message_id"] = jsontext.Value(encoded)
				survivorConversationID, ok = survivorConversations[survivor]
				if !ok {
					return "", fmt.Errorf("read loop %d evidence survivor conversation %d: missing", loop.id, survivor)
				}
				messageWasRemapped = true
				changed = true
			}
		}
		rawConversationID, ok := item["conversation_id"]
		if !ok {
			continue
		}
		var conversationID int64
		if err := json.Unmarshal(rawConversationID, &conversationID); err != nil {
			return "", fmt.Errorf("decode loop %d evidence item %d conversation ID: %w", loop.id, index, err)
		}
		destination, ok := conversations[conversationID]
		if messageWasRemapped {
			destination, ok = survivorConversationID, true
		}
		if !ok || destination == conversationID {
			continue
		}
		encoded, err := json.Marshal(destination)
		if err != nil {
			return "", fmt.Errorf("encode loop %d evidence conversation ID: %w", loop.id, err)
		}
		item["conversation_id"] = jsontext.Value(encoded)
		changed = true
	}
	if !changed {
		return loop.evidenceJSON, nil
	}
	encoded, err := json.Marshal(evidence)
	if err != nil {
		return "", fmt.Errorf("encode loop %d evidence: %w", loop.id, err)
	}
	return string(encoded), nil
}

func remapSourceMergeLoopMessageID(
	messageID sql.NullInt64,
	messageSurvivors map[int64]int64,
) sql.NullInt64 {
	if !messageID.Valid {
		return messageID
	}
	if survivor, ok := messageSurvivors[messageID.Int64]; ok {
		messageID.Int64 = survivor
	}
	return messageID
}
