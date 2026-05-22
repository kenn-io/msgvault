package tui

import (
	"errors"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"go.kenn.io/msgvault/internal/query"
)

// =============================================================================
// Init Tests
// =============================================================================

func TestModel_Init_ReturnsNonNilCmd(t *testing.T) {
	model := NewBuilder().Build()
	cmd := model.Init()
	if cmd == nil {
		t.Error("Init returned nil command, expected batch command for initial data loading")
	}
}

func TestModel_Init_SetsLoadingState(t *testing.T) {
	// A fresh model via New() starts with loading=true
	engine := newMockEngine(MockConfig{})
	model := New(engine, Options{DataDir: "/tmp/test", Version: "test123"})
	if !model.loading {
		t.Error("expected loading=true for fresh model")
	}
}

// =============================================================================
// New (Constructor) Tests
// =============================================================================

func TestNew_SetsDefaults(t *testing.T) {
	engine := newMockEngine(MockConfig{})
	model := New(engine, Options{DataDir: "/tmp/test", Version: "v1.0"})

	if model.version != "v1.0" {
		t.Errorf("expected version v1.0, got %s", model.version)
	}
	if model.aggregateLimit != defaultAggregateLimit {
		t.Errorf("expected aggregateLimit %d, got %d", defaultAggregateLimit, model.aggregateLimit)
	}
	if model.threadMessageLimit != defaultThreadMessageLimit {
		t.Errorf("expected threadMessageLimit %d, got %d", defaultThreadMessageLimit, model.threadMessageLimit)
	}
	if model.pageSize != 20 {
		t.Errorf("expected default pageSize 20, got %d", model.pageSize)
	}
	if model.level != levelAggregates {
		t.Errorf("expected initial level levelAggregates, got %v", model.level)
	}
	if model.viewType != query.ViewSenders {
		t.Errorf("expected initial viewType ViewSenders, got %v", model.viewType)
	}
	if model.sortField != query.SortByCount {
		t.Errorf("expected initial sortField SortByCount, got %v", model.sortField)
	}
	if model.sortDirection != query.SortDesc {
		t.Errorf("expected initial sortDirection SortDesc, got %v", model.sortDirection)
	}
}

func TestNew_OverridesLimits(t *testing.T) {
	engine := newMockEngine(MockConfig{})
	model := New(engine, Options{
		DataDir:            "/tmp/test",
		Version:            "test",
		AggregateLimit:     100,
		ThreadMessageLimit: 50,
	})

	if model.aggregateLimit != 100 {
		t.Errorf("expected aggregateLimit 100, got %d", model.aggregateLimit)
	}
	if model.threadMessageLimit != 50 {
		t.Errorf("expected threadMessageLimit 50, got %d", model.threadMessageLimit)
	}
}

// =============================================================================
// dataLoadedMsg Tests - State Transitions
// =============================================================================

func TestModel_Update_DataLoaded_TransitionsFromLoading(t *testing.T) {
	model := NewBuilder().WithLoading(true).Build()
	rows := []query.AggregateRow{{Key: "test@example.com", Count: 10}}

	msg := dataLoadedMsg{rows: rows, requestID: model.aggregateRequestID}
	updatedModel, _ := model.Update(msg)
	m := updatedModel.(Model)

	if m.loading {
		t.Error("expected loading=false after data load")
	}
	if len(m.rows) != 1 {
		t.Errorf("expected 1 row, got %d", len(m.rows))
	}
	if m.rows[0].Key != "test@example.com" {
		t.Errorf("expected key test@example.com, got %s", m.rows[0].Key)
	}
}

func TestModel_Update_DataLoaded_ResetsCursor(t *testing.T) {
	model := NewBuilder().
		WithRows(makeRows(10)...).
		WithLoading(true).
		Build()
	model.cursor = 5
	model.scrollOffset = 3

	newRows := []query.AggregateRow{{Key: "new@example.com", Count: 1}}
	msg := dataLoadedMsg{rows: newRows, requestID: model.aggregateRequestID}
	updatedModel, _ := model.Update(msg)
	m := updatedModel.(Model)

	if m.cursor != 0 {
		t.Errorf("expected cursor=0 after data load, got %d", m.cursor)
	}
	if m.scrollOffset != 0 {
		t.Errorf("expected scrollOffset=0 after data load, got %d", m.scrollOffset)
	}
}

