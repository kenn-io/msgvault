package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"go.kenn.io/msgvault/internal/textutil"
)

// ErrTelegramEditTimestampConflict reports different bodies with the same
// second-resolution edit timestamp. Callers can refresh the authoritative
// message before retrying the write.
var ErrTelegramEditTimestampConflict = errors.New("conflicting Telegram edits have the same timestamp")

// ApplyTelegramEditContext updates one existing Telegram message atomically.
// Older edits are ignored; a different body at the same edit time is refused.
func (s *Store) ApplyTelegramEditContext(ctx context.Context, sourceID int64, key string, editedAt time.Time, text string, raw []byte, attachmentPartKeys []string) (bool, error) {
	return s.applyTelegramEditContext(ctx, sourceID, key, editedAt, text, raw, attachmentPartKeys, false)
}

// ApplyTelegramEditAuthoritativeContext applies a message refreshed from
// Telegram when second-resolution edit timestamps cannot order two snapshots.
func (s *Store) ApplyTelegramEditAuthoritativeContext(ctx context.Context, sourceID int64, key string, editedAt time.Time, text string, raw []byte, attachmentPartKeys []string) (bool, error) {
	return s.applyTelegramEditContext(ctx, sourceID, key, editedAt, text, raw, attachmentPartKeys, true)
}

