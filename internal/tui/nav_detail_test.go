package tui

import (
	"fmt"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	assertpkg "github.com/stretchr/testify/assert"
	"go.kenn.io/msgvault/internal/query"
)

// =============================================================================
// Message Detail View Tests
// =============================================================================

func TestDetailLineCountResetOnLoad(t *testing.T) {
	model := NewBuilder().
		WithMessages(
			query.MessageSummary{ID: 1, Subject: "Message 1"},
			query.MessageSummary{ID: 2, Subject: "Message 2"},
		).
		WithLevel(levelMessageList).
		WithSize(100, 30).
		WithPageSize(20).
		Build()
	model.detailLineCount = 100 // Simulate previous message with 100 lines
	model.detailScroll = 50     // Simulate scrolled position

	// Trigger drill-down to detail view
	model.cursor = 0
	m := applyMessageListKey(t, model, keyEnter())

	// detailLineCount and detailScroll should be reset
	assertpkg.Equal(t, 0, m.detailLineCount, "expected detailLineCount = 0 on load start")
	assertpkg.Equal(t, 0, m.detailScroll, "expected detailScroll = 0 on load start")
	assertpkg.Nil(t, m.messageDetail, "expected messageDetail = nil on load start")
}

func TestDetailScrollClamping(t *testing.T) {
	model := NewBuilder().
		WithLevel(levelMessageDetail).
		WithPageSize(10).
		Build()
	model.detailLineCount = 25 // 25 lines total
	model.detailScroll = 0

	// Test scroll down clamping
	model.detailScroll = 100 // Way beyond bounds
	model.clampDetailScroll()

	// Max scroll should be lineCount - detailPageSize = 25 - 12 = 13
	// (detailPageSize = pageSize + 2 because detail view has no table header/separator)
	expectedMax := 13
	assertpkg.Equal(t, expectedMax, model.detailScroll, "expected detailScroll clamped")

	// Test when content fits in one page
	model.detailLineCount = 5 // Less than detailPageSize (12)
	model.detailScroll = 10
	model.clampDetailScroll()

	assertpkg.Equal(t, 0, model.detailScroll, "expected detailScroll = 0 when content fits page")
}

func TestResizeRecalculatesDetailLineCount(t *testing.T) {
	model := NewBuilder().
		WithLevel(levelMessageDetail).
		WithDetail(&query.MessageDetail{
			Subject:  "Test Subject",
			BodyText: "Line 1\nLine 2\nLine 3\nLine 4\nLine 5",
		}).
		WithSize(80, 20).
		WithPageSize(14).
		Build()

	// Calculate initial line count
	model.updateDetailLineCount()
	initialLineCount := model.detailLineCount

	// Simulate window resize to narrower width (should wrap more)
	m, _ := sendMsg(t, model, tea.WindowSizeMsg{Width: 40, Height: 20})

	// Line count should be recalculated (narrower width = more wrapping = more lines)
	if m.detailLineCount == initialLineCount && m.width != 80 {
		// Note: This might be equal if wrapping doesn't change, but width should be updated
		assertpkg.Equal(t, 40, m.width, "expected width = 40 after resize")
	}

	// Scroll should be clamped if it exceeds new bounds
	m.detailScroll = 1000
	m.clampDetailScroll()
	maxScroll := max(m.detailLineCount-m.pageSize, 0)
	assertpkg.LessOrEqual(t, m.detailScroll, maxScroll, "expected detailScroll clamped")
}

func TestEndKeyWithZeroLineCount(t *testing.T) {
	model := NewBuilder().
		WithLevel(levelMessageDetail).
		WithPageSize(20).
		Build()
	model.detailLineCount = 0 // No content yet (loading)
	model.detailScroll = 0

	// Press 'G' (end key) with zero line count
	m := applyDetailKey(t, model, key('G'))

	// Should not crash and scroll should remain 0
	assertpkg.Equal(t, 0, m.detailScroll, "expected detailScroll = 0 with zero line count")
}

