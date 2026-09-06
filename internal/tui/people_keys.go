package tui

import (
	"errors"
	"strings"
	"time"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"go.kenn.io/msgvault/internal/peoplebrowser"
)

const peopleSearchDebounceDelay = 250 * time.Millisecond
const peopleCompletionLimit = 8

func (m Model) handlePeopleKeyPress(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if m.peopleState.form.overlay != peopleOverlayNone {
		return m.handlePeopleFormKey(msg)
	}
	if m.modal != modalNone {
		return m.handleModalKeys(msg)
	}
	if m.peopleState.level == peopleLevelMessage && m.detailSearchActive {
		return m.handlePeopleMessageKey(msg)
	}
	if m.peopleState.level == peopleLevelActivityMessage && m.detailSearchActive {
		return m.handlePeopleActivityMessageKey(msg)
	}
	if m.peopleState.level == peopleLevelMeetingDetail && m.meetingState.detailSearchActive {
		return m.handleMeetingDetailSearchInput(msg)
	}
	if m.peopleState.searchActive {
		return m.handlePeopleSearchInput(msg)
	}
	if updated, cmd, handled := m.handleGlobalKeys(msg); handled {
		return updated, cmd
	}
	if m.peopleState.level >= peopleLevelInboxTypes {
		if m.peopleState.level == peopleLevelMeetingDetail {
			return m.handlePeopleMeetingDetailKey(msg)
		}
		if m.peopleState.level == peopleLevelActivityMessage {
			return m.handlePeopleActivityMessageKey(msg)
		}
		return m.handlePeopleInboxKey(msg)
	}

	if m.peopleState.level == peopleLevelContact {
		if msg.String() == "p" {
			return m.startPeoplePromotion()
		}
		if m.peopleState.tab == peopleTabOverview {
			if updated, cmd, handled := m.handlePeopleOverviewKey(msg); handled {
				return updated, cmd
			}
		}
		if m.peopleState.tab == peopleTabAttributes {
			if updated, cmd, handled := m.handlePeopleAttributesKey(msg); handled {
				return updated, cmd
			}
		}
		if m.peopleState.tab == peopleTabMeetings {
			if updated, cmd, handled := m.handlePeopleMeetingsKey(msg); handled {
				return updated, cmd
			}
		}
		if m.peopleState.tab == peopleTabFiles {
			if updated, cmd, handled := m.handlePeopleFilesKey(msg); handled {
				return updated, cmd
			}
		}
		if m.peopleState.tab == peopleTabActivity {
			if updated, cmd, handled := m.handlePeopleActivityKey(msg); handled {
				return updated, cmd
			}
		}
		switch msg.String() {
		case keyNameEsc, keyNameBackspace:
			m.peopleState.requestID++
			m.peopleState.contactLoading = false
			if m.peopleState.popBreadcrumb() {
				m.peopleState.contact = nil
				m.peopleState.participantID = 0
				m.peopleState.err = nil
				m.peopleState.resetAttributes()
				m.peopleState.resetRelationshipContact()
				m.peopleState.resetInboxes()
				m.peopleState.resetContent()
			}
			m.updatePeopleLoading()
			return m, nil
		case keyNameTab:
			return m.activatePeopleTab((m.peopleState.tab + 1) % peopleTabCount)
		case "shift+tab":
			return m.activatePeopleTab(
				(m.peopleState.tab + peopleTabCount - 1) % peopleTabCount,
			)
		case "r":
			if m.peopleState.err == nil || m.peopleState.participantID <= 0 {
				return m, nil
			}
			m.peopleState.requestID++
			m.peopleState.err = nil
			m.peopleState.contactLoading = true
			m.loading = true
			spinCmd := m.startSpinner()
			return m, tea.Batch(
				spinCmd,
				m.loadPeopleContact(m.peopleState.participantID),
			)
		}
		return m, nil
	}

	if m.navigatePeopleDirectory(msg.String()) {
		return m, m.maybeLoadMorePeople()
	}

	switch msg.String() {
	case "/":
		m.peopleState.searchActive = true
		m.peopleState.searchRestoreQuery = m.peopleState.searchQuery
		m.peopleState.searchInput.SetValue(m.peopleState.searchQuery)
		return m, m.peopleState.searchInput.Focus()
	case keyNameEsc, keyNameBackspace:
		if m.peopleState.searchQuery == "" {
			return m, nil
		}
		m.peopleState.searchQuery = ""
		m.peopleState.searchInput.SetValue("")
		return m.reloadPeopleDirectory()
	case "r":
		if m.peopleState.err != nil {
			return m.reloadPeopleDirectory()
		}
	case keyNameEnter:
		if m.peopleState.cursor < 0 || m.peopleState.cursor >= len(m.peopleState.rows) {
			return m, nil
		}
		return m.openPeopleContact(m.peopleState.rows[m.peopleState.cursor].ID)
	}
	return m, nil
}

func (m Model) startPeoplePromotion() (tea.Model, tea.Cmd) {
	contact := m.peopleState.contact
	if contact == nil || contact.ID <= 0 || m.peopleState.promoting {
		return m, nil
	}
	if contact.Profile != nil {
		m.peopleState.attributesNotice = "This contact already has a durable profile."
		return m, nil
	}
	m.peopleState.settleFileAction()
	m.peopleState.abandonRelationshipLoad()
	m.peopleState.attributesNotice = "Promoting contact..."
	m.peopleState.promoting = true
	m.peopleState.requestID++
	m.loading = true
	spinCmd := m.startSpinner()
	return m, tea.Batch(spinCmd, m.promotePeopleContact(contact.ID, m.peopleState.tab))
}

