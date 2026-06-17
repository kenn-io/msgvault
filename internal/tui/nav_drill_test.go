package tui

import (
	"context"
	"errors"
	"testing"

	assertpkg "github.com/stretchr/testify/assert"
	requirepkg "github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/query/querytest"
)

// =============================================================================
// Sub-Grouping and Drill-Down Navigation Tests
// =============================================================================

func TestSubGroupingNavigation(t *testing.T) {
	assert := assertpkg.New(t)
	rows := []query.AggregateRow{
		{Key: "alice@example.com", Count: 10},
		{Key: "bob@example.com", Count: 5},
	}
	msgs := []query.MessageSummary{
		{ID: 1, Subject: "Test 1"},
		{ID: 2, Subject: "Test 2"},
	}

	model := NewBuilder().WithRows(rows...).WithMessages(msgs...).
		WithPageSize(10).WithSize(100, 20).WithViewType(query.ViewSenders).Build()

	// Press Enter to drill into first sender - should go to message list (not sub-aggregate)
	newModel, cmd := model.handleAggregateKeys(keyEnter())
	m := asModel(t, newModel)

	assertLevel(t, m, levelMessageList)
	assert.True(m.hasDrillFilter(), "expected drillFilter to be set")
	assert.Equal("alice@example.com", m.drillFilter.Sender)
	assert.Equal(query.ViewSenders, m.drillViewType)
	assert.NotNil(cmd, "expected command to load messages")

	// Should have a breadcrumb
	assert.Len(m.breadcrumbs, 1)

	// Test Tab from message list goes to sub-aggregate view
	m.messages = msgs // Simulate messages loaded
	newModel, cmd = m.handleMessageListKeys(keyTab())
	m = asModel(t, newModel)

	assertLevel(t, m, levelDrillDown)
	// Default sub-group after drilling from Senders should be Recipients (skips redundant SenderNames)
	assert.Equal(query.ViewRecipients, m.viewType, "expected viewType for sub-grouping")
	assert.NotNil(cmd, "expected command to load sub-aggregate data")

	// Test Tab in sub-aggregate cycles views (skipping drill view type)
	m.rows = rows // Simulate data loaded
	newModel, cmd = m.handleAggregateKeys(keyTab())
	m = asModel(t, newModel)

	// From ViewRecipients, Tab cycles to ViewRecipientNames
	assert.Equal(query.ViewRecipientNames, m.viewType, "after Tab")
	assert.NotNil(cmd, "expected command to reload data after Tab")

	// Test Esc goes back to message list (not all the way to aggregates)
	m.rows = rows
	m = applyAggregateKey(t, m, keyEsc())

	assertLevel(t, m, levelMessageList)
	// Drill filter should still be set (we're still viewing alice's messages)
	assert.True(m.hasDrillFilter(), "expected drillFilter to still be set in message list")
	// Should have 1 breadcrumb (from aggregates → message list)
	assert.Len(m.breadcrumbs, 1, "after going back to message list")

	// Test Esc again goes back to aggregates
	m.messages = msgs
	m = applyMessageListKey(t, m, keyEsc())

	assertLevel(t, m, levelAggregates)
	assert.False(m.hasDrillFilter(), "expected drillFilter to be cleared after going back to aggregates")
	assert.Empty(m.breadcrumbs, "after going back to aggregates")
}

func TestSubAggregateDrillDown(t *testing.T) {
	model := NewBuilder().
		WithRows(query.AggregateRow{Key: "bob@example.com", Count: 3}).
		WithMessages(query.MessageSummary{ID: 1, Subject: "Test"}).
		WithPageSize(10).WithSize(100, 20).
		WithLevel(levelDrillDown).WithViewType(query.ViewRecipients).
		Build()
	model.drillViewType = query.ViewSenders
	model.drillFilter = query.MessageFilter{Sender: "alice@example.com"}

	// Press Enter on recipient - should go to message list with combined filter
	newModel, cmd := model.handleAggregateKeys(keyEnter())
	m := asModel(t, newModel)

	assertLevel(t, m, levelMessageList)
	// Drill filter should now include both sender and recipient
	assertpkg.Equal(t, "alice@example.com", m.drillFilter.Sender)
	assertpkg.Equal(t, "bob@example.com", m.drillFilter.Recipient)
	assertpkg.NotNil(t, cmd, "expected command to load messages")
}

