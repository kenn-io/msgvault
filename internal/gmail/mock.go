package gmail

import (
	"context"
	"fmt"
	"sync"
)

// MockAPI is a mock implementation of the Gmail API for testing.
type MockAPI struct {
	mu sync.Mutex

	// Profile to return
	Profile *Profile

	// Labels to return
	Labels []*Label

	// Messages indexed by ID
	Messages map[string]*RawMessage

	// Message list pages - each page is a list of message IDs
	MessagePages [][]string

	// History records
	HistoryRecords []HistoryRecord
	HistoryID      uint64

	// UseRawThreadID uses the ThreadID from RawMessage instead of generating "thread_" + id
	UseRawThreadID bool

	// ListThreadIDOverride overrides the threadID returned by ListMessages for specific message IDs.
	// This allows testing the case where list returns empty threadID but raw message has one.
	ListThreadIDOverride map[string]string

	// Error injection
	ProfileError      error
	LabelsError       error
	ListMessagesError error
	GetMessageError   map[string]error // Per-message errors
	HistoryError      error

	// Call tracking for assertions
	ProfileCalls      int
	LabelsCalls       int
	ListMessagesCalls int
	LastQuery         string // Last query passed to ListMessages
	GetMessageCalls   []string
	HistoryCalls      []uint64
	TrashCalls        []string
	DeleteCalls       []string
	BatchDeleteCalls  [][]string
}

// NewMockAPI creates a new mock API with empty state.
func NewMockAPI() *MockAPI {
	return &MockAPI{
		Messages:        make(map[string]*RawMessage),
		GetMessageError: make(map[string]error),
	}
}

// GetProfile returns the mock profile.
func (m *MockAPI) GetProfile(ctx context.Context) (*Profile, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ProfileCalls++

	if m.ProfileError != nil {
		return nil, m.ProfileError
	}
	if m.Profile == nil {
		return &Profile{
			EmailAddress:  "test@example.com",
			MessagesTotal: int64(len(m.Messages)),
			HistoryID:     m.HistoryID,
		}, nil
	}
	return m.Profile, nil
}

// ListLabels returns the mock labels.
func (m *MockAPI) ListLabels(ctx context.Context) ([]*Label, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.LabelsCalls++

	if m.LabelsError != nil {
		return nil, m.LabelsError
	}
	if m.Labels == nil {
		return []*Label{
			{ID: "INBOX", Name: "INBOX", Type: "system"},
			{ID: "SENT", Name: "SENT", Type: "system"},
			{ID: "STARRED", Name: "STARRED", Type: "system"},
			{ID: "TRASH", Name: "TRASH", Type: "system"},
			{ID: "UNREAD", Name: "UNREAD", Type: "system"},
			{ID: "IMPORTANT", Name: "IMPORTANT", Type: "system"},
			{ID: "SPAM", Name: "SPAM", Type: "system"},
			{ID: "DRAFT", Name: "DRAFT", Type: "system"},
		}, nil
	}
	return m.Labels, nil
}

// ListMessages returns mock message IDs with pagination.
func (m *MockAPI) ListMessages(ctx context.Context, query string, pageToken string) (*MessageListResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ListMessagesCalls++
	m.LastQuery = query

	if m.ListMessagesError != nil {
		return nil, m.ListMessagesError
	}

	// Determine which page to return
	pageNum := 0
	if pageToken != "" {
		_, err := fmt.Sscanf(pageToken, "page_%d", &pageNum)
		if err != nil {
			return nil, fmt.Errorf("invalid page token: %s", pageToken)
		}
	}

	if len(m.MessagePages) == 0 {
		// Return all messages if no pages configured
		var messages []MessageID
		for id := range m.Messages {
			threadID := m.getListThreadID(id)
			messages = append(messages, MessageID{ID: id, ThreadID: threadID})
		}
		return &MessageListResponse{
			Messages:           messages,
			ResultSizeEstimate: int64(len(messages)),
		}, nil
	}

	if pageNum >= len(m.MessagePages) {
		return &MessageListResponse{}, nil
	}

	page := m.MessagePages[pageNum]
	messages := make([]MessageID, len(page))
	for i, id := range page {
		threadID := m.getListThreadID(id)
		messages[i] = MessageID{ID: id, ThreadID: threadID}
	}

	var nextPageToken string
	if pageNum+1 < len(m.MessagePages) {
		nextPageToken = fmt.Sprintf("page_%d", pageNum+1)
	}

	total := int64(0)
	for _, p := range m.MessagePages {
		total += int64(len(p))
	}

	return &MessageListResponse{
		Messages:           messages,
		NextPageToken:      nextPageToken,
		ResultSizeEstimate: total,
	}, nil
}

