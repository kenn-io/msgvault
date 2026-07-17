package query

import (
	"slices"
	"strings"
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
	// TimeGranularitySet distinguishes an explicit TimeYear from an omitted
	// granularity, since TimeYear is the enum zero value.
	TimeGranularitySet bool
	SearchQuery        string
}

// HasTimeGranularity reports whether a text aggregate request explicitly
// selected a time granularity.
func (opts TextAggregateOptions) HasTimeGranularity() bool {
	return opts.TimeGranularitySet || opts.TimeGranularity != TimeYear
}

// EffectiveTimeGranularity returns the text aggregate granularity after
// applying the API-compatible default.
func (opts TextAggregateOptions) EffectiveTimeGranularity() TimeGranularity {
	if !opts.HasTimeGranularity() {
		return TimeMonth
	}
	return opts.TimeGranularity
}

// TextStatsOptions configures a text stats query.
type TextStatsOptions struct {
	SourceID    *int64
	SearchQuery string
}

const (
	messageTypeSMS               = "sms"
	messageTypeMeetingTranscript = "meeting_transcript"
)

// TextMessageTypes lists the message_type values included in Texts mode.
var TextMessageTypes = []string{
	"whatsapp", "imessage", messageTypeSMS, "mms", "google_voice_text", "teams", "beeper",
}

// TextMessageTypeSQLList renders TextMessageTypes as a quoted SQL IN-list
// ("'whatsapp','imessage',...") so filters derive from the one canonical
// slice instead of hand-maintained literals. Values are trusted package
// constants, never user input.
var TextMessageTypeSQLList = "'" + strings.Join(TextMessageTypes, "','") + "'"

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

// KnownMessageTypes enumerates every message_type value that msgvault's sync
// and import paths write to the messages table. The search --message-type
// flag validates against this set so a typo fails fast with the allowed
// values instead of silently returning no results.
var KnownMessageTypes = []string{
	messageTypeEmail,
	"calendar_event",
	messageTypeMeetingTranscript,
	messageTypeSMS,
	"mms",
	"whatsapp",
	"imessage",
	"teams",
	"fbmessenger",
	"synctech_sms_call",
	"google_voice_text",
	"google_voice_call",
	"google_voice_voicemail",
	"beeper",
}

// IsKnownMessageType reports whether mt is a message_type value that msgvault
// can produce.
func IsKnownMessageType(mt string) bool {
	return slices.Contains(KnownMessageTypes, mt)
}
