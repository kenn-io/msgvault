package tui

import (
	"time"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"go.kenn.io/msgvault/internal/query"
)

// Key names matched against tea.KeyPressMsg.String() in the key-handling switches.
const (
	keyNameEnter = "enter"
	keyNameEsc   = "esc"
	keyNameDown  = "down"
)

// handleInlineSearchKeys handles keys when inline search bar is active.
func (m Model) handleInlineSearchKeys(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case keyNameEnter:
		return m.commitInlineSearch()

	case keyNameEsc:
		return m.cancelInlineSearch()

	case "ctrl+c":
		m.quitting = true
		return m, tea.Quit

	case "tab":
		// Toggle search mode — only meaningful at message list level
		// where Fast (Parquet metadata) and Deep (FTS5 body) differ.
		// At aggregate level, both modes run the same query.
		if m.level != levelMessageList {
			return m, nil
		}
		if m.searchMode == searchModeFast {
			m.searchMode = searchModeDeep
			m.searchInput.Placeholder = "search (Tab: fast)"
		} else {
			m.searchMode = searchModeFast
			m.searchInput.Placeholder = "search (Tab: deep)"
		}
		m.inlineSearchDebounce++
		if query := m.searchInput.Value(); query != "" {
			m.searchQuery = query
			m.inlineSearchLoading = true
			spinCmd := m.startSpinner()
			m.searchFilter = m.drillFilter
			m.searchFilter.SourceID = m.accountFilter
			m.searchFilter.WithAttachmentsOnly = m.filters.attachmentsOnly
			m.searchFilter.HideDeletedFromSource = m.filters.hideDeletedFromSource
			m.searchRequestID++
			return m, tea.Batch(spinCmd, m.loadSearch(query))
		}
		return m, nil

	default:
		// Pass key to text input
		var cmd tea.Cmd
		m.searchInput, cmd = m.searchInput.Update(msg)

		// Trigger debounced search (Fast: 100ms, Deep: 500ms)
		query := m.searchInput.Value()
		m.inlineSearchDebounce++
		debounceID := m.inlineSearchDebounce

		delay := inlineSearchDebounceDelay
		if m.searchMode == searchModeDeep {
			delay = deepSearchDebounceDelay
		}

		// Show loading spinner immediately while waiting for debounce
		var spinCmd tea.Cmd
		if query != "" {
			m.inlineSearchLoading = true
			spinCmd = m.startSpinner()
		} else {
			m.inlineSearchLoading = false
		}

		debounceCmd := tea.Tick(delay, func(t time.Time) tea.Msg {
			return searchDebounceMsg{query: query, debounceID: debounceID}
		})

		return m, tea.Batch(cmd, spinCmd, debounceCmd)
	}
}

// handleGlobalKeys handles keys common to all views (quit, help, mode toggle).
// Returns (model, cmd, true) if the key was handled, or (model, nil, false) otherwise.
func (m Model) handleGlobalKeys(msg tea.KeyPressMsg) (Model, tea.Cmd, bool) {
	switch msg.String() {
	case "q":
		m.modal = modalQuitConfirm
		return m, nil, true
	case "ctrl+c":
		m.quitting = true
		return m, tea.Quit, true
	case "?":
		m.modal = modalHelp
		return m, nil, true
	case "m":
		m.presentationGeneration++
		m.mode = nextMode(m.mode, m.textEngine != nil)
		// A frozen view and the email search loading flags describe the mode
		// being left. Do not let them obscure or animate the destination mode.
		m.transitionBuffer = ""
		m.inlineSearchLoading = false
		m.searchLoadingMore = false
		switch m.mode {
		case modeTexts:
			m.textState.filter.SourceID = m.accountFilter
			m.loading = true
			spinCmd := m.startSpinner()
			loadCmd := m.loadTextConversations()
			return m, tea.Batch(spinCmd, loadCmd), true
		case modeMeetings:
			m.meetingState.listLoading = false
			m.meetingState.searchLoading = false
			m.meetingState.detailLoading = false
			if m.meetingState.initialized {
				m.loading = false
				return m, nil, true
			}
			m.loading = true
			m.meetingState.requestID++
			m.meetingState.listLoading = true
			m.meetingState.preSearch = nil
			m.meetingState.searchSnapshotInvalid = true
			spinCmd := m.startSpinner()
			return m, tea.Batch(spinCmd, m.loadMeetingMessages()), true
		default:
			m.loading = true
			m.aggregateRequestID++
			spinCmd := m.startSpinner()
			return m, tea.Batch(spinCmd, m.loadData(), m.loadStats()), true
		}
	}
	return m, nil, false
}