// =============================================================================
// Stats Update on Drill-Down Tests
// =============================================================================

// statsTracker records GetTotalStats calls on a querytest.MockEngine.
type statsTracker struct {
	callCount int
	lastOpts  query.StatsOptions
	result    *query.TotalStats // returned when non-nil; otherwise a default
}

// install wires the tracker into eng.GetTotalStatsFunc.
func (st *statsTracker) install(eng *querytest.MockEngine) {
	eng.GetTotalStatsFunc = func(_ context.Context, opts query.StatsOptions) (*query.TotalStats, error) {
		st.callCount++
		st.lastOpts = opts
		if st.result != nil {
			return st.result, nil
		}
		return &query.TotalStats{MessageCount: 1000, TotalSize: 5000000, AttachmentCount: 50}, nil
	}
}

// TestStatsUpdateOnDrillDown verifies stats are reloaded when drilling into a subgroup.
func TestStatsUpdateOnDrillDown(t *testing.T) {
	assert := assertpkg.New(t)
	engine := newMockEngine(MockConfig{
		Rows: []query.AggregateRow{
			{Key: "alice@example.com", Count: 100, TotalSize: 500000},
			{Key: "bob@example.com", Count: 50, TotalSize: 250000},
		},
		Messages: []query.MessageSummary{{ID: 1, Subject: "Test"}},
	})
	tracker := &statsTracker{}
	tracker.install(engine)

	model := New(engine, Options{DataDir: "/tmp/test", Version: "test123"})
	model.rows = engine.AggregateRows
	model.pageSize = 10
	model.width = 100
	model.height = 20
	model.level = levelAggregates
	model.viewType = query.ViewSenders
	model.cursor = 0

	// Press Enter to drill down into alice's messages
	newModel, cmd := model.handleAggregateKeys(keyEnter())
	m := asModel(t, newModel)

	// Verify we transitioned to message list
	assertLevel(t, m, levelMessageList)

	// The stats should be refreshed for the drill-down context
	assert.NotNil(cmd, "expected command to load messages/stats")

	// Verify drillFilter is set correctly
	assert.Equal("alice@example.com", m.drillFilter.Sender)

	// Verify contextStats is set from selected row (not from GetTotalStats call)
	if assert.NotNil(m.contextStats, "expected contextStats to be set from selected row") {
		assert.Equal(int64(100), m.contextStats.MessageCount)
	}
}

// TestContextStatsSetOnDrillDown verifies contextStats is set from selected row.
func TestContextStatsSetOnDrillDown(t *testing.T) {
	assert := assertpkg.New(t)
	rows := []query.AggregateRow{
		{Key: "alice@example.com", Count: 100, TotalSize: 500000, AttachmentSize: 100000},
		{Key: "bob@example.com", Count: 50, TotalSize: 250000, AttachmentSize: 50000},
	}
	engine := newMockEngine(MockConfig{Rows: rows, Messages: []query.MessageSummary{{ID: 1, Subject: "Test"}}})

	model := New(engine, Options{DataDir: "/tmp/test", Version: "test123"})
	model.rows = rows
	model.pageSize = 10
	model.width = 100
	model.height = 20
	model.level = levelAggregates
	model.viewType = query.ViewSenders
	model.cursor = 0 // Select alice

	// Before drill-down, contextStats should be nil
	assert.Nil(model.contextStats, "expected contextStats=nil before drill-down")

	// Press Enter to drill down into alice's messages
	newModel, _ := model.handleAggregateKeys(keyEnter())
	m := asModel(t, newModel)

	// Verify contextStats is set from selected row
	requirepkg.NotNil(t, m.contextStats, "expected contextStats to be set after drill-down")
	assert.Equal(int64(100), m.contextStats.MessageCount)
	assert.Equal(int64(500000), m.contextStats.TotalSize)
}