func (m Model) activatePeopleTab(tab peopleTab) (Model, tea.Cmd) {
	if tab >= peopleTabCount || m.peopleState.contact == nil || m.peopleState.promoting {
		return m, nil
	}
	settledInboxLoad := m.peopleState.inboxesLoading ||
		m.peopleState.conversationsLoading || m.peopleState.conversationLoading ||
		m.peopleState.messageLoading
	m.peopleState.abandonContentPageLoad(m.peopleState.tab)
	m.peopleState.settleFileAction()
	m.peopleState.requestID++
	m.peopleState.attributesLoading = false
	m.peopleState.abandonRelationshipLoad()
	m.peopleState.promoting = false
	m.peopleState.inboxesLoading = false
	m.peopleState.conversationsLoading = false
	m.peopleState.conversationLoading = false
	m.peopleState.messageLoading = false
	m.peopleState.meetingsLoading = false
	m.meetingState.detailLoading = false
	m.peopleState.filesLoading = false
	m.peopleState.activityLoading = false
	m.peopleState.tab = tab
	m.peopleState.attributesNotice = ""
	m.peopleState.inboxErr = nil
	m.peopleState.level = peopleLevelContact
	if tab == peopleTabOverview {
		m.peopleState.scrollOffset = 0
	}
	if tab == peopleTabInboxes {
		m.peopleState.level = peopleLevelInboxTypes
		m.peopleState.cursor = 0
		m.peopleState.scrollOffset = 0
		if m.peopleState.inboxesLoaded {
			m.updatePeopleLoading()
			return m, nil
		}
		m.peopleState.inboxesLoading = true
		m.loading = true
		spinCmd := m.startSpinner()
		return m, tea.Batch(spinCmd, m.loadPeopleInboxes(m.peopleState.participantID))
	}
	if tab == peopleTabMeetings {
		m.peopleState.cursor = 0
		m.peopleState.scrollOffset = 0
		if m.peopleState.meetingsLoaded {
			m.updatePeopleLoading()
			return m, nil
		}
		m.peopleState.meetingsLoading = true
		m.loading = !settledInboxLoad
		return m, tea.Batch(m.startSpinner(), m.loadPeopleMeetings("", false))
	}
	if tab == peopleTabFiles {
		m.peopleState.cursor = 0
		m.peopleState.scrollOffset = 0
		if m.peopleState.filesLoaded {
			m.updatePeopleLoading()
			return m, nil
		}
		m.peopleState.filesLoading = true
		m.loading = !settledInboxLoad
		return m, tea.Batch(m.startSpinner(), m.loadPeopleFiles("", false))
	}
	if tab == peopleTabActivity {
		m.peopleState.cursor = 0
		m.peopleState.scrollOffset = 0
		if m.peopleState.activityLoaded {
			m.updatePeopleLoading()
			return m, nil
		}
		m.peopleState.activityLoading = true
		m.loading = !settledInboxLoad
		return m, tea.Batch(m.startSpinner(), m.loadPeopleActivity("", false))
	}
	if tab != peopleTabOverview && tab != peopleTabAttributes {
		m.updatePeopleLoading()
		return m, nil
	}
	var commands []tea.Cmd
	if tab == peopleTabOverview {
		if cmd := m.beginPeopleRelationshipLoad(); cmd != nil {
			commands = append(commands, cmd)
		}
	}
	if m.peopleState.contact.Profile == nil {
		if tab == peopleTabAttributes {
			m.peopleState.attributesNotice = peoplePromotionInstruction
		}
		m.updatePeopleLoading()
		if len(commands) == 0 {
			return m, nil
		}
		m.loading = true
		return m, tea.Batch(append([]tea.Cmd{m.startSpinner()}, commands...)...)
	}
	if !m.peopleState.attributesLoaded {
		m.peopleState.attributesLoading = true
		commands = append(commands, m.loadPeopleAttributes(m.peopleState.contact.Profile.ID, tab))
	}
	if len(commands) == 0 {
		m.updatePeopleLoading()
		return m, nil
	}
	m.loading = true
	return m, tea.Batch(append([]tea.Cmd{m.startSpinner()}, commands...)...)
}

func (m Model) handlePeopleOverviewKey(
	msg tea.KeyPressMsg,
) (tea.Model, tea.Cmd, bool) {
	contact := m.peopleState.contact
	if contact == nil {
		return m, nil, false
	}
	if m.navigatePeopleOverview(msg.String()) {
		return m, nil, true
	}
	switch msg.String() {
	case "[":
		updated, cmd := m.changePeopleRelationshipYear(-1)
		return updated, cmd, true
	case "]":
		updated, cmd := m.changePeopleRelationshipYear(1)
		return updated, cmd, true
	case "n":
		if contact.Profile == nil {
			return m, nil, true
		}
		group, value, ok := m.peopleNotesAttribute()
		if !ok {
			if !m.peopleState.attributesLoading {
				m.peopleState.attributesNotice = "Notes are unavailable. Press r to retry."
			}
			return m, nil, true
		}
		if value == nil {
			return m.openPeopleAttributeAdd(group), nil, true
		}
		return m.openPeopleAttributeEdit(group, value), nil, true
	case "r":
		if m.peopleState.err != nil {
			return m, nil, false
		}
		if m.peopleState.relationshipErr != nil {
			m.peopleState.relationshipErr = nil
			m.peopleState.relationshipRestarted = false
			cmd := m.beginPeopleRelationshipLoad()
			if cmd != nil {
				m.loading = true
				return m, tea.Batch(m.startSpinner(), cmd), true
			}
			m.updatePeopleLoading()
			return m, nil, true
		}
		if contact.Profile == nil || m.peopleState.attributesLoading {
			return m, nil, true
		}
	default:
		return m, nil, false
	}
	m.peopleState.requestID++
	m.peopleState.attributesLoading = true
	m.peopleState.attributesNotice = ""
	m.loading = true
	return m, tea.Batch(
		m.startSpinner(),
		m.loadPeopleAttributes(contact.Profile.ID, peopleTabOverview),
	), true
}

