package tui

import (
	"encoding/json/jsontext"
	"errors"
	"strconv"
	"time"

	tea "charm.land/bubbletea/v2"
	"go.kenn.io/msgvault/internal/timeline"
)

func (m Model) handleTimelineKeyPress(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if m.modal != modalNone {
		return m.handleModalKeys(msg)
	}
	if m.timelineState.reader == timelineReaderMessage && m.detailSearchActive {
		return m.handleMessageDetailKeys(msg)
	}
	if m.timelineState.reader == timelineReaderMeeting && m.timelineState.meeting.detailSearchActive {
		return m.handleTimelineMeetingKey(msg)
	}
	// These keys navigate the timeline list. Readers keep their own keys.
	if m.timelineState.reader == timelineReaderNone {
		switch msg.String() {
		case "a":
			if entry := m.highlightedTimelineEntry(); entry != nil && entry.AnchorRef != "" {
				return m.requestTimeline(timeline.Request{Mode: timeline.ModeAnchor, Around: entry.AnchorRef}, true)
			}
			return m, nil
		case "w":
			if entry := m.highlightedTimelineEntry(); entry != nil {
				if ref := timelineFirstPerson(*entry); ref != "" {
					return m.requestTimeline(timeline.Request{Mode: timeline.ModeRelationship, With: ref}, true)
				}
			}
			return m, nil
		case "[", "]":
			return m.stepTimelineDay(msg.String() == "[")
		}
	}
	if msg.String() == keyNameCtrlC {
		m.cancelTimelineRequest()
	}
	if updated, cmd, handled := m.handleGlobalKeys(msg); handled {
		return updated, cmd
	}
	if msg.String() == keyNameEsc || msg.String() == keyNameBackspace {
		if m.timelineState.reader == timelineReaderMessage && m.detailSearchQuery != "" {
			return m.handleMessageDetailKeys(msg)
		}
		m.cancelTimelineRequest()
		m.loading = false
		m.timelineState.err = nil
		if m.timelineState.reader != timelineReaderNone {
			if m.timelineState.reader == timelineReaderMessage && m.timelineState.readerFromThread {
				m.timelineState.reader = timelineReaderThread
				m.timelineState.readerID = m.timelineState.threadID
				m.timelineState.readerFromThread = false
			} else {
				m.timelineState.reader = timelineReaderNone
			}
			return m, nil
		}
		m.popTimelineHistory()
		return m, nil
	}
	if msg.String() == "r" && m.timelineState.reader != timelineReaderNone && m.timelineState.err != nil {
		return m.openTimelineReader(m.timelineState.reader, m.timelineState.readerID)
	}
	switch m.timelineState.reader {
	case timelineReaderNone:
		// List navigation continues below.
	case timelineReaderCalendar, timelineReaderNote:
		return m.handleTimelineArchiveKey(msg)
	case timelineReaderMessage:
		// Horizontal navigation belongs to the parked Email/Text reader state.
		if msg.String() == "h" || msg.String() == keyNameLeft || msg.String() == "l" || msg.String() == keyNameRight || msg.String() == "T" {
			return m, nil
		}
		return m.handleMessageDetailKeys(msg)
	case timelineReaderMeeting:
		return m.handleTimelineMeetingKey(msg)
	case timelineReaderThread:
		if msg.String() == keyNameEnter {
			i := m.timelineState.threadCursor
			if i >= 0 && i < len(m.timelineState.messages) {
				m.messageDetail = nil
				m.timelineState.readerFromThread = true
				return m.openTimelineReader(timelineReaderMessage, m.timelineState.messages[i].ID)
			}
			return m, nil
		}
		// Reuse thread navigation against a value copy, leaving Email untouched.
		parked := m.viewState
		m.threadMessages = m.timelineState.messages
		m.threadCursor, m.threadScrollOffset = m.timelineState.threadCursor, m.timelineState.threadScroll
		updated, cmd := m.handleThreadViewKeys(msg)
		next, ok := updated.(Model)
		if !ok {
			return m, cmd
		}
		next.timelineState.threadCursor, next.timelineState.threadScroll = next.threadCursor, next.threadScrollOffset
		next.viewState = parked
		return next, cmd
	}
	switch msg.String() {
	case "r":
		req := m.timelineState.request
		req.Cursor = ""
		return m.requestTimeline(req, false)
	case "n":
		if result := m.timelineState.result; result != nil && result.Cursor != "" {
			req := m.timelineState.request
			req.Cursor = result.Cursor
			return m.requestTimeline(req, true)
		}
		return m, nil
	case keyNameEnter:
		if entry := m.highlightedTimelineEntry(); entry != nil {
			return m.openTimelineEntry(*entry)
		}
	case "up", "k", keyNameCtrlP:
		m.timelineState.cursor = max(m.timelineState.cursor-1, 0)
	case keyNameDown, "j", keyNameCtrlN:
		m.timelineState.cursor = min(m.timelineState.cursor+1, max(len(m.timelineState.entries)-1, 0))
	case keyNamePageUp, keyNameCtrlU:
		m.timelineState.scroll = max(m.timelineState.scroll-m.visibleRows(), 0)
		return m, nil
	case keyNamePageDown, keyNameCtrlD:
		m.timelineState.scroll = min(m.timelineState.scroll+m.visibleRows(), max(len(m.timelineState.lines)-m.visibleRows(), 0))
		return m, nil
	case keyNameHome:
		m.timelineState.cursor, m.timelineState.scroll = 0, 0
	case keyNameEnd, "G":
		m.timelineState.cursor = max(len(m.timelineState.entries)-1, 0)
	default:
		return m, nil
	}
	if len(m.timelineState.entries) > 0 {
		m.timelineState.scroll = calculateScrollOffset(m.timelineState.entries[m.timelineState.cursor].line, m.timelineState.scroll, m.visibleRows())
	}
	return m, nil
}

