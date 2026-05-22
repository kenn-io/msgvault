package tui

import (
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"go.kenn.io/msgvault/internal/query"
)

// viewLevel represents the current navigation depth.
type viewLevel int

const (
	levelAggregates viewLevel = iota
	levelDrillDown            // Sub-grouping after drill-down
	levelMessageList
	levelMessageDetail
	levelThreadView // Thread/conversation view showing all messages in a thread
)

// viewState encapsulates the state for a specific view (cursor, sort, filters, data).
type viewState struct {
	level            viewLevel
	viewType         query.ViewType
	timeGranularity  query.TimeGranularity
	sortField        query.SortField
	sortDirection    query.SortDirection
	msgSortField     query.MessageSortField
	msgSortDirection query.SortDirection
	cursor           int
	scrollOffset     int
	filterKey        string              // The aggregate key used to filter messages
	allMessages      bool                // True when showing all messages (not filtered by aggregate)
	drillFilter      query.MessageFilter // Filter from parent drill-down
	drillViewType    query.ViewType      // ViewType that created the drill filter
	contextStats     *query.TotalStats   // Contextual stats for header display
	searchQuery      string              // Active search query (for aggregate filtering)
	searchFilter     query.MessageFilter // Context filter applied to search

	// Message list pagination
	msgListOffset      int  // Current offset for non-search message pagination
	msgListLoadingMore bool // True when loading additional message pages
	msgListComplete    bool // True when all pages have been loaded (no more data)

	// Data
	rows          []query.AggregateRow
	messages      []query.MessageSummary
	messageDetail *query.MessageDetail

	// Detail view specific
	detailScroll         int
	detailLineCount      int
	detailMessageIndex   int
	detailFromThread     bool
	pendingDetailSubject string

	// Detail search (find-in-page)
	detailSearchActive     bool
	detailSearchInput      textinput.Model
	detailSearchQuery      string
	detailSearchMatches    []int // Line indices with matches
	detailSearchMatchIndex int   // Current match index

	// Thread view specific
	threadConversationID int64
	threadMessages       []query.MessageSummary
	threadCursor         int
	threadScrollOffset   int
	threadTruncated      bool
}

// navigationSnapshot stores state for navigation history.
type navigationSnapshot struct {
	state viewState
}

// calculateScrollOffset computes the new scroll offset to keep cursor visible within pageSize.
func calculateScrollOffset(cursor, currentOffset, pageSize int) int {
	if cursor < currentOffset {
		return cursor
	}
	if cursor >= currentOffset+pageSize {
		return cursor - pageSize + 1
	}
	return currentOffset
}

// visibleRows returns the number of data rows visible in the viewport.
// Views reserve one row for the info/notification line, so this is pageSize-1.
func (m *Model) visibleRows() int {
	v := m.pageSize - 1
	if v < 1 {
		return 1
	}
	return v
}

func (m *Model) ensureThreadCursorVisible() {
	m.threadScrollOffset = calculateScrollOffset(m.threadCursor, m.threadScrollOffset, m.visibleRows())
}

func (m Model) navigateDetailPrev() (tea.Model, tea.Cmd) {
	return m.changeDetailMessage(-1)
}

func (m Model) navigateDetailNext() (tea.Model, tea.Cmd) {
	return m.changeDetailMessage(1)
}

