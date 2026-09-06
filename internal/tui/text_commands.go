package tui

import (
	"context"
	"errors"
	"fmt"

	tea "charm.land/bubbletea/v2"
	"go.kenn.io/msgvault/internal/query"
)

// textConversationsLoadedMsg is sent when text conversations are loaded.
type textConversationsLoadedMsg struct {
	conversations          []query.ConversationRow
	stats                  *query.TotalStats
	err                    error
	requestID              uint64
	presentationGeneration uint64
}

// textAggregateLoadedMsg is sent when text aggregate data is loaded.
type textAggregateLoadedMsg struct {
	rows                   []query.AggregateRow
	stats                  *query.TotalStats
	err                    error
	requestID              uint64
	presentationGeneration uint64
}

// textMessagesLoadedMsg is sent when text conversation messages are loaded.
type textMessagesLoadedMsg struct {
	messages               []query.MessageSummary
	err                    error
	requestID              uint64
	presentationGeneration uint64
}

type textMessageLoadedMsg struct {
	detail                 *query.MessageDetail
	err                    error
	requestID              uint64
	conversationID         int64
	messageID              int64
	presentationGeneration uint64
}

// textSearchResultMsg is sent when text search results are loaded.
type textSearchResultMsg struct {
	messages               []query.MessageSummary
	err                    error
	requestID              uint64
	presentationGeneration uint64
}

// textStatsLoadedMsg is sent when text stats are loaded.
type textStatsLoadedMsg struct {
	stats *query.TotalStats
	err   error
}

// loadTextConversations fetches text conversations matching the current filter.
func (m *Model) nextTextRequestID() uint64 {
	m.textRequestID++
	return m.textRequestID
}

func (m *Model) loadTextConversations() tea.Cmd {
	te := m.textEngine
	filter := m.textState.filter
	requestID := m.nextTextRequestID()
	presentationGeneration := m.presentationGeneration
	return safeCmdWithPanic(
		func() tea.Msg {
			ctx := context.Background()
			convs, err := te.ListConversations(ctx, filter)
			if err != nil {
				return textConversationsLoadedMsg{
					err: err, requestID: requestID, presentationGeneration: presentationGeneration,
				}
			}
			stats, _ := te.GetTextStats(ctx, query.TextStatsOptions{
				SourceID: filter.SourceID,
			})
			return textConversationsLoadedMsg{
				conversations: convs, stats: stats,
				requestID: requestID, presentationGeneration: presentationGeneration,
			}
		},
		func(r any) tea.Msg {
			return textConversationsLoadedMsg{
				err:                    fmt.Errorf("text conversations panic: %v", r),
				requestID:              requestID,
				presentationGeneration: presentationGeneration,
			}
		},
	)
}

// loadTextAggregate fetches text aggregate data for the current view type.
func (m *Model) loadTextAggregate() tea.Cmd {
	te := m.textEngine
	vt := m.textState.viewType
	filter := m.textState.filter
	requestID := m.nextTextRequestID()
	presentationGeneration := m.presentationGeneration
	return safeCmdWithPanic(
		func() tea.Msg {
			ctx := context.Background()
			opts := query.TextAggregateOptions{
				SourceID:      filter.SourceID,
				After:         filter.After,
				Before:        filter.Before,
				SortField:     filter.SortField,
				SortDirection: filter.SortDirection,
				Limit:         defaultAggregateLimit,
			}
			rows, err := te.TextAggregate(ctx, vt, opts)
			if err != nil {
				return textAggregateLoadedMsg{
					err: err, requestID: requestID, presentationGeneration: presentationGeneration,
				}
			}
			stats, _ := te.GetTextStats(ctx, query.TextStatsOptions{
				SourceID: filter.SourceID,
			})
			return textAggregateLoadedMsg{
				rows: rows, stats: stats, requestID: requestID,
				presentationGeneration: presentationGeneration,
			}
		},
		func(r any) tea.Msg {
			return textAggregateLoadedMsg{
				err:                    fmt.Errorf("text aggregate panic: %v", r),
				requestID:              requestID,
				presentationGeneration: presentationGeneration,
			}
		},
	)
}

