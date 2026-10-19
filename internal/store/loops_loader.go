package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"

	"go.kenn.io/msgvault/internal/looplimits"
)

// LoopConversationAnchorExistsContext distinguishes an older window gap from
// an anchor deleted or moved out of the conversation being reconciled.
func (s *Store) LoopConversationAnchorExistsContext(ctx context.Context, conversationID, messageID int64) (bool, error) {
	var live bool
	err := s.db.QueryRowContext(ctx, `SELECT messages.deleted_at IS NULL AND messages.deleted_from_source_at IS NULL
		AND NOT (`+loopMessageExcludedSQL("messages", "sources")+`)
		FROM messages JOIN sources ON sources.id=messages.source_id
		WHERE messages.id=? AND messages.conversation_id=?`, messageID, conversationID).Scan(&live)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return live, err
}

func loopMessageExcludedSQL(message, source string) string {
	return `(` + draftEvidenceSQL(message, source) + `
		OR ` + message + `.message_type='synctech_sms_call'
		OR (` + source + `.source_type='synctech_sms'
			AND ` + message + `.message_type IN ('sms','mms')
			AND ` + message + `.is_sent=FALSE))`
}

func loopCategoryLabelIDFromSystemRole(role string) string {
	switch role {
	case labelSystemRoleGmailCategoryPersonal:
		return "CATEGORY_PERSONAL"
	case labelSystemRoleGmailCategorySocial:
		return "CATEGORY_SOCIAL"
	case labelSystemRoleGmailCategoryPromotions:
		return "CATEGORY_PROMOTIONS"
	case labelSystemRoleGmailCategoryUpdates:
		return "CATEGORY_UPDATES"
	case labelSystemRoleGmailCategoryForums:
		return "CATEGORY_FORUMS"
	default:
		return ""
	}
}

func (s *Store) LoopMessageBodyContext(ctx context.Context, messageID int64) (string, error) {
	var body sql.NullString
	err := s.db.QueryRowContext(ctx, "SELECT body_text FROM message_bodies WHERE message_id=?", messageID).Scan(&body)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return body.String, err
}

