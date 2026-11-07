package store

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strings"

	"go.kenn.io/msgvault/internal/lineidentity"
	"go.kenn.io/msgvault/internal/textutil"
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
	if userID, direct := strings.CutPrefix(a.SourceConversationID, "user:"); direct {
		if _, err := s.EnsureParticipantByIdentifierContext(ctx, "line_user_id", lineidentity.UserIdentifier(a.ChannelID, userID), ""); err != nil {
			return a, fmt.Errorf("resolve LINE reply recipient: %w", err)
		}
	}
	var sender, recipient, parentID int64
	var senderName, recipientName string
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
		parentID, err = validateLineReplyBinding(ctx, tx, a, false)
		if err != nil {
			return err
		}
		var quotedMessageID string
		if a.QuoteToken != "" {
			quotedMessageID = a.ReplyToSourceMessageID
		} else {
			parentID = 0
		}
		// Persistence already holds the shared identity lock. Resolve the registered
		// identifier here so a participant merge cannot leave a stale numeric ID.
		identifier := lineidentity.UserIdentifier(a.ChannelID, a.BotUserID)
		if err := tx.QueryRowContext(ctx, `SELECT participant_id FROM participant_identifiers WHERE identifier_type='line_user_id' AND identifier_value=?`, identifier).Scan(&sender); err != nil {
			return fmt.Errorf("resolve LINE reply bot: %w", err)
		}
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(display_name,'') FROM participants WHERE id=?`, sender).Scan(&senderName); err != nil {
			return err
		}
		if userID, direct := strings.CutPrefix(a.SourceConversationID, "user:"); direct {
			identifier := lineidentity.UserIdentifier(a.ChannelID, userID)
			if err := tx.QueryRowContext(ctx, `SELECT p.id,COALESCE(p.display_name,'') FROM participant_identifiers i JOIN participants p ON p.id=i.participant_id WHERE i.identifier_type='line_user_id' AND i.identifier_value=?`, identifier).Scan(&recipient, &recipientName); err != nil {
				return fmt.Errorf("resolve LINE reply recipient: %w", err)
			}
		}
		if err := validateExistingLineReply(ctx, tx, a, sender, parentID); err != nil {
			return err
		}
		fields, err := json.Marshal(struct {
			QuoteToken      string `json:"quoteToken,omitempty"`
			QuotedMessageID string `json:"quotedMessageId,omitempty"`
		}{QuoteToken: a.RemoteQuoteToken, QuotedMessageID: quotedMessageID})
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
		_, err = s.setLineMembershipClockTx(ctx, tx, a.SourceID, a.SourceConversationID, lineidentity.UserIdentifier(a.ChannelID, a.BotUserID), true, a.AdmittedAt)
		return err
	}
	build := func([]int64) *MessagePersistData {
		recipients := []RecipientSet{{Type: meetingSenderRole, ParticipantIDs: []int64{sender}, DisplayNames: []string{senderName}}}
		if recipient != 0 {
			recipients = append(recipients, RecipientSet{Type: "to", ParticipantIDs: []int64{recipient}, DisplayNames: []string{recipientName}})
		}
		return &MessagePersistData{
			Message: &Message{SourceID: a.SourceID, ConversationID: a.ConversationID, SourceMessageID: a.RemoteMessageID,
				MessageType: lineSourceType, SenderID: sql.NullInt64{Int64: sender, Valid: true}, IsFromMe: true,
				ReplyToMessageID: sql.NullInt64{Int64: parentID, Valid: parentID != 0 && a.QuoteToken != ""}, SentAt: sql.NullTime{Time: a.AdmittedAt, Valid: true},
				Snippet: sql.NullString{String: textutil.PrefixRunes(a.Body, 200), Valid: true}, SizeEstimate: int64(len(a.Body)), PreserveAttachmentStats: true},
			BodyText: sql.NullString{String: a.Body, Valid: true}, Metadata: &metadata, RawMIME: raw, RawFormat: "line_push_receipt_json",
			Recipients:     recipients,
			PreserveLabels: true, FTS: &FTSDoc{Body: a.Body},
		}
	}
	after := func(ctx context.Context, tx *loggedTx, _ *MessagePersistData, messageID int64) error {
		if err := s.projectLineMembershipTx(ctx, tx, a.SourceID, a.ConversationID, sender, lineidentity.UserIdentifier(a.ChannelID, a.BotUserID)); err != nil {
			return err
		}
		if err := s.recomputeConversationStatsWith(boundQuerier{ctx: ctx, q: tx}, "id = ?", a.ConversationID); err != nil {
			return err
		}
		if err := s.resolveLineQuotesForMessages(tx, a.SourceID, a.SourceConversationID, []string{a.RemoteMessageID}); err != nil {
			return fmt.Errorf("resolve LINE replies quoting archived send: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM chat_drafts WHERE draft_id=? AND revision=?`, a.DraftID, a.Revision); err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, `UPDATE line_reply_attempts SET state='sent',message_id=?,body='',quote_token='',remote_quote_token='',source_conversation_id='' WHERE draft_id=? AND state='remote_accepted' AND remote_message_id=?`, messageID, a.DraftID, a.RemoteMessageID)
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
	if err == nil {
		a.Body, a.QuoteToken, a.RemoteQuoteToken, a.SourceConversationID = "", "", "", ""
	}
	return a, err
}

// An existing native ID must already describe this same sent reply. Never
// overwrite an inbound message or a different conversation with a bad receipt.
func validateExistingLineReply(ctx context.Context, tx *loggedTx, a LineReplyAttempt, senderID, parentID int64) error {
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
	if conversation != a.ConversationID || reply != parentID || !fromMe || !sender.Valid || sender.Int64 != senderID {
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