// loadTextMessages fetches messages for the selected conversation.
func (m *Model) loadTextMessages() tea.Cmd {
	te := m.textEngine
	convID := m.textState.selectedConvID
	filter := m.textState.filter
	requestID := m.nextTextRequestID()
	presentationGeneration := m.presentationGeneration
	return safeCmdWithPanic(
		func() tea.Msg {
			msgs, err := te.ListConversationMessages(
				context.Background(), convID, filter,
			)
			return textMessagesLoadedMsg{
				messages: msgs, err: err, requestID: requestID,
				presentationGeneration: presentationGeneration,
			}
		},
		func(r any) tea.Msg {
			return textMessagesLoadedMsg{
				err:                    fmt.Errorf("text messages panic: %v", r),
				requestID:              requestID,
				presentationGeneration: presentationGeneration,
			}
		},
	)
}

func (m *Model) loadTextMessage(messageID int64) tea.Cmd {
	engine := m.engine
	requestID := m.nextTextRequestID()
	conversationID := m.textState.selectedConvID
	presentationGeneration := m.presentationGeneration
	return safeCmdWithPanic(
		func() tea.Msg {
			detail, err := engine.GetMessage(context.Background(), messageID)
			if err == nil && detail == nil {
				err = errors.New("message detail is empty")
			}
			return textMessageLoadedMsg{
				detail: detail, err: err, requestID: requestID,
				conversationID: conversationID, messageID: messageID,
				presentationGeneration: presentationGeneration,
			}
		},
		func(r any) tea.Msg {
			return textMessageLoadedMsg{
				err: fmt.Errorf("text message detail panic: %v", r), requestID: requestID,
				conversationID: conversationID, messageID: messageID,
				presentationGeneration: presentationGeneration,
			}
		},
	)
}

func (m Model) handleTextMessageLoaded(msg textMessageLoadedMsg) (tea.Model, tea.Cmd) {
	if m.mode != modeTexts || m.presentationGeneration != msg.presentationGeneration ||
		m.textRequestID != msg.requestID || m.textState.level != textLevelDetail ||
		m.textState.selectedConvID != msg.conversationID ||
		m.textState.selectedMessageID != msg.messageID {
		return m, nil
	}
	m.finishModePresentation(modeTexts, msg.presentationGeneration)
	if msg.err != nil {
		m.err = query.HintRepairEncoding(msg.err)
		m.modal = modalError
		m.modalResult = m.err.Error()
		return m, nil
	}
	m.err = nil
	if m.modal == modalError {
		m.modal = modalNone
	}
	m.messageDetail = msg.detail
	m.detailScroll = 0
	m.updateDetailLineCount()
	return m, nil
}

// loadTextSearch executes a text message search.
func (m *Model) loadTextSearch(searchQuery string) tea.Cmd {
	te := m.textEngine
	requestID := m.nextTextRequestID()
	presentationGeneration := m.presentationGeneration
	return safeCmdWithPanic(
		func() tea.Msg {
			msgs, err := te.TextSearch(
				context.Background(), searchQuery, 100, 0,
			)
			return textSearchResultMsg{
				messages: msgs, err: err, requestID: requestID,
				presentationGeneration: presentationGeneration,
			}
		},
		func(r any) tea.Msg {
			return textSearchResultMsg{
				err:                    fmt.Errorf("text search panic: %v", r),
				requestID:              requestID,
				presentationGeneration: presentationGeneration,
			}
		},
	)
}

// loadTextData dispatches the appropriate load command based on the current
// navigation level. Checking level (not just viewType) is necessary because
// drill-down from an aggregate keeps the aggregate viewType but should load
// conversations.
func (m *Model) loadTextData() tea.Cmd {
	switch m.textState.level {
	case textLevelDrillConversations, textLevelConversations:
		return m.loadTextConversations()
	case textLevelTimeline:
		return m.loadTextMessages()
	case textLevelDetail:
		return nil
	default:
		return m.loadTextAggregate()
	}
}

func (m *Model) textPresentationLoadCmd() tea.Cmd {
	switch {
	case m.textState.level == textLevelDetail:
		if m.messageDetail == nil && m.textState.selectedMessageID > 0 {
			return m.loadTextMessage(m.textState.selectedMessageID)
		}
		return nil
	case m.textState.level == textLevelTimeline && m.textState.globalSearchTimeline:
		return nil
	default:
		return m.loadTextData()
	}
}
