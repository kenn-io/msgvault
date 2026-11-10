package store

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"fmt"
	"strings"
)

// ReconcileRedditChatThreadsContext joins imported replies with their known parents.
func (s *Store) ReconcileRedditChatThreadsContext(ctx context.Context, sourceID int64) error {
	if err := s.requireSyncSource(sourceID); err != nil {
		return err
	}
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		rows, err := tx.QueryContext(ctx, `SELECT m.source_message_id, m.conversation_id, m.metadata, c.source_conversation_id
			FROM messages m JOIN conversations c ON c.id = m.conversation_id AND c.source_id = m.source_id
			WHERE m.source_id = ? AND m.message_type = 'reddit_chat' AND m.deleted_at IS NULL
			ORDER BY m.id`, sourceID)
		if err != nil {
			return fmt.Errorf("list Reddit chat replies: %w", err)
		}
		groups := &conversationThreadGroups{parents: make(map[int64]int64), channels: make(map[int64]string)}
		identities := make(map[string]threadIdentity)
		var parents []threadKeyedConversation
		for rows.Next() {
			var source, metadata, channel sql.NullString
			var conversation int64
			if err := rows.Scan(&source, &conversation, &metadata, &channel); err != nil {
				_ = rows.Close()
				return err
			}
			groups.parents[conversation] = conversation
			if strings.HasPrefix(channel.String, "chat:channel:") {
				groups.channels[conversation] = channel.String
			}
			identity := identities[source.String]
			identity.count++
			identity.conversation = conversation
			identities[source.String] = identity
			if metadata.Valid && metadata.String != "" {
				var fields struct {
					Parent string `json:"referenced_message_id"`
				}
				if err := json.Unmarshal([]byte(metadata.String), &fields); err != nil {
					_ = rows.Close()
					return fmt.Errorf("decode Reddit chat reply: %w", err)
				}
				if fields.Parent != "" {
					parents = append(parents, threadKeyedConversation{conversation: conversation, key: fields.Parent})
				}
			}
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return err
		}
		if err := rows.Close(); err != nil {
			return err
		}
		for _, parent := range parents {
			if identity, ok := identities[parent.key]; ok && identity.count == 1 {
				groups.join(parent.conversation, identity.conversation)
			}
		}
		return s.applyConversationThreadGroups(ctx, tx, sourceID, "reddit_chat", groups)
	})
}