// stepTimelineDay requests the previous or next day. Day views step from the
// window edge in the direction of travel so multi-day windows are not revisited.
func (m Model) stepTimelineDay(back bool) (tea.Model, tea.Cmd) {
	result := m.timelineState.result
	if result == nil {
		return m, nil
	}
	loc, err := time.LoadLocation(result.Timezone)
	if err != nil {
		loc = time.UTC
	}
	delta, edge := 1, result.Window.To
	if back {
		delta, edge = -1, result.Window.From
	}
	day := result.Now.In(loc)
	switch result.Mode {
	case timeline.ModeAnchor:
		if result.Anchor != nil {
			day = result.Anchor.Time.In(loc)
		}
	case timeline.ModeRelationship:
		if entry := m.highlightedTimelineEntry(); entry != nil {
			day = entry.Time.In(loc)
		}
	default:
		day, err = time.ParseInLocation(time.DateOnly, edge, loc)
		if err != nil {
			return m, nil
		}
	}
	return m.requestTimeline(timeline.Request{Mode: timeline.ModeDay, Date: day.AddDate(0, 0, delta).Format(time.DateOnly)}, true)
}

func (m Model) handleTimelineMeetingKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if !m.timelineState.meeting.detailSearchActive && (msg.String() == "h" || msg.String() == keyNameLeft || msg.String() == "l" || msg.String() == keyNameRight) {
		return m, nil
	}
	parked := m.meetingState
	m.meetingState = m.timelineState.meeting
	var updated tea.Model
	var cmd tea.Cmd
	if m.meetingState.detailSearchActive {
		updated, cmd = m.handleMeetingDetailSearchInput(msg)
	} else {
		updated, cmd = m.handleMeetingDetailKeys(msg)
	}
	next, ok := updated.(Model)
	if !ok {
		return m, cmd
	}
	next.timelineState.meeting = next.meetingState
	next.meetingState = parked
	return next, cmd
}

func (m Model) openTimelineEntry(entry timeline.Entry) (tea.Model, tea.Cmd) {
	// A burst's exact anchor can fall beyond the bounded thread prefix.
	if entry.Kind == "burst" {
		for _, open := range entry.Open {
			if open.Tool == "get_message" {
				if id := timelineOpenID(open.Args["id"]); id > 0 {
					m.messageDetail = nil
					m.timelineState.readerFromThread = false
					return m.openTimelineReader(timelineReaderMessage, id)
				}
			}
		}
	}
	// Preserve transcript/message openers when a meeting has both targets.
	var calendarRef string
	for _, open := range entry.Open {
		switch open.Tool {
		case "get_daily_note":
			date, _ := open.Args["date"].(string)
			if id := timelineOpenID(open.Args["id"]); date != "" && id > 0 {
				m.timelineState.noteDate, m.timelineState.note = date, nil
				return m.openTimelineReader(timelineReaderNote, id)
			}
		case "get_calendar_event":
			if ref, ok := open.Args["ref"].(string); ok && ref != "" {
				calendarRef = ref
			}

		case "get_message":
			if id := timelineOpenID(open.Args["id"]); id > 0 {
				m.messageDetail = nil
				m.timelineState.readerFromThread = false
				return m.openTimelineReader(timelineReaderMessage, id)
			}
		case "list_messages":
			if id := timelineOpenID(open.Args["conversation_id"]); id > 0 {
				m.timelineState.messages = nil
				m.timelineState.threadID = id
				m.timelineState.readerFromThread = false
				return m.openTimelineReader(timelineReaderThread, id)
			}
		case "get_meeting_context":
			var first any
			switch ids := open.Args["message_ids"].(type) {
			case []any:
				if len(ids) > 0 {
					first = ids[0]
				}
			}
			if id := timelineOpenID(first); id > 0 {
				m.timelineState.meeting.level = meetingLevelDetail
				m.timelineState.meeting.detail = nil
				m.timelineState.meeting.detailScroll = 0
				m.timelineState.meeting.detailSearchActive = false
				m.timelineState.meeting.detailSearchInput.SetValue("")
				m.timelineState.meeting.detailSearchQuery = ""
				m.timelineState.meeting.detailSearchMatches = nil
				m.timelineState.meeting.detailSearchMatchIndex = 0
				return m.openTimelineReader(timelineReaderMeeting, id)
			}
		}
	}
	if calendarRef != "" {
		m.timelineState.calendarRef = calendarRef
		m.timelineState.calendar = nil
		m.timelineState.calendarScroll = 0
		return m.openTimelineReader(timelineReaderCalendar, 0)
	}
	return m, nil
}