func TestModel_Update_DataLoaded_PreservesPositionWhenRestoring(t *testing.T) {
	model := NewBuilder().
		WithRows(makeRows(10)...).
		WithLoading(true).
		Build()
	model.cursor = 5
	model.scrollOffset = 3
	model.restorePosition = true

	newRows := makeRows(10)
	msg := dataLoadedMsg{rows: newRows, requestID: model.aggregateRequestID}
	updatedModel, _ := model.Update(msg)
	m := updatedModel.(Model)

	if m.cursor != 5 {
		t.Errorf("expected cursor=5 (preserved), got %d", m.cursor)
	}
	if m.scrollOffset != 3 {
		t.Errorf("expected scrollOffset=3 (preserved), got %d", m.scrollOffset)
	}
	if m.restorePosition {
		t.Error("expected restorePosition to be cleared after use")
	}
}

func TestModel_Update_DataLoaded_IgnoresStaleResponse(t *testing.T) {
	model := NewBuilder().WithLoading(true).Build()
	model.aggregateRequestID = 5

	// Send a stale response with old request ID
	staleMsg := dataLoadedMsg{
		rows:      []query.AggregateRow{{Key: "stale", Count: 1}},
		requestID: 3, // Old request ID
	}
	updatedModel, _ := model.Update(staleMsg)
	m := updatedModel.(Model)

	// Should still be loading, no data set
	if !m.loading {
		t.Error("expected loading=true (stale response should be ignored)")
	}
	if len(m.rows) != 0 {
		t.Errorf("expected no rows (stale response), got %d", len(m.rows))
	}
}

func TestModel_Update_DataLoaded_ClearsTransitionBuffer(t *testing.T) {
	model := NewBuilder().WithLoading(true).Build()
	model.transitionBuffer = "frozen view"

	msg := dataLoadedMsg{
		rows:      []query.AggregateRow{{Key: "test", Count: 1}},
		requestID: model.aggregateRequestID,
	}
	updatedModel, _ := model.Update(msg)
	m := updatedModel.(Model)

	if m.transitionBuffer != "" {
		t.Error("expected transitionBuffer to be cleared after data load")
	}
}

// =============================================================================
// Error Handling Tests
// =============================================================================

func TestModel_Update_DataLoaded_HandlesError(t *testing.T) {
	model := NewBuilder().WithLoading(true).Build()

	msg := dataLoadedMsg{
		err:       errors.New("database connection failed"),
		requestID: model.aggregateRequestID,
	}
	updatedModel, _ := model.Update(msg)
	m := updatedModel.(Model)

	if m.loading {
		t.Error("expected loading=false after error")
	}
	if m.err == nil {
		t.Error("expected err to be set")
	}
	if m.err.Error() != "database connection failed" {
		t.Errorf("unexpected error message: %v", m.err)
	}
}

func TestModel_Update_StatsLoaded_HandlesError(t *testing.T) {
	model := NewBuilder().Build()
	originalStats := model.stats

	msg := statsLoadedMsg{err: errors.New("stats query failed")}
	updatedModel, _ := model.Update(msg)
	m := updatedModel.(Model)

	// Stats should remain unchanged on error
	if m.stats != originalStats {
		t.Error("stats should not change on error")
	}
}

func TestModel_Update_AccountsLoaded_HandlesError(t *testing.T) {
	model := NewBuilder().Build()

	msg := accountsLoadedMsg{err: errors.New("accounts query failed")}
	updatedModel, _ := model.Update(msg)
	m := updatedModel.(Model)

	// Accounts should remain empty on error
	if len(m.accounts) != 0 {
		t.Errorf("expected no accounts on error, got %d", len(m.accounts))
	}
}

func TestModel_Update_MessagesLoaded_HandlesError(t *testing.T) {
	model := NewBuilder().
		WithLevel(levelMessageList).
		WithLoading(true).
		Build()

	msg := messagesLoadedMsg{
		err:       errors.New("messages query failed"),
		requestID: model.loadRequestID,
	}
	updatedModel, _ := model.Update(msg)
	m := updatedModel.(Model)

	if m.loading {
		t.Error("expected loading=false after error")
	}
	if m.err == nil {
		t.Error("expected err to be set")
	}
}

