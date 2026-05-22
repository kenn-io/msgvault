package tui

import (
	"context"
	"fmt"
	"testing"

	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/query/querytest"
)

// =============================================================================
// Sub-Grouping and Drill-Down Navigation Tests
// =============================================================================

func TestSubGroupingNavigation(t *testing.T) {
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
	m := newModel.(Model)

	assertLevel(t, m, levelMessageList)
	if !m.hasDrillFilter() {
		t.Error("expected drillFilter to be set")
	}
	if m.drillFilter.Sender != "alice@example.com" {
		t.Errorf("expected drillFilter.Sender = alice@example.com, got %s", m.drillFilter.Sender)
	}
	if m.drillViewType != query.ViewSenders {
		t.Errorf("expected drillViewType = ViewSenders, got %v", m.drillViewType)
	}
	if cmd == nil {
		t.Error("expected command to load messages")
	}

	// Should have a breadcrumb
	if len(m.breadcrumbs) != 1 {
		t.Errorf("expected 1 breadcrumb, got %d", len(m.breadcrumbs))
	}

	// Test Tab from message list goes to sub-aggregate view
	m.messages = msgs // Simulate messages loaded
	newModel, cmd = m.handleMessageListKeys(keyTab())
	m = newModel.(Model)

	assertLevel(t, m, levelDrillDown)
	// Default sub-group after drilling from Senders should be Recipients (skips redundant SenderNames)
	if m.viewType != query.ViewRecipients {
		t.Errorf("expected viewType = ViewRecipients for sub-grouping, got %v", m.viewType)
	}
	if cmd == nil {
		t.Error("expected command to load sub-aggregate data")
	}

	// Test Tab in sub-aggregate cycles views (skipping drill view type)
	m.rows = rows // Simulate data loaded
	newModel, cmd = m.handleAggregateKeys(keyTab())
	m = newModel.(Model)

	// From ViewRecipients, Tab cycles to ViewRecipientNames
	if m.viewType != query.ViewRecipientNames {
		t.Errorf("expected viewType = ViewRecipientNames after Tab, got %v", m.viewType)
	}
	if cmd == nil {
		t.Error("expected command to reload data after Tab")
	}

	// Test Esc goes back to message list (not all the way to aggregates)
	m.rows = rows
	m = applyAggregateKey(t, m, keyEsc())

	assertLevel(t, m, levelMessageList)
	// Drill filter should still be set (we're still viewing alice's messages)
	if !m.hasDrillFilter() {
		t.Error("expected drillFilter to still be set in message list")
	}
	// Should have 1 breadcrumb (from aggregates → message list)
	if len(m.breadcrumbs) != 1 {
		t.Errorf("expected 1 breadcrumb after going back to message list, got %d", len(m.breadcrumbs))
	}

	// Test Esc again goes back to aggregates
	m.messages = msgs
	m = applyMessageListKey(t, m, keyEsc())

	assertLevel(t, m, levelAggregates)
	if m.hasDrillFilter() {
		t.Error("expected drillFilter to be cleared after going back to aggregates")
	}
	if len(m.breadcrumbs) != 0 {
		t.Errorf("expected 0 breadcrumbs after going back to aggregates, got %d", len(m.breadcrumbs))
	}
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
	m := newModel.(Model)

	assertLevel(t, m, levelMessageList)
	// Drill filter should now include both sender and recipient
	if m.drillFilter.Sender != "alice@example.com" {
		t.Errorf("expected drillFilter.Sender = alice@example.com, got %s", m.drillFilter.Sender)
	}
	if m.drillFilter.Recipient != "bob@example.com" {
		t.Errorf("expected drillFilter.Recipient = bob@example.com, got %s", m.drillFilter.Recipient)
	}
	if cmd == nil {
		t.Error("expected command to load messages")
	}
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
	m := newModel.(Model)

	// Verify we transitioned to message list
	assertLevel(t, m, levelMessageList)

	// The stats should be refreshed for the drill-down context
	if cmd == nil {
		t.Error("expected command to load messages/stats")
	}

	// Verify drillFilter is set correctly
	if m.drillFilter.Sender != "alice@example.com" {
		t.Errorf("expected drillFilter.Sender='alice@example.com', got '%s'", m.drillFilter.Sender)
	}

	// Verify contextStats is set from selected row (not from GetTotalStats call)
	if m.contextStats == nil {
		t.Error("expected contextStats to be set from selected row")
	} else {
		if m.contextStats.MessageCount != 100 {
			t.Errorf("expected contextStats.MessageCount=100, got %d", m.contextStats.MessageCount)
		}
	}
}