func (m *Model) navigatePeopleOverview(key string) bool {
	lineCount := len(m.peopleContactTabLines())
	maxOffset := max(lineCount-m.visibleRows(), 0)
	previous := m.peopleState.scrollOffset
	switch key {
	case "up", "k":
		m.peopleState.scrollOffset = max(previous-1, 0)
	case keyNameDown, "j":
		m.peopleState.scrollOffset = min(previous+1, maxOffset)
	case keyNamePageUp, keyNameCtrlU:
		m.peopleState.scrollOffset = max(previous-m.visibleRows(), 0)
	case keyNamePageDown, keyNameCtrlD:
		m.peopleState.scrollOffset = min(previous+m.visibleRows(), maxOffset)
	case keyNameHome:
		m.peopleState.scrollOffset = 0
	case keyNameEnd, "G":
		m.peopleState.scrollOffset = maxOffset
	default:
		return false
	}
	return m.peopleState.scrollOffset != previous || maxOffset == 0
}

func (m Model) handlePeopleMeetingsKey(
	msg tea.KeyPressMsg,
) (tea.Model, tea.Cmd, bool) {
	if m.navigatePeopleContentList(msg.String(), len(m.peopleState.meetings)) {
		return m, m.maybeLoadMorePeopleMeetings(), true
	}
	switch msg.String() {
	case keyNameEnter:
		if m.peopleState.cursor < 0 || m.peopleState.cursor >= len(m.peopleState.meetings) {
			return m, nil, true
		}
		meeting := m.peopleState.meetings[m.peopleState.cursor]
		m.peopleState.selectedContentMessage = meeting.ID
		m.peopleState.level = peopleLevelMeetingDetail
		m.meetingState.detail = nil
		m.meetingState.detailScroll = 0
		m.meetingState.detailSearchActive = false
		m.meetingState.detailSearchQuery = ""
		m.meetingState.detailSearchMatches = nil
		m.peopleState.meetingsErr = nil
		m.peopleState.requestID++
		m.meetingState.detailLoading = true
		m.loading = true
		return m, tea.Batch(m.startSpinner(), m.loadPeopleMeeting(meeting.ID)), true
	case "r":
		if m.peopleState.meetingsErr == nil {
			return m, nil, true
		}
		m.peopleState.requestID++
		m.peopleState.meetings = nil
		m.peopleState.meetingsLoaded = false
		m.peopleState.meetingsNextCursor = ""
		m.peopleState.meetingsCacheRevision = ""
		m.peopleState.meetingsRestarted = false
		m.peopleState.meetingsErr = nil
		m.peopleState.meetingsLoading = true
		m.loading = true
		return m, tea.Batch(m.startSpinner(), m.loadPeopleMeetings("", false)), true
	}
	return m, nil, false
}

func (m *Model) maybeLoadMorePeopleMeetings() tea.Cmd {
	if len(m.peopleState.meetings) == 0 ||
		m.peopleState.cursor < max(len(m.peopleState.meetings)-5, 0) ||
		m.peopleState.meetingsLoading || m.peopleState.meetingsLoadingMore ||
		m.peopleState.meetingsNextCursor == "" || m.peopleState.meetingsErr != nil {
		return nil
	}
	m.peopleState.requestID++
	m.peopleState.meetingsLoading = true
	m.peopleState.meetingsLoadingMore = true
	m.loading = true
	return m.loadPeopleMeetings(m.peopleState.meetingsNextCursor, true)
}

func (m Model) handlePeopleFilesKey(
	msg tea.KeyPressMsg,
) (tea.Model, tea.Cmd, bool) {
	previousCursor := m.peopleState.cursor
	if m.navigatePeopleContentList(msg.String(), len(m.peopleState.files)) {
		if m.peopleState.cursor != previousCursor &&
			(m.peopleState.fileOpening || m.peopleState.fileOpenFailed) {
			if m.peopleState.settleFileAction() {
				m.peopleState.requestID++
			}
			m.updatePeopleLoading()
		}
		return m, m.maybeLoadMorePeopleFiles(), true
	}
	switch msg.String() {
	case keyNameEnter:
		if m.peopleState.cursor < 0 || m.peopleState.cursor >= len(m.peopleState.files) {
			return m, nil, true
		}
		file := m.peopleState.files[m.peopleState.cursor]
		m.peopleState.selectedContentFile = file.ID
		m.peopleState.selectedContentMessage = file.MessageID
		m.peopleState.filesErr = nil
		m.peopleState.fileOpenFailed = false
		m.peopleState.requestID++
		m.peopleState.fileOpening = true
		m.loading = true
		return m, tea.Batch(
			m.startSpinner(), m.loadPeopleFileMessage(file.ID, file.MessageID),
		), true
	case "r":
		if m.peopleState.filesErr == nil {
			return m, nil, true
		}
		m.peopleState.requestID++
		if m.peopleState.fileOpenFailed && m.peopleState.selectedContentFile > 0 &&
			m.peopleState.selectedContentMessage > 0 {
			m.peopleState.filesErr = nil
			m.peopleState.fileOpenFailed = false
			m.peopleState.fileOpening = true
			m.loading = true
			return m, tea.Batch(
				m.startSpinner(),
				m.loadPeopleFileMessage(
					m.peopleState.selectedContentFile, m.peopleState.selectedContentMessage,
				),
			), true
		}
		m.peopleState.files = nil
		m.peopleState.filesLoaded = false
		m.peopleState.filesNextCursor = ""
		m.peopleState.filesCacheRevision = ""
		m.peopleState.filesRestarted = false
		m.peopleState.filesErr = nil
		m.peopleState.fileOpenFailed = false
		m.peopleState.filesLoading = true
		m.loading = true
		return m, tea.Batch(m.startSpinner(), m.loadPeopleFiles("", false)), true
	}
	return m, nil, false
}

