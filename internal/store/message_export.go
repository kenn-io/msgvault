package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

const messageExportPageSize = 256

type MessageExportConversationType string

const (
	MessageExportConversationEmailThread MessageExportConversationType = "email_thread"
	MessageExportConversationChannel     MessageExportConversationType = "channel"
	MessageExportConversationThread      MessageExportConversationType = "thread"
	MessageExportConversationDirectChat  MessageExportConversationType = "direct_chat"
	MessageExportConversationGroupChat   MessageExportConversationType = "group_chat"
	MessageExportConversationMeeting     MessageExportConversationType = "meeting"
	MessageExportConversationCalendar    MessageExportConversationType = "calendar"
	MessageExportConversationOther       MessageExportConversationType = "other"
)

type MessageExportFilter struct {
	Start        time.Time
	End          time.Time
	SourceIDs    []int64
	MessageTypes []string
}

type MessageExportSource struct {
	SourceType           string
	Identifier           string
	DisplayName          string
	LastSuccessfulSyncAt *time.Time
}

type MessageExportConversation struct {
	SourceType       string
	SourceIdentifier string
	ID               string
	Title            string
	ConversationType MessageExportConversationType
	ParentID         *string
}

type MessageExportAuthor struct {
	DisplayName string
	Address     string
}

type MessageExportMessage struct {
	SourceType        string
	SourceIdentifier  string
	ID                string
	ConversationID    string
	MessageType       string
	Subject           string
	Text              string
	Author            *MessageExportAuthor
	OccurredAt        time.Time
	DeletedFromSource bool
}

type MessageExportCounts struct {
	Sources       int
	Conversations int
	Messages      int
}

type MessageExportSink interface {
	Source(source MessageExportSource) error
	Conversation(conversation MessageExportConversation) error
	Message(message MessageExportMessage) error
}

type messageExportConversationMetadata struct {
	ParentChannelID    string `json:"parent_channel_id"`
	DiscordChannelType int    `json:"discord_channel_type"`
}

type messageExportMessageMetadata struct {
	AuthorDisplayName string `json:"author_display_name"`
}

type messageExportSourceRow struct {
	id          int64
	sourceType  string
	identifier  string
	displayName string
}

type messageExportMessageRow struct {
	id                   int64
	sourceType           string
	sourceIdentifier     string
	sourceMessageID      string
	sourceConversationID string
	messageType          string
	subject              string
	metadata             string
	occurredAt           time.Time
	deletedFromSource    bool
	senderID             sql.NullInt64
	recipientDisplayName string
	senderDisplayName    string
	senderAddress        string
}

func (s *Store) ExportMessages(
	ctx context.Context,
	filter MessageExportFilter,
	sink MessageExportSink,
) (MessageExportCounts, error) {
	if sink == nil {
		return MessageExportCounts{}, errors.New("message export sink is required")
	}
	if filter.Start.IsZero() || filter.End.IsZero() || !filter.Start.Before(filter.End) {
		return MessageExportCounts{}, errors.New("message export requires increasing non-zero bounds")
	}
	for _, sourceID := range filter.SourceIDs {
		if sourceID <= 0 {
			return MessageExportCounts{}, fmt.Errorf("message export source ID must be positive: %d", sourceID)
		}
	}
	for _, messageType := range filter.MessageTypes {
		if strings.TrimSpace(messageType) == "" {
			return MessageExportCounts{}, errors.New("message export type must be non-empty")
		}
	}

	var counts MessageExportCounts
	if err := s.exportMessageSources(ctx, filter, sink, &counts); err != nil {
		return counts, err
	}
	if err := s.exportMessageConversations(ctx, filter, sink, &counts); err != nil {
		return counts, err
	}
	if err := s.exportMessageRows(ctx, filter, sink, &counts); err != nil {
		return counts, err
	}
	return counts, nil
}

