package tui

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/search"
)

// =============================================================================
// View Type Cycling Tests ('g' and Tab keys)
// =============================================================================

// TestGKeyCyclesViewType verifies that 'g' cycles through view types at aggregate level.
func TestGKeyCyclesViewType(t *testing.T) {
	assert := assert.New(t)
	model := NewBuilder().
		WithRows(query.AggregateRow{Key: "alice@example.com", Count: 10}).
		WithPageSize(10).WithSize(100, 20).
		WithViewType(query.ViewSenders).Build()
	// Set non-zero cursor/scroll to verify reset
	model.cursor = 5
	model.scrollOffset = 3

	// Press 'g' - should cycle to SenderNames (not go to home)
	newModel, cmd := model.handleAggregateKeys(key('g'))
	m := asModel(t, newModel)

	// Expected: viewType changes to ViewSenderNames
	assert.Equal(query.ViewSenderNames, m.viewType, "after 'g'")
	// Should trigger data reload
	assert.NotNil(cmd, "expected reload command after view type change")
	assert.True(m.loading, "expected loading=true after view type change")
	// Cursor and scroll should reset to 0 when view type changes
	assert.Equal(0, m.cursor, "after view type change")
	assert.Equal(0, m.scrollOffset, "after view type change")
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

		assert.Equal(t, expected, model.viewType, "cycle %d", i+1)
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
	assert.Equal(t, query.ViewRecipientNames, m.viewType, "skipping drillViewType")
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
	assert.Equal(t, query.ViewRecipients, m.viewType, "after 'g'")
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
	assert.Equal(t, 0, m.cursor, "after 'g' with no drill filter")
	assert.Equal(t, 0, m.scrollOffset, "after 'g' with no drill filter")
}

// TestTabCyclesViewTypeAtAggregates verifies Tab still cycles view types.
func TestTabCyclesViewTypeAtAggregates(t *testing.T) {
	assert := assert.New(t)
	model := NewBuilder().
		WithRows(query.AggregateRow{Key: "test", Count: 10}).
		WithPageSize(10).WithSize(100, 20).
		WithViewType(query.ViewSenders).Build()
	// Set non-zero cursor/scroll to verify reset
	model.cursor = 5
	model.scrollOffset = 3

	// Press Tab - should cycle to SenderNames
	newModel, cmd := model.handleAggregateKeys(keyTab())
	m := asModel(t, newModel)

	assert.Equal(query.ViewSenderNames, m.viewType, "after Tab")
	assert.NotNil(cmd, "expected reload command after Tab")
	// Cursor and scroll should reset to 0 when view type changes
	assert.Equal(0, m.cursor, "after Tab")
	assert.Equal(0, m.scrollOffset, "after Tab")
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

	assert.Equal(t, 0, m.cursor, "after 'home'")
	assert.Equal(t, 0, m.scrollOffset, "after 'home'")
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
	assert.Equal(t, query.ViewTime, m.viewType, "after 't' from Senders")
	assert.True(t, m.loading, "expected loading=true after 't' key")
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
		assert.Equal(t, query.ViewTime, m.viewType, "from %v after 't'", vt)
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
	assert.Equal(t, query.ViewTime, m.viewType, "expected to stay in ViewTime")
	assert.Equal(t, query.TimeMonth, m.timeGranularity, "after cycling from TimeYear")
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
	assert.Empty(t, m.selection.aggregateKeys, "expected selection cleared after 't' jump")
	assert.Equal(t, 0, m.cursor, "after 't' jump")
	assert.Equal(t, 0, m.scrollOffset, "after 't' jump")
}