func TestModel_Update_SearchResults_HandlesError(t *testing.T) {
	model := NewBuilder().
		WithLevel(levelMessageList).
		WithLoading(true).
		Build()
	model.searchRequestID = 1

	msg := searchResultsMsg{
		err:       errors.New("search failed"),
		requestID: 1,
	}
	updatedModel, _ := model.Update(msg)
	m := updatedModel.(Model)

	if m.loading {
		t.Error("expected loading=false after search error")
	}
	if m.err == nil {
		t.Error("expected err to be set after search error")
	}
}

// =============================================================================
// Search Results Pagination Tests
// =============================================================================

func TestModel_Update_SearchResults_ReplacesMessages(t *testing.T) {
	model := NewBuilder().
		WithMessages(makeMessages(5)...).
		WithLevel(levelMessageList).
		WithLoading(true).
		Build()
	model.cursor = 3
	model.scrollOffset = 2
	model.searchRequestID = 1

	newMessages := makeMessages(10)
	msg := searchResultsMsg{
		messages:   newMessages,
		totalCount: 100,
		requestID:  1,
		append:     false, // Replace mode
	}
	updatedModel, _ := model.Update(msg)
	m := updatedModel.(Model)

	if len(m.messages) != 10 {
		t.Errorf("expected 10 messages, got %d", len(m.messages))
	}
	if m.cursor != 0 {
		t.Errorf("expected cursor=0 after replace, got %d", m.cursor)
	}
	if m.scrollOffset != 0 {
		t.Errorf("expected scrollOffset=0 after replace, got %d", m.scrollOffset)
	}
	if m.searchTotalCount != 100 {
		t.Errorf("expected searchTotalCount=100, got %d", m.searchTotalCount)
	}
	if m.searchOffset != 10 {
		t.Errorf("expected searchOffset=10, got %d", m.searchOffset)
	}
}

func TestModel_Update_SearchResults_AppendsMessages(t *testing.T) {
	existingMessages := makeMessages(10)
	model := NewBuilder().
		WithMessages(existingMessages...).
		WithLevel(levelMessageList).
		Build()
	model.cursor = 5
	model.scrollOffset = 2
	model.searchRequestID = 1
	model.searchOffset = 10
	model.searchTotalCount = 100
	model.loading = true

	newMessages := makeMessages(10)
	// Adjust IDs to not conflict
	for i := range newMessages {
		newMessages[i].ID = int64(i + 11)
		newMessages[i].Subject = "Subject " + string(rune('A'+i))
	}

	msg := searchResultsMsg{
		messages:   newMessages,
		totalCount: 100,
		requestID:  1,
		append:     true, // Append mode
	}
	updatedModel, _ := model.Update(msg)
	m := updatedModel.(Model)

	if len(m.messages) != 20 {
		t.Errorf("expected 20 messages (10+10), got %d", len(m.messages))
	}
	// Cursor and scroll should NOT reset on append
	if m.cursor != 5 {
		t.Errorf("expected cursor=5 (preserved on append), got %d", m.cursor)
	}
	if m.searchOffset != 20 {
		t.Errorf("expected searchOffset=20 after append, got %d", m.searchOffset)
	}
}

func TestModel_Update_SearchResults_SetsContextStats(t *testing.T) {
	model := NewBuilder().
		WithLevel(levelMessageList).
		WithLoading(true).
		Build()
	model.searchRequestID = 1

	msg := searchResultsMsg{
		messages:   makeMessages(5),
		totalCount: 50,
		requestID:  1,
		append:     false,
	}
	updatedModel, _ := model.Update(msg)
	m := updatedModel.(Model)

	if m.contextStats == nil {
		t.Fatal("expected contextStats to be set")
	}
	if m.contextStats.MessageCount != 50 {
		t.Errorf("expected contextStats.MessageCount=50, got %d", m.contextStats.MessageCount)
	}
}

func TestModel_Update_SearchResults_IgnoresStaleResponse(t *testing.T) {
	model := NewBuilder().
		WithLevel(levelMessageList).
		WithLoading(true).
		Build()
	model.searchRequestID = 5

	msg := searchResultsMsg{
		messages:  makeMessages(10),
		requestID: 3, // Old request ID
	}
	updatedModel, _ := model.Update(msg)
	m := updatedModel.(Model)

	if !m.loading {
		t.Error("expected loading=true (stale response should be ignored)")
	}
	if len(m.messages) != 0 {
		t.Errorf("expected no messages (stale response), got %d", len(m.messages))
	}
}

// =============================================================================
// Window Size Tests
// =============================================================================