func (m *Model) maybeLoadMorePeopleFiles() tea.Cmd {
	if len(m.peopleState.files) == 0 ||
		m.peopleState.cursor < max(len(m.peopleState.files)-5, 0) ||
		m.peopleState.filesLoading || m.peopleState.filesLoadingMore ||
		m.peopleState.filesNextCursor == "" || m.peopleState.filesErr != nil {
		return nil
	}
	m.peopleState.settleFileAction()
	m.peopleState.requestID++
	m.peopleState.filesLoading = true
	m.peopleState.filesLoadingMore = true
	m.loading = true
	return m.loadPeopleFiles(m.peopleState.filesNextCursor, true)
}

func (m Model) handlePeopleActivityKey(
	msg tea.KeyPressMsg,
) (tea.Model, tea.Cmd, bool) {
	if m.navigatePeopleList(
		msg.String(), len(m.peopleState.activity), m.peopleActivityDataRows(),
	) {
		return m, m.maybeLoadMorePeopleActivity(), true
	}
	switch msg.String() {
	case keyNameEnter:
		if m.peopleState.cursor < 0 || m.peopleState.cursor >= len(m.peopleState.activity) {
			return m, nil, true
		}
		entry := m.peopleState.activity[m.peopleState.cursor]
		if entry.AnchorMessageID == nil || *entry.AnchorMessageID <= 0 {
			m.peopleState.activityErr = errors.New("open activity: no message detail is available")
			return m, nil, true
		}
		m.peopleState.selectedContentMessage = *entry.AnchorMessageID
		m.peopleState.level = peopleLevelActivityMessage
		m.messageDetail = nil
		m.peopleState.activityErr = nil
		m.peopleState.requestID++
		m.peopleState.messageLoading = true
		m.loading = true
		return m, tea.Batch(
			m.startSpinner(), m.loadPeopleActivityMessage(*entry.AnchorMessageID),
		), true
	case "r":
		if m.peopleState.activityErr == nil {
			return m, nil, true
		}
		m.peopleState.requestID++
		m.peopleState.activity = nil
		m.peopleState.activityLoaded = false
		m.peopleState.activityNextCursor = ""
		m.peopleState.activityCacheRevision = ""
		m.peopleState.activityRestarted = false
		m.peopleState.activityErr = nil
		m.peopleState.activityLoading = true
		m.loading = true
		return m, tea.Batch(m.startSpinner(), m.loadPeopleActivity("", false)), true
	}
	return m, nil, false
}

func (m *Model) maybeLoadMorePeopleActivity() tea.Cmd {
	if len(m.peopleState.activity) == 0 ||
		m.peopleState.cursor < max(len(m.peopleState.activity)-5, 0) ||
		m.peopleState.activityLoading || m.peopleState.activityLoadingMore ||
		m.peopleState.activityNextCursor == "" || m.peopleState.activityErr != nil {
		return nil
	}
	m.peopleState.requestID++
	m.peopleState.activityLoading = true
	m.peopleState.activityLoadingMore = true
	m.loading = true
	return m.loadPeopleActivity(m.peopleState.activityNextCursor, true)
}

func (m Model) handlePeopleActivityMessageKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if msg.String() == keyNameEsc || msg.String() == keyNameBackspace {
		if m.detailSearchActive || m.detailSearchQuery != "" {
			return m.handleMessageDetailKeys(msg)
		}
		m.peopleState.requestID++
		m.peopleState.level = peopleLevelContact
		m.peopleState.selectedContentMessage = 0
		m.peopleState.messageLoading = false
		m.messageDetail = nil
		m.peopleState.activityErr = nil
		m.updatePeopleLoading()
		return m, nil
	}
	if msg.String() == "left" || msg.String() == "h" ||
		msg.String() == keyNameRight || msg.String() == "l" {
		return m, nil
	}
	if msg.String() == "r" {
		if m.peopleState.activityErr == nil || m.peopleState.selectedContentMessage <= 0 {
			return m, nil
		}
		m.peopleState.requestID++
		m.peopleState.activityErr = nil
		m.peopleState.messageLoading = true
		m.loading = true
		return m, tea.Batch(
			m.startSpinner(), m.loadPeopleActivityMessage(m.peopleState.selectedContentMessage),
		)
	}
	return m.handleMessageDetailKeys(msg)
}

func (m Model) handlePeopleMeetingDetailKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case keyNameEsc, keyNameBackspace:
		m.peopleState.requestID++
		m.peopleState.level = peopleLevelContact
		m.peopleState.selectedContentMessage = 0
		m.meetingState.detail = nil
		m.meetingState.detailScroll = 0
		m.meetingState.detailLoading = false
		m.meetingState.detailSearchActive = false
		m.meetingState.detailSearchQuery = ""
		m.meetingState.detailSearchMatches = nil
		m.peopleState.meetingsErr = nil
		m.updatePeopleLoading()
		return m, nil
	case "left", "h", keyNameRight, "l":
		return m, nil
	case "r":
		if m.peopleState.meetingsErr == nil || m.peopleState.selectedContentMessage <= 0 {
			return m, nil
		}
		m.peopleState.requestID++
		m.peopleState.meetingsErr = nil
		m.meetingState.detailLoading = true
		m.loading = true
		return m, tea.Batch(
			m.startSpinner(), m.loadPeopleMeeting(m.peopleState.selectedContentMessage),
		)
	}
	return m.handleMeetingDetailKeys(msg)
}

func (m *Model) navigatePeopleContentList(key string, count int) bool {
	return m.navigatePeopleList(key, count, max(m.visibleRows()-2, 1))
}

