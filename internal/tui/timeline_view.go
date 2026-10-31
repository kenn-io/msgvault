package tui

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/charmbracelet/x/ansi"
	"go.kenn.io/msgvault/internal/textutil"
	"go.kenn.io/msgvault/internal/timeline"
)

// rebuildTimelineRows retains the shared Markdown verbatim. Entry metadata only
// supplies selection and navigation; the TUI does not compose activity rows.
func (m *Model) rebuildTimelineRows() {
	state := &m.timelineState
	state.lines = strings.Split(strings.TrimSuffix(timeline.Markdown(state.result), "\n"), "\n")
	state.entries = nil
	if state.result == nil {
		return
	}
	// Render a second value copy with unique entry references. Only this
	// metadata rendering carries markers; the displayed lines above remain
	// the original shared Markdown. This preserves grouped rows and repeated
	// references without trying to parse sender-controlled text as identity.
	prefix := "TIMELINE_SELECTION_"
	original := strings.Join(state.lines, "\n")
	for strings.Contains(original, prefix) {
		prefix += "_"
	}
	tagged := *state.result
	var entries []timeline.Entry
	var markers []string
	tagEntries := func(values []timeline.Entry) []timeline.Entry {
		copied := slices.Clone(values)
		for i := range copied {
			entries = append(entries, values[i])
			marker := prefix + strconv.Itoa(len(markers)) + "END"
			markers = append(markers, marker)
			copied[i].Ref = marker
		}
		return copied
	}
	if tagged.OpenLoops != nil {
		loops := *tagged.OpenLoops
		loops.Items = tagEntries(loops.Items)
		tagged.OpenLoops = &loops
	}
	tagged.Days = slices.Clone(tagged.Days)
	for i := range tagged.Days {
		day := &tagged.Days[i]
		if day.Summary != nil {
			continue
		}
		day.Meetings = tagEntries(day.Meetings)
		day.Bursts = tagEntries(day.Bursts)
		day.Calls = tagEntries(day.Calls)
		day.Notes = tagEntries(day.Notes)
	}
	tagged.History = tagEntries(tagged.History)
	tagged.Upcoming = tagEntries(tagged.Upcoming)
	taggedLines := strings.Split(strings.TrimSuffix(timeline.Markdown(&tagged), "\n"), "\n")
	if len(taggedLines) != len(state.lines) {
		return
	}
	for i, marker := range markers {
		for line, text := range taggedLines {
			if strings.Contains(text, marker) {
				state.entries = append(state.entries, timelineSelectableEntry{entry: entries[i], line: line})
				break
			}
		}
	}
	slices.SortStableFunc(state.entries, func(a, b timelineSelectableEntry) int { return a.line - b.line })
	state.cursor = min(max(state.cursor, 0), max(len(state.entries)-1, 0))
}

func (m Model) renderTimelineView() string {
	modal := m.modal
	m.modal = modalNone
	title := m.styles.titleBar.Render(padRight("msgvault [Timeline]", max(m.width-2, 0)))
	body := ""
	switch m.timelineState.reader {
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
	if m.timelineState.reader == timelineReaderNone {
		if result := m.timelineState.result; result != nil && result.Cursor != "" {
			footer = "n next page · " + footer
		}
		if entry := m.highlightedTimelineEntry(); entry != nil {
			footer = "Selected: " + textutil.SanitizeTerminal(entry.Ref) + " · " + footer
		}
	}
	return fmt.Sprintf("%s\n%s\n%s", title, body, m.styles.stats.Render(footer))
}

var timelineHelpLines = []string{
	"Timeline", "", "j/k, arrows       Select an entry", "PgUp/PgDn         Scroll", "[ / ]             Previous / next archive day",
	"a                 Around the highlighted entry", "w                 Relationship with its first person",
	"Enter             Open the existing reader or thread", "Esc               Back to the timeline / previous request",
	"n                 Next page (when available)", "r                 Retry / refresh", "m                 Switch content mode", "?                 Help", "q / ctrl+c        Quit",
}