// changeDetailMessage navigates to a different message in the detail view by delta offset.
func (m Model) changeDetailMessage(delta int) (tea.Model, tea.Cmd) {
	// Use thread messages if we entered from thread view, otherwise use list messages
	var msgs []query.MessageSummary
	if m.detailFromThread {
		msgs = m.threadMessages
	} else {
		msgs = m.messages
	}

	// Guard against empty message list
	if len(msgs) == 0 {
		return m.showFlash("No messages loaded")
	}

	// Clamp index if it's out of bounds (can happen if list changed)
	if m.detailMessageIndex >= len(msgs) {
		m.detailMessageIndex = len(msgs) - 1
	}

	newIndex := m.detailMessageIndex + delta
	if newIndex < 0 {
		return m.showFlash("At first message")
	}
	if newIndex >= len(msgs) {
		return m.showFlash("At last message")
	}

	m.detailMessageIndex = newIndex
	m.pendingDetailSubject = msgs[newIndex].Subject

	// Keep appropriate cursor in sync
	if m.detailFromThread {
		m.threadCursor = newIndex
		m.ensureThreadCursorVisible()
	} else {
		m.cursor = newIndex
	}

	// Reset view state for new message
	m.detailScroll = 0
	m.loading = true
	m.err = nil
	m.detailRequestID++

	return m, m.loadMessageDetail(msgs[newIndex].ID)
}

func (m Model) goBack() (tea.Model, tea.Cmd) {
	if len(m.breadcrumbs) == 0 {
		return m, nil
	}

	m.transitionBuffer = "" // Clear frozen view on navigation back

	// Pop breadcrumb
	bc := m.breadcrumbs[len(m.breadcrumbs)-1]
	m.breadcrumbs = m.breadcrumbs[:len(m.breadcrumbs)-1]

	// Track current cursor if we need to preserve it (e.g. from detail navigation)
	currentCursor := m.cursor
	currentLevel := m.level

	// Restore complete state from snapshot
	m.viewState = bc.state

	// Special case: returning from detail view to message list
	// The user might have navigated left/right in detail view.
	// If we simply restore `bc.state`, we revert to the cursor when we entered detail view.
	// We want to keep the NEW cursor if we just navigated.
	if currentLevel == levelMessageDetail && m.level == levelMessageList {
		m.cursor = currentCursor
		m.ensureCursorVisible()
	}

	m.err = nil       // Clear any stale error
	m.loading = false // Data is restored from snapshot

	// Reset loading-more flag: any in-flight pagination request from the
	// snapshotted view is stale (loadRequestID has changed), so clear the
	// flag to allow fresh pagination.
	m.msgListLoadingMore = false

	return m, nil
}

func (m *Model) ensureCursorVisible() {
	m.scrollOffset = calculateScrollOffset(m.cursor, m.scrollOffset, m.visibleRows())
}

func (m *Model) pushBreadcrumb() {
	m.breadcrumbs = append(m.breadcrumbs, navigationSnapshot{state: m.viewState})
}

func (m *Model) navigateList(key string, itemCount int) bool {
	changed := false

	switch key {
	case "up", "k":
		if m.cursor > 0 {
			m.cursor--
			changed = true
		}
	case "down", "j":
		if m.cursor < itemCount-1 {
			m.cursor++
			changed = true
		}
	case "pgup", "ctrl+u":
		step := m.visibleRows()
		m.cursor -= step
		m.scrollOffset -= step
		if m.cursor < 0 {
			m.cursor = 0
		}
		if m.scrollOffset < 0 {
			m.scrollOffset = 0
		}
		return true
	case "pgdown", "ctrl+d":
		step := m.visibleRows()
		m.cursor += step
		m.scrollOffset += step
		if m.cursor >= itemCount {
			m.cursor = itemCount - 1
		}
		if m.cursor < 0 {
			m.cursor = 0
		}
		// Clamp scroll so cursor stays visible and we don't scroll past the end
		maxScroll := itemCount - m.visibleRows()
		if maxScroll < 0 {
			maxScroll = 0
		}
		if m.scrollOffset > maxScroll {
			m.scrollOffset = maxScroll
		}
		return true
	case "home":
		m.cursor = 0
		m.scrollOffset = 0
		return true
	case "end", "G":
		m.cursor = itemCount - 1
		if m.cursor < 0 {
			m.cursor = 0
		}
		changed = true
	default:
		return false
	}

	if changed {
		m.ensureCursorVisible()
	}
	return true
}
