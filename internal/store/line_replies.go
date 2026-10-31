package store

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

const lineSourceType = "line"

const (
	LineReplyAdmitted                  = "admitted"
	LineReplyRemoteUnknown             = "remote_unknown"
	LineReplyAcceptedUnresolved        = "accepted_unresolved"
	LineReplyRemoteAccepted            = "remote_accepted"
	LineReplySent                      = "sent"
	LineReplyProviderRejected          = "provider_rejected"
	LineReplyRemoteUnknownExpired      = "remote_unknown_expired"
	LineReplyAcceptedUnresolvedExpired = "accepted_unresolved_expired"
)

var (
	ErrChatDraftFrozen          = errors.New("chat draft has an admitted send")
	ErrLineReplyRetryWait       = errors.New("LINE reply retry is not due")
	ErrLineReplyExpired         = errors.New("LINE reply retry window expired")
	ErrLineReplyInvalidState    = errors.New("invalid LINE reply attempt state")
	ErrLineReplyReceiptConflict = errors.New("LINE reply receipt conflicts with archive evidence")
)

// LineReplyAttempt owns the immutable request and its durable acceptance
// evidence. It survives draft deletion; source removal deliberately purges it.
type LineReplyAttempt struct {
	DraftID                string
	SourceID               int64
	ChannelID              string
	BotUserID              string
	Revision               int64
	ConversationID         int64
	SourceConversationID   string
	ConversationType       string
	ReplyToMessageID       int64
	ReplyToSourceMessageID string
	Body                   string
	QuoteToken             string
	RetryKey               string
	State                  string
	AdmittedAt             time.Time
	RetryUntil             time.Time
	NextRetryAt            time.Time
	StatusCode             int
	RequestID              string
	RemoteMessageID        string
	RemoteQuoteToken       string
	MessageID              int64
}

// LineReplyAuthorizer must be pure: check local policy/text and prepare a
// client without provider I/O or nested Store calls. Failure leaves no attempt.
type LineReplyAuthorizer func(LineReplyAttempt) error

const lineReplyColumns = `draft_id,source_id,channel_id,bot_user_id,revision,
 conversation_id,source_conversation_id,conversation_type,reply_to_message_id,
 reply_to_source_message_id,body,quote_token,retry_key,state,admitted_at,retry_until,
 next_retry_at,status_code,request_id,remote_message_id,remote_quote_token,message_id`

func loadLineReply(ctx context.Context, q contextRowQuerier, id string) (LineReplyAttempt, error) {
	var a LineReplyAttempt
	err := q.QueryRowContext(ctx, `SELECT `+lineReplyColumns+` FROM line_reply_attempts WHERE draft_id=?`, id).Scan(
		&a.DraftID, &a.SourceID, &a.ChannelID, &a.BotUserID, &a.Revision, &a.ConversationID,
		&a.SourceConversationID, &a.ConversationType, &a.ReplyToMessageID, &a.ReplyToSourceMessageID,
		&a.Body, &a.QuoteToken, &a.RetryKey, &a.State, &a.AdmittedAt, &a.RetryUntil, &a.NextRetryAt,
		&a.StatusCode, &a.RequestID, &a.RemoteMessageID, &a.RemoteQuoteToken, &a.MessageID,
	)
	if err != nil {
		return a, fmt.Errorf("read LINE reply attempt: %w", err)
	}
	a.AdmittedAt = a.AdmittedAt.UTC()
	a.RetryUntil = a.RetryUntil.UTC()
	a.NextRetryAt = a.NextRetryAt.UTC()
	return a, nil
}

func (s *Store) GetLineReplyAttemptContext(ctx context.Context, id string) (LineReplyAttempt, error) {
	return loadLineReply(ctx, s.db, id)
}

func lineReplyRevision(a LineReplyAttempt, revision int64) (LineReplyAttempt, error) {
	if a.Revision != revision {
		return a, ErrChatDraftRevisionConflict
	}
	return a, nil
}