func TestModel_Update_WindowSize_UpdatesDimensions(t *testing.T) {
	model := NewBuilder().WithSize(100, 24).Build()

	msg := tea.WindowSizeMsg{Width: 120, Height: 40}
	updatedModel, _ := model.Update(msg)
	m := updatedModel.(Model)

	if m.width != 120 {
		t.Errorf("expected width=120, got %d", m.width)
	}
	if m.height != 40 {
		t.Errorf("expected height=40, got %d", m.height)
	}
}

func TestModel_Update_WindowSize_RecalculatesPageSize(t *testing.T) {
	model := NewBuilder().WithSize(100, 24).Build()

	msg := tea.WindowSizeMsg{Width: 100, Height: 50}
	updatedModel, _ := model.Update(msg)
	m := updatedModel.(Model)

	expectedPageSize := 50 - headerFooterLines
	if m.pageSize != expectedPageSize {
		t.Errorf("expected pageSize=%d, got %d", expectedPageSize, m.pageSize)
	}
}

func TestModel_Update_WindowSize_ClampsNegativeDimensions(t *testing.T) {
	model := NewBuilder().WithSize(100, 24).Build()

	msg := tea.WindowSizeMsg{Width: -10, Height: -5}
	updatedModel, _ := model.Update(msg)
	m := updatedModel.(Model)

	if m.width < 0 {
		t.Errorf("expected width >= 0, got %d", m.width)
	}
	if m.height < 0 {
		t.Errorf("expected height >= 0, got %d", m.height)
	}
}

func TestModel_Update_WindowSize_ClearsTransitionBuffer(t *testing.T) {
	model := NewBuilder().Build()
	model.transitionBuffer = "frozen view"

	msg := tea.WindowSizeMsg{Width: 100, Height: 50}
	updatedModel, _ := model.Update(msg)
	m := updatedModel.(Model)

	if m.transitionBuffer != "" {
		t.Error("expected transitionBuffer to be cleared on resize")
	}
}

// =============================================================================
// Stats and Accounts Loaded Tests
// =============================================================================

func TestModel_Update_StatsLoaded_SetsStats(t *testing.T) {
	model := NewBuilder().Build()
	stats := &query.TotalStats{MessageCount: 1000, TotalSize: 5000000, AttachmentCount: 50}

	msg := statsLoadedMsg{stats: stats}
	updatedModel, _ := model.Update(msg)
	m := updatedModel.(Model)

	if m.stats != stats {
		t.Error("expected stats to be set")
	}
	if m.stats.MessageCount != 1000 {
		t.Errorf("expected MessageCount=1000, got %d", m.stats.MessageCount)
	}
}

func TestModel_Update_AccountsLoaded_SetsAccounts(t *testing.T) {
	model := NewBuilder().Build()
	accounts := []query.AccountInfo{
		{ID: 1, Identifier: "user1@gmail.com"},
		{ID: 2, Identifier: "user2@gmail.com"},
	}

	msg := accountsLoadedMsg{accounts: accounts}
	updatedModel, _ := model.Update(msg)
	m := updatedModel.(Model)

	if len(m.accounts) != 2 {
		t.Errorf("expected 2 accounts, got %d", len(m.accounts))
	}
	if m.accounts[0].Identifier != "user1@gmail.com" {
		t.Errorf("expected first account user1@gmail.com, got %s", m.accounts[0].Identifier)
	}
}

// =============================================================================
// Messages Loaded Tests
// =============================================================================

func TestModel_Update_MessagesLoaded_SetsMessages(t *testing.T) {
	model := NewBuilder().
		WithLevel(levelMessageList).
		WithLoading(true).
		Build()

	messages := makeMessages(5)
	msg := messagesLoadedMsg{
		messages:  messages,
		requestID: model.loadRequestID,
	}
	updatedModel, _ := model.Update(msg)
	m := updatedModel.(Model)

	if m.loading {
		t.Error("expected loading=false after messages loaded")
	}
	if len(m.messages) != 5 {
		t.Errorf("expected 5 messages, got %d", len(m.messages))
	}
}

func TestModel_Update_MessagesLoaded_IgnoresStaleResponse(t *testing.T) {
	model := NewBuilder().
		WithLevel(levelMessageList).
		WithLoading(true).
		Build()
	model.loadRequestID = 5

	msg := messagesLoadedMsg{
		messages:  makeMessages(10),
		requestID: 3, // Stale
	}
	updatedModel, _ := model.Update(msg)
	m := updatedModel.(Model)

	if !m.loading {
		t.Error("expected loading=true (stale response)")
	}
}

