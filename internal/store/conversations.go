package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ConversationWindow is a bounded, stable slice of full message details.
type ConversationWindow struct {
	Messages       []APIMessage
	AnchorPosition int64
	Total          int64
}

// ConversationInlineBodyBudget caps the cumulative body content (text + HTML,
// measured as SQL LENGTH — characters, a lower bound on UTF-8 bytes) inlined
// into a single conversation window. The anchor's body is always inlined;
// further messages are inlined in chronological order while they fit. Messages
// beyond the budget are returned with BodyOmitted set and no body so a thread
// of many large messages cannot exhaust daemon or client memory; callers fetch
// those bodies individually via the single-message lookup.
const ConversationInlineBodyBudget = 4 << 20

// ErrConversationAnchorOutsideRange is returned by
// GetConversationWindowContext when the anchor message exists in the
// conversation but falls outside the requested [start, end) time bounds.
// Callers should surface this as a client error rather than the generic
// "anchor not found" case, since the anchor is valid but the requested
// window excludes it.
var ErrConversationAnchorOutsideRange = errors.New("conversation anchor outside range")

// ConversationExists reports whether the containing conversation is present,
// independent of whether it currently has visible messages.
func (s *Store) ConversationExists(conversationID int64) (bool, error) {
	return s.ConversationExistsContext(context.Background(), conversationID)
}

// ConversationExistsContext is the context-aware existence check.
func (s *Store) ConversationExistsContext(ctx context.Context, conversationID int64) (bool, error) {
	var exists bool
	err := s.db.QueryRowContext(ctx, s.dialect.Rebind(`
		SELECT EXISTS (SELECT 1 FROM conversations WHERE id = ?)
	`), conversationID).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("check conversation: %w", err)
	}
	return exists, nil
}

// conversationRangeClause builds the optional time-bound SQL fragment for the
// conversation window's ordered CTE, plus the matching bind args in the order
// they must appear (start before end). Either bound may be nil to leave that
// side unrestricted. The returned clause is empty when both bounds are nil.
func conversationRangeClause(start, end *time.Time) (clause string, args []any) {
	if start != nil {
		clause += " AND COALESCE(m.sent_at, m.received_at, m.internal_date) >= ?"
		args = append(args, *start)
	}
	if end != nil {
		clause += " AND COALESCE(m.sent_at, m.received_at, m.internal_date) < ?"
		args = append(args, *end)
	}
	return clause, args
}

// anchorTimestampInRangeContext reports whether anchorID belongs to
// conversationID (exists) and, if so, whether its timestamp
// (COALESCE(sent_at, received_at, internal_date)) falls within [start, end).
// A nil bound leaves that side unrestricted. exists is false when the anchor
// is not a live message of the conversation, mirroring the visibility rules
// GetConversationWindowContext itself applies.
func (s *Store) anchorTimestampInRangeContext(
	ctx context.Context, conversationID, anchorID int64, start, end *time.Time,
) (inRange, exists bool, err error) {
	query := fmt.Sprintf(`
		SELECT COALESCE(m.sent_at, m.received_at, m.internal_date)
		FROM messages m
		WHERE m.conversation_id = ? AND m.id = ? AND %s
	`, LiveMessagesWhere("m", false))

	var ts nullableTimestamp
	scanErr := s.db.QueryRowContext(ctx, s.dialect.Rebind(query), conversationID, anchorID).Scan(&ts)
	if errors.Is(scanErr, sql.ErrNoRows) {
		return false, false, nil
	}
	if scanErr != nil {
		return false, false, fmt.Errorf("check conversation anchor range: %w", scanErr)
	}
	if !ts.Valid {
		return false, true, nil
	}
	afterStart := start == nil || !ts.Time.Before(*start)
	beforeEnd := end == nil || ts.Time.Before(*end)
	return afterStart && beforeEnd, true, nil
}

// GetConversationWindow returns full details around anchor in chronological
// (sent timestamp, message ID) order. before and after are caller-validated
// bounds; the selected anchor is always included.
func (s *Store) GetConversationWindow(
	conversationID, anchorID int64, before, after int,
) (*ConversationWindow, error) {
	return s.GetConversationWindowContext(context.Background(), conversationID, anchorID, before, after, nil, nil)
}

