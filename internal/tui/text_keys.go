package tui

import (
	"strings"

	tea "charm.land/bubbletea/v2"
	"go.kenn.io/msgvault/internal/query"
)

// handleTextKeyPress dispatches key events when in Texts mode.
func (m Model) handleTextKeyPress(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	// Modal takes priority
	if m.modal != modalNone {
		return m.handleModalKeys(msg)
	}

	// Inline search takes priority over view keys
	if m.inlineSearchActive {
		return m.handleTextInlineSearchKeys(msg)
	}

	// Check global keys first (q, ?, m)
	newM, cmd, handled := m.handleGlobalKeys(msg)
	if handled {
		return newM, cmd
	}

	// Disable selection/deletion keys in Texts mode (read-only)
	switch msg.String() {
	case "space", "S", "d", "D", "x":
		return m, nil
	}

	switch m.textState.level {
	case textLevelConversations, textLevelAggregate,
		textLevelDrillConversations:
		return m.handleTextListKeys(msg)
	case textLevelTimeline:
		return m.handleTextTimelineKeys(msg)
	}
	return m, nil
}

// handleTextListKeys handles keys in text list views
// (conversations, aggregates, drill-down conversations).
func (m Model) handleTextListKeys(
	msg tea.KeyPressMsg,
) (tea.Model, tea.Cmd) {
	// Handle list navigation (text-specific: operates on textState)
	if m.navigateTextList(msg.String(), m.textRowCount()) {
		return m, nil
	}

	switch msg.String() {
	case "tab", "Tab":
		m.cycleTextViewType(true)
		m.loading = true
		cmd := m.loadTextData()
		return m, cmd

	case "shift+tab":
		m.cycleTextViewType(false)
		m.loading = true
		cmd := m.loadTextData()
		return m, cmd

	case keyNameEnter:
		return m.textDrillDown()

	case keyNameEsc, "backspace":
		return m.textGoBack()

	case "s":
		m.cycleTextSortField()
		m.loading = true
		cmd := m.loadTextData()
		return m, cmd

	case "r", "v":
		if m.textState.filter.SortDirection == query.SortDesc {
			m.textState.filter.SortDirection = query.SortAsc
		} else {
			m.textState.filter.SortDirection = query.SortDesc
		}
		m.loading = true
		cmd := m.loadTextData()
		return m, cmd

	case "t":
		m.textState.viewType = query.TextViewTime
		m.textState.level = textLevelAggregate
		m.textState.cursor = 0
		m.textState.scrollOffset = 0
		m.loading = true
		cmd := m.loadTextData()
		return m, cmd

	case "a":
		// Reset to conversations view (clear filters)
		m.textState = textState{
			viewType: query.TextViewConversations,
		}
		m.loading = true
		cmd := m.loadTextConversations()
		return m, cmd

	case "A":
		m.openAccountSelector()
		return m, nil

	case "/":
		return m, m.activateInlineSearch("search texts")
	}

	return m, nil
}

// handleTextTimelineKeys handles keys in the text timeline view.
func (m Model) handleTextTimelineKeys(
	msg tea.KeyPressMsg,
) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "r":
		// Reverse chronological order
		if m.textState.filter.SortDirection == query.SortAsc {
			m.textState.filter.SortDirection = query.SortDesc
		} else {
			m.textState.filter.SortDirection = query.SortAsc
		}
		m.textState.cursor = 0
		m.textState.scrollOffset = 0
		m.loading = true
		cmd := m.loadTextMessages()
		return m, cmd

	case "/":
		m.inlineSearchActive = true
		m.searchInput.Reset()
		m.searchInput.Focus()
		return m, nil

	case keyNameEsc, "backspace":
		return m.textGoBack()

	case "j", keyNameDown:
		m.textMoveCursor(1)
		return m, nil

	case "k", "up":
		m.textMoveCursor(-1)
		return m, nil

	case "pgup", "ctrl+u":
		step := m.visibleRows()
		m.textState.cursor -= step
		m.textState.scrollOffset -= step
		if m.textState.cursor < 0 {
			m.textState.cursor = 0
		}
		if m.textState.scrollOffset < 0 {
			m.textState.scrollOffset = 0
		}
		return m, nil

	case "pgdown", "ctrl+d":
		itemCount := m.textRowCount()
		step := m.visibleRows()
		m.textState.cursor += step
		m.textState.scrollOffset += step
		if m.textState.cursor >= itemCount {
			m.textState.cursor = itemCount - 1
		}
		if m.textState.cursor < 0 {
			m.textState.cursor = 0
		}
		maxScroll := max(itemCount-m.visibleRows(), 0)
		if m.textState.scrollOffset > maxScroll {
			m.textState.scrollOffset = maxScroll
		}
		return m, nil

	case "home":
		m.textState.cursor = 0
		m.textState.scrollOffset = 0
		return m, nil

	case "end", "G":
		maxIdx := max(m.textRowCount()-1, 0)
		m.textState.cursor = maxIdx
		return m, nil
	}
	return m, nil
}

