package tui

import "go.kenn.io/msgvault/internal/query"

// tuiMode represents the top-level mode: Email or Texts.
type tuiMode int

const (
	modeEmail tuiMode = iota
	modeTexts
)

// textViewLevel represents the navigation depth within Texts mode.
type textViewLevel int

const (
	textLevelConversations      textViewLevel = iota // Top-level conversation list
	textLevelAggregate                               // Aggregate view (contacts, sources, etc.)
	textLevelDrillConversations                      // Conversations filtered by aggregate drill-down
	textLevelTimeline                                // Message timeline within a conversation
)

// textState holds all state for the Texts mode TUI.
type textState struct {
	viewType       query.TextViewType
	level          textViewLevel
	conversations  []query.ConversationRow
	aggregateRows  []query.AggregateRow
	messages       []query.MessageSummary
	cursor         int
	scrollOffset   int
	selectedConvID int64
	filter         query.TextFilter
	stats          *query.TotalStats
	breadcrumbs    []textNavSnapshot

	// unfilteredMessages holds the original timeline messages before
	// search filtering. Repeated searches always filter from this
	// snapshot to prevent stacking breadcrumbs and narrowing results.
	unfilteredMessages []query.MessageSummary
}

// textNavSnapshot stores state for text mode navigation history.
type textNavSnapshot struct {
	level          textViewLevel
	viewType       query.TextViewType
	cursor         int
	scrollOffset   int
	filter         query.TextFilter
	selectedConvID int64
}

// clampCursorToConversations ensures cursor and scrollOffset
// are within valid bounds after conversation data changes.
func (ts *textState) clampCursorToConversations() {
	n := len(ts.conversations)
	if ts.cursor >= n {
		ts.cursor = max(n-1, 0)
	}
	if ts.scrollOffset > ts.cursor {
		ts.scrollOffset = ts.cursor
	}
}

// clampCursorToAggregates ensures cursor and scrollOffset
// are within valid bounds after aggregate data changes.
func (ts *textState) clampCursorToAggregates() {
	n := len(ts.aggregateRows)
	if ts.cursor >= n {
		ts.cursor = max(n-1, 0)
	}
	if ts.scrollOffset > ts.cursor {
		ts.scrollOffset = ts.cursor
	}
}
