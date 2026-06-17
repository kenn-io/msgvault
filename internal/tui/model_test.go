package tui

import (
	"errors"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	assertpkg "github.com/stretchr/testify/assert"
	requirepkg "github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/query"
)

// =============================================================================
// Init Tests
// =============================================================================

func TestModel_Init_ReturnsNonNilCmd(t *testing.T) {
	model := NewBuilder().Build()
	cmd := model.Init()
	assertpkg.NotNil(t, cmd, "Init returned nil command, expected batch command for initial data loading")
}

func TestModel_Init_SetsLoadingState(t *testing.T) {
	// A fresh model via New() starts with loading=true
	engine := newMockEngine(MockConfig{})
	model := New(engine, Options{DataDir: "/tmp/test", Version: "test123"})
	assertpkg.True(t, model.loading, "expected loading=true for fresh model")
}

// =============================================================================
// New (Constructor) Tests
// =============================================================================

func TestNew_SetsDefaults(t *testing.T) {
	assert := assertpkg.New(t)
	engine := newMockEngine(MockConfig{})
	model := New(engine, Options{DataDir: "/tmp/test", Version: "v1.0"})

	assert.Equal("v1.0", model.version)
	assert.Equal(defaultAggregateLimit, model.aggregateLimit)
	assert.Equal(defaultThreadMessageLimit, model.threadMessageLimit)
	assert.Equal(20, model.pageSize)
	assert.Equal(levelAggregates, model.level)
	assert.Equal(query.ViewSenders, model.viewType)
	assert.Equal(query.SortByCount, model.sortField)
	assert.Equal(query.SortDesc, model.sortDirection)
}

func TestNew_OverridesLimits(t *testing.T) {
	engine := newMockEngine(MockConfig{})
	model := New(engine, Options{
		DataDir:            "/tmp/test",
		Version:            "test",
		AggregateLimit:     100,
		ThreadMessageLimit: 50,
	})

	assertpkg.Equal(t, 100, model.aggregateLimit)
	assertpkg.Equal(t, 50, model.threadMessageLimit)
}

// =============================================================================
// dataLoadedMsg Tests - State Transitions
// =============================================================================

func TestModel_Update_DataLoaded_TransitionsFromLoading(t *testing.T) {
	model := NewBuilder().WithLoading(true).Build()
	rows := []query.AggregateRow{{Key: "test@example.com", Count: 10}}

	msg := dataLoadedMsg{rows: rows, requestID: model.aggregateRequestID}
	updatedModel, _ := model.Update(msg)
	m := asModel(t, updatedModel)

	assertpkg.False(t, m.loading, "expected loading=false after data load")
	requirepkg.Len(t, m.rows, 1)
	assertpkg.Equal(t, "test@example.com", m.rows[0].Key)
}

func TestModel_Update_DataLoaded_ResetsCursor(t *testing.T) {
	model := NewBuilder().
		WithRows(makeRows()...).
		WithLoading(true).
		Build()
	model.cursor = 5
	model.scrollOffset = 3

	newRows := []query.AggregateRow{{Key: "new@example.com", Count: 1}}
	msg := dataLoadedMsg{rows: newRows, requestID: model.aggregateRequestID}
	updatedModel, _ := model.Update(msg)
	m := asModel(t, updatedModel)

	assertpkg.Equal(t, 0, m.cursor, "expected cursor=0 after data load")
	assertpkg.Equal(t, 0, m.scrollOffset, "expected scrollOffset=0 after data load")
}

func TestModel_Update_DataLoaded_PreservesPositionWhenRestoring(t *testing.T) {
	model := NewBuilder().
		WithRows(makeRows()...).
		WithLoading(true).
		Build()
	model.cursor = 5
	model.scrollOffset = 3
	model.restorePosition = true

	newRows := makeRows()
	msg := dataLoadedMsg{rows: newRows, requestID: model.aggregateRequestID}
	updatedModel, _ := model.Update(msg)
	m := asModel(t, updatedModel)

	assertpkg.Equal(t, 5, m.cursor, "expected cursor preserved")
	assertpkg.Equal(t, 3, m.scrollOffset, "expected scrollOffset preserved")
	assertpkg.False(t, m.restorePosition, "expected restorePosition to be cleared after use")
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
	m := asModel(t, updatedModel)

	// Should still be loading, no data set
	assertpkg.True(t, m.loading, "expected loading=true (stale response should be ignored)")
	assertpkg.Empty(t, m.rows, "expected no rows (stale response)")
}