// handleAggregateKeys handles keys in the aggregate and sub-aggregate views.
func (m Model) handleAggregateKeys(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	isSub := m.level == levelDrillDown

	// Handle global keys (quit, help)
	if m2, cmd, handled := m.handleGlobalKeys(msg); handled {
		return m2, cmd
	}

	// Handle list navigation
	if m.navigateList(msg.String(), len(m.rows)) {
		return m, nil
	}

	switch msg.String() {
	// Esc: sub-agg tries goBack() first; top-level clears search
	case keyNameEsc:
		if isSub {
			if len(m.breadcrumbs) > 0 {
				return m.goBack()
			}
			if m.searchQuery != "" {
				m.searchQuery = ""
				m.contextStats = nil
				m.searchInput.SetValue("")
				m.aggregateRequestID++
				return m, m.loadData()
			}
			return m.goBack()
		}
		// Top-level: clear search filter
		if m.searchQuery != "" {
			m.searchQuery = ""
			m.contextStats = nil
			m.searchInput.SetValue("")
			m.aggregateRequestID++
			return m, m.loadData()
		}

	// Account selector
	case "A":
		m.openAccountSelector()
		return m, nil

	// Attachment filter
	case "f":
		m.openFilterModal()
		return m, nil

	// Search - activate inline search bar
	case "/":
		return m, m.activateInlineSearch("search")

	// Selection
	case "space": // Space to toggle selection
		m.toggleAggregateSelection()

	case "S": // Select all visible
		m.selectVisibleAggregates()

	case "x": // Clear selection
		m.clearAllSelections()

	case "a": // Jump to all messages view
		m.transitionBuffer = m.renderView() // Freeze screen until data loads
		m.pushBreadcrumb()
		// Top-level: allMessages=true (no filter); sub-agg: allMessages=false (preserve drill filter)
		m.allMessages = !isSub
		if !isSub {
			m.filterKey = ""
		}
		m.level = levelMessageList
		m.cursor = 0
		m.scrollOffset = 0
		m.messages = nil // Clear stale messages from previous view
		m.msgListOffset = 0
		m.msgListLoadingMore = false
		m.msgListComplete = false
		m.loading = true
		m.err = nil

		// If there's an active search query, show search results instead of all messages
		if m.searchQuery != "" {
			m.searchFilter = m.drillFilter
			m.searchFilter.SourceID = m.accountFilter
			m.searchFilter.WithAttachmentsOnly = m.filters.attachmentsOnly
			m.searchFilter.HideDeletedFromSource = m.filters.hideDeletedFromSource
			m.loadRequestID++ // Invalidate stale loadMessages responses
			m.searchRequestID++
			return m, m.loadSearch(m.searchQuery)
		}
		m.loadRequestID++
		return m, m.loadMessages()

	case "d", "D": // Stage for deletion (selection or current row)
		if !m.hasSelection() && len(m.rows) > 0 && m.cursor < len(m.rows) {
			// No selection - select current row first
			m.selection.aggregateKeys[m.rows[m.cursor].Key] = true
		}
		return m.stageForDeletion()

	// Drill down - go to message list for selected aggregate
	case keyNameEnter:
		if len(m.rows) > 0 && m.cursor < len(m.rows) {
			return m.enterDrillDown(m.rows[m.cursor])
		}

	// View switching - 'g' cycles through groupings, Tab also works
	// Sub-agg skips the drill view type (can't sub-group by the same dimension)
	case "g", "tab":
		skipView := query.ViewType(-1)
		if isSub {
			skipView = m.drillViewType
		}
		m.cycleViewType(true, skipView)
		m.resetViewState()
		m.loading = true
		m.aggregateRequestID++
		return m, m.loadData()

	case "shift+tab":
		skipView := query.ViewType(-1)
		if isSub {
			skipView = m.drillViewType
		}
		m.cycleViewType(false, skipView)
		m.resetViewState()
		m.loading = true
		m.aggregateRequestID++
		return m, m.loadData()

	// Time view: jump to Time view, or cycle granularity if already there
	case "t":
		if m.viewType == query.ViewTime {
			m.timeGranularity = (m.timeGranularity + 1) % query.TimeGranularityCount
		} else if isSub && m.drillViewType == query.ViewTime {
			// Can't sub-aggregate by the same dimension we drilled from
			return m, nil
		} else {
			m.viewType = query.ViewTime
			m.resetViewState()
		}
		m.loading = true
		m.aggregateRequestID++
		return m, m.loadData()

	// Sorting
	case "s":
		m.sortField = (m.sortField + 1) % 4
		m.loading = true
		m.aggregateRequestID++
		return m, m.loadData()

	case "r", "v":
		if m.sortDirection == query.SortDesc {
			m.sortDirection = query.SortAsc
		} else {
			m.sortDirection = query.SortDesc
		}
		m.loading = true
		m.aggregateRequestID++
		return m, m.loadData()
	}

	return m, nil
}

// nextSubGroupView returns the next logical sub-group view type.
// Skips the "Name" variant when drilling from the corresponding email view,
// since an email address almost always maps to exactly one display name.
// The reverse (Name → email) is kept because one name can have multiple emails.
func (m Model) nextSubGroupView(current query.ViewType) query.ViewType {
	switch current {
	case query.ViewSenders:
		return query.ViewRecipients // skip SenderNames (redundant from email drill)
	case query.ViewSenderNames:
		return query.ViewRecipients
	case query.ViewRecipients:
		return query.ViewDomains // skip RecipientNames (redundant from email drill)
	case query.ViewRecipientNames:
		return query.ViewDomains
	case query.ViewDomains:
		return query.ViewLabels
	case query.ViewLabels:
		return query.ViewTime
	case query.ViewTime:
		return query.ViewSenders
	default:
		return query.ViewRecipients
	}
}

