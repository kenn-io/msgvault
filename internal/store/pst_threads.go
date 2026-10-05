package store

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"fmt"
	"slices"

	"go.kenn.io/msgvault/internal/mime"
)

// ReconcilePstEmailThreadsContext joins email conversations in one source.
// Two conversations join when their messages share a recorded PST thread key,
// when a conversation's key names a message in the other, or when a message's
// recorded parent is a unique, live message in the other. Joins only merge:
// messages move to the lowest conversation ID in each group, and the emptied
// conversations stay in place. It reads every email in the source, because a
// newly imported message can connect conversations from earlier imports.
func (s *Store) ReconcilePstEmailThreadsContext(ctx context.Context, sourceID int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := s.requireSyncSource(sourceID); err != nil {
		return err
	}
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		groups, err := loadPstThreadGroups(ctx, tx, sourceID)
		if err != nil {
			return err
		}
		facts, err := loadPstThreadFacts(ctx, tx, sourceID, groups)
		if err != nil {
			return err
		}
		facts.join(groups)
		return s.applyPstThreadGroups(ctx, tx, sourceID, groups)
	})
}

// pstThreadGroups is a union-find over one source's email conversations.
type pstThreadGroups struct {
	parents map[int64]int64
	keys    map[string]int64
}

func (g *pstThreadGroups) find(id int64) int64 {
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

func (g *pstThreadGroups) join(a, b int64) {
	a, b = g.find(a), g.find(b)
	if a > b {
		a, b = b, a
	}
	g.parents[b] = a
}

func loadPstThreadGroups(ctx context.Context, tx *loggedTx, sourceID int64) (*pstThreadGroups, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id, source_conversation_id FROM conversations
   WHERE source_id = ? AND conversation_type = 'email_thread' ORDER BY id`, sourceID)
	if err != nil {
		return nil, fmt.Errorf("list PST conversations: %w", err)
	}
	defer func() { _ = rows.Close() }()
	groups := &pstThreadGroups{parents: make(map[int64]int64), keys: make(map[string]int64)}
	for rows.Next() {
		var id int64
		var key sql.NullString
		if err := rows.Scan(&id, &key); err != nil {
			return nil, fmt.Errorf("scan PST conversation: %w", err)
		}
		groups.parents[id] = id
		if key := mime.NormalizeMessageID(key.String); key != "" {
			groups.keys[key] = id
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list PST conversations: %w", err)
	}
	return groups, rows.Close()
}

type pstIdentity struct {
	conversation int64
	count        int
}

type pstKeyedConversation struct {
	conversation int64
	key          string
}

// pstThreadFacts holds the message facts that connect conversations.
type pstThreadFacts struct {
	identities map[string]pstIdentity
	parents    []pstKeyedConversation
	threadKeys []pstKeyedConversation
}

type pstThreadMetadata struct {
	Parent    string `json:"email_in_reply_to"`
	ThreadKey string `json:"pst_thread_key"`
}

func loadPstThreadFacts(
	ctx context.Context, tx *loggedTx, sourceID int64, groups *pstThreadGroups,
) (*pstThreadFacts, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id, conversation_id, rfc822_message_id, metadata FROM messages
   WHERE source_id = ? AND message_type = 'email' AND deleted_at IS NULL`, sourceID)
	if err != nil {
		return nil, fmt.Errorf("list PST thread facts: %w", err)
	}
	defer func() { _ = rows.Close() }()
	facts := &pstThreadFacts{identities: make(map[string]pstIdentity)}
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var id, conv int64
		var rfcID, metadata sql.NullString
		if err := rows.Scan(&id, &conv, &rfcID, &metadata); err != nil {
			return nil, fmt.Errorf("scan PST thread facts: %w", err)
		}
		if _, ok := groups.parents[conv]; !ok {
			continue
		}
		if key := mime.NormalizeMessageID(rfcID.String); key != "" {
			identity := facts.identities[key]
			identity.count++
			identity.conversation = conv
			facts.identities[key] = identity
		}
		if !metadata.Valid || metadata.String == "" {
			continue
		}
		var fields pstThreadMetadata
		if err := json.Unmarshal([]byte(metadata.String), &fields); err != nil {
			return nil, fmt.Errorf("decode PST thread metadata for message %d: %w", id, err)
		}
		if key := mime.NormalizeMessageID(fields.Parent); key != "" {
			facts.parents = append(facts.parents, pstKeyedConversation{conversation: conv, key: key})
		}
		if key := mime.NormalizeMessageID(fields.ThreadKey); key != "" {
			facts.threadKeys = append(facts.threadKeys, pstKeyedConversation{conversation: conv, key: key})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list PST thread facts: %w", err)
	}
	return facts, rows.Close()
}

func (f *pstThreadFacts) join(groups *pstThreadGroups) {
	// Messages that share a thread key belong together even when no
	// conversation or archived message carries that key, such as a References
	// root that is missing from the archive.
	byThreadKey := make(map[string]int64)
	for _, fact := range f.threadKeys {
		if target, ok := groups.keys[fact.key]; ok {
			groups.join(fact.conversation, target)
		}
		if first, ok := byThreadKey[fact.key]; ok {
			groups.join(fact.conversation, first)
		} else {
			byThreadKey[fact.key] = fact.conversation
		}
	}
	// Do not use reply_to_message_id blindly: dedup may redirect it across
	// sources. Only current unique canonical identity facts connect aliases.
	for key, identity := range f.identities {
		if target, ok := groups.keys[key]; ok && identity.count == 1 {
			groups.join(target, identity.conversation)
		}
	}
	// References can name an ancestor absent from the archive. An accepted,
	// uniquely resolved direct parent still connects that reply chain.
	for _, parent := range f.parents {
		if identity, ok := f.identities[parent.key]; ok && identity.count == 1 {
			groups.join(parent.conversation, identity.conversation)
		}
	}
}

func (s *Store) applyPstThreadGroups(ctx context.Context, tx *loggedTx, sourceID int64, groups *pstThreadGroups) error {
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
   WHERE source_id = ? AND conversation_id = ? AND message_type = 'email' AND deleted_at IS NULL`,
			target, sourceID, id)
		if err != nil {
			return fmt.Errorf("move PST conversation: %w", err)
		}
		count, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("count moved PST messages: %w", err)
		}
		if count > 0 {
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
	return s.bumpDerivedDataRevision(tx)
}