func TestModel_Update_DataLoaded_ClearsTransitionBuffer(t *testing.T) {
	model := NewBuilder().WithLoading(true).Build()
	model.transitionBuffer = "frozen view"

	msg := dataLoadedMsg{
		rows:      []query.AggregateRow{{Key: "test", Count: 1}},
		requestID: model.aggregateRequestID,
	}
	updatedModel, _ := model.Update(msg)
	m := asModel(t, updatedModel)

	assertpkg.Empty(t, m.transitionBuffer, "expected transitionBuffer to be cleared after data load")
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
	m := asModel(t, updatedModel)

	assertpkg.False(t, m.loading, "expected loading=false after error")
	requirepkg.Error(t, m.err)
	assertpkg.Equal(t, "database connection failed", m.err.Error())
}

func TestModel_Update_StatsLoaded_HandlesError(t *testing.T) {
	model := NewBuilder().Build()
	originalStats := model.stats

	msg := statsLoadedMsg{err: errors.New("stats query failed")}
	updatedModel, _ := model.Update(msg)
	m := asModel(t, updatedModel)

	// Stats should remain unchanged on error
	assertpkg.Same(t, originalStats, m.stats, "stats should not change on error")
}

func TestModel_Update_AccountsLoaded_HandlesError(t *testing.T) {
	model := NewBuilder().Build()

	msg := accountsLoadedMsg{err: errors.New("accounts query failed")}
	updatedModel, _ := model.Update(msg)
	m := asModel(t, updatedModel)

	// Accounts should remain empty on error
	assertpkg.Empty(t, m.accounts, "expected no accounts on error")
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
	m := asModel(t, updatedModel)

	assertpkg.False(t, m.loading, "expected loading=false after error")
	requirepkg.Error(t, m.err)
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
	m := asModel(t, updatedModel)

	assertpkg.False(t, m.loading, "expected loading=false after search error")
	requirepkg.Error(t, m.err, "expected err to be set after search error")
}

// =============================================================================
// Search Results Pagination Tests
// =============================================================================

func TestModel_Update_SearchResults_ReplacesMessages(t *testing.T) {
	assert := assertpkg.New(t)
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
	m := asModel(t, updatedModel)

	assert.Len(m.messages, 10)
	assert.Equal(0, m.cursor, "expected cursor=0 after replace")
	assert.Equal(0, m.scrollOffset, "expected scrollOffset=0 after replace")
	assert.Equal(int64(100), m.searchTotalCount)
	assert.Equal(10, m.searchOffset)
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
	m := asModel(t, updatedModel)

	assertpkg.Len(t, m.messages, 20, "expected 20 messages (10+10)")
	// Cursor and scroll should NOT reset on append
	assertpkg.Equal(t, 5, m.cursor, "expected cursor=5 (preserved on append)")
	assertpkg.Equal(t, 20, m.searchOffset, "expected searchOffset=20 after append")
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
	m := asModel(t, updatedModel)

	requirepkg.NotNil(t, m.contextStats, "expected contextStats to be set")
	assertpkg.Equal(t, int64(50), m.contextStats.MessageCount)
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
	m := asModel(t, updatedModel)

	assertpkg.True(t, m.loading, "expected loading=true (stale response should be ignored)")
	assertpkg.Empty(t, m.messages, "expected no messages (stale response)")
}

// =============================================================================
// Window Size Tests
// =============================================================================

func TestModel_Update_WindowSize_UpdatesDimensions(t *testing.T) {
	model := NewBuilder().WithSize(100, 24).Build()

	msg := tea.WindowSizeMsg{Width: 120, Height: 40}
	updatedModel, _ := model.Update(msg)
	m := asModel(t, updatedModel)

	assertpkg.Equal(t, 120, m.width)
	assertpkg.Equal(t, 40, m.height)
}

func TestModel_Update_WindowSize_RecalculatesPageSize(t *testing.T) {
	model := NewBuilder().WithSize(100, 24).Build()

	msg := tea.WindowSizeMsg{Width: 100, Height: 50}
	updatedModel, _ := model.Update(msg)
	m := asModel(t, updatedModel)

	expectedPageSize := 50 - headerFooterLines
	assertpkg.Equal(t, expectedPageSize, m.pageSize)
}

