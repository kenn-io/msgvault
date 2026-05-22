package tui

import (
	"testing"

	"go.kenn.io/msgvault/internal/query"
)

// =============================================================================
// View Type Cycling Tests ('g' and Tab keys)
// =============================================================================

// TestGKeyCyclesViewType verifies that 'g' cycles through view types at aggregate level.
func TestGKeyCyclesViewType(t *testing.T) {
	model := NewBuilder().
		WithRows(query.AggregateRow{Key: "alice@example.com", Count: 10}).
		WithPageSize(10).WithSize(100, 20).
		WithViewType(query.ViewSenders).Build()
	// Set non-zero cursor/scroll to verify reset
	model.cursor = 5
	model.scrollOffset = 3

	// Press 'g' - should cycle to SenderNames (not go to home)
	newModel, cmd := model.handleAggregateKeys(key('g'))
	m := newModel.(Model)

	// Expected: viewType changes to ViewSenderNames
	if m.viewType != query.ViewSenderNames {
		t.Errorf("expected ViewSenderNames after 'g', got %v", m.viewType)
	}
	// Should trigger data reload
	if cmd == nil {
		t.Error("expected reload command after view type change")
	}
	if !m.loading {
		t.Error("expected loading=true after view type change")
	}
	// Cursor and scroll should reset to 0 when view type changes
	if m.cursor != 0 {
		t.Errorf("expected cursor=0 after view type change, got %d", m.cursor)
	}
	if m.scrollOffset != 0 {
		t.Errorf("expected scrollOffset=0 after view type change, got %d", m.scrollOffset)
	}
}

// TestGKeyCyclesViewTypeFullCycle verifies 'g' cycles through all view types.
func TestGKeyCyclesViewTypeFullCycle(t *testing.T) {
	model := NewBuilder().
		WithRows(query.AggregateRow{Key: "test", Count: 10}).
		WithPageSize(10).WithSize(100, 20).
		WithViewType(query.ViewSenders).Build()

	expectedOrder := []query.ViewType{
		query.ViewSenderNames,
		query.ViewRecipients,
		query.ViewRecipientNames,
		query.ViewDomains,
		query.ViewLabels,
		query.ViewTime,
		query.ViewSenders, // Cycles back
	}

	for i, expected := range expectedOrder {
		model = applyAggregateKey(t, model, key('g'))
		model.loading = false // Reset for next iteration

		if model.viewType != expected {
			t.Errorf("cycle %d: expected %v, got %v", i+1, expected, model.viewType)
		}
	}
}

// TestGKeyInSubAggregate verifies 'g' cycles view types in sub-aggregate view.
func TestGKeyInSubAggregate(t *testing.T) {
	model := NewBuilder().
		WithRows(query.AggregateRow{Key: "bob@example.com", Count: 5}).
		WithPageSize(10).WithSize(100, 20).
		WithLevel(levelDrillDown).WithViewType(query.ViewRecipients).
		Build()
	model.drillViewType = query.ViewSenders // Drilled from Senders
	model.drillFilter = query.MessageFilter{Sender: "alice@example.com"}

	// Press 'g' - should cycle to next view type, skipping drillViewType
	m := applyAggregateKey(t, model, key('g'))

	// Should skip ViewSenders (the drillViewType) and go to RecipientNames
	if m.viewType != query.ViewRecipientNames {
		t.Errorf("expected ViewRecipientNames (skipping drillViewType), got %v", m.viewType)
	}
}

// TestGKeyInMessageListWithDrillFilter verifies 'g' switches to sub-aggregate view
// when there's a drill filter.
func TestGKeyInMessageListWithDrillFilter(t *testing.T) {
	model := NewBuilder().
		WithMessages(
			query.MessageSummary{ID: 1, Subject: "Test 1"},
			query.MessageSummary{ID: 2, Subject: "Test 2"},
			query.MessageSummary{ID: 3, Subject: "Test 3"},
		).
		WithPageSize(10).WithSize(100, 20).
		WithLevel(levelMessageList).WithViewType(query.ViewSenders).
		Build()
	model.cursor = 2 // Start at third item
	model.scrollOffset = 1
	// Set up a drill filter so 'g' triggers sub-grouping
	model.drillFilter = query.MessageFilter{Sender: "alice@example.com"}
	model.drillViewType = query.ViewSenders

	// Press 'g' - should switch to sub-aggregate view
	m := applyMessageListKey(t, model, key('g'))

	assertLevel(t, m, levelDrillDown)
	// ViewType should be next logical view (Recipients after Senders, skipping SenderNames)
	if m.viewType != query.ViewRecipients {
		t.Errorf("expected viewType=Recipients after 'g', got %v", m.viewType)
	}
}

// TestGKeyInMessageListNoDrillFilter verifies 'g' goes back to aggregates when no drill filter.
func TestGKeyInMessageListNoDrillFilter(t *testing.T) {
	model := NewBuilder().
		WithMessages(
			query.MessageSummary{ID: 1, Subject: "Test 1"},
			query.MessageSummary{ID: 2, Subject: "Test 2"},
			query.MessageSummary{ID: 3, Subject: "Test 3"},
		).
		WithPageSize(10).WithSize(100, 20).
		WithLevel(levelMessageList).Build()
	model.cursor = 2 // Start at third item
	model.scrollOffset = 1
	// No drill filter - 'g' should go back to aggregates

	// Press 'g' - should go back to aggregate view
	m := applyMessageListKey(t, model, key('g'))

	// Should transition to aggregate level
	assertLevel(t, m, levelAggregates)
	// Cursor and scroll should reset
	if m.cursor != 0 {
		t.Errorf("expected cursor=0 after 'g' with no drill filter, got %d", m.cursor)
	}
	if m.scrollOffset != 0 {
		t.Errorf("expected scrollOffset=0 after 'g' with no drill filter, got %d", m.scrollOffset)
	}
}