// =============================================================================
// Message Detail Loaded Tests
// =============================================================================

func TestModel_Update_MessageDetailLoaded_SetsDetail(t *testing.T) {
	model := NewBuilder().
		WithLevel(levelMessageDetail).
		WithLoading(true).
		Build()
	model.width = 100
	model.height = 40

	detail := &query.MessageDetail{
		ID:       1,
		Subject:  "Test Subject",
		BodyText: "Test body content",
	}
	msg := messageDetailLoadedMsg{
		detail:    detail,
		requestID: model.detailRequestID,
	}
	updatedModel, _ := model.Update(msg)
	m := updatedModel.(Model)

	if m.loading {
		t.Error("expected loading=false after detail loaded")
	}
	if m.messageDetail == nil {
		t.Fatal("expected messageDetail to be set")
	}
	if m.messageDetail.Subject != "Test Subject" {
		t.Errorf("expected subject 'Test Subject', got '%s'", m.messageDetail.Subject)
	}
	if m.detailScroll != 0 {
		t.Errorf("expected detailScroll=0, got %d", m.detailScroll)
	}
}

func TestModel_Update_MessageDetailLoaded_IgnoresStaleResponse(t *testing.T) {
	model := NewBuilder().
		WithLevel(levelMessageDetail).
		WithLoading(true).
		Build()
	model.detailRequestID = 5

	detail := &query.MessageDetail{ID: 1, Subject: "Stale"}
	msg := messageDetailLoadedMsg{
		detail:    detail,
		requestID: 3, // Stale
	}
	updatedModel, _ := model.Update(msg)
	m := updatedModel.(Model)

	if !m.loading {
		t.Error("expected loading=true (stale response)")
	}
	if m.messageDetail != nil {
		t.Error("expected messageDetail to remain nil")
	}
}

// =============================================================================
// Update Check Tests
// =============================================================================

func TestModel_Update_UpdateCheck_SetsVersion(t *testing.T) {
	model := NewBuilder().Build()

	msg := updateCheckMsg{version: "v2.0.0", isDevBuild: false}
	updatedModel, _ := model.Update(msg)
	m := updatedModel.(Model)

	if m.updateAvailable != "v2.0.0" {
		t.Errorf("expected updateAvailable='v2.0.0', got '%s'", m.updateAvailable)
	}
	if m.updateIsDevBuild {
		t.Error("expected updateIsDevBuild=false")
	}
}

func TestModel_Update_UpdateCheck_SetsDevBuild(t *testing.T) {
	model := NewBuilder().Build()

	msg := updateCheckMsg{version: "", isDevBuild: true}
	updatedModel, _ := model.Update(msg)
	m := updatedModel.(Model)

	if !m.updateIsDevBuild {
		t.Error("expected updateIsDevBuild=true")
	}
}

// =============================================================================
// Search Filter with Context Stats Tests
// =============================================================================

func TestModel_Update_DataLoaded_SetsContextStatsWhenSearchActive(t *testing.T) {
	model := NewBuilder().WithLoading(true).Build()
	model.searchQuery = "test query"

	filteredStats := &query.TotalStats{MessageCount: 50, TotalSize: 1000, AttachmentCount: 5}
	msg := dataLoadedMsg{
		rows:          []query.AggregateRow{{Key: "test", Count: 50}},
		filteredStats: filteredStats,
		requestID:     model.aggregateRequestID,
	}
	updatedModel, _ := model.Update(msg)
	m := updatedModel.(Model)

	if m.contextStats == nil {
		t.Fatal("expected contextStats to be set when search is active")
	}
	if m.contextStats.MessageCount != 50 {
		t.Errorf("expected contextStats.MessageCount=50, got %d", m.contextStats.MessageCount)
	}
}

func TestModel_Update_DataLoaded_ClearsContextStatsAtTopLevelWithoutSearch(t *testing.T) {
	model := NewBuilder().WithLoading(true).Build()
	model.contextStats = &query.TotalStats{MessageCount: 100} // Pre-existing
	model.searchQuery = ""                                    // No search
	model.level = levelAggregates

	msg := dataLoadedMsg{
		rows:      []query.AggregateRow{{Key: "test", Count: 50}},
		requestID: model.aggregateRequestID,
	}
	updatedModel, _ := model.Update(msg)
	m := updatedModel.(Model)

	if m.contextStats != nil {
		t.Error("expected contextStats to be cleared at top level without search")
	}
}

