package store

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strings"
)

const MatrixEncryptedPlaceholder = "[encrypted message — keys unavailable]"

// matrixPendingEncryptedSQL uses recovery state rather than user-authored text.
func matrixPendingEncryptedSQL(message string) string {
	return `EXISTS(SELECT 1 FROM matrix_encrypted_events encrypted WHERE encrypted.message_id=` + message + `.id AND NOT encrypted.recovered)`
}

const migrationMatrixEncryptedEventsV1 = "matrix_encrypted_events_v1"

// nativeMatrixRoom skips rooms a source merge moved in. Their IDs carry the
// archive-internal prefix, which survives subset export unlike merge records.
const nativeMatrixRoom = " AND c.source_conversation_id NOT LIKE 'msgvault-archive:%'"

// ListMatrixRoomIDsContext includes archived rooms absent from the sync cursor.
// It skips rooms moved in by a source merge, which the homeserver cannot know.
func (s *Store) ListMatrixRoomIDsContext(ctx context.Context, sourceID int64) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT c.source_conversation_id FROM conversations c WHERE c.source_id = ?`+nativeMatrixRoom+` ORDER BY c.id`, sourceID)
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

// MatrixPendingRoom is a room with encrypted events that are not yet recovered.
type MatrixPendingRoom struct {
	ConversationID int64
	RoomID         string
}

// MatrixEncryptedEvent is an encrypted event that is due for a recovery attempt.
type MatrixEncryptedEvent struct {
	MessageID  int64
	Ciphertext []byte
}

// MatrixRecoveryDue selects the pending events a recovery pass attempts:
// every event when All is set, otherwise new events and events last tried
// under another backup state before RetryBefore (Unix seconds).
type MatrixRecoveryDue struct {
	All         bool
	Stamp       string
	RetryBefore int64
}

func (d MatrixRecoveryDue) where() (string, []any) {
	if d.All {
		return "", nil
	}
	return ` AND (e.tried_stamp IS NULL OR (e.tried_stamp <> ? AND e.tried_at <= ?))`, []any{d.Stamp, d.RetryBefore}
}

// ErrMatrixPlaceholderGone reports that a placeholder was deleted or replaced
// while its event was being decrypted.
var ErrMatrixPlaceholderGone = errors.New("matrix placeholder is no longer pending")

// MatrixPersist adds recovery bookkeeping to a Matrix message write.
type MatrixPersist struct {
	// SessionID registers a new encrypted event, with the raw event as its ciphertext.
	SessionID string
	// Placeholder is the pending placeholder a decrypted or reverted event replaces.
	Placeholder int64
}

// PersistMatrixMessageContext writes a Matrix message and its recovery
// bookkeeping in one transaction. Replacing a placeholder fails with
// ErrMatrixPlaceholderGone unless that row is still pending for the same event.
func (s *Store) PersistMatrixMessageContext(ctx context.Context, data *MessagePersistData, matrix MatrixPersist) (int64, error) {
	if data == nil || data.Message == nil {
		return 0, errors.New("persist message requires a message")
	}
	prepare := func(ctx context.Context, tx *loggedTx, data *MessagePersistData) (*MessagePersistData, error) {
		if matrix.Placeholder == 0 {
			return data, nil
		}
		var pending bool
		if err := (boundQuerier{ctx: ctx, q: tx}).QueryRow(`SELECT EXISTS (SELECT 1 FROM matrix_encrypted_events e JOIN messages m ON m.id = e.message_id
			WHERE e.message_id = ? AND NOT e.recovered AND m.source_message_id = ?)`, matrix.Placeholder, data.Message.SourceMessageID).Scan(&pending); err != nil {
			return nil, err
		}
		if !pending {
			return nil, ErrMatrixPlaceholderGone
		}
		return data, nil
	}
	after := func(ctx context.Context, tx *loggedTx, data *MessagePersistData, messageID int64) error {
		if matrix.Placeholder != 0 {
			if messageID != matrix.Placeholder {
				return fmt.Errorf("matrix placeholder %d was written as message %d", matrix.Placeholder, messageID)
			}
			// Replacing a placeholder's text makes cached derived data stale.
			if err := s.advanceDerivedRevisionWithEmailReceiptContext(ctx, tx); err != nil {
				return err
			}
		}
		if matrix.SessionID == "" {
			return nil
		}
		// A row left by a deleted message whose ID was reused is replaced.
		_, err := (boundQuerier{ctx: ctx, q: tx}).Exec(`INSERT INTO matrix_encrypted_events (message_id, conversation_id, session_id, ciphertext) VALUES (?, ?, ?, ?)
			ON CONFLICT (message_id) DO UPDATE SET conversation_id = excluded.conversation_id, session_id = excluded.session_id,
				ciphertext = excluded.ciphertext, recovered = FALSE, tried_stamp = NULL, tried_at = NULL`,
			messageID, data.Message.ConversationID, matrix.SessionID, string(data.RawMIME))
		return err
	}
	return s.persistMessageWithParticipantsTransaction(
		ctx, attributionLockForMessage(data.Message.SourceID, data.Message.MessageType),
		nil, nil, func([]int64) *MessagePersistData { return data }, prepare, after,
	)
}

// matrixRoomFilter selects the configured rooms. It always skips rooms moved
// in by a source merge, whose events the homeserver cannot decrypt.
func matrixRoomFilter(include, exclude []string) (string, []any) {
	var query strings.Builder
	query.WriteString(nativeMatrixRoom)
	var args []any
	for _, filter := range []struct {
		rooms    []string
		operator string
	}{{include, "IN"}, {exclude, "NOT IN"}} {
		if len(filter.rooms) == 0 {
			continue
		}
		query.WriteString(" AND c.source_conversation_id " + filter.operator + " (" + strings.TrimSuffix(strings.Repeat("?,", len(filter.rooms)), ",") + ")")
		for _, room := range filter.rooms {
			args = append(args, room)
		}
	}
	return query.String(), args
}

// MatrixPendingRooms lists the selected rooms that still hold encrypted events.
func (s *Store) MatrixPendingRooms(ctx context.Context, sourceID int64, include, exclude []string) ([]MatrixPendingRoom, error) {
	filter, filterArgs := matrixRoomFilter(include, exclude)
	rows, err := s.db.QueryContext(ctx, `SELECT c.id, c.source_conversation_id FROM conversations c
		WHERE c.source_id = ?`+filter+` AND EXISTS (SELECT 1 FROM matrix_encrypted_events e WHERE e.conversation_id = c.id AND NOT e.recovered)
		ORDER BY c.id`, append([]any{sourceID}, filterArgs...)...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var rooms []MatrixPendingRoom
	for rows.Next() {
		var room MatrixPendingRoom
		if err := rows.Scan(&room.ConversationID, &room.RoomID); err != nil {
			return nil, err
		}
		rooms = append(rooms, room)
	}
	return rooms, rows.Err()
}

// CountMatrixPending counts the encrypted events not yet recovered in the selected rooms.
func (s *Store) CountMatrixPending(ctx context.Context, sourceID int64, include, exclude []string) (int64, error) {
	filter, filterArgs := matrixRoomFilter(include, exclude)
	var count int64
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM matrix_encrypted_events e JOIN conversations c ON c.id = e.conversation_id
		WHERE c.source_id = ? AND NOT e.recovered`+filter, append([]any{sourceID}, filterArgs...)...).Scan(&count)
	return count, err
}

