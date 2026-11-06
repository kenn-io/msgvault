package tui

import (
	"context"

	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/timeline"
)

// TimelineBackend reads the selected daemon's timeline and archived event details.
type TimelineBackend interface {
	GetDailyNoteContext(ctx context.Context, date string, id int64) (*store.DailyNoteEntry, error)
	Timeline(ctx context.Context, req timeline.Request) (*timeline.Timeline, string, error)
	GetCalendarOccurrenceContext(ctx context.Context, ref string) (store.CalendarOccurrenceDetail, error)
}

type timelineReader uint8

const (
	timelineReaderNone timelineReader = iota
	timelineReaderMessage
	timelineReaderMeeting
	timelineReaderCalendar
	timelineReaderNote
)

type timelineNavigation struct {
	request        timeline.Request
	result         *timeline.Timeline
	cursor, scroll int
}

type timelineState struct {
	request        timeline.Request
	result         *timeline.Timeline
	lines          []string
	entries        []timeline.RenderedEntry
	cursor, scroll int
	history        []timelineNavigation
	requestID      uint64
	cancel         context.CancelFunc
	loading        bool
	needsLoad      bool
	err            error
	reader         timelineReader
	readerID       int64
	meeting        meetingState
	calendarRef    string
	calendar       *store.CalendarOccurrenceDetail
	calendarScroll int
	noteDate       string
	note           *store.DailyNoteEntry
	noteScroll     int
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
	entry := m.timelineState.entries[m.timelineState.cursor].Entry
	return &entry
}
