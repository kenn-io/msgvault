package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/textutil"
)

// renderTextView renders the current Texts mode view.
func (m Model) renderTextView() string {
	header := m.textHeaderView()
	var body string
	switch m.textState.level {
	case textLevelConversations, textLevelDrillConversations:
		body = m.textConversationsView()
	case textLevelAggregate:
		body = m.textAggregateView()
	case textLevelTimeline:
		body = m.textTimelineView()
	default:
		body = m.textConversationsView()
	}
	footer := m.textFooterView()
	// \x1b[J clears from cursor to end of screen, preventing
	// stale content when switching between views of different heights.
	return fmt.Sprintf("%s\n%s\n%s\x1b[J", header, body, footer)
}

// textHeaderView renders the Texts mode header (title bar + breadcrumb).
func (m Model) textHeaderView() string {
	line1 := m.textTitleBar()

	breadcrumb := m.textBreadcrumb()
	statsStr := m.textStatsString()

	breadcrumbStyled := statsStyle.Render(" " + breadcrumb + " ")
	statsStyled := statsStyle.Render(statsStr + " ")
	gap := max(m.width-
		lipgloss.Width(breadcrumbStyled)-
		lipgloss.Width(statsStyled), 0)
	line2 := breadcrumbStyled +
		strings.Repeat(" ", gap) + statsStyled

	return line1 + "\n" + line2
}

// textTitleBar builds the title bar for Texts mode.
func (m Model) textTitleBar() string {
	titleText := "msgvault"
	if m.version != "" && m.version != "dev" &&
		m.version != "unknown" {
		titleText = fmt.Sprintf("msgvault [%s]", m.version)
	}

	// Mode indicator
	accountStr := "Texts"
	if m.accountFilter != nil {
		for _, acc := range m.accounts {
			if acc.ID == *m.accountFilter {
				accountStr = "Texts - " + acc.Identifier
				break
			}
		}
	}

	content := fmt.Sprintf("%s - %s", titleText, accountStr)
	return titleBarStyle.Render(padRight(content, m.width-2))
}

// textBreadcrumb builds the breadcrumb for the current text view.
func (m Model) textBreadcrumb() string {
	switch m.textState.level {
	case textLevelConversations:
		return "Conversations"
	case textLevelAggregate:
		return m.textState.viewType.String()
	case textLevelDrillConversations:
		return fmt.Sprintf(
			"%s: %s",
			textViewTypePrefix(m.textState.viewType),
			textDrillKey(m),
		)
	case textLevelTimeline:
		order := "\u2191 oldest first"
		if m.textState.filter.SortDirection == query.SortDesc {
			order = "\u2193 newest first"
		}
		return "Timeline " + order
	}
	return ""
}

// textDrillKey returns the active drill filter key for breadcrumbs.
func textDrillKey(m Model) string {
	f := m.textState.filter
	if f.ContactPhone != "" {
		return truncateRunes(f.ContactPhone, 30)
	}
	if f.ContactName != "" {
		return truncateRunes(f.ContactName, 30)
	}
	if f.SourceType != "" {
		return f.SourceType
	}
	if f.Label != "" {
		return truncateRunes(f.Label, 30)
	}
	return "?"
}

// textViewTypePrefix returns a short prefix for text view breadcrumbs.
func textViewTypePrefix(vt query.TextViewType) string {
	switch vt {
	case query.TextViewContacts:
		return "Contact"
	case query.TextViewContactNames:
		return "Name"
	case query.TextViewSources:
		return "Source"
	case query.TextViewLabels:
		return "Label"
	case query.TextViewTime:
		return "Time"
	default:
		return "?"
	}
}

// textStatsString builds the stats summary for the Texts header.
func (m Model) textStatsString() string {
	if m.textState.stats != nil {
		return fmt.Sprintf(
			"%d msgs | %s",
			m.textState.stats.MessageCount,
			formatBytes(m.textState.stats.TotalSize),
		)
	}
	return ""
}

// measureMaxWidth returns the widest string length in values,
// using headerWidth as the minimum.
func measureMaxWidth(values []string, headerWidth int) int {
	w := headerWidth
	for _, v := range values {
		if len(v) > w {
			w = len(v)
		}
	}
	return w
}

