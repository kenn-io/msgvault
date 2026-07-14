package tui

import (
	"context"
	"fmt"

	tea "charm.land/bubbletea/v2"
	"go.kenn.io/msgvault/internal/query"
)

type meetingMessagesLoadedMsg struct {
	messages  []query.MessageSummary
	err       error
	requestID uint64
}

func (m Model) loadMeetingMessages() tea.Cmd {
	engine := m.engine
	filter := m.meetingMessageFilter()
	requestID := m.meetingState.requestID
	return safeCmdWithPanic(
		func() tea.Msg {
			messages, err := engine.ListMessages(context.Background(), filter)
			return meetingMessagesLoadedMsg{messages: messages, err: err, requestID: requestID}
		},
		func(r any) tea.Msg {
			return meetingMessagesLoadedMsg{
				err:       fmt.Errorf("meeting messages panic: %v", r),
				requestID: requestID,
			}
		},
	)
}

func (m Model) handleMeetingMessagesLoaded(msg meetingMessagesLoadedMsg) (tea.Model, tea.Cmd) {
	if msg.requestID != m.meetingState.requestID {
		return m, nil
	}
	m.loading = false
	if msg.err != nil {
		m.err = query.HintRepairEncoding(msg.err)
		m.modal = modalError
		m.modalResult = m.err.Error()
		return m, nil
	}
	m.err = nil
	m.meetingState.messages = msg.messages
	return m, nil
}