func (s *Store) applyTelegramEditContext(ctx context.Context, sourceID int64, key string, editedAt time.Time, text string, raw []byte, attachmentPartKeys []string, authoritative bool) (bool, error) {
	if err := s.requireSyncSource(sourceID); err != nil {
		return false, err
	}
	if key == "" || editedAt.IsZero() || len(raw) == 0 || !json.Valid(raw) {
		return false, errors.New("invalid Telegram edit")
	}
	changed := false
	err := s.withTxLockedContext(ctx, nil, func(tx *loggedTx) error { return s.lockSyncSourceTx(ctx, tx, sourceID) }, func(tx *loggedTx) error {
		var sourceType string
		if err := tx.QueryRowContext(ctx, `SELECT source_type FROM sources WHERE id = ?`, sourceID).Scan(&sourceType); err != nil {
			return err
		}
		if sourceType != "telegram" {
			return errors.New("Telegram edit requires a Telegram source")
		}
		var id, convID int64
		var senderID sql.NullInt64
		var metadata sql.NullString
		var sent sql.NullTime
		err := tx.QueryRowContext(ctx, `SELECT id, conversation_id, sender_id, metadata, sent_at FROM messages WHERE source_id = ? AND source_message_id = ? AND message_type = ?`+s.dialect.SelectForUpdate(), sourceID, key, "telegram").Scan(&id, &convID, &senderID, &metadata, &sent)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("find Telegram edit: %w", err)
		}
		if sent.Valid && editedAt.Before(sent.Time) {
			return errors.New("Telegram edit precedes the archived message")
		}
		fields := map[string]json.RawMessage{}
		if metadata.Valid && metadata.String != "" {
			if err := json.Unmarshal([]byte(metadata.String), &fields); err != nil || fields == nil {
				return errors.New("invalid Telegram message metadata")
			}
		}
		var previousEdit time.Time
		hasPreviousEdit := false
		if prior, ok := fields["edited_at"]; ok {
			if err := json.Unmarshal(prior, &previousEdit); err != nil {
				return errors.New("invalid Telegram edit timestamp")
			}
			hasPreviousEdit = true
			if editedAt.Before(previousEdit) {
				return nil
			}
		}
		manifestChanged, err := mergeTelegramAttachmentPartKeys(fields, attachmentPartKeys)
		if err != nil {
			return err
		}
		if hasPreviousEdit && editedAt.Equal(previousEdit) {
			var body sql.NullString
			if err := tx.QueryRowContext(ctx, `SELECT body_text FROM message_bodies WHERE message_id = ?`+s.dialect.SelectForUpdate(), id).Scan(&body); err != nil {
				return fmt.Errorf("read Telegram edit body: %w", err)
			}
			if (body.Valid && body.String != text) || (!body.Valid && text != "") {
				if !authoritative {
					return ErrTelegramEditTimestampConflict
				}
			} else {
				if !manifestChanged {
					return nil
				}
				encoded, err := json.Marshal(fields)
				if err != nil {
					return err
				}
				query := fmt.Sprintf(`UPDATE messages SET metadata = %s WHERE id = ? AND source_id = ?`, s.dialect.JSONBindExpr())
				if _, err := tx.ExecContext(ctx, query, string(encoded), id, sourceID); err != nil {
					return fmt.Errorf("write Telegram edit attachment manifest: %w", err)
				}
				if err := upsertMessageRawWithFormat(boundQuerier{ctx: ctx, q: tx}, id, raw, "telegram_json"); err != nil {
					return fmt.Errorf("write Telegram edit evidence: %w", err)
				}
				changed = true
				return nil
			}
		}
		encodedTime, err := json.Marshal(editedAt.UTC())
		if err != nil {
			return err
		}
		fields["edited_at"] = encodedTime
		encoded, err := json.Marshal(fields)
		if err != nil {
			return err
		}
		var title, sender string
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(title, ''), COALESCE((SELECT display_name FROM participants WHERE id = ?), '') FROM conversations WHERE id = ? AND source_id = ?`, senderID, convID, sourceID).Scan(&title, &sender); err != nil {
			return fmt.Errorf("read Telegram edit context: %w", err)
		}
		q := boundQuerier{ctx: ctx, q: tx}
		body := sql.NullString{String: text, Valid: text != ""}
		if err := upsertMessageBody(q, s.dialect, s.fts5Available, id, body, sql.NullString{}); err != nil {
			return fmt.Errorf("write Telegram edit body: %w", err)
		}
		if err := upsertMessageRawWithFormat(q, id, raw, "telegram_json"); err != nil {
			return fmt.Errorf("write Telegram edit evidence: %w", err)
		}
		snippet := textutil.SnippetFromBody(text)
		query := fmt.Sprintf(`UPDATE messages SET metadata = %s, is_edited = ?, snippet = ?, size_estimate = ? WHERE id = ? AND source_id = ?`, s.dialect.JSONBindExpr())
		if _, err := tx.ExecContext(ctx, query, string(encoded), true, sql.NullString{String: snippet, Valid: snippet != ""}, int64(len(text)), id, sourceID); err != nil {
			return fmt.Errorf("write Telegram edit metadata: %w", err)
		}
		if s.fts5Available {
			if _, err := s.upsertFTSOrUnsearchable(q, FTSDoc{MessageID: id, Subject: title, Body: text, FromAddr: sender}); err != nil {
				return fmt.Errorf("index Telegram edit: %w", err)
			}
		}
		changed = true
		return nil
	})
	return changed, err
}

func mergeTelegramAttachmentPartKeys(fields map[string]json.RawMessage, keys []string) (bool, error) {
	if len(keys) == 0 {
		return false, nil
	}
	var current []string
	if raw, exists := fields["attachment_part_keys"]; exists {
		if err := json.Unmarshal(raw, &current); err != nil {
			return false, errors.New("invalid Telegram attachment manifest")
		}
	}
	if current == nil {
		current = make([]string, 0, len(keys))
	}
	seen := make(map[string]struct{}, len(current)+len(keys))
	for _, key := range current {
		seen[key] = struct{}{}
	}
	changed := false
	for _, key := range keys {
		if key == "" {
			return false, errors.New("invalid Telegram attachment key")
		}
		if _, exists := seen[key]; exists {
			continue
		}
		current = append(current, key)
		seen[key] = struct{}{}
		changed = true
	}
	if !changed {
		return false, nil
	}
	encoded, err := json.Marshal(current)
	if err != nil {
		return false, err
	}
	fields["attachment_part_keys"] = encoded
	return true, nil
}