// handleTextInlineSearchKeys handles keys when inline search is
// active in Texts mode. Enter commits the search; Esc cancels.
func (m Model) handleTextInlineSearchKeys(
	msg tea.KeyPressMsg,
) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case keyNameEnter:
		m.exitInlineSearchMode()
		queryStr := m.searchInput.Value()
		if queryStr == "" {
			return m, nil
		}
		// In timeline view, filter locally (messages already loaded
		// with full body text). In other views, use global FTS.
		if m.textState.level == textLevelTimeline {
			// Save unfiltered messages on first search so repeated
			// searches filter from the original set, not stacked results.
			if m.textState.unfilteredMessages == nil {
				m.textState.unfilteredMessages = m.textState.messages
				// Push breadcrumb only on first search to avoid stacking
				m.textState.breadcrumbs = append(
					m.textState.breadcrumbs,
					textNavSnapshot{
						level:          m.textState.level,
						viewType:       m.textState.viewType,
						cursor:         m.textState.cursor,
						scrollOffset:   m.textState.scrollOffset,
						filter:         m.textState.filter,
						selectedConvID: m.textState.selectedConvID,
					},
				)
			}
			source := m.textState.unfilteredMessages
			needle := strings.ToLower(queryStr)
			var filtered []query.MessageSummary
			for _, msg := range source {
				body := strings.ToLower(msg.BodyText)
				if body == "" {
					body = strings.ToLower(msg.Snippet)
				}
				sender := strings.ToLower(
					msg.FromName + " " + msg.FromPhone,
				)
				if strings.Contains(body, needle) ||
					strings.Contains(sender, needle) {
					filtered = append(filtered, msg)
				}
			}
			m.textState.messages = filtered
			m.textState.cursor = 0
			m.textState.scrollOffset = 0
			return m, nil
		}
		// Save current state so Esc can return from search results
		m.textState.breadcrumbs = append(
			m.textState.breadcrumbs,
			textNavSnapshot{
				level:          m.textState.level,
				viewType:       m.textState.viewType,
				cursor:         m.textState.cursor,
				scrollOffset:   m.textState.scrollOffset,
				filter:         m.textState.filter,
				selectedConvID: m.textState.selectedConvID,
			},
		)
		m.loading = true
		cmd := m.loadTextSearch(queryStr)
		return m, cmd

	case keyNameEsc:
		m.exitInlineSearchMode()
		m.searchInput.SetValue("")
		return m, nil

	case "ctrl+c":
		m.quitting = true
		return m, tea.Quit

	default:
		var cmd tea.Cmd
		m.searchInput, cmd = m.searchInput.Update(msg)
		return m, cmd
	}
}