// TestContextStatsClearedOnGoBack verifies contextStats is cleared when going back to aggregates.
func TestContextStatsClearedOnGoBack(t *testing.T) {
	model := NewBuilder().
		WithRows(query.AggregateRow{Key: "alice@example.com", Count: 100, TotalSize: 500000}).
		WithMessages(query.MessageSummary{ID: 1, Subject: "Test"}).
		WithPageSize(10).WithSize(100, 20).
		WithViewType(query.ViewSenders).Build()

	// Drill down
	m := drillDown(t, model)

	requirepkg.NotNil(t, m.contextStats, "expected contextStats to be set after drill-down")

	// Go back
	newModel2, _ := m.goBack()
	m2 := asModel(t, newModel2)

	// contextStats should be cleared
	assertpkg.Nil(t, m2.contextStats, "expected contextStats=nil after going back to aggregates")
}

// TestContextStatsRestoredOnGoBackToSubAggregate verifies contextStats is restored when going back.
func TestContextStatsRestoredOnGoBackToSubAggregate(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	msgs := []query.MessageSummary{{ID: 1, Subject: "Test"}}
	model := NewBuilder().
		WithRows(
			query.AggregateRow{Key: "alice@example.com", Count: 100, TotalSize: 500000},
			query.AggregateRow{Key: "bob@example.com", Count: 50, TotalSize: 250000},
		).
		WithMessages(msgs...).
		WithPageSize(10).WithSize(100, 20).
		WithViewType(query.ViewSenders).Build()

	// Step 1: Drill down to message list (sets contextStats from alice's row)
	m := applyAggregateKey(t, model, keyEnter())
	require.NotNil(m.contextStats)
	require.Equal(int64(100), m.contextStats.MessageCount)

	// Simulate messages loaded and transition to message list level
	m.level = levelMessageList
	m.messages = msgs
	m.filterKey = "alice@example.com"
	originalContextStats := m.contextStats

	// Step 2: Press Tab to go to sub-aggregate view (contextStats saved in breadcrumb)
	m2 := applyMessageListKey(t, m, keyTab())
	// Simulate data load completing with sub-aggregate rows
	m2.rows = []query.AggregateRow{
		{Key: "domain1.com", Count: 60, TotalSize: 300000},
		{Key: "domain2.com", Count: 40, TotalSize: 200000},
	}
	m2.loading = false
	assertLevel(t, m2, levelDrillDown)
	// contextStats should still be the same (alice's stats)
	assert.Same(originalContextStats, m2.contextStats, "contextStats should be preserved after Tab")

	// Step 3: Drill down from sub-aggregate to message list (contextStats overwritten)
	m3 := applyAggregateKey(t, m2, keyEnter())
	assertLevel(t, m3, levelMessageList)
	// contextStats should now be domain1's stats (60)
	require.NotNil(m3.contextStats)
	assert.Equal(int64(60), m3.contextStats.MessageCount, "expected domain1's stats")

	// Step 4: Go back to sub-aggregate (contextStats should be restored to alice's stats)
	newModel4, _ := m3.goBack()
	m4 := asModel(t, newModel4)
	assertLevel(t, m4, levelDrillDown)
	// contextStats should be restored from breadcrumb
	if assert.NotNil(m4.contextStats, "expected contextStats to be restored after goBack") {
		assert.Equal(int64(100), m4.contextStats.MessageCount, "after goBack")
	}
}

// =============================================================================
// View Type Restoration Tests
// =============================================================================

// TestViewTypeRestoredAfterEscFromSubAggregate verifies viewType is restored when
// navigating back from sub-aggregate to message list.
func TestViewTypeRestoredAfterEscFromSubAggregate(t *testing.T) {
	model := NewBuilder().
		WithMessages(query.MessageSummary{ID: 1}, query.MessageSummary{ID: 2}).
		WithLevel(levelMessageList).WithViewType(query.ViewSenders).Build()
	model.drillFilter = query.MessageFilter{Sender: "alice@example.com"}
	model.drillViewType = query.ViewSenders
	model.cursor = 1
	model.scrollOffset = 0

	// Press Tab to go to sub-aggregate (changes viewType)
	m, _ := sendKey(t, model, keyTab())

	assertLevel(t, m, levelDrillDown)
	// viewType should have changed to next sub-group view (Recipients, skipping redundant SenderNames)
	assertpkg.Equal(t, query.ViewRecipients, m.viewType, "in sub-aggregate")

	// Press Esc to go back to message list
	newModel2, _ := m.goBack()
	m2 := asModel(t, newModel2)

	assertLevel(t, m2, levelMessageList)
	// viewType should be restored to ViewSenders
	assertpkg.Equal(t, query.ViewSenders, m2.viewType, "after going back")
}

