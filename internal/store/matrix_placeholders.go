package store

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"log/slog"
	"strings"

	"go.kenn.io/msgvault/internal/attachmentpolicy"
)

const MatrixEncryptedPlaceholder = "[encrypted message — keys unavailable]"

// ListMatrixRoomIDsContext includes archived rooms absent from the sync cursor.
func (s *Store) ListMatrixRoomIDsContext(ctx context.Context, sourceID int64) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT source_conversation_id FROM conversations WHERE source_id = ? ORDER BY id`, sourceID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var rooms []string
	for rows.Next() {
		var room string
		if err := rows.Scan(&room); err != nil {
			return nil, err
		}
		rooms = append(rooms, room)
	}
	return rooms, rows.Err()
}

// MatrixPendingEditTarget finds the original holding an edit that awaits decryption.
func (s *Store) MatrixPendingEditTarget(ctx context.Context, conversationID int64, eventID string) (int64, error) {
	field := `CASE WHEN instr(m.metadata, '"matrix_pending_edits"') > 0 THEN EXISTS (SELECT 1 FROM json_each(m.metadata, '$.matrix_pending_edits') e WHERE json_extract(e.value, '$.event_id') = ?) ELSE false END`
	if s.IsPostgreSQL() {
		field = "CASE WHEN jsonb_exists(m.metadata, 'matrix_pending_edits') THEN EXISTS (SELECT 1 FROM jsonb_array_elements(COALESCE(NULLIF(m.metadata -> 'matrix_pending_edits', 'null'::jsonb), '[]'::jsonb)) e WHERE e ->> 'event_id' = ?) ELSE false END"
	}
	var messageID int64
	err := s.db.QueryRowContext(ctx, `SELECT m.id FROM messages m WHERE m.conversation_id = ? AND `+field+` ORDER BY m.id LIMIT 1`, conversationID, eventID).Scan(&messageID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return messageID, err
}

func (s *Store) matrixEventTypeField() string {
	if s.IsPostgreSQL() {
		return "m.metadata ->> 'matrix_event_type'"
	}
	return "json_extract(m.metadata, '$.matrix_event_type')"
}

func matrixPlaceholderEventType(raw []byte, compression string) string {
	decoded, err := decodeMessageRaw(raw, sql.NullString{String: compression, Valid: compression != ""})
	if err != nil {
		return ""
	}
	var evt struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(decoded, &evt) != nil {
		return ""
	}
	return evt.Type
}

func (s *Store) classifyMatrixPlaceholders(ctx context.Context, query string, args []any) error {
	var afterID int64
	for {
		pageArgs := append(append([]any{}, args...), afterID)
		rows, err := s.db.QueryContext(ctx, `SELECT m.id, m.metadata, r.raw_data, r.compression`+query+` AND `+s.matrixEventTypeField()+` IS NULL AND m.id > ? ORDER BY m.id LIMIT 100`, pageArgs...)
		if err != nil {
			return err
		}
		type classification struct {
			id       int64
			metadata sql.NullString
		}
		var page []classification
		for rows.Next() {
			var row classification
			var raw []byte
			var compression string
			if err := rows.Scan(&row.id, &row.metadata, &raw, &compression); err != nil {
				_ = rows.Close()
				return err
			}
			metadata := map[string]any{}
			if row.metadata.Valid {
				if err := json.Unmarshal([]byte(row.metadata.String), &metadata); err != nil {
					_ = rows.Close()
					return err
				}
			}
			if metadata == nil {
				metadata = map[string]any{}
			}
			metadata["matrix_event_type"] = matrixPlaceholderEventType(raw, compression)
			encoded, err := json.Marshal(metadata)
			if err != nil {
				_ = rows.Close()
				return err
			}
			row.metadata = sql.NullString{String: string(encoded), Valid: true}
			page = append(page, row)
			afterID = row.id
		}
		err = rows.Err()
		_ = rows.Close()
		if err != nil || len(page) == 0 {
			return err
		}
		for _, row := range page {
			if err := s.SetMessageMetadataContext(ctx, row.id, row.metadata); err != nil {
				return err
			}
		}
	}
}

// MatrixRecoveryTransaction commits a recovered event and placeholder retirement together.
func (s *Store) MatrixRecoveryTransaction(ctx context.Context, fn func(*Store) error) error {
	return s.withAttributionTxContext(ctx, attributionLock{Exclusive: true}, func(tx *loggedTx) error {
		view := s.ScopedToSync(0, 0)
		view.syncGeneration, view.syncBase = s.syncGeneration, s.syncBase
		pool := sql.OpenDB(transactionViewConnector{driver: s.db.Driver()})
		_ = pool.Close()
		view.db = &loggedDB{DB: pool, rebind: s.db.rebind, tx: tx}
		return fn(view)
	})
}

type MatrixPlaceholderRoom struct {
	ConversationType string
	ParticipantCount int
	ConversationID   int64
	RoomID           string
	Count            int64
	MaxMessageID     int64
	MaxEventID       string
}

type MatrixPlaceholder struct {
	MessageID int64
	Deleted   bool
	Hidden    bool
	Raw       []byte
}

// MatrixRecoveredEvents reads durable Megolm replay claims, including hidden rows.
func (s *Store) MatrixRecoveredEvents(ctx context.Context, conversationID int64) (map[string]string, error) {
	fields := "json_each(m.metadata)"
	if s.IsPostgreSQL() {
		fields = "jsonb_each_text(m.metadata)"
	}
	rows, err := s.db.QueryContext(ctx, `SELECT e.key, e.value FROM messages m CROSS JOIN `+fields+` e WHERE m.conversation_id = ? AND substr(e.key, 1, 14) = 'matrix_megolm_' AND e.value IS NOT NULL ORDER BY m.id`, conversationID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	claims := map[string]string{}
	for rows.Next() {
		var key, eventID string
		if err := rows.Scan(&key, &eventID); err != nil {
			return nil, err
		}
		if _, exists := claims[key]; !exists {
			claims[key] = eventID
		}
	}
	return claims, rows.Err()
}

// MatrixPlaceholderMatches prevents recovery from restoring a purged or changed row.
func (s *Store) MatrixPlaceholderMatches(ctx context.Context, sourceID, conversationID int64, item MatrixPlaceholder, eventID string) (bool, error) {
	var matches bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM messages WHERE id = ? AND source_id = ? AND conversation_id = ? AND source_message_id = ? AND message_type = 'matrix' AND snippet = ? AND (deleted_from_source_at IS NOT NULL) = ? AND (deleted_at IS NOT NULL) = ?)`, item.MessageID, sourceID, conversationID, eventID, MatrixEncryptedPlaceholder, item.Deleted, item.Hidden).Scan(&matches)
	return matches, err
}