// LoadLoopConversationContext reads a bounded newest metadata window and returns it chronologically.
func (s *Store) LoadLoopConversationContext(ctx context.Context, conversationID int64, limit int) (LoopConversation, error) {
	var out LoopConversation
	if conversationID <= 0 || limit < 1 || limit > looplimits.MaxConversationMessageLimit {
		return out, fmt.Errorf("%w: conversation and limit required", ErrInvalidLoop)
	}
	err := s.withReadSnapshotContext(ctx, func(tx *loggedTx) error {
		var last nullableTimestamp
		err := tx.QueryRowContext(ctx, `SELECT c.id,c.source_id,s.source_type,c.conversation_type,
(SELECT MAX(COALESCE(m.sent_at,m.received_at,m.internal_date)) FROM messages m JOIN sources src ON src.id=m.source_id
WHERE m.conversation_id=c.id AND COALESCE(m.sent_at,m.received_at,m.internal_date) IS NOT NULL AND NOT (`+loopMessageExcludedSQL("m", "src")+`))
FROM conversations c JOIN sources s ON s.id=c.source_id WHERE c.id=?`, conversationID).Scan(&out.ConversationID, &out.SourceID, &out.SourceType, &out.ConversationType, &last)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrLoopNotFound
		}
		if err != nil {
			return err
		}
		if last.Valid {
			out.LastMessageAt = last.Time
		}
		rows, err := tx.QueryContext(ctx, `SELECT m.id,m.source_id,m.conversation_id,m.message_type,m.sent_at,m.received_at,m.internal_date,COALESCE(m.source_is_from_me,FALSE),m.deleted_at,m.deleted_from_source_at,m.sender_id,COALESCE(m.subject,''),COALESCE(m.snippet,''),COALESCE(m.list_id,''),`+draftEvidenceSQL("m", "src")+` FROM messages m JOIN sources src ON src.id=m.source_id
WHERE m.conversation_id=? AND COALESCE(m.sent_at,m.received_at,m.internal_date) IS NOT NULL AND NOT (`+loopMessageExcludedSQL("m", "src")+`)
ORDER BY COALESCE(m.sent_at,m.received_at,m.internal_date) DESC NULLS LAST,m.id DESC LIMIT ?`, conversationID, limit+1)
		if err != nil {
			return err
		}
		var candidates []ActivityCandidate
		var senderIDs []*int64
		for rows.Next() {
			var m LoopMessage
			var c ActivityCandidate
			var sent, received, internal, deleted, sourceDeleted sql.NullTime
			var sender *int64
			if err := rows.Scan(&m.ID, &c.SourceID, &c.ConversationID, &c.MessageType, &sent, &received, &internal, &c.SourceIsFromMe, &deleted, &sourceDeleted, &sender, &m.Subject, &m.Snippet, &m.ListID, &m.Draft); err != nil {
				_ = rows.Close()
				return err
			}
			c.MessageID = m.ID
			c.ConversationType = out.ConversationType
			c.SentAt = activityTimePointer(sent)
			c.ReceivedAt = activityTimePointer(received)
			c.InternalDate = activityTimePointer(internal)
			m.Deleted = deleted.Valid || sourceDeleted.Valid
			for _, at := range []*sql.NullTime{&sent, &received, &internal} {
				if at.Valid && !at.Time.IsZero() {
					m.OccurredAt = at.Time
					break
				}
			}
			candidates = append(candidates, c)
			out.Messages = append(out.Messages, m)
			senderIDs = append(senderIDs, sender)
		}
		err = rows.Err()
		_ = rows.Close()
		if err != nil {
			return err
		}
		out.HistoryComplete = len(out.Messages) <= limit
		if !out.HistoryComplete {
			out.Messages = out.Messages[:limit]
			candidates = candidates[:limit]
			senderIDs = senderIDs[:limit]
		}
		candidates, err = s.attachActivityCounterpartsQueryerContext(ctx, tx, candidates)
		if err != nil {
			return err
		}
		// Hydrate every counterpart, including departed members, plus sender metadata.
		byID := map[int64]LoopParticipant{}
		participantMetadataIDs := `SELECT participant_id FROM conversation_participants WHERE conversation_id=?
UNION SELECT sender_id FROM messages WHERE conversation_id=? AND sender_id IS NOT NULL
UNION SELECT mr.participant_id FROM message_recipients mr JOIN messages m ON m.id=mr.message_id WHERE m.conversation_id=?`
		rows, err = tx.QueryContext(ctx, `SELECT p.id,pp.person_id,COALESCE(p.email_address,''),COALESCE(p.phone_number,''),COALESCE(p.display_name,''),COALESCE(p.canonical_id,'') FROM (`+participantMetadataIDs+`) participant_ids JOIN participants p ON p.id=participant_ids.participant_id LEFT JOIN person_participants pp ON pp.participant_id=p.id ORDER BY p.id`, conversationID, conversationID, conversationID)
		if err != nil {
			return err
		}
		for rows.Next() {
			var p LoopParticipant
			if err := rows.Scan(&p.ParticipantID, &p.PersonID, &p.EmailAddress, &p.PhoneNumber, &p.DisplayName, &p.CanonicalID); err != nil {
				_ = rows.Close()
				return err
			}
			byID[p.ParticipantID] = p
		}
		err = rows.Err()
		_ = rows.Close()
		if err != nil {
			return err
		}
		for i, c := range candidates {
			class := ClassifyActivityCandidate(c, DefaultMaxDirectActivityCounterparts)
			out.Messages[i].Direction = class.Direction
			out.Messages[i].OwnerAddress = class.OwnerAddress
			if senderIDs[i] != nil {
				out.Messages[i].Sender = byID[*senderIDs[i]]
			}
			for _, cp := range c.Counterparts {
				p := byID[cp.ParticipantID]
				p.PersonID = cp.PersonID
				p.IsOwner = cp.IsOwner
				if cp.IsOwner {
					aggregate := byID[cp.ParticipantID]
					aggregate.IsOwner = true
					byID[cp.ParticipantID] = aggregate
				}
				if cp.RecipientType == meetingSenderRole {
					if senderIDs[i] == nil || *senderIDs[i] == p.ParticipantID {
						out.Messages[i].Sender = p
					}
					continue
				}
				out.Messages[i].Recipients = append(out.Messages[i].Recipients, LoopRecipient{LoopParticipant: p, RecipientType: cp.RecipientType})
			}
		}
		// Group eligibility uses current roster members and unlisted senders. A sender
		// with a roster row that has left must not remain a current counterpart. Email
		// recipients remain participants even when the conversation also has a roster.
		participantIDs := `SELECT participant_id FROM conversation_participants WHERE conversation_id=? AND left_at IS NULL

UNION SELECT m.sender_id FROM messages m WHERE m.conversation_id=? AND m.sender_id IS NOT NULL
AND NOT EXISTS (SELECT 1 FROM conversation_participants cp WHERE cp.conversation_id=m.conversation_id AND cp.participant_id=m.sender_id)`
		participantArgs := []any{conversationID, conversationID}
		if out.ConversationType == "email_thread" {
			participantIDs += ` UNION SELECT mr.participant_id FROM message_recipients mr JOIN messages m ON m.id=mr.message_id WHERE m.conversation_id=?`
			participantArgs = append(participantArgs, conversationID)
		}
		rows, err = tx.QueryContext(ctx, `SELECT participant_id FROM (`+participantIDs+`) loop_participant_ids ORDER BY participant_id`, participantArgs...)
		if err != nil {
			return err
		}
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				_ = rows.Close()
				return err
			}
			out.Participants = append(out.Participants, byID[id])
		}
		err = rows.Err()
		_ = rows.Close()
		if err != nil {
			return err
		}
		if len(out.Messages) > 0 {
			ids := make([]any, len(out.Messages))
			positions := map[int64]int{}
			for i, m := range out.Messages {
				ids[i] = m.ID
				positions[m.ID] = i
			}
			placeholders := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
			labelArgs := append(append([]any(nil), ids...),
				labelSystemRoleGmailCategoryPersonal, labelSystemRoleGmailCategorySocial,
				labelSystemRoleGmailCategoryPromotions, labelSystemRoleGmailCategoryUpdates,
				labelSystemRoleGmailCategoryForums,
			)
			rows, err = tx.QueryContext(ctx, `SELECT ml.message_id,l.source_label_id,l.system_role FROM message_labels ml JOIN labels l ON l.id=ml.label_id WHERE ml.message_id IN (`+placeholders+`) AND (l.source_label_id IS NOT NULL OR l.system_role IN (?,?,?,?,?)) ORDER BY ml.message_id,COALESCE(l.source_label_id,l.system_role,'')`, labelArgs...)
			if err != nil {
				return err
			}
			for rows.Next() {
				var id int64
				var providerLabelID, systemRole sql.NullString
				if err := rows.Scan(&id, &providerLabelID, &systemRole); err != nil {
					_ = rows.Close()
					return err
				}
				label := providerLabelID.String
				if !providerLabelID.Valid {
					label = loopCategoryLabelIDFromSystemRole(systemRole.String)
				}
				if label == "" {
					continue
				}
				i := positions[id]
				out.Messages[i].Labels = append(out.Messages[i].Labels, label)
			}
			err = rows.Err()
			_ = rows.Close()
			if err != nil {
				return err
			}
			rows, err = tx.QueryContext(ctx, `SELECT message_id, email_address FROM message_recipients
				WHERE message_id IN (`+placeholders+`) AND LOWER(TRIM(recipient_type)) = 'from'
				AND TRIM(COALESCE(email_address, '')) <> '' ORDER BY message_id, id`, ids...)
			if err != nil {
				return err
			}
			seenFrom := make(map[int64]bool)
			for rows.Next() {
				var id int64
				var address string
				if err := rows.Scan(&id, &address); err != nil {
					_ = rows.Close()
					return err
				}
				if seenFrom[id] {
					continue
				}
				seenFrom[id] = true
				out.Messages[positions[id]].Sender.EmailAddress = strings.TrimSpace(address)
			}
			err = rows.Err()
			_ = rows.Close()
			if err != nil {
				return err
			}
			rows, err = tx.QueryContext(ctx, `SELECT message_id,auto_submitted,precedence_bulk,list_unsubscribe,calendar_part,reply_to_noreply,parser_version FROM message_automation_signals WHERE message_id IN (`+placeholders+`)`, ids...)
			if err != nil {
				return err
			}
			for rows.Next() {
				var id int64
				var signals MessageAutomationSignals
				if err := rows.Scan(&id, &signals.AutoSubmitted, &signals.PrecedenceBulk, &signals.ListUnsubscribe, &signals.CalendarPart, &signals.ReplyToNoReply, &signals.ParserVersion); err != nil {
					_ = rows.Close()
					return err
				}
				out.Messages[positions[id]].Signals = &signals
			}
			err = rows.Err()
			_ = rows.Close()
			if err != nil {
				return err
			}
		}
		slices.Reverse(out.Messages)
		return nil
	})
	if err != nil {
		return LoopConversation{}, fmt.Errorf("load loop conversation: %w", err)
	}
	return out, nil
}