func TestTKeyDoesNotResetSelectionOnCycle(t *testing.T) {
	assert := assert.New(t)
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
	assert.Equal(query.ViewTime, m.viewType)
	assert.Equal(query.TimeMonth, m.timeGranularity)
	assert.True(m.selection.aggregateKeys["2024"], "expected selection preserved after 't' granularity cycle")
	assert.Equal(1, m.cursor, "expected cursor preserved")
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
	assert.Equal(t, query.ViewSenders, m.viewType, "expected viewType unchanged")
	assert.False(t, m.loading, "expected no-op")
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
	assert.Equal(t, query.ViewTime, m.viewType, "after 't'")
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
	assert.False(t, m.loading, "expected no-op")
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

	assert.Equal(t, query.ViewRecipients, m.viewType, "sub-group from Senders should skip SenderNames")
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

	assert.Equal(t, query.ViewDomains, m.viewType, "sub-group from Recipients should skip RecipientNames")
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

	assert.Equal(t, query.ViewRecipients, m.viewType, "sub-group from SenderNames")
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

	assert.Equal(t, query.ViewDomains, m.viewType, "sub-group from RecipientNames")
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

	assert.Equal(t, query.ViewLabels, m.viewType, "sub-group from Domains")
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

			assert.Equal(t, tt.key, m.drillFilter.TimeRange.Period)
			assert.Equal(t, tt.granularity, m.drillFilter.TimeRange.Granularity)
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

			assert.Equal(t, tt.key, m.drillFilter.TimeRange.Period)
			assert.Equal(t, tt.subGranularity, m.drillFilter.TimeRange.Granularity,
				"should match sub-agg granularity, not initial %v", tt.initialGranularity)
			// Sender filter from original drill should be preserved
			assert.Equal(t, "alice@example.com", m.drillFilter.Sender,
				"should preserve parent drill filter")
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
	assert.Equal(t, query.TimeYear, m.drillFilter.TimeRange.Granularity,
		"non-time drill should not change it")
	assert.Equal(t, "INBOX", m.drillFilter.Label)
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
	assert.Equal(t, query.TimeYear, m.drillFilter.TimeRange.Granularity)
	assert.Equal(t, "2024", m.drillFilter.TimeRange.Period)
}

func TestSubAggregateTimeDrillDown_FullScenario(t *testing.T) {
	assert := assert.New(t)
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

	require.Equal(t, query.TimeMonth, step1.drillFilter.TimeRange.Granularity,
		"after top-level drill")

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
	assert.Equal(query.TimeYear, step3.drillFilter.TimeRange.Granularity,
		"was stale TimeMonth from top-level drill")
	assert.Equal("2024", step3.drillFilter.TimeRange.Period)
	// Original sender filter should be preserved
	assert.Equal("alice@example.com", step3.drillFilter.Sender)
}

// =============================================================================
// Sender Names View Tests
// =============================================================================

// TestSenderNamesDrillDown verifies that pressing Enter on a SenderNames row
// sets drillFilter.SenderName and transitions to message list.
func TestSenderNamesDrillDown(t *testing.T) {
	assert := assert.New(t)
	rows := []query.AggregateRow{
		{Key: "Alice Smith", Count: 10},
		{Key: "Bob Jones", Count: 5},
	}

	model := NewBuilder().WithRows(rows...).
		WithPageSize(10).WithSize(100, 20).WithViewType(query.ViewSenderNames).Build()

	// Press Enter to drill into first sender name
	newModel, cmd := model.handleAggregateKeys(keyEnter())
	m := asModel(t, newModel)

	assertLevel(t, m, levelMessageList)
	assert.Equal("Alice Smith", m.drillFilter.SenderName)
	assert.Equal(query.ViewSenderNames, m.drillViewType)
	assert.NotNil(cmd, "expected command to load messages")
	assert.Len(m.breadcrumbs, 1)
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
	m := asModel(t, newModel)

	assert.True(t, m.drillFilter.MatchesEmpty(query.ViewSenderNames), "expected MatchEmptySenderName=true for empty key")
	assert.Empty(t, m.drillFilter.SenderName)
}

// TestSenderNamesDrillFilterKey verifies drillFilterKey returns the SenderName.
func TestSenderNamesDrillFilterKey(t *testing.T) {
	model := NewBuilder().
		WithRows(query.AggregateRow{Key: "test", Count: 1}).
		WithPageSize(10).WithSize(100, 20).Build()
	model.drillViewType = query.ViewSenderNames
	model.drillFilter = query.MessageFilter{SenderName: "John Doe"}

	key := model.drillFilterKey()
	assert.Equal(t, "John Doe", key)

	// Test empty case
	model.drillFilter = query.MessageFilter{EmptyValueTargets: map[query.ViewType]bool{query.ViewSenderNames: true}}
	key = model.drillFilterKey()
	assert.Equal(t, "(empty)", key, "for MatchEmptySenderName")
}