// HideMatrixPlaceholder retains ciphertext while removing an unsupported event from derived data.
func (s *Store) HideMatrixPlaceholder(ctx context.Context, sourceID, messageID int64) error {
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		if err := s.requireSyncSource(sourceID); err != nil {
			return err
		}
		q := boundQuerier{ctx: ctx, q: tx}
		result, err := q.Exec(`UPDATE messages SET deleted_at = CURRENT_TIMESTAMP, delete_batch_id = NULL, embed_gen = NULL WHERE id = ? AND source_id = ? AND message_type = 'matrix'`, messageID, sourceID)
		if err != nil {
			return err
		}
		if count, err := result.RowsAffected(); err != nil || count == 0 {
			return err
		}
		if s.fts5Available {
			if err := s.dialect.InvalidateFTSForMessage(q, messageID); err != nil {
				return err
			}
		}
		return s.bumpDerivedDataRevisionContext(ctx, tx)
	})
}

func (s *Store) matrixPlaceholderCandidatesQuery(sourceID int64, include, exclude []string) (string, []any) {
	var query strings.Builder
	query.WriteString(` FROM messages m JOIN conversations c ON c.id = m.conversation_id
		JOIN message_raw r ON r.message_id = m.id
		WHERE m.source_id = ? AND m.message_type = 'matrix' AND m.snippet = ? AND r.raw_format = 'matrix_json'`)
	field := s.matrixEventTypeField()
	query.WriteString(" AND COALESCE(" + field + ", 'm.room.encrypted') = 'm.room.encrypted'")
	args := []any{sourceID, MatrixEncryptedPlaceholder}
	appendMatrixRoomFilters(&query, &args, include, exclude)
	return query.String(), args
}

func appendMatrixRoomFilters(query *strings.Builder, args *[]any, include, exclude []string) {
	for _, filter := range []struct {
		rooms    []string
		operator string
	}{{include, "IN"}, {exclude, "NOT IN"}} {
		if len(filter.rooms) != 0 {
			query.WriteString(" AND c.source_conversation_id " + filter.operator + " (" + strings.TrimSuffix(strings.Repeat("?,", len(filter.rooms)), ",") + ")")
			for _, room := range filter.rooms {
				*args = append(*args, room)
			}
		}
	}
}

