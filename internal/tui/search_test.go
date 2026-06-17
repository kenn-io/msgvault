package tui

import (
	"strings"
	"testing"

	assertpkg "github.com/stretchr/testify/assert"
	requirepkg "github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/query"
)

func TestSearchModalOpen(t *testing.T) {
	model := NewBuilder().
		WithRows(makeRow("alice@example.com", 10)).
		Build()

	// Press '/' to activate inline search
	model, cmd := sendKey(t, model, key('/'))

	assertInlineSearchActive(t, model, true)
	assertSearchMode(t, model, searchModeFast)
	assertCmd(t, cmd, true)
}

// TestSearchResultsDisplay verifies search results are displayed.
func TestSearchResultsDisplay(t *testing.T) {
	model := NewBuilder().WithPageSize(10).WithSize(100, 20).Build()
	model.searchQuery = "test query"
	model.searchMode = searchModeFast
	model.searchRequestID = 1

	m := applySearchResults(t, model, 1, []query.MessageSummary{
		{ID: 1, Subject: "Result 1"},
		{ID: 2, Subject: "Result 2"},
	}, 0)

	assertLevel(t, m, levelMessageList)
	assertpkg.Len(t, m.messages, 2)
	assertLoading(t, m, false, false)
}

// TestSearchResultsStale verifies stale search results are ignored.
func TestSearchResultsStale(t *testing.T) {
	model := NewBuilder().WithPageSize(10).WithSize(100, 20).Build()
	model.searchRequestID = 2 // Current request is 2

	m := applySearchResults(t, model, 1, []query.MessageSummary{
		{ID: 1, Subject: "Stale Result"},
	}, 0)

	// Messages should not be updated (still nil/empty)
	assertpkg.Empty(t, m.messages, "stale ignored")
}

// TestInlineSearchTabToggle verifies Tab key behavior across different search states.
func TestInlineSearchTabToggle(t *testing.T) {
	tests := []struct {
		name                     string
		level                    viewLevel
		initialMode              searchModeKind
		query                    string
		wantMode                 searchModeKind
		wantCmd                  bool
		wantInlineSearchLoading  bool
		wantSearchIDIncrement    bool
		wantAggregateIDIncrement bool
	}{
		{
			name:                    "toggle fast to deep at message list",
			level:                   levelMessageList,
			initialMode:             searchModeFast,
			query:                   "test query",
			wantMode:                searchModeDeep,
			wantCmd:                 true,
			wantInlineSearchLoading: true,
			wantSearchIDIncrement:   true,
		},
		{
			name:                    "toggle deep to fast at message list",
			level:                   levelMessageList,
			initialMode:             searchModeDeep,
			query:                   "test query",
			wantMode:                searchModeFast,
			wantCmd:                 true,
			wantInlineSearchLoading: true,
			wantSearchIDIncrement:   true,
		},
		{
			name:                    "no search with empty query",
			level:                   levelMessageList,
			initialMode:             searchModeFast,
			query:                   "",
			wantMode:                searchModeDeep,
			wantCmd:                 false,
			wantInlineSearchLoading: false,
		},
		{
			name:        "no-op at aggregate level",
			level:       levelAggregates,
			initialMode: searchModeFast,
			query:       "test query",
			wantMode:    searchModeFast,
			wantCmd:     false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert := assertpkg.New(t)
			model := NewBuilder().WithPageSize(10).WithSize(100, 20).
				WithLevel(tt.level).
				WithActiveSearch(tt.query, tt.initialMode).
				Build()
			initialSearchID := model.searchRequestID
			initialAggregateID := model.aggregateRequestID

			m, cmd := applyInlineSearchKey(t, model, keyTab())

			assertSearchMode(t, m, tt.wantMode)
			assertCmd(t, cmd, tt.wantCmd)

			assert.Equal(tt.wantInlineSearchLoading, m.inlineSearchLoading)

			if tt.wantSearchIDIncrement {
				assert.Equal(initialSearchID+1, m.searchRequestID, "searchRequestID")
			} else {
				assert.Equal(initialSearchID, m.searchRequestID, "searchRequestID should not change")
			}

			if tt.wantAggregateIDIncrement {
				assert.Equal(initialAggregateID+1, m.aggregateRequestID, "aggregateRequestID")
			} else {
				assert.Equal(initialAggregateID, m.aggregateRequestID, "aggregateRequestID should not change")
			}
		})
	}
}

// TestSpinnerAppearsInViewWhenLoading verifies spinner character appears in rendered view.
func TestSpinnerAppearsInViewWhenLoading(t *testing.T) {
	model := NewBuilder().
		WithRows(query.AggregateRow{Key: "test@example.com", Count: 10}).
		WithPageSize(10).WithSize(100, 20).Build()

	// Verify no spinner when not loading
	view1 := model.View()
	hasSpinner := false
	for _, frame := range spinnerFrames {
		if strings.Contains(view1, frame) {
			hasSpinner = true
			break
		}
	}
	assertpkg.False(t, hasSpinner, "expected no spinner when loading = false")

	// Now set loading state
	model.inlineSearchLoading = true
	model.inlineSearchActive = true
	model.searchInput.SetValue("test")

	view2 := model.View()
	hasSpinner = false
	for _, frame := range spinnerFrames {
		if strings.Contains(view2, frame) {
			hasSpinner = true
			break
		}
	}
	assertpkg.True(t, hasSpinner, "expected spinner in view when inlineSearchLoading = true, got:\n%s", view2)
}

// TestSearchBackClears verifies going back clears search state.
func TestSearchBackClears(t *testing.T) {
	model := NewBuilder().WithPageSize(10).WithSize(100, 20).
		WithLevel(levelMessageList).Build()
	model.searchQuery = "test query"
	model.searchFilter = query.MessageFilter{Sender: "alice@example.com"}
	model.breadcrumbs = []navigationSnapshot{{state: viewState{level: levelAggregates}}}

	// Go back
	newModel, _ := model.goBack()
	m := asModel(t, newModel)

	assertSearchQuery(t, m, "")
	assertpkg.Empty(t, m.searchFilter.Sender, "expected empty searchFilter after goBack")
}

// TestSearchFromSubAggregate verifies search from sub-aggregate view.
func TestSearchFromSubAggregate(t *testing.T) {
	model := NewBuilder().
		WithRows(query.AggregateRow{Key: "bob@example.com", Count: 3}).
		WithPageSize(10).WithSize(100, 20).
		WithLevel(levelDrillDown).WithViewType(query.ViewRecipients).
		Build()
	model.drillViewType = query.ViewSenders
	model.drillFilter = query.MessageFilter{Sender: "alice@example.com"}

	// Press '/' to activate inline search
	m, cmd := applyAggregateKeyWithCmd(t, model, key('/'))

	assertInlineSearchActive(t, m, true)
	assertCmd(t, cmd, true)
}

