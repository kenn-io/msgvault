package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
)

const chatDraftLocation = "msgvault"

// ChatDraft is a local, unsent text draft addressed to one archived chat
// conversation. The destination fields are snapshots from creation time.
type ChatDraft struct {
	DraftID                string
	SourceID               int64
	ConversationID         int64
	SourceConversationID   string
	ConversationType       string
	ReplyToSourceMessageID string
	Body                   string
	Revision               int64
	CreatedAt              time.Time
	UpdatedAt              time.Time
	Location               string
}

// ChatDraftCreate identifies an existing source and conversation. Native
// provider keys are read from the Store and cannot be supplied by callers.
type ChatDraftCreate struct {
	SourceID         int64
	SourceType       string
	SourceIdentifier string
	ConversationID   int64
	ReplyToMessageID int64
	Body             string
}

var (
	ErrChatDraftNotFound           = errors.New("chat draft not found")
	ErrChatDraftRevisionConflict   = errors.New("chat draft revision conflict")
	ErrChatDraftInvalidDestination = errors.New("invalid chat draft destination")
	ErrChatDraftUnsupportedSource  = errors.New("unsupported chat draft source")
	ErrChatDraftInvalidInput       = errors.New("invalid chat draft input")
)

// CreateChatDraftContext stores unsent text without creating an archive
// message or making a provider request.
func (s *Store) CreateChatDraftContext(ctx context.Context, input ChatDraftCreate) (ChatDraft, error) {
	if err := validateChatDraftCreate(input); err != nil {
		return ChatDraft{}, err
	}
	draftID, err := newChatDraftID()
	if err != nil {
		return ChatDraft{}, err
	}
	var draft ChatDraft
	err = s.withTxContext(ctx, func(tx *loggedTx) error {
		var sourceType, sourceIdentifier string
		if err := tx.QueryRowContext(ctx, `
			SELECT source_type, identifier
			FROM sources
			WHERE id = ?
		`, input.SourceID).Scan(&sourceType, &sourceIdentifier); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("source %d: %w", input.SourceID, ErrChatDraftInvalidDestination)
			}
			return fmt.Errorf("read chat draft source: %w", err)
		}
		if sourceType != input.SourceType || sourceIdentifier != input.SourceIdentifier {
			return fmt.Errorf("source snapshot does not match: %w", ErrChatDraftInvalidDestination)
		}

		var sourceConversationID, conversationType sql.NullString
		if err := tx.QueryRowContext(ctx, `
			SELECT source_conversation_id, conversation_type
			FROM conversations
			WHERE id = ? AND source_id = ?
		`, input.ConversationID, input.SourceID).Scan(&sourceConversationID, &conversationType); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("conversation %d: %w", input.ConversationID, ErrChatDraftInvalidDestination)
			}
			return fmt.Errorf("read chat draft conversation: %w", err)
		}
		if !sourceConversationID.Valid || !conversationType.Valid ||
			strings.TrimSpace(sourceConversationID.String) == "" || strings.TrimSpace(conversationType.String) == "" {
			return fmt.Errorf("conversation %d has no native destination: %w", input.ConversationID, ErrChatDraftInvalidDestination)
		}

		replyToSourceMessageID := sql.NullString{}
		if input.ReplyToMessageID != 0 {
			if err := tx.QueryRowContext(ctx, `
				SELECT source_message_id
				FROM messages
				WHERE id = ? AND source_id = ? AND conversation_id = ?
			`, input.ReplyToMessageID, input.SourceID, input.ConversationID).Scan(&replyToSourceMessageID); err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return fmt.Errorf("reply target %d: %w", input.ReplyToMessageID, ErrChatDraftInvalidDestination)
				}
				return fmt.Errorf("read chat draft reply target: %w", err)
			}
			if !replyToSourceMessageID.Valid || strings.TrimSpace(replyToSourceMessageID.String) == "" {
				return fmt.Errorf("reply target %d has no native key: %w", input.ReplyToMessageID, ErrChatDraftInvalidDestination)
			}
		}

		if _, err := tx.ExecContext(ctx, fmt.Sprintf(`
			INSERT INTO chat_drafts (
				draft_id, source_id, conversation_id, source_conversation_id,
				conversation_type, reply_to_source_message_id, body, revision,
				created_at, updated_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, 1, %s, %s)
		`, s.dialect.Now(), s.dialect.Now()), draftID, input.SourceID,
			input.ConversationID, sourceConversationID.String, conversationType.String,
			replyToSourceMessageID, input.Body); err != nil {
			return fmt.Errorf("insert chat draft: %w", err)
		}
		var err error
		draft, err = loadChatDraft(ctx, tx, draftID)
		return err
	})
	if err != nil {
		return ChatDraft{}, err
	}
	return draft, nil
}

// GetChatDraftContext reads one local draft without reading message bodies or
// contacting a provider.
func (s *Store) GetChatDraftContext(ctx context.Context, draftID string) (ChatDraft, error) {
	if err := validateChatDraftID(draftID); err != nil {
		return ChatDraft{}, err
	}
	return loadChatDraft(ctx, s.db, draftID)
}

