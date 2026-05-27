package tui

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/query"
)

// =============================================================================
// Async Response Handling Tests
// =============================================================================

func TestStaleAsyncResponsesIgnored(t *testing.T) {
	model := NewBuilder().
		WithLevel(levelMessageList).
		Build()
	model.loadRequestID = 5 // Current request ID

	// Simulate a stale response with old request ID
	staleMsg := messagesLoadedMsg{
		messages:  []query.MessageSummary{{ID: 99, Subject: "Stale"}},
		requestID: 3, // Old request ID
	}

	m := sendMsg(t, model, staleMsg)

	// Stale response should be ignored - messages should be unchanged (empty)
	assert.Empty(t, m.messages, "stale response should be ignored")

	// Now send a valid response with current request ID
	validMsg := messagesLoadedMsg{
		messages:  []query.MessageSummary{{ID: 1, Subject: "Valid"}},
		requestID: 5, // Current request ID
	}

	m = sendMsg(t, m, validMsg)

	// Valid response should be processed
	require.Len(t, m.messages, 1, "valid response should be processed")
	assert.Equal(t, "Valid", m.messages[0].Subject)
}

func TestStaleDetailResponsesIgnored(t *testing.T) {
	model := NewBuilder().
		WithLevel(levelMessageDetail).
		WithSize(100, 30).
		WithPageSize(20).
		Build()
	model.detailRequestID = 10 // Current request ID

	// Simulate a stale response with old request ID
	staleMsg := messageDetailLoadedMsg{
		detail:    &query.MessageDetail{ID: 99, Subject: "Stale Detail"},
		requestID: 8, // Old request ID
	}

	m := sendMsg(t, model, staleMsg)

	// Stale response should be ignored
	assert.Nil(t, m.messageDetail, "stale detail response should be ignored")

	// Now send a valid response with current request ID
	validMsg := messageDetailLoadedMsg{
		detail:    &query.MessageDetail{ID: 1, Subject: "Valid Detail"},
		requestID: 10, // Current request ID
	}

	m = sendMsg(t, m, validMsg)

	// Valid response should be processed
	require.NotNil(t, m.messageDetail, "valid detail response should be processed")
	assert.Equal(t, "Valid Detail", m.messageDetail.Subject)
}

// =============================================================================
// Window Size and Page Size Tests
// =============================================================================

func TestWindowSizeClampNegative(t *testing.T) {
	model := NewBuilder().Build()

	// Simulate negative window size (can happen during terminal resize)
	m := resizeModel(t, model, -1, -1)

	assert.GreaterOrEqual(t, m.width, 0)
	assert.GreaterOrEqual(t, m.height, 0)
	assert.GreaterOrEqual(t, m.pageSize, 1)
}

func TestDefaultLoadingWithNoData(t *testing.T) {
	// Build with no rows/messages and no explicit loading override.
	// The builder should preserve New()'s default loading=true.
	model := NewBuilder().WithPageSize(10).WithSize(100, 20).Build()

	assert.True(t, model.loading, "expected loading=true (New default) when no data provided")
}

func TestPageSizeRawZeroAndNegative(t *testing.T) {
	tests := []struct {
		name     string
		pageSize int
	}{
		{"zero page size", 0},
		{"negative page size", -1},
		{"large negative page size", -100},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Should not panic when building or rendering with raw zero/negative page sizes.
			model := NewBuilder().
				WithPageSizeRaw(tc.pageSize).
				WithRows(testAggregateRows...).
				WithSize(100, 20).
				Build()

			assert.Equal(t, tc.pageSize, model.pageSize)

			// Rendering should not panic even with unusual page sizes.
			_ = model.View()
		})
	}
}

func TestWithPageSizeClearsRawFlag(t *testing.T) {
	// WithPageSizeRaw followed by WithPageSize should clear the raw flag,
	// so the normal clamping logic applies.
	model := NewBuilder().
		WithPageSizeRaw(0).
		WithPageSize(10).
		WithRows(testAggregateRows...).
		WithSize(100, 20).
		Build()

	assert.Equal(t, 10, model.pageSize, "expected pageSize after WithPageSize cleared raw flag")
}

// =============================================================================
// List Navigation Helper Tests
// =============================================================================