// handleMessageListKeys handles keys in the message list view.
func (m Model) handleMessageListKeys(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	// Handle global keys (quit, help)
	if m2, cmd, handled := m.handleGlobalKeys(msg); handled {
		return m2, cmd
	}

	// Handle list navigation.
	// NOTE: Unlike handleAggregateKeys, we check for search pagination
	// between navigation and the early return. This is because navigation keys
	// (even when cursor is clamped at the boundary) may need to trigger loading
	// more search results. navigateList only returns true for navigation keys
	// (up/down/j/k/pgup/pgdown/home/end/G), so handled=true is safe to use
	// as the gate for pagination checks.
	handled := m.navigateList(msg.String(), len(m.messages))

	// Check if we need to load more deep search results after pgdown
	key := msg.String()
	if (key == "pgdown" || key == "ctrl+d") &&
		m.searchQuery != "" && m.searchMode == searchModeDeep &&
		m.searchTotalCount == -1 && !m.searchLoadingMore && !m.loading &&
		m.cursor >= len(m.messages)-1 && len(m.messages) > 0 {
		m.searchLoadingMore = true
		m.searchRequestID++
		spinCmd := m.startSpinner()
		return m, tea.Batch(spinCmd, m.loadSearchWithOffset(m.searchQuery, m.searchOffset, true))
	}

	if handled {
		// Check if navigation requires loading more results (search or list).
		// This fires even when cursor is clamped at the boundary (e.g., pressing
		// down at the last loaded item) so pagination can still trigger.
		if cmd := m.maybeLoadMoreSearchResults(); cmd != nil {
			return m, cmd
		}
		if cmd := m.maybeLoadMoreMessages(); cmd != nil {
			return m, cmd
		}
		return m, nil
	}

	switch msg.String() {
	// Back - clear inner search first, then navigate back
	case keyNameEsc:
		// Clear search only if it was initiated at this level (snapshot exists).
		// Inherited search (from aggregate drill-down) has no snapshot —
		// goBack restores the parent view with its search intact.
		if m.searchQuery != "" && m.preSearchMessages != nil {
			return m.clearMessageListSearch()
		}
		// Invalidate in-flight search responses so they don't write
		// stale message data into the restored parent view.
		m.searchRequestID++
		return m.goBack()

	// Selection
	case "space": // Space to toggle selection
		if len(m.messages) > 0 && m.cursor < len(m.messages) {
			id := m.messages[m.cursor].ID
			if m.selection.messageIDs[id] {
				delete(m.selection.messageIDs, id)
			} else {
				m.selection.messageIDs[id] = true
			}
		}

	case "S": // Select all visible (only on-screen items)
		endRow := min(m.scrollOffset+m.pageSize, len(m.messages))
		for i := m.scrollOffset; i < endRow; i++ {
			m.selection.messageIDs[m.messages[i].ID] = true
		}

	case "x": // Clear selection
		m.clearAllSelections()

	case "d", "D": // Stage for deletion (selection or current row)
		if !m.hasSelection() && len(m.messages) > 0 && m.cursor < len(m.messages) {
			// No selection - select current row first
			m.selection.messageIDs[m.messages[m.cursor].ID] = true
		}
		return m.stageForDeletion()

	// Attachment filter
	case "f":
		m.openFilterModal()
		return m, nil

	// Search - activate inline search bar
	case "/":
		return m, m.activateInlineSearch("search (Tab: deep)")

	// Sub-grouping: switch to aggregate breakdown within current filter
	case "tab":
		if m.hasDrillFilter() {
			m.transitionBuffer = m.renderView() // Freeze screen until data loads

			// Save current state to breadcrumb (including viewType for proper restoration)
			m.pushBreadcrumb()

			// Switch to sub-aggregate view
			m.level = levelDrillDown
			m.viewType = m.nextSubGroupView(m.drillViewType)
			m.cursor = 0
			m.scrollOffset = 0
			m.rows = nil // Clear stale rows from previous view
			m.loading = true
			m.err = nil
			m.selection.aggregateKeys = make(map[string]bool)
			m.selection.aggregateViewType = m.viewType
			m.aggregateRequestID++
			return m, m.loadData()
		}

	// Drill down to message detail
	case keyNameEnter:
		if len(m.messages) > 0 && m.cursor < len(m.messages) {
			m.transitionBuffer = m.renderView() // Freeze screen until data loads

			// Save current state (include all fields for proper restoration)
			m.pushBreadcrumb()

			// Store pending subject for breadcrumb while loading
			m.pendingDetailSubject = m.messages[m.cursor].Subject
			m.detailMessageIndex = m.cursor // Track which message we're viewing
			m.detailFromThread = false      // Navigate within list messages
			m.messageDetail = nil           // Clear stale detail
			m.detailLineCount = 0           // Reset line count to avoid stale N/M display
			m.detailScroll = 0              // Reset scroll position
			m.level = levelMessageDetail
			m.loading = true
			m.err = nil         // Clear any previous error
			m.detailRequestID++ // Increment to invalidate stale responses
			return m, m.loadMessageDetail(m.messages[m.cursor].ID)
		}

	// Time sub-grouping: jump directly to sub-aggregate Time view
	case "t":
		if m.hasDrillFilter() && m.drillViewType != query.ViewTime {
			m.transitionBuffer = m.renderView()
			m.pushBreadcrumb()
			m.level = levelDrillDown
			m.viewType = query.ViewTime
			m.cursor = 0
			m.scrollOffset = 0
			m.rows = nil
			m.loading = true
			m.err = nil
			m.selection.aggregateKeys = make(map[string]bool)
			m.selection.aggregateViewType = m.viewType
			m.aggregateRequestID++
			return m, m.loadData()
		}

	// Sub-grouping: 'g' switches to aggregate breakdown within current filter (like tab)
	case "g":
		m.transitionBuffer = m.renderView() // Freeze screen until data loads
		if m.hasDrillFilter() {
			// Save current state to breadcrumb (including viewType for proper restoration)
			m.pushBreadcrumb()

			// Switch to sub-aggregate view
			m.level = levelDrillDown
			m.viewType = m.nextSubGroupView(m.drillViewType)
			m.cursor = 0
			m.scrollOffset = 0
			m.rows = nil // Clear stale rows from previous view
			m.loading = true
			m.err = nil
			m.selection.aggregateKeys = make(map[string]bool)
			m.selection.aggregateViewType = m.viewType
			m.aggregateRequestID++
			return m, m.loadData()
		}
		// No drill filter (e.g., All Messages or search results) - go back to aggregate view
		// Clear search state (mirroring goBack behavior)
		m.searchQuery = ""
		m.searchFilter = query.MessageFilter{}
		m.contextStats = nil
		m.level = levelAggregates
		m.cursor = 0
		m.scrollOffset = 0
		m.rows = nil // Clear stale rows from previous view
		m.loading = true
		m.err = nil
		m.aggregateRequestID++
		return m, m.loadData()

	// Sorting - use explicit field list to avoid hidden coupling
	case "s":
		msgSortFields := []query.MessageSortField{
			query.MessageSortByDate,
			query.MessageSortBySize,
			query.MessageSortBySubject,
		}
		for i, f := range msgSortFields {
			if f == m.msgSortField {
				m.msgSortField = msgSortFields[(i+1)%len(msgSortFields)]
				break
			}
		}
		m.loading = true
		m.err = nil       // Clear any previous error
		m.loadRequestID++ // Increment to invalidate stale responses
		return m, m.loadMessages()

	case "r", "v":
		if m.msgSortDirection == query.SortDesc {
			m.msgSortDirection = query.SortAsc
		} else {
			m.msgSortDirection = query.SortDesc
		}
		m.loading = true
		m.err = nil       // Clear any previous error
		m.loadRequestID++ // Increment to invalidate stale responses
		return m, m.loadMessages()

	// View thread
	case "T":
		if len(m.messages) > 0 && m.cursor < len(m.messages) {
			convID := m.messages[m.cursor].ConversationID
			if convID > 0 {
				m.transitionBuffer = m.renderView() // Freeze screen until data loads

				// Save current state
				m.pushBreadcrumb()

				m.threadConversationID = convID
				m.threadMessages = nil
				m.threadCursor = 0
				m.threadScrollOffset = 0
				m.level = levelThreadView
				m.loading = true
				m.err = nil
				m.loadRequestID++
				return m, m.loadThreadMessages(convID)
			}
		}
	}

	// Check if we should load more search results
	if cmd := m.maybeLoadMoreSearchResults(); cmd != nil {
		return m, cmd
	}

	return m, nil
}

