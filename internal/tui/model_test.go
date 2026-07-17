package tui

import (
	"context"
	"errors"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/search"
)

// =============================================================================
// Init Tests
// =============================================================================

func TestModel_Init_ReturnsNonNilCmd(t *testing.T) {
	model := NewBuilder().Build()
	cmd := model.Init()
	assert.NotNil(t, cmd, "Init returned nil command, expected batch command for initial data loading")
}

func TestModel_Init_SetsLoadingState(t *testing.T) {
	// A fresh model via New() starts with loading=true
	engine := newMockEngine(MockConfig{})
	model := New(engine, Options{DataDir: "/tmp/test", Version: "test123"})
	assert.True(t, model.loading, "expected loading=true for fresh model")
}

// =============================================================================
// New (Constructor) Tests
// =============================================================================

func TestNew_SetsDefaults(t *testing.T) {
	assert := assert.New(t)
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

	assert.Equal(t, 100, model.aggregateLimit)
	assert.Equal(t, 50, model.threadMessageLimit)
}

func TestDeepSearchStatsOptions_EnableSearchScope(t *testing.T) {
	t.Run("deep search stats use merged representable scope", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		engine := newMockEngine(MockConfig{})
		tracker := &statsTracker{result: &query.TotalStats{}}
		tracker.install(engine)
		model := New(engine, Options{DataDir: "/tmp/test", Version: "test"})
		model.searchMode = searchModeDeep
		after := time.Date(2024, 2, 1, 0, 0, 0, 0, time.UTC)
		before := time.Date(2024, 3, 1, 0, 0, 0, 0, time.UTC)
		model.searchFilter = query.MessageFilter{
			Sender:                "alice@example.com",
			Recipient:             "bob@example.com",
			Domain:                "example.com",
			Label:                 "Project Review",
			MessageType:           "meeting_transcript",
			SourceIDs:             []int64{7, 8},
			After:                 &after,
			Before:                &before,
			WithAttachmentsOnly:   true,
			HideDeletedFromSource: true,
		}

		cmd := model.loadSearch("cross-type stats needle")
		require.NotNil(cmd, "loadSearch command")
		msg := cmd()
		require.IsType(searchResultsMsg{}, msg)
		require.Equal(1, tracker.callCount, "deep search stats call count")
		assert.True(tracker.lastOpts.SearchScope, "deep search stats use search scope")
		assert.Nil(tracker.lastOpts.SourceID, "multi-source scope does not collapse to one source")
		assert.Equal([]int64{7, 8}, tracker.lastOpts.SourceIDs, "merged source scope")
		assert.True(tracker.lastOpts.WithAttachmentsOnly, "merged attachment scope")
		assert.True(tracker.lastOpts.HideDeletedFromSource, "merged deletion scope")

		formatted := search.Parse(tracker.lastOpts.SearchQuery)
		assert.Equal([]string{"cross-type", "stats", "needle"}, formatted.TextTerms)
		assert.ElementsMatch([]string{"alice@example.com", "@example.com"}, formatted.FromAddrs)
		assert.Equal([]string{"bob@example.com"}, formatted.ToAddrs)
		assert.Equal([]string{"Project Review"}, formatted.Labels)
		assert.Equal([]string{"email"}, formatted.MessageTypes)
		require.NotNil(formatted.AfterDate, "merged after date")
		require.NotNil(formatted.BeforeDate, "merged before date")
		assert.Equal(after, *formatted.AfterDate)
		assert.Equal(before, *formatted.BeforeDate)
	})

	t.Run("conflicting message types short-circuit stats", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		engine := newMockEngine(MockConfig{})
		tracker := &statsTracker{result: &query.TotalStats{MessageCount: 99}}
		tracker.install(engine)
		model := New(engine, Options{DataDir: "/tmp/test", Version: "test"})
		model.searchMode = searchModeDeep
		model.searchFilter = query.MessageFilter{}

		cmd := model.loadSearch("message_type:sms conflictneedle")
		require.NotNil(cmd, "loadSearch command")
		msg := cmd()
		result, ok := msg.(searchResultsMsg)
		require.True(ok, "expected searchResultsMsg, got %T", msg)
		assert.Zero(tracker.callCount, "conflicting scope skips stats query")
		require.NotNil(result.stats, "conflicting scope zero stats")
		assert.Zero(result.stats.MessageCount, "conflicting scope stats count")
	})

	t.Run("aggregate analytics stats", func(t *testing.T) {
		require := require.New(t)
		engine := newMockEngine(MockConfig{})
		tracker := &statsTracker{result: &query.TotalStats{}}
		tracker.install(engine)
		model := New(engine, Options{DataDir: "/tmp/test", Version: "test"})
		model.searchQuery = "cross-type stats needle"

		cmd := model.loadData()
		require.NotNil(cmd, "loadData command")
		msg := cmd()
		require.IsType(dataLoadedMsg{}, msg)
		require.Equal(1, tracker.callCount, "aggregate analytics stats call count")
		assert.False(t, tracker.lastOpts.SearchScope, "aggregate analytics retain default scope")
	})

	t.Run("ordinary total stats", func(t *testing.T) {
		require := require.New(t)
		engine := newMockEngine(MockConfig{})
		tracker := &statsTracker{result: &query.TotalStats{}}
		tracker.install(engine)
		model := New(engine, Options{DataDir: "/tmp/test", Version: "test"})

		cmd := model.loadStats()
		require.NotNil(cmd, "loadStats command")
		msg := cmd()
		require.IsType(statsLoadedMsg{}, msg)
		require.Equal(1, tracker.callCount, "ordinary stats call count")
		assert.False(t, tracker.lastOpts.SearchScope, "ordinary stats retain default scope")
	})
}

