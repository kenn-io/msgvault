package tui

import (
    "encoding/json"
    "strconv"
    "time"

    tea "charm.land/bubbletea/v2"
    "go.kenn.io/msgvault/internal/timeline"
)

func (m Model) handleTimelineKeyPress(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
    if m.modal != modalNone { return m.handleModalKeys(msg) }
    if m.timelineState.reader == timelineReaderMessage && m.detailSearchActive {
        return m.handleMessageDetailKeys(msg)
    }
    if m.timelineState.reader == timelineReaderMeeting && m.timelineState.meeting.detailSearchActive {
        return m.handleTimelineMeetingKey(msg)
    }
    // These keys are local to Timeline, before the global dispatcher.
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
        if m.timelineState.result == nil { return m, nil }
        loc, err := time.LoadLocation(m.timelineState.result.Timezone)
        if err != nil { loc = time.UTC }
        date := m.timelineState.result.Window.From
        if entry := m.highlightedTimelineEntry(); entry != nil && !entry.Time.IsZero() {
            date = entry.Time.In(loc).Format(time.DateOnly)
        }
        day, err := time.ParseInLocation(time.DateOnly, date, loc)
        if err != nil { return m, nil }
        delta := 1
        if msg.String() == "[" { delta = -1 }
        return m.requestTimeline(timeline.Request{Mode: timeline.ModeDay, Date: day.AddDate(0, 0, delta).Format(time.DateOnly)}, true)
    }
    if msg.String() == keyNameCtrlC { m.cancelTimelineRequest() }
    if updated, cmd, handled := m.handleGlobalKeys(msg); handled { return updated, cmd }
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
        if n := len(m.timelineState.history); n > 0 {
            prev := m.timelineState.history[n-1]
            m.timelineState.history = m.timelineState.history[:n-1]
            m.timelineState.request, m.timelineState.result = prev.request, prev.result
            m.timelineState.cursor, m.timelineState.scroll = prev.cursor, prev.scroll
            m.timelineState.needsLoad = false
            m.rebuildTimelineRows()
        }
        return m, nil
    }
    if msg.String() == "r" && m.timelineState.reader != timelineReaderNone && m.timelineState.err != nil {
        return m.openTimelineReader(m.timelineState.reader, m.timelineState.readerID)
    }
    switch m.timelineState.reader {
    case timelineReaderMessage:
        // Horizontal navigation belongs to the parked Email/Text reader state.
        if msg.String() == "h" || msg.String() == "left" || msg.String() == "l" || msg.String() == keyNameRight || msg.String() == "T" { return m, nil }
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
        next := updated.(Model)
        next.timelineState.threadCursor, next.timelineState.threadScroll = next.threadCursor, next.threadScrollOffset
        next.viewState = parked
        return next, cmd
    }
    switch msg.String() {
    case "r":
        return m.requestTimeline(m.timelineState.request, false)
    case keyNameEnter:
        if entry := m.highlightedTimelineEntry(); entry != nil { return m.openTimelineEntry(*entry) }
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

func (m Model) handleTimelineMeetingKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
    if msg.String() == "h" || msg.String() == "left" || msg.String() == "l" || msg.String() == keyNameRight { return m, nil }
    parked := m.meetingState
    m.meetingState = m.timelineState.meeting
    var updated tea.Model
    var cmd tea.Cmd
    if m.meetingState.detailSearchActive { updated, cmd = m.handleMeetingDetailSearchInput(msg) } else { updated, cmd = m.handleMeetingDetailKeys(msg) }
    next := updated.(Model)
    next.timelineState.meeting = next.meetingState
    next.meetingState = parked
    return next, cmd
}

func (m Model) openTimelineEntry(entry timeline.Entry) (tea.Model, tea.Cmd) {
    if entry.HiddenText { return m, nil }
    for _, open := range entry.Open {
        switch open.Tool {
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
            case []int64:
                if len(ids) > 0 { first = ids[0] }
            case []any:
                if len(ids) > 0 { first = ids[0] }
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
    return m, nil
}

// The daemon client retains JSON numbers, including IDs above 2^53.
func timelineOpenID(value any) int64 {
    switch id := value.(type) {
    case int64:
        return id
    case int:
        return int64(id)
    case json.Number:
        n, err := id.Int64()
        if err == nil { return n }
    }
    return 0
}

func timelineFirstPerson(entry timeline.Entry) string {
    var people []timeline.Person
    switch {
    case entry.Meeting != nil:
        people = entry.Meeting.Attendees
        if entry.Kind == "transcript" { people = entry.Meeting.People }
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
    if len(people) == 0 { return "" }
    p := people[0]
    if p.PersonID > 0 { return "person:"+strconv.FormatInt(p.PersonID, 10) }
    if p.ParticipantID > 0 { return "participant:"+strconv.FormatInt(p.ParticipantID, 10) }
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
