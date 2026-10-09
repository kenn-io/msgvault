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
