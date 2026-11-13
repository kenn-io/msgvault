// Package identityindex defines the cache datasets and shared classification
// rules used to build and query identity-oriented analytical indexes.
package identityindex

import (
	"math"
	"slices"
	"strings"
)

const (
	DatasetActivity                 = "relationship_activity"
	DatasetPeople                   = "relationship_people"
	DatasetDomains                  = "relationship_domains"
	DatasetRelationshipDaily        = "relationship_daily"
	DatasetLogicalContributions     = "relationship_logical_contributions"
	DatasetTemperatureContributions = "relationship_temperature_contributions"
	DatasetMeetingOccurrences       = "meeting_occurrences"

	ModalityEmail   uint8 = 1
	ModalityChat    uint8 = 2
	ModalityMeeting uint8 = 4

	RelationshipHalfLifeDays = 365.0
	RelationshipDecayRate    = math.Ln2 / RelationshipHalfLifeDays
)

// MeetingOccurrenceStageSchema defines the stored fields before the occurrence ID is assigned.
const MeetingOccurrenceStageSchema = "message_id BIGINT, anchor_message_id BIGINT, occurrence_key VARCHAR, occurred_at TIMESTAMP, start_date DATE, counted BOOLEAN, declined_emails VARCHAR[]"

var (
	// MeetingOccurrenceColumns defines the cache row order, including its derived ID.
	MeetingOccurrenceColumns = func() string {
		fields := strings.Split(MeetingOccurrenceStageSchema, ",")
		names := make([]string, 0, len(fields)+1)
		for _, field := range fields {
			names = append(names, strings.Fields(field)[0])
		}
		return strings.Join(append(names, "occurrence_id"), ", ")
	}()
	// MeetingOccurrenceFixtureRow is a typed row for empty cache fixtures.
	MeetingOccurrenceFixtureRow = func() string {
		fields := strings.Split(MeetingOccurrenceStageSchema, ",")
		values := make([]string, 0, len(fields)+1)
		for _, field := range fields {
			values = append(values, "NULL::"+strings.Fields(field)[1])
		}
		return "(" + strings.Join(append(values, "NULL::DECIMAL(38,0)"), ", ") + ")"
	}()
	// EmptyMeetingOccurrencesSQL produces the same columns as a published cache.
	EmptyMeetingOccurrencesSQL = "SELECT * FROM (VALUES " + MeetingOccurrenceFixtureRow + ") AS t(" + MeetingOccurrenceColumns + ") WHERE false"

	TextMessageTypes = []string{
		"google_chat", "whatsapp", "imessage", "sms", "mms", "rcs",
		"google_voice_text", "teams", "discord", "beeper", "slack", "inline", "fbmessenger", "matrix", "chatwoot", "telegram", "x_dm", "facebook_comments", "instagram_comments",
	}
	ChatFallbackMessageTypes = []string{"", "chat", "text"}
	ChatConversationTypes    = []string{"direct_chat", "group_chat", "channel", "chat"}

	RequiredDatasets = []string{
		DatasetActivity,
		DatasetPeople,
		DatasetDomains,
		DatasetRelationshipDaily,
		DatasetLogicalContributions,
		DatasetTemperatureContributions,
	}
)

// CacheStatsSummary contains the scalar archive statistics committed with a
// cache publication.
type CacheStatsSummary struct {
	TotalMessages       int64  `json:"total_messages"`
	Sources             int64  `json:"sources"`
	UniqueSenders       int64  `json:"unique_senders"`
	UniqueDomains       int64  `json:"unique_domains"`
	MinYear             *int64 `json:"min_year,omitzero"`
	MaxYear             *int64 `json:"max_year,omitzero"`
	TotalSizeBytes      int64  `json:"total_size_bytes"`
	AttachmentSizeBytes int64  `json:"attachment_size_bytes"`
}

// IsChatSQL renders the shared chat-classification predicate for trusted SQL
// column expressions.
func IsChatSQL(messageType, conversationType string) string {
	return "lower(" + messageType + ") IN (" + quotedList(TextMessageTypes) + ")" +
		" OR (lower(" + messageType + ") IN (" + quotedList(ChatFallbackMessageTypes) + ")" +
		" AND lower(" + conversationType + ") IN (" + quotedList(ChatConversationTypes) + "))"
}

// IsChat reports whether a message is grouped as a chat conversation.
func IsChat(messageType, conversationType string) bool {
	messageType = strings.ToLower(messageType)
	if slices.Contains(TextMessageTypes, messageType) {
		return true
	}
	return slices.Contains(ChatFallbackMessageTypes, messageType) &&
		slices.Contains(ChatConversationTypes, strings.ToLower(conversationType))
}

// EntryKindSQL renders the shared logical-entry classification for a trusted
// message-type SQL expression.
func EntryKindSQL(messageType string) string {
	return "CASE WHEN lower(" + messageType + ") = 'email' OR " + messageType + " = '' THEN 'email'" +
		" WHEN lower(" + messageType + ") = 'calendar_event' THEN 'event'" +
		" WHEN lower(" + messageType + ") IN ('meeting_transcript','meeting_note','meeting_minutes') THEN 'meeting'" +
		" ELSE 'item' END"
}

func quotedList(values []string) string {
	return "'" + strings.Join(values, "','") + "'"
}