// =============================================================================
// Thread Messages Loaded Tests
// =============================================================================

func TestModel_Update_ThreadMessagesLoaded_SetsMessages(t *testing.T) {
	model := NewBuilder().
		WithLevel(levelThreadView).
		WithLoading(true).
		Build()
	model.loadRequestID = 1

	messages := makeMessages(5)
	msg := threadMessagesLoadedMsg{
		messages:       messages,
		conversationID: 42,
		truncated:      false,
		requestID:      1,
	}
	updatedModel, _ := model.Update(msg)
	m := updatedModel.(Model)

	if m.loading {
		t.Error("expected loading=false after thread messages loaded")
	}
	if len(m.threadMessages) != 5 {
		t.Errorf("expected 5 thread messages, got %d", len(m.threadMessages))
	}
	if m.threadConversationID != 42 {
		t.Errorf("expected conversationID=42, got %d", m.threadConversationID)
	}
	if m.threadTruncated {
		t.Error("expected threadTruncated=false")
	}
	// Should reset cursor/scroll
	if m.threadCursor != 0 {
		t.Errorf("expected threadCursor=0, got %d", m.threadCursor)
	}
	if m.threadScrollOffset != 0 {
		t.Errorf("expected threadScrollOffset=0, got %d", m.threadScrollOffset)
	}
}

func TestModel_Update_ThreadMessagesLoaded_IgnoresStaleResponse(t *testing.T) {
	model := NewBuilder().
		WithLevel(levelThreadView).
		WithLoading(true).
		Build()
	model.loadRequestID = 5

	msg := threadMessagesLoadedMsg{
		messages:       makeMessages(10),
		conversationID: 42,
		requestID:      3, // Stale
	}
	updatedModel, _ := model.Update(msg)
	m := updatedModel.(Model)

	if !m.loading {
		t.Error("expected loading=true (stale response should be ignored)")
	}
	if len(m.threadMessages) != 0 {
		t.Errorf("expected no thread messages (stale response), got %d", len(m.threadMessages))
	}
}

func TestModel_Update_ThreadMessagesLoaded_ClearsTransitionBuffer(t *testing.T) {
	model := NewBuilder().
		WithLevel(levelThreadView).
		WithLoading(true).
		Build()
	model.transitionBuffer = "frozen view"
	model.loadRequestID = 1

	msg := threadMessagesLoadedMsg{
		messages:       makeMessages(3),
		conversationID: 42,
		requestID:      1,
	}
	updatedModel, _ := model.Update(msg)
	m := updatedModel.(Model)

	if m.transitionBuffer != "" {
		t.Error("expected transitionBuffer to be cleared after thread messages load")
	}
}

func TestModel_Update_ThreadMessagesLoaded_ResetsCursorAndScroll(t *testing.T) {
	model := NewBuilder().
		WithLevel(levelThreadView).
		WithLoading(true).
		Build()
	model.loadRequestID = 1
	// Set non-zero values to verify reset
	model.threadCursor = 5
	model.threadScrollOffset = 3

	msg := threadMessagesLoadedMsg{
		messages:       makeMessages(10),
		conversationID: 42,
		requestID:      1,
	}
	updatedModel, _ := model.Update(msg)
	m := updatedModel.(Model)

	if m.threadCursor != 0 {
		t.Errorf("expected threadCursor=0 after load, got %d", m.threadCursor)
	}
	if m.threadScrollOffset != 0 {
		t.Errorf("expected threadScrollOffset=0 after load, got %d", m.threadScrollOffset)
	}
}

func TestModel_Update_ThreadMessagesLoaded_HandlesError(t *testing.T) {
	model := NewBuilder().
		WithLevel(levelThreadView).
		WithLoading(true).
		Build()
	model.loadRequestID = 1

	msg := threadMessagesLoadedMsg{
		err:       errors.New("thread load failed"),
		requestID: 1,
	}
	updatedModel, _ := model.Update(msg)
	m := updatedModel.(Model)

	if m.loading {
		t.Error("expected loading=false after error")
	}
	if m.err == nil {
		t.Error("expected err to be set")
	}
	if m.err.Error() != "thread load failed" {
		t.Errorf("unexpected error message: %v", m.err)
	}
}