func TestNavigateList(t *testing.T) {
	tests := []struct {
		name        string
		key         string
		itemCount   int
		initCursor  int
		wantCursor  int
		wantHandled bool
	}{
		{"down from top", "j", 5, 0, 1, true},
		{"up from second", "k", 5, 1, 0, true},
		{"down at end", "j", 5, 4, 4, true},
		{"up at top", "k", 5, 0, 0, true},
		{"unhandled key", "x", 5, 0, 0, false},
		{"empty list down", "j", 0, 0, 0, true},
		{"empty list up", "k", 0, 0, 0, true},
		{"home", "home", 5, 3, 0, true},
		{"end", "end", 5, 0, 4, true},
		{"end empty list", "end", 0, 0, 0, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := NewBuilder().WithRows(
				query.AggregateRow{Key: "a"},
			).Build()
			m.cursor = tt.initCursor

			handled := m.navigateList(tt.key, tt.itemCount)
			assert.Equal(t, tt.wantHandled, handled, "navigateList(%q, %d) handled", tt.key, tt.itemCount)
			assert.Equal(t, tt.wantCursor, m.cursor, "navigateList(%q, %d) cursor", tt.key, tt.itemCount)
		})
	}
}

// =============================================================================
// Page Up/Down Scroll Tests
// =============================================================================

func TestNavigateListPageDown(t *testing.T) {
	tests := []struct {
		name             string
		pageSize         int // raw pageSize; visibleRows = pageSize - 1
		itemCount        int
		initCursor       int
		initScrollOffset int
		wantCursor       int
		wantScrollOffset int
	}{
		{
			name:             "moves by visibleRows not pageSize",
			pageSize:         24,
			itemCount:        100,
			initCursor:       0,
			initScrollOffset: 0,
			wantCursor:       23, // visibleRows = 24 - 1 = 23
			wantScrollOffset: 23,
		},
		{
			name:             "preserves relative cursor position",
			pageSize:         24,
			itemCount:        100,
			initCursor:       5,
			initScrollOffset: 0,
			wantCursor:       28, // 5 + 23
			wantScrollOffset: 23, // 0 + 23
		},
		{
			name:             "clamps cursor at end of list",
			pageSize:         24,
			itemCount:        30,
			initCursor:       20,
			initScrollOffset: 10,
			wantCursor:       29, // clamped to itemCount-1
			wantScrollOffset: 7,  // clamped: 30 - 23 = 7
		},
		{
			name:             "clamps scroll at max",
			pageSize:         24,
			itemCount:        40,
			initCursor:       25,
			initScrollOffset: 15,
			wantCursor:       39, // clamped to 39
			wantScrollOffset: 17, // max: 40 - 23 = 17
		},
		{
			name:             "small list fewer items than visibleRows",
			pageSize:         24,
			itemCount:        10,
			initCursor:       3,
			initScrollOffset: 0,
			wantCursor:       9,
			wantScrollOffset: 0, // max scroll = 0 since items < visibleRows
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := NewBuilder().WithPageSizeRaw(tt.pageSize).Build()
			m.cursor = tt.initCursor
			m.scrollOffset = tt.initScrollOffset

			handled := m.navigateList("pgdown", tt.itemCount)
			require.True(t, handled, "expected pgdown to be handled")
			assert.Equal(t, tt.wantCursor, m.cursor)
			assert.Equal(t, tt.wantScrollOffset, m.scrollOffset)
		})
	}
}

func TestNavigateListPageUp(t *testing.T) {
	tests := []struct {
		name             string
		pageSize         int
		itemCount        int
		initCursor       int
		initScrollOffset int
		wantCursor       int
		wantScrollOffset int
	}{
		{
			name:             "moves by visibleRows not pageSize",
			pageSize:         24,
			itemCount:        100,
			initCursor:       50,
			initScrollOffset: 30,
			wantCursor:       27, // 50 - 23
			wantScrollOffset: 7,  // 30 - 23
		},
		{
			name:             "preserves relative cursor position",
			pageSize:         24,
			itemCount:        100,
			initCursor:       30,
			initScrollOffset: 25,
			wantCursor:       7,
			wantScrollOffset: 2,
		},
		{
			name:             "clamps cursor at top",
			pageSize:         24,
			itemCount:        100,
			initCursor:       10,
			initScrollOffset: 5,
			wantCursor:       0,
			wantScrollOffset: 0,
		},
		{
			name:             "clamps scroll at zero",
			pageSize:         24,
			itemCount:        100,
			initCursor:       30,
			initScrollOffset: 10,
			wantCursor:       7, // 30 - 23
			wantScrollOffset: 0, // clamped to 0
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := NewBuilder().WithPageSizeRaw(tt.pageSize).Build()
			m.cursor = tt.initCursor
			m.scrollOffset = tt.initScrollOffset

			handled := m.navigateList("pgup", tt.itemCount)
			require.True(t, handled, "expected pgup to be handled")
			assert.Equal(t, tt.wantCursor, m.cursor)
			assert.Equal(t, tt.wantScrollOffset, m.scrollOffset)
		})
	}
}