// maybeLoadMoreSearchResults checks if we're near the end of search results and should load more.
func (m *Model) maybeLoadMoreSearchResults() tea.Cmd {
	// Only paginate search results in fast mode
	if m.searchQuery == "" || m.searchMode != searchModeFast {
		return nil
	}

	// Don't load more if already loading or no more results
	if m.searchLoadingMore || m.loading {
		return nil
	}

	// Don't load more if we have no messages (empty results)
	if len(m.messages) == 0 {
		return nil
	}

	// Check if total count is known and we have all results
	// Note: searchTotalCount == 0 means no results, so we should not load more
	if m.searchTotalCount >= 0 && int64(len(m.messages)) >= m.searchTotalCount {
		return nil
	}

	// Load more when cursor is within 20 rows of the end
	threshold := 20
	if m.cursor >= len(m.messages)-threshold {
		m.searchLoadingMore = true
		m.searchRequestID++
		return m.loadSearchWithOffset(m.searchQuery, m.searchOffset, true)
	}

	return nil
}

// maybeLoadMoreMessages checks if we're near the end of the loaded message list and should load more.
// This enables infinite-scroll pagination for non-search message lists.
func (m *Model) maybeLoadMoreMessages() tea.Cmd {
	// Don't paginate during search — search has its own pagination
	if m.searchQuery != "" {
		return nil
	}

	// Don't load more if already loading
	if m.msgListLoadingMore || m.loading {
		return nil
	}

	// Don't load more if we have no messages (empty results)
	if len(m.messages) == 0 {
		return nil
	}

	// Don't load more if we already know all data has been loaded
	if m.msgListComplete {
		return nil
	}

	// If total loaded messages isn't a multiple of the page size,
	// the last page was short — we've reached the end of the data.
	if len(m.messages)%messageListPageSize != 0 {
		return nil
	}

	// Check if contextStats tells us we have all results
	if m.contextStats != nil && m.contextStats.MessageCount > 0 &&
		int64(len(m.messages)) >= m.contextStats.MessageCount {
		return nil
	}
	if m.allMessages && m.stats != nil && m.stats.MessageCount > 0 &&
		int64(len(m.messages)) >= m.stats.MessageCount {
		return nil
	}

	// Load more when cursor is within 20 rows of the end
	threshold := 20
	if m.cursor >= len(m.messages)-threshold {
		m.msgListLoadingMore = true
		m.loadRequestID++
		return m.loadMessagesWithOffset(m.msgListOffset, true)
	}

	return nil
}