// TestTabCyclesViewTypeAtAggregates verifies Tab still cycles view types.
func TestTabCyclesViewTypeAtAggregates(t *testing.T) {
	model := NewBuilder().
		WithRows(query.AggregateRow{Key: "test", Count: 10}).
		WithPageSize(10).WithSize(100, 20).
		WithViewType(query.ViewSenders).Build()
	// Set non-zero cursor/scroll to verify reset
	model.cursor = 5
	model.scrollOffset = 3

	// Press Tab - should cycle to SenderNames
	newModel, cmd := model.handleAggregateKeys(keyTab())
	m := newModel.(Model)

	if m.viewType != query.ViewSenderNames {
		t.Errorf("expected ViewSenderNames after Tab, got %v", m.viewType)
	}
	if cmd == nil {
		t.Error("expected reload command after Tab")
	}
	// Cursor and scroll should reset to 0 when view type changes
	if m.cursor != 0 {
		t.Errorf("expected cursor=0 after Tab, got %d", m.cursor)
	}
	if m.scrollOffset != 0 {
		t.Errorf("expected scrollOffset=0 after Tab, got %d", m.scrollOffset)
	}
}

// TestHomeKeyGoesToTop verifies 'home' key goes to top (separate from 'g').
func TestHomeKeyGoesToTop(t *testing.T) {
	model := NewBuilder().
		WithRows(
			query.AggregateRow{Key: "a", Count: 1},
			query.AggregateRow{Key: "b", Count: 2},
			query.AggregateRow{Key: "c", Count: 3},
		).
		WithPageSize(10).WithSize(100, 20).Build()
	model.cursor = 2
	model.scrollOffset = 1

	// Press 'home' - should go to top
	m := applyAggregateKey(t, model, keyHome())

	if m.cursor != 0 {
		t.Errorf("expected cursor=0 after 'home', got %d", m.cursor)
	}
	if m.scrollOffset != 0 {
		t.Errorf("expected scrollOffset=0 after 'home', got %d", m.scrollOffset)
	}
}

// =============================================================================
// Time View and 't' Key Tests
// =============================================================================

func TestTKeyJumpsToTimeView(t *testing.T) {
	model := NewBuilder().
		WithRows(query.AggregateRow{Key: "test", Count: 10}).
		WithPageSize(10).WithSize(100, 20).
		WithViewType(query.ViewSenders).Build()

	// Press 't' from Senders view - should jump to Time
	m := applyAggregateKey(t, model, key('t'))
	if m.viewType != query.ViewTime {
		t.Errorf("expected ViewTime after 't' from Senders, got %v", m.viewType)
	}
	if !m.loading {
		t.Error("expected loading=true after 't' key")
	}
}

func TestTKeyJumpsToTimeFromAnyView(t *testing.T) {
	views := []query.ViewType{
		query.ViewSenders,
		query.ViewSenderNames,
		query.ViewRecipients,
		query.ViewRecipientNames,
		query.ViewDomains,
		query.ViewLabels,
	}

	for _, vt := range views {
		model := NewBuilder().
			WithRows(query.AggregateRow{Key: "test", Count: 10}).
			WithPageSize(10).WithSize(100, 20).
			WithViewType(vt).Build()

		m := applyAggregateKey(t, model, key('t'))
		if m.viewType != query.ViewTime {
			t.Errorf("from %v: expected ViewTime after 't', got %v", vt, m.viewType)
		}
	}
}

func TestTKeyCyclesGranularityInTimeView(t *testing.T) {
	model := NewBuilder().
		WithRows(query.AggregateRow{Key: "2024-01", Count: 10}).
		WithPageSize(10).WithSize(100, 20).
		WithViewType(query.ViewTime).Build()
	model.timeGranularity = query.TimeYear

	// Press 't' in Time view - should cycle granularity
	m := applyAggregateKey(t, model, key('t'))
	if m.viewType != query.ViewTime {
		t.Errorf("expected to stay in ViewTime, got %v", m.viewType)
	}
	if m.timeGranularity != query.TimeMonth {
		t.Errorf("expected TimeMonth after cycling from TimeYear, got %v", m.timeGranularity)
	}
}

func TestTKeyResetsSelectionOnJump(t *testing.T) {
	model := NewBuilder().
		WithRows(query.AggregateRow{Key: "test", Count: 10}).
		WithPageSize(10).WithSize(100, 20).
		WithViewType(query.ViewSenders).Build()
	model.selection.aggregateKeys["test"] = true
	model.cursor = 5
	model.scrollOffset = 3

	m := applyAggregateKey(t, model, key('t'))
	if len(m.selection.aggregateKeys) != 0 {
		t.Error("expected selection cleared after 't' jump")
	}
	if m.cursor != 0 {
		t.Errorf("expected cursor=0 after 't' jump, got %d", m.cursor)
	}
	if m.scrollOffset != 0 {
		t.Errorf("expected scrollOffset=0 after 't' jump, got %d", m.scrollOffset)
	}
}

func TestTKeyDoesNotResetSelectionOnCycle(t *testing.T) {
	model := NewBuilder().
		WithRows(query.AggregateRow{Key: "2024", Count: 10}, query.AggregateRow{Key: "2023", Count: 5}).
		WithPageSize(10).WithSize(100, 20).
		WithViewType(query.ViewTime).Build()
	model.timeGranularity = query.TimeYear
	model.selection.aggregateKeys["2024"] = true
	model.cursor = 1
	model.scrollOffset = 0

	// When already in Time view, 't' cycles granularity but preserves selection/cursor
	m := applyAggregateKey(t, model, key('t'))
	if m.viewType != query.ViewTime {
		t.Errorf("expected ViewTime, got %v", m.viewType)
	}
	if m.timeGranularity != query.TimeMonth {
		t.Errorf("expected TimeMonth, got %v", m.timeGranularity)
	}
	if !m.selection.aggregateKeys["2024"] {
		t.Error("expected selection preserved after 't' granularity cycle")
	}
	if m.cursor != 1 {
		t.Errorf("expected cursor=1 preserved, got %d", m.cursor)
	}
}

