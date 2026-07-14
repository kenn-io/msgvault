package tui

import (
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"go.kenn.io/msgvault/internal/query"
)

func (m Model) handleMeetingKeyPress(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if m.modal != modalNone {
		return m.handleModalKeys(msg)
	}
	if m.meetingState.level == meetingLevelDetail && m.meetingState.detailSearchActive {
		return m.handleMeetingDetailSearchInput(msg)
	}
	if m.meetingState.searchActive {
		return m.handleMeetingSearchInput(msg)
	}

	if updated, cmd, handled := m.handleGlobalKeys(msg); handled {
		return updated, cmd
	}
	if m.meetingState.level == meetingLevelDetail {
		return m.handleMeetingDetailKeys(msg)
	}

	// Meeting sources are read-only. Selection and deletion workflows are
	// intentionally unavailable in this mode.
	switch msg.String() {
	case "space", "S", "d", "D", "x":
		return m, nil
	}

	if m.navigateMeetingList(msg.String()) {
		if cmd := m.maybeLoadMoreMeetings(); cmd != nil {
			return m, cmd
		}
		return m, nil
	}

	switch msg.String() {
	case keyNameEsc, "backspace":
		if m.meetingState.searchQuery != "" {
			m.meetingState.searchQuery = ""
			m.meetingState.searchInput.SetValue("")
			m.invalidateMeetingSearchLoad()
			if !m.meetingState.searchSnapshotInvalid && m.meetingState.preSearch != nil {
				m.meetingState.messages = m.meetingState.preSearch
				m.meetingState.preSearch = nil
				m.restoreMeetingListPagination()
			} else {
				// A source change invalidates the old source's pre-search
				// snapshot. Reload the selected source rather than presenting
				// former search results as an unsearched list.
				m.meetingState.messages = nil
			}
			m.meetingState.cursor = 0
			m.meetingState.scrollOffset = 0
			if m.meetingState.messages == nil {
				m.meetingState.searchSnapshotInvalid = false
				return m.reloadMeetingList()
			}
		}
		return m, nil
	case "A":
		m.openAccountSelector()
		return m, nil
	case "s":
		if m.meetingState.searchQuery != "" {
			return m, nil
		}
		if m.meetingState.sortField == query.MessageSortByDate {
			m.meetingState.sortField = query.MessageSortBySubject
		} else {
			m.meetingState.sortField = query.MessageSortByDate
		}
		return m.reloadMeetingList()
	case "r":
		if m.meetingState.searchQuery != "" {
			return m, nil
		}
		if m.meetingState.sortDirection == query.SortDesc {
			m.meetingState.sortDirection = query.SortAsc
		} else {
			m.meetingState.sortDirection = query.SortDesc
		}
		return m.reloadMeetingList()
	case "/":
		if !m.meetingState.searchSnapshotInvalid && m.meetingState.preSearch == nil {
			m.meetingState.preSearch = append([]query.MessageSummary(nil), m.meetingState.messages...)
		}
		m.meetingState.searchActive = true
		m.meetingState.searchInput.SetValue(m.meetingState.searchQuery)
		return m, m.meetingState.searchInput.Focus()
	case keyNameEnter:
		if len(m.meetingState.messages) == 0 || m.meetingState.cursor >= len(m.meetingState.messages) {
			return m, nil
		}
		m.meetingState.level = meetingLevelDetail
		m.meetingState.detail = nil
		m.meetingState.detailScroll = 0
		m.meetingState.detailRequestID++
		m.meetingState.detailLoading = true
		m.loading = true
		spinCmd := m.startSpinner()
		return m, tea.Batch(spinCmd, m.loadMeetingDetail(m.meetingState.messages[m.meetingState.cursor].ID))
	}
	return m, nil
}

func (m Model) reloadMeetingList() (tea.Model, tea.Cmd) {
	m.meetingState.requestID++
	m.meetingState.listOffset = 0
	m.meetingState.listComplete = false
	m.meetingState.listLoadingMore = false
	m.meetingState.listLoading = true
	m.meetingState.preSearch = nil
	m.meetingState.searchSnapshotInvalid = true
	m.loading = true
	spinCmd := m.startSpinner()
	return m, tea.Batch(spinCmd, m.loadMeetingMessages())
}

func (m *Model) invalidateMeetingSearchLoad() {
	m.meetingState.searchRequestID++
	m.meetingState.searchOffset = 0
	m.meetingState.searchComplete = false
	m.meetingState.listLoadingMore = false
	m.meetingState.searchLoading = false
	m.updateMeetingLoading()
}

