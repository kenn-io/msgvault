package store

import (
	"context"
	"fmt"
	"slices"
)

type threadIdentity struct {
	conversation int64
	count        int
}

type threadKeyedConversation struct {
	conversation int64
	key          string
}

// conversationThreadGroups joins conversations within one source.
type conversationThreadGroups struct {
	parents  map[int64]int64
	keys     map[string]int64
	channels map[int64]string
}

func (g *conversationThreadGroups) find(id int64) int64 {
	root := id
	for g.parents[root] != root {
		root = g.parents[root]
	}
	for g.parents[id] != id {
		next := g.parents[id]
		g.parents[id] = root
		id = next
	}
	return root
}

func (g *conversationThreadGroups) join(a, b int64) {
	a, b = g.find(a), g.find(b)
	if a == b {
		return
	}
	ca, cb := g.channels[a], g.channels[b]
	if ca != "" && cb != "" && ca != cb {
		return
	}
	if (ca == "" && cb != "") || (ca == cb && a > b) {
		a, b = b, a
	}
	g.parents[b] = a
}

func (s *Store) applyConversationThreadGroups(ctx context.Context, tx *loggedTx, sourceID int64, messageType string, groups *conversationThreadGroups) error {
	ids := make([]int64, 0, len(groups.parents))
	for id := range groups.parents {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	affected := make(map[int64]bool)
	for _, id := range ids {
		target := groups.find(id)
		if id == target {
			continue
		}
		result, err := tx.ExecContext(ctx, `UPDATE messages SET conversation_id = ?
   WHERE source_id = ? AND conversation_id = ? AND message_type = ? AND deleted_at IS NULL`,
			target, sourceID, id, messageType)
		if err != nil {
			return fmt.Errorf("move conversation: %w", err)
		}
		count, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("count moved messages: %w", err)
		}
		if count > 0 {
			if messageType == "reddit_chat" {
				rows, err := tx.QueryContext(ctx, `SELECT participant_id, COALESCE(role, 'member') FROM conversation_participants WHERE conversation_id = ? AND left_at IS NULL`, id)
				if err != nil {
					return err
				}
				var participants []ConversationParticipantRef
				for rows.Next() {
					var p ConversationParticipantRef
					if err := rows.Scan(&p.ParticipantID, &p.Role); err != nil {
						_ = rows.Close()
						return err
					}
					participants = append(participants, p)
				}
				if err := rows.Err(); err != nil {
					_ = rows.Close()
					return err
				}
				if err := rows.Close(); err != nil {
					return err
				}
				if err := mergeConversationParticipantsWith(boundQuerier{ctx: ctx, q: tx}, s.dialect, target, participants); err != nil {
					return err
				}
			}
			affected[id] = true
			affected[target] = true
		}
	}
	if len(affected) == 0 {
		return nil
	}
	for _, id := range ids {
		if affected[id] {
			if err := s.recomputeConversationStatsWith(boundQuerier{ctx: ctx, q: tx}, "id = ?", id); err != nil {
				return err
			}
		}
	}
	return s.bumpDerivedDataRevisionContext(ctx, tx)
}
