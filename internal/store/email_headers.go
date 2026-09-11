package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"go.kenn.io/msgvault/internal/mime"
)

const emailReplyMetadataKey = "email_in_reply_to"
const emailReplyBatchSize = 200

// RecordEmailHeadersContext fills missing imported header facts without replacing
// the archived message. The cache revision and a repaired RFC ID commit together.
func (s *Store) RecordEmailHeadersContext(ctx context.Context, sourceID, messageID int64, rfcID, inReplyTo string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := s.requireSyncSource(sourceID); err != nil {
		return err
	}
	rfcID, inReplyTo = mime.NormalizeMessageID(rfcID), mime.NormalizeMessageID(inReplyTo)
	if rfcID == "" && inReplyTo == "" {
		return nil
	}
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		if err := s.lockEmailHeaderRow(ctx, tx, messageID); err != nil {
			return err
		}
		var storedID, metadata sql.NullString
		if err := tx.QueryRowContext(ctx, `SELECT rfc822_message_id, metadata FROM messages
   WHERE id = ? AND source_id = ? AND message_type = 'email'`+s.dialect.SelectForUpdate(),
			messageID, sourceID).Scan(&storedID, &metadata); err != nil {
			return fmt.Errorf("read email header target: %w", err)
		}
		fillID := storedID.String == "" && rfcID != ""
		if inReplyTo != "" {
			fields := make(map[string]json.RawMessage)
			if metadata.Valid && metadata.String != "" {
				if err := json.Unmarshal([]byte(metadata.String), &fields); err != nil {
					return fmt.Errorf("decode email metadata: %w", err)
				}
				if fields == nil {
					fields = make(map[string]json.RawMessage)
				}
			}
			if _, exists := fields[emailReplyMetadataKey]; !exists {
				value, err := json.Marshal(inReplyTo)
				if err != nil {
					return err
				}
				fields[emailReplyMetadataKey] = value
				encoded, err := json.Marshal(fields)
				if err != nil {
					return err
				}
				if _, err := tx.ExecContext(ctx, fmt.Sprintf(`UPDATE messages SET metadata = %s WHERE id = ?`, s.dialect.JSONBindExpr()), string(encoded), messageID); err != nil {
					return err
				}
			}
		}
		if !fillID {
			return nil
		}
		if _, err := tx.ExecContext(ctx, `UPDATE messages SET rfc822_message_id = ? WHERE id = ?`, rfcID, messageID); err != nil {
			return err
		}
		return s.bumpDerivedDataRevision(tx)
	})
}

func (s *Store) lockEmailHeaderRow(ctx context.Context, tx *loggedTx, messageID int64) error {
	// Acquire the SQLite writer slot before reading. This column is outside the
	// last-modified trigger, so an idempotent repair does not change message facts.
	if query := s.dialect.RowWriterLockSQL("messages", "content_changed_at"); query != "" {
		if _, err := tx.ExecContext(ctx, query, messageID); err != nil {
			return fmt.Errorf("lock email header row: %w", err)
		}
	}
	return nil
}

// ResolveEmailReplyParentsContext links unique archived parents in bounded
// pages. checkpoint runs after a page's writes commit; repeating a page after
// interruption is safe because existing links are never overwritten.
func (s *Store) ResolveEmailReplyParentsContext(ctx context.Context, sourceID, afterID int64, checkpoint func(int64) error) error {
	if err := s.requireSyncSource(sourceID); err != nil {
		return err
	}
	for {
		ids, err := s.unresolvedEmailReplyPage(ctx, sourceID, afterID)
		if err != nil {
			return err
		}
		if len(ids) == 0 {
			return ctx.Err()
		}
		for _, id := range ids {
			if err := s.resolveEmailReply(ctx, sourceID, id); err != nil {
				return err
			}
		}
		afterID = ids[len(ids)-1]
		if checkpoint != nil {
			if err := checkpoint(afterID); err != nil {
				return fmt.Errorf("checkpoint email replies: %w", err)
			}
		}
	}
}

func (s *Store) unresolvedEmailReplyPage(ctx context.Context, sourceID, afterID int64) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM messages
  WHERE source_id = ? AND message_type = 'email' AND id > ?
   AND reply_to_message_id IS NULL AND deleted_at IS NULL
   AND CAST(metadata AS TEXT) LIKE '%"email_in_reply_to"%'
  ORDER BY id LIMIT ?`, sourceID, afterID, emailReplyBatchSize)
	if err != nil {
		return nil, fmt.Errorf("list email replies: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (s *Store) resolveEmailReply(ctx context.Context, sourceID, childID int64) error {
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		if err := s.lockEmailHeaderRow(ctx, tx, childID); err != nil {
			return err
		}
		var metadata sql.NullString
		var reply sql.NullInt64
		err := tx.QueryRowContext(ctx, `SELECT metadata, reply_to_message_id FROM messages
   WHERE id = ? AND source_id = ? AND message_type = 'email' AND deleted_at IS NULL`+s.dialect.SelectForUpdate(), childID, sourceID).Scan(&metadata, &reply)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if reply.Valid || !metadata.Valid {
			return nil
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal([]byte(metadata.String), &fields); err != nil {
			return fmt.Errorf("decode email reply metadata: %w", err)
		}
		var rawID string
		if value, ok := fields[emailReplyMetadataKey]; !ok {
			return nil
		} else if err := json.Unmarshal(value, &rawID); err != nil {
			return fmt.Errorf("decode email parent ID: %w", err)
		}
		parentRFCID := mime.NormalizeMessageID(rawID)
		if parentRFCID == "" {
			return nil
		}
		// The SQLite canonical index compares BLOB bytes; PostgreSQL compares TEXT.
		var canonical any = parentRFCID
		if !s.IsPostgreSQL() {
			canonical = []byte(parentRFCID)
		}
		rows, err := tx.QueryContext(ctx, fmt.Sprintf(`SELECT id FROM messages
   WHERE %s = ? AND source_id = ? AND message_type = 'email' AND deleted_at IS NULL
   ORDER BY id LIMIT 2`, s.dialect.RFC822CanonicalIDExpr("rfc822_message_id")), canonical, sourceID)
		if err != nil {
			return fmt.Errorf("find email parent: %w", err)
		}
		var parents []int64
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				_ = rows.Close()
				return err
			}
			parents = append(parents, id)
		}
		rowErr := rows.Err()
		closeErr := rows.Close()
		if rowErr != nil {
			return rowErr
		}
		if closeErr != nil {
			return closeErr
		}
		if len(parents) != 1 || parents[0] == childID {
			return nil
		}
		_, err = tx.ExecContext(ctx, `UPDATE messages SET reply_to_message_id = ?
   WHERE id = ? AND source_id = ? AND reply_to_message_id IS NULL`, parents[0], childID, sourceID)
		return err
	})
}