func (m *Model) navigatePeopleList(key string, count, visible int) bool {
	if count == 0 {
		return false
	}
	switch key {
	case "up", "k":
		m.peopleState.cursor = max(m.peopleState.cursor-1, 0)
	case keyNameDown, "j":
		m.peopleState.cursor = min(m.peopleState.cursor+1, count-1)
	case keyNameHome:
		m.peopleState.cursor = 0
	case keyNameEnd, "G":
		m.peopleState.cursor = count - 1
	case keyNamePageUp, keyNameCtrlU:
		m.peopleState.cursor = max(m.peopleState.cursor-visible, 0)
	case keyNamePageDown, keyNameCtrlD:
		m.peopleState.cursor = min(m.peopleState.cursor+visible, count-1)
	default:
		return false
	}
	m.peopleState.scrollOffset = calculateScrollOffset(
		m.peopleState.cursor, m.peopleState.scrollOffset, visible,
	)
	return true
}

func (m Model) handlePeopleInboxKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if m.peopleState.level == peopleLevelMessage {
		return m.handlePeopleMessageKey(msg)
	}
	if m.navigatePeopleInboxList(msg.String()) {
		return m, m.maybeLoadMorePeopleInbox()
	}
	switch msg.String() {
	case keyNameEsc, keyNameBackspace:
		m.peopleState.requestID++
		m.peopleState.inboxesLoading = false
		m.peopleState.conversationsLoading = false
		m.peopleState.conversationLoading = false
		m.peopleState.messageLoading = false
		if m.peopleState.popBreadcrumb() {
			if m.peopleState.level == peopleLevelConversation {
				m.textState.cursor = m.peopleState.cursor
				m.textState.scrollOffset = m.peopleState.scrollOffset
			}
			if m.peopleState.level == peopleLevelDirectory {
				m.peopleState.contact = nil
				m.peopleState.participantID = 0
				m.peopleState.err = nil
				m.peopleState.resetAttributes()
				m.peopleState.resetRelationshipContact()
				m.peopleState.resetInboxes()
			}
		}
		m.updatePeopleLoading()
		return m, nil
	case keyNameTab:
		if m.peopleState.level == peopleLevelInboxTypes {
			return m.activatePeopleTab((m.peopleState.tab + 1) % peopleTabCount)
		}
	case "shift+tab":
		if m.peopleState.level == peopleLevelInboxTypes {
			return m.activatePeopleTab(
				(m.peopleState.tab + peopleTabCount - 1) % peopleTabCount,
			)
		}
	case "r":
		return m.retryPeopleInboxLoad()
	case keyNameEnter:
		return m.drillPeopleInbox()
	}
	return m, nil
}

func (m *Model) maybeLoadMorePeopleInbox() tea.Cmd {
	if m.peopleState.inboxErr != nil || m.peopleState.selectedInboxSource == nil {
		return nil
	}
	switch m.peopleState.level {
	case peopleLevelConversations:
		if m.peopleState.conversationsLoading || m.peopleState.conversationsComplete ||
			len(m.peopleState.conversations) == 0 ||
			m.peopleState.cursor < len(m.peopleState.conversations)-max(m.peopleInboxDataRows(), 1) {
			return nil
		}
		offset := m.peopleState.conversationsNextOffset
		m.peopleState.conversationsPendingOffset = offset
		m.peopleState.conversationsLoading = true
		m.loading = true
		return tea.Batch(m.startSpinner(), m.loadPeopleConversations(*m.peopleState.selectedInboxSource, offset))
	case peopleLevelConversation:
		if m.peopleState.conversationLoading || m.peopleState.conversationComplete ||
			len(m.textState.messages) == 0 ||
			m.textState.cursor < len(m.textState.messages)-max(m.visibleRows(), 1) {
			return nil
		}
		offset := m.peopleState.conversationNextOffset
		m.peopleState.conversationPendingOffset = offset
		m.peopleState.conversationLoading = true
		m.loading = true
		return tea.Batch(m.startSpinner(), m.loadPeopleConversationMessages(
			*m.peopleState.selectedInboxSource, m.peopleState.selectedConversationID, offset,
		))
	case peopleLevelDirectory, peopleLevelContact, peopleLevelInboxTypes,
		peopleLevelInboxSources, peopleLevelMessage, peopleLevelMeetingDetail,
		peopleLevelActivityMessage:
		return nil
	}
	return nil
}

func (m *Model) navigatePeopleInboxList(key string) bool {
	if m.peopleState.level == peopleLevelConversation {
		return m.navigateTextList(key, len(m.textState.messages))
	}
	visible := m.peopleInboxDataRows()
	count := 0
	switch m.peopleState.level {
	case peopleLevelInboxTypes:
		count = len(m.peopleState.inboxTypes)
	case peopleLevelInboxSources:
		if group, ok := m.selectedPeopleInboxType(); ok {
			count = len(group.sources)
		}
	case peopleLevelConversations:
		count = len(m.peopleState.conversations)
	case peopleLevelDirectory, peopleLevelContact, peopleLevelConversation,
		peopleLevelMessage, peopleLevelMeetingDetail, peopleLevelActivityMessage:
		return false
	}
	if count == 0 {
		return false
	}
	switch key {
	case "up", "k":
		m.peopleState.cursor = max(m.peopleState.cursor-1, 0)
	case keyNameDown, "j":
		m.peopleState.cursor = min(m.peopleState.cursor+1, count-1)
	case keyNameHome:
		m.peopleState.cursor = 0
	case keyNameEnd, "G":
		m.peopleState.cursor = count - 1
	case keyNamePageUp, keyNameCtrlU:
		m.peopleState.cursor = max(m.peopleState.cursor-visible, 0)
	case keyNamePageDown, keyNameCtrlD:
		m.peopleState.cursor = min(m.peopleState.cursor+visible, count-1)
	default:
		return false
	}
	m.peopleState.scrollOffset = calculateScrollOffset(
		m.peopleState.cursor, m.peopleState.scrollOffset, visible,
	)
	return true
}

func (m Model) selectedPeopleInboxType() (peopleInboxType, bool) {
	for _, group := range m.peopleState.inboxTypes {
		if group.key == m.peopleState.selectedInboxType {
			return group, true
		}
	}
	return peopleInboxType{}, false
}

