package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// ChatDraftIDPrefix marks draft IDs owned by the local chat draft table.
const ChatDraftIDPrefix = "chat-draft-"

// ChatDraft is unsent text kept in msgvault for one archived Slack, Teams, or
// Discord conversation. Native keys are copied at creation so a later mapping
// repair cannot retarget it.
type ChatDraft struct {
	DraftID                string
	SourceID               int64
	SourceType             string
	SourceIdentifier       string
	ConversationID         int64
	SourceConversationID   string
	ConversationType       string
	ReplyToSourceMessageID string
	Body                   string
	Revision               int64
}

// ChatDraftAuthorizer vets a conversation's source before its drafts are
// written or listed.
type ChatDraftAuthorizer func(sourceType, identifier string) error

var (
	ErrChatDraftNotFound           = errors.New("chat draft not found")
	ErrChatDraftRevisionConflict   = errors.New("chat draft revision conflict")
	ErrChatDraftInvalidDestination = errors.New("invalid chat draft destination")
	ErrChatDraftUnsupportedSource  = errors.New("unsupported chat draft source")
)

const chatDraftSelect = `
	SELECT d.draft_id, d.source_id, s.source_type, s.identifier, d.conversation_id,
	       d.source_conversation_id, d.conversation_type,
	       COALESCE(d.reply_to_source_message_id, ''), d.body, d.revision
	FROM chat_drafts d JOIN sources s ON s.id = d.source_id
`

type chatDraftDestination struct {
	sourceID         int64
	sourceType       string
	identifier       string
	nativeID         sql.NullString
	conversationType sql.NullString
}

func loadChatDraftDestination(
	ctx context.Context, q contextRowQuerier, conversationID int64, authorize ChatDraftAuthorizer,
) (chatDraftDestination, error) {
	var dest chatDraftDestination
	err := q.QueryRowContext(ctx, `
		SELECT c.source_id, s.source_type, s.identifier, c.source_conversation_id, c.conversation_type
		FROM conversations c JOIN sources s ON s.id = c.source_id
		WHERE c.id = ?
	`, conversationID).Scan(&dest.sourceID, &dest.sourceType, &dest.identifier, &dest.nativeID, &dest.conversationType)
	if errors.Is(err, sql.ErrNoRows) {
		return dest, fmt.Errorf("conversation %d: %w", conversationID, ErrChatDraftInvalidDestination)
	}
	if err != nil {
		return dest, fmt.Errorf("read chat draft conversation: %w", err)
	}
	if err := authorize(dest.sourceType, dest.identifier); err != nil {
		return dest, err
	}
	switch dest.sourceType {
	case "slack", "slackdump", "teams", "discord":
	default:
		return dest, fmt.Errorf("source type %q: %w", dest.sourceType, ErrChatDraftUnsupportedSource)
	}
	return dest, nil
}

// CreateChatDraftContext stores unsent text for an archived conversation
// without writing an archive message or contacting a provider.
func (s *Store) CreateChatDraftContext(
	ctx context.Context, conversationID, replyToMessageID int64, body string, authorize ChatDraftAuthorizer,
) (ChatDraft, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return ChatDraft{}, fmt.Errorf("generate chat draft ID: %w", err)
	}
	draftID := ChatDraftIDPrefix + hex.EncodeToString(raw[:])
	var draft ChatDraft
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		dest, err := loadChatDraftDestination(ctx, tx, conversationID, authorize)
		if err != nil {
			return err
		}
		if strings.TrimSpace(dest.nativeID.String) == "" || strings.TrimSpace(dest.conversationType.String) == "" {
			return fmt.Errorf("conversation %d has no source key: %w", conversationID, ErrChatDraftInvalidDestination)
		}
		var reply sql.NullString
		if replyToMessageID != 0 {
			err := tx.QueryRowContext(ctx, `
				SELECT source_message_id FROM messages WHERE id = ? AND conversation_id = ?
			`, replyToMessageID, conversationID).Scan(&reply)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("read chat draft reply target: %w", err)
			}
			if strings.TrimSpace(reply.String) == "" {
				return fmt.Errorf("reply target %d: %w", replyToMessageID, ErrChatDraftInvalidDestination)
			}
		}
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(`
			INSERT INTO chat_drafts (
				draft_id, source_id, conversation_id, source_conversation_id,
				conversation_type, reply_to_source_message_id, body, revision,
				created_at, updated_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, 1, %s, %s)
		`, s.dialect.ContentChangedNow(), s.dialect.Now()), draftID, dest.sourceID, conversationID,
			dest.nativeID.String, dest.conversationType.String, reply, body); err != nil {
			return fmt.Errorf("insert chat draft: %w", err)
		}
		draft, err = loadChatDraft(ctx, tx, draftID)
		return err
	})
	return draft, err
}

