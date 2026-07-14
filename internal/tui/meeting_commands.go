package tui

import (
	"context"
	"fmt"

	tea "charm.land/bubbletea/v2"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/search"
)

type meetingMessagesLoadedMsg struct {
	messages  []query.MessageSummary
	err       error
	requestID uint64
	append    bool
}

type meetingSearchLoadedMsg struct {
	messages  []query.MessageSummary
	err       error
	requestID uint64
	offset    int
}

type meetingDetailLoadedMsg struct {
	detail    *query.MessageDetail
	err       error
	requestID uint64
}

func (m Model) loadMeetingMessages() tea.Cmd {
	return m.loadMeetingMessagesWithOffset(0, false)
}

func (m Model) loadMeetingMessagesWithOffset(offset int, appendResults bool) tea.Cmd {
	engine := m.engine
	filter := m.meetingMessageFilter()
	filter.Pagination.Offset = offset
	requestID := m.meetingState.requestID
	return safeCmdWithPanic(
		func() tea.Msg {
			messages, err := engine.ListMessages(context.Background(), filter)
			return meetingMessagesLoadedMsg{
				messages: messages, err: err, requestID: requestID, append: appendResults,
			}
		},
		func(r any) tea.Msg {
			return meetingMessagesLoadedMsg{
				err:       fmt.Errorf("meeting messages panic: %v", r),
				requestID: requestID,
				append:    appendResults,
			}
		},
	)
}

func (m Model) handleMeetingMessagesLoaded(msg meetingMessagesLoadedMsg) (tea.Model, tea.Cmd) {
	if msg.requestID != m.meetingState.requestID {
		return m, nil
	}
	if m.mode == modeMeetings {
		m.loading = false
	}
	m.meetingState.listLoadingMore = false
	if msg.err != nil {
		if m.mode == modeMeetings {
			m.err = query.HintRepairEncoding(msg.err)
			m.modal = modalError
			m.modalResult = m.err.Error()
		}
		return m, nil
	}
	if m.mode == modeMeetings {
		m.err = nil
	}
	m.meetingState.initialized = true
	if msg.append {
		m.meetingState.messages = append(m.meetingState.messages, msg.messages...)
	} else {
		m.meetingState.messages = msg.messages
		m.meetingState.cursor = 0
		m.meetingState.scrollOffset = 0
	}
	m.meetingState.listOffset = len(m.meetingState.messages)
	m.meetingState.listComplete = len(msg.messages) < messageListPageSize
	return m, nil
}

func (m Model) loadMeetingSearch(queryString string, offset int, _ bool) tea.Cmd {
	engine := m.engine
	filter := m.meetingMessageFilter()
	requestID := m.meetingState.searchRequestID
	return safeCmdWithPanic(
		func() tea.Msg {
			parsed := search.Parse(queryString)
			if err := parsed.Err(); err != nil {
				return meetingSearchLoadedMsg{err: err, requestID: requestID, offset: offset}
			}
			merged := query.MergeFilterIntoQuery(parsed, filter)
			messages, err := engine.Search(context.Background(), merged, searchPageSize, offset)
			return meetingSearchLoadedMsg{
				messages: messages,
				err:      err, requestID: requestID, offset: offset,
			}
		},
		func(r any) tea.Msg {
			return meetingSearchLoadedMsg{
				err: fmt.Errorf("meeting search panic: %v", r), requestID: requestID, offset: offset,
			}
		},
	)
}

func (m Model) handleMeetingSearchLoaded(msg meetingSearchLoadedMsg) (tea.Model, tea.Cmd) {
	if msg.requestID != m.meetingState.searchRequestID {
		return m, nil
	}
	if m.mode == modeMeetings {
		m.loading = false
	}
	m.meetingState.listLoadingMore = false
	if msg.err != nil {
		if m.mode == modeMeetings {
			m.err = query.HintRepairEncoding(msg.err)
			m.modal = modalError
			m.modalResult = m.err.Error()
		}
		return m, nil
	}
	m.err = nil
	m.meetingState.initialized = true
	if msg.offset > 0 {
		m.meetingState.messages = append(m.meetingState.messages, msg.messages...)
	} else {
		m.meetingState.messages = msg.messages
		m.meetingState.cursor = 0
		m.meetingState.scrollOffset = 0
	}
	m.meetingState.searchOffset = len(m.meetingState.messages)
	m.meetingState.searchComplete = len(msg.messages) < searchPageSize
	return m, nil
}

func (m Model) loadMeetingDetail(id int64) tea.Cmd {
	engine := m.engine
	requestID := m.meetingState.detailRequestID
	return safeCmdWithPanic(
		func() tea.Msg {
			detail, err := engine.GetMessage(context.Background(), id)
			return meetingDetailLoadedMsg{detail: detail, err: err, requestID: requestID}
		},
		func(r any) tea.Msg {
			return meetingDetailLoadedMsg{
				err: fmt.Errorf("meeting detail panic: %v", r), requestID: requestID,
			}
		},
	)
}

func (m Model) handleMeetingDetailLoaded(msg meetingDetailLoadedMsg) (tea.Model, tea.Cmd) {
	if msg.requestID != m.meetingState.detailRequestID {
		return m, nil
	}
	if m.mode == modeMeetings {
		m.loading = false
	}
	if msg.err != nil {
		if m.mode == modeMeetings {
			m.err = query.HintRepairEncoding(msg.err)
			m.modal = modalError
			m.modalResult = m.err.Error()
		}
		return m, nil
	}
	m.err = nil
	m.meetingState.detail = msg.detail
	if m.meetingState.detailSearchQuery != "" {
		m.findMeetingDetailMatches()
		if len(m.meetingState.detailSearchMatches) > 0 {
			m.scrollToMeetingDetailMatch()
		}
	}
	return m, nil
}