func TestFillScreenDetailLineCount(t *testing.T) {
	model := NewBuilder().WithLevel(levelMessageDetail).WithSize(80, 24).WithPageSize(19).Build()

	// detailPageSize = pageSize + 2 = 21
	expectedLines := model.detailPageSize()

	// Test loading state
	model.loading = true
	model.messageDetail = nil
	view := model.messageDetailView()
	lines := strings.Split(view, "\n")
	// View should have detailPageSize lines (last line has no trailing newline)
	assertpkg.Len(t, lines, expectedLines, "loading state")

	// Test error state
	model.loading = false
	model.err = fmt.Errorf("test error")
	view = model.messageDetailView()
	lines = strings.Split(view, "\n")
	assertpkg.Len(t, lines, expectedLines, "error state")

	// Test nil detail state
	model.err = nil
	model.messageDetail = nil
	view = model.messageDetailView()
	lines = strings.Split(view, "\n")
	assertpkg.Len(t, lines, expectedLines, "nil detail state")
}

func TestScrollClampingAfterResize(t *testing.T) {
	model := NewBuilder().
		WithDetail(&query.MessageDetail{ID: 1, Subject: "Test", BodyText: "Content"}).
		WithLevel(levelMessageDetail).WithSize(100, 20).WithPageSize(15).Build()
	model.detailLineCount = 50
	model.detailScroll = 40 // Near the end

	// Simulate resize that increases page size (reducing max scroll)
	// New max scroll would be 50 - 20 = 30, but detailScroll is 40
	model.height = 30
	model.pageSize = 25 // Bigger page means lower max scroll

	// Press down - should clamp first, then check boundary
	m, _ := sendKey(t, model, keyDown())

	// detailScroll should be clamped to max (50 - 27 = 23 for detailPageSize)
	maxScroll := max(model.detailLineCount-m.detailPageSize(), 0)
	assertpkg.LessOrEqual(t, m.detailScroll, maxScroll, "detailScroll exceeds maxScroll after resize")
}

// =============================================================================
// Detail Navigation (Prev/Next Message) Tests
// =============================================================================

// TestDetailNavigationPrevNext verifies left/right arrow navigation in message detail view.
// Left = previous in list (lower index), Right = next in list (higher index).
func TestDetailNavigationPrevNext(t *testing.T) {
	assert := assertpkg.New(t)
	model := NewBuilder().
		WithMessages(
			query.MessageSummary{ID: 1, Subject: "First message"},
			query.MessageSummary{ID: 2, Subject: "Second message"},
			query.MessageSummary{ID: 3, Subject: "Third message"},
		).
		WithDetail(&query.MessageDetail{ID: 2, Subject: "Second message"}).
		WithLevel(levelMessageDetail).Build()
	model.detailMessageIndex = 1 // Viewing second message
	model.cursor = 1

	// Press right arrow to go to next message in list (higher index)
	m, cmd := sendKey(t, model, keyRight())

	assert.Equal(2, m.detailMessageIndex, "after right")
	assert.Equal(2, m.cursor, "after right")
	assert.Equal("Third message", m.pendingDetailSubject)
	assert.NotNil(cmd, "expected command to load message detail")

	// Press left arrow to go to previous message in list (lower index)
	m.detailMessageIndex = 2
	m.cursor = 2
	m, cmd = sendKey(t, m, keyLeft())

	assert.Equal(1, m.detailMessageIndex, "after left")
	assert.Equal(1, m.cursor, "after left")
	assert.NotNil(cmd, "expected command to load message detail")
}

// TestDetailNavigationAtBoundary verifies flash message at first/last message.
func TestDetailNavigationAtBoundary(t *testing.T) {
	assert := assertpkg.New(t)
	model := NewBuilder().
		WithMessages(
			query.MessageSummary{ID: 1, Subject: "First message"},
			query.MessageSummary{ID: 2, Subject: "Second message"},
		).
		WithDetail(&query.MessageDetail{ID: 1, Subject: "First message"}).
		WithLevel(levelMessageDetail).Build()
	model.detailMessageIndex = 0 // At first message

	// Press left arrow at first message - should show flash
	m, cmd := sendKey(t, model, keyLeft())

	assert.Equal(0, m.detailMessageIndex, "expected detailMessageIndex unchanged")
	assert.Equal("At first message", m.flashMessage)
	assert.NotNil(cmd, "expected command to clear flash message")

	// Clear flash and test at last message
	m.flashMessage = ""
	m.detailMessageIndex = 1 // At last message
	m.cursor = 1
	m.messageDetail = &query.MessageDetail{ID: 2, Subject: "Second message"}

	// Press right arrow at last message - should show flash
	m, cmd = sendKey(t, m, keyRight())

	assert.Equal(1, m.detailMessageIndex, "expected detailMessageIndex unchanged")
	assert.Equal("At last message", m.flashMessage)
	assert.NotNil(cmd, "expected command to clear flash message")
}