// handleMessageDetailKeys handles keys in the message detail view.
func (m Model) handleMessageDetailKeys(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	// When detail search input is active, route keys there first
	if m.detailSearchActive {
		switch msg.String() {
		case keyNameEnter:
			m.detailSearchActive = false
			m.detailSearchQuery = m.detailSearchInput.Value()
			m.findDetailMatches()
			if len(m.detailSearchMatches) > 0 {
				m.detailSearchMatchIndex = 0
				m.scrollToDetailMatch()
			}
			return m, nil
		case keyNameEsc:
			m.detailSearchActive = false
			m.detailSearchInput.SetValue("")
			return m, nil
		default:
			var cmd tea.Cmd
			m.detailSearchInput, cmd = m.detailSearchInput.Update(msg)
			return m, cmd
		}
	}

	// Handle global keys (quit, help) but not when detail search is active
	if !m.detailSearchActive {
		if m2, cmd, handled := m.handleGlobalKeys(msg); handled {
			return m2, cmd
		}
	}

	switch msg.String() {
	// Back to message list or clear detail search
	case keyNameEsc:
		if m.detailSearchQuery != "" {
			m.detailSearchQuery = ""
			m.detailSearchMatches = nil
			m.detailSearchMatchIndex = 0
			return m, nil
		}
		return m.goBack()

	// Detail search
	case "/":
		m.detailSearchActive = true
		m.detailSearchInput = textinput.New()
		m.detailSearchInput.Placeholder = "find in message..."
		m.detailSearchInput.CharLimit = 200
		m.detailSearchInput.SetWidth(50)
		if m.detailSearchQuery != "" {
			m.detailSearchInput.SetValue(m.detailSearchQuery)
		}
		m.detailSearchInput.Focus()
		return m, textinput.Blink

	// Next match
	case "n":
		if m.detailSearchQuery != "" && len(m.detailSearchMatches) > 0 {
			m.detailSearchMatchIndex = (m.detailSearchMatchIndex + 1) % len(m.detailSearchMatches)
			m.scrollToDetailMatch()
		}
		return m, nil

	// Previous match
	case "N":
		if m.detailSearchQuery != "" && len(m.detailSearchMatches) > 0 {
			m.detailSearchMatchIndex--
			if m.detailSearchMatchIndex < 0 {
				m.detailSearchMatchIndex = len(m.detailSearchMatches) - 1
			}
			m.scrollToDetailMatch()
		}
		return m, nil

	// Navigate to previous message in list (left = towards first)
	case "left", "h":
		return m.navigateDetailPrev()

	// Navigate to next message in list (right = towards last)
	case "right", "l":
		return m.navigateDetailNext()

	// Scroll content
	case "up", "k":
		// Clamp first in case scroll is out of range after resize
		m.clampDetailScroll()
		if m.detailScroll > 0 {
			m.detailScroll--
		} else {
			return m.showFlash("At top")
		}
	case keyNameDown, "j":
		// Clamp first in case scroll is out of range after resize
		m.clampDetailScroll()
		maxScroll := max(m.detailLineCount-m.detailPageSize(), 0)
		if m.detailScroll < maxScroll {
			m.detailScroll++
		} else {
			return m.showFlash("At bottom")
		}
	case "pgup", "ctrl+u":
		// Clamp first in case scroll is out of range after resize
		m.clampDetailScroll()
		if m.detailScroll == 0 {
			return m.showFlash("At top")
		}
		m.detailScroll -= m.detailPageSize()
		if m.detailScroll < 0 {
			m.detailScroll = 0
		}
	case "pgdown", "ctrl+d":
		// Clamp first in case scroll is out of range after resize
		m.clampDetailScroll()
		maxScroll := max(m.detailLineCount-m.detailPageSize(), 0)
		if m.detailScroll >= maxScroll {
			return m.showFlash("At bottom")
		}
		m.detailScroll += m.detailPageSize()
		m.clampDetailScroll()
	case "home", "g":
		m.detailScroll = 0
	case "end", "G":
		m.detailScroll = max(m.detailLineCount-m.detailPageSize(), 0)

	// View thread
	case "T":
		if m.messageDetail != nil && m.messageDetail.ConversationID > 0 {
			m.transitionBuffer = m.renderView() // Freeze screen until data loads

			// Save current state
			m.pushBreadcrumb()

			m.threadConversationID = m.messageDetail.ConversationID
			m.threadMessages = nil
			m.threadCursor = 0
			m.threadScrollOffset = 0
			m.level = levelThreadView
			m.loading = true
			m.err = nil
			m.loadRequestID++
			return m, m.loadThreadMessages(m.messageDetail.ConversationID)
		}

	// Export attachments
	case "e":
		if m.messageDetail != nil && len(m.messageDetail.Attachments) > 0 {
			m.modal = modalExportAttachments
			m.modalCursor = 0
			// Initialize selection: all attachments selected by default
			m.exportSelection = make(map[int]bool)
			for i := range m.messageDetail.Attachments {
				m.exportSelection[i] = true
			}
			m.exportCursor = 0
		} else {
			return m.showFlash("No attachments to export")
		}
	}

	return m, nil
}

// handleThreadViewKeys handles keys in the thread view.
func (m Model) handleThreadViewKeys(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	// Handle global keys (quit, help)
	if m2, cmd, handled := m.handleGlobalKeys(msg); handled {
		return m2, cmd
	}

	switch msg.String() {
	// Back to previous view
	case keyNameEsc:
		return m.goBack()

	// Navigation
	case "up", "k":
		if m.threadCursor > 0 {
			m.threadCursor--
			m.ensureThreadCursorVisible()
		}
	case keyNameDown, "j":
		if m.threadCursor < len(m.threadMessages)-1 {
			m.threadCursor++
			m.ensureThreadCursorVisible()
		}
	case "pgup", "ctrl+u":
		step := m.visibleRows()
		m.threadCursor -= step
		m.threadScrollOffset -= step
		if m.threadCursor < 0 {
			m.threadCursor = 0
		}
		if m.threadScrollOffset < 0 {
			m.threadScrollOffset = 0
		}
	case "pgdown", "ctrl+d":
		step := m.visibleRows()
		itemCount := len(m.threadMessages)
		m.threadCursor += step
		m.threadScrollOffset += step
		if m.threadCursor >= itemCount {
			m.threadCursor = itemCount - 1
		}
		if m.threadCursor < 0 {
			m.threadCursor = 0
		}
		maxScroll := max(itemCount-m.visibleRows(), 0)
		if m.threadScrollOffset > maxScroll {
			m.threadScrollOffset = maxScroll
		}

	// View message detail
	case keyNameEnter:
		if len(m.threadMessages) > 0 && m.threadCursor < len(m.threadMessages) {
			m.transitionBuffer = m.renderView() // Freeze screen until data loads

			// Save current thread view state
			m.pushBreadcrumb()

			// Load message detail
			m.pendingDetailSubject = m.threadMessages[m.threadCursor].Subject
			m.detailMessageIndex = m.threadCursor
			m.detailFromThread = true // Navigate within thread messages
			m.messageDetail = nil
			m.detailLineCount = 0
			m.detailScroll = 0
			m.level = levelMessageDetail
			m.loading = true
			m.err = nil
			m.detailRequestID++
			return m, m.loadMessageDetail(m.threadMessages[m.threadCursor].ID)
		}
	}

	return m, nil
}

func (m Model) handleModalKeys(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch m.modal {
	case modalDeleteConfirm:
		return m.handleDeleteConfirmKeys(msg)
	case modalDeleteResult:
		return m.handleDeleteResultKeys()
	case modalQuitConfirm:
		return m.handleQuitConfirmKeys(msg)
	case modalAccountSelector:
		return m.handleAccountSelectorKeys(msg)
	case modalFilterToggle:
		return m.handleFilterToggleKeys(msg)
	case modalExportAttachments:
		return m.handleExportAttachmentsKeys(msg)
	case modalExportResult:
		return m.handleExportResultKeys()
	case modalError:
		return m.handleErrorKeys()
	case modalHelp:
		return m.handleHelpKeys(msg)
	case modalNone:
		// no modal active; fall through to nil return
	}
	return m, nil
}

