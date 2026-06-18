// Package gmail provides a Gmail API client with rate limiting and retry logic.
package gmail

import "context"

// AccountReader provides read access to account-level Gmail data.
type AccountReader interface {
	// GetProfile returns the authenticated user's profile.
	GetProfile(ctx context.Context) (*Profile, error)

	// ListLabels returns all labels for the account.
	ListLabels(ctx context.Context) ([]*Label, error)
}

// MessageReader provides read access to Gmail messages and history.
type MessageReader interface {
	// ListMessages returns message IDs matching the query.
	// Use pageToken for pagination. Returns next page token if more results exist.
	ListMessages(ctx context.Context, query string, pageToken string) (*MessageListResponse, error)

	// GetMessageRaw fetches a single message with raw MIME data.
	GetMessageRaw(ctx context.Context, messageID string) (*RawMessage, error)

	// GetMessagesRawBatch fetches multiple messages in parallel with rate limiting.
	// Returns results in the same order as input IDs. Failed fetches return nil.
	GetMessagesRawBatch(ctx context.Context, messageIDs []string) ([]*RawMessage, error)

	// ListHistory returns changes since the given history ID.
	ListHistory(ctx context.Context, startHistoryID uint64, pageToken string) (*HistoryResponse, error)
}

// MessageDeleter provides write operations for deleting Gmail messages.
type MessageDeleter interface {
	// TrashMessage moves a message to trash (recoverable for 30 days).
	TrashMessage(ctx context.Context, messageID string) error

	// DeleteMessage permanently deletes a message.
	DeleteMessage(ctx context.Context, messageID string) error

	// BatchDeleteMessages permanently deletes multiple messages (max 1000).
	BatchDeleteMessages(ctx context.Context, messageIDs []string) error
}

// API defines the interface for Gmail operations.
// This interface enables mocking for tests without hitting the real API.
type API interface {
	AccountReader
	MessageReader
	MessageDeleter

	// Close releases any resources held by the client.
	Close() error
}

// Profile represents a Gmail user profile.
type Profile struct {
	EmailAddress  string
	MessagesTotal int64
	ThreadsTotal  int64
	HistoryID     uint64
}

// Label represents a Gmail label.
type Label struct {
	ID                    string
	Name                  string
	Type                  string // "system" or "user"
	MessagesTotal         int64
	MessagesUnread        int64
	MessageListVisibility string
	LabelListVisibility   string
}

// MessageListResponse contains a page of message IDs.
type MessageListResponse struct {
	Messages           []MessageID
	NextPageToken      string
	ResultSizeEstimate int64
}

// MessageID represents a message reference from list operations.
type MessageID struct {
	ID       string
	ThreadID string
}

// RawMessage contains the raw MIME data for a message.
type RawMessage struct {
	ID           string
	ThreadID     string
	LabelIDs     []string
	Snippet      string
	HistoryID    uint64
	InternalDate int64 // Unix milliseconds
	SizeEstimate int64
	Raw          []byte // Decoded from base64url
}

// RawMessageBatchResult is one per-message result from a batch raw fetch.
// Message is nil when the fetch failed; Err preserves the per-message cause.
type RawMessageBatchResult struct {
	ID      string
	Message *RawMessage
	Err     error
}

// HistoryResponse contains changes since a history ID.
type HistoryResponse struct {
	History       []HistoryRecord
	NextPageToken string
	HistoryID     uint64
}

// HistoryRecord represents a single history change.
type HistoryRecord struct {
	ID              uint64
	MessagesAdded   []HistoryMessage
	MessagesDeleted []HistoryMessage
	LabelsAdded     []HistoryLabelChange
	LabelsRemoved   []HistoryLabelChange
}

// HistoryMessage represents a message in history.
type HistoryMessage struct {
	Message MessageID
}

// HistoryLabelChange represents a label change in history.
type HistoryLabelChange struct {
	Message  MessageID
	LabelIDs []string
}