// MatrixPlaceholderRooms persists legacy event types before counting metadata.
func (s *Store) MatrixPlaceholderRooms(ctx context.Context, sourceID int64, include, exclude []string, policy attachmentpolicy.Policy) ([]MatrixPlaceholderRoom, error) {
	query, args := s.matrixPlaceholderCandidatesQuery(sourceID, include, exclude)
	var pending bool
	if err := s.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1`+query+`)`, args...).Scan(&pending); err != nil || !pending {
		return nil, err
	}
	if err := s.classifyMatrixPlaceholders(ctx, query, args); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT pending.*, newest.source_message_id, c.conversation_type, COALESCE(c.participant_count, 0), COALESCE(CAST(c.metadata AS TEXT), '') FROM (
		SELECT m.conversation_id, c.source_conversation_id, COUNT(*) AS count, MAX(m.id) AS max_message_id`+query+`
		GROUP BY m.conversation_id, c.source_conversation_id
	) pending JOIN messages newest ON newest.id = pending.max_message_id JOIN conversations c ON c.id = pending.conversation_id ORDER BY pending.conversation_id`, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var rooms []MatrixPlaceholderRoom
	for rows.Next() {
		var room MatrixPlaceholderRoom
		var metadata string
		if err := rows.Scan(&room.ConversationID, &room.RoomID, &room.Count, &room.MaxMessageID, &room.MaxEventID, &room.ConversationType, &room.ParticipantCount, &metadata); err != nil {
			return nil, err
		}
		room.ParticipantCount = policyParticipantCount(sourceTypeMatrix, room.ParticipantCount, metadata, policy)
		rooms = append(rooms, room)
	}
	return rooms, rows.Err()
}

// MatrixPlaceholders returns raw candidates and the last scanned ID.
func (s *Store) MatrixPlaceholders(ctx context.Context, conversationID, afterID int64, limit int) ([]MatrixPlaceholder, int64, error) {
	query := ` FROM messages m
		JOIN message_raw r ON r.message_id = m.id
		WHERE m.conversation_id = ? AND m.message_type = 'matrix' AND m.snippet = ? AND r.raw_format = 'matrix_json' AND m.id > ?`
	args := []any{conversationID, MatrixEncryptedPlaceholder, afterID}
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, `SELECT m.id, m.deleted_from_source_at IS NOT NULL, m.deleted_at IS NOT NULL, r.raw_data, r.compression, `+s.matrixEventTypeField()+query+` ORDER BY m.id LIMIT ?`, args...)
	if err != nil {
		return nil, afterID, err
	}
	defer func() { _ = rows.Close() }()
	var items []MatrixPlaceholder
	for rows.Next() {
		var item MatrixPlaceholder
		var raw []byte
		var compression string
		var eventType sql.NullString
		if err := rows.Scan(&item.MessageID, &item.Deleted, &item.Hidden, &raw, &compression, &eventType); err != nil {
			return nil, afterID, err
		}
		afterID = item.MessageID
		if eventType.Valid && eventType.String != "m.room.encrypted" {
			continue
		}
		item.Raw, err = decodeMessageRaw(raw, sql.NullString{String: compression, Valid: compression != ""})
		if err != nil {
			slog.Warn("skip undecodable Matrix placeholder", "message_id", item.MessageID, "error", err)
			continue
		}
		items = append(items, item)
	}
	return items, afterID, rows.Err()
}

// DeleteMatrixPlaceholder retires a recovered relation after its effect is durable.
func (s *Store) DeleteMatrixPlaceholder(ctx context.Context, sourceID, messageID int64) error {
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		if err := s.requireSyncSource(sourceID); err != nil {
			return err
		}
		q := boundQuerier{ctx: ctx, q: tx}
		var owned bool
		if err := q.QueryRow(`SELECT EXISTS (SELECT 1 FROM messages WHERE id = ? AND source_id = ? AND message_type = 'matrix')`, messageID, sourceID).Scan(&owned); err != nil {
			return err
		}
		if !owned {
			return nil
		}
		if s.fts5Available {
			if err := s.dialect.InvalidateFTSForMessage(q, messageID); err != nil {
				return err
			}
		}
		if _, err := q.Exec(`UPDATE messages SET reply_to_message_id = NULL WHERE reply_to_message_id = ?`, messageID); err != nil {
			return err
		}
		if _, err := q.Exec(`DELETE FROM messages WHERE id = ? AND source_id = ?`, messageID, sourceID); err != nil {
			return err
		}
		return s.bumpDerivedDataRevisionContext(ctx, tx)
	})
}