// MatrixDueEncryptedEvents returns the next page of due events in a room, after afterID.
func (s *Store) MatrixDueEncryptedEvents(ctx context.Context, conversationID, afterID int64, due MatrixRecoveryDue, limit int) ([]MatrixEncryptedEvent, error) {
	dueWhere, dueArgs := due.where()
	args := append(append([]any{conversationID, afterID}, dueArgs...), limit)
	rows, err := s.db.QueryContext(ctx, `SELECT e.message_id, e.ciphertext FROM matrix_encrypted_events e
		WHERE e.conversation_id = ? AND NOT e.recovered AND e.message_id > ?`+dueWhere+` ORDER BY e.message_id LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var events []MatrixEncryptedEvent
	for rows.Next() {
		var evt MatrixEncryptedEvent
		var ciphertext string
		if err := rows.Scan(&evt.MessageID, &ciphertext); err != nil {
			return nil, err
		}
		evt.Ciphertext = []byte(ciphertext)
		events = append(events, evt)
	}
	return events, rows.Err()
}

// MarkMatrixEncryptedEventsTried records a failed attempt under the given
// backup state. Matching the ciphertext skips a row whose ID a new event reused.
func (s *Store) MarkMatrixEncryptedEventsTried(ctx context.Context, events []MatrixEncryptedEvent, stamp string, at int64) error {
	if len(events) == 0 {
		return nil
	}
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		q := boundQuerier{ctx: ctx, q: tx}
		for _, evt := range events {
			if _, err := q.Exec(`UPDATE matrix_encrypted_events SET tried_stamp = ?, tried_at = ? WHERE message_id = ? AND ciphertext = ?`,
				stamp, at, evt.MessageID, string(evt.Ciphertext)); err != nil {
				return err
			}
		}
		return nil
	})
}

// MatrixEncryptedEventPending reports whether a message is still the pending
// placeholder for eventID, and whether its source deleted it.
func (s *Store) MatrixEncryptedEventPending(ctx context.Context, messageID int64, eventID string) (pending, deleted bool, err error) {
	err = s.db.QueryRowContext(ctx, `SELECT m.deleted_from_source_at IS NOT NULL FROM matrix_encrypted_events e JOIN messages m ON m.id = e.message_id
		WHERE e.message_id = ? AND NOT e.recovered AND m.source_message_id = ?`, messageID, eventID).Scan(&deleted)
	if errors.Is(err, sql.ErrNoRows) {
		return false, false, nil
	}
	return err == nil, deleted, err
}

// MatrixEncryptedEventCiphertext returns the encrypted event archived for a message, or nil.
func (s *Store) MatrixEncryptedEventCiphertext(ctx context.Context, messageID int64) ([]byte, error) {
	var ciphertext string
	err := s.db.QueryRowContext(ctx, `SELECT ciphertext FROM matrix_encrypted_events WHERE message_id = ?`, messageID).Scan(&ciphertext)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return []byte(ciphertext), err
}

// SetMatrixEncryptedEventRecovered marks a placeholder recovered, or pending
// again after a replay conflict. Recovery drops its queued edits, which it has applied.
func (s *Store) SetMatrixEncryptedEventRecovered(ctx context.Context, messageID int64, recovered bool, stamp string, at int64) error {
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		q := boundQuerier{ctx: ctx, q: tx}
		if err := s.requireSyncMessageSourceTx(q, messageID); err != nil {
			return err
		}
		if _, err := q.Exec(`UPDATE matrix_encrypted_events SET recovered = ?, tried_stamp = ?, tried_at = ? WHERE message_id = ?`,
			recovered, sql.NullString{String: stamp, Valid: !recovered}, sql.NullInt64{Int64: at, Valid: !recovered}, messageID); err != nil {
			return err
		}
		if !recovered {
			return nil
		}
		_, err := q.Exec(`DELETE FROM matrix_pending_edits WHERE target_message_id = ?`, messageID)
		return err
	})
}

// MatrixMegolmClaim returns the event that first used a Megolm message index in a room.
func (s *Store) MatrixMegolmClaim(ctx context.Context, conversationID int64, sessionID string, index uint) (string, error) {
	var eventID string
	err := s.db.QueryRowContext(ctx, `SELECT event_id FROM matrix_megolm_claims WHERE conversation_id = ? AND session_id = ? AND message_index = ?`,
		conversationID, sessionID, index).Scan(&eventID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return eventID, err
}

// ClaimMatrixMegolmIndex records eventID as the first user of a Megolm message
// index unless another event already holds it, and reports whether eventID holds it.
func (s *Store) ClaimMatrixMegolmIndex(ctx context.Context, conversationID int64, sessionID string, index uint, eventID string) (bool, error) {
	var holder string
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		q := boundQuerier{ctx: ctx, q: tx}
		if err := s.requireSyncConversationSourceTx(q, conversationID); err != nil {
			return err
		}
		if _, err := q.Exec(`INSERT INTO matrix_megolm_claims (conversation_id, session_id, message_index, event_id) VALUES (?, ?, ?, ?)
			ON CONFLICT (conversation_id, session_id, message_index) DO NOTHING`, conversationID, sessionID, index, eventID); err != nil {
			return err
		}
		return q.QueryRow(`SELECT event_id FROM matrix_megolm_claims WHERE conversation_id = ? AND session_id = ? AND message_index = ?`,
			conversationID, sessionID, index).Scan(&holder)
	})
	return holder == eventID, err
}

// ReassignMatrixMegolmClaim gives a Megolm message index to an earlier event.
func (s *Store) ReassignMatrixMegolmClaim(ctx context.Context, conversationID int64, sessionID string, index uint, eventID string) error {
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		q := boundQuerier{ctx: ctx, q: tx}
		if err := s.requireSyncConversationSourceTx(q, conversationID); err != nil {
			return err
		}
		_, err := q.Exec(`UPDATE matrix_megolm_claims SET event_id = ? WHERE conversation_id = ? AND session_id = ? AND message_index = ?`,
			eventID, conversationID, sessionID, index)
		return err
	})
}

// QueueMatrixPendingEdit holds an edit until its target is recovered, keeping
// only the newest keep edits for each target.
func (s *Store) QueueMatrixPendingEdit(ctx context.Context, targetID int64, eventID string, originTS int64, evt []byte, keep int) error {
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		q := boundQuerier{ctx: ctx, q: tx}
		if err := s.requireSyncMessageSourceTx(q, targetID); err != nil {
			return err
		}
		if _, err := q.Exec(`INSERT INTO matrix_pending_edits (target_message_id, event_id, origin_ts, event) VALUES (?, ?, ?, ?)
			ON CONFLICT (target_message_id, event_id) DO NOTHING`, targetID, eventID, originTS, string(evt)); err != nil {
			return err
		}
		_, err := q.Exec(`DELETE FROM matrix_pending_edits WHERE target_message_id = ? AND event_id NOT IN (
			SELECT event_id FROM matrix_pending_edits WHERE target_message_id = ? ORDER BY origin_ts DESC, event_id DESC LIMIT ?)`,
			targetID, targetID, keep)
		return err
	})
}

// MatrixPendingEdits returns a target's queued edits, oldest first.
func (s *Store) MatrixPendingEdits(ctx context.Context, targetID int64) ([][]byte, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT event FROM matrix_pending_edits WHERE target_message_id = ? ORDER BY origin_ts, event_id`, targetID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var edits [][]byte
	for rows.Next() {
		var evt string
		if err := rows.Scan(&evt); err != nil {
			return nil, err
		}
		edits = append(edits, []byte(evt))
	}
	return edits, rows.Err()
}

// DeleteMatrixPendingEdit drops a redacted edit from a room's queue and reports whether one was queued.
func (s *Store) DeleteMatrixPendingEdit(ctx context.Context, conversationID int64, eventID string) (bool, error) {
	var deleted int64
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		q := boundQuerier{ctx: ctx, q: tx}
		if err := s.requireSyncConversationSourceTx(q, conversationID); err != nil {
			return err
		}
		result, err := q.Exec(`DELETE FROM matrix_pending_edits WHERE event_id = ?
			AND target_message_id IN (SELECT id FROM messages WHERE conversation_id = ?)`, eventID, conversationID)
		if err != nil {
			return err
		}
		deleted, err = result.RowsAffected()
		return err
	})
	return deleted > 0, err
}

