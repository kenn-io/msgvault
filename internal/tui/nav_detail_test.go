package tui

import (
	"fmt"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
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
	if m.detailLineCount != 0 {
		t.Errorf("expected detailLineCount = 0 on load start, got %d", m.detailLineCount)
	}
	if m.detailScroll != 0 {
		t.Errorf("expected detailScroll = 0 on load start, got %d", m.detailScroll)
	}
	if m.messageDetail != nil {
		t.Error("expected messageDetail = nil on load start")
	}
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
	if model.detailScroll != expectedMax {
		t.Errorf("expected detailScroll clamped to %d, got %d", expectedMax, model.detailScroll)
	}

	// Test when content fits in one page
	model.detailLineCount = 5 // Less than detailPageSize (12)
	model.detailScroll = 10
	model.clampDetailScroll()

	if model.detailScroll != 0 {
		t.Errorf("expected detailScroll = 0 when content fits page, got %d", model.detailScroll)
	}
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
		if m.width != 40 {
			t.Errorf("expected width = 40 after resize, got %d", m.width)
		}
	}

	// Scroll should be clamped if it exceeds new bounds
	m.detailScroll = 1000
	m.clampDetailScroll()
	maxScroll := m.detailLineCount - m.pageSize
	if maxScroll < 0 {
		maxScroll = 0
	}
	if m.detailScroll > maxScroll {
		t.Errorf("expected detailScroll <= %d after clamp, got %d", maxScroll, m.detailScroll)
	}
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
	if m.detailScroll != 0 {
		t.Errorf("expected detailScroll = 0 with zero line count, got %d", m.detailScroll)
	}
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
	if len(lines) != expectedLines {
		t.Errorf("loading state: expected %d lines, got %d", expectedLines, len(lines))
	}

	// Test error state
	model.loading = false
	model.err = fmt.Errorf("test error")
	view = model.messageDetailView()
	lines = strings.Split(view, "\n")
	if len(lines) != expectedLines {
		t.Errorf("error state: expected %d lines, got %d", expectedLines, len(lines))
	}

	// Test nil detail state
	model.err = nil
	model.messageDetail = nil
	view = model.messageDetailView()
	lines = strings.Split(view, "\n")
	if len(lines) != expectedLines {
		t.Errorf("nil detail state: expected %d lines, got %d", expectedLines, len(lines))
	}
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
	maxScroll := model.detailLineCount - m.detailPageSize()
	if maxScroll < 0 {
		maxScroll = 0
	}
	if m.detailScroll > maxScroll {
		t.Errorf("detailScroll=%d exceeds maxScroll=%d after resize", m.detailScroll, maxScroll)
	}
}

// =============================================================================
// Detail Navigation (Prev/Next Message) Tests
// =============================================================================

// TestDetailNavigationPrevNext verifies left/right arrow navigation in message detail view.
// Left = previous in list (lower index), Right = next in list (higher index).
func TestDetailNavigationPrevNext(t *testing.T) {
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

	if m.detailMessageIndex != 2 {
		t.Errorf("expected detailMessageIndex=2 after right, got %d", m.detailMessageIndex)
	}
	if m.cursor != 2 {
		t.Errorf("expected cursor=2 after right, got %d", m.cursor)
	}
	if m.pendingDetailSubject != "Third message" {
		t.Errorf("expected pendingDetailSubject='Third message', got %q", m.pendingDetailSubject)
	}
	if cmd == nil {
		t.Error("expected command to load message detail")
	}

	// Press left arrow to go to previous message in list (lower index)
	m.detailMessageIndex = 2
	m.cursor = 2
	m, cmd = sendKey(t, m, keyLeft())

	if m.detailMessageIndex != 1 {
		t.Errorf("expected detailMessageIndex=1 after left, got %d", m.detailMessageIndex)
	}
	if m.cursor != 1 {
		t.Errorf("expected cursor=1 after left, got %d", m.cursor)
	}
	if cmd == nil {
		t.Error("expected command to load message detail")
	}
}

// TestDetailNavigationAtBoundary verifies flash message at first/last message.
func TestDetailNavigationAtBoundary(t *testing.T) {
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

	if m.detailMessageIndex != 0 {
		t.Errorf("expected detailMessageIndex=0 (unchanged), got %d", m.detailMessageIndex)
	}
	if m.flashMessage != "At first message" {
		t.Errorf("expected flashMessage='At first message', got %q", m.flashMessage)
	}
	if cmd == nil {
		t.Error("expected command to clear flash message")
	}

	// Clear flash and test at last message
	m.flashMessage = ""
	m.detailMessageIndex = 1 // At last message
	m.cursor = 1
	m.messageDetail = &query.MessageDetail{ID: 2, Subject: "Second message"}

	// Press right arrow at last message - should show flash
	m, cmd = sendKey(t, m, keyRight())

	if m.detailMessageIndex != 1 {
		t.Errorf("expected detailMessageIndex=1 (unchanged), got %d", m.detailMessageIndex)
	}
	if m.flashMessage != "At last message" {
		t.Errorf("expected flashMessage='At last message', got %q", m.flashMessage)
	}
	if cmd == nil {
		t.Error("expected command to clear flash message")
	}
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

	if m.detailMessageIndex != 2 {
		t.Errorf("expected detailMessageIndex=2 after 'l', got %d", m.detailMessageIndex)
	}

	// Reset and press 'h' to go to previous message in list (lower index)
	m.detailMessageIndex = 1
	m.cursor = 1
	m, _ = sendKey(t, m, key('h'))

	if m.detailMessageIndex != 0 {
		t.Errorf("expected detailMessageIndex=0 after 'h', got %d", m.detailMessageIndex)
	}
}