// GetConversationWindowContext is the context-aware conversation reader.
// start and end optionally bound the window to messages with a timestamp
// (COALESCE(sent_at, received_at, internal_date)) in the half-open range
// [start, end); either or both may be nil to leave that side unbounded.
// When bounds are set, position numbering, total_count, and anchor_position
// are all computed over the bounded subset only, so before/after and the
// caller-derived HasBefore/HasAfter are relative to the range rather than
// the full conversation. If the anchor exists in the conversation but its
// timestamp falls outside the bounds, ErrConversationAnchorOutsideRange is
// returned instead of an empty window.
func (s *Store) GetConversationWindowContext(
	ctx context.Context,
	conversationID, anchorID int64,
	before, after int,
	start, end *time.Time,
) (*ConversationWindow, error) {
	if start != nil || end != nil {
		inRange, exists, err := s.anchorTimestampInRangeContext(ctx, conversationID, anchorID, start, end)
		if err != nil {
			return nil, err
		}
		if exists && !inRange {
			return nil, ErrConversationAnchorOutsideRange
		}
	}

	rangeClause, rangeArgs := conversationRangeClause(start, end)
	query := fmt.Sprintf(`
		WITH ordered AS (
			SELECT
				m.id,
				ROW_NUMBER() OVER (
					ORDER BY COALESCE(m.sent_at, m.received_at, m.internal_date) ASC, m.id ASC
				) AS position,
				COUNT(*) OVER () AS total_count
			FROM messages m
			WHERE m.conversation_id = ? AND %s%s
		), anchor AS (
			SELECT position AS anchor_position FROM ordered WHERE id = ?
		), selected AS (
			SELECT ordered.id, ordered.position, ordered.total_count, anchor.anchor_position
			FROM ordered CROSS JOIN anchor
			WHERE ordered.position BETWEEN anchor.anchor_position - ? AND anchor.anchor_position + ?
		)
		SELECT
			m.id,
			m.source_id,
			COALESCE(m.source_message_id, ''),
			COALESCE(m.conversation_id, 0),
			COALESCE(c.source_conversation_id, ''),
			COALESCE(m.subject, ''),
			COALESCE(m.message_type, ''),
			%s,
			COALESCE(m.sent_at, m.received_at, m.internal_date),
			COALESCE(m.snippet, ''),
			m.has_attachments,
			COALESCE(m.size_estimate, 0),
			m.deleted_from_source_at,
			COALESCE(LENGTH(mb.body_text), 0) + COALESCE(LENGTH(mb.body_html), 0),
			selected.position,
			selected.total_count,
			selected.anchor_position
		FROM selected
		JOIN messages m ON m.id = selected.id
		LEFT JOIN message_recipients mr ON mr.id = (
			SELECT mr2.id FROM message_recipients mr2
			WHERE mr2.message_id = m.id AND mr2.recipient_type = 'from'
			ORDER BY mr2.id LIMIT 1
		)
		LEFT JOIN participants p ON p.id = COALESCE(m.sender_id, mr.participant_id)
		LEFT JOIN conversations c ON c.id = m.conversation_id
		LEFT JOIN message_bodies mb ON mb.message_id = m.id
		ORDER BY selected.position ASC
	`, LiveMessagesWhere("m", false), rangeClause, participantSummarySenderSQL)

	args := make([]any, 0, 4+len(rangeArgs))
	args = append(args, conversationID)
	args = append(args, rangeArgs...)
	args = append(args, anchorID, before, after)

	rows, err := s.db.QueryContext(ctx, s.dialect.Rebind(query), args...)
	if err != nil {
		return nil, fmt.Errorf("get conversation window: %w", err)
	}
	defer func() { _ = rows.Close() }()

	window := &ConversationWindow{Messages: []APIMessage{}}
	ids := make([]int64, 0, before+after+1)
	bodySizes := make([]int64, 0, before+after+1)
	for rows.Next() {
		var message APIMessage
		var sentAt, deletedAt nullableTimestamp
		var position, bodySize int64
		if err := rows.Scan(
			&message.ID,
			&message.SourceID,
			&message.SourceMessageID,
			&message.ConversationID,
			&message.SourceConversationID,
			&message.Subject,
			&message.MessageType,
			&message.From,
			&message.FromEmail,
			&message.FromName,
			&message.FromPhone,
			&sentAt,
			&message.Snippet,
			&message.HasAttachments,
			&message.SizeEstimate,
			&deletedAt,
			&bodySize,
			&position,
			&window.Total,
			&window.AnchorPosition,
		); err != nil {
			return nil, fmt.Errorf("scan conversation message: %w", err)
		}
		if sentAt.Valid {
			message.SentAt = sentAt.Time
		}
		if deletedAt.Valid {
			deleted := deletedAt.Time
			message.DeletedAt = &deleted
		}
		message.Headers = map[string]string{}
		window.Messages = append(window.Messages, message)
		ids = append(ids, message.ID)
		bodySizes = append(bodySizes, bodySize)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate conversation messages: %w", err)
	}
	if len(ids) == 0 {
		return window, nil
	}
	inlineIDs := applyConversationBodyBudget(window.Messages, bodySizes, anchorID)
	if err := s.batchPopulateBodies(ctx, window.Messages, inlineIDs); err != nil {
		return nil, err
	}
	if err := s.batchPopulateContext(ctx, window.Messages, ids); err != nil {
		return nil, fmt.Errorf("populate conversation participants: %w", err)
	}
	if err := s.batchPopulateAttachments(ctx, window.Messages, ids); err != nil {
		return nil, err
	}
	return window, nil
}

