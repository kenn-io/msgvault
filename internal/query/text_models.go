package query

import (
	"slices"
	"time"
)

// TextViewType represents the type of view in Texts mode.
type TextViewType int

const (
	TextViewConversations TextViewType = iota
	TextViewContacts
	TextViewContactNames
	TextViewSources
	TextViewLabels
	TextViewTime
	TextViewTypeCount
)

func (v TextViewType) String() string {
	switch v {
	case TextViewConversations:
		return "Conversations"
	case TextViewContacts:
		return "Contacts"
	case TextViewContactNames:
		return "Contact Names"
	case TextViewSources:
		return "Sources"
	case TextViewLabels:
		return "Labels"
	case TextViewTime:
		return "Time"
	default:
		return "Unknown"
	}
}

// ConversationRow represents a conversation in the Conversations view.
type ConversationRow struct {
	ConversationID   int64
	Title            string
	SourceType       string
	MessageCount     int64
	ParticipantCount int64
	LastMessageAt    time.Time
	LastPreview      string
}

// TextSortField represents fields available for sorting in Texts mode.
type TextSortField int

const (
	// TextSortByLastMessage sorts by last message timestamp (default).
	TextSortByLastMessage TextSortField = iota
	TextSortByCount
	TextSortByName
)

// TextFilter specifies which text messages/conversations to retrieve.
// Note: conversation scope for ListConversationMessages is passed as
// an explicit parameter, not through this filter.
type TextFilter struct {
	SourceID      *int64
	ContactPhone  string
	ContactName   string
	SourceType    string
	Label         string
	TimeRange     TimeRange
	After         *time.Time
	Before        *time.Time
	Pagination    Pagination
	SortField     TextSortField
	SortDirection SortDirection
}

// TextAggregateOptions configures a text aggregate query.
type TextAggregateOptions struct {
	SourceID        *int64
	After           *time.Time
	Before          *time.Time
	SortField       TextSortField
	SortDirection   SortDirection
	Limit           int
	TimeGranularity TimeGranularity
	SearchQuery     string
}

// TextStatsOptions configures a text stats query.
type TextStatsOptions struct {
	SourceID    *int64
	SearchQuery string
}

// TextMessageTypes lists the message_type values included in Texts mode.
var TextMessageTypes = []string{
	"whatsapp", "imessage", "sms", "mms", "google_voice_text",
}

// textSortFieldToSortField converts a TextSortField to the generic SortField
// used by aggregate queries. TextSortByLastMessage has no direct equivalent
// in SortField so it falls back to SortByCount.
func textSortFieldToSortField(f TextSortField) SortField {
	switch f {
	case TextSortByCount:
		return SortByCount
	case TextSortByName:
		return SortByName
	default: // TextSortByLastMessage
		return SortByCount
	}
}

// IsTextMessageType returns true if the given type is a text message type.
func IsTextMessageType(mt string) bool {
	return slices.Contains(TextMessageTypes, mt)
}