func (m Model) drillPeopleInbox() (tea.Model, tea.Cmd) {
	switch m.peopleState.level {
	case peopleLevelInboxTypes:
		if m.peopleState.cursor < 0 || m.peopleState.cursor >= len(m.peopleState.inboxTypes) {
			return m, nil
		}
		selected := m.peopleState.inboxTypes[m.peopleState.cursor]
		m.peopleState.pushBreadcrumb()
		m.peopleState.selectedInboxType = selected.key
		m.peopleState.level = peopleLevelInboxSources
		m.peopleState.cursor = 0
		m.peopleState.scrollOffset = 0
		m.peopleState.inboxErr = nil
		return m, nil
	case peopleLevelInboxSources:
		group, ok := m.selectedPeopleInboxType()
		if !ok || m.peopleState.cursor < 0 || m.peopleState.cursor >= len(group.sources) {
			return m, nil
		}
		selected := group.sources[m.peopleState.cursor]
		m.peopleState.pushBreadcrumb()
		m.peopleState.selectedInboxSource = &selected
		m.peopleState.level = peopleLevelConversations
		m.peopleState.cursor = 0
		m.peopleState.scrollOffset = 0
		m.peopleState.conversations = nil
		m.peopleState.conversationsNextOffset = 0
		m.peopleState.conversationsPendingOffset = 0
		m.peopleState.conversationsComplete = false
		m.peopleState.conversationsCacheRevision = ""
		m.peopleState.conversationsRestarted = false
		m.textState.conversations = nil
		m.peopleState.inboxErr = nil
		m.peopleState.requestID++
		m.peopleState.conversationsLoading = true
		m.loading = true
		return m, tea.Batch(m.startSpinner(), m.loadPeopleConversations(selected, 0))
	case peopleLevelConversations:
		if m.peopleState.cursor < 0 ||
			m.peopleState.cursor >= len(m.peopleState.conversations) ||
			m.peopleState.selectedInboxSource == nil {
			return m, nil
		}
		conversation := m.peopleState.conversations[m.peopleState.cursor]
		m.peopleState.pushBreadcrumb()
		m.peopleState.selectedConversationID = conversation.ConversationID
		m.peopleState.level = peopleLevelConversation
		m.textState.level = textLevelTimeline
		m.textState.selectedConvID = conversation.ConversationID
		m.textState.filter = peopleConversationFilter(
			m.peopleState.contact, *m.peopleState.selectedInboxSource, 0,
		)
		m.textState.cursor = 0
		m.textState.scrollOffset = 0
		m.textState.messages = nil
		m.peopleState.conversationNextOffset = 0
		m.peopleState.conversationPendingOffset = 0
		m.peopleState.conversationComplete = false
		m.peopleState.conversationCacheRevision = ""
		m.peopleState.conversationRestarted = false
		m.peopleState.inboxErr = nil
		m.peopleState.conversationsLoading = false
		m.peopleState.requestID++
		m.peopleState.conversationLoading = true
		m.updatePeopleLoading()
		return m, tea.Batch(
			m.startSpinner(),
			m.loadPeopleConversationMessages(
				*m.peopleState.selectedInboxSource, conversation.ConversationID, 0,
			),
		)
	case peopleLevelConversation:
		if m.peopleState.selectedInboxSource == nil ||
			m.textState.cursor < 0 || m.textState.cursor >= len(m.textState.messages) {
			return m, nil
		}
		message := m.textState.messages[m.textState.cursor]
		m.peopleState.cursor = m.textState.cursor
		m.peopleState.scrollOffset = m.textState.scrollOffset
		m.peopleState.pushBreadcrumb()
		m.peopleState.selectedMessageID = message.ID
		m.peopleState.level = peopleLevelMessage
		m.messageDetail = nil
		m.peopleState.inboxErr = nil
		m.peopleState.conversationLoading = false
		m.peopleState.requestID++
		m.peopleState.messageLoading = true
		m.updatePeopleLoading()
		return m, tea.Batch(
			m.startSpinner(),
			m.loadPeopleMessage(
				*m.peopleState.selectedInboxSource,
				m.peopleState.selectedConversationID, message.ID,
			),
		)
	case peopleLevelDirectory, peopleLevelContact, peopleLevelMessage,
		peopleLevelMeetingDetail, peopleLevelActivityMessage:
		return m, nil
	}
	return m, nil
}

func (m Model) handlePeopleMessageKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if msg.String() == keyNameEsc || msg.String() == keyNameBackspace {
		if m.detailSearchActive || m.detailSearchQuery != "" {
			return m.handleMessageDetailKeys(msg)
		}
		m.peopleState.requestID++
		m.peopleState.messageLoading = false
		if m.peopleState.popBreadcrumb() {
			m.textState.cursor = m.peopleState.cursor
			m.textState.scrollOffset = m.peopleState.scrollOffset
		}
		m.messageDetail = nil
		m.peopleState.inboxErr = nil
		m.updatePeopleLoading()
		return m, nil
	}
	if msg.String() == "left" || msg.String() == "h" ||
		msg.String() == keyNameRight || msg.String() == "l" {
		return m, nil
	}
	return m.handleMessageDetailKeys(msg)
}