// TestSearchFromMessageList verifies search from message list view.
func TestSearchFromMessageList(t *testing.T) {
	model := NewBuilder().
		WithMessages(query.MessageSummary{ID: 1, Subject: "Test"}).
		WithPageSize(10).WithSize(100, 20).
		WithLevel(levelMessageList).Build()

	// Press '/' to activate inline search
	m, cmd := applyMessageListKeyWithCmd(t, model, key('/'))

	assertInlineSearchActive(t, m, true)
	assertCmd(t, cmd, true)
}

// TestGKeyCyclesViewType verifies that 'g' cycles through view types at aggregate level.

// TestSearchSetsContextStats verifies search results set contextStats for header metrics.
func TestSearchSetsContextStats(t *testing.T) {
	model := NewBuilder().Build()
	model.searchRequestID = 1

	m := applySearchResults(t, model, 1, make([]query.MessageSummary, 10), 150)

	assertContextStats(t, m, 150, -1, -1)
}

// TestSearchZeroResultsClearsContextStats verifies contextStats is set to zero on empty search.
func TestSearchZeroResultsClearsContextStats(t *testing.T) {
	model := NewBuilder().
		WithContextStats(&query.TotalStats{MessageCount: 500}).
		Build()
	model.searchRequestID = 1

	m := applySearchResults(t, model, 1, []query.MessageSummary{}, 0)

	assertContextStats(t, m, 0, -1, -1)
}

// TestSearchPaginationUpdatesContextStats verifies contextStats updates on append when total unknown.
func TestSearchPaginationUpdatesContextStats(t *testing.T) {
	model := NewBuilder().
		WithMessages(make([]query.MessageSummary, 50)...).
		WithLevel(levelMessageList).
		WithContextStats(&query.TotalStats{MessageCount: 50}).
		Build()
	model.searchRequestID = 1
	model.searchTotalCount = -1 // Unknown total

	m := applySearchResultsAppend(t, model, 1, make([]query.MessageSummary, 50), -1)

	assertpkg.Len(t, m.messages, 100, "after append")
	assertContextStats(t, m, 100, -1, -1)
}

// TestFastSearchPaginationTriggersOnNavigation verifies that cursor movement
// near the end of loaded fast search results triggers loading more results.
// This was a bug where navigateList returned early before maybeLoadMoreSearchResults
// could fire, making fast search pagination completely non-functional.
func TestFastSearchPaginationTriggersOnNavigation(t *testing.T) {
	t.Run("down arrow near end triggers load more", func(t *testing.T) {
		msgs := makeMessages(100)
		model := NewBuilder().
			WithMessages(msgs...).
			WithLevel(levelMessageList).
			WithPageSize(20).
			Build()
		model.searchQuery = "test"
		model.searchMode = searchModeFast
		model.searchTotalCount = -1 // Unknown total = more pages available
		model.searchOffset = 100
		model.cursor = 90 // Within 20 of end (threshold)

		// Press down arrow — cursor moves to 91, which is within threshold
		m, cmd := applyMessageListKeyWithCmd(t, model, keyDown())

		assertpkg.Equal(t, 91, m.cursor)
		assertpkg.NotNil(t, cmd, "expected load-more command to be returned for fast search pagination")
		assertpkg.True(t, m.searchLoadingMore)
	})

	t.Run("down arrow far from end does not trigger load", func(t *testing.T) {
		msgs := makeMessages(100)
		model := NewBuilder().
			WithMessages(msgs...).
			WithLevel(levelMessageList).
			WithPageSize(20).
			Build()
		model.searchQuery = "test"
		model.searchMode = searchModeFast
		model.searchTotalCount = -1
		model.searchOffset = 100
		model.cursor = 10 // Far from end

		m, cmd := applyMessageListKeyWithCmd(t, model, keyDown())

		assertpkg.Equal(t, 11, m.cursor)
		assertpkg.Nil(t, cmd, "expected no command when cursor is far from end")
	})

	t.Run("no pagination when all results loaded", func(t *testing.T) {
		msgs := makeMessages(50)
		model := NewBuilder().
			WithMessages(msgs...).
			WithLevel(levelMessageList).
			WithPageSize(20).
			Build()
		model.searchQuery = "test"
		model.searchMode = searchModeFast
		model.searchTotalCount = 50 // Known total, all loaded
		model.searchOffset = 50
		model.cursor = 40 // Near end

		m, cmd := applyMessageListKeyWithCmd(t, model, keyDown())

		assertpkg.Equal(t, 41, m.cursor)
		assertpkg.Nil(t, cmd, "expected no command when all results are loaded")
	})

	t.Run("cursor at last item pressing down still triggers load", func(t *testing.T) {
		msgs := makeMessages(100)
		model := NewBuilder().
			WithMessages(msgs...).
			WithLevel(levelMessageList).
			WithPageSize(20).
			Build()
		model.searchQuery = "test"
		model.searchMode = searchModeFast
		model.searchTotalCount = -1 // More results available
		model.searchOffset = 100
		model.cursor = 99 // Already at last item

		// Press down — cursor can't move (clamped at 99), but pagination should still trigger
		m, cmd := applyMessageListKeyWithCmd(t, model, keyDown())

		assertpkg.Equal(t, 99, m.cursor, "expected cursor unchanged")
		assertpkg.NotNil(t, cmd, "expected load-more command even when cursor can't move beyond end")
		assertpkg.True(t, m.searchLoadingMore)
	})

	t.Run("no pagination for deep search mode", func(t *testing.T) {
		msgs := makeMessages(100)
		model := NewBuilder().
			WithMessages(msgs...).
			WithLevel(levelMessageList).
			WithPageSize(20).
			Build()
		model.searchQuery = "test"
		model.searchMode = searchModeDeep // Deep mode uses different pagination
		model.searchTotalCount = -1
		model.searchOffset = 100
		model.cursor = 90

		m, cmd := applyMessageListKeyWithCmd(t, model, keyDown())

		assertpkg.Equal(t, 91, m.cursor)
		assertpkg.Nil(t, cmd, "expected no command for deep search mode (uses different pagination)")
	})
}