// TestSenderNamesBreadcrumbPrefix verifies the "N:" prefix in breadcrumbs.
func TestSenderNamesBreadcrumbPrefix(t *testing.T) {
	prefix := viewTypePrefix(query.ViewSenderNames)
	assert.Equal(t, "N", prefix)

	abbrev := viewTypeAbbrev(query.ViewSenderNames)
	assert.Equal(t, "Sender Name", abbrev)
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
	assert.Equal(t, query.ViewSenders, m.viewType, "after shift+tab from SenderNames")
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
	m := asModel(t, newModel)

	// Tab to sub-aggregate
	m.messages = msgs
	newModel2, _ := m.handleMessageListKeys(keyTab())
	m2 := asModel(t, newModel2)

	assertLevel(t, m2, levelDrillDown)
	// Should skip SenderNames (the drill view type) and go to Recipients
	assert.Equal(t, query.ViewRecipients, m2.viewType, "skipping SenderNames")
}

// TestHasDrillFilterWithSenderName verifies hasDrillFilter returns true
// for SenderName and MatchEmptySenderName.
func TestHasDrillFilterWithSenderName(t *testing.T) {
	model := NewBuilder().
		WithRows(query.AggregateRow{Key: "test", Count: 1}).
		WithPageSize(10).WithSize(100, 20).Build()

	model.drillFilter = query.MessageFilter{SenderName: "John"}
	assert.True(t, model.hasDrillFilter(), "for SenderName")

	model.drillFilter = query.MessageFilter{EmptyValueTargets: map[query.ViewType]bool{query.ViewSenderNames: true}}
	assert.True(t, model.hasDrillFilter(), "for MatchEmptySenderName")
}

// TestSenderNamesBreadcrumbRoundTrip verifies that drilling into a sender name,
// navigating to message detail, and going back preserves the SenderName filter.
func TestSenderNamesBreadcrumbRoundTrip(t *testing.T) {
	assert := assert.New(t)
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
	require.NotEmpty(t, m.breadcrumbs, "expected breadcrumb to be saved")
	bc := m.breadcrumbs[len(m.breadcrumbs)-1]
	assert.Equal("Alice Smith", bc.state.drillFilter.SenderName)

	// Press Esc to go back
	newModel2, _ := m.goBack()
	m2 := asModel(t, newModel2)

	assert.Equal("Alice Smith", m2.drillFilter.SenderName, "after goBack")
	assert.Equal(query.ViewSenderNames, m2.drillViewType)
}

// =============================================================================
// RecipientNames tests
// =============================================================================

func TestRecipientNamesDrillDown(t *testing.T) {
	assert := assert.New(t)
	rows := []query.AggregateRow{
		{Key: "Bob Jones", Count: 10},
		{Key: "Carol White", Count: 5},
	}

	model := NewBuilder().WithRows(rows...).
		WithPageSize(10).WithSize(100, 20).WithViewType(query.ViewRecipientNames).Build()

	// Press Enter to drill into first recipient name
	newModel, cmd := model.handleAggregateKeys(keyEnter())
	m := asModel(t, newModel)

	assertLevel(t, m, levelMessageList)
	assert.Equal("Bob Jones", m.drillFilter.RecipientName)
	assert.Equal(query.ViewRecipientNames, m.drillViewType)
	assert.NotNil(cmd, "expected command to load messages")
	assert.Len(m.breadcrumbs, 1)
}

func TestRecipientNamesDrillDownEmptyKey(t *testing.T) {
	rows := []query.AggregateRow{
		{Key: "", Count: 3},
	}

	model := NewBuilder().WithRows(rows...).
		WithPageSize(10).WithSize(100, 20).WithViewType(query.ViewRecipientNames).Build()

	newModel, _ := model.handleAggregateKeys(keyEnter())
	m := asModel(t, newModel)

	assert.True(t, m.drillFilter.MatchesEmpty(query.ViewRecipientNames), "expected MatchEmptyRecipientName=true for empty key")
	assert.Empty(t, m.drillFilter.RecipientName)
}

func TestRecipientNamesDrillFilterKey(t *testing.T) {
	model := NewBuilder().
		WithRows(query.AggregateRow{Key: "test", Count: 1}).
		WithPageSize(10).WithSize(100, 20).Build()
	model.drillViewType = query.ViewRecipientNames
	model.drillFilter = query.MessageFilter{RecipientName: "Jane Doe"}

	key := model.drillFilterKey()
	assert.Equal(t, "Jane Doe", key)

	// Test empty case
	model.drillFilter = query.MessageFilter{EmptyValueTargets: map[query.ViewType]bool{query.ViewRecipientNames: true}}
	key = model.drillFilterKey()
	assert.Equal(t, "(empty)", key, "for MatchEmptyRecipientName")
}