// textConversationsView renders the conversations list table.
func (m Model) textConversationsView() string {
	if len(m.textState.conversations) == 0 && !m.loading {
		var sb strings.Builder
		// Still render header + separator for consistent height
		sb.WriteString(tableHeaderStyle.Render(
			padRight("   Conversations", m.width),
		))
		sb.WriteString("\n")
		sb.WriteString(separatorStyle.Render(
			strings.Repeat("\u2500", m.width),
		))
		sb.WriteString("\n")
		sb.WriteString(normalRowStyle.Render(
			padRight("   No conversations", m.width),
		))
		sb.WriteString("\n")
		// 1 "No data" + (pageSize-2) blanks = pageSize-1 data rows,
		// then +1 info line = pageSize body rows total.
		for i := 1; i < m.pageSize-1; i++ {
			sb.WriteString(normalRowStyle.Render(
				strings.Repeat(" ", m.width),
			))
			sb.WriteString("\n")
		}
		sb.WriteString(m.renderInfoLine("", m.loading))
		s := sb.String()
		if m.modal != modalNone {
			return m.overlayModal(s)
		}
		return s
	}

	var sb strings.Builder

	// Visible row range
	// Available data rows = pageSize - header(1) - separator(1) - info(1)
	availRows := max(m.pageSize-1, 1)
	endRow := min(m.textState.scrollOffset+availRows, len(m.textState.conversations))

	// Measure source column from visible data
	sourceVals := make(
		[]string, 0, endRow-m.textState.scrollOffset,
	)
	for i := m.textState.scrollOffset; i < endRow; i++ {
		sourceVals = append(
			sourceVals,
			m.textState.conversations[i].SourceType,
		)
	}
	sourceWidth := min(measureMaxWidth(sourceVals, len("Source")), 16)

	// Fixed column widths
	const (
		indicatorWidth = 3
		msgsWidth      = 10
		lastMsgWidth   = 16
		colSpacing     = 6 // gaps between columns
	)
	fixedTotal := indicatorWidth + sourceWidth +
		msgsWidth + lastMsgWidth + colSpacing
	nameWidth := max(m.width-fixedTotal, 15)

	// Header with sort indicators
	sortArrow := func(field query.TextSortField) string {
		if m.textState.filter.SortField == field {
			if m.textState.filter.SortDirection == query.SortDesc {
				return "\u2193"
			}
			return "\u2191"
		}
		return ""
	}
	convLabel := "Conversation" + sortArrow(query.TextSortByName)
	msgsLabel := "Messages" + sortArrow(query.TextSortByCount)
	lastLabel := "Last Message" + sortArrow(query.TextSortByLastMessage)

	headerRow := fmt.Sprintf(
		"   %-*s  %-*s %*s  %-*s",
		nameWidth, convLabel,
		sourceWidth, "Source",
		msgsWidth, msgsLabel,
		lastMsgWidth, lastLabel,
	)
	sb.WriteString(
		tableHeaderStyle.Render(padRight(headerRow, m.width)),
	)
	sb.WriteString("\n")
	sb.WriteString(
		separatorStyle.Render(strings.Repeat("\u2500", m.width)),
	)
	sb.WriteString("\n")

	// Data rows
	for i := m.textState.scrollOffset; i < endRow; i++ {
		conv := m.textState.conversations[i]
		isCursor := i == m.textState.cursor

		indicator := "   "
		if isCursor {
			indicator = cursorRowStyle.Render("\u25b6  ")
		}

		title := textutil.SanitizeTerminal(conv.Title)
		if title == "" {
			title = fmt.Sprintf("(conv %d)", conv.ConversationID)
		}
		title = truncateRunes(title, nameWidth)
		title = fmt.Sprintf("%-*s", nameWidth, title)

		source := truncateRunes(conv.SourceType, sourceWidth)
		source = fmt.Sprintf("%-*s", sourceWidth, source)
		msgs := formatCount(conv.MessageCount)
		lastMsg := conv.LastMessageAt.Format(
			"2006-01-02 15:04",
		)

		line := fmt.Sprintf(
			"%s  %s %*s  %-*s",
			title, source,
			msgsWidth, msgs,
			lastMsgWidth, lastMsg,
		)

		var style lipgloss.Style
		if isCursor {
			style = cursorRowStyle
		} else if i%2 == 0 {
			style = normalRowStyle
		} else {
			style = altRowStyle
		}

		sb.WriteString(indicator)
		sb.WriteString(
			style.Render(padRight(line, m.width-3)),
		)
		sb.WriteString("\n")
	}

	// Fill remaining space
	dataRows := endRow - m.textState.scrollOffset
	for i := dataRows; i < availRows; i++ {
		sb.WriteString(
			normalRowStyle.Render(strings.Repeat(" ", m.width)),
		)
		sb.WriteString("\n")
	}

	// Info line
	var infoContent string
	if m.inlineSearchActive {
		infoContent = "/" + m.searchInput.View()
	}
	sb.WriteString(m.renderInfoLine(infoContent, m.loading))

	if m.modal != modalNone {
		return m.overlayModal(sb.String())
	}
	return sb.String()
}