func TestTKeyNoOpInSubAggregateWhenDrillIsTime(t *testing.T) {
	model := NewBuilder().
		WithRows(query.AggregateRow{Key: "alice@example.com", Count: 10}).
		WithPageSize(10).WithSize(100, 20).
		WithLevel(levelDrillDown).WithViewType(query.ViewSenders).Build()
	model.drillViewType = query.ViewTime
	model.drillFilter = query.MessageFilter{TimeRange: query.TimeRange{Period: "2024"}}

	// Press 't' in sub-aggregate where drill was from Time — should be a no-op
	m := applyAggregateKey(t, model, key('t'))
	if m.viewType != query.ViewSenders {
		t.Errorf("expected viewType unchanged (ViewSenders), got %v", m.viewType)
	}
	if m.loading {
		t.Error("expected loading=false (no-op)")
	}
}

// TestTKeyInMessageListJumpsToTimeSubGroup verifies that pressing 't' in a
// drilled-down message list enters sub-grouping with ViewTime.
func TestTKeyInMessageListJumpsToTimeSubGroup(t *testing.T) {
	model := NewBuilder().
		WithMessages(
			query.MessageSummary{ID: 1, Subject: "Test 1"},
			query.MessageSummary{ID: 2, Subject: "Test 2"},
		).
		WithPageSize(10).WithSize(100, 20).
		WithLevel(levelMessageList).WithViewType(query.ViewSenders).
		Build()
	model.drillFilter = query.MessageFilter{Sender: "alice@example.com"}
	model.drillViewType = query.ViewSenders

	m := applyMessageListKey(t, model, key('t'))

	assertLevel(t, m, levelDrillDown)
	if m.viewType != query.ViewTime {
		t.Errorf("expected viewType=ViewTime after 't', got %v", m.viewType)
	}
}

// TestTKeyInMessageListFromTimeDrillIsNoop verifies that pressing 't' when
// the drill dimension is already Time is a no-op (avoids redundant sub-aggregate).
func TestTKeyInMessageListFromTimeDrillIsNoop(t *testing.T) {
	model := NewBuilder().
		WithMessages(
			query.MessageSummary{ID: 1, Subject: "Test 1"},
		).
		WithPageSize(10).WithSize(100, 20).
		WithLevel(levelMessageList).WithViewType(query.ViewTime).
		Build()
	model.drillFilter = query.MessageFilter{TimeRange: query.TimeRange{Period: "2024-01"}}
	model.drillViewType = query.ViewTime

	m := applyMessageListKey(t, model, key('t'))

	assertLevel(t, m, levelMessageList)
	if m.loading {
		t.Error("expected loading=false (no-op)")
	}
}

// TestTKeyInMessageListNoDrillFilterIsNoop verifies that 't' does nothing
// in message list without a drill filter.
func TestTKeyInMessageListNoDrillFilterIsNoop(t *testing.T) {
	model := NewBuilder().
		WithMessages(
			query.MessageSummary{ID: 1, Subject: "Test 1"},
		).
		WithPageSize(10).WithSize(100, 20).
		WithLevel(levelMessageList).Build()

	m := applyMessageListKey(t, model, key('t'))

	assertLevel(t, m, levelMessageList)
}

// =============================================================================
// Sub-Group View Skipping Tests
// =============================================================================

// TestNextSubGroupViewSkipsSenderNames verifies that drilling from Senders
// skips SenderNames (redundant) and goes straight to Recipients.
func TestNextSubGroupViewSkipsSenderNames(t *testing.T) {
	model := NewBuilder().
		WithMessages(
			query.MessageSummary{ID: 1, Subject: "Test 1"},
		).
		WithPageSize(10).WithSize(100, 20).
		WithLevel(levelMessageList).WithViewType(query.ViewSenders).
		Build()
	model.drillFilter = query.MessageFilter{Sender: "alice@example.com"}
	model.drillViewType = query.ViewSenders

	m := applyMessageListKey(t, model, key('g'))

	if m.viewType != query.ViewRecipients {
		t.Errorf("expected sub-group from Senders to be Recipients (skip SenderNames), got %v", m.viewType)
	}
}

// TestNextSubGroupViewSkipsRecipientNames verifies that drilling from Recipients
// skips RecipientNames (redundant) and goes straight to Domains.
func TestNextSubGroupViewSkipsRecipientNames(t *testing.T) {
	model := NewBuilder().
		WithMessages(
			query.MessageSummary{ID: 1, Subject: "Test 1"},
		).
		WithPageSize(10).WithSize(100, 20).
		WithLevel(levelMessageList).WithViewType(query.ViewRecipients).
		Build()
	model.drillFilter = query.MessageFilter{Recipient: "bob@example.com"}
	model.drillViewType = query.ViewRecipients

	m := applyMessageListKey(t, model, key('g'))

	if m.viewType != query.ViewDomains {
		t.Errorf("expected sub-group from Recipients to be Domains (skip RecipientNames), got %v", m.viewType)
	}
}