func TestRecipientNamesBreadcrumbPrefix(t *testing.T) {
	prefix := viewTypePrefix(query.ViewRecipientNames)
	assert.Equal(t, "RN", prefix)

	abbrev := viewTypeAbbrev(query.ViewRecipientNames)
	assert.Equal(t, "Recipient Name", abbrev)
}

func TestShiftTabCyclesRecipientNames(t *testing.T) {
	model := NewBuilder().
		WithRows(query.AggregateRow{Key: "test", Count: 1}).
		WithPageSize(10).WithSize(100, 20).
		WithViewType(query.ViewRecipientNames).Build()

	// Shift+tab from RecipientNames should go back to Recipients
	m := applyAggregateKey(t, model, keyShiftTab())
	assert.Equal(t, query.ViewRecipients, m.viewType, "after shift+tab from RecipientNames")
}

func TestTabFromRecipientsThenRecipientNames(t *testing.T) {
	model := NewBuilder().
		WithRows(query.AggregateRow{Key: "test", Count: 1}).
		WithPageSize(10).WithSize(100, 20).
		WithViewType(query.ViewRecipients).Build()

	// Tab from Recipients should go to RecipientNames
	m := applyAggregateKey(t, model, keyTab())
	assert.Equal(t, query.ViewRecipientNames, m.viewType, "after tab from Recipients")

	// Tab from RecipientNames should go to Domains
	m.loading = false
	m = applyAggregateKey(t, m, keyTab())
	assert.Equal(t, query.ViewDomains, m.viewType, "after tab from RecipientNames")
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
	m := asModel(t, newModel)

	// Tab to sub-aggregate
	m.messages = msgs
	newModel2, _ := m.handleMessageListKeys(keyTab())
	m2 := asModel(t, newModel2)

	assertLevel(t, m2, levelDrillDown)
	// nextSubGroupView(RecipientNames) = Domains
	assert.Equal(t, query.ViewDomains, m2.viewType, "nextSubGroupView from RecipientNames")
}

func TestHasDrillFilterWithRecipientName(t *testing.T) {
	model := NewBuilder().
		WithRows(query.AggregateRow{Key: "test", Count: 1}).
		WithPageSize(10).WithSize(100, 20).Build()

	model.drillFilter = query.MessageFilter{RecipientName: "John"}
	assert.True(t, model.hasDrillFilter(), "for RecipientName")

	model.drillFilter = query.MessageFilter{EmptyValueTargets: map[query.ViewType]bool{query.ViewRecipientNames: true}}
	assert.True(t, model.hasDrillFilter(), "for MatchEmptyRecipientName")
}

func TestRecipientNamesBreadcrumbRoundTrip(t *testing.T) {
	assert := assert.New(t)
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
	require.NotEmpty(t, m.breadcrumbs, "expected breadcrumb to be saved")
	bc := m.breadcrumbs[len(m.breadcrumbs)-1]
	assert.Equal("Bob Jones", bc.state.drillFilter.RecipientName)

	// Press Esc to go back
	newModel2, _ := m.goBack()
	m2 := asModel(t, newModel2)

	assertLevel(t, m2, levelMessageList)
	assert.Equal("Bob Jones", m2.drillFilter.RecipientName, "preserved after goBack")
	assert.Equal(query.ViewRecipientNames, m2.drillViewType)
}

// =============================================================================
// LoadData Stats Options Tests
// =============================================================================

// TestLoadDataSetsGroupByInStatsOpts verifies that loadData passes the current
// viewType as GroupBy in StatsOptions when search is active. This ensures the
// DuckDB engine searches the correct key columns for 1:N views.
func TestLoadDataSetsGroupByInStatsOpts(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
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
	require.NotNil(cmd, "expected loadData to return a command")
	msg := cmd()

	// The command should have called GetTotalStats with GroupBy=ViewRecipients
	require.NotZero(tracker.callCount, "expected GetTotalStats to be called during loadData with search active")
	assert.Equal(query.ViewRecipients, tracker.lastOpts.GroupBy)
	scopedSearch := search.Parse(tracker.lastOpts.SearchQuery)
	assert.Equal([]string{"bob"}, scopedSearch.TextTerms)
	assert.Equal([]string{"email"}, scopedSearch.MessageTypes)

	// Verify the result contains filteredStats
	dlm, ok := msg.(dataLoadedMsg)
	require.True(ok, "expected dataLoadedMsg, got %T", msg)
	assert.NotNil(dlm.filteredStats, "expected filteredStats to be set")
}