// textAggregateView renders the text aggregate table
// (contacts, sources, etc.).
func (m Model) textAggregateView() string {
	if len(m.textState.aggregateRows) == 0 && !m.loading {
		var sb strings.Builder
		sb.WriteString(tableHeaderStyle.Render(
			padRight("   "+m.textState.viewType.String(), m.width),
		))
		sb.WriteString("\n")
		sb.WriteString(separatorStyle.Render(
			strings.Repeat("\u2500", m.width),
		))
		sb.WriteString("\n")
		sb.WriteString(normalRowStyle.Render(
			padRight("   No data", m.width),
		))
		sb.WriteString("\n")
		// 1 "No data" + (pageSize-2) blanks = pageSize-1 data rows,
		// then +1 info line = pageSize body rows total.
		for i := 1; i < m.pageSize-1; i++ {
			sb.WriteString(normalRowStyle.Render(
				strings.Repeat(" ", m.width),
			))
			sb.WriteString("\n")
		}
		sb.WriteString(m.renderInfoLine("", m.loading))
		s := sb.String()
		if m.modal != modalNone {
			return m.overlayModal(s)
		}
		return s
	}

	var sb strings.Builder

	// Available data rows = pageSize - header(1) - separator(1) - info(1)
	aggAvailRows := max(m.pageSize-1, 1)
	endRow := min(m.textState.scrollOffset+aggAvailRows, len(m.textState.aggregateRows))

	// Fixed column widths
	const (
		indicatorWidth = 3
		countWidth     = 10
		sizeWidth      = 12
		attachWidth    = 12
		colSpacing     = 6 // gaps between columns
	)
	fixedTotal := indicatorWidth + countWidth +
		sizeWidth + attachWidth + colSpacing
	keyWidth := max(m.width-fixedTotal, 20)

	// Sort indicators
	sortInd := func(field query.TextSortField) string {
		if m.textState.filter.SortField == field {
			if m.textState.filter.SortDirection == query.SortDesc {
				return "\u2193"
			}
			return "\u2191"
		}
		return ""
	}

	viewLabel := m.textState.viewType.String()
	if si := sortInd(query.TextSortByName); si != "" {
		viewLabel += si
	}
	countLabel := "Count"
	if si := sortInd(query.TextSortByCount); si != "" {
		countLabel += si
	}
	sizeLabel := "Size"
	attachLabel := "Attchs"

	headerRow := fmt.Sprintf(
		"   %-*s %*s %*s %*s",
		keyWidth, viewLabel,
		countWidth, countLabel,
		sizeWidth, sizeLabel,
		attachWidth, attachLabel,
	)
	sb.WriteString(
		tableHeaderStyle.Render(padRight(headerRow, m.width)),
	)
	sb.WriteString("\n")
	sb.WriteString(
		separatorStyle.Render(strings.Repeat("\u2500", m.width)),
	)
	sb.WriteString("\n")

	for i := m.textState.scrollOffset; i < endRow; i++ {
		row := m.textState.aggregateRows[i]
		isCursor := i == m.textState.cursor

		indicator := "   "
		if isCursor {
			indicator = cursorRowStyle.Render("\u25b6  ")
		}

		key := truncateRunes(row.Key, keyWidth)
		key = fmt.Sprintf("%-*s", keyWidth, key)

		line := fmt.Sprintf(
			"%s %*s %*s %*s",
			key,
			countWidth, formatCount(row.Count),
			sizeWidth, formatBytes(row.TotalSize),
			attachWidth, formatBytes(row.AttachmentSize),
		)

		var style lipgloss.Style
		if isCursor {
			style = cursorRowStyle
		} else if i%2 == 0 {
			style = normalRowStyle
		} else {
			style = altRowStyle
		}

		sb.WriteString(indicator)
		sb.WriteString(
			style.Render(padRight(line, m.width-3)),
		)
		sb.WriteString("\n")
	}

	// Fill remaining space
	dataRows := endRow - m.textState.scrollOffset
	if len(m.textState.aggregateRows) == 0 && !m.loading {
		dataRows = 1
	}
	for i := dataRows; i < aggAvailRows; i++ {
		sb.WriteString(
			normalRowStyle.Render(strings.Repeat(" ", m.width)),
		)
		sb.WriteString("\n")
	}

	// Info line
	var infoContent string
	if m.inlineSearchActive {
		infoContent = "/" + m.searchInput.View()
	}
	sb.WriteString(m.renderInfoLine(infoContent, m.loading))

	if m.modal != modalNone {
		return m.overlayModal(sb.String())
	}
	return sb.String()
}