// applyConversationBodyBudget selects which window messages get inline bodies
// under ConversationInlineBodyBudget. The anchor is always inlined (its cost
// still counts against the budget); the rest are inlined in chronological
// order when they fit the remaining budget. Messages left out are marked
// BodyOmitted so clients know to fetch the body individually. Returns the IDs
// whose bodies should be loaded.
func applyConversationBodyBudget(messages []APIMessage, bodySizes []int64, anchorID int64) []int64 {
	remaining := int64(ConversationInlineBodyBudget)
	inlineIDs := make([]int64, 0, len(messages))
	for i := range messages {
		if messages[i].ID != anchorID {
			continue
		}
		remaining -= bodySizes[i]
		if bodySizes[i] > 0 {
			inlineIDs = append(inlineIDs, messages[i].ID)
		}
	}
	for i := range messages {
		message := &messages[i]
		if message.ID == anchorID || bodySizes[i] == 0 {
			continue
		}
		if bodySizes[i] > remaining {
			message.BodyOmitted = true
			continue
		}
		remaining -= bodySizes[i]
		inlineIDs = append(inlineIDs, message.ID)
	}
	return inlineIDs
}

// batchPopulateBodies loads message bodies for the budget-selected IDs via a
// primary-key IN lookup on message_bodies — the only sanctioned access path
// for that table — and assigns BodyText/BodyHTML/Body on the matching
// messages.
func (s *Store) batchPopulateBodies(ctx context.Context, messages []APIMessage, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	placeholders := make([]string, len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		placeholders[i] = "?"
		args[i] = id
	}
	indexByID := make(map[int64]int, len(messages))
	for i := range messages {
		indexByID[messages[i].ID] = i
	}
	rows, err := s.db.QueryContext(ctx, s.dialect.Rebind(fmt.Sprintf(`
		SELECT message_id, COALESCE(body_text, ''), COALESCE(body_html, '')
		FROM message_bodies
		WHERE message_id IN (%s)
	`, strings.Join(placeholders, ","))), args...)
	if err != nil {
		return fmt.Errorf("get conversation bodies: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var messageID int64
		var bodyText, bodyHTML string
		if err := rows.Scan(&messageID, &bodyText, &bodyHTML); err != nil {
			return fmt.Errorf("scan conversation body: %w", err)
		}
		index, ok := indexByID[messageID]
		if !ok {
			continue
		}
		message := &messages[index]
		message.BodyText = bodyText
		message.BodyHTML = bodyHTML
		if message.BodyText != "" {
			message.Body = message.BodyText
		} else {
			message.Body = message.BodyHTML
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate conversation bodies: %w", err)
	}
	return nil
}

func (s *Store) batchPopulateAttachments(ctx context.Context, messages []APIMessage, ids []int64) error {
	placeholders := make([]string, len(ids))
	args := make([]any, len(ids))
	indexByID := make(map[int64]int, len(ids))
	for i, id := range ids {
		placeholders[i] = "?"
		args[i] = id
		indexByID[id] = i
	}
	rows, err := s.db.QueryContext(ctx, s.dialect.Rebind(fmt.Sprintf(`
		SELECT message_id, id, COALESCE(filename, ''), COALESCE(mime_type, ''),
			COALESCE(size, 0), COALESCE(content_hash, ''), COALESCE(storage_path, '')
		FROM attachments
		WHERE message_id IN (%s)
		ORDER BY message_id, id
	`, strings.Join(placeholders, ","))), args...)
	if err != nil {
		return fmt.Errorf("get conversation attachments: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var messageID int64
		var attachment APIAttachment
		var storagePath sql.NullString
		if err := rows.Scan(&messageID, &attachment.ID, &attachment.Filename, &attachment.MimeType,
			&attachment.Size, &attachment.ContentHash, &storagePath); err != nil {
			return fmt.Errorf("scan conversation attachment: %w", err)
		}
		if storagePath.Valid && (strings.HasPrefix(storagePath.String, "http://") || strings.HasPrefix(storagePath.String, "https://")) {
			attachment.ContentHash = ""
			attachment.URL = storagePath.String
		}
		if index, ok := indexByID[messageID]; ok {
			messages[index].Attachments = append(messages[index].Attachments, attachment)
		}
	}
	return rows.Err()
}

// SetConversationMetadata writes the conversations.metadata JSON/JSONB column.
// Passing an invalid sql.NullString clears the column.
func (s *Store) SetConversationMetadata(conversationID int64, metadata sql.NullString) error {
	_, err := s.db.Exec(fmt.Sprintf(`
		UPDATE conversations
		SET metadata = %s
		WHERE id = ?
	`, s.dialect.JSONBindExpr()), metadata, conversationID)
	if err != nil {
		return fmt.Errorf("set conversation metadata (id=%d): %w", conversationID, err)
	}
	return nil
}

// GetConversationMetadata reads the conversations.metadata JSON/JSONB column.
func (s *Store) GetConversationMetadata(conversationID int64) (sql.NullString, error) {
	var metadata sql.NullString
	if err := s.db.QueryRow(
		`SELECT metadata FROM conversations WHERE id = ?`, conversationID,
	).Scan(&metadata); err != nil {
		return sql.NullString{}, fmt.Errorf("get conversation metadata (id=%d): %w", conversationID, err)
	}
	return metadata, nil
}

// ConversationMetadataBatch returns provider metadata keyed by source
// conversation ID for the requested conversations that exist under sourceID.
// Existing conversations with SQL NULL metadata remain present with an invalid
// sql.NullString; missing or other-source conversations are omitted.
func (s *Store) ConversationMetadataBatch(
	sourceID int64, sourceConversationIDs []string,
) (map[string]sql.NullString, error) {
	if len(sourceConversationIDs) == 0 {
		return make(map[string]sql.NullString), nil
	}

	metadata := make(map[string]sql.NullString)
	err := queryInChunks(s.db, sourceConversationIDs, []any{sourceID},
		`SELECT source_conversation_id, metadata
		 FROM conversations
		 WHERE source_id = ? AND source_conversation_id IN (%s)`,
		func(rows *loggedRows) error {
			var sourceConversationID string
			var value sql.NullString
			if err := rows.Scan(&sourceConversationID, &value); err != nil {
				return err
			}
			metadata[sourceConversationID] = value
			return nil
		})
	if err != nil {
		return nil, fmt.Errorf("load conversation metadata batch: %w", err)
	}
	return metadata, nil
}
