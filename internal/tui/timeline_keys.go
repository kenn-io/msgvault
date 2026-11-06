package tui

import (
	"encoding/json/jsontext"
	"errors"
	"strconv"
	"strings"
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
				return m.requestTimeline(timeline.Request{Mode: timeline.ModeAnchor, Around: entry.AnchorRef})
			}
			return m, nil
		case "w":
			if entry := m.highlightedTimelineEntry(); entry != nil {
				if ref := timelineFirstPerson(*entry); ref != "" {
					return m.requestTimeline(timeline.Request{Mode: timeline.ModeRelationship, With: ref})
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
		if m.timelineState.reader != timelineReaderNone {
			m.timelineState.err = nil
			m.timelineState.reader = timelineReaderNone
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
	}
	switch msg.String() {
	case "r":
		req := m.timelineState.request
		req.Cursor = ""
		return m.requestTimeline(req)
	case "n":
		if result := m.timelineState.result; result != nil && result.Cursor != "" {
			req := m.timelineState.request
			req.Cursor = result.Cursor
			return m.requestTimeline(req)
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
	return m.requestTimeline(timeline.Request{Mode: timeline.ModeDay, Date: day.AddDate(0, 0, delta).Format(time.DateOnly)})
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
	if entry.Kind == "note" {
		if key, ok := strings.CutPrefix(entry.Key, "note:"); ok {
			if id, err := strconv.ParseInt(key, 10, 64); err == nil && id > 0 {
				m.timelineState.noteDate, m.timelineState.note = entry.Ref, nil
				return m.openTimelineReader(timelineReaderNote, id)
			}
		}
	}
	// Preserve transcript/message openers when a meeting has both targets.
	var calendarRef string
	for _, open := range entry.Open {
		switch open.Tool {
		case "get_calendar_event":
			if ref, ok := open.Args["ref"].(string); ok && ref != "" {
				calendarRef = ref
			}

		case "get_message":
			if id := timelineOpenID(open.Args["id"]); id > 0 {
				m.messageDetail = nil
				return m.openTimelineReader(timelineReaderMessage, id)
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
	people := timeline.EntryPeople(entry)
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

func (m Model) requestTimeline(req timeline.Request) (tea.Model, tea.Cmd) {
	cmd := m.startTimelineRequest(req, true)
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
		if m.timelineState.result == nil && m.timelineState.err == nil {
			m.timelineState.err = errTimelineLoadCancelled
		}
		return
	}
	prev := m.timelineState.history[n-1]
	m.timelineState.history = m.timelineState.history[:n-1]
	m.timelineState.request, m.timelineState.result = prev.request, prev.result
	m.timelineState.cursor, m.timelineState.scroll = prev.cursor, prev.scroll
	m.timelineState.needsLoad = false
	m.timelineState.err = nil
	m.rebuildTimelineRows()
}
