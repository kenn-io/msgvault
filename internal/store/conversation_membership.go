package store

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"strings"
)

// ListUnresolvedConversationIDs returns provider conversation IDs whose archived
// membership is missing, unreadable, or still being applied, including
// conversations without messages. It uses the same membership record as media
// policy, not accumulated senders.
func (s *Store) ListUnresolvedConversationIDs(ctx context.Context, sourceID int64) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, s.dialect.Rebind(`
		SELECT source_conversation_id, COALESCE(CAST(metadata AS TEXT), '')
		FROM conversations WHERE source_id = ? AND source_conversation_id <> '' ORDER BY id
	`), sourceID)
	if err != nil {
		return nil, fmt.Errorf("list unresolved membership for source %d: %w", sourceID, err)
	}
	defer func() { _ = rows.Close() }()
	var ids []string
	for rows.Next() {
		var id, metadata string
		if err := rows.Scan(&id, &metadata); err != nil {
			return nil, fmt.Errorf("scan unresolved membership for source %d: %w", sourceID, err)
		}
		record := decodeMembershipRecord(metadata)
		if !record.counted || record.unknown || record.repairPending {
			ids = append(ids, id)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list unresolved membership for source %d: %w", sourceID, err)
	}
	return ids, nil
}

// SetSourceConversationRosterRepairPendingContext keeps an interrupted roster
// repair eligible even after it has committed a member count. The marker stays
// until the participant rows, route evidence, and conversation stats are all
// refreshed.
func (s *Store) SetSourceConversationRosterRepairPendingContext(
	ctx context.Context, sourceID int64, sourceConversationID string, pending bool,
) error {
	var conversationID int64
	err := s.db.QueryRowContext(ctx, s.dialect.Rebind(`
		SELECT id FROM conversations WHERE source_id = ? AND source_conversation_id = ?
	`), sourceID, sourceConversationID).Scan(&conversationID)
	if err != nil {
		return fmt.Errorf("find conversation %d/%q for roster repair marker: %w",
			sourceID, sourceConversationID, err)
	}
	return s.SetConversationRosterRepairPendingContext(ctx, conversationID, pending)
}

// SetConversationRosterRepairPendingContext updates the durable marker on one
// archived conversation. A normal sync uses it after refreshing a roster that
// an earlier quiet repair left pending.
func (s *Store) SetConversationRosterRepairPendingContext(
	ctx context.Context, conversationID int64, pending bool,
) error {
	return s.withSyncConversationWriteContext(ctx, conversationID, func(q querier) error {
		return s.setConversationRosterRepairPending(q, conversationID, pending)
	})
}

func (s *Store) setConversationRosterRepairPending(q querier, conversationID int64, pending bool) error {
	var stored sql.NullString
	if err := q.QueryRow(`SELECT metadata FROM conversations WHERE id = ?`, conversationID).Scan(&stored); err != nil {
		return fmt.Errorf("get conversation metadata (id=%d): %w", conversationID, err)
	}
	metadata := make(map[string]jsontext.Value)
	if stored.Valid && strings.TrimSpace(stored.String) != "" {
		if err := json.Unmarshal([]byte(stored.String), &metadata); err != nil {
			return fmt.Errorf("decode conversation metadata (id=%d): %w", conversationID, err)
		}
	}
	_, alreadyPending := metadata["roster_repair_pending"]
	if alreadyPending == pending {
		return nil
	}
	if pending {
		metadata["roster_repair_pending"] = jsontext.Value("true")
	} else {
		delete(metadata, "roster_repair_pending")
	}
	encoded, err := json.Marshal(metadata, json.Deterministic(true))
	if err != nil {
		return fmt.Errorf("encode conversation metadata (id=%d): %w", conversationID, err)
	}
	_, err = q.Exec(fmt.Sprintf(`UPDATE conversations SET metadata = %s WHERE id = ?`, s.dialect.JSONBindExpr()), sql.NullString{String: string(encoded), Valid: len(metadata) != 0}, conversationID)
	if err != nil {
		return fmt.Errorf("set roster repair marker for conversation %d: %w", conversationID, err)
	}
	return nil
}