func TestDeepSearchStatsFailureClearsStaleContextStats(t *testing.T) {
	tests := []struct {
		name         string
		messages     []query.MessageSummary
		wantTotal    int64
		wantMsgCount int64
	}{
		{
			name:         "short page keeps known result count",
			messages:     []query.MessageSummary{{ID: 42, Subject: "matching result"}},
			wantTotal:    1,
			wantMsgCount: 1,
		},
		{
			name:         "full page keeps loaded count when total is unknown",
			messages:     makeMessages(searchPageSize),
			wantTotal:    -1,
			wantMsgCount: searchPageSize,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			engine := newMockEngine(MockConfig{Messages: tc.messages})
			engine.GetTotalStatsFunc = func(context.Context, query.StatsOptions) (*query.TotalStats, error) {
				return nil, errors.New("stats unavailable")
			}
			model := New(engine, Options{DataDir: "/tmp/test", Version: "test"})
			model.searchMode = searchModeDeep
			model.contextStats = &query.TotalStats{
				MessageCount:    99,
				TotalSize:       12345,
				AttachmentCount: 7,
				LabelCount:      3,
				AccountCount:    2,
			}

			cmd := model.loadSearch("matching")
			require.NotNil(cmd, "loadSearch command")
			msg, ok := cmd().(searchResultsMsg)
			require.True(ok, "expected searchResultsMsg")
			require.NoError(msg.err, "successful results survive stats failure")
			assert.Equal(tc.wantTotal, msg.totalCount)
			require.NotNil(msg.stats, "stats failure produces explicit fallback stats")
			assert.Equal(tc.wantMsgCount, msg.stats.MessageCount)
			assert.Zero(msg.stats.TotalSize)
			assert.Zero(msg.stats.AttachmentCount)
			assert.Zero(msg.stats.LabelCount)
			assert.Zero(msg.stats.AccountCount)

			model.searchRequestID = msg.requestID
			updated, _ := model.Update(msg)
			got := asModel(t, updated)
			require.NotNil(got.contextStats)
			assert.Equal(tc.wantMsgCount, got.contextStats.MessageCount)
			assert.Zero(got.contextStats.TotalSize)
			assert.Zero(got.contextStats.AttachmentCount)
			assert.Zero(got.contextStats.LabelCount)
			assert.Zero(got.contextStats.AccountCount)
			assert.Len(got.messages, len(tc.messages), "successful search results are preserved")
		})
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
	m := asModel(t, updatedModel)

	assert.False(t, m.loading, "expected loading=false after data load")
	require.Len(t, m.rows, 1)
	assert.Equal(t, "test@example.com", m.rows[0].Key)
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

	assert.Equal(t, 0, m.cursor, "expected cursor=0 after data load")
	assert.Equal(t, 0, m.scrollOffset, "expected scrollOffset=0 after data load")
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

	assert.Equal(t, 5, m.cursor, "expected cursor preserved")
	assert.Equal(t, 3, m.scrollOffset, "expected scrollOffset preserved")
	assert.False(t, m.restorePosition, "expected restorePosition to be cleared after use")
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
	assert.True(t, m.loading, "expected loading=true (stale response should be ignored)")
	assert.Empty(t, m.rows, "expected no rows (stale response)")
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

	assert.Empty(t, m.transitionBuffer, "expected transitionBuffer to be cleared after data load")
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

	assert.False(t, m.loading, "expected loading=false after error")
	require.Error(t, m.err)
	assert.Equal(t, "database connection failed", m.err.Error())
}