// AdmitLineReplyContext locks the reviewed draft, authorizes its exact frozen
// snapshot, and commits its UUID before any provider request is possible.
func (s *Store) AdmitLineReplyContext(ctx context.Context, id string, revision int64, now time.Time, authorize LineReplyAuthorizer) (LineReplyAttempt, error) {
	if a, err := s.GetLineReplyAttemptContext(ctx, id); err == nil {
		return lineReplyRevision(a, revision)
	} else if !errors.Is(err, sql.ErrNoRows) {
		return a, err
	}
	draft, err := s.GetChatDraftContext(ctx, id)
	if err != nil {
		return LineReplyAttempt{}, err
	}
	var a LineReplyAttempt
	err = s.withTxContext(ctx, func(tx *loggedTx) error {
		var err error
		a, err = s.admitLineReplyTx(ctx, tx, draft.SourceID, id, revision, now, authorize)
		return err
	})
	return a, err
}

// Source-before-draft ordering agrees with maintenance and source retirement.
func (s *Store) admitLineReplyTx(ctx context.Context, tx *loggedTx, sourceID int64, id string, revision int64, now time.Time, authorize LineReplyAuthorizer) (LineReplyAttempt, error) {
	var a LineReplyAttempt
	if err := s.requireSyncSource(sourceID); err != nil {
		return a, err
	}
	if err := s.lockSyncSourceTx(ctx, tx, sourceID); err != nil {
		return a, err
	}
	if err := requireSourceWritableWith(boundQuerier{ctx: ctx, q: tx}, sourceID); err != nil {
		return a, err
	}
	if err := lockChatDraftTx(ctx, tx, id); err != nil {
		return a, err
	}
	if prior, err := loadLineReply(ctx, tx, id); err == nil {
		return lineReplyRevision(prior, revision)
	} else if !errors.Is(err, sql.ErrNoRows) {
		return a, err
	}
	draft, err := loadChatDraft(ctx, tx, id)
	if err != nil {
		return a, err
	}
	if draft.Revision != revision {
		return a, ErrChatDraftRevisionConflict
	}
	if draft.SourceType != lineSourceType {
		return a, ErrChatDraftUnsupportedSource
	}
	if draft.SourceID != sourceID {
		return a, ErrChatDraftInvalidDestination
	}
	binding, err := readLineReplyBinding(ctx, tx, draft.SourceID, draft.ConversationID, draft.ReplyToSourceMessageID, true)
	if err != nil {
		return a, err
	}
	if binding.nativeID != draft.SourceConversationID || binding.kind != draft.ConversationType {
		return a, ErrChatDraftInvalidDestination
	}
	var parentMetadata struct {
		QuoteToken string `json:"quoteToken"`
	}
	if binding.metadata.Valid && binding.metadata.String != "" {
		if json.Unmarshal([]byte(binding.metadata.String), &parentMetadata) != nil {
			return a, ErrChatDraftInvalidDestination
		}
	}
	key, err := uuid.NewRandom()
	if err != nil {
		return a, fmt.Errorf("generate LINE reply retry UUID: %w", err)
	}
	now = now.UTC().Truncate(time.Microsecond)
	a = LineReplyAttempt{DraftID: id, SourceID: sourceID, ChannelID: binding.channelID, BotUserID: binding.botID,
		Revision: revision, ConversationID: draft.ConversationID, SourceConversationID: draft.SourceConversationID,
		ConversationType: draft.ConversationType, ReplyToMessageID: binding.parentID,
		ReplyToSourceMessageID: draft.ReplyToSourceMessageID, Body: draft.Body, QuoteToken: parentMetadata.QuoteToken,
		RetryKey: key.String(), State: LineReplyAdmitted, AdmittedAt: now, RetryUntil: now.Add(23 * time.Hour), NextRetryAt: now}
	if authorize == nil {
		return LineReplyAttempt{}, errors.New("LINE reply admission requires an authorizer")
	}
	if err := authorize(a); err != nil {
		return LineReplyAttempt{}, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO line_reply_attempts
 (draft_id,source_id,channel_id,bot_user_id,revision,conversation_id,source_conversation_id,
 conversation_type,reply_to_message_id,reply_to_source_message_id,body,quote_token,retry_key,
 state,admitted_at,retry_until,next_retry_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		a.DraftID, a.SourceID, a.ChannelID, a.BotUserID, a.Revision, a.ConversationID, a.SourceConversationID,
		a.ConversationType, a.ReplyToMessageID, a.ReplyToSourceMessageID, a.Body, a.QuoteToken, a.RetryKey,
		a.State, a.AdmittedAt, a.RetryUntil, a.NextRetryAt)
	if err != nil {
		return LineReplyAttempt{}, fmt.Errorf("admit LINE reply: %w", err)
	}
	return a, nil
}

// A separate statement is essential on PostgreSQL: a waiting UPDATE with a
// cross-table NOT EXISTS still has its pre-admission statement snapshot.
func lockChatDraftTx(ctx context.Context, tx *loggedTx, id string) error {
	_, err := tx.ExecContext(ctx, `UPDATE chat_drafts SET revision=revision WHERE draft_id=?`, id)
	if err != nil {
		return fmt.Errorf("lock chat draft: %w", err)
	}
	return nil
}

type lineReplyBinding struct {
	channelID, botID, nativeID, kind string
	parentID                         int64
	metadata                         sql.NullString
}

func readLineReplyBinding(ctx context.Context, q contextRowQuerier, sourceID, conversationID int64, parentNativeID string, checkBot bool) (lineReplyBinding, error) {
	var b lineReplyBinding
	if parentNativeID == "" {
		return b, ErrChatDraftInvalidDestination
	}
	var sourceType string
	var raw sql.NullString
	err := q.QueryRowContext(ctx, `SELECT source_type,identifier,sync_config FROM sources WHERE id=?`, sourceID).Scan(&sourceType, &b.channelID, &raw)
	if err != nil {
		return b, fmt.Errorf("read LINE reply source: %w", err)
	}
	if sourceType != lineSourceType {
		return b, ErrChatDraftInvalidDestination
	}
	if checkBot {
		var settings struct {
			BotUserID string `json:"line_bot_user_id"`
		}
		if !raw.Valid || json.Unmarshal([]byte(raw.String), &settings) != nil || settings.BotUserID == "" {
			return b, ErrChatDraftInvalidDestination
		}
		b.botID = settings.BotUserID
	}
	err = q.QueryRowContext(ctx, `SELECT source_conversation_id,conversation_type FROM conversations WHERE id=? AND source_id=?`, conversationID, sourceID).Scan(&b.nativeID, &b.kind)
	if errors.Is(err, sql.ErrNoRows) {
		return b, ErrChatDraftInvalidDestination
	}
	if err != nil {
		return b, fmt.Errorf("read LINE reply conversation: %w", err)
	}
	err = q.QueryRowContext(ctx, `SELECT id,metadata FROM messages WHERE source_id=? AND conversation_id=? AND source_message_id=?`, sourceID, conversationID, parentNativeID).Scan(&b.parentID, &b.metadata)
	if errors.Is(err, sql.ErrNoRows) {
		return b, ErrChatDraftInvalidDestination
	}
	if err != nil {
		return b, fmt.Errorf("read LINE reply parent: %w", err)
	}

	return b, nil
}

func validateLineReplyBinding(ctx context.Context, q contextStatementQuerier, a LineReplyAttempt, checkBot bool) error {
	if err := requireSourceWritableWith(boundQuerier{ctx: ctx, q: q}, a.SourceID); err != nil {
		return err
	}
	b, err := readLineReplyBinding(ctx, q, a.SourceID, a.ConversationID, a.ReplyToSourceMessageID, checkBot)
	if err != nil {
		return err
	}
	if b.channelID != a.ChannelID || b.nativeID != a.SourceConversationID || b.kind != a.ConversationType || b.parentID != a.ReplyToMessageID || checkBot && b.botID != a.BotUserID {
		return ErrChatDraftInvalidDestination
	}
	return nil
}

// ValidateLineReplyContext rechecks destination/identity before provider I/O.
// Parent quote metadata may change; the admitted request keeps its old token.
func (s *Store) ValidateLineReplyContext(ctx context.Context, id string) error {
	a, err := s.GetLineReplyAttemptContext(ctx, id)
	if err != nil {
		return err
	}
	if err := s.requireSyncSource(a.SourceID); err != nil {
		return err
	}
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		if err := s.lockSyncSourceTx(ctx, tx, a.SourceID); err != nil {
			return err
		}
		return validateLineReplyBinding(ctx, tx, a, true)
	})
}
