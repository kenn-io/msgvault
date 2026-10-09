package store

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"

	"go.kenn.io/msgvault/internal/mime"
	"go.kenn.io/msgvault/internal/sqliteutil"
)

const emailReplyMetadataKey = "email_in_reply_to"
const emailReplyBatchSize = 200

// RecordEmailHeadersContext fills missing imported header facts without replacing
// the archived message. The cache revision and a repaired RFC ID commit together.
func (s *Store) RecordEmailHeadersContext(ctx context.Context, sourceID, messageID int64, rfcID, inReplyTo string) error {
	return s.recordEmailHeadersContext(ctx, sourceID, messageID, rfcID, inReplyTo, "", false)
}

const pstThreadMetadataKey = "pst_thread_key"

// RecordPstEmailHeadersContext repairs accepted PST header facts without
// replacing archived content. A stored ID that differs from rfcID wins, and the
// message keeps its stored facts. ReconcilePstEmailThreadsContext reads the
// durable thread key, so finalization can resume after an interruption.
func (s *Store) RecordPstEmailHeadersContext(
	ctx context.Context, sourceID, messageID int64, rfcID, inReplyTo, threadKey string,
) error {
	threadKey = mime.NormalizeMessageID(threadKey)
	return s.recordEmailHeadersContext(ctx, sourceID, messageID, rfcID, inReplyTo, threadKey, true)
}

func (s *Store) recordEmailHeadersContext(
	ctx context.Context, sourceID, messageID int64, rfcID, inReplyTo, threadKey string, pst bool,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := s.requireSyncSource(sourceID); err != nil {
		return err
	}
	rfcID, inReplyTo = mime.NormalizeMessageID(rfcID), mime.NormalizeMessageID(inReplyTo)
	if rfcID == "" && inReplyTo == "" && threadKey == "" {
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
		if pst && storedID.String != "" && rfcID != "" && mime.NormalizeMessageID(storedID.String) != rfcID {
			return nil
		}
		if inReplyTo != "" || threadKey != "" {
			encoded, changed, err := mergeEmailHeaderMetadata(metadata, inReplyTo, threadKey)
			if err != nil {
				return fmt.Errorf("message %d: %w", messageID, err)
			}
			if changed {
				if _, err := tx.ExecContext(ctx, fmt.Sprintf(`UPDATE messages SET metadata = %s WHERE id = ?`,
					s.dialect.JSONBindExpr()), encoded, messageID); err != nil {
					return err
				}
			}
		}
		if storedID.String != "" || rfcID == "" {
			return nil
		}
		if _, err := tx.ExecContext(ctx, `UPDATE messages SET rfc822_message_id = ? WHERE id = ?`, rfcID, messageID); err != nil {
			return err
		}
		return s.bumpDerivedDataRevision(tx)
	})
}

// mergeEmailHeaderMetadata adds header facts that metadata does not already
// hold. A thread key is dropped when the stored parent differs from inReplyTo,
// because the key would then describe a different reply chain.
func mergeEmailHeaderMetadata(metadata sql.NullString, inReplyTo, threadKey string) (string, bool, error) {
	fields := make(map[string]jsontext.Value)
	if metadata.Valid && metadata.String != "" {
		if err := json.Unmarshal([]byte(metadata.String), &fields); err != nil {
			return "", false, fmt.Errorf("decode email metadata: %w", err)
		}
		if fields == nil {
			fields = make(map[string]jsontext.Value)
		}
	}
	if value, ok := fields[emailReplyMetadataKey]; ok && inReplyTo != "" && threadKey != "" {
		var storedParent string
		if err := json.Unmarshal(value, &storedParent); err != nil {
			return "", false, fmt.Errorf("decode email parent ID: %w", err)
		}
		if mime.NormalizeMessageID(storedParent) != inReplyTo {
			threadKey = ""
		}
	}
	changed := false
	for _, fact := range []struct{ key, value string }{
		{emailReplyMetadataKey, inReplyTo}, {pstThreadMetadataKey, threadKey},
	} {
		if _, exists := fields[fact.key]; fact.value == "" || exists {
			continue
		}
		value, err := json.Marshal(fact.value, json.Deterministic(true))
		if err != nil {
			return "", false, err
		}
		fields[fact.key] = value
		changed = true
	}
	if !changed {
		return "", false, nil
	}
	encoded, err := json.Marshal(fields, json.Deterministic(true))
	if err != nil {
		return "", false, err
	}
	return string(encoded), true, nil
}

func (s *Store) lockEmailHeaderRow(ctx context.Context, tx *loggedTx, messageID int64) error {
	// A completed generation fence already reserves SQLite's writer slot for
	// this transaction. Avoid rewriting the content_changed_at index merely
	// to acquire that same reservation again.
	if s.dialect.DriverName() == sqliteutil.DriverName() && tx.syncGenerationFenced {
		return nil
	}
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
		var fields map[string]jsontext.Value
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