// TestNextSubGroupViewFromSenderNamesKeepsRecipients verifies that drilling from
// SenderNames goes to Recipients (name→email sub-grouping is useful).
func TestNextSubGroupViewFromSenderNamesKeepsRecipients(t *testing.T) {
	model := NewBuilder().
		WithMessages(
			query.MessageSummary{ID: 1, Subject: "Test 1"},
		).
		WithPageSize(10).WithSize(100, 20).
		WithLevel(levelMessageList).WithViewType(query.ViewSenderNames).
		Build()
	model.drillFilter = query.MessageFilter{SenderName: "Alice"}
	model.drillViewType = query.ViewSenderNames

	m := applyMessageListKey(t, model, key('g'))

	if m.viewType != query.ViewRecipients {
		t.Errorf("expected sub-group from SenderNames to be Recipients, got %v", m.viewType)
	}
}

// TestNextSubGroupViewFromRecipientNamesKeepsDomains verifies that drilling from
// RecipientNames goes to Domains.
func TestNextSubGroupViewFromRecipientNamesKeepsDomains(t *testing.T) {
	model := NewBuilder().
		WithMessages(
			query.MessageSummary{ID: 1, Subject: "Test 1"},
		).
		WithPageSize(10).WithSize(100, 20).
		WithLevel(levelMessageList).WithViewType(query.ViewRecipientNames).
		Build()
	model.drillFilter = query.MessageFilter{RecipientName: "Bob"}
	model.drillViewType = query.ViewRecipientNames

	m := applyMessageListKey(t, model, key('g'))

	if m.viewType != query.ViewDomains {
		t.Errorf("expected sub-group from RecipientNames to be Domains, got %v", m.viewType)
	}
}

// TestNextSubGroupViewFromDomainsGoesToLabels verifies the standard chain continues.
func TestNextSubGroupViewFromDomainsGoesToLabels(t *testing.T) {
	model := NewBuilder().
		WithMessages(
			query.MessageSummary{ID: 1, Subject: "Test 1"},
		).
		WithPageSize(10).WithSize(100, 20).
		WithLevel(levelMessageList).WithViewType(query.ViewDomains).
		Build()
	model.drillFilter = query.MessageFilter{Domain: "example.com"}
	model.drillViewType = query.ViewDomains

	m := applyMessageListKey(t, model, key('g'))

	if m.viewType != query.ViewLabels {
		t.Errorf("expected sub-group from Domains to be Labels, got %v", m.viewType)
	}
}

// =============================================================================
// Time Granularity Drill-Down Tests
// =============================================================================

func TestTopLevelTimeDrillDown_AllGranularities(t *testing.T) {
	// Test that top-level drill-down from Time view correctly sets both
	// TimePeriod and TimeGranularity on the drillFilter.
	tests := []struct {
		name        string
		granularity query.TimeGranularity
		key         string
	}{
		{"Year", query.TimeYear, "2024"},
		{"Month", query.TimeMonth, "2024-06"},
		{"Day", query.TimeDay, "2024-06-15"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			model := NewBuilder().
				WithRows(query.AggregateRow{Key: tt.key, Count: 87, TotalSize: 500000}).
				WithViewType(query.ViewTime).
				Build()

			model.timeGranularity = tt.granularity
			model.cursor = 0

			m := applyAggregateKey(t, model, keyEnter())

			assertState(t, m, levelMessageList, query.ViewTime, 0)

			if m.drillFilter.TimeRange.Period != tt.key {
				t.Errorf("drillFilter.TimePeriod = %q, want %q", m.drillFilter.TimeRange.Period, tt.key)
			}
			if m.drillFilter.TimeRange.Granularity != tt.granularity {
				t.Errorf("drillFilter.TimeGranularity = %v, want %v", m.drillFilter.TimeRange.Granularity, tt.granularity)
			}
		})
	}
}

func TestSubAggregateTimeDrillDown_AllGranularities(t *testing.T) {
	// Regression test: drilling down from sub-aggregate Time view must set
	// TimeGranularity on the drillFilter to match the current view granularity,
	// not the stale value from the original top-level drill.
	tests := []struct {
		name               string
		initialGranularity query.TimeGranularity // Set when top-level drill was created
		subGranularity     query.TimeGranularity // Changed in sub-aggregate view
		key                string
	}{
		{"Month_to_Year", query.TimeMonth, query.TimeYear, "2024"},
		{"Year_to_Month", query.TimeYear, query.TimeMonth, "2024-06"},
		{"Year_to_Day", query.TimeYear, query.TimeDay, "2024-06-15"},
		{"Day_to_Year", query.TimeDay, query.TimeYear, "2023"},
		{"Day_to_Month", query.TimeDay, query.TimeMonth, "2023-12"},
		{"Month_to_Day", query.TimeMonth, query.TimeDay, "2024-01-15"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Start with a model already in sub-aggregate Time view
			// (simulating: top-level sender drill → sub-group by time)
			model := NewBuilder().
				WithRows(query.AggregateRow{Key: tt.key, Count: 87, TotalSize: 500000}).
				WithLevel(levelDrillDown).
				WithViewType(query.ViewTime).
				Build()

			// drillFilter was created during top-level drill with the initial granularity
			model.drillFilter = query.MessageFilter{
				Sender:    "alice@example.com",
				TimeRange: query.TimeRange{Granularity: tt.initialGranularity},
			}
			model.drillViewType = query.ViewSenders
			// User changed granularity in the sub-aggregate view
			model.timeGranularity = tt.subGranularity
			model.cursor = 0

			m := applyAggregateKey(t, model, keyEnter())

			assertLevel(t, m, levelMessageList)

			if m.drillFilter.TimeRange.Period != tt.key {
				t.Errorf("drillFilter.TimePeriod = %q, want %q", m.drillFilter.TimeRange.Period, tt.key)
			}
			if m.drillFilter.TimeRange.Granularity != tt.subGranularity {
				t.Errorf("drillFilter.TimeGranularity = %v, want %v (should match sub-agg granularity, not initial %v)",
					m.drillFilter.TimeRange.Granularity, tt.subGranularity, tt.initialGranularity)
			}
			// Sender filter from original drill should be preserved
			if m.drillFilter.Sender != "alice@example.com" {
				t.Errorf("drillFilter.Sender = %q, want %q (should preserve parent drill filter)",
					m.drillFilter.Sender, "alice@example.com")
			}
		})
	}
}