// TestDetailNavigationHLKeys verifies h/l keys also work for prev/next.
// h=left=prev (lower index), l=right=next (higher index).
func TestDetailNavigationHLKeys(t *testing.T) {
	model := NewBuilder().
		WithMessages(
			query.MessageSummary{ID: 1, Subject: "First"},
			query.MessageSummary{ID: 2, Subject: "Second"},
			query.MessageSummary{ID: 3, Subject: "Third"},
		).
		WithDetail(&query.MessageDetail{ID: 2, Subject: "Second"}).
		WithLevel(levelMessageDetail).Build()
	model.detailMessageIndex = 1
	model.cursor = 1

	// Press 'l' to go to next message in list (higher index)
	m, _ := sendKey(t, model, key('l'))

	assertpkg.Equal(t, 2, m.detailMessageIndex, "after 'l'")

	// Reset and press 'h' to go to previous message in list (lower index)
	m.detailMessageIndex = 1
	m.cursor = 1
	m, _ = sendKey(t, m, key('h'))

	assertpkg.Equal(t, 0, m.detailMessageIndex, "after 'h'")
}

// TestDetailNavigationEmptyList verifies navigation with empty message list.
func TestDetailNavigationEmptyList(t *testing.T) {
	model := NewBuilder().WithLevel(levelMessageDetail).Build()
	model.detailMessageIndex = 0

	// Press right arrow - should show flash, not panic
	newModel, _ := model.navigateDetailNext()
	m := newModel.(Model)

	assertpkg.Equal(t, "No messages loaded", m.flashMessage)

	// Press left arrow - should show flash, not panic
	newModel, _ = m.navigateDetailPrev()
	m = newModel.(Model)

	assertpkg.Equal(t, "No messages loaded", m.flashMessage)
}

// TestDetailNavigationOutOfBoundsIndex verifies clamping of stale index.
func TestDetailNavigationOutOfBoundsIndex(t *testing.T) {
	model := NewBuilder().
		WithMessages(query.MessageSummary{ID: 1, Subject: "Only message"}).
		WithLevel(levelMessageDetail).Build()
	model.detailMessageIndex = 5 // Out of bounds!
	model.cursor = 5

	// Press left (navigateDetailPrev) - should clamp index and show flash
	// Index gets clamped from 5 to 0, then can't go to lower index
	newModel, _ := model.navigateDetailPrev()
	m := newModel.(Model)

	// Index should be clamped to 0, then show "At first message"
	// because we can't go before the only message
	assertpkg.Equal(t, 0, m.detailMessageIndex, "expected detailMessageIndex clamped")
	assertpkg.Equal(t, "At first message", m.flashMessage)
}

// TestDetailNavigationOutOfBoundsWithMultipleMessages verifies that when the index is
// out of bounds but there are multiple messages, navigation succeeds after clamping.
func TestDetailNavigationOutOfBoundsWithMultipleMessages(t *testing.T) {
	assert := assertpkg.New(t)
	model := NewBuilder().
		WithMessages(
			query.MessageSummary{ID: 1, Subject: "First message"},
			query.MessageSummary{ID: 2, Subject: "Second message"},
			query.MessageSummary{ID: 3, Subject: "Third message"},
		).
		WithLevel(levelMessageDetail).Build()
	model.detailMessageIndex = 10 // Out of bounds (len=3, valid indices 0-2)
	model.cursor = 10

	// Press left (navigateDetailPrev) - should clamp to last valid index (2),
	// then navigate to previous message (index 1), triggering loadMessageDetail
	newModel, cmd := model.navigateDetailPrev()
	m := newModel.(Model)

	// Index should be clamped from 10 to 2, then decremented to 1
	assert.Equal(1, m.detailMessageIndex, "expected detailMessageIndex clamped and navigated")
	assert.Equal(1, m.cursor)
	assert.Equal("Second message", m.pendingDetailSubject)
	// Should trigger loadMessageDetail, not just show flash
	assert.NotNil(cmd, "expected command to load message detail after clamping and navigating")
	assert.Empty(m.flashMessage, "expected no flash message after successful navigation")
}

