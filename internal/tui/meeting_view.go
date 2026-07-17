package tui

import (
	"fmt"
	"strings"

	"charm.land/lipgloss/v2"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/textutil"
)

func (m Model) renderMeetingView() string {
	body := m.meetingListView()
	if m.meetingState.level == meetingLevelDetail {
		body = m.meetingDetailView()
	}
	return fmt.Sprintf("%s\n%s\n%s",
		m.meetingHeaderView(),
		body,
		m.meetingFooterView(),
	)
}

func (m Model) meetingHeaderView() string {
	titleText := "msgvault"
	if m.version != "" && m.version != "dev" && m.version != "unknown" {
		titleText = fmt.Sprintf("msgvault [%s]", m.version)
	}
	scope := "All Sources"
	if m.meetingState.sourceID != nil {
		scope = m.meetingSourceLabel(*m.meetingState.sourceID)
	}
	line1 := m.styles.titleBar.Render(padRight(
		fmt.Sprintf("%s [Meetings] - %s", titleText, scope),
		max(m.width-2, 0),
	))
	breadcrumb := "Meetings"
	if m.meetingState.level == meetingLevelDetail {
		breadcrumb += " / "
		if m.meetingState.detail != nil {
			breadcrumb += truncateRunes(textutil.SanitizeTerminal(m.meetingState.detail.Subject), 50)
		} else {
			breadcrumb += "Loading…"
		}
	}
	line2 := m.styles.stats.Render(fmt.Sprintf(" %s | %d meetings ", breadcrumb, len(m.meetingState.messages)))
	return line1 + "\n" + padRight(line2, m.width)
}

func (m Model) meetingSourceLabel(sourceID int64) string {
	if sourceID == 0 {
		return "—"
	}
	for _, account := range m.accounts {
		if account.ID != sourceID {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(account.SourceType)) {
		case meetingSourceGranola:
			return "Granola"
		case meetingSourceCircleback:
			return "Circleback"
		}
		if account.DisplayName != "" {
			return textutil.SanitizeTerminal(account.DisplayName)
		}
		if account.Identifier != "" {
			return textutil.SanitizeTerminal(account.Identifier)
		}
		return "—"
	}
	return "—"
}

func meetingColumnWidths(width int) (date, title, organizer, source int) {
	available := max(width-9, 31)
	date = min(16, max(8, available/6))
	source = min(12, max(6, available/8))
	organizer = min(24, max(8, available/5))
	title = max(available-date-organizer-source, 9)
	return date, title, organizer, source
}

func (m Model) meetingListView() string {
	if len(m.meetingState.messages) == 0 && !m.loading && m.err == nil &&
		!m.meetingState.searchActive && m.meetingState.searchQuery == "" {
		message := "No meetings found. Sync Granola or Circleback to import meetings."
		if len(m.meetingAccounts()) == 0 {
			message = "No meeting sources configured. Add Granola or Circleback to begin."
		}
		content := m.fillScreen(m.styles.normalRow.Render(padRight(message, m.width)))
		if m.modal != modalNone {
			return m.overlayModal(content)
		}
		return content
	}

	dateWidth, titleWidth, organizerWidth, sourceWidth := meetingColumnWidths(m.width)
	rowFormat := "   %-*s  %-*s  %-*s  %-*s"
	var sb strings.Builder
	header := fmt.Sprintf(rowFormat,
		dateWidth, "Date",
		titleWidth, "Title",
		organizerWidth, "Organizer",
		sourceWidth, "Source",
	)
	sb.WriteString(m.styles.tableHeader.Render(padRight(header, m.width)))
	sb.WriteString("\n")
	sb.WriteString(m.styles.separator.Render(strings.Repeat("─", max(m.width, 0))))
	sb.WriteString("\n")

	end := min(m.meetingState.scrollOffset+m.pageSize-1, len(m.meetingState.messages))
	if len(m.meetingState.messages) == 0 && !m.loading {
		sb.WriteString(m.styles.normalRow.Render(padRight("   No results found", m.width)))
		sb.WriteString("\n")
	}
	for i := m.meetingState.scrollOffset; i < end; i++ {
		meeting := m.meetingState.messages[i]
		indicator := "   "
		style := m.styles.normalRow
		if i == m.meetingState.cursor {
			indicator = "▶  "
			style = m.styles.cursorRow
		}
		organizer := meeting.FromName
		if organizer == "" {
			organizer = meeting.FromEmail
		}
		row := indicator + fmt.Sprintf("%-*s  %-*s  %-*s  %-*s",
			dateWidth, truncateRunes(meeting.SentAt.Format("2006-01-02 15:04"), dateWidth),
			titleWidth, truncateRunes(textutil.SanitizeTerminal(meeting.Subject), titleWidth),
			organizerWidth, truncateRunes(textutil.SanitizeTerminal(organizer), organizerWidth),
			sourceWidth, truncateRunes(m.meetingSourceLabel(meeting.SourceID), sourceWidth),
		)
		sb.WriteString(style.Render(padRight(row, m.width)))
		sb.WriteString("\n")
	}

	usedRows := end - m.meetingState.scrollOffset
	if len(m.meetingState.messages) == 0 && !m.loading {
		usedRows = 1
	}
	for i := usedRows; i < m.pageSize-1; i++ {
		sb.WriteString(m.styles.normalRow.Render(strings.Repeat(" ", max(m.width, 0))))
		sb.WriteString("\n")
	}
	info := ""
	if m.meetingState.searchActive {
		info = "[Transcript]/" + m.meetingState.searchInput.View()
	} else if m.meetingState.searchQuery != "" {
		info = fmt.Sprintf(" Search: %q [Transcript]", m.meetingState.searchQuery)
	} else if m.loading {
		info = m.spinnerIndicator() + " Loading meetings..."
	}
	sb.WriteString(m.renderInfoLine(info, m.loading))

	content := sb.String()
	if m.modal != modalNone {
		return m.overlayModal(content)
	}
	return content
}

