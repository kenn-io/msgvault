// Package identityindex defines the cache datasets and shared classification
// rules used to build and query identity-oriented analytical indexes.
package identityindex

import (
	"math"
	"slices"
	"strings"
)

const (
	DatasetActivity          = "relationship_activity"
	DatasetPeople            = "relationship_people"
	DatasetDomains           = "relationship_domains"
	DatasetRelationshipDaily = "relationship_daily"

	// DatasetEntryFacts and the following legacy names remain temporarily for
	// source compatibility while callers move to the four-dataset index.
	DatasetEntryFacts        = "identity_entry_facts"
	DatasetDirectEdges       = "identity_direct_edges"
	DatasetConversationEdges = "identity_conversation_edges"
	DatasetDirectory         = "identity_directory"
	DatasetRollups           = "identity_rollups"
	DatasetDomainRollups     = "domain_rollups"
	DatasetRelationships     = "relationship_rollups"

	ModalityEmail   uint8 = 1
	ModalityChat    uint8 = 2
	ModalityMeeting uint8 = 4

	RelationshipHalfLifeDays = 365.0
	RelationshipDecayRate    = math.Ln2 / RelationshipHalfLifeDays
)

var (
	TextMessageTypes = []string{
		"whatsapp", "imessage", "sms", "mms", "rcs",
		"google_voice_text", "teams", "discord", "beeper", "fbmessenger",
	}
	ChatFallbackMessageTypes = []string{"", "chat", "text"}
	ChatConversationTypes    = []string{"direct_chat", "group_chat", "channel", "chat"}

	RequiredDatasets = []string{
		DatasetActivity,
		DatasetPeople,
		DatasetDomains,
		DatasetRelationshipDaily,
	}
)

// CacheStatsSummary contains the scalar archive statistics committed with a
// cache publication.
type CacheStatsSummary struct {
	TotalMessages       int64  `json:"total_messages"`
	Sources             int64  `json:"sources"`
	UniqueSenders       int64  `json:"unique_senders"`
	UniqueDomains       int64  `json:"unique_domains"`
	MinYear             *int64 `json:"min_year,omitempty"`
	MaxYear             *int64 `json:"max_year,omitempty"`
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