func (m Model) handleDeleteConfirmKeys(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "y", "Y":
		return m.confirmDeletion()
	case "n", "N", keyNameEsc:
		m.modal = modalNone
		m.pendingManifest = nil
	}
	return m, nil
}

func (m Model) handleDeleteResultKeys() (tea.Model, tea.Cmd) {
	// Any key dismisses the result
	m.modal = modalNone
	m.modalResult = ""
	return m, nil
}

func (m Model) handleQuitConfirmKeys(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "y", "Y", keyNameEnter:
		m.quitting = true
		return m, tea.Quit
	case "n", "N", keyNameEsc, "q":
		m.modal = modalNone
	}
	return m, nil
}

func (m Model) handleAccountSelectorKeys(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	accounts := m.selectableAccounts()
	maxIdx := len(accounts) // 0 = All Accounts/Sources, then selectable sources
	switch msg.String() {
	case "up", "k":
		if m.modalCursor > 0 {
			m.modalCursor--
		}
	case keyNameDown, "j":
		if m.modalCursor < maxIdx {
			m.modalCursor++
		}
	case keyNameEnter:
		// Apply selection with bounds check
		var selectedID *int64
		if m.modalCursor > 0 && m.modalCursor <= len(accounts) {
			accID := accounts[m.modalCursor-1].ID
			selectedID = &accID
		}
		if m.mode == modeMeetings {
			m.meetingState.sourceID = selectedID
		} else {
			m.accountFilter = selectedID
		}
		m.modal = modalNone
		m.loading = true
		if m.mode == modeMeetings {
			m.meetingState.requestID++
			m.meetingState.searchRequestID++
			m.meetingState.listOffset = 0
			m.meetingState.listComplete = false
			m.meetingState.listLoadingMore = false
			m.meetingState.listLoading = false
			m.meetingState.searchLoading = false
			m.meetingState.searchOffset = 0
			m.meetingState.searchComplete = false
			m.meetingState.cursor = 0
			m.meetingState.scrollOffset = 0
			// A pre-search snapshot belongs to the previous source and must
			// never be restored after the source changes.
			m.meetingState.preSearch = nil
			if m.meetingState.searchQuery != "" {
				m.meetingState.searchSnapshotInvalid = true
				m.meetingState.searchLoading = true
				spinCmd := m.startSpinner()
				return m, tea.Batch(
					spinCmd,
					m.loadMeetingSearch(m.meetingState.searchQuery, 0, false),
				)
			}
			m.meetingState.searchSnapshotInvalid = true
			m.meetingState.listLoading = true
			return m, m.loadMeetingMessages()
		}
		if m.mode == modeTexts {
			m.textState.filter.SourceID = m.accountFilter
			cmd := m.loadTextData()
			return m, cmd
		}
		m.aggregateRequestID++
		return m, tea.Batch(m.loadData(), m.loadStats())
	case keyNameEsc:
		m.modal = modalNone
	}
	return m, nil
}

// filterOptionCount is the number of toggleable options in the filter modal.
const filterOptionCount = 2

func (m Model) handleFilterToggleKeys(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "up", "k":
		if m.modalCursor > 0 {
			m.modalCursor--
		}
	case keyNameDown, "j":
		if m.modalCursor < filterOptionCount-1 {
			m.modalCursor++
		}
	case "space", "x":
		// Toggle the checkbox at current cursor
		switch m.modalCursor {
		case 0:
			m.filters.attachmentsOnly = !m.filters.attachmentsOnly
		case 1:
			m.filters.hideDeletedFromSource = !m.filters.hideDeletedFromSource
		}
	case keyNameEnter, keyNameEsc:
		// Apply filters: close modal and reload data
		m.modal = modalNone
		m.loading = true

		// Keep drillFilter in sync with global toggles so drill-down
		// and sub-aggregate results reflect the latest filter state.
		if m.level == levelMessageList || m.level == levelDrillDown {
			m.drillFilter.WithAttachmentsOnly = m.filters.attachmentsOnly
			m.drillFilter.HideDeletedFromSource = m.filters.hideDeletedFromSource
		}

		if m.level == levelMessageList {
			m.loadRequestID++
			return m, tea.Batch(m.loadMessages(), m.loadStats())
		}
		m.aggregateRequestID++
		return m, tea.Batch(m.loadData(), m.loadStats())
	}
	return m, nil
}

func (m Model) handleExportAttachmentsKeys(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if m.messageDetail == nil || len(m.messageDetail.Attachments) == 0 {
		m.modal = modalNone
		return m, nil
	}
	maxIdx := len(m.messageDetail.Attachments) - 1
	switch msg.String() {
	case "up", "k":
		if m.exportCursor > 0 {
			m.exportCursor--
		}
	case keyNameDown, "j":
		if m.exportCursor < maxIdx {
			m.exportCursor++
		}
	case "space": // Space toggles selection
		m.exportSelection[m.exportCursor] = !m.exportSelection[m.exportCursor]
	case "a": // Select all
		for i := range m.messageDetail.Attachments {
			m.exportSelection[i] = true
		}
	case "n": // Select none
		for i := range m.messageDetail.Attachments {
			m.exportSelection[i] = false
		}
	case keyNameEnter:
		return m.exportAttachments()
	case keyNameEsc:
		m.modal = modalNone
		m.exportSelection = nil
	}
	return m, nil
}

func (m Model) handleExportResultKeys() (tea.Model, tea.Cmd) {
	// Any key closes the result modal
	m.modal = modalNone
	m.modalResult = ""
	return m, nil
}

func (m Model) handleErrorKeys() (tea.Model, tea.Cmd) {
	// Any key dismisses the error modal
	m.modal = modalNone
	m.modalResult = ""
	m.err = nil
	return m, nil
}