func (m Model) meetingFooterView() string {
	keys := []string{"↑/k", "↓/j", "Enter", "A source", "/ search", "m mode", "? help"}
	if m.width < 60 {
		keys = []string{"↑/↓", "Enter", "/", "m", "?"}
	}
	if m.meetingState.level == meetingLevelDetail {
		keys = []string{"↑/k", "↓/j", "←/h prev", "→/l next", "/ find", "Esc back", "m mode", "? help"}
		if m.width < 60 {
			keys = []string{"↑/↓", "←/→", "/", "Esc", "?"}
		}
		if m.meetingState.detailSearchActive {
			keys = []string{"[Find]/" + m.meetingState.detailSearchInput.View(), "Enter find", "Esc cancel"}
		}
	}
	footer := " " + strings.Join(keys, " │ ")
	contentWidth := max(m.width-2, 1)
	if len(m.meetingState.messages) > 0 {
		position := fmt.Sprintf(" %d/%d ", m.meetingState.cursor+1, len(m.meetingState.messages))
		gap := contentWidth - lipgloss.Width(footer) - lipgloss.Width(position)
		if gap >= 1 {
			footer += strings.Repeat(" ", gap) + position
		}
	}
	footer = truncateToWidth(footer, contentWidth)
	return m.styles.footer.Render(padRight(footer, contentWidth))
}

func (m Model) meetingDetailLines() []string {
	detail := m.meetingState.detail
	if detail == nil {
		return nil
	}
	lines := []string{
		"Title: " + textutil.SanitizeTerminal(detail.Subject),
		"When: " + detail.SentAt.Format("Mon, 02 Jan 2006 15:04:05 MST"),
	}
	if len(detail.From) > 0 {
		lines = append(lines, "Organizer: "+textutil.SanitizeTerminal(formatAddresses(detail.From)))
	}
	attendees := append([]query.Address(nil), detail.To...)
	attendees = append(attendees, detail.Cc...)
	if len(attendees) > 0 {
		lines = append(lines, "Attendees: "+textutil.SanitizeTerminal(formatAddresses(attendees)))
	}
	lines = append(lines,
		"Source: "+m.meetingSourceLabel(detail.SourceID),
		"",
		strings.Repeat("─", max(min(m.width-2, 80), 1)),
		"Transcript / Notes",
		"",
	)
	body := detail.BodyText
	if body == "" {
		body = "(No transcript or notes available)"
	}
	bodyWidth := max(m.width-2, 1)
	lines = append(lines, m.markdownCache.meetingLinesFor(detail.ID, body, bodyWidth)...)
	return lines
}

func (m Model) meetingDetailView() string {
	if m.meetingState.detail == nil {
		if m.loading {
			return m.fillScreenDetail(m.styles.loading.Render(padRight(m.spinnerIndicator()+" Loading meeting...", m.width)))
		}
		return m.fillScreenDetail(m.styles.err.Render(padRight("Meeting not found", m.width)))
	}
	lines := m.meetingDetailLines()
	pageSize := m.detailPageSize()
	maxScroll := max(len(lines)-pageSize, 0)
	start := min(m.meetingState.detailScroll, maxScroll)
	end := min(start+pageSize, len(lines))
	var sb strings.Builder
	for _, line := range lines[start:end] {
		if m.meetingState.detailSearchQuery != "" {
			line = highlightTerms(line, m.meetingState.detailSearchQuery)
		}
		sb.WriteString(m.styles.normalRow.Render(padRight(line, m.width)))
		sb.WriteString("\n")
	}
	for i := end - start; i < pageSize; i++ {
		sb.WriteString(m.styles.normalRow.Render(strings.Repeat(" ", max(m.width, 0))))
		sb.WriteString("\n")
	}
	content := strings.TrimSuffix(sb.String(), "\n")
	if m.modal != modalNone {
		return m.overlayModal(content)
	}
	return content
}