func (m Model) retryPeopleInboxLoad() (tea.Model, tea.Cmd) {
	if m.peopleState.inboxErr == nil || m.peopleState.contact == nil {
		return m, nil
	}
	m.peopleState.requestID++
	m.peopleState.inboxErr = nil
	m.loading = true
	spinCmd := m.startSpinner()
	switch m.peopleState.level {
	case peopleLevelInboxTypes:
		m.peopleState.inboxesLoading = true
		return m, tea.Batch(spinCmd, m.loadPeopleInboxes(m.peopleState.participantID))
	case peopleLevelConversations:
		if m.peopleState.selectedInboxSource != nil {
			m.peopleState.conversationsPendingOffset = 0
			m.peopleState.conversationsComplete = false
			m.peopleState.conversationsRestarted = false
			m.peopleState.conversationsLoading = true
			return m, tea.Batch(
				spinCmd, m.loadPeopleConversations(*m.peopleState.selectedInboxSource, 0),
			)
		}
	case peopleLevelConversation:
		if m.peopleState.selectedInboxSource != nil &&
			m.peopleState.selectedConversationID > 0 {
			m.peopleState.conversationPendingOffset = 0
			m.peopleState.conversationComplete = false
			m.peopleState.conversationRestarted = false
			m.peopleState.conversationLoading = true
			return m, tea.Batch(
				spinCmd,
				m.loadPeopleConversationMessages(
					*m.peopleState.selectedInboxSource,
					m.peopleState.selectedConversationID,
					0,
				),
			)
		}
	case peopleLevelMessage:
		if m.peopleState.selectedInboxSource != nil &&
			m.peopleState.selectedConversationID > 0 &&
			m.peopleState.selectedMessageID > 0 {
			m.peopleState.messageLoading = true
			return m, tea.Batch(
				spinCmd,
				m.loadPeopleMessage(
					*m.peopleState.selectedInboxSource,
					m.peopleState.selectedConversationID,
					m.peopleState.selectedMessageID,
				),
			)
		}
	case peopleLevelDirectory, peopleLevelContact, peopleLevelInboxSources,
		peopleLevelMeetingDetail, peopleLevelActivityMessage:
		// These levels have no retryable inbox request.
	}
	m.updatePeopleLoading()
	return m, nil
}

func (m Model) handlePeopleAttributesKey(
	msg tea.KeyPressMsg,
) (tea.Model, tea.Cmd, bool) {
	contact := m.peopleState.contact
	if contact == nil {
		return m, nil, false
	}
	if m.navigatePeopleAttributes(msg.String()) {
		m.peopleState.attributesNotice = ""
		return m, nil, true
	}
	switch msg.String() {
	case "n":
		if contact.Profile == nil {
			m.peopleState.attributesNotice = peoplePromotionInstruction
			return m, nil, true
		}
		m.peopleState.form = newPeopleFieldForm()
		m.peopleState.attributesNotice = ""
		return m, textinput.Blink, true
	case keyNameEnter:
		if contact.Profile == nil {
			m.peopleState.attributesNotice = peoplePromotionInstruction
			return m, nil, true
		}
		group, value, ok := m.selectedPeopleAttribute()
		if !ok {
			m.peopleState.attributesNotice = "No attribute field selected. Press n to create one."
			return m, nil, true
		}
		if value != nil {
			m.peopleState.attributesNotice = "Press e to edit the selected value."
			return m, nil, true
		}
		return m.openPeopleAttributeAdd(group), nil, true
	case "e":
		if contact.Profile == nil {
			m.peopleState.attributesNotice = peoplePromotionInstruction
			return m, nil, true
		}
		group, value, ok := m.selectedPeopleAttribute()
		if !ok {
			m.peopleState.attributesNotice = "No attribute value selected."
			return m, nil, true
		}
		return m.openPeopleAttributeEdit(group, value), nil, true
	case "r":
		if contact.Profile == nil || m.peopleState.attributesLoading {
			return m, nil, true
		}
		m.peopleState.requestID++
		m.peopleState.attributesLoading = true
		m.peopleState.attributesNotice = ""
		m.loading = true
		spinCmd := m.startSpinner()
		return m, tea.Batch(
			spinCmd,
			m.loadPeopleAttributes(contact.Profile.ID, peopleTabAttributes),
		), true
	}
	return m, nil, false
}

func (m Model) handlePeopleSearchInput(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "up", "ctrl+p":
		if len(m.peopleState.completions) > 0 {
			m.peopleState.completionCursor = max(m.peopleState.completionCursor-1, 0)
			return m, nil
		}
	case keyNameDown, "ctrl+n":
		if len(m.peopleState.completions) > 0 {
			m.peopleState.completionCursor = min(
				m.peopleState.completionCursor+1, len(m.peopleState.completions)-1,
			)
			return m, nil
		}
	case keyNameTab:
		if completion, ok := m.selectedPeopleCompletion(); ok {
			m.peopleState.searchInput.SetValue(completion.Value)
			return m.schedulePeopleSearchDebounce(nil)
		}
	case keyNameEnter:
		if completion, ok := m.selectedPeopleCompletion(); ok {
			m.peopleState.searchActive = false
			m.peopleState.searchInput.Blur()
			m.peopleState.searchDebounceID++
			m.peopleState.requestID++
			m.peopleState.clearCompletions()
			return m.openPeopleContact(completion.ParticipantID)
		}
		m.peopleState.searchActive = false
		m.peopleState.searchInput.Blur()
		m.peopleState.searchDebounceID++
		m.peopleState.requestID++
		m.peopleState.searchQuery = strings.TrimSpace(m.peopleState.searchInput.Value())
		m.peopleState.clearCompletions()
		return m.startPeopleDirectoryLoad()
	case keyNameEsc:
		restoreQuery := m.peopleState.searchRestoreQuery
		restartCommittedQuery := !m.peopleState.initialized ||
			(m.peopleState.directoryLoading && !m.peopleState.loadingMore) ||
			m.peopleState.searchQuery != restoreQuery
		m.peopleState.searchActive = false
		m.peopleState.searchInput.Blur()
		m.peopleState.searchQuery = restoreQuery
		m.peopleState.searchInput.SetValue(restoreQuery)
		m.peopleState.searchDebounceID++
		m.peopleState.requestID++
		m.peopleState.clearCompletions()
		if restartCommittedQuery {
			return m.startPeopleDirectoryLoad()
		}
		m.settlePeopleDirectoryLoad()
		return m, nil
	case keyNameCtrlC:
		m.quitting = true
		return m, tea.Quit
	default:
		var inputCmd tea.Cmd
		m.peopleState.searchInput, inputCmd = m.peopleState.searchInput.Update(msg)
		return m.schedulePeopleSearchDebounce(inputCmd)
	}
	return m, nil
}

