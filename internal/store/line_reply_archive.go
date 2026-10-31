package store

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
)

var errLineReplyAlreadySent = errors.New("LINE reply already archived")

// CompleteLineReplyContext publishes the accepted message, receipt, and draft
// removal in one transaction through the ordinary message/FTS persistence path.
// No policy, secret or retry-window check belongs to this local-only repair.
func (s *Store) CompleteLineReplyContext(ctx context.Context, id string) (LineReplyAttempt, error) {
	a, err := s.GetLineReplyAttemptContext(ctx, id)
	if err != nil || a.State == LineReplySent {
		return a, err
	}
	if a.State != LineReplyRemoteAccepted || a.RemoteMessageID == "" {
		return a, ErrLineReplyInvalidState
	}
	if err := s.requireSyncSource(a.SourceID); err != nil {
		return a, err
	}
	var sender int64
	var senderName string
	var metadata sql.NullString
	var raw []byte
	before := func(ctx context.Context, tx *loggedTx) error {
		if err := s.lockSyncSourceTx(ctx, tx, a.SourceID); err != nil {
			return err
		}
		if err := lockChatDraftTx(ctx, tx, id); err != nil {
			return err
		}
		current, err := loadLineReply(ctx, tx, id)
		if err != nil {
			return err
		}
		a = current
		if a.State == LineReplySent {
			return errLineReplyAlreadySent
		}
		if a.State != LineReplyRemoteAccepted || a.RemoteMessageID == "" {
			return ErrLineReplyInvalidState
		}
		if err := validateLineReplyBinding(ctx, tx, a, false); err != nil {
			return err
		}
		// Persistence already holds the shared identity lock. Resolve the registered
		// identifier here so a participant merge cannot leave a stale numeric ID.
		identifier := "channel:" + a.ChannelID + ":user:" + a.BotUserID
		if err := tx.QueryRowContext(ctx, `SELECT participant_id FROM participant_identifiers WHERE identifier_type='line_user_id' AND identifier_value=?`, identifier).Scan(&sender); err != nil {
			return fmt.Errorf("resolve LINE reply bot: %w", err)
		}
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(display_name,'') FROM participants WHERE id=?`, sender).Scan(&senderName); err != nil {
			return err
		}
		if err := validateExistingLineReply(ctx, tx, a, sender); err != nil {
			return err
		}
		fields, err := json.Marshal(struct {
			QuoteToken      string `json:"quoteToken,omitempty"`
			QuotedMessageID string `json:"quotedMessageId"`
		}{QuoteToken: a.RemoteQuoteToken, QuotedMessageID: a.ReplyToSourceMessageID})
		if err != nil {
			return errors.New("encode LINE reply metadata")
		}
		metadata = sql.NullString{String: string(fields), Valid: true}
		raw, err = json.Marshal(struct {
			StatusCode   int    `json:"status_code"`
			RequestID    string `json:"request_id"`
			SentMessages []struct {
				ID         string `json:"id"`
				QuoteToken string `json:"quoteToken,omitempty"`
			} `json:"sentMessages"`
		}{StatusCode: a.StatusCode, RequestID: a.RequestID, SentMessages: []struct {
			ID         string `json:"id"`
			QuoteToken string `json:"quoteToken,omitempty"`
		}{{ID: a.RemoteMessageID, QuoteToken: a.RemoteQuoteToken}}})
		if err != nil {
			return errors.New("encode LINE push receipt")
		}
		return nil
	}
	build := func([]int64) *MessagePersistData {
		snippet := []rune(a.Body)
		if len(snippet) > 200 {
			snippet = snippet[:200]
		}
		return &MessagePersistData{
			Message: &Message{SourceID: a.SourceID, ConversationID: a.ConversationID, SourceMessageID: a.RemoteMessageID,
				MessageType: lineSourceType, SenderID: sql.NullInt64{Int64: sender, Valid: true}, IsFromMe: true,
				ReplyToMessageID: sql.NullInt64{Int64: a.ReplyToMessageID, Valid: true}, SentAt: sql.NullTime{Time: a.AdmittedAt, Valid: true},
				Snippet: sql.NullString{String: string(snippet), Valid: true}, SizeEstimate: int64(len(a.Body)), PreserveAttachmentStats: true},
			BodyText: sql.NullString{String: a.Body, Valid: true}, Metadata: &metadata, RawMIME: raw, RawFormat: "line_push_receipt_json",
			Recipients:     []RecipientSet{{Type: messageSenderRole, ParticipantIDs: []int64{sender}, DisplayNames: []string{senderName}}},
			PreserveLabels: true, FTS: &FTSDoc{Body: a.Body, FromAddr: senderName},
		}
	}
	after := func(ctx context.Context, tx *loggedTx, _ *MessagePersistData, messageID int64) error {
		if err := s.recomputeConversationStatsWith(boundQuerier{ctx: ctx, q: tx}, "id = ?", a.ConversationID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM chat_drafts WHERE draft_id=? AND revision=?`, a.DraftID, a.Revision); err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, `UPDATE line_reply_attempts SET state='sent',message_id=? WHERE draft_id=? AND state='remote_accepted' AND remote_message_id=?`, messageID, a.DraftID, a.RemoteMessageID)
		if err != nil {
			return err
		}
		count, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if count != 1 {
			return ErrLineReplyInvalidState
		}
		a.State, a.MessageID = LineReplySent, messageID
		return nil
	}
	_, err = s.persistMessageWithParticipantsTransaction(ctx, attributionLock{}, before, nil, build, nil, after)
	if errors.Is(err, errLineReplyAlreadySent) {
		return a, nil
	}
	if err != nil && a.State == LineReplySent {
		// The transaction may have rolled back at COMMIT. Only a fresh read
		// can prove that a local message ID and the sent receipt are durable.
		a.State, a.MessageID = LineReplyRemoteAccepted, 0
	}
	return a, err
}

// An existing native ID must already describe this same sent reply. Never
// overwrite an inbound message or a different conversation with a bad receipt.
func validateExistingLineReply(ctx context.Context, tx *loggedTx, a LineReplyAttempt, senderID int64) error {
	var id, conversation, reply int64
	var sender sql.NullInt64
	var fromMe bool
	err := tx.QueryRowContext(ctx, `SELECT id,conversation_id,COALESCE(reply_to_message_id,0),sender_id,is_from_me FROM messages WHERE source_id=? AND source_message_id=?`, a.SourceID, a.RemoteMessageID).Scan(&id, &conversation, &reply, &sender, &fromMe)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if conversation != a.ConversationID || reply != a.ReplyToMessageID || !fromMe || !sender.Valid || sender.Int64 != senderID {
		return ErrLineReplyReceiptConflict
	}
	var body sql.NullString
	err = tx.QueryRowContext(ctx, `SELECT body_text FROM message_bodies WHERE message_id=?`, id).Scan(&body)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrLineReplyReceiptConflict
		}
		return err
	}
	if !body.Valid || body.String != a.Body {
		return ErrLineReplyReceiptConflict
	}
	return nil
}