// TestMessageListPaginationTriggersOnNavigation verifies that cursor movement
// near the end of a non-search message list triggers loading more messages.
func TestMessageListPaginationTriggersOnNavigation(t *testing.T) {
	t.Run("near end triggers load more", func(t *testing.T) {
		msgs := makeMessages(messageListPageSize) // Exactly one full page
		model := NewBuilder().
			WithMessages(msgs...).
			WithLevel(levelMessageList).
			WithPageSize(20).
			Build()
		model.msgListOffset = messageListPageSize // Simulate having loaded a full page
		model.cursor = messageListPageSize - 10   // Within threshold of end

		m, cmd := applyMessageListKeyWithCmd(t, model, keyDown())

		assertpkg.Equal(t, messageListPageSize-9, m.cursor)
		assertpkg.NotNil(t, cmd, "expected load-more command to be returned for message list pagination")
		assertpkg.True(t, m.msgListLoadingMore)
	})

	t.Run("far from end does not trigger load", func(t *testing.T) {
		msgs := makeMessages(messageListPageSize)
		model := NewBuilder().
			WithMessages(msgs...).
			WithLevel(levelMessageList).
			WithPageSize(20).
			Build()
		model.msgListOffset = messageListPageSize
		model.cursor = 10 // Far from end

		m, cmd := applyMessageListKeyWithCmd(t, model, keyDown())

		assertpkg.Equal(t, 11, m.cursor)
		assertpkg.Nil(t, cmd, "expected no command when cursor is far from end")
	})

	t.Run("short last page means no more data", func(t *testing.T) {
		// 300 messages loaded but page size is 500 — last page was short
		msgs := makeMessages(300)
		model := NewBuilder().
			WithMessages(msgs...).
			WithLevel(levelMessageList).
			WithPageSize(20).
			Build()
		model.msgListOffset = 300
		model.cursor = 290 // Near end

		m, cmd := applyMessageListKeyWithCmd(t, model, keyDown())

		assertpkg.Equal(t, 291, m.cursor)
		assertpkg.Nil(t, cmd, "expected no command when last page was short (all data loaded)")
	})

	t.Run("no pagination during search", func(t *testing.T) {
		msgs := makeMessages(messageListPageSize)
		model := NewBuilder().
			WithMessages(msgs...).
			WithLevel(levelMessageList).
			WithPageSize(20).
			Build()
		model.searchQuery = "test"
		model.searchMode = searchModeFast
		model.msgListOffset = messageListPageSize
		model.cursor = messageListPageSize - 10

		_, cmd := applyMessageListKeyWithCmd(t, model, keyDown())

		// Should use search pagination, not message list pagination
		// (maybeLoadMoreMessages returns nil when searchQuery is set)
		// Note: search pagination may or may not fire depending on searchTotalCount
		_ = cmd // Don't assert — just verify no panic
	})

	t.Run("contextStats prevents extra load when all loaded", func(t *testing.T) {
		msgs := makeMessages(messageListPageSize)
		model := NewBuilder().
			WithMessages(msgs...).
			WithLevel(levelMessageList).
			WithPageSize(20).
			WithContextStats(&query.TotalStats{MessageCount: int64(messageListPageSize)}).
			Build()
		model.msgListOffset = messageListPageSize
		model.cursor = messageListPageSize - 10

		m, cmd := applyMessageListKeyWithCmd(t, model, keyDown())

		assertpkg.Equal(t, messageListPageSize-9, m.cursor)
		assertpkg.Nil(t, cmd, "expected no command when contextStats shows all messages loaded")
	})

	t.Run("append mode preserves cursor and appends messages", func(t *testing.T) {
		assert := assertpkg.New(t)
		msgs := makeMessages(messageListPageSize)
		model := NewBuilder().
			WithMessages(msgs...).
			WithLevel(levelMessageList).
			WithPageSize(20).
			Build()
		model.msgListOffset = messageListPageSize
		model.cursor = 400 // Preserve this cursor position
		model.loadRequestID = 5

		// Simulate appended results arriving
		newMsgs := makeMessages(100)
		loadMsg := messagesLoadedMsg{
			messages:  newMsgs,
			requestID: 5,
			append:    true,
		}
		m := sendMsg(t, model, loadMsg)

		assert.Len(m.messages, messageListPageSize+100, "after append")
		assert.Equal(400, m.cursor, "expected cursor preserved")
		assert.Equal(messageListPageSize+100, m.msgListOffset)
		assert.False(m.msgListLoadingMore, "after load completes")
	})

	t.Run("empty append marks end-of-data", func(t *testing.T) {
		msgs := makeMessages(messageListPageSize)
		model := NewBuilder().
			WithMessages(msgs...).
			WithLevel(levelMessageList).
			WithPageSize(20).
			Build()
		model.msgListOffset = messageListPageSize
		model.loadRequestID = 5

		// Simulate empty append (no more data)
		loadMsg := messagesLoadedMsg{
			messages:  []query.MessageSummary{},
			requestID: 5,
			append:    true,
		}
		m := sendMsg(t, model, loadMsg)

		assertpkg.True(t, m.msgListComplete, "after empty append")
		assertpkg.Len(t, m.messages, messageListPageSize, "expected messages unchanged")

		// Subsequent navigation near end should NOT trigger another load
		m.cursor = messageListPageSize - 5
		m2, cmd := applyMessageListKeyWithCmd(t, m, keyDown())

		assertpkg.Nil(t, cmd, "expected no command after end-of-data is known")
		_ = m2
	})

	t.Run("fresh load resets msgListComplete", func(t *testing.T) {
		model := NewBuilder().
			WithLevel(levelMessageList).
			WithPageSize(20).
			Build()
		model.msgListComplete = true
		model.loadRequestID = 5

		// Simulate fresh (non-append) load
		loadMsg := messagesLoadedMsg{
			messages:  makeMessages(messageListPageSize),
			requestID: 5,
			append:    false,
		}
		m := sendMsg(t, model, loadMsg)

		assertpkg.False(t, m.msgListComplete, "after fresh load")
	})
}

