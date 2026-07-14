package tui

import (
	"strings"

	"go.kenn.io/msgvault/internal/query"
)

const meetingMessageType = "meeting_transcript"

// meetingState holds state that must remain independent from Email and Texts.
type meetingState struct {
	sourceID  *int64
	messages  []query.MessageSummary
	requestID uint64
}

func (m Model) meetingMessageFilter() query.MessageFilter {
	filter := query.MessageFilter{
		MessageType: meetingMessageType,
		SourceID:    m.meetingState.sourceID,
		Sorting: query.MessageSorting{
			Field:     query.MessageSortByDate,
			Direction: query.SortDesc,
		},
	}
	filter.Pagination.Limit = messageListPageSize
	return filter
}

func (m Model) meetingAccounts() []query.AccountInfo {
	accounts := make([]query.AccountInfo, 0, len(m.accounts))
	for _, account := range m.accounts {
		switch strings.ToLower(strings.TrimSpace(account.SourceType)) {
		case "granola", "circleback":
			accounts = append(accounts, account)
		}
	}
	return accounts
}

func (m Model) selectableAccounts() []query.AccountInfo {
	if m.mode == modeMeetings {
		return m.meetingAccounts()
	}
	return m.accounts
}
