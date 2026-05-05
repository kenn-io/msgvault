// Package query provides a reusable query layer for msgvault.
// It supports aggregation queries for TUI views and message retrieval for detail views.
// The package is designed with a backend-agnostic interface to support both
// SQLite (for flexibility) and Parquet (for performance) data sources.
package query

import "time"

// AggregateRow represents a single row in an aggregate view.
// Used for Senders, Recipients, Domains, Labels, and Time views.
type AggregateRow struct {
	Key             string // email, domain, label name, or time period
	Count           int64  // number of messages
	TotalSize       int64  // sum of size_estimate in bytes
	AttachmentSize  int64  // sum of attachment sizes in bytes
	AttachmentCount int64  // number of attachments
	TotalUnique     int64  // total unique keys (same for all rows, computed via COUNT(*) OVER())
}

// MessageSummary represents a message in list views.
// Contains enough information for display without fetching the full body.
type MessageSummary struct {
	ID                   int64      `json:"id"`
	SourceMessageID      string     `json:"source_message_id"`
	ConversationID       int64      `json:"conversation_id"`
	SourceConversationID string     `json:"source_conversation_id"` // Gmail Thread ID
	Subject              string     `json:"subject"`
	Snippet              string     `json:"snippet"`
	FromEmail            string     `json:"from_email"`
	FromName             string     `json:"from_name"`
	FromPhone            string     `json:"from_phone,omitempty"` // Phone number (for WhatsApp/chat sources)
	SentAt               time.Time  `json:"sent_at"`
	SizeEstimate         int64      `json:"size_estimate"`
	HasAttachments       bool       `json:"has_attachments"`
	AttachmentCount      int        `json:"attachment_count"`
	Labels               []string   `json:"labels"`
	DeletedAt            *time.Time `json:"deleted_at,omitempty"`         // When message was deleted from server (nil if not deleted)
	MessageType          string     `json:"message_type,omitempty"`       // e.g., "email", "whatsapp" — from messages.message_type
	ConversationTitle    string     `json:"conversation_title,omitempty"` // Group/chat name from conversations.title
	BodyText             string     `json:"body_text,omitempty"`          // Full body text (only populated for timeline views)
}

// MessageDetail represents a full message with body and attachments.
type MessageDetail struct {
	ID                   int64      `json:"id"`
	SourceMessageID      string     `json:"source_message_id"`
	ConversationID       int64      `json:"conversation_id"`
	SourceConversationID string     `json:"source_conversation_id"` // Gmail Thread ID
	Subject              string     `json:"subject"`
	Snippet              string     `json:"snippet"`
	SentAt               time.Time  `json:"sent_at"`
	ReceivedAt           *time.Time `json:"received_at,omitempty"`
	DeletedAt            *time.Time `json:"deleted_at,omitempty"` // When message was deleted from source (nil if not deleted)
	SizeEstimate         int64      `json:"size_estimate"`
	HasAttachments       bool       `json:"has_attachments"`

	// Participants
	From []Address `json:"from"`
	To   []Address `json:"to"`
	Cc   []Address `json:"cc"`
	Bcc  []Address `json:"bcc"`

	// Content
	BodyText string `json:"body_text"`
	BodyHTML string `json:"body_html"`

	// Metadata
	Labels      []string         `json:"labels"`
	Attachments []AttachmentInfo `json:"attachments"`
}

// Address represents an email address with optional display name.
type Address struct {
	Email string
	Name  string
}

// AttachmentInfo represents attachment metadata.
type AttachmentInfo struct {
	ID          int64
	Filename    string
	MimeType    string
	Size        int64
	ContentHash string
}

// ViewType represents the type of aggregate view.
type ViewType int

const (
	ViewSenders ViewType = iota
	ViewSenderNames
	ViewRecipients
	ViewRecipientNames
	ViewDomains
	ViewLabels
	ViewTime

	// ViewTypeCount is the total number of view types. Must be last.
	ViewTypeCount
)

func (v ViewType) String() string {
	switch v {
	case ViewSenders:
		return "Senders"
	case ViewSenderNames:
		return "Sender Names"
	case ViewRecipients:
		return "Recipients"
	case ViewRecipientNames:
		return "Recipient Names"
	case ViewDomains:
		return "Domains"
	case ViewLabels:
		return "Labels"
	case ViewTime:
		return "Time"
	default:
		return "Unknown"
	}
}

// TimeGranularity represents the time grouping level.
type TimeGranularity int

const (
	TimeYear TimeGranularity = iota
	TimeMonth
	TimeDay

	// TimeGranularityCount is the total number of time granularity options. Must be last.
	TimeGranularityCount
)

func (g TimeGranularity) String() string {
	switch g {
	case TimeYear:
		return "Year"
	case TimeMonth:
		return "Month"
	case TimeDay:
		return "Day"
	default:
		return "Unknown"
	}
}

// SortField represents the field to sort aggregates by.
type SortField int

const (
	SortByCount SortField = iota
	SortBySize
	SortByAttachmentSize
	SortByName
)

func (f SortField) String() string {
	switch f {
	case SortByCount:
		return "Count"
	case SortBySize:
		return "Size"
	case SortByAttachmentSize:
		return "Attachments"
	case SortByName:
		return "Name"
	default:
		return "Unknown"
	}
}