// GetMessageRaw returns a mock message.
func (m *MockAPI) GetMessageRaw(ctx context.Context, messageID string) (*RawMessage, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.GetMessageCalls = append(m.GetMessageCalls, messageID)

	if err, ok := m.GetMessageError[messageID]; ok && err != nil {
		return nil, err
	}

	msg, ok := m.Messages[messageID]
	if !ok {
		return nil, &NotFoundError{Path: "/messages/" + messageID}
	}
	return msg, nil
}

// GetMessagesRawBatch fetches multiple messages.
// Mirrors the real Client behavior: individual fetch errors leave a nil entry
// in the results slice rather than failing the entire batch. Callers must
// handle nil entries (see sync.go).
func (m *MockAPI) GetMessagesRawBatch(ctx context.Context, messageIDs []string) ([]*RawMessage, error) {
	results := make([]*RawMessage, len(messageIDs))
	for i, id := range messageIDs {
		msg, err := m.GetMessageRaw(ctx, id)
		if err != nil {
			continue
		}
		results[i] = msg
	}
	return results, nil
}

// ListHistory returns mock history records.
func (m *MockAPI) ListHistory(ctx context.Context, startHistoryID uint64, pageToken string) (*HistoryResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.HistoryCalls = append(m.HistoryCalls, startHistoryID)

	if m.HistoryError != nil {
		return nil, m.HistoryError
	}

	return &HistoryResponse{
		History:   m.HistoryRecords,
		HistoryID: m.HistoryID,
	}, nil
}

// TrashMessage records a trash call.
func (m *MockAPI) TrashMessage(ctx context.Context, messageID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.TrashCalls = append(m.TrashCalls, messageID)
	return nil
}

// DeleteMessage records a delete call.
func (m *MockAPI) DeleteMessage(ctx context.Context, messageID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.DeleteCalls = append(m.DeleteCalls, messageID)
	return nil
}

// BatchDeleteMessages records a batch delete call.
func (m *MockAPI) BatchDeleteMessages(ctx context.Context, messageIDs []string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.BatchDeleteCalls = append(m.BatchDeleteCalls, messageIDs)
	return nil
}

// Close is a no-op for the mock.
func (m *MockAPI) Close() error {
	return nil
}

// getListThreadID returns the threadID to use in ListMessages for a given message ID.
// Priority: ListThreadIDOverride > UseRawThreadID > default "thread_" + id.
func (m *MockAPI) getListThreadID(id string) string {
	// Check override first
	if m.ListThreadIDOverride != nil {
		if override, ok := m.ListThreadIDOverride[id]; ok {
			return override
		}
	}
	// Then check UseRawThreadID
	if m.UseRawThreadID {
		if msg, ok := m.Messages[id]; ok {
			return msg.ThreadID
		}
	}
	// Default
	return "thread_" + id
}

// SetupMessages adds multiple pre-built RawMessage values to the mock store
// in a thread-safe manner. Nil entries in the input slice are silently skipped.
func (m *MockAPI) SetupMessages(msgs ...*RawMessage) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Messages == nil {
		m.Messages = make(map[string]*RawMessage)
	}
	for _, msg := range msgs {
		if msg == nil {
			continue
		}
		m.Messages[msg.ID] = msg
	}
}

// AddMessage adds a message to the mock store.
func (m *MockAPI) AddMessage(id string, raw []byte, labelIDs []string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.Messages[id] = &RawMessage{
		ID:           id,
		ThreadID:     "thread_" + id,
		LabelIDs:     labelIDs,
		Raw:          raw,
		SizeEstimate: int64(len(raw)),
		InternalDate: 1704067200000, // 2024-01-01 00:00:00 UTC
	}
}

// Reset clears all state and call tracking.
func (m *MockAPI) Reset() {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.Messages = make(map[string]*RawMessage)
	m.MessagePages = nil
	m.HistoryRecords = nil
	m.GetMessageError = make(map[string]error)
	m.ListThreadIDOverride = nil
	m.UseRawThreadID = false

	m.ProfileCalls = 0
	m.LabelsCalls = 0
	m.ListMessagesCalls = 0
	m.LastQuery = ""
	m.GetMessageCalls = nil
	m.HistoryCalls = nil
	m.TrashCalls = nil
	m.DeleteCalls = nil
	m.BatchDeleteCalls = nil
}

// Ensure MockAPI implements API interface.
var _ API = (*MockAPI)(nil)