func (m Model) handleHelpKeys(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case keyNameDown, "j":
		m.helpScroll++
	case "up", "k":
		if m.helpScroll > 0 {
			m.helpScroll--
		}
	case "pgdown":
		m.helpScroll += 10
	case "pgup":
		m.helpScroll -= 10
		if m.helpScroll < 0 {
			m.helpScroll = 0
		}
	default:
		// Any other key closes help
		m.modal = modalNone
		m.helpScroll = 0
		return m, nil
	}
	// Clamp scroll to prevent overscroll
	if maxScroll := len(m.activeHelpLines()) - m.helpMaxVisible(); maxScroll > 0 {
		if m.helpScroll > maxScroll {
			m.helpScroll = maxScroll
		}
	} else {
		m.helpScroll = 0
	}
	return m, nil
}

// cycleViewType cycles the view type forward or backward, optionally skipping a view.
// skipView is the view type to skip (e.g., drillViewType in sub-aggregate mode), or -1 to skip none.
func (m *Model) cycleViewType(forward bool, skipView query.ViewType) {
	numViews := int(query.ViewTypeCount)
	if forward {
		m.viewType = (m.viewType + 1) % query.ViewType(numViews)
		if skipView >= 0 && m.viewType == skipView {
			m.viewType = (m.viewType + 1) % query.ViewType(numViews)
		}
	} else {
		if m.viewType == 0 {
			m.viewType = query.ViewType(numViews - 1)
		} else {
			m.viewType--
		}
		if skipView >= 0 && m.viewType == skipView {
			if m.viewType == 0 {
				m.viewType = query.ViewType(numViews - 1)
			} else {
				m.viewType--
			}
		}
	}
}

// resetViewState resets cursor and selection state after a view type change.
func (m *Model) resetViewState() {
	m.selection.aggregateKeys = make(map[string]bool)
	m.selection.aggregateViewType = m.viewType
	m.cursor = 0
	m.scrollOffset = 0
}

// setDrillFilterForView sets the appropriate filter field on drillFilter based on the current viewType.
func (m *Model) setDrillFilterForView(key string) {
	switch m.viewType {
	case query.ViewSenders:
		m.drillFilter.Sender = key
		if key == "" {
			m.drillFilter.SetEmptyTarget(query.ViewSenders)
		}
	case query.ViewSenderNames:
		m.drillFilter.SenderName = key
		if key == "" {
			m.drillFilter.SetEmptyTarget(query.ViewSenderNames)
		}
	case query.ViewRecipients:
		m.drillFilter.Recipient = key
		if key == "" {
			m.drillFilter.SetEmptyTarget(query.ViewRecipients)
		}
	case query.ViewRecipientNames:
		m.drillFilter.RecipientName = key
		if key == "" {
			m.drillFilter.SetEmptyTarget(query.ViewRecipientNames)
		}
	case query.ViewDomains:
		m.drillFilter.Domain = key
		if key == "" {
			m.drillFilter.SetEmptyTarget(query.ViewDomains)
		}
	case query.ViewLabels:
		m.drillFilter.Label = key
		if key == "" {
			m.drillFilter.SetEmptyTarget(query.ViewLabels)
		}
	case query.ViewTime:
		m.drillFilter.TimeRange.Period = key
		m.drillFilter.TimeRange.Granularity = m.timeGranularity
	case query.ViewTypeCount:
		// Sentinel for enum length; not a real view.
	}
}

// enterDrillDown handles the drill-down from aggregate view to message list.
func (m Model) enterDrillDown(row query.AggregateRow) (tea.Model, tea.Cmd) {
	isSub := m.level == levelDrillDown

	m.transitionBuffer = m.renderView() // Freeze screen until data loads
	m.pushBreadcrumb()

	m.contextStats = &query.TotalStats{
		MessageCount:    row.Count,
		TotalSize:       row.TotalSize,
		AttachmentSize:  row.AttachmentSize,
		AttachmentCount: row.AttachmentCount,
	}

	if !isSub {
		// Top-level: create fresh drill filter
		m.drillViewType = m.viewType
		m.drillFilter = query.MessageFilter{
			SourceID:              m.accountFilter,
			WithAttachmentsOnly:   m.filters.attachmentsOnly,
			HideDeletedFromSource: m.filters.hideDeletedFromSource,
			TimeRange:             query.TimeRange{Granularity: m.timeGranularity},
		}
	}

	// Set filter field on drillFilter (accumulates for sub-agg)
	m.setDrillFilterForView(row.Key)

	m.filterKey = row.Key
	m.allMessages = false
	m.level = levelMessageList
	m.cursor = 0
	m.scrollOffset = 0
	m.messages = nil // Clear stale messages from previous drill-down
	m.msgListOffset = 0
	m.msgListLoadingMore = false
	m.msgListComplete = false
	m.loading = true
	m.err = nil

	// Only clear selection on top-level drill-down (sub-agg didn't clear before)
	if !isSub {
		m.selection.aggregateKeys = make(map[string]bool)
		m.selection.messageIDs = make(map[int64]bool)
	}

	// Invalidate in-flight search responses from the aggregate level.
	m.searchRequestID++

	// Preserve search query through drill-down so the message list
	// shows only messages matching both the drill filter and the search.
	if m.searchQuery != "" {
		m.searchFilter = m.drillFilter
		m.searchFilter.SourceID = m.accountFilter
		m.searchFilter.WithAttachmentsOnly = m.filters.attachmentsOnly
		m.searchFilter.HideDeletedFromSource = m.filters.hideDeletedFromSource
		m.loadRequestID++ // Invalidate stale loadMessages responses
		m.searchRequestID++
		return m, m.loadSearch(m.searchQuery)
	}

	m.loadRequestID++
	return m, m.loadMessages()
}