func TestModel_Update_StatsLoaded_HandlesError(t *testing.T) {
	model := NewBuilder().Build()
	originalStats := model.stats

	msg := statsLoadedMsg{err: errors.New("stats query failed")}
	updatedModel, _ := model.Update(msg)
	m := asModel(t, updatedModel)

	// Stats should remain unchanged on error
	assert.Same(t, originalStats, m.stats, "stats should not change on error")
}

func TestModel_Update_AccountsLoaded_HandlesError(t *testing.T) {
	model := NewBuilder().Build()

	msg := accountsLoadedMsg{err: errors.New("accounts query failed")}
	updatedModel, _ := model.Update(msg)
	m := asModel(t, updatedModel)

	// Accounts should remain empty on error
	assert.Empty(t, m.accounts, "expected no accounts on error")
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

	assert.False(t, m.loading, "expected loading=false after error")
	require.Error(t, m.err)
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

	assert.False(t, m.loading, "expected loading=false after search error")
	require.Error(t, m.err, "expected err to be set after search error")
}

// =============================================================================
// Search Results Pagination Tests
// =============================================================================

func TestModel_Update_SearchResults_ReplacesMessages(t *testing.T) {
	assert := assert.New(t)
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

	assert.Len(t, m.messages, 20, "expected 20 messages (10+10)")
	// Cursor and scroll should NOT reset on append
	assert.Equal(t, 5, m.cursor, "expected cursor=5 (preserved on append)")
	assert.Equal(t, 20, m.searchOffset, "expected searchOffset=20 after append")
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

	require.NotNil(t, m.contextStats, "expected contextStats to be set")
	assert.Equal(t, int64(50), m.contextStats.MessageCount)
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

	assert.True(t, m.loading, "expected loading=true (stale response should be ignored)")
	assert.Empty(t, m.messages, "expected no messages (stale response)")
}

// =============================================================================
// Window Size Tests
// =============================================================================

func TestModel_Update_WindowSize_UpdatesDimensions(t *testing.T) {
	model := NewBuilder().WithSize(100, 24).Build()

	msg := tea.WindowSizeMsg{Width: 120, Height: 40}
	updatedModel, _ := model.Update(msg)
	m := asModel(t, updatedModel)

	assert.Equal(t, 120, m.width)
	assert.Equal(t, 40, m.height)
}

func TestModel_Update_WindowSize_RecalculatesPageSize(t *testing.T) {
	model := NewBuilder().WithSize(100, 24).Build()

	msg := tea.WindowSizeMsg{Width: 100, Height: 50}
	updatedModel, _ := model.Update(msg)
	m := asModel(t, updatedModel)

	expectedPageSize := 50 - headerFooterLines
	assert.Equal(t, expectedPageSize, m.pageSize)
}

func TestModel_Update_WindowSize_ClampsNegativeDimensions(t *testing.T) {
	model := NewBuilder().WithSize(100, 24).Build()

	msg := tea.WindowSizeMsg{Width: -10, Height: -5}
	updatedModel, _ := model.Update(msg)
	m := asModel(t, updatedModel)

	assert.GreaterOrEqual(t, m.width, 0)
	assert.GreaterOrEqual(t, m.height, 0)
}