// TestMessageListPaginationBreadcrumbRestore verifies that breadcrumb navigation
// preserves message list pagination state. When navigating from a paginated list
// to a detail view and back, the pagination offset should be restored so the
// next page request uses the correct offset.
func TestMessageListPaginationBreadcrumbRestore(t *testing.T) {
	t.Run("goBack restores msgListOffset from breadcrumb", func(t *testing.T) {
		// Build a model with a paginated message list (simulating 2 pages loaded)
		totalMsgs := messageListPageSize + 200
		msgs := makeMessages(totalMsgs)
		model := NewBuilder().
			WithMessages(msgs...).
			WithLevel(levelMessageList).
			WithPageSize(20).
			Build()
		model.msgListOffset = totalMsgs
		model.cursor = 600

		// Push breadcrumb and navigate to detail view
		model.pushBreadcrumb()
		model.level = levelMessageDetail
		model.cursor = 0

		// Change some pagination state in the detail view context
		model.msgListOffset = 0 // Simulating stale/reset state

		// Go back — should restore the snapshot
		m, _ := sendKey(t, model, keyEsc())

		assertLevel(t, m, levelMessageList)
		assertpkg.Equal(t, totalMsgs, m.msgListOffset, "after goBack")
		assertpkg.Len(t, m.messages, totalMsgs, "after goBack")
	})

	t.Run("goBack resets stale msgListLoadingMore", func(t *testing.T) {
		// Simulate: user is in a paginated message list, a load-more request is
		// in-flight (msgListLoadingMore=true), and the user navigates to detail
		// view. The breadcrumb captures the stale loading flag. When they go
		// back, the flag must be cleared so pagination can resume.
		msgs := makeMessages(messageListPageSize)
		model := NewBuilder().
			WithMessages(msgs...).
			WithLevel(levelMessageList).
			WithPageSize(20).
			Build()
		model.msgListOffset = messageListPageSize
		model.msgListLoadingMore = true // In-flight load-more

		// Push breadcrumb (captures msgListLoadingMore=true) and navigate to detail
		model.pushBreadcrumb()
		model.level = levelMessageDetail
		model.cursor = 0

		// Go back — loadingMore must be cleared since the in-flight request
		// is stale (loadRequestID has changed)
		m, _ := sendKey(t, model, keyEsc())

		assertLevel(t, m, levelMessageList)
		assertpkg.False(t, m.msgListLoadingMore, "after goBack, but it was still true")
	})

	t.Run("goBack preserves msgListComplete flag", func(t *testing.T) {
		msgs := makeMessages(300) // Short page = all data loaded
		model := NewBuilder().
			WithMessages(msgs...).
			WithLevel(levelMessageList).
			WithPageSize(20).
			Build()
		model.msgListOffset = 300
		model.msgListComplete = true

		// Push breadcrumb and navigate forward
		model.pushBreadcrumb()
		model.level = levelMessageDetail
		model.msgListComplete = false // State changes in new view

		// Go back
		m, _ := sendKey(t, model, keyEsc())

		assertLevel(t, m, levelMessageList)
		assertpkg.True(t, m.msgListComplete, "after goBack (restored from breadcrumb)")
	})
}

// TestSearchResultsPreservesDrillDownContextStats verifies that when drilling down
// from a search-filtered aggregate, contextStats (TotalSize, AttachmentCount) set
// from the selected row is preserved when searchResultsMsg arrives.
// This is the fix for the bug where drilling down into a sender after search
// caused TotalSize and AttachmentCount to disappear from the header.
func TestSearchResultsPreservesDrillDownContextStats(t *testing.T) {
	model := newTestModelWithRows(testAggregateRows)
	model.level = levelAggregates
	model.searchQuery = "important"
	model.cursor = 0 // alice@example.com: Count=100, TotalSize=1000, AttachmentCount=5

	// Press Enter to drill down (sets contextStats from selected row)
	m := applyAggregateKey(t, model, keyEnter())

	// Verify contextStats was set from selected row with full stats
	assertContextStats(t, m, 100, 1000, 5)

	// Simulate searchResultsMsg arriving with total count
	m2 := applySearchResults(t, m, m.searchRequestID, []query.MessageSummary{{ID: 1}, {ID: 2}}, 100)

	// contextStats should preserve TotalSize and AttachmentCount from drill-down
	assertContextStats(t, m2, 100, 1000, 5)
}

// TestSearchResultsWithoutDrillDownContextStats verifies that when searching
// without a drill-down context, contextStats is created with only MessageCount.
func TestSearchResultsWithoutDrillDownContextStats(t *testing.T) {
	model := newTestModelAtLevel(levelMessageList)
	model.searchRequestID = 1

	m := applySearchResults(t, model, 1, []query.MessageSummary{{ID: 1}, {ID: 2}}, 50)

	assertContextStats(t, m, 50, 0, 0)
}

// TestSearchStatsUpdateOnSubsequentSearch verifies that typing more characters
// (triggering a new search) updates ALL stats fields, not just MessageCount.
// This was a regression where hasDrillDownStats incorrectly treated stats from
// a previous search as drill-down stats, preventing fresh stats from being applied.
func TestSearchStatsUpdateOnSubsequentSearch(t *testing.T) {
	model := newTestModelAtLevel(levelMessageList)
	model.searchRequestID = 1

	// First search: returns 10 messages with specific stats
	firstStats := &query.TotalStats{
		MessageCount:    10,
		TotalSize:       50000,
		AttachmentCount: 5,
		AttachmentSize:  20000,
		AccountCount:    2,
	}
	m := applySearchResultsWithStats(t, model, 1, make([]query.MessageSummary, 10), 10, firstStats)

	assertContextStats(t, m, 10, 50000, 5)

	// Second search (user typed more): returns 3 messages with different stats
	m.searchRequestID = 2
	secondStats := &query.TotalStats{
		MessageCount:    3,
		TotalSize:       12000,
		AttachmentCount: 1,
		AttachmentSize:  5000,
		AccountCount:    1,
	}
	m2 := applySearchResultsWithStats(t, m, 2, make([]query.MessageSummary, 3), 3, secondStats)

	// ALL stats fields must reflect the second search, not the first
	assertContextStats(t, m2, 3, 12000, 1)
	assertpkg.Equal(t, int64(5000), m2.contextStats.AttachmentSize)
	assertpkg.Equal(t, int64(1), m2.contextStats.AccountCount)
}

// TestSearchStatsUpdateOnDeleteKey verifies that deleting characters (broadening
// the search) also updates ALL stats fields correctly.
func TestSearchStatsUpdateOnDeleteKey(t *testing.T) {
	model := newTestModelAtLevel(levelMessageList)
	model.searchRequestID = 1

	// Narrow search: "foobar" → 2 messages
	narrowStats := &query.TotalStats{
		MessageCount:    2,
		TotalSize:       8000,
		AttachmentCount: 0,
	}
	m := applySearchResultsWithStats(t, model, 1, make([]query.MessageSummary, 2), 2, narrowStats)

	assertContextStats(t, m, 2, 8000, 0)

	// Broader search after delete: "foo" → 15 messages with more attachments
	m.searchRequestID = 2
	broadStats := &query.TotalStats{
		MessageCount:    15,
		TotalSize:       75000,
		AttachmentCount: 8,
		AttachmentSize:  40000,
	}
	m2 := applySearchResultsWithStats(t, m, 2, make([]query.MessageSummary, 15), 15, broadStats)

	assertContextStats(t, m2, 15, 75000, 8)
}

// TestDrillDownStatsPreservedWhenSearchHasNoStats verifies that the drill-down
// stats preservation still works correctly when search results arrive WITHOUT
// fresh stats (e.g., deep/FTS search path).
func TestDrillDownStatsPreservedWhenSearchHasNoStats(t *testing.T) {
	model := newTestModelAtLevel(levelMessageList)
	model.searchRequestID = 1
	// Simulate drill-down context with known stats
	model.contextStats = &query.TotalStats{
		MessageCount:    100,
		TotalSize:       500000,
		AttachmentCount: 25,
	}

	// Search returns results WITHOUT stats (nil) — should preserve drill-down stats
	m := applySearchResults(t, model, 1, make([]query.MessageSummary, 5), 50)

	assertContextStats(t, m, 50, 500000, 25)
}