// cycleTextViewType cycles through text view types.
func (m *Model) cycleTextViewType(forward bool) {
	if forward {
		m.textState.viewType++
		if m.textState.viewType >= query.TextViewTypeCount {
			m.textState.viewType = 0
		}
	} else {
		if m.textState.viewType == 0 {
			m.textState.viewType = query.TextViewTypeCount - 1
		} else {
			m.textState.viewType--
		}
	}
	if m.textState.viewType == query.TextViewConversations {
		m.textState.level = textLevelConversations
		m.textState.filter.SortField = query.TextSortByLastMessage
	} else {
		m.textState.level = textLevelAggregate
		m.textState.filter.SortField = query.TextSortByCount
	}
	m.textState.cursor = 0
	m.textState.scrollOffset = 0
}

// textMoveCursor moves the cursor by delta and adjusts scroll offset.
func (m *Model) textMoveCursor(delta int) {
	m.textState.cursor += delta
	maxIdx := max(m.textRowCount()-1, 0)
	if m.textState.cursor < 0 {
		m.textState.cursor = 0
	}
	if m.textState.cursor > maxIdx {
		m.textState.cursor = maxIdx
	}
	m.textState.scrollOffset = calculateScrollOffset(
		m.textState.cursor,
		m.textState.scrollOffset,
		m.visibleRows(),
	)
}

// navigateTextList handles list navigation keys for text mode,
// operating on textState.cursor and textState.scrollOffset instead
// of the email-mode viewState fields.
// Returns true if the key was handled.
func (m *Model) navigateTextList(key string, itemCount int) bool {
	switch key {
	case "up", "k":
		if m.textState.cursor > 0 {
			m.textState.cursor--
			m.textState.scrollOffset = calculateScrollOffset(
				m.textState.cursor,
				m.textState.scrollOffset,
				m.visibleRows(),
			)
		}
		return true
	case keyNameDown, "j":
		if m.textState.cursor < itemCount-1 {
			m.textState.cursor++
			m.textState.scrollOffset = calculateScrollOffset(
				m.textState.cursor,
				m.textState.scrollOffset,
				m.visibleRows(),
			)
		}
		return true
	case "pgup", "ctrl+u":
		step := m.visibleRows()
		m.textState.cursor -= step
		m.textState.scrollOffset -= step
		if m.textState.cursor < 0 {
			m.textState.cursor = 0
		}
		if m.textState.scrollOffset < 0 {
			m.textState.scrollOffset = 0
		}
		return true
	case "pgdown", "ctrl+d":
		step := m.visibleRows()
		m.textState.cursor += step
		m.textState.scrollOffset += step
		if m.textState.cursor >= itemCount {
			m.textState.cursor = itemCount - 1
		}
		if m.textState.cursor < 0 {
			m.textState.cursor = 0
		}
		maxScroll := max(itemCount-m.visibleRows(), 0)
		if m.textState.scrollOffset > maxScroll {
			m.textState.scrollOffset = maxScroll
		}
		return true
	case "home":
		m.textState.cursor = 0
		m.textState.scrollOffset = 0
		return true
	case "end", "G":
		maxIdx := max(itemCount-1, 0)
		m.textState.cursor = maxIdx
		m.textState.scrollOffset = calculateScrollOffset(
			m.textState.cursor,
			m.textState.scrollOffset,
			m.visibleRows(),
		)
		return true
	default:
		return false
	}
}

// textRowCount returns the number of rows in the current text view.
func (m Model) textRowCount() int {
	switch m.textState.level {
	case textLevelConversations, textLevelDrillConversations:
		return len(m.textState.conversations)
	case textLevelAggregate:
		return len(m.textState.aggregateRows)
	case textLevelTimeline:
		return len(m.textState.messages)
	}
	return 0
}

// cycleTextSortField cycles between sort fields for text views.
// Conversations: Name → Count → LastMessage (3 columns).
// Aggregates: Name → Count only (no LastMessage column).
func (m *Model) cycleTextSortField() {
	isConv := m.textState.level == textLevelConversations ||
		m.textState.level == textLevelDrillConversations

	switch m.textState.filter.SortField {
	case query.TextSortByName:
		m.textState.filter.SortField = query.TextSortByCount
	case query.TextSortByCount:
		if isConv {
			m.textState.filter.SortField = query.TextSortByLastMessage
		} else {
			m.textState.filter.SortField = query.TextSortByName
		}
	default:
		m.textState.filter.SortField = query.TextSortByName
	}
}

