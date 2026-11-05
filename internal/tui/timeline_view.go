package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/x/ansi"
	"go.kenn.io/msgvault/internal/textutil"
	"go.kenn.io/msgvault/internal/timeline"
)

// rebuildTimelineRows retains the shared Markdown verbatim. Entry metadata only
// supplies selection and navigation; the TUI does not compose activity rows.
func (m *Model) rebuildTimelineRows() {
	state := &m.timelineState
	text, entries := timeline.MarkdownWithEntries(state.result)
	state.lines = strings.Split(strings.TrimSuffix(text, "\n"), "\n")
	state.entries = nil
	for _, entry := range entries {
		state.entries = append(state.entries, timelineSelectableEntry{entry: entry.Entry, line: entry.Line})
	}
	state.cursor = min(max(state.cursor, 0), max(len(state.entries)-1, 0))
}

func (m Model) renderTimelineView() string {
	modal := m.modal
	m.modal = modalNone
	title := m.styles.titleBar.Render(padRight("msgvault [Timeline]", max(m.width-2, 0)))
	var body string
	switch m.timelineState.reader {
	case timelineReaderCalendar:
		body = m.timelineCalendarView()
	case timelineReaderMessage:
		body = m.messageDetailView()
	case timelineReaderMeeting:
		m.meetingState = m.timelineState.meeting
		body = m.meetingDetailView()
	case timelineReaderThread:
		m.threadMessages = m.timelineState.messages
		m.threadConversationID = m.timelineState.readerID
		m.threadCursor, m.threadScrollOffset = m.timelineState.threadCursor, m.timelineState.threadScroll
		m.threadTruncated = m.timelineState.threadTruncated
		body = m.threadView()
	default:
		lines := make([]string, 0, m.visibleRows())
		selectedLine := -1
		if len(m.timelineState.entries) > 0 {
			selectedLine = m.timelineState.entries[m.timelineState.cursor].line
		}
		start := min(m.timelineState.scroll, max(len(m.timelineState.lines)-1, 0))
		for i := start; i < len(m.timelineState.lines) && len(lines) < m.visibleRows(); i++ {
			line := ansi.Truncate(textutil.SanitizeTerminal(m.timelineState.lines[i]), max(m.width-2, 1), "…")
			if i == selectedLine {
				line = m.styles.cursorRow.Render(padRight(line, max(m.width-2, 1)))
			} else {
				line = m.styles.normalRow.Render(line)
			}
			lines = append(lines, line)
		}
		if m.timelineState.loading {
			lines = append(lines, m.styles.stats.Render("Loading…"))
		}
		if m.timelineState.err != nil {
			lines = append(lines, textutil.SanitizeTerminal(m.timelineState.err.Error()))
		}
		body = m.fillScreen(strings.Join(lines, "\n"))
	}
	if m.timelineState.reader != timelineReaderNone && m.timelineState.err != nil {
		body = textutil.SanitizeTerminal(m.timelineState.err.Error()) + "\n" + body
	}
	if modal != modalNone {
		m.modal = modal
		body = m.overlayModal(body)
	}
	footer := "j/k move · [/] day · a around · w person · Enter open · Esc back · m mode · ? help"
	if m.timelineState.reader != timelineReaderNone {
		footer = "j/k scroll · PgUp/PgDn · Home/End · Esc back · m mode · ? help"
		if m.timelineState.err != nil {
			footer = "r retry · " + footer
		}
	}
	if m.timelineState.reader == timelineReaderNone {
		if result := m.timelineState.result; result != nil && result.Cursor != "" {
			footer = "n next page · " + footer
		}
	}
	footerView := m.styles.stats.Render(footer)
	if m.timelineState.reader == timelineReaderMeeting && m.timelineState.meeting.detailSearchActive {
		footerView = m.meetingFooterView()
	}
	return fmt.Sprintf("%s\n%s\n%s", title, body, footerView)
}

var timelineHelpLines = []string{
	"Timeline", "", "j/k, arrows       Select an entry", "PgUp/PgDn         Scroll", "[ / ]             Previous / next archive day",
	"a                 Around the highlighted entry", "w                 Relationship with its first person",
	"Enter             Open details", "Esc               Back to the timeline / previous request",
	"n                 Next page (when available)", "r                 Retry / refresh", "m                 Switch content mode", "?                 Help", "q / ctrl+c        Quit",
}

func (m Model) timelineCalendarLines() []string {
	event := m.timelineState.calendar
	if event == nil {
		return nil
	}
	lines := []string{"Archived calendar occurrence"}
	field := func(name, value string) {
		if value != "" {
			lines = append(lines, name+": "+textutil.SanitizeTerminal(value))
		}
	}
	field("Ref", event.Ref)
	field("Title", event.Subject)
	if event.AllDay {
		field("All day", event.StartDate+" to "+event.EndDate+" (end date exclusive)")
	} else {
		field("Start", event.Start.Format(time.RFC3339Nano))
		field("End", event.End.Format(time.RFC3339Nano))
	}
	field("Status", event.Status)
	if event.DeletedFromSource {
		lines = append(lines, "Deleted from source; showing the archived occurrence")
	}
	field("Location", event.Location)
	field("Join URL", event.JoinURL)
	for _, person := range event.Attendees {
		field("Attendee", strings.TrimSpace(person.DisplayName+" <"+person.Email+"> "+person.Role+" "+person.RSVP))
	}
	lines = append(lines, "", "Description", "")
	description := event.Description
	if description == "" {
		description = "(No archived description)"
	}
	lines = append(lines, strings.Split(textutil.SanitizeTerminalMultiline(description), "\n")...)
	wrapped := make([]string, 0, len(lines))
	for _, line := range lines {
		wrapped = append(wrapped, wrapText(line, max(m.width-2, 1))...)
	}
	return wrapped
}

func (m Model) timelineCalendarView() string {
	lines := m.timelineCalendarLines()
	if lines == nil {
		message := "Loading archived calendar occurrence…"
		if !m.timelineState.loading {
			message = "Could not load archived calendar occurrence."
		}
		return m.fillScreenDetail(message)
	}
	size := m.detailPageSize()
	start := min(max(m.timelineState.calendarScroll, 0), max(len(lines)-size, 0))
	end := min(start+size, len(lines))
	return m.fillScreenWithPageSize(strings.Join(lines[start:end], "\n"), end-start, size)
}