// UpdateChatDraftContext replaces the local body when the expected revision
// still owns the row.
func (s *Store) UpdateChatDraftContext(
	ctx context.Context, draftID string, expectedRevision int64, body string,
) (ChatDraft, error) {
	if err := validateChatDraftID(draftID); err != nil {
		return ChatDraft{}, err
	}
	if expectedRevision <= 0 {
		return ChatDraft{}, fmt.Errorf("revision must be positive: %w", ErrChatDraftInvalidInput)
	}
	if expectedRevision == math.MaxInt64 {
		return ChatDraft{}, fmt.Errorf("revision cannot advance: %w", ErrChatDraftRevisionConflict)
	}
	var draft ChatDraft
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		result, err := tx.ExecContext(ctx, fmt.Sprintf(`
			UPDATE chat_drafts
			SET body = ?, revision = ?, updated_at = %s
			WHERE draft_id = ? AND revision = ?
		`, s.dialect.Now()), body, expectedRevision+1, draftID, expectedRevision)
		if err != nil {
			return fmt.Errorf("update chat draft: %w", err)
		}
		rows, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("check chat draft update: %w", err)
		}
		if rows == 0 {
			return chatDraftWriteMiss(ctx, tx, draftID, ErrChatDraftRevisionConflict)
		}
		draft, err = loadChatDraft(ctx, tx, draftID)
		return err
	})
	if err != nil {
		return ChatDraft{}, err
	}
	return draft, nil
}

// DeleteChatDraftContext removes a local draft when the expected revision
// still owns the row.
func (s *Store) DeleteChatDraftContext(
	ctx context.Context, draftID string, expectedRevision int64,
) error {
	if err := validateChatDraftID(draftID); err != nil {
		return err
	}
	if expectedRevision <= 0 {
		return fmt.Errorf("revision must be positive: %w", ErrChatDraftInvalidInput)
	}
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		result, err := tx.ExecContext(ctx, `
			DELETE FROM chat_drafts
			WHERE draft_id = ? AND revision = ?
		`, draftID, expectedRevision)
		if err != nil {
			return fmt.Errorf("delete chat draft: %w", err)
		}
		rows, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("check chat draft delete: %w", err)
		}
		if rows == 0 {
			return chatDraftWriteMiss(ctx, tx, draftID, ErrChatDraftRevisionConflict)
		}
		return nil
	})
}

func validateChatDraftCreate(input ChatDraftCreate) error {
	if input.SourceID <= 0 || input.ConversationID <= 0 ||
		strings.TrimSpace(input.SourceType) == "" || strings.TrimSpace(input.SourceIdentifier) == "" {
		return fmt.Errorf("source and conversation are required: %w", ErrChatDraftInvalidInput)
	}
	if input.ReplyToMessageID < 0 {
		return fmt.Errorf("reply target must be positive: %w", ErrChatDraftInvalidInput)
	}
	switch input.SourceType {
	case "slack", "teams", "discord":
		return nil
	default:
		return fmt.Errorf("source type %q: %w", input.SourceType, ErrChatDraftUnsupportedSource)
	}
}

func validateChatDraftID(draftID string) error {
	if strings.TrimSpace(draftID) == "" || strings.ContainsAny(draftID, "\x00\r\n") {
		return fmt.Errorf("draft ID is invalid: %w", ErrChatDraftInvalidInput)
	}
	return nil
}

func newChatDraftID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate chat draft ID: %w", err)
	}
	return "chat-draft-" + hex.EncodeToString(raw[:]), nil
}

func chatDraftWriteMiss(
	ctx context.Context, tx *loggedTx, draftID string, conflict error,
) error {
	var exists int
	err := tx.QueryRowContext(ctx, `SELECT 1 FROM chat_drafts WHERE draft_id = ?`, draftID).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("draft %q: %w", draftID, ErrChatDraftNotFound)
	}
	if err != nil {
		return fmt.Errorf("check chat draft after conditional write: %w", err)
	}
	return fmt.Errorf("draft %q: %w", draftID, conflict)
}

func loadChatDraft(ctx context.Context, q contextRowQuerier, draftID string) (ChatDraft, error) {
	var (
		draft     ChatDraft
		reply     sql.NullString
		createdAt nullableTimestamp
		updatedAt nullableTimestamp
	)
	err := q.QueryRowContext(ctx, `
		SELECT draft_id, source_id, conversation_id, source_conversation_id,
		       conversation_type, reply_to_source_message_id, body, revision,
		       created_at, updated_at
		FROM chat_drafts
		WHERE draft_id = ?
	`, draftID).Scan(
		&draft.DraftID, &draft.SourceID, &draft.ConversationID,
		&draft.SourceConversationID, &draft.ConversationType, &reply,
		&draft.Body, &draft.Revision, &createdAt, &updatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return ChatDraft{}, fmt.Errorf("draft %q: %w", draftID, ErrChatDraftNotFound)
	}
	if err != nil {
		return ChatDraft{}, fmt.Errorf("load chat draft %q: %w", draftID, err)
	}
	if reply.Valid {
		draft.ReplyToSourceMessageID = reply.String
	}
	if createdAt.Valid {
		draft.CreatedAt = createdAt.Time
	}
	if updatedAt.Valid {
		draft.UpdatedAt = updatedAt.Time
	}
	draft.Location = chatDraftLocation
	return draft, nil
}