func (s *Store) exportMessageSources(
	ctx context.Context,
	filter MessageExportFilter,
	sink MessageExportSink,
	counts *MessageExportCounts,
) error {
	messagePredicate, messageArgs := messageExportPredicate("m", filter, false)
	var query string
	var args []any
	if len(filter.SourceIDs) > 0 {
		sourceClause, sourceArgs := messageExportInClause("s.id", filter.SourceIDs)
		query = `
			SELECT s.id, s.source_type, s.identifier, COALESCE(s.display_name, '')
			FROM sources s
			WHERE ` + sourceClause + `
			ORDER BY s.source_type, s.identifier
		`
		args = sourceArgs
	} else {
		query = `
			SELECT s.id, s.source_type, s.identifier, COALESCE(s.display_name, '')
			FROM sources s
			WHERE EXISTS (
				SELECT 1
				FROM messages m
				WHERE m.source_id = s.id AND ` + messagePredicate + `
			)
			ORDER BY s.source_type, s.identifier
		`
		args = messageArgs
	}
	rows, err := s.db.QueryContext(ctx, s.dialect.Rebind(query), args...)
	if err != nil {
		return fmt.Errorf("query message export sources: %w", err)
	}
	var selected []messageExportSourceRow
	for rows.Next() {
		var source messageExportSourceRow
		if err := rows.Scan(
			&source.id, &source.sourceType, &source.identifier, &source.displayName,
		); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan message export source: %w", err)
		}
		if source.sourceType == "" || source.identifier == "" {
			_ = rows.Close()
			return errors.New("message export source has an empty stable identity")
		}
		selected = append(selected, source)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("iterate message export sources: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close message export sources: %w", err)
	}

	for _, source := range selected {
		var completedAt *time.Time
		run, err := s.GetLastSuccessfulSync(source.id)
		if err != nil && !errors.Is(err, ErrSyncRunNotFound) {
			return fmt.Errorf("load message export source sync: %w", err)
		}
		if err == nil && run.CompletedAt.Valid {
			value := run.CompletedAt.Time.UTC()
			completedAt = &value
		}
		if err := sink.Source(MessageExportSource{
			SourceType:           source.sourceType,
			Identifier:           source.identifier,
			DisplayName:          source.displayName,
			LastSuccessfulSyncAt: completedAt,
		}); err != nil {
			return fmt.Errorf("emit message export source: %w", err)
		}
		counts.Sources++
	}
	return nil
}

