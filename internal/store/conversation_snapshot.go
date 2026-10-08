package store

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strings"
)

// ApplyConversationSnapshotContext refreshes a room with no new message. Native
// message writers instead include the same projection in MessagePersistData.
func (s *Store) ApplyConversationSnapshotContext(ctx context.Context, sourceID int64, data *ConversationPersistData) (int64, error) {
	if data == nil {
		return 0, errors.New("conversation snapshot is required")
	}
	if err := s.requireSyncSource(sourceID); err != nil {
		return 0, err
	}
	var id int64
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		var err error
		id, err = s.persistConversationSnapshotTx(ctx, tx, sourceID, data)
		return err
	})
	return id, err
}

func (s *Store) persistConversationSnapshotTx(ctx context.Context, tx *loggedTx, sourceID int64, data *ConversationPersistData) (int64, error) {
	q := boundQuerier{ctx: ctx, q: tx}
	conversationID, err := ensureConversationWithTypePolicy(q, s.dialect, sourceID, data.SourceConversationID, data.ConversationType, data.Title, data.PreserveExistingType)
	if err != nil {
		return 0, fmt.Errorf("ensure conversation: %w", err)
	}
	if data.ClearTitle {
		if _, err := q.Exec(`UPDATE conversations SET title='' WHERE id=? AND COALESCE(title,'')<>''`, conversationID); err != nil {
			return 0, fmt.Errorf("clear conversation title: %w", err)
		}
	}
	if data.Participants != nil {
		if data.PreserveExistingParticipants {
			err = mergeConversationParticipantsTx(ctx, tx, s.dialect, conversationID, data.Participants)
		} else {
			err = replaceConversationParticipantsTx(ctx, tx, s.dialect, conversationID, data.Participants)
		}
		if err != nil {
			return 0, fmt.Errorf("persist conversation participants: %w", err)
		}
	}
	if data.MemberCount != nil {
		var stored sql.NullString
		if err := q.QueryRow(`SELECT metadata FROM conversations WHERE id=?`, conversationID).Scan(&stored); err != nil {
			return 0, err
		}
		metadata := make(map[string]jsontext.Value)
		if stored.Valid && strings.TrimSpace(stored.String) != "" {
			if err := json.Unmarshal([]byte(stored.String), &metadata); err != nil {
				return 0, fmt.Errorf("decode conversation snapshot metadata: %w", err)
			}
		}
		if metadata == nil {
			metadata = make(map[string]jsontext.Value)
		}
		count, err := json.Marshal(*data.MemberCount)
		if err != nil {
			return 0, err
		}
		metadata["member_count"] = count
		delete(metadata, "member_count_unknown")
		encoded, err := json.Marshal(metadata, json.Deterministic(true))
		if err != nil {
			return 0, err
		}
		if _, err := q.Exec(fmt.Sprintf(`UPDATE conversations SET metadata=%s WHERE id=?`, s.dialect.JSONBindExpr()), string(encoded), conversationID); err != nil {
			return 0, fmt.Errorf("persist conversation member count: %w", err)
		}
	}
	return conversationID, nil
}