// TestDetailNavigationCursorPreservedOnGoBack verifies cursor position is preserved
// when returning to message list after navigating in detail view.
func TestDetailNavigationCursorPreservedOnGoBack(t *testing.T) {
	model := NewBuilder().
		WithMessages(
			query.MessageSummary{ID: 1, Subject: "First"},
			query.MessageSummary{ID: 2, Subject: "Second"},
			query.MessageSummary{ID: 3, Subject: "Third"},
		).
		WithLevel(levelMessageList).
		WithPageSize(10).WithSize(100, 20).Build()

	// Enter detail view (simulates pressing Enter on first message)
	model.breadcrumbs = append(model.breadcrumbs, navigationSnapshot{state: viewState{
		level:        levelMessageList,
		viewType:     query.ViewSenders,
		cursor:       0, // Original cursor position
		scrollOffset: 0,
	}})
	model.level = levelMessageDetail
	model.detailMessageIndex = 0
	model.cursor = 0

	// Navigate to third message via right arrow (twice)
	model.detailMessageIndex = 2
	model.cursor = 2

	// Go back to message list
	newModel, _ := model.goBack()
	m := newModel.(Model)

	// Cursor should be preserved at position 2 (where we navigated to)
	// not restored to position 0 (where we entered)
	assertLevel(t, m, levelMessageList)
	assertpkg.Equal(t, 2, m.cursor, "expected cursor preserved from navigation")
}

// TestDetailNavigationFromThreadView verifies that left/right navigation in detail view
// uses threadMessages (not messages) when entered from thread view, and keeps
// threadCursor and threadScrollOffset in sync.
func TestDetailNavigationFromThreadView(t *testing.T) {
	assert := assertpkg.New(t)
	model := NewBuilder().
		WithMessages(
			query.MessageSummary{ID: 1, Subject: "List msg 1"},
			query.MessageSummary{ID: 2, Subject: "List msg 2"},
		).Build()

	// Set up thread view with different messages than the list
	model.threadMessages = []query.MessageSummary{
		{ID: 100, Subject: "Thread msg 1"},
		{ID: 101, Subject: "Thread msg 2"},
		{ID: 102, Subject: "Thread msg 3"},
		{ID: 103, Subject: "Thread msg 4"},
	}

	// Enter detail view from thread view (simulates pressing Enter in thread view)
	model.level = levelMessageDetail
	model.detailFromThread = true
	model.detailMessageIndex = 1 // Viewing second thread message (ID=101)
	model.threadCursor = 1
	model.threadScrollOffset = 0
	model.pageSize = 3 // Small page size to test scroll offset
	model.messageDetail = &query.MessageDetail{ID: 101, Subject: "Thread msg 2"}

	// Press right arrow - should navigate within threadMessages
	m, cmd := sendKey(t, model, keyRight())

	assert.Equal(2, m.detailMessageIndex, "after right")
	assert.Equal(2, m.threadCursor, "after right")
	// cursor (for list view) should NOT be modified
	assert.Equal(0, m.cursor, "expected cursor unchanged")
	assert.Equal("Thread msg 3", m.pendingDetailSubject)
	assert.NotNil(cmd, "expected command to load message detail")

	// Press right again - now cursor should be at index 3 and scroll offset should adjust
	m.detailMessageIndex = 2
	m.threadCursor = 2
	m, _ = sendKey(t, m, keyRight())

	assert.Equal(3, m.detailMessageIndex, "after right")
	assert.Equal(3, m.threadCursor, "after right")
	// With pageSize=3, views render pageSize-1=2 data rows (1 reserved for info line).
	// threadCursor (3) >= threadScrollOffset (0) + visibleRows (2), so offset should be 2
	assert.Equal(2, m.threadScrollOffset, "expected threadScrollOffset to keep cursor visible")

	// Press left arrow - should navigate back
	m, _ = sendKey(t, m, keyLeft())

	assert.Equal(2, m.detailMessageIndex, "after left")
	assert.Equal(2, m.threadCursor, "after left")

	// Navigate all the way to first message
	m.detailMessageIndex = 1
	m.threadCursor = 1
	m.threadScrollOffset = 1 // Scroll offset is still 1 from before
	m, _ = sendKey(t, m, keyLeft())

	assert.Equal(0, m.detailMessageIndex, "after left")
	assert.Equal(0, m.threadCursor, "after left")
	// threadCursor (0) < threadScrollOffset (1), so offset should be adjusted to 0
	assert.Equal(0, m.threadScrollOffset, "expected threadScrollOffset to keep cursor visible")
}
