package tui

import (
	"context"
	"errors"
	"fmt"

	tea "charm.land/bubbletea/v2"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/timeline"
)

type timelineLoadedMsg struct {
	result                *timeline.Timeline
	err                   error
	requestID, generation uint64
}

type timelineReaderLoadedMsg struct {
	detail                *query.MessageDetail
	messages              []query.MessageSummary
	truncated             bool
	err                   error
	requestID, generation uint64
}

func (m *Model) startTimelineRequest(req timeline.Request, remember bool) tea.Cmd {
	if m.timelineBackend == nil {
		return nil
	}
	if remember && m.timelineState.result != nil && !m.timelineState.needsLoad {
		m.timelineState.history = append(m.timelineState.history, timelineNavigation{
			request: m.timelineState.request, result: m.timelineState.result,
			cursor: m.timelineState.cursor, scroll: m.timelineState.scroll,
		})
	}
	m.cancelTimelineRequest()
	req.Format = "json"
	m.timelineState.request = req
	m.timelineState.needsLoad = true
	// Pending requests have no selectable stale entries. Esc restores the
	// complete prior result and selection from history.
	m.timelineState.result = nil
	m.timelineState.lines = nil
	m.timelineState.entries = nil
	m.timelineState.cursor, m.timelineState.scroll = 0, 0
	m.timelineState.reader = timelineReaderNone
	m.timelineState.loading = true
	m.timelineState.err = nil
	m.loading = true
	ctx, cancel := context.WithCancel(context.Background())
	m.timelineState.cancel = cancel
	backend := m.timelineBackend
	requestID, generation := m.timelineState.requestID, m.presentationGeneration
	return safeCmdWithPanic(func() tea.Msg {
		result, _, err := backend.Timeline(ctx, req)
		return timelineLoadedMsg{result: result, err: err, requestID: requestID, generation: generation}
	}, func(r any) tea.Msg {
		return timelineLoadedMsg{err: fmt.Errorf("timeline panic: %v", r), requestID: requestID, generation: generation}
	})
}

func (m Model) handleTimelineLoaded(msg timelineLoadedMsg) (tea.Model, tea.Cmd) {
	if m.mode != modeTimeline || msg.generation != m.presentationGeneration || msg.requestID != m.timelineState.requestID {
		return m, nil
	}
	m.cancelTimelineRequest()
	m.loading = false
	m.timelineState.err = msg.err
	if msg.err == nil {
		if msg.result == nil {
			m.timelineState.err = errors.New("timeline: empty response")
		} else {
			m.timelineState.needsLoad = false
			m.timelineState.result = msg.result
			m.timelineState.cursor, m.timelineState.scroll = 0, 0
			m.rebuildTimelineRows()
		}
	}
	return m, nil
}

func (m *Model) loadTimelineReader(reader timelineReader, id int64) tea.Cmd {
	m.cancelTimelineRequest()
	m.timelineState.reader = reader
	m.timelineState.readerID = id
	m.timelineState.loading = true
	m.timelineState.err = nil
	m.loading = true
	ctx, cancel := context.WithCancel(context.Background())
	m.timelineState.cancel = cancel
	requestID, generation := m.timelineState.requestID, m.presentationGeneration
	engine, limit := m.engine, m.threadMessageLimit
	return safeCmdWithPanic(func() tea.Msg {
		msg := timelineReaderLoadedMsg{requestID: requestID, generation: generation}
		if reader == timelineReaderThread {
			msg.messages, msg.err = engine.ListMessages(ctx, query.MessageFilter{
				ConversationID: &id,
				Sorting:        query.MessageSorting{Field: query.MessageSortByDate, Direction: query.SortAsc},
				Pagination:     query.Pagination{Limit: limit + 1},
			})
			if len(msg.messages) > limit {
				msg.messages = msg.messages[:limit]
				msg.truncated = true
			}
		} else {
			msg.detail, msg.err = engine.GetMessage(ctx, id)
			if msg.err == nil && (msg.detail == nil || msg.detail.ID != id) {
				msg.err = errors.New("timeline reader: empty or mismatched response")
			}
		}
		return msg
	}, func(r any) tea.Msg {
		return timelineReaderLoadedMsg{err: fmt.Errorf("timeline reader panic: %v", r), requestID: requestID, generation: generation}
	})
}

func (m Model) handleTimelineReaderLoaded(msg timelineReaderLoadedMsg) (tea.Model, tea.Cmd) {
	if m.mode != modeTimeline || msg.generation != m.presentationGeneration || msg.requestID != m.timelineState.requestID || m.timelineState.reader == timelineReaderNone {
		return m, nil
	}
	m.cancelTimelineRequest()
	m.loading = false
	m.timelineState.err = msg.err
	if msg.err != nil {
		return m, nil
	}
	switch m.timelineState.reader {
	case timelineReaderThread:
		m.timelineState.messages = msg.messages
		m.timelineState.threadTruncated = msg.truncated
		m.timelineState.threadCursor, m.timelineState.threadScroll = 0, 0
	case timelineReaderMeeting:
		m.timelineState.meeting.detail = msg.detail
		m.timelineState.meeting.detailScroll = 0
	case timelineReaderMessage:
		m.messageReaderState = messageReaderState{messageDetail: msg.detail}
		m.updateDetailLineCount()
	}
	return m, nil
}

func (m *Model) activateTimeline() tea.Cmd {
	// Re-entering resumes a cancelled load, including a parked reader load.
	if m.timelineState.reader != timelineReaderNone {
		switch m.timelineState.reader {
		case timelineReaderMessage:
			if m.messageDetail != nil {
				m.loading = false
				return nil
			}
		case timelineReaderMeeting:
			if m.timelineState.meeting.detail != nil {
				m.loading = false
				return nil
			}
		case timelineReaderThread:
			if m.timelineState.messages != nil {
				m.loading = false
				return nil
			}
		}
		return m.loadTimelineReader(m.timelineState.reader, m.timelineState.readerID)
	}
	if m.timelineState.result != nil && !m.timelineState.needsLoad {
		m.loading = false
		return nil
	}
	req := m.timelineState.request
	if req.Mode == "" {
		req.Mode = timeline.ModeDay
	}
	return m.startTimelineRequest(req, false)
}