func TestModel_Update_ThreadMessagesLoaded_SetsTruncatedFlag(t *testing.T) {
	model := NewBuilder().
		WithLevel(levelThreadView).
		WithLoading(true).
		Build()
	model.loadRequestID = 1

	msg := threadMessagesLoadedMsg{
		messages:       makeMessages(1000),
		conversationID: 42,
		truncated:      true,
		requestID:      1,
	}
	updatedModel, _ := model.Update(msg)
	m := updatedModel.(Model)

	if !m.threadTruncated {
		t.Error("expected threadTruncated=true when more messages exist")
	}
}

// =============================================================================
// Window Size Tests - Detail View with Search
// =============================================================================

func TestModel_Update_WindowSize_RecalculatesDetailSearchMatches(t *testing.T) {
	// Create a message detail with multi-line body that wrapping will affect
	detail := &query.MessageDetail{
		ID:       1,
		Subject:  "Test Subject",
		BodyText: "This is a test body with a searchterm in it.\nAnother line here.\nAnd a third line with searchterm again.",
	}

	model := NewBuilder().
		WithLevel(levelMessageDetail).
		WithDetail(detail).
		WithSize(100, 40).
		Build()
	model.width = 100
	model.height = 40
	model.loading = false

	// Set up detail search state
	model.detailSearchQuery = "searchterm"
	model.findDetailMatches()
	originalMatchCount := len(model.detailSearchMatches)
	model.detailSearchMatchIndex = 1 // Point to second match

	// Resize the window - this should trigger re-wrapping and match recomputation
	msg := tea.WindowSizeMsg{Width: 60, Height: 30}
	updatedModel, _ := model.Update(msg)
	m := updatedModel.(Model)

	// Verify dimensions updated
	if m.width != 60 {
		t.Errorf("expected width=60, got %d", m.width)
	}
	if m.height != 30 {
		t.Errorf("expected height=30, got %d", m.height)
	}

	// Verify search matches were recomputed (the function should have been called)
	// The match count may differ due to different wrapping
	if m.detailSearchQuery != "searchterm" {
		t.Error("detailSearchQuery should be preserved")
	}

	// Match index should be clamped to valid range
	if len(m.detailSearchMatches) > 0 {
		if m.detailSearchMatchIndex >= len(m.detailSearchMatches) {
			t.Errorf("detailSearchMatchIndex %d should be < match count %d",
				m.detailSearchMatchIndex, len(m.detailSearchMatches))
		}
	} else {
		if m.detailSearchMatchIndex != 0 {
			t.Errorf("expected detailSearchMatchIndex=0 when no matches, got %d",
				m.detailSearchMatchIndex)
		}
	}

	// Original match count check to ensure the test is meaningful
	if originalMatchCount == 0 {
		t.Error("test setup error: expected at least one match in original search")
	}
}

func TestModel_Update_WindowSize_ClampsMatchIndexWhenMatchesDecrease(t *testing.T) {
	// Create detail with content that will have matches
	detail := &query.MessageDetail{
		ID:       1,
		Subject:  "Test",
		BodyText: "line1 keyword\nline2 keyword\nline3 keyword",
	}

	model := NewBuilder().
		WithLevel(levelMessageDetail).
		WithDetail(detail).
		WithSize(100, 40).
		Build()
	model.loading = false

	// Set up search with matches
	model.detailSearchQuery = "keyword"
	model.findDetailMatches()

	// Simulate having match index pointing beyond what might exist after resize
	// (in real scenarios, wrapping changes could affect line indices)
	if len(model.detailSearchMatches) > 0 {
		model.detailSearchMatchIndex = len(model.detailSearchMatches) - 1
	}

	// Resize - should preserve valid match index or clamp it
	msg := tea.WindowSizeMsg{Width: 80, Height: 35}
	updatedModel, _ := model.Update(msg)
	m := updatedModel.(Model)

	// Match index should never exceed matches length
	if len(m.detailSearchMatches) > 0 && m.detailSearchMatchIndex >= len(m.detailSearchMatches) {
		t.Errorf("detailSearchMatchIndex %d exceeds match count %d",
			m.detailSearchMatchIndex, len(m.detailSearchMatches))
	}
}