// TestFreshStatsOverrideDrillDownStats verifies that when a search returns
// fresh stats, they replace even existing drill-down stats.
func TestFreshStatsOverrideDrillDownStats(t *testing.T) {
	model := newTestModelAtLevel(levelMessageList)
	model.searchRequestID = 1
	// Simulate drill-down context with known stats
	model.contextStats = &query.TotalStats{
		MessageCount:    100,
		TotalSize:       500000,
		AttachmentCount: 25,
	}

	// Search returns results WITH fresh stats — should replace drill-down stats
	freshStats := &query.TotalStats{
		MessageCount:    7,
		TotalSize:       30000,
		AttachmentCount: 2,
	}
	m := applySearchResultsWithStats(t, model, 1, make([]query.MessageSummary, 7), 7, freshStats)

	assertContextStats(t, m, 7, 30000, 2)
}

// TestAggregateSearchFilterSetsContextStats verifies contextStats is calculated from
// filtered aggregate rows when a search filter is active.
func TestAggregateSearchFilterSetsContextStats(t *testing.T) {
	assert := assertpkg.New(t)
	model := newTestModelAtLevel(levelAggregates).
		withSearchQuery("test query").
		withAggregateRequestID()

	msg := dataLoadedMsg{
		rows:      testAggregateRows,
		requestID: 1,
	}

	newModel, _ := model.Update(msg)
	m := asModel(t, newModel)

	requirepkg.NotNil(t, m.contextStats, "expected contextStats to be set when search filter is active")

	wantCount, wantSize, wantAttachments := sumAggregateStats(testAggregateRows)
	assert.Equal(wantCount, m.contextStats.MessageCount)
	assert.Equal(wantSize, m.contextStats.TotalSize)
	assert.Equal(wantAttachments, m.contextStats.AttachmentCount)
}

// TestAggregateSearchFilterUsesFilteredStats verifies that contextStats uses
// the filteredStats from the query (distinct message count) rather than summing
// row counts, which would overcount for 1:N views like Recipients and Labels.
func TestAggregateSearchFilterUsesFilteredStats(t *testing.T) {
	model := newTestModelAtLevel(levelAggregates).
		withSearchQuery("test query").
		withAggregateRequestID()

	// Simulate recipient view: rows sum to 175 (inflated) but actual distinct is 100
	filteredStats := &query.TotalStats{MessageCount: 100, TotalSize: 5000, AttachmentCount: 10}
	msg := dataLoadedMsg{
		rows: []query.AggregateRow{
			{Key: "alice@example.com", Count: 80, TotalSize: 4000, AttachmentCount: 5},
			{Key: "bob@example.com", Count: 60, TotalSize: 3000, AttachmentCount: 3},
			{Key: "carol@example.com", Count: 35, TotalSize: 1500, AttachmentCount: 2},
		},
		filteredStats: filteredStats,
		requestID:     1,
	}

	newModel, _ := model.Update(msg)
	m := asModel(t, newModel)

	requirepkg.NotNil(t, m.contextStats, "expected contextStats to be set")
	// Should use filteredStats (100), not sum of row counts (175)
	assertpkg.Equal(t, int64(100), m.contextStats.MessageCount, "from filteredStats, not row sum 175")
	assertpkg.Equal(t, int64(5000), m.contextStats.TotalSize)
}

// TestAggregateNoSearchFilterClearsContextStats verifies contextStats is cleared
// when no search filter is active at aggregate level.
func TestAggregateNoSearchFilterClearsContextStats(t *testing.T) {
	model := newTestModelAtLevel(levelAggregates).
		withAggregateRequestID().
		withContextStats(&query.TotalStats{MessageCount: 500}) // Stale stats

	msg := dataLoadedMsg{
		rows:      testAggregateRows[:1], // Just one row
		requestID: 1,
	}

	newModel, _ := model.Update(msg)
	m := asModel(t, newModel)

	assertpkg.Nil(t, m.contextStats, "expected contextStats to be nil when no search filter at aggregate level")
}

// TestSubAggregateSearchFilterSetsContextStats verifies contextStats is calculated
// at sub-aggregate level when search filter is active.
func TestSubAggregateSearchFilterSetsContextStats(t *testing.T) {
	model := newTestModelAtLevel(levelDrillDown).
		withSearchQuery("important").
		withAggregateRequestID()

	rows := []query.AggregateRow{
		{Key: "work", Count: 30, TotalSize: 3000, AttachmentCount: 10},
		{Key: "personal", Count: 20, TotalSize: 2000, AttachmentCount: 5},
	}

	msg := dataLoadedMsg{
		rows:      rows,
		requestID: 1,
	}

	newModel, _ := model.Update(msg)
	m := asModel(t, newModel)

	requirepkg.NotNil(t, m.contextStats, "expected contextStats to be set at sub-aggregate with search filter")

	wantCount, _, _ := sumAggregateStats(rows)
	assertpkg.Equal(t, wantCount, m.contextStats.MessageCount)
}

// TestHeaderViewShowsFilteredStatsOnSearch verifies the header shows contextStats
// when search filter is active at aggregate level.
func TestHeaderViewShowsFilteredStatsOnSearch(t *testing.T) {
	filteredStats := &query.TotalStats{MessageCount: 42, TotalSize: 12345, AttachmentCount: 7}
	globalStats := &query.TotalStats{MessageCount: 1000, TotalSize: 999999, AttachmentCount: 100}

	model := newTestModelAtLevel(levelAggregates).
		withSearchQuery("test").
		withContextStats(filteredStats).
		withGlobalStats(globalStats)

	header := model.headerView()

	// Should show filtered stats (42 msgs), not global stats (1000 msgs)
	assertpkg.Contains(t, header, "42 msgs", "expected header to show filtered stats")
	assertpkg.NotContains(t, header, "1000 msgs", "header should not show global stats when search filter active")
}

// TestDrillDownWithSearchQueryPreservesSearch verifies that drilling down from a
// filtered aggregate preserves the search query so the message list is filtered.
func TestDrillDownWithSearchQueryPreservesSearch(t *testing.T) {
	model := newTestModelWithRows(testAggregateRows)
	model.level = levelAggregates
	model.searchQuery = "important" // Active search filter
	model.cursor = 0                // alice@example.com

	initialSearchRequestID := model.searchRequestID

	// Press Enter to drill down
	m, cmd := applyAggregateKeyWithCmd(t, model, keyEnter())

	assertLevel(t, m, levelMessageList)
	assertSearchQuery(t, m, "important") // Search preserved
	assertCmd(t, cmd, true)

	// Should use loadSearch (searchRequestID incremented twice: invalidate + new search)
	assertpkg.Equal(t, initialSearchRequestID+2, m.searchRequestID, "expected searchRequestID to increment by 2")
}