func (m *Model) invalidateMeetingListForSearch() {
	m.meetingState.requestID++
	m.meetingState.listOffset = 0
	m.meetingState.listComplete = false
	m.meetingState.listLoadingMore = false
	m.meetingState.listLoading = false
	m.meetingState.searchOffset = 0
	m.meetingState.searchComplete = false
	m.updateMeetingLoading()
}

func (m *Model) restoreMeetingListPagination() {
	m.meetingState.listOffset = len(m.meetingState.messages)
	m.meetingState.listComplete = len(m.meetingState.messages) < messageListPageSize
	m.meetingState.listLoadingMore = false
}

func (m *Model) maybeLoadMoreMeetings() tea.Cmd {
	if len(m.meetingState.messages) == 0 ||
		m.meetingState.cursor < len(m.meetingState.messages)-1 ||
		m.meetingState.listLoadingMore {
		return nil
	}
	m.meetingState.listLoadingMore = true
	if m.meetingState.searchQuery != "" {
		if m.meetingState.searchComplete {
			m.meetingState.listLoadingMore = false
			return nil
		}
		m.loading = true
		m.meetingState.searchRequestID++
		m.meetingState.searchLoading = true
		return m.loadMeetingSearch(m.meetingState.searchQuery, len(m.meetingState.messages), true)
	}
	if m.meetingState.listComplete {
		m.meetingState.listLoadingMore = false
		return nil
	}
	m.loading = true
	m.meetingState.requestID++
	m.meetingState.listLoading = true
	m.meetingState.preSearch = nil
	m.meetingState.searchSnapshotInvalid = true
	return m.loadMeetingMessagesWithOffset(len(m.meetingState.messages), true)
}

func (m Model) handleMeetingDetailKeys(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case keyNameEsc, "backspace":
		m.meetingState.level = meetingLevelList
		m.meetingState.detailScroll = 0
		m.meetingState.detailRequestID++
		m.meetingState.detailLoading = false
		m.updateMeetingLoading()
		return m, nil
	case "up", "k":
		m.meetingState.detailScroll = max(m.meetingState.detailScroll-1, 0)
	case keyNameDown, "j":
		m.meetingState.detailScroll++
	case "pgup", "ctrl+u":
		m.meetingState.detailScroll = max(m.meetingState.detailScroll-m.visibleRows(), 0)
	case "pgdown", "ctrl+d":
		m.meetingState.detailScroll += m.visibleRows()
	case "left", "h":
		return m.changeMeetingDetail(-1)
	case "right", "l":
		return m.changeMeetingDetail(1)
	case "/":
		m.meetingState.detailSearchActive = true
		m.meetingState.detailSearchInput.SetValue(m.meetingState.detailSearchQuery)
		return m, m.meetingState.detailSearchInput.Focus()
	case "n":
		m.moveMeetingDetailMatch(1)
	case "N":
		m.moveMeetingDetailMatch(-1)
	}
	m.clampMeetingDetailScroll()
	return m, nil
}

func (m Model) handleMeetingDetailSearchInput(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case keyNameEnter:
		m.meetingState.detailSearchActive = false
		m.meetingState.detailSearchInput.Blur()
		m.meetingState.detailSearchQuery = strings.TrimSpace(m.meetingState.detailSearchInput.Value())
		m.findMeetingDetailMatches()
		if len(m.meetingState.detailSearchMatches) > 0 {
			m.scrollToMeetingDetailMatch()
		}
		return m, nil
	case keyNameEsc:
		m.meetingState.detailSearchActive = false
		m.meetingState.detailSearchInput.Blur()
		return m, nil
	case "ctrl+c":
		m.quitting = true
		return m, tea.Quit
	default:
		var cmd tea.Cmd
		m.meetingState.detailSearchInput, cmd = m.meetingState.detailSearchInput.Update(msg)
		return m, cmd
	}
}

func (m *Model) findMeetingDetailMatches() {
	m.meetingState.detailSearchMatches = nil
	m.meetingState.detailSearchMatchIndex = 0
	needle := strings.ToLower(m.meetingState.detailSearchQuery)
	if needle == "" {
		return
	}
	for index, line := range m.meetingDetailLines() {
		if strings.Contains(strings.ToLower(ansi.Strip(line)), needle) {
			m.meetingState.detailSearchMatches = append(m.meetingState.detailSearchMatches, index)
		}
	}
}