func (s *Store) exportMessageConversations(
	ctx context.Context,
	filter MessageExportFilter,
	sink MessageExportSink,
	counts *MessageExportCounts,
) error {
	predicate, args := messageExportPredicate("m", filter, true)
	var sourcePredicate string
	if len(filter.SourceIDs) > 0 {
		clause, sourceArgs := messageExportInClause("c.source_id", filter.SourceIDs)
		sourcePredicate = " AND " + clause
		args = append(args, sourceArgs...)
	}
	rows, err := s.db.QueryContext(ctx, s.dialect.Rebind(`
		SELECT s.source_type, s.identifier, c.source_conversation_id,
		       COALESCE(c.title, ''), c.conversation_type,
		       COALESCE(c.metadata, '{}')
		FROM conversations c
		JOIN sources s ON s.id = c.source_id
		WHERE EXISTS (
			SELECT 1
			FROM messages m
			WHERE m.conversation_id = c.id AND `+predicate+`
		)`+sourcePredicate+`
		ORDER BY s.source_type, s.identifier, c.source_conversation_id
	`), args...)
	if err != nil {
		return fmt.Errorf("query message export conversations: %w", err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var (
			sourceType, sourceIdentifier, title, rawType, rawMetadata string
			conversationID                                            sql.NullString
		)
		if err := rows.Scan(
			&sourceType, &sourceIdentifier, &conversationID,
			&title, &rawType, &rawMetadata,
		); err != nil {
			return fmt.Errorf("scan message export conversation: %w", err)
		}
		if sourceType == "" || sourceIdentifier == "" ||
			!conversationID.Valid || conversationID.String == "" {
			return errors.New("message export conversation has an empty stable identity")
		}
		conversationType, parentID, err := normalizeMessageExportConversation(
			sourceType, rawType, rawMetadata,
		)
		if err != nil {
			return fmt.Errorf(
				"normalize message export conversation %q: %w",
				conversationID.String, err,
			)
		}
		if err := sink.Conversation(MessageExportConversation{
			SourceType:       sourceType,
			SourceIdentifier: sourceIdentifier,
			ID:               conversationID.String,
			Title:            title,
			ConversationType: conversationType,
			ParentID:         parentID,
		}); err != nil {
			return fmt.Errorf("emit message export conversation: %w", err)
		}
		counts.Conversations++
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate message export conversations: %w", err)
	}
	return nil
}

func (s *Store) exportMessageRows(
	ctx context.Context,
	filter MessageExportFilter,
	sink MessageExportSink,
	counts *MessageExportCounts,
) error {
	predicate, filterArgs := messageExportPredicate("m", filter, true)
	var cursor *messageExportMessageRow
	for {
		pagePredicate := predicate
		args := append([]any{}, filterArgs...)
		if cursor != nil {
			pagePredicate += `
				AND (
					s.source_type, s.identifier, c.source_conversation_id,
					COALESCE(m.sent_at, m.received_at, m.internal_date),
					m.source_message_id, m.id
				) > (?, ?, ?, ?, ?, ?)
			`
			args = append(args,
				cursor.sourceType,
				cursor.sourceIdentifier,
				cursor.sourceConversationID,
				cursor.occurredAt,
				cursor.sourceMessageID,
				cursor.id,
			)
		}
		args = append(args, messageExportPageSize)
		rows, err := s.db.QueryContext(ctx, s.dialect.Rebind(`
			SELECT m.id, s.source_type, s.identifier, m.source_message_id,
			       c.source_conversation_id, m.message_type,
			       COALESCE(m.subject, ''), COALESCE(m.metadata, '{}'),
			       COALESCE(m.sent_at, m.received_at, m.internal_date),
			       m.deleted_from_source_at, p_sender.id,
			       CASE
			           WHEN m.sender_id IS NULL
			                OR mr_from.participant_id = m.sender_id
			           THEN COALESCE(mr_from.display_name, '')
			           ELSE ''
			       END,
			       COALESCE(p_sender.display_name, ''),
			       COALESCE(p_sender.email_address, p_sender.phone_number, '')
			FROM messages m
			JOIN conversations c ON c.id = m.conversation_id
			JOIN sources s ON s.id = m.source_id
			LEFT JOIN message_recipients mr_from ON mr_from.id = (
				SELECT mr.id FROM message_recipients mr
				WHERE mr.message_id = m.id AND mr.recipient_type = 'from'
				ORDER BY mr.id LIMIT 1
			)
			LEFT JOIN participants p_sender
			       ON p_sender.id = COALESCE(m.sender_id, mr_from.participant_id)
			WHERE `+pagePredicate+`
			ORDER BY s.source_type, s.identifier, c.source_conversation_id,
			         COALESCE(m.sent_at, m.received_at, m.internal_date),
			         m.source_message_id, m.id
			LIMIT ?
		`), args...)
		if err != nil {
			return fmt.Errorf("query message export page: %w", err)
		}
		page, err := scanMessageExportPage(rows)
		if err != nil {
			return err
		}
		if len(page) == 0 {
			return nil
		}
		for _, row := range page {
			var text sql.NullString
			if err := s.db.QueryRowContext(
				ctx,
				s.dialect.Rebind(`SELECT body_text FROM message_bodies WHERE message_id = ?`),
				row.id,
			).Scan(&text); err != nil && !errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("load message export body: %w", err)
			}
			author, err := normalizeMessageExportAuthor(row)
			if err != nil {
				return fmt.Errorf("normalize message export author: %w", err)
			}
			if err := sink.Message(MessageExportMessage{
				SourceType:        row.sourceType,
				SourceIdentifier:  row.sourceIdentifier,
				ID:                row.sourceMessageID,
				ConversationID:    row.sourceConversationID,
				MessageType:       row.messageType,
				Subject:           row.subject,
				Text:              text.String,
				Author:            author,
				OccurredAt:        row.occurredAt.UTC(),
				DeletedFromSource: row.deletedFromSource,
			}); err != nil {
				return fmt.Errorf("emit message export message: %w", err)
			}
			counts.Messages++
		}
		if len(page) < messageExportPageSize {
			return nil
		}
		last := page[len(page)-1]
		cursor = &last
	}
}

