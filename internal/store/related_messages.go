package store

import (
	"context"
	"encoding/json/v2"
	"fmt"
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
	var ids []int64
	for _, message := range messages {
		if message.RelatedMessageID != nil {
			ids = append(ids, *message.RelatedMessageID)
		}
	}
	targets, err := s.GetMessagesSummariesByIDsContext(ctx, ids)
	if err != nil {
		return fmt.Errorf("get related message targets: %w", err)
	}
	byID := make(map[int64]APIMessage, len(targets))
	for _, target := range targets {
		byID[target.ID] = target
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