// TestCursorScrollPreservedAfterGoBack verifies cursor and scroll are preserved
// when navigating back. With view caching, data is restored from cache instantly
// without requiring a reload.
func TestCursorScrollPreservedAfterGoBack(t *testing.T) {
	assert := assertpkg.New(t)
	rows := makeRows()
	model := NewBuilder().WithRows(rows...).WithViewType(query.ViewSenders).Build()
	model.cursor = 5
	model.scrollOffset = 3

	// Drill down to message list (saves breadcrumb with cached rows)
	m, _ := sendKey(t, model, keyEnter())

	assertLevel(t, m, levelMessageList)

	// Verify breadcrumb was saved with cached rows
	requirepkg.Len(t, m.breadcrumbs, 1)
	assert.NotNil(m.breadcrumbs[0].state.rows, "expected CachedRows to be set in breadcrumb")

	// Go back to aggregates - with caching, this restores instantly without reload
	newModel2, cmd := m.goBack()
	m2 := asModel(t, newModel2)

	// With caching, no reload command is returned
	assert.Nil(cmd, "expected nil command when restoring from cache")

	// Loading should be false (no async reload needed)
	assert.False(m2.loading, "expected loading=false when restoring from cache")

	// Cursor and scroll should be preserved from breadcrumb
	assert.Equal(5, m2.cursor)
	assert.Equal(3, m2.scrollOffset)

	// Rows should be restored from cache
	assert.Len(m2.rows, 10)
}

// TestGoBackClearsError verifies that goBack clears any stale error.
func TestGoBackClearsError(t *testing.T) {
	model := NewBuilder().WithLevel(levelMessageList).Build()
	model.err = errors.New("some previous error")
	model.breadcrumbs = []navigationSnapshot{{state: viewState{
		level:    levelAggregates,
		viewType: query.ViewSenders,
	}}}

	// Go back
	newModel, _ := model.goBack()
	m := asModel(t, newModel)

	// Error should be cleared
	assertpkg.NoError(t, m.err, "expected err=nil after goBack")
}

// TestDrillFilterPreservedAfterMessageDetail verifies drillFilter is preserved
// when navigating back from message detail to message list.
func TestDrillFilterPreservedAfterMessageDetail(t *testing.T) {
	assert := assertpkg.New(t)
	model := NewBuilder().
		WithMessages(
			query.MessageSummary{ID: 1, Subject: "Test message"},
			query.MessageSummary{ID: 2, Subject: "Another message"},
		).
		WithLevel(levelMessageList).WithViewType(query.ViewRecipients).Build()
	model.drillFilter = query.MessageFilter{
		Sender:    "alice@example.com",
		Recipient: "bob@example.com",
	}
	model.drillViewType = query.ViewSenders
	model.filterKey = "bob@example.com"

	// Press Enter to go to message detail
	m, _ := sendKey(t, model, keyEnter())

	assertLevel(t, m, levelMessageDetail)

	// Verify breadcrumb saved drillFilter
	requirepkg.NotEmpty(t, m.breadcrumbs, "expected breadcrumb to be saved")
	bc := m.breadcrumbs[len(m.breadcrumbs)-1]
	assert.Equal("alice@example.com", bc.state.drillFilter.Sender)
	assert.Equal("bob@example.com", bc.state.drillFilter.Recipient)
	assert.Equal(query.ViewSenders, bc.state.drillViewType)

	// Press Esc to go back to message list
	newModel2, _ := m.goBack()
	m2 := asModel(t, newModel2)

	assertLevel(t, m2, levelMessageList)

	// drillFilter should be restored
	assert.Equal("alice@example.com", m2.drillFilter.Sender)
	assert.Equal("bob@example.com", m2.drillFilter.Recipient)
	assert.Equal(query.ViewSenders, m2.drillViewType)
	assert.Equal(query.ViewRecipients, m2.viewType)
}

// =============================================================================
// Breadcrumb Tests
// =============================================================================

func TestPushBreadcrumb(t *testing.T) {
	m := NewBuilder().Build()

	requirepkg.Empty(t, m.breadcrumbs, "expected no breadcrumbs initially")

	m.pushBreadcrumb()
	assertpkg.Len(t, m.breadcrumbs, 1)

	m.pushBreadcrumb()
	assertpkg.Len(t, m.breadcrumbs, 2)
}