// textTimelineView renders a chat-style message timeline.
// Each message shows a sender/time header line followed by the full
// body text with word wrapping — like reading a chat app.
func (m Model) textTimelineView() string {
	if len(m.textState.messages) == 0 && !m.loading {
		var sb strings.Builder
		sb.WriteString(tableHeaderStyle.Render(
			padRight("   Messages", m.width),
		))
		sb.WriteString("\n")
		sb.WriteString(separatorStyle.Render(
			strings.Repeat("\u2500", m.width),
		))
		sb.WriteString("\n")
		sb.WriteString(normalRowStyle.Render(
			padRight("   No messages", m.width),
		))
		sb.WriteString("\n")
		// 1 "No messages" + (pageSize-2) blanks = pageSize-1 data rows,
		// then +1 info line = pageSize body rows total.
		for i := 1; i < m.pageSize-1; i++ {
			sb.WriteString(normalRowStyle.Render(
				strings.Repeat(" ", m.width),
			))
			sb.WriteString("\n")
		}
		sb.WriteString(m.renderInfoLine("", m.loading))
		s := sb.String()
		if m.modal != modalNone {
			return m.overlayModal(s)
		}
		return s
	}

	var sb strings.Builder

	// Header + separator (matches conversations/aggregate views so
	// the footer doesn't shift when drilling into a conversation)
	convTitle := ""
	for _, c := range m.textState.conversations {
		if c.ConversationID == m.textState.selectedConvID {
			convTitle = c.Title
			break
		}
	}
	if convTitle == "" {
		convTitle = "Messages"
	}
	sb.WriteString(
		tableHeaderStyle.Render(
			padRight("   "+convTitle, m.width),
		),
	)
	sb.WriteString("\n")
	sb.WriteString(
		separatorStyle.Render(strings.Repeat("\u2500", m.width)),
	)
	sb.WriteString("\n")

	// Build rendered lines for visible messages. Each message
	// produces multiple screen lines: a header + wrapped body.
	type chatLine struct {
		text    string
		msgIdx  int
		isFirst bool // first line of this message (shows cursor)
	}

	bodyWidth := max(
		// indent + margin
		m.width-6, 20)

	var allLines []chatLine
	for i := range len(m.textState.messages) {
		msg := m.textState.messages[i]

		// Sender line: "Name  12:34 PM" or "Name  2026-03-05 12:34"
		from := textutil.SanitizeTerminal(msg.FromName)
		if from == "" && msg.FromPhone != "" {
			from = textutil.SanitizeTerminal(msg.FromPhone)
		}
		if from == "" {
			from = textutil.SanitizeTerminal(msg.FromEmail)
		}
		if from == "" {
			from = "Unknown"
		}
		timeStr := msg.SentAt.Format("2006-01-02 15:04")
		// Right-justify timestamp: sender on left, time on right
		gap := max(bodyWidth-len(from)-len(timeStr), 2)
		headerLine := from + strings.Repeat(" ", gap) + timeStr

		allLines = append(allLines, chatLine{
			text: headerLine, msgIdx: i, isFirst: true,
		})

		// Body lines — use BodyText if available, fall back to Snippet
		body := textutil.SanitizeTerminal(msg.BodyText)
		if body == "" {
			body = textutil.SanitizeTerminal(msg.Snippet)
		}
		if body == "" {
			body = "(no text)"
		}
		for _, wline := range wrapText(body, bodyWidth) {
			allLines = append(allLines, chatLine{
				text: wline, msgIdx: i,
			})
		}

		// Blank line between messages
		allLines = append(allLines, chatLine{
			text: "", msgIdx: i,
		})
	}

	// Scroll offset is in screen lines, not message indices.
	// Map cursor (message index) to screen line offset.
	cursorLine := 0
	for _, cl := range allLines {
		if cl.msgIdx == m.textState.cursor && cl.isFirst {
			break
		}
		cursorLine++
	}

	// Ensure cursor is visible with some body context.
	// Available lines = pageSize - header(1) - separator(1) - info(1)
	visibleLines := max(m.pageSize-1, 1)
	scrollLine := min(cursorLine, m.textState.scrollOffset)
	// Show the message header plus a few body lines (not just
	// the header), so long messages don't appear cut off.
	cursorEndLine := cursorLine + 3
	if cursorEndLine >= scrollLine+visibleLines {
		scrollLine = cursorEndLine - visibleLines + 1
	}
	if scrollLine < 0 {
		scrollLine = 0
	}

	// Render visible lines
	linesWritten := 0
	for li := scrollLine; li < len(allLines) &&
		linesWritten < visibleLines; li++ {
		cl := allLines[li]
		isCursorMsg := cl.msgIdx == m.textState.cursor

		var style lipgloss.Style
		if isCursorMsg {
			style = cursorRowStyle
		} else if cl.msgIdx%2 == 0 {
			style = normalRowStyle
		} else {
			style = altRowStyle
		}

		indicator := "   "
		if cl.isFirst && isCursorMsg {
			indicator = cursorRowStyle.Render("\u25b6  ")
		}

		if cl.isFirst {
			// Header line: bold-ish via the style
			sb.WriteString(indicator)
			sb.WriteString(
				style.Bold(true).Render(
					padRight(cl.text, m.width-3),
				),
			)
		} else {
			// Body or blank line
			sb.WriteString("   ")
			sb.WriteString(
				style.Render(
					padRight("  "+cl.text, m.width-3),
				),
			)
		}
		sb.WriteString("\n")
		linesWritten++
	}

	// Fill remaining space
	for linesWritten < visibleLines {
		sb.WriteString(
			normalRowStyle.Render(
				strings.Repeat(" ", m.width),
			),
		)
		sb.WriteString("\n")
		linesWritten++
	}

	// Store scroll offset back (in screen lines)
	// Note: can't mutate m here since View is read-only;
	// the scrollOffset is maintained by the key handler.

	// Info line (with search bar when active)
	var infoContent string
	if m.inlineSearchActive {
		infoContent = "/" + m.searchInput.View()
	}
	sb.WriteString(m.renderInfoLine(infoContent, m.loading))

	if m.modal != modalNone {
		return m.overlayModal(sb.String())
	}
	return sb.String()
}