func TestSubAggregateTimeDrillDown_NonTimeViewPreservesGranularity(t *testing.T) {
	// When sub-aggregate view is NOT Time (e.g., Labels), drilling down should
	// NOT change the drillFilter's TimeGranularity (it may have been set by
	// a previous time drill).
	model := NewBuilder().
		WithRows(query.AggregateRow{Key: "INBOX", Count: 50, TotalSize: 100000}).
		WithLevel(levelDrillDown).
		WithViewType(query.ViewLabels).
		Build()

	model.drillFilter = query.MessageFilter{
		Sender:    "alice@example.com",
		TimeRange: query.TimeRange{Period: "2024", Granularity: query.TimeYear},
	}
	model.drillViewType = query.ViewSenders
	model.timeGranularity = query.TimeMonth // Different from drillFilter
	model.cursor = 0

	m := applyAggregateKey(t, model, keyEnter())

	assertLevel(t, m, levelMessageList)

	// TimeGranularity should be unchanged (we drilled by Label, not Time)
	if m.drillFilter.TimeRange.Granularity != query.TimeYear {
		t.Errorf("drillFilter.TimeGranularity = %v, want TimeYear (non-time drill should not change it)",
			m.drillFilter.TimeRange.Granularity)
	}
	if m.drillFilter.Label != "INBOX" {
		t.Errorf("drillFilter.Label = %q, want %q", m.drillFilter.Label, "INBOX")
	}
}

func TestTopLevelTimeDrillDown_GranularityChangedBeforeEnter(t *testing.T) {
	// User starts in Time view with Month, changes to Year, then presses Enter.
	// drillFilter should use the CURRENT granularity (Year), not the initial one.
	model := NewBuilder().
		WithRows(query.AggregateRow{Key: "2024", Count: 200, TotalSize: 1000000}).
		WithViewType(query.ViewTime).
		Build()

	// Default is TimeMonth, user toggles to TimeYear
	model.timeGranularity = query.TimeYear
	model.cursor = 0

	m := applyAggregateKey(t, model, keyEnter())

	assertLevel(t, m, levelMessageList)
	if m.drillFilter.TimeRange.Granularity != query.TimeYear {
		t.Errorf("drillFilter.TimeGranularity = %v, want TimeYear", m.drillFilter.TimeRange.Granularity)
	}
	if m.drillFilter.TimeRange.Period != "2024" {
		t.Errorf("drillFilter.TimePeriod = %q, want %q", m.drillFilter.TimeRange.Period, "2024")
	}
}

func TestSubAggregateTimeDrillDown_FullScenario(t *testing.T) {
	// Full user scenario: search sender → drill → sub-group by time → toggle Year → Enter
	// This is the exact bug report scenario.
	model := NewBuilder().
		WithRows(
			query.AggregateRow{Key: "alice@example.com", Count: 200, TotalSize: 1000000},
		).
		WithViewType(query.ViewSenders).
		Build()

	// Step 1: Drill into alice (top-level, creates drillFilter with TimeMonth default)
	model.timeGranularity = query.TimeMonth // default
	model.cursor = 0
	step1 := applyAggregateKey(t, model, keyEnter())
	assertLevel(t, step1, levelMessageList)

	if step1.drillFilter.TimeRange.Granularity != query.TimeMonth {
		t.Fatalf("after top-level drill, TimeGranularity = %v, want TimeMonth", step1.drillFilter.TimeRange.Granularity)
	}

	// Step 2: Tab to sub-aggregate view
	step1.rows = nil
	step1.loading = false
	step2 := applyMessageListKey(t, step1, keyTab())
	assertLevel(t, step2, levelDrillDown)

	// Simulate sub-agg data loaded, switch to Time view, toggle to Year
	step2.rows = []query.AggregateRow{
		{Key: "2024", Count: 87, TotalSize: 400000},
		{Key: "2023", Count: 113, TotalSize: 600000},
	}
	step2.loading = false
	step2.viewType = query.ViewTime
	step2.timeGranularity = query.TimeYear // User toggled granularity

	// Step 3: Enter on "2024" — this was the bug
	step2.cursor = 0
	step3 := applyAggregateKey(t, step2, keyEnter())

	assertLevel(t, step3, levelMessageList)

	// KEY ASSERTION: TimeGranularity must match the sub-agg view (Year), not the
	// stale value from the top-level drill (Month). Otherwise the query generates
	// a month-format expression compared against "2024", returning zero rows.
	if step3.drillFilter.TimeRange.Granularity != query.TimeYear {
		t.Errorf("drillFilter.TimeGranularity = %v, want TimeYear (was stale TimeMonth from top-level drill)",
			step3.drillFilter.TimeRange.Granularity)
	}
	if step3.drillFilter.TimeRange.Period != "2024" {
		t.Errorf("drillFilter.TimePeriod = %q, want %q", step3.drillFilter.TimeRange.Period, "2024")
	}
	// Original sender filter should be preserved
	if step3.drillFilter.Sender != "alice@example.com" {
		t.Errorf("drillFilter.Sender = %q, want %q", step3.drillFilter.Sender, "alice@example.com")
	}
}

// =============================================================================
// Sender Names View Tests
// =============================================================================