func (m Model) handleTimelineArchiveKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	scroll := m.timelineState.calendarScroll
	lines := m.timelineCalendarLines()
	if m.timelineState.reader == timelineReaderNote {
		scroll, lines = m.timelineState.noteScroll, m.timelineNoteLines()
	}
	switch msg.String() {
	case "up", "k", keyNameCtrlP:
		scroll--
	case keyNameDown, "j", keyNameCtrlN:
		scroll++
	case keyNamePageUp, keyNameCtrlU:
		scroll -= m.detailPageSize()
	case keyNamePageDown, keyNameCtrlD:
		scroll += m.detailPageSize()
	case keyNameHome:
		scroll = 0
	case keyNameEnd, "G":
		scroll = len(lines)
	}
	scroll = min(max(scroll, 0), max(len(lines)-m.detailPageSize(), 0))
	if m.timelineState.reader == timelineReaderNote {
		m.timelineState.noteScroll = scroll
	} else {
		m.timelineState.calendarScroll = scroll
	}
	return m, nil
}

// The daemon client retains JSON numbers, including IDs above 2^53.
func timelineOpenID(value any) int64 {
	switch id := value.(type) {
	case jsontext.Value:
		n, err := strconv.ParseInt(string(id), 10, 64)
		if err == nil {
			return n
		}
	}
	return 0
}

func timelineFirstPerson(entry timeline.Entry) string {
	var people []timeline.Person
	switch {
	case entry.Meeting != nil:
		people = entry.Meeting.Attendees
		if entry.Kind == "transcript" {
			people = entry.Meeting.People
		}
	case entry.Burst != nil:
		people = entry.Burst.People
	case entry.Message != nil:
		people = entry.Message.People
	case entry.Call != nil:
		people = entry.Call.People
	case entry.Loop != nil:
		people = []timeline.Person{entry.Loop.Person}
	case entry.Note != nil:
		people = entry.Note.People
	}
	for _, p := range people {
		if p.PersonID > 0 {
			return "person:" + strconv.FormatInt(p.PersonID, 10)
		}
		if p.ParticipantID > 0 {
			return "participant:" + strconv.FormatInt(p.ParticipantID, 10)
		}
	}
	return ""
}

func (m Model) requestTimeline(req timeline.Request, remember bool) (tea.Model, tea.Cmd) {
	cmd := m.startTimelineRequest(req, remember)
	return m, cmd
}

func (m Model) openTimelineReader(reader timelineReader, id int64) (tea.Model, tea.Cmd) {
	cmd := m.loadTimelineReader(reader, id)
	return m, cmd
}

var errTimelineLoadCancelled = errors.New("timeline load cancelled · r to retry")

// popTimelineHistory restores the previous request. Without one, a cancelled
// load says so instead of leaving an empty list.
func (m *Model) popTimelineHistory() {
	n := len(m.timelineState.history)
	if n == 0 {
		if m.timelineState.result == nil {
			m.timelineState.err = errTimelineLoadCancelled
		}
		return
	}
	prev := m.timelineState.history[n-1]
	m.timelineState.history = m.timelineState.history[:n-1]
	m.timelineState.request, m.timelineState.result = prev.request, prev.result
	m.timelineState.cursor, m.timelineState.scroll = prev.cursor, prev.scroll
	m.timelineState.needsLoad = false
	m.rebuildTimelineRows()
}