// textFooterView renders the footer with keybindings for Texts mode.
func (m Model) textFooterView() string {
	var keys []string
	var posStr string

	switch m.textState.level {
	case textLevelConversations, textLevelDrillConversations:
		keys = []string{
			"\u2191/k", "\u2193/j", "Enter",
			"Tab group", "s sort", "A acct",
			"m email", "? help",
		}
		if m.textState.level == textLevelDrillConversations {
			keys = append([]string{"\u2191/k", "\u2193/j", "Enter",
				"Esc back", "Tab group", "s sort",
				"m email", "? help"}, []string{}...)
		}
		n := len(m.textState.conversations)
		if n > 0 {
			posStr = fmt.Sprintf(
				" %d/%d ", m.textState.cursor+1, n,
			)
		}

	case textLevelAggregate:
		keys = []string{
			"\u2191/k", "\u2193/j", "Enter",
			"Esc back", "Tab group", "s sort",
			"m email", "? help",
		}
		n := len(m.textState.aggregateRows)
		if n > 0 {
			posStr = fmt.Sprintf(
				" %d/%d ", m.textState.cursor+1, n,
			)
		}

	case textLevelTimeline:
		keys = []string{
			"\u2191/\u2193 navigate", "r reverse",
			"/ search", "Esc back",
			"m email", "? help",
		}
		n := len(m.textState.messages)
		if n > 0 {
			posStr = fmt.Sprintf(
				" %d/%d ", m.textState.cursor+1, n,
			)
		}
	}

	keysStr := strings.Join(keys, " \u2502 ")
	gap := max(m.width-
		lipgloss.Width(keysStr)-
		lipgloss.Width(posStr)-2, 0)

	return footerStyle.Render(
		keysStr + strings.Repeat(" ", gap) + posStr,
	)
}