func (m *Model) openAccountSelector() {
	m.modal = modalAccountSelector
	m.modalCursor = 0 // Default to "All Accounts" / "All Sources"
	selectedID := m.accountFilter
	if m.mode == modeMeetings {
		selectedID = m.meetingState.sourceID
	}
	accounts := m.selectableAccounts()
	if selectedID != nil {
		for i, acc := range accounts {
			if acc.ID == *selectedID {
				m.modalCursor = i + 1 // +1 because 0 is "All Accounts"
				break
			}
		}
	}
	// Clamp to valid range in case accounts list changed
	if m.modalCursor > len(accounts) {
		m.modalCursor = 0
	}
}

func (m *Model) openFilterModal() {
	m.modal = modalFilterToggle
	m.modalCursor = 0
}

// exitInlineSearchMode resets inline search UI state without changing filter state.
func (m *Model) exitInlineSearchMode() {
	m.inlineSearchActive = false
	m.inlineSearchLoading = false
}

// clearSearchState clears search query and invalidates pending requests.
func (m *Model) clearSearchState() {
	m.searchQuery = ""
	m.searchRequestID++
	m.contextStats = nil
}

// reloadCurrentView triggers a data reload based on the current level.
func (m Model) reloadCurrentView() (tea.Model, tea.Cmd) {
	if m.level == levelMessageList {
		m.loadRequestID++
		return m, m.loadMessages()
	}
	m.aggregateRequestID++
	return m, m.loadData()
}

// commitInlineSearch finalizes the search and exits inline mode.
func (m Model) commitInlineSearch() (tea.Model, tea.Cmd) {
	m.exitInlineSearchMode()
	queryStr := m.searchInput.Value()

	if queryStr == "" {
		// Empty search clears filter - restore from snapshot if available
		m.clearSearchState()
		if m.level == levelMessageList && m.preSearchMessages != nil {
			m.restorePreSearchSnapshot()
			return m, nil
		}
		return m.reloadCurrentView()
	}

	m.searchQuery = queryStr
	// In message list view, execute search to show results
	if m.level == levelMessageList {
		m.searchFilter = m.drillFilter
		m.searchFilter.SourceID = m.accountFilter
		m.searchFilter.WithAttachmentsOnly = m.filters.attachmentsOnly
		m.searchFilter.HideDeletedFromSource = m.filters.hideDeletedFromSource
		m.searchRequestID++
		m.loading = true
		spinCmd := m.startSpinner()
		return m, tea.Batch(spinCmd, m.loadSearch(queryStr))
	}
	// In aggregate views, results already showing from debounced search
	return m, nil
}

// cancelInlineSearch cancels the search and restores previous state.
func (m Model) cancelInlineSearch() (tea.Model, tea.Cmd) {
	m.exitInlineSearchMode()
	m.searchInput.SetValue("")
	m.clearSearchState()

	if m.level == levelMessageList && m.preSearchMessages != nil {
		m.restorePreSearchSnapshot()
		return m, nil
	}
	return m.reloadCurrentView()
}

// clearMessageListSearch clears an active search in message list view and restores previous state.
func (m Model) clearMessageListSearch() (tea.Model, tea.Cmd) {
	m.searchQuery = ""
	m.searchFilter = query.MessageFilter{}
	m.searchInput.SetValue("")
	m.searchRequestID++

	if m.preSearchMessages != nil {
		m.restorePreSearchSnapshot()
		return m, nil
	}
	m.contextStats = nil
	m.loadRequestID++
	return m, m.loadMessages()
}

// restorePreSearchSnapshot restores the cached message list state from before
// the search began, avoiding a re-query. No async work is needed, so callers
// pair it with a nil command.
func (m *Model) restorePreSearchSnapshot() {
	m.messages = m.preSearchMessages
	m.cursor = m.preSearchCursor
	m.scrollOffset = m.preSearchScrollOffset
	m.contextStats = m.preSearchContextStats
	m.loading = false
	m.searchLoadingMore = false
	m.inlineSearchLoading = false
	m.searchOffset = 0
	m.searchTotalCount = 0
	// Clear the snapshot
	m.preSearchMessages = nil
	m.preSearchContextStats = nil
}

func (m *Model) activateInlineSearch(placeholder string) tea.Cmd {
	// Snapshot current message list so Esc can restore instantly.
	// Only take a snapshot if we don't already have one (re-searches
	// keep the original snapshot). This also handles inherited search
	// from aggregate drill-down: the first local / captures the
	// inherited results so Esc can restore them.
	if m.level == levelMessageList && m.preSearchMessages == nil {
		m.preSearchMessages = m.messages
		m.preSearchCursor = m.cursor
		m.preSearchScrollOffset = m.scrollOffset
		// Deep copy stats to avoid aliasing with mutations during search
		if m.contextStats != nil {
			tmp := *m.contextStats
			m.preSearchContextStats = &tmp
		} else {
			m.preSearchContextStats = nil
		}
	}
	m.inlineSearchActive = true
	m.searchMode = searchModeFast
	m.searchInput.Placeholder = placeholder
	m.searchInput.SetValue("") // Clear previous search
	m.searchInput.Focus()
	return textinput.Blink
}

func (m *Model) toggleAggregateSelection() {
	if len(m.rows) > 0 && m.cursor < len(m.rows) {
		key := m.rows[m.cursor].Key
		if m.selection.aggregateKeys[key] {
			delete(m.selection.aggregateKeys, key)
		} else {
			m.selection.aggregateKeys[key] = true
		}
	}
}

func (m *Model) selectVisibleAggregates() {
	endRow := min(m.scrollOffset+m.pageSize, len(m.rows))
	for i := m.scrollOffset; i < endRow; i++ {
		m.selection.aggregateKeys[m.rows[i].Key] = true
	}
}

func (m *Model) clearAllSelections() {
	m.selection.aggregateKeys = make(map[string]bool)
	m.selection.messageIDs = make(map[int64]bool)
}
