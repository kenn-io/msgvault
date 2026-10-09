package store

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"fmt"
	"strconv"
	"time"
)

func (s *Store) appendArchivedMessageTx(ctx context.Context, tx *loggedTx, id int64) error {
	if !s.captureMCPEnabled() || s.ingestContext().Mode != IngestLive {
		return nil
	}
	var source, conversation int64
	var fromMe bool
	var sent sql.NullTime
	var archived time.Time
	if err := tx.QueryRowContext(ctx, `SELECT source_id, conversation_id, is_from_me, sent_at, archived_at FROM messages WHERE id = ?`, id).Scan(&source, &conversation, &fromMe, &sent, &archived); err != nil {
		return fmt.Errorf("read ready message: %w", err)
	}
	var sentAt any
	occurred := archived
	if sent.Valid {
		sentAt = sent.Time.UTC().Format(time.RFC3339Nano)
		occurred = sent.Time
	}
	data, err := json.Marshal(map[string]any{"kind": "message", "message_id": strconv.FormatInt(id, 10), "conversation_id": strconv.FormatInt(conversation, 10), "source_id": strconv.FormatInt(source, 10), "from_me": fromMe, "sent_at": sentAt, "archived_at": archived.UTC().Format(time.RFC3339Nano)})
	if err != nil {
		return err
	}
	return s.appendMCPEventTx(ctx, tx, MCPEvent{Family: "msgvault.message_archived", Kind: "message", ScopeKind: "conversation", ScopeID: conversation, SourceID: source, ConversationID: conversation, MessageID: id, ItemKey: fmt.Sprintf("message:%d", id), FromMe: fromMe, OccurredAt: occurred, Data: data})
}

type reactionIdentity struct {
	ParticipantID int64
	Type, Value   string
}

func (s *Store) appendReactionTx(ctx context.Context, tx *loggedTx, id int64, r ReactionRef) error {
	if !s.captureMCPEnabled() || s.ingestContext().Mode != IngestLive {
		return nil
	}
	var source, conversation int64
	var fromMe bool
	predicate := senderOwnerFallback("?", "?")
	if err := tx.QueryRowContext(ctx, `SELECT source_id, conversation_id FROM messages WHERE id = ?`, id).Scan(&source, &conversation); err != nil {
		return err
	}
	// Resolve against this message's source, never the target sender's identity.
	if err := tx.QueryRowContext(ctx, `SELECT (`+predicate+`)`, r.ParticipantID, source, r.ParticipantID, source).Scan(&fromMe); err != nil {
		return err
	}
	reacted := r.CreatedAt
	if reacted.IsZero() {
		reacted = s.ingestContext().ObservedAt
	}
	if reacted.IsZero() {
		reacted = time.Now().UTC()
	}
	key, err := json.Marshal([]any{id, r.ParticipantID, r.Type, r.Value})
	if err != nil {
		return err
	}
	data, err := json.Marshal(map[string]any{"kind": "reaction", "target_message_id": strconv.FormatInt(id, 10), "reaction_key": r.Value, "reactor_participant_id": strconv.FormatInt(r.ParticipantID, 10), "reacted_at": reacted.UTC().Format(time.RFC3339Nano), "conversation_id": strconv.FormatInt(conversation, 10), "source_id": strconv.FormatInt(source, 10), "from_me": fromMe})
	if err != nil {
		return err
	}
	return s.appendMCPEventTx(ctx, tx, MCPEvent{Family: "msgvault.message_archived", Kind: "reaction", ScopeKind: "conversation", ScopeID: conversation, ConversationID: conversation, SourceID: source, MessageID: id, ItemKey: "reaction:" + string(key), FromMe: fromMe, OccurredAt: reacted, Data: data})
}