func TestModel_Update_WindowSize_NoMatchesAfterResize(t *testing.T) {
	detail := &query.MessageDetail{
		ID:       1,
		Subject:  "Test",
		BodyText: "some text here",
	}

	model := NewBuilder().
		WithLevel(levelMessageDetail).
		WithDetail(detail).
		WithSize(100, 40).
		Build()
	model.loading = false

	// Set up search with no matches
	model.detailSearchQuery = "nonexistent"
	model.findDetailMatches()
	model.detailSearchMatchIndex = 5 // Invalid index

	// Resize
	msg := tea.WindowSizeMsg{Width: 80, Height: 35}
	updatedModel, _ := model.Update(msg)
	m := updatedModel.(Model)

	// When no matches, index should be 0
	if len(m.detailSearchMatches) == 0 && m.detailSearchMatchIndex != 0 {
		t.Errorf("expected detailSearchMatchIndex=0 when no matches, got %d",
			m.detailSearchMatchIndex)
	}
}

// =============================================================================
// Append Search Results with Unknown Total Tests
// =============================================================================

func TestModel_Update_SearchResults_AppendsUpdatesContextStatsWhenTotalUnknown(t *testing.T) {
	existingMessages := makeMessages(10)
	model := NewBuilder().
		WithMessages(existingMessages...).
		WithLevel(levelMessageList).
		WithContextStats(&query.TotalStats{MessageCount: 10, TotalSize: 1000}).
		Build()
	model.searchRequestID = 1
	model.searchOffset = 10
	model.searchTotalCount = -1 // Unknown total
	model.loading = true

	newMessages := makeMessages(5)
	// Adjust IDs to not conflict
	for i := range newMessages {
		newMessages[i].ID = int64(i + 11)
	}

	msg := searchResultsMsg{
		messages:   newMessages,
		totalCount: -1, // Still unknown
		requestID:  1,
		append:     true,
	}
	updatedModel, _ := model.Update(msg)
	m := updatedModel.(Model)

	// Total messages should be 15 (10 + 5)
	if len(m.messages) != 15 {
		t.Errorf("expected 15 messages, got %d", len(m.messages))
	}

	// contextStats.MessageCount should be updated to reflect loaded count
	if m.contextStats == nil {
		t.Fatal("expected contextStats to be set")
	}
	if m.contextStats.MessageCount != 15 {
		t.Errorf("expected contextStats.MessageCount=15, got %d", m.contextStats.MessageCount)
	}
}

func TestModel_Update_SearchResults_AppendDoesNotUpdateContextStatsWhenTotalKnown(t *testing.T) {
	existingMessages := makeMessages(10)
	model := NewBuilder().
		WithMessages(existingMessages...).
		WithLevel(levelMessageList).
		WithContextStats(&query.TotalStats{MessageCount: 100}).
		Build()
	model.searchRequestID = 1
	model.searchOffset = 10
	model.searchTotalCount = 100 // Known total
	model.loading = true

	newMessages := makeMessages(5)
	for i := range newMessages {
		newMessages[i].ID = int64(i + 11)
	}

	msg := searchResultsMsg{
		messages:   newMessages,
		totalCount: 100,
		requestID:  1,
		append:     true,
	}
	updatedModel, _ := model.Update(msg)
	m := updatedModel.(Model)

	// contextStats.MessageCount should remain at known total (100), not loaded count (15)
	if m.contextStats == nil {
		t.Fatal("expected contextStats to be set")
	}
	if m.contextStats.MessageCount != 100 {
		t.Errorf("expected contextStats.MessageCount=100 (known total), got %d", m.contextStats.MessageCount)
	}
}

func TestModel_Update_SearchResults_AppendWithNilContextStats(t *testing.T) {
	existingMessages := makeMessages(10)
	model := NewBuilder().
		WithMessages(existingMessages...).
		WithLevel(levelMessageList).
		Build()
	model.contextStats = nil // Explicitly nil
	model.searchRequestID = 1
	model.searchOffset = 10
	model.searchTotalCount = -1 // Unknown total
	model.loading = true

	newMessages := makeMessages(5)
	for i := range newMessages {
		newMessages[i].ID = int64(i + 11)
	}

	msg := searchResultsMsg{
		messages:   newMessages,
		totalCount: -1,
		requestID:  1,
		append:     true,
	}
	updatedModel, _ := model.Update(msg)
	m := updatedModel.(Model)

	// Messages should be appended
	if len(m.messages) != 15 {
		t.Errorf("expected 15 messages, got %d", len(m.messages))
	}

	// contextStats should remain nil when unknown total and no pre-existing contextStats
	// (the code only updates MessageCount when contextStats != nil)
	if m.contextStats != nil {
		t.Error("expected contextStats to remain nil when not pre-existing")
	}
}