// TestSenderNamesDrillDown verifies that pressing Enter on a SenderNames row
// sets drillFilter.SenderName and transitions to message list.
func TestSenderNamesDrillDown(t *testing.T) {
	rows := []query.AggregateRow{
		{Key: "Alice Smith", Count: 10},
		{Key: "Bob Jones", Count: 5},
	}

	model := NewBuilder().WithRows(rows...).
		WithPageSize(10).WithSize(100, 20).WithViewType(query.ViewSenderNames).Build()

	// Press Enter to drill into first sender name
	newModel, cmd := model.handleAggregateKeys(keyEnter())
	m := newModel.(Model)

	assertLevel(t, m, levelMessageList)
	if m.drillFilter.SenderName != "Alice Smith" {
		t.Errorf("expected drillFilter.SenderName='Alice Smith', got %q", m.drillFilter.SenderName)
	}
	if m.drillViewType != query.ViewSenderNames {
		t.Errorf("expected drillViewType=ViewSenderNames, got %v", m.drillViewType)
	}
	if cmd == nil {
		t.Error("expected command to load messages")
	}
	if len(m.breadcrumbs) != 1 {
		t.Errorf("expected 1 breadcrumb, got %d", len(m.breadcrumbs))
	}
}

// TestSenderNamesDrillDownEmptyKey verifies drilling into an empty sender name
// sets MatchEmptySenderName.
func TestSenderNamesDrillDownEmptyKey(t *testing.T) {
	rows := []query.AggregateRow{
		{Key: "", Count: 3},
	}

	model := NewBuilder().WithRows(rows...).
		WithPageSize(10).WithSize(100, 20).WithViewType(query.ViewSenderNames).Build()

	newModel, _ := model.handleAggregateKeys(keyEnter())
	m := newModel.(Model)

	if !m.drillFilter.MatchesEmpty(query.ViewSenderNames) {
		t.Error("expected MatchEmptySenderName=true for empty key")
	}
	if m.drillFilter.SenderName != "" {
		t.Errorf("expected empty SenderName, got %q", m.drillFilter.SenderName)
	}
}

// TestSenderNamesDrillFilterKey verifies drillFilterKey returns the SenderName.
func TestSenderNamesDrillFilterKey(t *testing.T) {
	model := NewBuilder().
		WithRows(query.AggregateRow{Key: "test", Count: 1}).
		WithPageSize(10).WithSize(100, 20).Build()
	model.drillViewType = query.ViewSenderNames
	model.drillFilter = query.MessageFilter{SenderName: "John Doe"}

	key := model.drillFilterKey()
	if key != "John Doe" {
		t.Errorf("expected drillFilterKey='John Doe', got %q", key)
	}

	// Test empty case
	model.drillFilter = query.MessageFilter{EmptyValueTargets: map[query.ViewType]bool{query.ViewSenderNames: true}}
	key = model.drillFilterKey()
	if key != "(empty)" {
		t.Errorf("expected '(empty)' for MatchEmptySenderName, got %q", key)
	}
}

// TestSenderNamesBreadcrumbPrefix verifies the "N:" prefix in breadcrumbs.
func TestSenderNamesBreadcrumbPrefix(t *testing.T) {
	prefix := viewTypePrefix(query.ViewSenderNames)
	if prefix != "N" {
		t.Errorf("expected prefix 'N', got %q", prefix)
	}

	abbrev := viewTypeAbbrev(query.ViewSenderNames)
	if abbrev != "Sender Name" {
		t.Errorf("expected abbrev 'Sender Name', got %q", abbrev)
	}
}

// TestShiftTabCyclesSenderNames verifies shift+tab cycles backward through
// SenderNames in the correct order.
func TestShiftTabCyclesSenderNames(t *testing.T) {
	model := NewBuilder().
		WithRows(query.AggregateRow{Key: "test", Count: 1}).
		WithPageSize(10).WithSize(100, 20).
		WithViewType(query.ViewSenderNames).Build()

	// Shift+tab from SenderNames should go back to Senders
	m := applyAggregateKey(t, model, keyShiftTab())
	if m.viewType != query.ViewSenders {
		t.Errorf("expected ViewSenders after shift+tab from SenderNames, got %v", m.viewType)
	}
}

// TestSubAggregateFromSenderNames verifies that drilling from SenderNames
// and then tabbing skips SenderNames in the sub-aggregate cycle.
func TestSubAggregateFromSenderNames(t *testing.T) {
	rows := []query.AggregateRow{
		{Key: "Alice Smith", Count: 10},
	}
	msgs := []query.MessageSummary{
		{ID: 1, Subject: "Test"},
	}

	model := NewBuilder().WithRows(rows...).WithMessages(msgs...).
		WithPageSize(10).WithSize(100, 20).WithViewType(query.ViewSenderNames).Build()

	// Drill into the name
	newModel, _ := model.handleAggregateKeys(keyEnter())
	m := newModel.(Model)

	// Tab to sub-aggregate
	m.messages = msgs
	newModel2, _ := m.handleMessageListKeys(keyTab())
	m2 := newModel2.(Model)

	assertLevel(t, m2, levelDrillDown)
	// Should skip SenderNames (the drill view type) and go to Recipients
	if m2.viewType != query.ViewRecipients {
		t.Errorf("expected ViewRecipients (skipping SenderNames), got %v", m2.viewType)
	}
}

// TestHasDrillFilterWithSenderName verifies hasDrillFilter returns true
// for SenderName and MatchEmptySenderName.
func TestHasDrillFilterWithSenderName(t *testing.T) {
	model := NewBuilder().
		WithRows(query.AggregateRow{Key: "test", Count: 1}).
		WithPageSize(10).WithSize(100, 20).Build()

	model.drillFilter = query.MessageFilter{SenderName: "John"}
	if !model.hasDrillFilter() {
		t.Error("expected hasDrillFilter=true for SenderName")
	}

	model.drillFilter = query.MessageFilter{EmptyValueTargets: map[query.ViewType]bool{query.ViewSenderNames: true}}
	if !model.hasDrillFilter() {
		t.Error("expected hasDrillFilter=true for MatchEmptySenderName")
	}
}

