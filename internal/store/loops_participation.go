package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"go.kenn.io/msgvault/internal/mime"
)

// CheckLoopOwnerParticipationContext revalidates the message that previously
// established owner participation for a live reply loop. A missing or
// disqualified message is known invalid; stale MIME evidence remains unknown
// until its signal backfill has run.
func (s *Store) CheckLoopOwnerParticipationContext(
	ctx context.Context,
	conversationID, anchorMessageID, participationMessageID int64,
) (valid, known bool, err error) {
	if conversationID <= 0 {
		return false, true, fmt.Errorf("%w: conversation id is required", ErrInvalidLoop)
	}
	err = s.withReadSnapshotContext(ctx, func(tx *loggedTx) error {
		var anchorSentAt, anchorReceivedAt, anchorInternalDate sql.NullTime
		err := tx.QueryRowContext(ctx, `SELECT sent_at,received_at,internal_date
			FROM messages WHERE id=? AND conversation_id=?`, anchorMessageID, conversationID).Scan(
			&anchorSentAt, &anchorReceivedAt, &anchorInternalDate,
		)
		if errors.Is(err, sql.ErrNoRows) {
			known = true
			return nil
		}
		if err != nil {
			return fmt.Errorf("load loop anchor ordering metadata: %w", err)
		}
		anchorAt := firstLoopActivityTime(anchorSentAt, anchorReceivedAt, anchorInternalDate)
		if !anchorAt.Valid {
			return nil
		}

		var candidate ActivityCandidate
		var conversationIDValue int64
		var deletedAt, deletedFromSourceAt sql.NullTime
		var participationSentAt, participationReceivedAt, participationInternalDate sql.NullTime
		var subject string
		var excluded, rawMIME bool
		var autoSubmitted, calendarPart, replyToNoReply sql.NullBool
		var parserVersion sql.NullInt64
		err = tx.QueryRowContext(ctx, `SELECT m.id,m.source_id,m.conversation_id,c.conversation_type,
			m.message_type,COALESCE(m.source_is_from_me,FALSE),
			m.sent_at,m.received_at,m.internal_date,m.deleted_at,m.deleted_from_source_at,
			COALESCE(m.subject,''),`+loopMessageExcludedSQL("m", "src")+`,
			EXISTS(SELECT 1 FROM message_raw r WHERE r.message_id=m.id AND r.raw_format='mime'),
			mas.auto_submitted,mas.calendar_part,mas.reply_to_noreply,mas.parser_version
			FROM messages m JOIN sources src ON src.id=m.source_id
			JOIN conversations c ON c.id=m.conversation_id
			LEFT JOIN message_automation_signals mas ON mas.message_id=m.id
			WHERE m.id=? AND m.conversation_id=?`, participationMessageID, conversationID).Scan(
			&candidate.MessageID, &candidate.SourceID, &conversationIDValue, &candidate.ConversationType,
			&candidate.MessageType, &candidate.SourceIsFromMe, &participationSentAt,
			&participationReceivedAt, &participationInternalDate, &deletedAt,
			&deletedFromSourceAt, &subject, &excluded, &rawMIME, &autoSubmitted,
			&calendarPart, &replyToNoReply, &parserVersion,
		)
		if errors.Is(err, sql.ErrNoRows) {
			known = true
			return nil
		}
		if err != nil {
			return fmt.Errorf("load saved loop owner participation: %w", err)
		}
		candidate.SentAt = activityTimePointer(participationSentAt)
		candidate.ReceivedAt = activityTimePointer(participationReceivedAt)
		candidate.InternalDate = activityTimePointer(participationInternalDate)
		participationAt := firstLoopActivityTime(participationSentAt, participationReceivedAt, participationInternalDate)
		if deletedAt.Valid || deletedFromSourceAt.Valid || excluded {
			known = true
			return nil
		}
		if (autoSubmitted.Valid && autoSubmitted.Bool) || (calendarPart.Valid && calendarPart.Bool) || (replyToNoReply.Valid && replyToNoReply.Bool) || loopParticipationCalendarSubject(subject) {
			known = true
			return nil
		}

		conversationIDCopy := conversationIDValue
		candidate.ConversationID = &conversationIDCopy
		candidates, err := s.attachActivityCounterpartsQueryerContext(ctx, tx, []ActivityCandidate{candidate})
		if err != nil {
			return fmt.Errorf("resolve current loop owner attribution: %w", err)
		}
		if ClassifyActivityCandidate(candidates[0], DefaultMaxDirectActivityCounterparts).Direction != DirectionOutbound {
			known = true
			return nil
		}
		if rawMIME && (!parserVersion.Valid || parserVersion.Int64 < mime.AutomationParserVersion) {
			return nil
		}
		if !participationAt.Valid {
			return nil
		}
		if participationAt.Time.After(anchorAt.Time) || (participationAt.Time.Equal(anchorAt.Time) && participationMessageID >= anchorMessageID) {
			known = true
			return nil
		}
		valid, known = true, true
		return nil
	})
	if err != nil {
		return false, false, err
	}
	return valid, known, nil
}

func firstLoopActivityTime(values ...sql.NullTime) sql.NullTime {
	for _, value := range values {
		if value.Valid && !value.Time.IsZero() {
			return value
		}
	}
	return sql.NullTime{}
}

func loopParticipationCalendarSubject(subject string) bool {
	subject = strings.ToLower(strings.TrimSpace(subject))
	for _, prefix := range []string{
		"invitation:", "updated invitation:", "accepted:", "declined:",
		"tentatively accepted:", "canceled event:", "cancelled event:",
	} {
		if strings.HasPrefix(subject, prefix) {
			return true
		}
	}
	return false
}