// =============================================================================
// Selection Preservation Tests
// =============================================================================

func TestSubAggregateDrillDownPreservesSelection(t *testing.T) {
	// Regression test: drilling down from sub-aggregate via Enter should NOT
	// clear the aggregate selection (only top-level Enter does that).
	model := NewBuilder().
		WithRows(
			query.AggregateRow{Key: "alice@example.com", Count: 100, TotalSize: 500000},
			query.AggregateRow{Key: "bob@example.com", Count: 50, TotalSize: 250000},
		).
		Build()

	// Step 1: Drill down from top-level to message list (Enter on alice)
	model.cursor = 0
	m1 := applyAggregateKey(t, model, keyEnter())
	assertLevel(t, m1, levelMessageList)

	// Step 2: Go to sub-aggregate view (Tab)
	m1.rows = []query.AggregateRow{
		{Key: "domain1.com", Count: 60, TotalSize: 300000},
		{Key: "domain2.com", Count: 40, TotalSize: 200000},
	}
	m1.loading = false
	m2 := applyMessageListKey(t, m1, keyTab())
	assertLevel(t, m2, levelDrillDown)

	// Step 3: Select an aggregate in sub-aggregate view, then drill down with Enter
	m2.rows = []query.AggregateRow{
		{Key: "domain1.com", Count: 60, TotalSize: 300000},
		{Key: "domain2.com", Count: 40, TotalSize: 200000},
	}
	m2.loading = false
	m2.selection.aggregateKeys["domain2.com"] = true
	m2.cursor = 0

	m3 := applyAggregateKey(t, m2, keyEnter())
	assertLevel(t, m3, levelMessageList)

	// The selection should NOT have been cleared by the sub-aggregate Enter
	assertpkg.NotEmpty(t, m3.selection.aggregateKeys, "sub-aggregate Enter should not clear aggregate selection")
}

func TestTopLevelDrillDownClearsSelection(t *testing.T) {
	// Top-level Enter should clear selections (contrasts with sub-aggregate behavior)
	model := NewBuilder().
		WithRows(
			query.AggregateRow{Key: "alice@example.com", Count: 100, TotalSize: 500000},
			query.AggregateRow{Key: "bob@example.com", Count: 50, TotalSize: 250000},
		).
		Build()

	// Select bob, then drill into alice via Enter
	model.selection.aggregateKeys["bob@example.com"] = true
	model.cursor = 0

	m := applyAggregateKey(t, model, keyEnter())
	assertLevel(t, m, levelMessageList)

	// Selection should be cleared
	assertpkg.Empty(t, m.selection.aggregateKeys, "top-level Enter should clear aggregate selection")
	assertpkg.Empty(t, m.selection.messageIDs, "top-level Enter should clear message selection")
}

// =============================================================================
// Sub-Aggregate 'a' Key Tests
// =============================================================================

// TestSubAggregateAKeyJumpsToMessages verifies 'a' key in sub-aggregate view
// jumps to message list with the drill filter applied.
func TestSubAggregateAKeyJumpsToMessages(t *testing.T) {
	assert := assertpkg.New(t)
	model := NewBuilder().
		WithRows(
			query.AggregateRow{Key: "work", Count: 5},
			query.AggregateRow{Key: "personal", Count: 3},
		).
		WithLevel(levelDrillDown).WithViewType(query.ViewLabels).
		WithPageSize(10).WithSize(100, 20).Build()
	model.drillFilter = query.MessageFilter{Sender: "alice@example.com"}
	model.drillViewType = query.ViewSenders

	// Press 'a' to jump to all messages (with drill filter)
	newModel, cmd := model.handleAggregateKeys(key('a'))
	m := asModel(t, newModel)

	// Should navigate to message list
	assertLevel(t, m, levelMessageList)

	// Should have a command to load messages
	assert.NotNil(cmd, "expected command to load messages")

	// Should preserve drill filter
	assert.Equal("alice@example.com", m.drillFilter.Sender)

	// Should have saved breadcrumb
	requirepkg.Len(t, m.breadcrumbs, 1)

	// Breadcrumb should be for sub-aggregate level
	assert.Equal(levelDrillDown, m.breadcrumbs[0].state.level)
}
