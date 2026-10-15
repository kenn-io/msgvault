package store

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"strings"
)

// RelatedMessageCandidate decodes an alternate representation of an archived call.
func RelatedMessageCandidate(id int64, messageType, metadata string) *int64 {
	if messageType != "chatwoot" && messageType != "meeting_transcript" {
		return nil
	}
	var fields struct {
		Provider  string `json:"provider"`
		MeetingID int64  `json:"meeting_message_id"`
		ChatID    int64  `json:"chat_message_id"`
	}
	if json.Unmarshal([]byte(metadata), &fields) != nil {
		return nil
	}
	var candidate int64
	switch messageType {
	case "chatwoot":
		candidate = fields.MeetingID
	case "meeting_transcript":
		if fields.Provider == "chatwoot" {
			candidate = fields.ChatID
		}
	}
	if candidate <= 0 || candidate == id {
		return nil
	}
	return &candidate
}

// RelatedMessageTargetMatches keeps alternate representations within their source.
func RelatedMessageTargetMatches(sourceID int64, messageType string, targetSourceID int64, targetType string) bool {
	return sourceID == targetSourceID && ((messageType == "chatwoot" && targetType == "meeting_transcript") ||
		(messageType == "meeting_transcript" && targetType == "chatwoot"))
}

func (s *Store) populateRelatedMessages(ctx context.Context, messages []APIMessage) error {
	return PopulateRelatedMessagesContext(ctx, ReadDBContext(ctx, s.DB()), s.Rebind, "", messages)
}

// PopulateRelatedMessagesContext keeps links to live alternate messages in the same source.
func PopulateRelatedMessagesContext(ctx context.Context, db SQLReader, rebind func(string) string, tablePrefix string, messages []APIMessage) error {
	var args []any
	var placeholders []string
	for _, message := range messages {
		if message.RelatedMessageID != nil {
			args = append(args, *message.RelatedMessageID)
			placeholders = append(placeholders, "?")
		}
	}
	if len(args) == 0 {
		return nil
	}
	query := fmt.Sprintf("SELECT m.id, m.source_id, COALESCE(m.message_type, '') FROM %smessages m WHERE m.id IN (%s) AND %s", tablePrefix, strings.Join(placeholders, ","), LiveMessagesWhere("m", true))
	rows, err := db.QueryContext(ctx, rebind(query), args...)
	if err != nil {
		return fmt.Errorf("get related message targets: %w", err)
	}
	defer func() { _ = rows.Close() }()
	byID := make(map[int64]APIMessage, len(args))
	for rows.Next() {
		var target APIMessage
		if err := rows.Scan(&target.ID, &target.SourceID, &target.MessageType); err != nil {
			return fmt.Errorf("scan related message target: %w", err)
		}
		byID[target.ID] = target
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate related message targets: %w", err)
	}
	for i := range messages {
		message := &messages[i]
		if message.RelatedMessageID != nil {
			target, found := byID[*message.RelatedMessageID]
			if !found || !RelatedMessageTargetMatches(message.SourceID, message.MessageType, target.SourceID, target.MessageType) {
				message.RelatedMessageID = nil
			}
		}
	}
	return nil
}