// textDrillDown enters the selected item in text mode.
func (m Model) textDrillDown() (tea.Model, tea.Cmd) {
	switch m.textState.level {
	case textLevelConversations, textLevelDrillConversations:
		if m.textState.cursor >= len(m.textState.conversations) {
			return m, nil
		}
		conv := m.textState.conversations[m.textState.cursor]
		m.textState.breadcrumbs = append(
			m.textState.breadcrumbs,
			textNavSnapshot{
				level:          m.textState.level,
				viewType:       m.textState.viewType,
				cursor:         m.textState.cursor,
				scrollOffset:   m.textState.scrollOffset,
				filter:         m.textState.filter,
				selectedConvID: m.textState.selectedConvID,
			},
		)
		m.textState.selectedConvID = conv.ConversationID
		m.textState.level = textLevelTimeline
		m.textState.cursor = 0
		m.textState.scrollOffset = 0
		m.loading = true
		cmd := m.loadTextMessages()
		return m, cmd

	case textLevelAggregate:
		if m.textState.cursor >= len(m.textState.aggregateRows) {
			return m, nil
		}
		row := m.textState.aggregateRows[m.textState.cursor]
		m.textState.breadcrumbs = append(
			m.textState.breadcrumbs,
			textNavSnapshot{
				level:          m.textState.level,
				viewType:       m.textState.viewType,
				cursor:         m.textState.cursor,
				scrollOffset:   m.textState.scrollOffset,
				filter:         m.textState.filter,
				selectedConvID: m.textState.selectedConvID,
			},
		)
		// Apply aggregate filter and drill to conversations
		switch m.textState.viewType {
		case query.TextViewContacts:
			m.textState.filter.ContactPhone = row.Key
		case query.TextViewContactNames:
			m.textState.filter.ContactName = row.Key
		case query.TextViewSources:
			m.textState.filter.SourceType = row.Key
		case query.TextViewLabels:
			m.textState.filter.Label = row.Key
		case query.TextViewConversations, query.TextViewTime, query.TextViewTypeCount:
			// Not an aggregate drill source.
		}
		m.textState.level = textLevelDrillConversations
		m.textState.cursor = 0
		m.textState.scrollOffset = 0
		m.loading = true
		cmd := m.loadTextConversations()
		return m, cmd

	case textLevelTimeline:
		// Drill-down doesn't fire from the timeline level (no children).
	}
	return m, nil
}

// textGoBack returns to the previous text navigation state.
func (m Model) textGoBack() (tea.Model, tea.Cmd) {
	// If we have unfiltered messages (from a timeline search), restore
	// them directly without reloading. This is instant and avoids
	// re-querying the database.
	if m.textState.unfilteredMessages != nil {
		m.textState.messages = m.textState.unfilteredMessages
		m.textState.unfilteredMessages = nil
		// Pop the search breadcrumb
		if len(m.textState.breadcrumbs) > 0 {
			snap := m.textState.breadcrumbs[len(m.textState.breadcrumbs)-1]
			m.textState.breadcrumbs = m.textState.breadcrumbs[:len(m.textState.breadcrumbs)-1]
			m.textState.cursor = snap.cursor
			m.textState.scrollOffset = snap.scrollOffset
		} else {
			m.textState.cursor = 0
			m.textState.scrollOffset = 0
		}
		return m, nil
	}
	if len(m.textState.breadcrumbs) == 0 {
		return m, nil
	}
	snap := m.textState.breadcrumbs[len(m.textState.breadcrumbs)-1]
	m.textState.breadcrumbs = m.textState.breadcrumbs[:len(m.textState.breadcrumbs)-1]
	m.textState.level = snap.level
	m.textState.viewType = snap.viewType
	m.textState.cursor = snap.cursor
	m.textState.scrollOffset = snap.scrollOffset
	m.textState.filter = snap.filter
	m.textState.selectedConvID = snap.selectedConvID
	m.loading = true
	cmd := m.loadTextData()
	return m, cmd
}