func (m Model) selectedPeopleCompletion() (peoplebrowser.CompletionRow, bool) {
	if m.peopleState.completionCursor < 0 ||
		m.peopleState.completionCursor >= len(m.peopleState.completions) {
		return peoplebrowser.CompletionRow{}, false
	}
	return m.peopleState.completions[m.peopleState.completionCursor], true
}

func (m Model) schedulePeopleSearchDebounce(inputCmd tea.Cmd) (tea.Model, tea.Cmd) {
	m.peopleState.searchDebounceID++
	m.peopleState.requestID++
	m.settlePeopleDirectoryLoad()
	m.peopleState.clearCompletions()
	debounce := peopleSearchDebounceMsg{
		query:      strings.TrimSpace(m.peopleState.searchInput.Value()),
		debounceID: m.peopleState.searchDebounceID, requestID: m.peopleState.requestID,
		presentationGeneration: m.presentationGeneration,
	}
	debounceCmd := tea.Tick(peopleSearchDebounceDelay, func(time.Time) tea.Msg { return debounce })
	return m, tea.Batch(inputCmd, debounceCmd)
}

func (m Model) handlePeopleSearchDebounce(msg peopleSearchDebounceMsg) (tea.Model, tea.Cmd) {
	if m.mode != modePeople || msg.presentationGeneration != m.presentationGeneration ||
		msg.requestID != m.peopleState.requestID ||
		msg.debounceID != m.peopleState.searchDebounceID || !m.peopleState.searchActive {
		return m, nil
	}
	m.peopleState.searchQuery = msg.query
	m, directoryCmd := m.startPeopleDirectoryLoad()
	if msg.query == "" {
		m.peopleState.clearCompletions()
		return m, directoryCmd
	}
	m.peopleState.completionLoading = true
	m.peopleState.completionErr = nil
	m.peopleState.completions = nil
	m.peopleState.completionCursor = 0
	m.peopleState.completionCacheRevision = ""
	return m, tea.Batch(directoryCmd, m.loadPeopleCompletions(msg.query))
}

func (m Model) openPeopleContact(participantID int64) (tea.Model, tea.Cmd) {
	if participantID <= 0 {
		return m, nil
	}
	m.settlePeopleDirectoryLoad()
	m.peopleState.pushBreadcrumb()
	m.peopleState.level = peopleLevelContact
	m.peopleState.tab = peopleTabOverview
	m.peopleState.participantID = participantID
	m.peopleState.scrollOffset = 0
	m.peopleState.contact = nil
	m.peopleState.err = nil
	m.peopleState.resetAttributes()
	m.peopleState.resetRelationshipContact()
	m.peopleState.resetInboxes()
	m.peopleState.resetContent()
	m.peopleState.requestID++
	m.peopleState.contactLoading = true
	m.loading = true
	return m, tea.Batch(m.startSpinner(), m.loadPeopleContact(participantID))
}

func (m Model) startPeopleDirectoryLoad() (Model, tea.Cmd) {
	m.peopleState.initialized = false
	m.peopleState.rows = nil
	m.peopleState.totalCount = 0
	m.peopleState.nextCursor = ""
	m.peopleState.cacheRevision = ""
	m.peopleState.cursor = 0
	m.peopleState.scrollOffset = 0
	m.peopleState.paginationRestarted = false
	m.peopleState.err = nil
	m.peopleState.directoryLoading = true
	m.peopleState.loadingMore = false
	m.loading = true
	spinCmd := m.startSpinner()
	return m, tea.Batch(spinCmd, m.loadPeopleDirectory("", false))
}

func (m Model) reloadPeopleDirectory() (tea.Model, tea.Cmd) {
	m.peopleState.requestID++
	return m.startPeopleDirectoryLoad()
}

func (m *Model) navigatePeopleDirectory(key string) bool {
	count := len(m.peopleState.rows)
	changed := false
	switch key {
	case "up", "k":
		if m.peopleState.cursor > 0 {
			m.peopleState.cursor--
			changed = true
		}
	case keyNameDown, "j":
		if m.peopleState.cursor < count-1 {
			m.peopleState.cursor++
			changed = true
		}
	case keyNameHome:
		m.peopleState.cursor = 0
		m.peopleState.scrollOffset = 0
		return true
	case keyNameEnd, "G":
		m.peopleState.cursor = max(count-1, 0)
		changed = true
	case keyNamePageUp, keyNameCtrlU:
		m.peopleState.cursor = max(m.peopleState.cursor-m.visibleRows(), 0)
		changed = true
	case keyNamePageDown, keyNameCtrlD:
		m.peopleState.cursor = min(m.peopleState.cursor+m.visibleRows(), max(count-1, 0))
		changed = true
	default:
		return false
	}
	if changed {
		m.peopleState.scrollOffset = calculateScrollOffset(
			m.peopleState.cursor, m.peopleState.scrollOffset, m.visibleRows(),
		)
	}
	return true
}

func (m *Model) maybeLoadMorePeople() tea.Cmd {
	if len(m.peopleState.rows) == 0 || m.peopleState.nextCursor == "" ||
		m.peopleState.loadingMore || m.peopleState.directoryLoading ||
		m.peopleState.err != nil ||
		m.peopleState.cursor < max(len(m.peopleState.rows)-5, 0) {
		return nil
	}
	m.peopleState.requestID++
	m.peopleState.directoryLoading = true
	m.peopleState.loadingMore = true
	m.loading = true
	return m.loadPeopleDirectory(m.peopleState.nextCursor, true)
}