// TestSenderNamesBreadcrumbRoundTrip verifies that drilling into a sender name,
// navigating to message detail, and going back preserves the SenderName filter.
func TestSenderNamesBreadcrumbRoundTrip(t *testing.T) {
	model := NewBuilder().
		WithMessages(
			query.MessageSummary{ID: 1, Subject: "Test message"},
		).
		WithLevel(levelMessageList).WithViewType(query.ViewRecipients).Build()
	model.drillFilter = query.MessageFilter{SenderName: "Alice Smith"}
	model.drillViewType = query.ViewSenderNames

	// Press Enter to go to message detail
	m, _ := sendKey(t, model, keyEnter())

	assertLevel(t, m, levelMessageDetail)

	// Verify breadcrumb saved SenderName
	if len(m.breadcrumbs) == 0 {
		t.Fatal("expected breadcrumb to be saved")
	}
	bc := m.breadcrumbs[len(m.breadcrumbs)-1]
	if bc.state.drillFilter.SenderName != "Alice Smith" {
		t.Errorf("expected breadcrumb SenderName='Alice Smith', got %q", bc.state.drillFilter.SenderName)
	}

	// Press Esc to go back
	newModel2, _ := m.goBack()
	m2 := newModel2.(Model)

	if m2.drillFilter.SenderName != "Alice Smith" {
		t.Errorf("expected SenderName='Alice Smith' after goBack, got %q", m2.drillFilter.SenderName)
	}
	if m2.drillViewType != query.ViewSenderNames {
		t.Errorf("expected drillViewType=ViewSenderNames, got %v", m2.drillViewType)
	}
}

// =============================================================================
// RecipientNames tests
// =============================================================================

func TestRecipientNamesDrillDown(t *testing.T) {
	rows := []query.AggregateRow{
		{Key: "Bob Jones", Count: 10},
		{Key: "Carol White", Count: 5},
	}

	model := NewBuilder().WithRows(rows...).
		WithPageSize(10).WithSize(100, 20).WithViewType(query.ViewRecipientNames).Build()

	// Press Enter to drill into first recipient name
	newModel, cmd := model.handleAggregateKeys(keyEnter())
	m := newModel.(Model)

	assertLevel(t, m, levelMessageList)
	if m.drillFilter.RecipientName != "Bob Jones" {
		t.Errorf("expected drillFilter.RecipientName='Bob Jones', got %q", m.drillFilter.RecipientName)
	}
	if m.drillViewType != query.ViewRecipientNames {
		t.Errorf("expected drillViewType=ViewRecipientNames, got %v", m.drillViewType)
	}
	if cmd == nil {
		t.Error("expected command to load messages")
	}
	if len(m.breadcrumbs) != 1 {
		t.Errorf("expected 1 breadcrumb, got %d", len(m.breadcrumbs))
	}
}

func TestRecipientNamesDrillDownEmptyKey(t *testing.T) {
	rows := []query.AggregateRow{
		{Key: "", Count: 3},
	}

	model := NewBuilder().WithRows(rows...).
		WithPageSize(10).WithSize(100, 20).WithViewType(query.ViewRecipientNames).Build()

	newModel, _ := model.handleAggregateKeys(keyEnter())
	m := newModel.(Model)

	if !m.drillFilter.MatchesEmpty(query.ViewRecipientNames) {
		t.Error("expected MatchEmptyRecipientName=true for empty key")
	}
	if m.drillFilter.RecipientName != "" {
		t.Errorf("expected empty RecipientName, got %q", m.drillFilter.RecipientName)
	}
}

func TestRecipientNamesDrillFilterKey(t *testing.T) {
	model := NewBuilder().
		WithRows(query.AggregateRow{Key: "test", Count: 1}).
		WithPageSize(10).WithSize(100, 20).Build()
	model.drillViewType = query.ViewRecipientNames
	model.drillFilter = query.MessageFilter{RecipientName: "Jane Doe"}

	key := model.drillFilterKey()
	if key != "Jane Doe" {
		t.Errorf("expected drillFilterKey='Jane Doe', got %q", key)
	}

	// Test empty case
	model.drillFilter = query.MessageFilter{EmptyValueTargets: map[query.ViewType]bool{query.ViewRecipientNames: true}}
	key = model.drillFilterKey()
	if key != "(empty)" {
		t.Errorf("expected '(empty)' for MatchEmptyRecipientName, got %q", key)
	}
}

func TestRecipientNamesBreadcrumbPrefix(t *testing.T) {
	prefix := viewTypePrefix(query.ViewRecipientNames)
	if prefix != "RN" {
		t.Errorf("expected prefix 'RN', got %q", prefix)
	}

	abbrev := viewTypeAbbrev(query.ViewRecipientNames)
	if abbrev != "Recipient Name" {
		t.Errorf("expected abbrev 'Recipient Name', got %q", abbrev)
	}
}

func TestShiftTabCyclesRecipientNames(t *testing.T) {
	model := NewBuilder().
		WithRows(query.AggregateRow{Key: "test", Count: 1}).
		WithPageSize(10).WithSize(100, 20).
		WithViewType(query.ViewRecipientNames).Build()

	// Shift+tab from RecipientNames should go back to Recipients
	m := applyAggregateKey(t, model, keyShiftTab())
	if m.viewType != query.ViewRecipients {
		t.Errorf("expected ViewRecipients after shift+tab from RecipientNames, got %v", m.viewType)
	}
}