// TestContextStatsSetOnDrillDown verifies contextStats is set from selected row.
func TestContextStatsSetOnDrillDown(t *testing.T) {
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
	if model.contextStats != nil {
		t.Error("expected contextStats=nil before drill-down")
	}

	// Press Enter to drill down into alice's messages
	newModel, _ := model.handleAggregateKeys(keyEnter())
	m := newModel.(Model)

	// Verify contextStats is set from selected row
	if m.contextStats == nil {
		t.Fatal("expected contextStats to be set after drill-down")
	}
	if m.contextStats.MessageCount != 100 {
		t.Errorf("expected MessageCount=100, got %d", m.contextStats.MessageCount)
	}
	if m.contextStats.TotalSize != 500000 {
		t.Errorf("expected TotalSize=500000, got %d", m.contextStats.TotalSize)
	}
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

	if m.contextStats == nil {
		t.Fatal("expected contextStats to be set after drill-down")
	}

	// Go back
	newModel2, _ := m.goBack()
	m2 := newModel2.(Model)

	// contextStats should be cleared
	if m2.contextStats != nil {
		t.Error("expected contextStats=nil after going back to aggregates")
	}
}

// TestContextStatsRestoredOnGoBackToSubAggregate verifies contextStats is restored when going back.
func TestContextStatsRestoredOnGoBackToSubAggregate(t *testing.T) {
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
	if m.contextStats == nil || m.contextStats.MessageCount != 100 {
		t.Fatalf("expected contextStats.MessageCount=100, got %v", m.contextStats)
	}

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
	if m2.contextStats != originalContextStats {
		t.Errorf("contextStats should be preserved after Tab")
	}

	// Step 3: Drill down from sub-aggregate to message list (contextStats overwritten)
	m3 := applyAggregateKey(t, m2, keyEnter())
	assertLevel(t, m3, levelMessageList)
	// contextStats should now be domain1's stats (60)
	if m3.contextStats == nil || m3.contextStats.MessageCount != 60 {
		t.Errorf("expected contextStats.MessageCount=60 for domain1, got %v", m3.contextStats)
	}

	// Step 4: Go back to sub-aggregate (contextStats should be restored to alice's stats)
	newModel4, _ := m3.goBack()
	m4 := newModel4.(Model)
	assertLevel(t, m4, levelDrillDown)
	// contextStats should be restored from breadcrumb
	if m4.contextStats == nil {
		t.Error("expected contextStats to be restored after goBack")
	} else if m4.contextStats.MessageCount != 100 {
		t.Errorf("expected contextStats.MessageCount=100 after goBack, got %d", m4.contextStats.MessageCount)
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
	if m.viewType != query.ViewRecipients {
		t.Errorf("expected ViewRecipients in sub-aggregate, got %v", m.viewType)
	}

	// Press Esc to go back to message list
	newModel2, _ := m.goBack()
	m2 := newModel2.(Model)

	assertLevel(t, m2, levelMessageList)
	// viewType should be restored to ViewSenders
	if m2.viewType != query.ViewSenders {
		t.Errorf("expected ViewSenders after going back, got %v", m2.viewType)
	}
}

// TestCursorScrollPreservedAfterGoBack verifies cursor and scroll are preserved
// when navigating back. With view caching, data is restored from cache instantly
// without requiring a reload.
func TestCursorScrollPreservedAfterGoBack(t *testing.T) {
	rows := makeRows(10)
	model := NewBuilder().WithRows(rows...).WithViewType(query.ViewSenders).Build()
	model.cursor = 5
	model.scrollOffset = 3

	// Drill down to message list (saves breadcrumb with cached rows)
	m, _ := sendKey(t, model, keyEnter())

	assertLevel(t, m, levelMessageList)

	// Verify breadcrumb was saved with cached rows
	if len(m.breadcrumbs) != 1 {
		t.Fatalf("expected 1 breadcrumb, got %d", len(m.breadcrumbs))
	}
	if m.breadcrumbs[0].state.rows == nil {
		t.Error("expected CachedRows to be set in breadcrumb")
	}

	// Go back to aggregates - with caching, this restores instantly without reload
	newModel2, cmd := m.goBack()
	m2 := newModel2.(Model)

	// With caching, no reload command is returned
	if cmd != nil {
		t.Error("expected nil command when restoring from cache")
	}

	// Loading should be false (no async reload needed)
	if m2.loading {
		t.Error("expected loading=false when restoring from cache")
	}

	// Cursor and scroll should be preserved from breadcrumb
	if m2.cursor != 5 {
		t.Errorf("expected cursor=5, got %d", m2.cursor)
	}
	if m2.scrollOffset != 3 {
		t.Errorf("expected scrollOffset=3, got %d", m2.scrollOffset)
	}

	// Rows should be restored from cache
	if len(m2.rows) != 10 {
		t.Errorf("expected 10 rows, got %d", len(m2.rows))
	}
}

// TestGoBackClearsError verifies that goBack clears any stale error.
func TestGoBackClearsError(t *testing.T) {
	model := NewBuilder().WithLevel(levelMessageList).Build()
	model.err = fmt.Errorf("some previous error")
	model.breadcrumbs = []navigationSnapshot{{state: viewState{
		level:    levelAggregates,
		viewType: query.ViewSenders,
	}}}

	// Go back
	newModel, _ := model.goBack()
	m := newModel.(Model)

	// Error should be cleared
	if m.err != nil {
		t.Errorf("expected err=nil after goBack, got %v", m.err)
	}
}

// TestDrillFilterPreservedAfterMessageDetail verifies drillFilter is preserved
// when navigating back from message detail to message list.
func TestDrillFilterPreservedAfterMessageDetail(t *testing.T) {
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
	if len(m.breadcrumbs) == 0 {
		t.Fatal("expected breadcrumb to be saved")
	}
	bc := m.breadcrumbs[len(m.breadcrumbs)-1]
	if bc.state.drillFilter.Sender != "alice@example.com" {
		t.Errorf("expected breadcrumb DrillFilter.Sender='alice@example.com', got %q", bc.state.drillFilter.Sender)
	}
	if bc.state.drillFilter.Recipient != "bob@example.com" {
		t.Errorf("expected breadcrumb DrillFilter.Recipient='bob@example.com', got %q", bc.state.drillFilter.Recipient)
	}
	if bc.state.drillViewType != query.ViewSenders {
		t.Errorf("expected breadcrumb DrillViewType=ViewSenders, got %v", bc.state.drillViewType)
	}

	// Press Esc to go back to message list
	newModel2, _ := m.goBack()
	m2 := newModel2.(Model)

	assertLevel(t, m2, levelMessageList)

	// drillFilter should be restored
	if m2.drillFilter.Sender != "alice@example.com" {
		t.Errorf("expected drillFilter.Sender='alice@example.com', got %q", m2.drillFilter.Sender)
	}
	if m2.drillFilter.Recipient != "bob@example.com" {
		t.Errorf("expected drillFilter.Recipient='bob@example.com', got %q", m2.drillFilter.Recipient)
	}
	if m2.drillViewType != query.ViewSenders {
		t.Errorf("expected drillViewType=ViewSenders, got %v", m2.drillViewType)
	}
	if m2.viewType != query.ViewRecipients {
		t.Errorf("expected viewType=ViewRecipients, got %v", m2.viewType)
	}
}

// =============================================================================
// Breadcrumb Tests
// =============================================================================

func TestPushBreadcrumb(t *testing.T) {
	m := NewBuilder().Build()

	if len(m.breadcrumbs) != 0 {
		t.Fatal("expected no breadcrumbs initially")
	}

	m.pushBreadcrumb()
	if len(m.breadcrumbs) != 1 {
		t.Errorf("expected 1 breadcrumb, got %d", len(m.breadcrumbs))
	}

	m.pushBreadcrumb()
	if len(m.breadcrumbs) != 2 {
		t.Errorf("expected 2 breadcrumbs, got %d", len(m.breadcrumbs))
	}
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
	if len(m3.selection.aggregateKeys) == 0 {
		t.Error("sub-aggregate Enter should not clear aggregate selection")
	}
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
	if len(m.selection.aggregateKeys) != 0 {
		t.Errorf("top-level Enter should clear aggregate selection, got %v", m.selection.aggregateKeys)
	}
	if len(m.selection.messageIDs) != 0 {
		t.Errorf("top-level Enter should clear message selection, got %v", m.selection.messageIDs)
	}
}

// =============================================================================
// Sub-Aggregate 'a' Key Tests
// =============================================================================

// TestSubAggregateAKeyJumpsToMessages verifies 'a' key in sub-aggregate view
// jumps to message list with the drill filter applied.
func TestSubAggregateAKeyJumpsToMessages(t *testing.T) {
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
	m := newModel.(Model)

	// Should navigate to message list
	assertLevel(t, m, levelMessageList)

	// Should have a command to load messages
	if cmd == nil {
		t.Error("expected command to load messages")
	}

	// Should preserve drill filter
	if m.drillFilter.Sender != "alice@example.com" {
		t.Errorf("expected drillFilter.Sender = alice@example.com, got %s", m.drillFilter.Sender)
	}

	// Should have saved breadcrumb
	if len(m.breadcrumbs) != 1 {
		t.Errorf("expected 1 breadcrumb, got %d", len(m.breadcrumbs))
	}

	// Breadcrumb should be for sub-aggregate level
	if m.breadcrumbs[0].state.level != levelDrillDown {
		t.Errorf("expected breadcrumb level = levelDrillDown, got %v", m.breadcrumbs[0].state.level)
	}
}