// TestDrillDownWithoutSearchQueryUsesLoadMessages verifies that drilling down
// without a search filter uses loadMessages (not search).
func TestDrillDownWithoutSearchQueryUsesLoadMessages(t *testing.T) {
	model := newTestModelWithRows(testAggregateRows)
	model.level = levelAggregates
	model.searchQuery = "" // No search filter
	model.cursor = 0

	// Capture initial request IDs to verify exact increments
	initialLoadRequestID := model.loadRequestID
	initialSearchRequestID := model.searchRequestID

	m, cmd := applyAggregateKeyWithCmd(t, model, keyEnter())

	assertLevel(t, m, levelMessageList)
	assertCmd(t, cmd, true) // Should return command to load messages

	assertpkg.Equal(t, initialLoadRequestID+1, m.loadRequestID, "expected loadRequestID to increment by 1")
	assertpkg.Equal(t, initialSearchRequestID+1, m.searchRequestID, "expected searchRequestID to increment by 1")
}

// TestSubAggregateDrillDownWithSearchQueryPreservesSearch verifies drill-down from
// sub-aggregate preserves the search query.
func TestSubAggregateDrillDownWithSearchQueryPreservesSearch(t *testing.T) {
	model := newTestModelWithRows(testAggregateRows)
	model.level = levelDrillDown
	model.searchQuery = "urgent"
	model.drillFilter = query.MessageFilter{Sender: "alice@example.com"}
	model.drillViewType = query.ViewSenders
	model.viewType = query.ViewLabels
	model.cursor = 0

	initialSearchRequestID := model.searchRequestID

	m, cmd := applyAggregateKeyWithCmd(t, model, keyEnter())

	assertLevel(t, m, levelMessageList)
	assertSearchQuery(t, m, "urgent") // Search preserved
	assertCmd(t, cmd, true)

	assertpkg.Equal(t, initialSearchRequestID+2, m.searchRequestID, "expected searchRequestID to increment by 2")
}

// TestDrillDownSearchBreadcrumbRoundTrip verifies that searching at aggregate level,
// drilling down (which preserves search), then pressing Esc restores the aggregate view
// with the search still in place. Inherited search should not require two Esc presses.
func TestDrillDownSearchBreadcrumbRoundTrip(t *testing.T) {
	model := newTestModelWithRows(testAggregateRows)
	model.level = levelAggregates
	model.searchQuery = "important"
	model.cursor = 0

	// Drill down — search should persist
	m := applyAggregateKey(t, model, keyEnter())

	assertSearchQuery(t, m, "important")
	assertLevel(t, m, levelMessageList)

	// Populate messages so Esc handler works
	m.messages = []query.MessageSummary{{ID: 1}}

	// Single Esc goes back to aggregate with search restored from breadcrumb.
	// Inherited search (no preSearchMessages snapshot) does not get cleared.
	m2 := applyMessageListKey(t, m, keyEsc())

	assertLevel(t, m2, levelAggregates)
	assertSearchQuery(t, m2, "important")
}

// TestDrillDownPreservesSearchQuery verifies that searchQuery persists
// after drill-down so highlighting and filtering remain active.
func TestDrillDownPreservesSearchQuery(t *testing.T) {
	model := newTestModelWithRows(testAggregateRows)
	model.level = levelAggregates
	model.searchQuery = "alice"
	model.cursor = 0

	m := applyAggregateKey(t, model, keyEnter())

	assertSearchQuery(t, m, "alice")
}

// TestSubAggregateDrillDownSearchBreadcrumbRoundTrip verifies the breadcrumb
// round-trip through a sub-aggregate drill-down with active search.
func TestSubAggregateDrillDownSearchBreadcrumbRoundTrip(t *testing.T) {
	model := newTestModelWithRows(testAggregateRows)
	model.level = levelDrillDown
	model.searchQuery = "urgent"
	model.drillFilter = query.MessageFilter{Sender: "alice@example.com"}
	model.drillViewType = query.ViewSenders
	model.viewType = query.ViewLabels
	model.cursor = 0

	// Drill down to message list — search should persist
	m := applyAggregateKey(t, model, keyEnter())

	assertSearchQuery(t, m, "urgent")

	// Single Esc navigates back (inherited search, no snapshot)
	m.messages = []query.MessageSummary{{ID: 1}}
	m2 := applyMessageListKey(t, m, keyEsc())

	assertSearchQuery(t, m2, "urgent")
	assertLevel(t, m2, levelDrillDown)
}

// TestEscBehaviorInheritedVsLocalSearch verifies that Esc at message list level
// distinguishes inherited search (from aggregate drill-down) from locally-initiated
// search. Inherited search: single Esc goes back. Local search: first Esc clears
// search, second Esc goes back.
func TestEscBehaviorInheritedVsLocalSearch(t *testing.T) {
	t.Run("inherited search: single Esc goes back", func(t *testing.T) {
		model := newTestModelWithRows(testAggregateRows)
		model.level = levelAggregates
		model.searchQuery = "avro"
		model.cursor = 0

		// Drill down — search inherited, no preSearchMessages snapshot
		m := applyAggregateKey(t, model, keyEnter())
		m.messages = []query.MessageSummary{{ID: 1}, {ID: 2}}

		requirepkg.Nil(t, m.preSearchMessages, "inherited search should not have preSearchMessages")

		// Single Esc goes back to aggregate with search intact
		m2 := applyMessageListKey(t, m, keyEsc())
		assertLevel(t, m2, levelAggregates)
		assertSearchQuery(t, m2, "avro")
	})

	t.Run("local search: two-step Esc", func(t *testing.T) {
		model := newTestModelWithRows(testAggregateRows)
		model.level = levelAggregates
		model.cursor = 0

		// Drill down without search
		m := applyAggregateKey(t, model, keyEnter())
		m.messages = []query.MessageSummary{{ID: 1}, {ID: 2}, {ID: 3}}
		m.loading = false

		// User initiates search locally — snapshot is created
		cmd := m.activateInlineSearch("search")
		assertCmd(t, cmd, true)
		m.inlineSearchActive = false
		m.searchQuery = "test"
		m.messages = []query.MessageSummary{{ID: 99}}

		requirepkg.NotNil(t, m.preSearchMessages, "local search should have preSearchMessages")

		// First Esc clears the local search, restores pre-search messages
		m2 := applyMessageListKey(t, m, keyEsc())
		assertLevel(t, m2, levelMessageList)
		assertSearchQuery(t, m2, "")
		assertpkg.Len(t, m2.messages, 3, "expected pre-search messages restored")

		// Second Esc goes back to aggregate
		m3 := applyMessageListKey(t, m2, keyEsc())
		assertLevel(t, m3, levelAggregates)
	})
}

