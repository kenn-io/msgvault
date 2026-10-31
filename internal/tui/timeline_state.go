package tui

import (
    "context"

    "go.kenn.io/msgvault/internal/query"
    "go.kenn.io/msgvault/internal/timeline"
)

// TimelineBackend reads the selected daemon's composed archive timeline.
type TimelineBackend interface {
    Timeline(context.Context, timeline.Request) (*timeline.Timeline, string, error)
}

type timelineReader uint8
const (
    timelineReaderNone timelineReader = iota
    timelineReaderMessage
    timelineReaderThread
    timelineReaderMeeting
)

type timelineSelectableEntry struct {
    entry timeline.Entry
    line int
}

type timelineNavigation struct {
    request timeline.Request
    result *timeline.Timeline
    cursor, scroll int
}

type timelineState struct {
    request timeline.Request
    result *timeline.Timeline
    lines []string
    entries []timelineSelectableEntry
    cursor, scroll int
    history []timelineNavigation
    requestID uint64
    cancel context.CancelFunc
    loading bool
    needsLoad bool
    err error
    reader timelineReader
    readerID int64
    threadID int64
    readerFromThread bool
    messages []query.MessageSummary
    threadCursor, threadScroll int
    threadTruncated bool
    meeting meetingState
}

func (m *Model) cancelTimelineRequest() {
    if m.timelineState.cancel != nil {
        m.timelineState.cancel()
        m.timelineState.cancel = nil
    }
    m.timelineState.requestID++
    m.timelineState.loading = false
}

func (m Model) highlightedTimelineEntry() *timeline.Entry {
    if m.timelineState.cursor < 0 || m.timelineState.cursor >= len(m.timelineState.entries) {
        return nil
    }
    entry := m.timelineState.entries[m.timelineState.cursor].entry
    return &entry
}
