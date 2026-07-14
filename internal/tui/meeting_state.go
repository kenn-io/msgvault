package tui

import (
	"strings"

	"charm.land/bubbles/v2/textinput"
	"go.kenn.io/msgvault/internal/query"
)

const meetingMessageType = "meeting_transcript"

const (
	meetingSourceGranola    = "granola"
	meetingSourceCircleback = "circleback"
)

type meetingViewLevel int

const (
	meetingLevelList meetingViewLevel = iota
	meetingLevelDetail
)

// meetingState holds state that must remain independent from Email and Texts.
type meetingState struct {
	level                  meetingViewLevel
	initialized            bool
	sourceID               *int64
	messages               []query.MessageSummary
	cursor                 int
	scrollOffset           int
	requestID              uint64
	sortField              query.MessageSortField
	sortDirection          query.SortDirection
	detail                 *query.MessageDetail
	detailScroll           int
	detailRequestID        uint64
	detailSearchActive     bool
	detailSearchInput      textinput.Model
	detailSearchQuery      string
	detailSearchMatches    []int
	detailSearchMatchIndex int
	listOffset             int
	listComplete           bool
	listLoadingMore        bool
	listLoading            bool
	searchLoading          bool
	detailLoading          bool

	searchActive    bool
	searchInput     textinput.Model
	searchQuery     string
	searchRequestID uint64
	preSearch       []query.MessageSummary
	// searchSnapshotInvalid marks that preSearch belonged to a previous
	// source. Clearing the query must load the selected source's normal list.
	searchSnapshotInvalid bool
	searchOffset          int
	searchComplete        bool
}

func (m Model) meetingMessageFilter() query.MessageFilter {
	filter := query.MessageFilter{
		MessageType: meetingMessageType,
		SourceID:    m.meetingState.sourceID,
		Sorting: query.MessageSorting{
			Field:     m.meetingState.sortField,
			Direction: m.meetingState.sortDirection,
		},
	}
	filter.Pagination.Limit = messageListPageSize
	return filter
}

func (m Model) meetingAccounts() []query.AccountInfo {
	accounts := make([]query.AccountInfo, 0, len(m.accounts))
	for _, account := range m.accounts {
		switch strings.ToLower(strings.TrimSpace(account.SourceType)) {
		case meetingSourceGranola, meetingSourceCircleback:
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