func (m *Model) scrollToMeetingDetailMatch() {
	if len(m.meetingState.detailSearchMatches) == 0 {
		return
	}
	target := m.meetingState.detailSearchMatches[m.meetingState.detailSearchMatchIndex]
	m.meetingState.detailScroll = max(target-m.detailPageSize()/2, 0)
	m.clampMeetingDetailScroll()
}

func (m *Model) clampMeetingDetailScroll() {
	maxScroll := max(len(m.meetingDetailLines())-m.detailPageSize(), 0)
	m.meetingState.detailScroll = min(max(m.meetingState.detailScroll, 0), maxScroll)
}

func (m *Model) moveMeetingDetailMatch(delta int) {
	count := len(m.meetingState.detailSearchMatches)
	if count == 0 {
		return
	}
	m.meetingState.detailSearchMatchIndex = (m.meetingState.detailSearchMatchIndex + delta + count) % count
	m.scrollToMeetingDetailMatch()
}

func (m Model) changeMeetingDetail(delta int) (tea.Model, tea.Cmd) {
	index := m.meetingState.cursor + delta
	if index < 0 || index >= len(m.meetingState.messages) {
		return m, nil
	}
	m.meetingState.cursor = index
	m.meetingState.detail = nil
	m.meetingState.detailScroll = 0
	m.meetingState.detailRequestID++
	m.meetingState.detailLoading = true
	m.loading = true
	spinCmd := m.startSpinner()
	return m, tea.Batch(spinCmd, m.loadMeetingDetail(m.meetingState.messages[index].ID))
}

func (m Model) handleMeetingSearchInput(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case keyNameEnter:
		queryString := strings.TrimSpace(m.meetingState.searchInput.Value())
		m.meetingState.searchActive = false
		m.meetingState.searchInput.Blur()
		m.meetingState.searchQuery = queryString
		if queryString == "" {
			m.invalidateMeetingSearchLoad()
			if m.meetingState.searchSnapshotInvalid || m.meetingState.preSearch == nil {
				m.meetingState.searchSnapshotInvalid = false
				m.meetingState.preSearch = nil
				m.meetingState.messages = nil
				m.meetingState.cursor = 0
				m.meetingState.scrollOffset = 0
				return m.reloadMeetingList()
			}
			m.meetingState.messages = m.meetingState.preSearch
			m.meetingState.preSearch = nil
			m.meetingState.cursor = 0
			m.meetingState.scrollOffset = 0
			m.restoreMeetingListPagination()
			return m, nil
		}
		m.invalidateMeetingListForSearch()
		if !m.meetingState.searchSnapshotInvalid && m.meetingState.preSearch == nil {
			m.meetingState.preSearch = append([]query.MessageSummary(nil), m.meetingState.messages...)
		}
		m.loading = true
		m.meetingState.searchRequestID++
		m.meetingState.searchLoading = true
		spinCmd := m.startSpinner()
		return m, tea.Batch(spinCmd, m.loadMeetingSearch(queryString, 0, false))
	case keyNameEsc:
		m.meetingState.searchActive = false
		m.meetingState.searchInput.Blur()
		m.meetingState.searchInput.SetValue(m.meetingState.searchQuery)
		return m, nil
	case "ctrl+c":
		m.quitting = true
		return m, tea.Quit
	default:
		var cmd tea.Cmd
		m.meetingState.searchInput, cmd = m.meetingState.searchInput.Update(msg)
		return m, cmd
	}
}

func (m *Model) navigateMeetingList(key string) bool {
	count := len(m.meetingState.messages)
	changed := false
	switch key {
	case "up", "k":
		if m.meetingState.cursor > 0 {
			m.meetingState.cursor--
			changed = true
		}
	case keyNameDown, "j":
		if m.meetingState.cursor < count-1 {
			m.meetingState.cursor++
			changed = true
		}
	case "home":
		m.meetingState.cursor = 0
		m.meetingState.scrollOffset = 0
		return true
	case "end", "G":
		m.meetingState.cursor = max(count-1, 0)
		changed = true
	case "pgup", "ctrl+u":
		m.meetingState.cursor = max(m.meetingState.cursor-m.visibleRows(), 0)
		changed = true
	case "pgdown", "ctrl+d":
		m.meetingState.cursor = min(m.meetingState.cursor+m.visibleRows(), max(count-1, 0))
		changed = true
	default:
		return false
	}

	if changed {
		m.meetingState.scrollOffset = calculateScrollOffset(
			m.meetingState.cursor,
			m.meetingState.scrollOffset,
			m.visibleRows(),
		)
	}
	return true
}
