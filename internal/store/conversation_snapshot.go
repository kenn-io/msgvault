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
			err = mergeConversationParticipantsTx(ctx, tx, s.dialect, conversationID, data.Participants, data.PreserveExistingParticipantRoles)
		} else {
			err = replaceConversationParticipantsTx(ctx, tx, s.dialect, conversationID, data.Participants)
		}
		if err != nil {
			return 0, fmt.Errorf("persist conversation participants: %w", err)
		}
	}
	if data.ParticipantTitle != nil {
		policy := data.ParticipantTitle
		if policy.MaxNames < 1 || policy.MaxNames > 16 {
			return 0, errors.New("participant title limit must be between 1 and 16")
		}
		rows, err := tx.QueryContext(ctx, `SELECT COALESCE(NULLIF(p.display_name,''),NULLIF(p.phone_number,''),NULLIF(p.email_address,''),'?'),COUNT(*) OVER()
   FROM conversation_participants cp JOIN participants p ON p.id=cp.participant_id
   WHERE cp.conversation_id=? AND (?='' OR COALESCE(p.email_address,'')<>?) AND (?='' OR COALESCE(p.display_name,'')<>?)
   ORDER BY p.id LIMIT ?`, conversationID, policy.ExcludeEmail, policy.ExcludeEmail, policy.ExcludeDisplayName, policy.ExcludeDisplayName, policy.MaxNames)
		if err != nil {
			return 0, fmt.Errorf("read participant title: %w", err)
		}
		var names []string
		var count int
		for rows.Next() {
			var name string
			if err := rows.Scan(&name, &count); err != nil {
				_ = rows.Close()
				return 0, err
			}
			names = append(names, name)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return 0, err
		}
		if err := rows.Close(); err != nil {
			return 0, err
		}
		if len(names) > 0 {
			title := strings.Join(names, ", ")
			if count > len(names) {
				title += fmt.Sprintf(" +%d more", count-len(names))
			}
			if _, err := q.Exec(`UPDATE conversations SET title=? WHERE id=?`, title, conversationID); err != nil {
				return 0, fmt.Errorf("persist participant title: %w", err)
			}
		}
	}
	if data.MemberCount != nil || data.MemberCountUnknown {
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
		if data.MemberCountUnknown {
			metadata["member_count_unknown"] = jsontext.Value("true")
		} else {
			count, err := json.Marshal(*data.MemberCount)
			if err != nil {
				return 0, err
			}
			metadata["member_count"] = count
			delete(metadata, "member_count_unknown")
		}
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

// ConversationIDContext finds an existing native conversation without creating
// it. Zero means absent; callers still resolve it inside their write transaction.
func (s *Store) ConversationIDContext(ctx context.Context, sourceID int64, key string) (int64, error) {
	var id int64
	err := s.db.QueryRowContext(ctx, s.Rebind(`SELECT id FROM conversations WHERE source_id=? AND source_conversation_id=?`), sourceID, key).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return id, err
}