func TestModel_Update_WindowSize_ClearsTransitionBuffer(t *testing.T) {
	model := NewBuilder().Build()
	model.transitionBuffer = "frozen view"

	msg := tea.WindowSizeMsg{Width: 100, Height: 50}
	updatedModel, _ := model.Update(msg)
	m := asModel(t, updatedModel)

	assert.Empty(t, m.transitionBuffer, "expected transitionBuffer to be cleared on resize")
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

	assert.Same(t, stats, m.stats, "expected stats to be set")
	assert.Equal(t, int64(1000), m.stats.MessageCount)
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

	require.Len(t, m.accounts, 2)
	assert.Equal(t, "user1@gmail.com", m.accounts[0].Identifier)
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

	assert.False(t, m.loading, "expected loading=false after messages loaded")
	assert.Len(t, m.messages, 5)
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

	assert.True(t, m.loading, "expected loading=true (stale response)")
}

// =============================================================================
// Message Detail Loaded Tests
// =============================================================================

func TestModel_Update_MessageDetailLoaded_SetsDetail(t *testing.T) {
	assert := assert.New(t)
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
	require.NotNil(t, m.messageDetail, "expected messageDetail to be set")
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

	assert.True(t, m.loading, "expected loading=true (stale response)")
	assert.Nil(t, m.messageDetail, "expected messageDetail to remain nil")
}

// =============================================================================
// Update Check Tests
// =============================================================================

func TestModel_Update_UpdateCheck_SetsVersion(t *testing.T) {
	model := NewBuilder().Build()

	msg := updateCheckMsg{version: "v2.0.0", isDevBuild: false}
	updatedModel, _ := model.Update(msg)
	m := asModel(t, updatedModel)

	assert.Equal(t, "v2.0.0", m.updateAvailable)
	assert.False(t, m.updateIsDevBuild)
}

func TestModel_Update_UpdateCheck_SetsDevBuild(t *testing.T) {
	model := NewBuilder().Build()

	msg := updateCheckMsg{version: "", isDevBuild: true}
	updatedModel, _ := model.Update(msg)
	m := asModel(t, updatedModel)

	assert.True(t, m.updateIsDevBuild)
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

	require.NotNil(t, m.contextStats, "expected contextStats to be set when search is active")
	assert.Equal(t, int64(50), m.contextStats.MessageCount)
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

	assert.Nil(t, m.contextStats, "expected contextStats to be cleared at top level without search")
}

// =============================================================================
// Thread Messages Loaded Tests
// =============================================================================

func TestModel_Update_ThreadMessagesLoaded_SetsMessages(t *testing.T) {
	assert := assert.New(t)
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

	assert.True(t, m.loading, "expected loading=true (stale response should be ignored)")
	assert.Empty(t, m.threadMessages, "expected no thread messages (stale response)")
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

	assert.Empty(t, m.transitionBuffer, "expected transitionBuffer to be cleared after thread messages load")
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

	assert.Equal(t, 0, m.threadCursor)
	assert.Equal(t, 0, m.threadScrollOffset)
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

	assert.False(t, m.loading, "expected loading=false after error")
	require.Error(t, m.err)
	assert.Equal(t, "thread load failed", m.err.Error())
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

	assert.True(t, m.threadTruncated, "expected threadTruncated=true when more messages exist")
}

// =============================================================================
// Window Size Tests - Detail View with Search
// =============================================================================

func TestModel_Update_WindowSize_RecalculatesDetailSearchMatches(t *testing.T) {
	assert := assert.New(t)
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
		assert.Less(t, m.detailSearchMatchIndex, len(m.detailSearchMatches),
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
		assert.Equal(t, 0, m.detailSearchMatchIndex, "expected detailSearchMatchIndex=0 when no matches")
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
	assert.Len(t, m.messages, 15)

	// contextStats.MessageCount should be updated to reflect loaded count
	require.NotNil(t, m.contextStats, "expected contextStats to be set")
	assert.Equal(t, int64(15), m.contextStats.MessageCount)
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
	require.NotNil(t, m.contextStats, "expected contextStats to be set")
	assert.Equal(t, int64(100), m.contextStats.MessageCount, "expected known total")
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
	assert.Len(t, m.messages, 15)

	// contextStats should remain nil when unknown total and no pre-existing contextStats
	// (the code only updates MessageCount when contextStats != nil)
	assert.Nil(t, m.contextStats, "expected contextStats to remain nil when not pre-existing")
}