// TestStaleSearchResponseIgnoredAfterDrillDown verifies that a search response
// from the aggregate level is ignored after drill-down because searchRequestID
// was incremented.
func TestStaleSearchResponseIgnoredAfterDrillDown(t *testing.T) {
	model := newTestModelWithRows(testAggregateRows)
	model.level = levelAggregates
	model.searchQuery = "important"
	model.searchRequestID = 5 // Simulate prior searches
	model.cursor = 0

	// Capture the pre-drill searchRequestID (this is what an in-flight response would carry)
	staleRequestID := model.searchRequestID

	// Drill down — clears search and increments searchRequestID
	m := applyAggregateKey(t, model, keyEnter())

	// Populate the message list with expected data
	m.messages = []query.MessageSummary{{ID: 100, Subject: "Drilled message"}}
	m.loading = false

	// Simulate a stale search response arriving with the old requestID
	m2 := applySearchResults(t, m, staleRequestID, []query.MessageSummary{{ID: 999, Subject: "Stale search result"}}, 0)

	// The stale response should be ignored — messages unchanged
	requirepkg.Len(t, m2.messages, 1, "stale ignored")
	assertpkg.Equal(t, int64(100), m2.messages[0].ID, "expected message ID 100 (original)")
}

// TestStaleSearchIgnoredAfterInheritedEsc verifies that after pressing Esc to
// go back from an inherited-search message list, in-flight search responses
// are ignored because searchRequestID was incremented.
func TestStaleSearchIgnoredAfterInheritedEsc(t *testing.T) {
	model := newTestModelWithRows(testAggregateRows)
	model.level = levelAggregates
	model.searchQuery = "avro"
	model.cursor = 0

	// Drill down with inherited search
	m := applyAggregateKey(t, model, keyEnter())
	m.messages = []query.MessageSummary{{ID: 1}}

	// Capture the search request ID that the in-flight search carries
	inflightRequestID := m.searchRequestID

	// Esc back to aggregate
	m2 := applyMessageListKey(t, m, keyEsc())
	assertLevel(t, m2, levelAggregates)

	// Stale search response arrives with the old request ID
	m3 := applySearchResults(t, m2, inflightRequestID,
		[]query.MessageSummary{{ID: 999}}, 0)

	// Must be ignored — level and rows unchanged
	assertLevel(t, m3, levelAggregates)
	if len(m3.messages) == 1 {
		assertpkg.NotEqual(t, int64(999), m3.messages[0].ID, "stale search response should have been ignored")
	}
}

// TestAKeyWithActiveSearchUsesLoadSearch verifies that pressing "a" at
// aggregate level with an active search query uses loadSearch (not
// loadMessages) and invalidates stale loadMessages responses.
func TestAKeyWithActiveSearchUsesLoadSearch(t *testing.T) {
	model := newTestModelWithRows(testAggregateRows)
	model.level = levelAggregates
	model.searchQuery = "important"
	model.cursor = 0

	initialSearchID := model.searchRequestID
	initialLoadID := model.loadRequestID

	m, cmd := applyAggregateKeyWithCmd(t, model, key('a'))

	assertLevel(t, m, levelMessageList)
	assertSearchQuery(t, m, "important")
	assertCmd(t, cmd, true)

	// searchRequestID should increment (invalidate + new search)
	assertpkg.Greater(t, m.searchRequestID, initialSearchID)
	// loadRequestID should also increment to invalidate stale loads
	assertpkg.Greater(t, m.loadRequestID, initialLoadID)
}

// TestInheritedSearchLocalReSearchSnapshots verifies that pressing /
// from a message list with inherited search (no snapshot) creates a
// snapshot, allowing two-step Esc to restore the inherited results.
func TestInheritedSearchLocalReSearchSnapshots(t *testing.T) {
	model := newTestModelWithRows(testAggregateRows)
	model.level = levelAggregates
	model.searchQuery = "avro"
	model.cursor = 0

	// Drill down with inherited search
	m := applyAggregateKey(t, model, keyEnter())
	m.messages = []query.MessageSummary{{ID: 1}, {ID: 2}}
	m.loading = false

	// User presses / to refine search — should snapshot inherited results
	cmd := m.activateInlineSearch("search")
	assertCmd(t, cmd, true)

	requirepkg.NotNil(t, m.preSearchMessages, "activateInlineSearch should snapshot inherited results")
	assertpkg.Len(t, m.preSearchMessages, 2, "snapshot")

	// Simulate new search committed
	m.inlineSearchActive = false
	m.searchQuery = "new query"
	m.messages = []query.MessageSummary{{ID: 99}}

	// First Esc restores inherited search results
	m2 := applyMessageListKey(t, m, keyEsc())
	assertLevel(t, m2, levelMessageList)
	assertSearchQuery(t, m2, "")
	assertpkg.Len(t, m2.messages, 2, "expected inherited messages restored")
}

// TestPreSearchSnapshotRestoreOnEsc verifies that activating inline search at the
// message list level snapshots state, and Esc restores it instantly without re-query.
func TestPreSearchSnapshotRestoreOnEsc(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	originalMsgs := []query.MessageSummary{{ID: 1, Subject: "Msg1"}, {ID: 2, Subject: "Msg2"}}
	originalStats := &query.TotalStats{MessageCount: 100, TotalSize: 5000}

	model := NewBuilder().WithMessages(originalMsgs...).
		WithLevel(levelMessageList).WithSize(100, 24).Build()
	model.messages = originalMsgs
	model.cursor = 1
	model.scrollOffset = 0
	model.contextStats = originalStats

	// Activate inline search — should snapshot and return blink command
	cmd := model.activateInlineSearch("search")
	assertCmd(t, cmd, true) // Should return textinput.Blink command

	// Verify snapshot was taken
	require.NotNil(model.preSearchMessages, "expected preSearchMessages to be set")

	// Simulate search results arriving — mutates contextStats and replaces messages
	model.searchQuery = "test"
	model.messages = []query.MessageSummary{{ID: 99, Subject: "SearchResult"}}
	model.cursor = 0
	model.contextStats.MessageCount = 1 // Mutate original pointer
	model.searchLoadingMore = true
	model.searchOffset = 50
	model.searchTotalCount = 200

	// Esc from inline search — should restore snapshot
	m, _ := applyInlineSearchKey(t, model, keyEsc())

	// Messages restored
	require.Len(m.messages, 2, "expected messages restored")
	assert.Equal(int64(1), m.messages[0].ID)

	// Cursor restored
	assert.Equal(1, m.cursor)

	// Stats restored (deep copy: original mutation shouldn't affect snapshot)
	require.NotNil(m.contextStats, "expected contextStats restored")
	assert.Equal(int64(100), m.contextStats.MessageCount)

	// Search state fully cleared
	assertSearchQuery(t, m, "")
	assert.False(m.searchLoadingMore, "after restore")
	assert.Equal(0, m.searchOffset)
	assert.Equal(int64(0), m.searchTotalCount)
	assertLoading(t, m, false, false)

	// Snapshot cleared
	assert.Nil(m.preSearchMessages, "after restore")
}