func TestTabFromRecipientsThenRecipientNames(t *testing.T) {
	model := NewBuilder().
		WithRows(query.AggregateRow{Key: "test", Count: 1}).
		WithPageSize(10).WithSize(100, 20).
		WithViewType(query.ViewRecipients).Build()

	// Tab from Recipients should go to RecipientNames
	m := applyAggregateKey(t, model, keyTab())
	if m.viewType != query.ViewRecipientNames {
		t.Errorf("expected ViewRecipientNames after tab from Recipients, got %v", m.viewType)
	}

	// Tab from RecipientNames should go to Domains
	m.loading = false
	m = applyAggregateKey(t, m, keyTab())
	if m.viewType != query.ViewDomains {
		t.Errorf("expected ViewDomains after tab from RecipientNames, got %v", m.viewType)
	}
}

func TestSubAggregateFromRecipientNames(t *testing.T) {
	rows := []query.AggregateRow{
		{Key: "Bob Jones", Count: 10},
	}
	msgs := []query.MessageSummary{
		{ID: 1, Subject: "Test"},
	}

	model := NewBuilder().WithRows(rows...).WithMessages(msgs...).
		WithPageSize(10).WithSize(100, 20).WithViewType(query.ViewRecipientNames).Build()

	// Drill into the name
	newModel, _ := model.handleAggregateKeys(keyEnter())
	m := newModel.(Model)

	// Tab to sub-aggregate
	m.messages = msgs
	newModel2, _ := m.handleMessageListKeys(keyTab())
	m2 := newModel2.(Model)

	assertLevel(t, m2, levelDrillDown)
	// nextSubGroupView(RecipientNames) = Domains
	if m2.viewType != query.ViewDomains {
		t.Errorf("expected ViewDomains (nextSubGroupView from RecipientNames), got %v", m2.viewType)
	}
}

func TestHasDrillFilterWithRecipientName(t *testing.T) {
	model := NewBuilder().
		WithRows(query.AggregateRow{Key: "test", Count: 1}).
		WithPageSize(10).WithSize(100, 20).Build()

	model.drillFilter = query.MessageFilter{RecipientName: "John"}
	if !model.hasDrillFilter() {
		t.Error("expected hasDrillFilter=true for RecipientName")
	}

	model.drillFilter = query.MessageFilter{EmptyValueTargets: map[query.ViewType]bool{query.ViewRecipientNames: true}}
	if !model.hasDrillFilter() {
		t.Error("expected hasDrillFilter=true for MatchEmptyRecipientName")
	}
}

func TestRecipientNamesBreadcrumbRoundTrip(t *testing.T) {
	model := NewBuilder().
		WithMessages(
			query.MessageSummary{ID: 1, Subject: "Test message"},
		).
		WithLevel(levelMessageList).WithViewType(query.ViewRecipients).Build()
	model.drillFilter = query.MessageFilter{RecipientName: "Bob Jones"}
	model.drillViewType = query.ViewRecipientNames

	// Press Enter to go to message detail
	m, _ := sendKey(t, model, keyEnter())

	assertLevel(t, m, levelMessageDetail)

	// Verify breadcrumb saved RecipientName
	if len(m.breadcrumbs) == 0 {
		t.Fatal("expected breadcrumb to be saved")
	}
	bc := m.breadcrumbs[len(m.breadcrumbs)-1]
	if bc.state.drillFilter.RecipientName != "Bob Jones" {
		t.Errorf("expected breadcrumb RecipientName='Bob Jones', got %q", bc.state.drillFilter.RecipientName)
	}

	// Press Esc to go back
	newModel2, _ := m.goBack()
	m2 := newModel2.(Model)

	assertLevel(t, m2, levelMessageList)
	if m2.drillFilter.RecipientName != "Bob Jones" {
		t.Errorf("expected RecipientName preserved after goBack, got %q", m2.drillFilter.RecipientName)
	}
	if m2.drillViewType != query.ViewRecipientNames {
		t.Errorf("expected drillViewType=ViewRecipientNames, got %v", m2.drillViewType)
	}
}

// =============================================================================
// LoadData Stats Options Tests
// =============================================================================

// TestLoadDataSetsGroupByInStatsOpts verifies that loadData passes the current
// viewType as GroupBy in StatsOptions when search is active. This ensures the
// DuckDB engine searches the correct key columns for 1:N views.
func TestLoadDataSetsGroupByInStatsOpts(t *testing.T) {
	engine := newMockEngine(MockConfig{
		Rows: []query.AggregateRow{
			{Key: "bob@example.com", Count: 10, TotalSize: 5000},
		},
	})
	tracker := &statsTracker{result: &query.TotalStats{MessageCount: 10, TotalSize: 5000}}
	tracker.install(engine)

	model := New(engine, Options{DataDir: "/tmp/test", Version: "test123"})
	model.viewType = query.ViewRecipients
	model.searchQuery = "bob"
	model.level = levelAggregates
	model.width = 100
	model.height = 20

	// Execute the loadData command synchronously
	cmd := model.loadData()
	if cmd == nil {
		t.Fatal("expected loadData to return a command")
	}
	msg := cmd()

	// The command should have called GetTotalStats with GroupBy=ViewRecipients
	if tracker.callCount == 0 {
		t.Fatal("expected GetTotalStats to be called during loadData with search active")
	}
	if tracker.lastOpts.GroupBy != query.ViewRecipients {
		t.Errorf("expected StatsOptions.GroupBy=ViewRecipients, got %v", tracker.lastOpts.GroupBy)
	}
	if tracker.lastOpts.SearchQuery != "bob" {
		t.Errorf("expected StatsOptions.SearchQuery='bob', got %q", tracker.lastOpts.SearchQuery)
	}

	// Verify the result contains filteredStats
	dlm, ok := msg.(dataLoadedMsg)
	if !ok {
		t.Fatalf("expected dataLoadedMsg, got %T", msg)
	}
	if dlm.filteredStats == nil {
		t.Error("expected filteredStats to be set")
	}
}