func TestModel_Update_WindowSize_ClampsNegativeDimensions(t *testing.T) {
	model := NewBuilder().WithSize(100, 24).Build()

	msg := tea.WindowSizeMsg{Width: -10, Height: -5}
	updatedModel, _ := model.Update(msg)
	m := asModel(t, updatedModel)

	assertpkg.GreaterOrEqual(t, m.width, 0)
	assertpkg.GreaterOrEqual(t, m.height, 0)
}

func TestModel_Update_WindowSize_ClearsTransitionBuffer(t *testing.T) {
	model := NewBuilder().Build()
	model.transitionBuffer = "frozen view"

	msg := tea.WindowSizeMsg{Width: 100, Height: 50}
	updatedModel, _ := model.Update(msg)
	m := asModel(t, updatedModel)

	assertpkg.Empty(t, m.transitionBuffer, "expected transitionBuffer to be cleared on resize")
}

// =============================================================================
// Stats and Accounts Loaded Tests
// =============================================================================

func TestModel_Update_StatsLoaded_SetsStats(t *testing.T) {
	model := NewBuilder().Build()
	stats := &query.TotalStats{MessageCount: 1000, TotalSize: 5000000, AttachmentCount: 50}

	msg := statsLoadedMsg{stats: stats}
	updatedModel, _ := model.Update(msg)
	m := asModel(t, updatedModel)

	assertpkg.Same(t, stats, m.stats, "expected stats to be set")
	assertpkg.Equal(t, int64(1000), m.stats.MessageCount)
}

func TestModel_Update_AccountsLoaded_SetsAccounts(t *testing.T) {
	model := NewBuilder().Build()
	accounts := []query.AccountInfo{
		{ID: 1, Identifier: "user1@gmail.com"},
		{ID: 2, Identifier: "user2@gmail.com"},
	}

	msg := accountsLoadedMsg{accounts: accounts}
	updatedModel, _ := model.Update(msg)
	m := asModel(t, updatedModel)

	requirepkg.Len(t, m.accounts, 2)
	assertpkg.Equal(t, "user1@gmail.com", m.accounts[0].Identifier)
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
	m := asModel(t, updatedModel)

	assertpkg.False(t, m.loading, "expected loading=false after messages loaded")
	assertpkg.Len(t, m.messages, 5)
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
	m := asModel(t, updatedModel)

	assertpkg.True(t, m.loading, "expected loading=true (stale response)")
}

// =============================================================================
// Message Detail Loaded Tests
// =============================================================================

func TestModel_Update_MessageDetailLoaded_SetsDetail(t *testing.T) {
	assert := assertpkg.New(t)
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
	m := asModel(t, updatedModel)

	assert.False(m.loading, "expected loading=false after detail loaded")
	requirepkg.NotNil(t, m.messageDetail, "expected messageDetail to be set")
	assert.Equal("Test Subject", m.messageDetail.Subject)
	assert.Equal(0, m.detailScroll)
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
	m := asModel(t, updatedModel)

	assertpkg.True(t, m.loading, "expected loading=true (stale response)")
	assertpkg.Nil(t, m.messageDetail, "expected messageDetail to remain nil")
}

// =============================================================================
// Update Check Tests
// =============================================================================

func TestModel_Update_UpdateCheck_SetsVersion(t *testing.T) {
	model := NewBuilder().Build()

	msg := updateCheckMsg{version: "v2.0.0", isDevBuild: false}
	updatedModel, _ := model.Update(msg)
	m := asModel(t, updatedModel)

	assertpkg.Equal(t, "v2.0.0", m.updateAvailable)
	assertpkg.False(t, m.updateIsDevBuild)
}

func TestModel_Update_UpdateCheck_SetsDevBuild(t *testing.T) {
	model := NewBuilder().Build()

	msg := updateCheckMsg{version: "", isDevBuild: true}
	updatedModel, _ := model.Update(msg)
	m := asModel(t, updatedModel)

	assertpkg.True(t, m.updateIsDevBuild)
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
	m := asModel(t, updatedModel)

	requirepkg.NotNil(t, m.contextStats, "expected contextStats to be set when search is active")
	assertpkg.Equal(t, int64(50), m.contextStats.MessageCount)
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
	m := asModel(t, updatedModel)

	assertpkg.Nil(t, m.contextStats, "expected contextStats to be cleared at top level without search")
}