// ListChatDraftsContext returns a conversation's local drafts, oldest first,
// so a lost draft ID can be found again.
func (s *Store) ListChatDraftsContext(
	ctx context.Context, conversationID int64, authorize ChatDraftAuthorizer,
) ([]ChatDraft, error) {
	if _, err := loadChatDraftDestination(ctx, s.db, conversationID, authorize); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, chatDraftSelect+`
		WHERE d.conversation_id = ? ORDER BY d.created_at, d.draft_id
	`, conversationID)
	if err != nil {
		return nil, fmt.Errorf("list chat drafts: %w", err)
	}
	defer func() { _ = rows.Close() }()
	drafts := []ChatDraft{}
	for rows.Next() {
		draft, err := scanChatDraft(rows)
		if err != nil {
			return nil, fmt.Errorf("read chat draft: %w", err)
		}
		drafts = append(drafts, draft)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list chat drafts: %w", err)
	}
	return drafts, nil
}

// GetChatDraftContext reads one local draft.
func (s *Store) GetChatDraftContext(ctx context.Context, draftID string) (ChatDraft, error) {
	return loadChatDraft(ctx, s.db, draftID)
}

// UpdateChatDraftContext replaces the body when expectedRevision is current.
func (s *Store) UpdateChatDraftContext(
	ctx context.Context, draftID string, expectedRevision int64, body string,
) (ChatDraft, error) {
	var draft ChatDraft
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		result, err := tx.ExecContext(ctx, fmt.Sprintf(`
			UPDATE chat_drafts SET body = ?, revision = revision + 1, updated_at = %s
			WHERE draft_id = ? AND revision = ?
		`, s.dialect.Now()), body, draftID, expectedRevision)
		if err := chatDraftWriteResult(ctx, tx, draftID, result, err); err != nil {
			return err
		}
		draft, err = loadChatDraft(ctx, tx, draftID)
		return err
	})
	return draft, err
}

// DeleteChatDraftContext removes the draft when expectedRevision is current.
func (s *Store) DeleteChatDraftContext(ctx context.Context, draftID string, expectedRevision int64) error {
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		result, err := tx.ExecContext(ctx, `
			DELETE FROM chat_drafts WHERE draft_id = ? AND revision = ?
		`, draftID, expectedRevision)
		return chatDraftWriteResult(ctx, tx, draftID, result, err)
	})
}

// chatDraftWriteResult tells a missing draft from a stale revision after a
// conditional write matched no row.
func chatDraftWriteResult(ctx context.Context, tx *loggedTx, draftID string, result sql.Result, err error) error {
	if err != nil {
		return fmt.Errorf("write chat draft: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("check chat draft write: %w", err)
	}
	if rows > 0 {
		return nil
	}
	if _, err := loadChatDraft(ctx, tx, draftID); err != nil {
		return err
	}
	return fmt.Errorf("draft %q: %w", draftID, ErrChatDraftRevisionConflict)
}

func scanChatDraft(row scanner) (ChatDraft, error) {
	var draft ChatDraft
	err := row.Scan(
		&draft.DraftID, &draft.SourceID, &draft.SourceType, &draft.SourceIdentifier,
		&draft.ConversationID, &draft.SourceConversationID, &draft.ConversationType,
		&draft.ReplyToSourceMessageID, &draft.Body, &draft.Revision,
	)
	return draft, err
}

func loadChatDraft(ctx context.Context, q contextRowQuerier, draftID string) (ChatDraft, error) {
	draft, err := scanChatDraft(q.QueryRowContext(ctx, chatDraftSelect+`WHERE d.draft_id = ?`, draftID))
	if errors.Is(err, sql.ErrNoRows) {
		return ChatDraft{}, fmt.Errorf("draft %q: %w", draftID, ErrChatDraftNotFound)
	}
	if err != nil {
		return ChatDraft{}, fmt.Errorf("load chat draft %q: %w", draftID, err)
	}
	return draft, nil
}