// DeleteMatrixPlaceholder removes a placeholder whose decrypted event left no message.
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

// registerLegacyMatrixPlaceholders registers placeholders archived before
// matrix_encrypted_events existed, so recovery can find them.
func (s *Store) registerLegacyMatrixPlaceholders(ctx context.Context) error {
	var afterID int64
	for {
		type placeholder struct {
			messageID, conversationID int64
			session                   string
			raw                       []byte
		}
		var page []placeholder
		rows, err := s.db.QueryContext(ctx, `SELECT m.id, m.conversation_id, r.raw_data, r.compression FROM messages m
			JOIN message_raw r ON r.message_id = m.id
			WHERE m.message_type = 'matrix' AND m.snippet = ? AND r.raw_format = 'matrix_json' AND m.id > ?
				AND NOT EXISTS (SELECT 1 FROM matrix_encrypted_events e WHERE e.message_id = m.id)
			ORDER BY m.id LIMIT 500`, MatrixEncryptedPlaceholder, afterID)
		if err != nil {
			return err
		}
		scanned := 0
		for rows.Next() {
			scanned++
			var row placeholder
			var raw []byte
			var compression sql.NullString
			if err := rows.Scan(&row.messageID, &row.conversationID, &raw, &compression); err != nil {
				_ = rows.Close()
				return err
			}
			afterID = row.messageID
			if row.raw, err = decodeMessageRaw(raw, compression); err != nil {
				continue
			}
			var evt struct {
				Type    string `json:"type"`
				Content struct {
					SessionID string `json:"session_id"`
				} `json:"content"`
			}
			if json.Unmarshal(row.raw, &evt) != nil || evt.Type != "m.room.encrypted" || evt.Content.SessionID == "" {
				continue
			}
			row.session = evt.Content.SessionID
			page = append(page, row)
		}
		err = rows.Err()
		_ = rows.Close()
		if err != nil || scanned == 0 {
			return err
		}
		for _, row := range page {
			if _, err := s.db.ExecContext(ctx, `INSERT INTO matrix_encrypted_events (message_id, conversation_id, session_id, ciphertext) VALUES (?, ?, ?, ?)`,
				row.messageID, row.conversationID, row.session, string(row.raw)); err != nil {
				return fmt.Errorf("register Matrix placeholder %d: %w", row.messageID, err)
			}
		}
	}
}