// TestTwoStepEscClearsSearchThenGoesBack verifies that the first Esc clears
// the inner search and the second Esc navigates back via goBack.
func TestTwoStepEscClearsSearchThenGoesBack(t *testing.T) {
	// Start at aggregate level, drill down, then search at message list level
	model := newTestModelWithRows(testAggregateRows)
	model.level = levelAggregates
	model.cursor = 0

	// Drill down to message list
	m := applyAggregateKey(t, model, keyEnter())
	m.messages = []query.MessageSummary{{ID: 1}, {ID: 2}, {ID: 3}}
	m.loading = false

	// Activate search and simulate results
	cmd := m.activateInlineSearch("search")
	assertCmd(t, cmd, true)      // Should return textinput.Blink command
	m.inlineSearchActive = false // Simulate search submitted
	m.searchQuery = "test"
	m.messages = []query.MessageSummary{{ID: 99}}

	// First Esc — should clear search and restore pre-search messages
	m2 := applyMessageListKey(t, m, keyEsc())

	assertSearchQuery(t, m2, "")
	assertLevel(t, m2, levelMessageList)
	assertpkg.Len(t, m2.messages, 3, "expected pre-search messages restored")

	// Second Esc — should goBack to aggregates
	m3 := applyMessageListKey(t, m2, keyEsc())

	assertLevel(t, m3, levelAggregates)
}

// TestZeroSearchResultsRendersSearchBar verifies that the view still shows
// the search bar, "No results found", and "(0 results)" when a fast search
// returns zero matches (instead of breaking the layout).
func TestZeroSearchResultsRendersSearchBar(t *testing.T) {
	t.Run("inline search active with zero results", func(t *testing.T) {
		model := NewBuilder().
			WithPageSize(20).WithSize(100, 30).
			Build()
		model = resizeModel(t, model, 100, 30)
		model.searchRequestID = 1

		// Simulate: user activated inline search, typed a query, got zero results
		m := applySearchResults(t, model, 1, []query.MessageSummary{}, 0)
		m.inlineSearchActive = true
		m.searchInput.SetValue("nonexistent_query")
		m.searchMode = searchModeFast

		view := m.View()
		assertViewFitsHeight(t, view, 30)

		assertpkg.Contains(t, view, "No results found")
		assertpkg.Contains(t, view, "[Fast]/", "expected search bar with '[Fast]/' prefix")
	})

	t.Run("completed search with zero results shows count", func(t *testing.T) {
		model := NewBuilder().
			WithPageSize(20).WithSize(100, 30).
			Build()
		model = resizeModel(t, model, 100, 30)
		model.searchRequestID = 1

		// Simulate: user completed a search (pressed Enter), got zero results
		m := applySearchResults(t, model, 1, []query.MessageSummary{}, 0)
		m.searchQuery = "nonexistent_query"
		m.searchTotalCount = 0

		view := m.View()
		assertViewFitsHeight(t, view, 30)

		assertpkg.Contains(t, view, "No results found")
		assertpkg.Contains(t, view, "(0 results)", "in info line")
		assertpkg.Contains(t, view, "nonexistent_query", "expected search query shown in info line")
	})

	t.Run("non-search empty state still shows No messages", func(t *testing.T) {
		assert := assertpkg.New(t)
		model := NewBuilder().
			WithLevel(levelMessageList).
			WithLoading(false).
			WithPageSize(20).WithSize(100, 30).
			Build()
		model = resizeModel(t, model, 100, 30)
		// No search active, no search query — plain empty state

		view := model.View()
		assertViewFitsHeight(t, view, 30)

		assert.Contains(view, "No messages", "in non-search empty view")
		// Should NOT show search UI elements in the non-search empty state
		assert.NotContains(view, "No results found", "when no search is active")
		assert.NotContains(view, "[Fast]/", "when no search is active")
		assert.NotContains(view, "(0 results)", "when no search is active")
	})
}

// TestStaleLoadMessagesDoesNotOverwriteSearch verifies that an in-flight
// loadMessages response (e.g., from pressing "a" or "v" before searching)
// does not overwrite search results. This was a race condition: the TUI would
// show stale "all messages" results instead of the filtered search results.
func TestStaleLoadMessagesDoesNotOverwriteSearch(t *testing.T) {
	allMessages := makeMessages(50)
	searchResults := []query.MessageSummary{
		{ID: 901, Subject: "Search Hit 1"},
		{ID: 902, Subject: "Search Hit 2"},
	}

	model := NewBuilder().
		WithMessages(allMessages...).
		WithLevel(levelMessageList).
		WithPageSize(20).WithSize(100, 30).
		Build()

	// Simulate: user presses "v" (sort toggle) which fires loadMessages
	// with loadRequestID=N.
	staleLoadRequestID := model.loadRequestID

	// Simulate: user then activates search, which should increment loadRequestID
	// to invalidate the pending loadMessages.
	model.searchQuery = "test"
	model.inlineSearchActive = true
	model.searchRequestID++
	model.loadRequestID++ // This is the fix under test

	// Simulate: search results arrive and are applied.
	model = applySearchResults(t, model, model.searchRequestID, searchResults, 2)
	requirepkg.Len(t, model.messages, 2, "expected search results")

	// Simulate: the stale loadMessages response arrives with the OLD requestID.
	staleMsg := messagesLoadedMsg{
		messages:  allMessages,
		requestID: staleLoadRequestID,
	}
	model = sendMsg(t, model, staleMsg)

	// The stale response must be ignored — search results should remain.
	requirepkg.Len(t, model.messages, 2, "stale loadMessages overwrote search results")
	assertpkg.Equal(t, "Search Hit 1", model.messages[0].Subject)
}

// TestSearchClearsStaleMessages verifies that starting an inline search
// immediately clears the message list so stale "all messages" results
// are never visible during the search transition.
func TestSearchClearsStaleMessages(t *testing.T) {
	allMessages := makeMessages(50)

	model := NewBuilder().
		WithMessages(allMessages...).
		WithLevel(levelMessageList).
		WithPageSize(20).WithSize(100, 30).
		Build()

	requirepkg.Len(t, model.messages, 50, "expected pre-loaded messages")

	// Simulate debounce firing with a search query.
	debounceMsg := searchDebounceMsg{
		query:      "test",
		debounceID: model.inlineSearchDebounce,
	}
	model.inlineSearchActive = true
	model = sendMsg(t, model, debounceMsg)

	// Messages should be nil immediately — not showing stale results while
	// waiting for the async search to complete.
	assertpkg.Nil(t, model.messages, "expected messages to be nil after search starts")
}

// TestHighlightedColumnsAligned verifies that highlighting search terms in
// aggregate rows doesn't break column alignment.