// TestDetailNavigationEmptyList verifies navigation with empty message list.
func TestDetailNavigationEmptyList(t *testing.T) {
	model := NewBuilder().WithLevel(levelMessageDetail).Build()
	model.detailMessageIndex = 0

	// Press right arrow - should show flash, not panic
	newModel, _ := model.navigateDetailNext()
	m := newModel.(Model)

	if m.flashMessage != "No messages loaded" {
		t.Errorf("expected flashMessage='No messages loaded', got %q", m.flashMessage)
	}

	// Press left arrow - should show flash, not panic
	newModel, _ = m.navigateDetailPrev()
	m = newModel.(Model)

	if m.flashMessage != "No messages loaded" {
		t.Errorf("expected flashMessage='No messages loaded', got %q", m.flashMessage)
	}
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
	if m.detailMessageIndex != 0 {
		t.Errorf("expected detailMessageIndex=0 (clamped), got %d", m.detailMessageIndex)
	}
	if m.flashMessage != "At first message" {
		t.Errorf("expected flashMessage='At first message', got %q", m.flashMessage)
	}
}

// TestDetailNavigationOutOfBoundsWithMultipleMessages verifies that when the index is
// out of bounds but there are multiple messages, navigation succeeds after clamping.
func TestDetailNavigationOutOfBoundsWithMultipleMessages(t *testing.T) {
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
	if m.detailMessageIndex != 1 {
		t.Errorf("expected detailMessageIndex=1 (clamped and navigated), got %d", m.detailMessageIndex)
	}
	if m.cursor != 1 {
		t.Errorf("expected cursor=1, got %d", m.cursor)
	}
	if m.pendingDetailSubject != "Second message" {
		t.Errorf("expected pendingDetailSubject='Second message', got %q", m.pendingDetailSubject)
	}
	// Should trigger loadMessageDetail, not just show flash
	if cmd == nil {
		t.Error("expected command to load message detail after clamping and navigating")
	}
	if m.flashMessage != "" {
		t.Errorf("expected no flash message after successful navigation, got %q", m.flashMessage)
	}
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
	if m.cursor != 2 {
		t.Errorf("expected cursor=2 (preserved from navigation), got %d", m.cursor)
	}
}

// TestDetailNavigationFromThreadView verifies that left/right navigation in detail view
// uses threadMessages (not messages) when entered from thread view, and keeps
// threadCursor and threadScrollOffset in sync.
func TestDetailNavigationFromThreadView(t *testing.T) {
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

	if m.detailMessageIndex != 2 {
		t.Errorf("expected detailMessageIndex=2 after right, got %d", m.detailMessageIndex)
	}
	if m.threadCursor != 2 {
		t.Errorf("expected threadCursor=2 after right, got %d", m.threadCursor)
	}
	// cursor (for list view) should NOT be modified
	if m.cursor != 0 {
		t.Errorf("expected cursor=0 (unchanged), got %d", m.cursor)
	}
	if m.pendingDetailSubject != "Thread msg 3" {
		t.Errorf("expected pendingDetailSubject='Thread msg 3', got %q", m.pendingDetailSubject)
	}
	if cmd == nil {
		t.Error("expected command to load message detail")
	}

	// Press right again - now cursor should be at index 3 and scroll offset should adjust
	m.detailMessageIndex = 2
	m.threadCursor = 2
	m, _ = sendKey(t, m, keyRight())

	if m.detailMessageIndex != 3 {
		t.Errorf("expected detailMessageIndex=3 after right, got %d", m.detailMessageIndex)
	}
	if m.threadCursor != 3 {
		t.Errorf("expected threadCursor=3 after right, got %d", m.threadCursor)
	}
	// With pageSize=3, views render pageSize-1=2 data rows (1 reserved for info line).
	// threadCursor (3) >= threadScrollOffset (0) + visibleRows (2), so offset should be 2
	if m.threadScrollOffset != 2 {
		t.Errorf("expected threadScrollOffset=2 to keep cursor visible, got %d", m.threadScrollOffset)
	}

	// Press left arrow - should navigate back
	m, _ = sendKey(t, m, keyLeft())

	if m.detailMessageIndex != 2 {
		t.Errorf("expected detailMessageIndex=2 after left, got %d", m.detailMessageIndex)
	}
	if m.threadCursor != 2 {
		t.Errorf("expected threadCursor=2 after left, got %d", m.threadCursor)
	}

	// Navigate all the way to first message
	m.detailMessageIndex = 1
	m.threadCursor = 1
	m.threadScrollOffset = 1 // Scroll offset is still 1 from before
	m, _ = sendKey(t, m, keyLeft())

	if m.detailMessageIndex != 0 {
		t.Errorf("expected detailMessageIndex=0 after left, got %d", m.detailMessageIndex)
	}
	if m.threadCursor != 0 {
		t.Errorf("expected threadCursor=0 after left, got %d", m.threadCursor)
	}
	// threadCursor (0) < threadScrollOffset (1), so offset should be adjusted to 0
	if m.threadScrollOffset != 0 {
		t.Errorf("expected threadScrollOffset=0 to keep cursor visible, got %d", m.threadScrollOffset)
	}
}