// =============================================================================
// Thread View Page Up/Down Tests
// =============================================================================

func TestThreadViewPageDown(t *testing.T) {
	tests := []struct {
		name             string
		pageSize         int
		threadMsgCount   int
		initCursor       int
		initScrollOffset int
		wantCursor       int
		wantScrollOffset int
	}{
		{
			name:             "moves by visibleRows not pageSize",
			pageSize:         24,
			threadMsgCount:   100,
			initCursor:       0,
			initScrollOffset: 0,
			wantCursor:       23,
			wantScrollOffset: 23,
		},
		{
			name:             "preserves relative cursor position",
			pageSize:         24,
			threadMsgCount:   100,
			initCursor:       5,
			initScrollOffset: 0,
			wantCursor:       28,
			wantScrollOffset: 23,
		},
		{
			name:             "clamps at end of thread",
			pageSize:         24,
			threadMsgCount:   30,
			initCursor:       20,
			initScrollOffset: 10,
			wantCursor:       29,
			wantScrollOffset: 7,
		},
		{
			name:             "small thread fewer items than visibleRows",
			pageSize:         24,
			threadMsgCount:   5,
			initCursor:       1,
			initScrollOffset: 0,
			wantCursor:       4,
			wantScrollOffset: 0,
		},
		{
			name:             "empty thread",
			pageSize:         24,
			threadMsgCount:   0,
			initCursor:       0,
			initScrollOffset: 0,
			wantCursor:       0,
			wantScrollOffset: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := NewBuilder().
				WithLevel(levelThreadView).
				WithPageSizeRaw(tt.pageSize).
				WithLoading(false).
				Build()
			m.threadMessages = makeMessages(tt.threadMsgCount)
			m.threadCursor = tt.initCursor
			m.threadScrollOffset = tt.initScrollOffset

			m, _ = sendKey(t, m, tea.KeyMsg{Type: tea.KeyPgDown})

			assert.Equal(t, tt.wantCursor, m.threadCursor)
			assert.Equal(t, tt.wantScrollOffset, m.threadScrollOffset)
		})
	}
}

func TestThreadViewPageUp(t *testing.T) {
	tests := []struct {
		name             string
		pageSize         int
		threadMsgCount   int
		initCursor       int
		initScrollOffset int
		wantCursor       int
		wantScrollOffset int
	}{
		{
			name:             "moves by visibleRows not pageSize",
			pageSize:         24,
			threadMsgCount:   100,
			initCursor:       50,
			initScrollOffset: 30,
			wantCursor:       27,
			wantScrollOffset: 7,
		},
		{
			name:             "preserves relative cursor position",
			pageSize:         24,
			threadMsgCount:   100,
			initCursor:       30,
			initScrollOffset: 25,
			wantCursor:       7,
			wantScrollOffset: 2,
		},
		{
			name:             "clamps at top",
			pageSize:         24,
			threadMsgCount:   100,
			initCursor:       10,
			initScrollOffset: 5,
			wantCursor:       0,
			wantScrollOffset: 0,
		},
		{
			name:             "small thread",
			pageSize:         24,
			threadMsgCount:   5,
			initCursor:       4,
			initScrollOffset: 0,
			wantCursor:       0,
			wantScrollOffset: 0,
		},
		{
			name:             "empty thread",
			pageSize:         24,
			threadMsgCount:   0,
			initCursor:       0,
			initScrollOffset: 0,
			wantCursor:       0,
			wantScrollOffset: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := NewBuilder().
				WithLevel(levelThreadView).
				WithPageSizeRaw(tt.pageSize).
				WithLoading(false).
				Build()
			m.threadMessages = makeMessages(tt.threadMsgCount)
			m.threadCursor = tt.initCursor
			m.threadScrollOffset = tt.initScrollOffset

			m, _ = sendKey(t, m, tea.KeyMsg{Type: tea.KeyPgUp})

			assert.Equal(t, tt.wantCursor, m.threadCursor)
			assert.Equal(t, tt.wantScrollOffset, m.threadScrollOffset)
		})
	}
}

func TestVisibleRows(t *testing.T) {
	tests := []struct {
		name     string
		pageSize int
		want     int
	}{
		{"normal", 24, 23},
		{"small", 2, 1},
		{"minimum clamped", 1, 1},
		{"zero clamped", 0, 1},
		{"negative clamped", -5, 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := NewBuilder().WithPageSizeRaw(tt.pageSize).Build()
			assert.Equal(t, tt.want, m.visibleRows())
		})
	}
}