// =============================================================================
// Thread Messages Loaded Tests
// =============================================================================

func TestModel_Update_ThreadMessagesLoaded_SetsMessages(t *testing.T) {
	assert := assertpkg.New(t)
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
	m := asModel(t, updatedModel)

	assert.False(m.loading, "expected loading=false after thread messages loaded")
	assert.Len(m.threadMessages, 5)
	assert.Equal(int64(42), m.threadConversationID)
	assert.False(m.threadTruncated)
	// Should reset cursor/scroll
	assert.Equal(0, m.threadCursor)
	assert.Equal(0, m.threadScrollOffset)
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
	m := asModel(t, updatedModel)

	assertpkg.True(t, m.loading, "expected loading=true (stale response should be ignored)")
	assertpkg.Empty(t, m.threadMessages, "expected no thread messages (stale response)")
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
	m := asModel(t, updatedModel)

	assertpkg.Empty(t, m.transitionBuffer, "expected transitionBuffer to be cleared after thread messages load")
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
	m := asModel(t, updatedModel)

	assertpkg.Equal(t, 0, m.threadCursor)
	assertpkg.Equal(t, 0, m.threadScrollOffset)
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
	m := asModel(t, updatedModel)

	assertpkg.False(t, m.loading, "expected loading=false after error")
	requirepkg.Error(t, m.err)
	assertpkg.Equal(t, "thread load failed", m.err.Error())
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
	m := asModel(t, updatedModel)

	assertpkg.True(t, m.threadTruncated, "expected threadTruncated=true when more messages exist")
}

// =============================================================================
// Window Size Tests - Detail View with Search
// =============================================================================

func TestModel_Update_WindowSize_RecalculatesDetailSearchMatches(t *testing.T) {
	assert := assertpkg.New(t)
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
	m := asModel(t, updatedModel)

	// Verify dimensions updated
	assert.Equal(60, m.width)
	assert.Equal(30, m.height)

	// Verify search matches were recomputed (the function should have been called)
	// The match count may differ due to different wrapping
	assert.Equal("searchterm", m.detailSearchQuery, "detailSearchQuery should be preserved")

	// Match index should be clamped to valid range
	if len(m.detailSearchMatches) > 0 {
		assert.Less(m.detailSearchMatchIndex, len(m.detailSearchMatches),
			"detailSearchMatchIndex should be < match count")
	} else {
		assert.Equal(0, m.detailSearchMatchIndex, "expected detailSearchMatchIndex=0 when no matches")
	}

	// Original match count check to ensure the test is meaningful
	assert.NotZero(originalMatchCount, "test setup error: expected at least one match in original search")
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
	m := asModel(t, updatedModel)

	// Match index should never exceed matches length
	if len(m.detailSearchMatches) > 0 {
		assertpkg.Less(t, m.detailSearchMatchIndex, len(m.detailSearchMatches),
			"detailSearchMatchIndex exceeds match count")
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
	m := asModel(t, updatedModel)

	// When no matches, index should be 0
	if len(m.detailSearchMatches) == 0 {
		assertpkg.Equal(t, 0, m.detailSearchMatchIndex, "expected detailSearchMatchIndex=0 when no matches")
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
	m := asModel(t, updatedModel)

	// Total messages should be 15 (10 + 5)
	assertpkg.Len(t, m.messages, 15)

	// contextStats.MessageCount should be updated to reflect loaded count
	requirepkg.NotNil(t, m.contextStats, "expected contextStats to be set")
	assertpkg.Equal(t, int64(15), m.contextStats.MessageCount)
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
	m := asModel(t, updatedModel)

	// contextStats.MessageCount should remain at known total (100), not loaded count (15)
	requirepkg.NotNil(t, m.contextStats, "expected contextStats to be set")
	assertpkg.Equal(t, int64(100), m.contextStats.MessageCount, "expected known total")
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
	m := asModel(t, updatedModel)

	// Messages should be appended
	assertpkg.Len(t, m.messages, 15)

	// contextStats should remain nil when unknown total and no pre-existing contextStats
	// (the code only updates MessageCount when contextStats != nil)
	assertpkg.Nil(t, m.contextStats, "expected contextStats to remain nil when not pre-existing")
}