func scanMessageExportPage(rows *loggedRows) ([]messageExportMessageRow, error) {
	var page []messageExportMessageRow
	for rows.Next() {
		var (
			row                                   messageExportMessageRow
			sourceMessageID, sourceConversationID sql.NullString
			occurredAt                            nullableTimestamp
			deletedFromSource                     sql.NullTime
		)
		if err := rows.Scan(
			&row.id, &row.sourceType, &row.sourceIdentifier, &sourceMessageID,
			&sourceConversationID, &row.messageType, &row.subject, &row.metadata,
			&occurredAt, &deletedFromSource, &row.senderID,
			&row.recipientDisplayName, &row.senderDisplayName, &row.senderAddress,
		); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("scan message export message: %w", err)
		}
		if row.sourceType == "" || row.sourceIdentifier == "" ||
			!sourceMessageID.Valid || sourceMessageID.String == "" ||
			!sourceConversationID.Valid || sourceConversationID.String == "" {
			_ = rows.Close()
			return nil, errors.New("message export message has an empty stable identity")
		}
		row.sourceMessageID = sourceMessageID.String
		row.sourceConversationID = sourceConversationID.String
		row.occurredAt = occurredAt.Time
		row.deletedFromSource = deletedFromSource.Valid
		page = append(page, row)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, fmt.Errorf("iterate message export page: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close message export page: %w", err)
	}
	return page, nil
}

func messageExportPredicate(
	alias string,
	filter MessageExportFilter,
	includeSourceFilter bool,
) (string, []any) {
	effectiveTimestamp := fmt.Sprintf(
		"COALESCE(%[1]s.sent_at, %[1]s.received_at, %[1]s.internal_date)",
		alias,
	)
	parts := []string{
		alias + ".deleted_at IS NULL",
		effectiveTimestamp + " >= ?",
		effectiveTimestamp + " < ?",
	}
	args := []any{filter.Start, filter.End}
	if includeSourceFilter && len(filter.SourceIDs) > 0 {
		clause, values := messageExportInClause(alias+".source_id", filter.SourceIDs)
		parts = append(parts, clause)
		args = append(args, values...)
	}
	if len(filter.MessageTypes) > 0 {
		clause, values := messageExportInClause(alias+".message_type", filter.MessageTypes)
		parts = append(parts, clause)
		args = append(args, values...)
	}
	return strings.Join(parts, " AND "), args
}

func messageExportInClause[T any](column string, values []T) (string, []any) {
	placeholders := make([]string, len(values))
	args := make([]any, len(values))
	for i, value := range values {
		placeholders[i] = "?"
		args[i] = value
	}
	return column + " IN (" + strings.Join(placeholders, ", ") + ")", args
}

func normalizeMessageExportConversation(
	sourceType, rawType, rawMetadata string,
) (MessageExportConversationType, *string, error) {
	if sourceType == "discord" {
		var metadata messageExportConversationMetadata
		if err := json.Unmarshal([]byte(rawMetadata), &metadata); err != nil {
			return "", nil, fmt.Errorf("decode metadata: %w", err)
		}
		conversationType := MessageExportConversationChannel
		if metadata.DiscordChannelType >= 10 && metadata.DiscordChannelType <= 12 {
			conversationType = MessageExportConversationThread
		}
		var parentID *string
		if metadata.ParentChannelID != "" {
			value := metadata.ParentChannelID
			parentID = &value
		}
		return conversationType, parentID, nil
	}
	switch rawType {
	case string(AttributeFieldEmail), "email_thread":
		return MessageExportConversationEmailThread, nil, nil
	case "channel":
		return MessageExportConversationChannel, nil, nil
	case "thread":
		return MessageExportConversationThread, nil, nil
	case "direct_chat", "oneOnOne":
		return MessageExportConversationDirectChat, nil, nil
	case "group", "group_chat":
		return MessageExportConversationGroupChat, nil, nil
	case "meeting":
		return MessageExportConversationMeeting, nil, nil
	case "calendar", "calendar_event":
		return MessageExportConversationCalendar, nil, nil
	default:
		return MessageExportConversationOther, nil, nil
	}
}

func normalizeMessageExportAuthor(
	row messageExportMessageRow,
) (*MessageExportAuthor, error) {
	displayName := strings.TrimSpace(row.recipientDisplayName)
	if row.sourceType == "discord" && displayName == "" {
		var metadata messageExportMessageMetadata
		if err := json.Unmarshal([]byte(row.metadata), &metadata); err != nil {
			return nil, fmt.Errorf("decode metadata: %w", err)
		}
		displayName = strings.TrimSpace(metadata.AuthorDisplayName)
	}
	if displayName == "" {
		displayName = row.senderDisplayName
	}
	if !row.senderID.Valid && displayName == "" {
		// A missing normalized sender is represented by a null author.
		//nolint:nilnil
		return nil, nil
	}
	return &MessageExportAuthor{
		DisplayName: displayName,
		Address:     row.senderAddress,
	}, nil
}