// SortDirection represents ascending or descending sort order.
type SortDirection int

const (
	SortDesc SortDirection = iota
	SortAsc
)

// MessageSortField represents the field to sort messages by.
type MessageSortField int

const (
	MessageSortByDate MessageSortField = iota
	MessageSortBySize
	MessageSortBySubject
)

// MessageFilter specifies which messages to retrieve.
type MessageFilter struct {
	// Filter by aggregate key
	Sender        string // filter by sender email
	SenderName    string // filter by sender display name (COALESCE(display_name, email))
	Recipient     string // filter by recipient email
	RecipientName string // filter by recipient display name (COALESCE(display_name, email))
	Domain        string // filter by sender domain
	Label         string // filter by label name

	// Filter by conversation (thread)
	ConversationID *int64 // filter by conversation/thread ID

	// EmptyValueTargets specifies which dimensions to filter for NULL/empty values.
	// When empty (default): empty filter strings mean "no filter" (return all).
	// When a ViewType is present in the map: that dimension filters for NULL/empty values,
	// enabling drilldown into empty-bucket aggregates (e.g., messages with no sender).
	// Multiple dimensions can be set when drilling from one empty bucket into another.
	EmptyValueTargets map[ViewType]bool

	// Time range
	TimeRange TimeRange

	// Account filter
	SourceID  *int64  // nil means all accounts
	SourceIDs []int64 // multi-source filter (collections); overrides SourceID

	// Date range
	After  *time.Time
	Before *time.Time

	// Content filter
	WithAttachmentsOnly   bool // only return messages with attachments
	HideDeletedFromSource bool // exclude messages where deleted_from_source_at IS NOT NULL

	// Pagination
	Pagination Pagination

	// Sorting
	Sorting MessageSorting
}

// Pagination specifies limit and offset for paginated queries.
type Pagination struct {
	Limit  int
	Offset int
}

// MessageSorting specifies how to sort message results.
type MessageSorting struct {
	Field     MessageSortField
	Direction SortDirection
}

// TimeRange groups time-related filter fields.
type TimeRange struct {
	Period      string // e.g., "2024", "2024-01", "2024-01-15"
	Granularity TimeGranularity
}

// MatchesEmpty returns true if the given ViewType is in EmptyValueTargets.
func (f *MessageFilter) MatchesEmpty(v ViewType) bool {
	return f.EmptyValueTargets != nil && f.EmptyValueTargets[v]
}

// SetEmptyTarget adds the given ViewType to EmptyValueTargets.
// Initializes the map if nil.
func (f *MessageFilter) SetEmptyTarget(v ViewType) {
	if f.EmptyValueTargets == nil {
		f.EmptyValueTargets = make(map[ViewType]bool)
	}
	f.EmptyValueTargets[v] = true
}

// HasEmptyTargets returns true if any empty targets are active (set to true).
func (f *MessageFilter) HasEmptyTargets() bool {
	for _, active := range f.EmptyValueTargets {
		if active {
			return true
		}
	}
	return false
}

// Clone returns a deep copy of the MessageFilter.
// This is necessary because EmptyValueTargets is a map, and a simple struct
// copy would share the underlying map between the original and copy.
func (f MessageFilter) Clone() MessageFilter {
	clone := f
	if f.EmptyValueTargets != nil {
		clone.EmptyValueTargets = make(map[ViewType]bool, len(f.EmptyValueTargets))
		for k, v := range f.EmptyValueTargets {
			clone.EmptyValueTargets[k] = v
		}
	}
	if f.SourceIDs != nil {
		clone.SourceIDs = append([]int64(nil), f.SourceIDs...)
	}
	return clone
}

// AggregateOptions configures an aggregate query.
type AggregateOptions struct {
	// Account filter
	SourceID  *int64  // nil means all accounts
	SourceIDs []int64 // multi-source filter (collections)

	// Date range
	After  *time.Time
	Before *time.Time

	// Sorting
	SortField     SortField
	SortDirection SortDirection

	// Limit results (0 means default, typically 100)
	Limit int

	// Time-specific options
	TimeGranularity TimeGranularity

	// Filter options
	WithAttachmentsOnly   bool
	HideDeletedFromSource bool

	// Text search filter (filters aggregates to only include messages matching search)
	SearchQuery string
}

// DefaultAggregateOptions returns sensible defaults.
func DefaultAggregateOptions() AggregateOptions {
	return AggregateOptions{
		SortField:       SortByCount,
		SortDirection:   SortDesc,
		Limit:           100,
		TimeGranularity: TimeMonth,
	}
}

// AccountInfo represents a source account.
type AccountInfo struct {
	ID          int64
	SourceType  string
	Identifier  string // email address
	DisplayName string
}

// StatsOptions configures a stats query.
type StatsOptions struct {
	SourceID              *int64   // nil means all accounts
	SourceIDs             []int64  // multi-source filter (collections)
	WithAttachmentsOnly   bool     // only count messages with attachments
	HideDeletedFromSource bool     // exclude messages where deleted_from_source_at IS NOT NULL
	SearchQuery           string   // when set, stats reflect only messages matching this search
	GroupBy               ViewType // when set, search filters on this view's key columns instead of subject+sender
}
