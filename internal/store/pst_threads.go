package store

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"slices"

	"go.kenn.io/msgvault/internal/mime"
)

// ReconcilePstEmailThreadsContext joins imported email conversations using
// accepted PST thread keys and unique, live, same-source Internet Message-IDs.
// Existing conversation rows retain their keys: a later import through an empty
// key reconnects through the matching message's current conversation. Native
// provider threads are unaffected unless the caller explicitly opts in.
func (s *Store) ReconcilePstEmailThreadsContext(ctx context.Context, sourceID int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := s.requireSyncSource(sourceID); err != nil {
		return err
	}
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		rows, err := tx.QueryContext(ctx, `SELECT id, source_conversation_id FROM conversations WHERE source_id = ? AND conversation_type = 'email_thread' ORDER BY id`, sourceID)
		if err != nil {
			return fmt.Errorf("list PST conversations: %w", err)
		}
		parents := make(map[int64]int64)
		keys := make(map[string]int64)
		for rows.Next() {
			var id int64
			var key sql.NullString
			if err := rows.Scan(&id, &key); err != nil {
				_ = rows.Close()
				return err
			}
			parents[id] = id
			if key := mime.NormalizeMessageID(key.String); key != "" {
				keys[key] = id
			}
		}
		err = rows.Err()
		closeErr := rows.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
		find := func(id int64) int64 {
			root := id
			for parents[root] != root {
				root = parents[root]
			}
			for parents[id] != id {
				next := parents[id]
				parents[id] = root
				id = next
			}
			return root
		}
		join := func(a, b int64) {
			a, b = find(a), find(b)
			if a > b {
				a, b = b, a
			}
			parents[b] = a
		}
		type identity struct {
			conversation int64
			count        int
		}
		identities := make(map[string]identity)
		type parentFact struct {
			conversation int64
			key          string
		}
		var parentFacts []parentFact
		rows, err = tx.QueryContext(ctx, `SELECT conversation_id, rfc822_message_id, metadata FROM messages WHERE source_id = ? AND message_type = 'email' AND deleted_at IS NULL`, sourceID)
		if err != nil {
			return fmt.Errorf("list PST thread facts: %w", err)
		}
		for rows.Next() {
			if err := ctx.Err(); err != nil {
				_ = rows.Close()
				return err
			}
			var conv int64
			var rfcID, metadata sql.NullString
			if err := rows.Scan(&conv, &rfcID, &metadata); err != nil {
				_ = rows.Close()
				return err
			}
			if _, ok := parents[conv]; !ok {
				continue
			}
			if key := mime.NormalizeMessageID(rfcID.String); key != "" {
				fact := identities[key]
				fact.count++
				fact.conversation = conv
				identities[key] = fact
			}
			if metadata.Valid && metadata.String != "" {
				var fields map[string]jsontext.Value
				if err := json.Unmarshal([]byte(metadata.String), &fields); err != nil {
					_ = rows.Close()
					return fmt.Errorf("decode PST thread metadata: %w", err)
				}
				if value, ok := fields[emailReplyMetadataKey]; ok {
					var key string
					if err := json.Unmarshal(value, &key); err != nil {
						_ = rows.Close()
						return fmt.Errorf("decode PST parent ID: %w", err)
					}
					if key = mime.NormalizeMessageID(key); key != "" {
						parentFacts = append(parentFacts, parentFact{conversation: conv, key: key})
					}
				}
				if value, ok := fields[pstThreadMetadataKey]; ok {
					var key string
					if err := json.Unmarshal(value, &key); err != nil {
						_ = rows.Close()
						return fmt.Errorf("decode PST thread key: %w", err)
					}
					if target, ok := keys[mime.NormalizeMessageID(key)]; ok {
						join(conv, target)
					}
				}
			}
		}
		err = rows.Err()
		closeErr = rows.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
		// Do not use reply_to_message_id blindly: dedup may redirect it across
		// sources. Only current unique canonical identity facts connect aliases.
		for key, fact := range identities {
			if target, ok := keys[key]; ok && fact.count == 1 {
				join(target, fact.conversation)
			}
		}
		// References can name an ancestor absent from the archive. An accepted,
		// uniquely resolved direct parent still connects that reply chain.
		for _, parent := range parentFacts {
			if fact, ok := identities[parent.key]; ok && fact.count == 1 {
				join(parent.conversation, fact.conversation)
			}
		}
		ids := make([]int64, 0, len(parents))
		for id := range parents {
			ids = append(ids, id)
		}
		slices.Sort(ids)
		affected := make(map[int64]bool)
		for _, id := range ids {
			target := find(id)
			if id == target {
				continue
			}
			result, err := tx.ExecContext(ctx, `UPDATE messages SET conversation_id = ? WHERE source_id = ? AND conversation_id = ? AND message_type = 'email' AND deleted_at IS NULL`, target, sourceID, id)
			if err != nil {
				return fmt.Errorf("move PST conversation: %w", err)
			}
			count, err := result.RowsAffected()
			if err != nil {
				return err
			}
			if count > 0 {
				affected[id] = true
				affected[target] = true
			}
		}
		for _, id := range ids {
			if affected[id] {
				if err := s.recomputeConversationStatsWith(boundQuerier{ctx: ctx, q: tx}, "id = ?", id); err != nil {
					return err
				}
			}
		}
		if len(affected) > 0 {
			return s.bumpDerivedDataRevision(tx)
		}
		return nil
	})
}
