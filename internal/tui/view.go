package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/wesm/msgvault/internal/query"
	"github.com/wesm/msgvault/internal/textutil"
)

// Monochrome theme - inherits terminal background where possible.
// Only cursor, title bar, and alt rows use explicit backgrounds.
var (
	// Explicit backgrounds only for elements that need contrast
	bgAlt    = lipgloss.AdaptiveColor{Light: "#f0f0f0", Dark: "#181818"}
	bgCursor = lipgloss.AdaptiveColor{Light: "#e0e0e0", Dark: "#282828"}

	// Title bar style - bold with visible background
	titleBarStyle = lipgloss.NewStyle().
			Bold(true).
			Background(lipgloss.AdaptiveColor{Light: "#e0e0e0", Dark: "#333333"}).
			Foreground(lipgloss.AdaptiveColor{Light: "#000000", Dark: "#ffffff"}).
			Padding(0, 1)

	statsStyle = lipgloss.NewStyle().
			Foreground(lipgloss.AdaptiveColor{Light: "#555555", Dark: "#999999"}).
			Padding(0, 1)

	// Spinner style - NOT faint so it's visible
	spinnerStyle = lipgloss.NewStyle().
			Bold(true)

	tableHeaderStyle = lipgloss.NewStyle().
				Bold(true)

	// Separator line style for under headers
	separatorStyle = lipgloss.NewStyle().
			Faint(true)

	// Cursor row: subtle background for visibility
	cursorRowStyle = lipgloss.NewStyle().
			Background(bgCursor)

	// Selected (checked) rows: bold, inherits terminal background
	selectedRowStyle = lipgloss.NewStyle().
				Bold(true)

	// Normal rows inherit terminal background
	normalRowStyle = lipgloss.NewStyle()

	// Alternating rows: very subtle shift from terminal default
	altRowStyle = lipgloss.NewStyle().
			Background(bgAlt)

	footerStyle = lipgloss.NewStyle().
			Foreground(lipgloss.AdaptiveColor{Light: "#555555", Dark: "#999999"}).
			Padding(0, 1)

	errorStyle = lipgloss.NewStyle().
			Bold(true)

	loadingStyle = lipgloss.NewStyle().
			Italic(true)

	selectedIndicatorStyle = lipgloss.NewStyle().
				Bold(true)

	modalStyle = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			Padding(1, 2).
			Background(lipgloss.AdaptiveColor{Light: "#ffffff", Dark: "#000000"})

	modalTitleStyle = lipgloss.NewStyle().
			Bold(true)

	flashStyle = lipgloss.NewStyle().
			Italic(true).
			Foreground(lipgloss.AdaptiveColor{Light: "#996600", Dark: "#ffcc00"}) // Amber for visibility

	highlightStyle = lipgloss.NewStyle().
			Foreground(lipgloss.AdaptiveColor{Light: "#000000", Dark: "#000000"}).
			Background(lipgloss.AdaptiveColor{Light: "#e8d44d", Dark: "#e8d44d"}).
			Bold(true)
)

// viewTypeAbbrev returns view type name for column headers and top-level breadcrumb.
func viewTypeAbbrev(vt query.ViewType) string {
	switch vt {
	case query.ViewSenders:
		return "Sender"
	case query.ViewSenderNames:
		return "Sender Name"
	case query.ViewRecipients:
		return "Recipient"
	case query.ViewRecipientNames:
		return "Recipient Name"
	case query.ViewDomains:
		return "Domain"
	case query.ViewLabels:
		return "Label"
	case query.ViewTime:
		return "Time"
	default:
		return vt.String()
	}
}

// viewTypePrefix returns a short prefix for drill-down breadcrumbs (e.g., "S:" for Sender).
func viewTypePrefix(vt query.ViewType) string {
	switch vt {
	case query.ViewSenders:
		return "S"
	case query.ViewSenderNames:
		return "N"
	case query.ViewRecipients:
		return "R"
	case query.ViewRecipientNames:
		return "RN"
	case query.ViewDomains:
		return "D"
	case query.ViewLabels:
		return "L"
	case query.ViewTime:
		return "T"
	default:
		s := vt.String()
		if len(s) > 0 {
			return s[:1]
		}
		return "?"
	}
}

// buildTitleBar builds the title bar line (line 1 of the header).
// Format: "msgvault [version] - Account          update: vX.Y.Z"
func (m Model) buildTitleBar() string {
	// Build title with version
	titleText := "msgvault"
	if m.version != "" && m.version != "dev" && m.version != "unknown" {
		titleText = fmt.Sprintf("msgvault [%s]", m.version)
	}

	// Account indicator
	var accountStr string
	if m.accountFilter == nil {
		accountStr = "All Accounts"
	} else {
		for _, acc := range m.accounts {
			if acc.ID == *m.accountFilter {
				accountStr = acc.Identifier
				break
			}
		}
	}

	// Filter indicators
	if m.filters.attachmentsOnly {
		accountStr += " [Attachments]"
	}
	if m.filters.hideDeletedFromSource {
		accountStr += " [Hide Deleted]"
	}

	// Update notification (right-aligned on title bar)
	var updateNotice string
	if m.updateAvailable != "" {
		if m.updateIsDevBuild {
			updateNotice = fmt.Sprintf("latest: %s — msgvault update --force", m.updateAvailable)
		} else {
			updateNotice = fmt.Sprintf("update: %s — msgvault update", m.updateAvailable)
		}
	}

	// Mode indicator when text engine is available
	modeStr := ""
	if m.textEngine != nil {
		modeStr = " [Email]"
	}

	// Build line content: "msgvault [hash] [Email] - Account          update: vX.Y.Z"
	line1Content := fmt.Sprintf("%s%s - %s", titleText, modeStr, accountStr)
	if updateNotice != "" {
		gap := m.width - 2 - lipgloss.Width(line1Content) - lipgloss.Width(updateNotice)
		if gap > 1 {
			line1Content += strings.Repeat(" ", gap) + updateNotice
		}
	}
	return titleBarStyle.Render(padRight(line1Content, m.width-2)) // -2 for padding
}

// buildBreadcrumb builds the breadcrumb text based on the current navigation level.
func (m Model) buildBreadcrumb() string {
	switch m.level {
	case levelAggregates:
		breadcrumb := viewTypeAbbrev(m.viewType)
		if m.viewType == query.ViewTime {
			breadcrumb += " (" + m.timeGranularity.String() + ")"
		}
		return breadcrumb
	case levelDrillDown:
		// Show drill context: "S: foo@example.com (by To)"
		drillKey := m.drillFilterKey()
		breadcrumb := fmt.Sprintf("%s: %s (by %s)", viewTypePrefix(m.drillViewType), truncateRunes(drillKey, 30), viewTypeAbbrev(m.viewType))
		if m.viewType == query.ViewTime {
			breadcrumb += " " + m.timeGranularity.String()
		}
		return breadcrumb
	case levelMessageList:
		if m.searchQuery != "" {
			return "Search Results"
		}
		if m.allMessages {
			return "All Messages"
		}
		if m.hasDrillFilter() {
			drillKey := m.drillFilterKey()
			if m.filterKey != "" && m.filterKey != drillKey {
				return fmt.Sprintf("%s: %s > %s: %s", viewTypePrefix(m.drillViewType), truncateRunes(drillKey, 20), viewTypePrefix(m.viewType), truncateRunes(m.filterKey, 20))
			}
			return fmt.Sprintf("%s: %s", viewTypePrefix(m.drillViewType), truncateRunes(drillKey, 40))
		}
		return fmt.Sprintf("%s: %s", viewTypePrefix(m.viewType), truncateRunes(m.filterKey, 40))
	case levelMessageDetail:
		subject := m.pendingDetailSubject
		if m.messageDetail != nil {
			subject = m.messageDetail.Subject
		}
		return fmt.Sprintf("Message: %s", truncateRunes(subject, 50))
	case levelThreadView:
		if m.threadTruncated {
			return fmt.Sprintf("Thread (showing %d of %d+ messages)", len(m.threadMessages), len(m.threadMessages))
		}
		return fmt.Sprintf("Thread (%d messages)", len(m.threadMessages))
	default:
		return ""
	}
}

// buildStatsString builds the stats summary string for the header.
func (m Model) buildStatsString() string {
	if m.contextStats != nil && (m.level == levelMessageList || m.level == levelDrillDown || m.searchQuery != "") {
		// Show "+" suffix when search has more results than loaded
		msgsSuffix := ""
		if m.searchTotalCount == -1 {
			msgsSuffix = "+"
		}
		return fmt.Sprintf("%d%s msgs | %s | %d attchs",
			m.contextStats.MessageCount,
			msgsSuffix,
			formatBytes(m.contextStats.TotalSize),
			m.contextStats.AttachmentCount,
		)
	}
	if m.stats != nil {
		return fmt.Sprintf("%d msgs | %s | %d attchs",
			m.stats.MessageCount,
			formatBytes(m.stats.TotalSize),
			m.stats.AttachmentCount,
		)
	}
	return ""
}

// headerView renders a two-level header:
// Line 1: msgvault [version] - account
// Line 2: breadcrumb | stats
func (m Model) headerView() string {
	line1 := m.buildTitleBar()

	// Build line 2: breadcrumb and stats
	breadcrumb := m.buildBreadcrumb()
	statsStr := m.buildStatsString()

	breadcrumbStyled := statsStyle.Render(" " + breadcrumb + " ")
	statsStyled := statsStyle.Render(statsStr + " ")
	gap := m.width - lipgloss.Width(breadcrumbStyled) - lipgloss.Width(statsStyled)
	if gap < 0 {
		gap = 0
	}
	line2 := breadcrumbStyled + strings.Repeat(" ", gap) + statsStyled

	return line1 + "\n" + line2
}

// aggregateTableView renders the aggregate data table.
func (m Model) aggregateTableView() string {
	if len(m.rows) == 0 && !m.loading && !m.inlineSearchActive && m.searchQuery == "" && m.err == nil {
		var sb strings.Builder
		sb.WriteString(tableHeaderStyle.Render(padRight("   "+viewTypeAbbrev(m.viewType), m.width)))
		sb.WriteString("\n")
		sb.WriteString(separatorStyle.Render(strings.Repeat("\u2500", m.width)))
		sb.WriteString("\n")
		sb.WriteString(normalRowStyle.Render(padRight("   No data", m.width)))
		sb.WriteString("\n")
		// 1 "No data" + (pageSize-2) blanks = pageSize-1 data rows,
		// then +1 info line = pageSize body rows total.
		for i := 1; i < m.pageSize-1; i++ {
			sb.WriteString(normalRowStyle.Render(strings.Repeat(" ", m.width)))
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

	// Calculate column widths (reserve 3 for selection indicator)
	keyWidth := m.width - 43
	if keyWidth < 20 {
		keyWidth = 20
	}
	if keyWidth > 57 {
		keyWidth = 57
	}

	// Header row with sort indicators
	sortIndicator := func(field query.SortField) string {
		if m.sortField == field {
			if m.sortDirection == query.SortDesc {
				return "↓"
			}
			return "↑"
		}
		return ""
	}

	// Use abbreviated view type for column header
	viewLabel := viewTypeAbbrev(m.viewType)
	if si := sortIndicator(query.SortByName); si != "" {
		viewLabel += si
	}

	countLabel := "Count"
	if si := sortIndicator(query.SortByCount); si != "" {
		countLabel += si
	}

	sizeLabel := "Size"
	if si := sortIndicator(query.SortBySize); si != "" {
		sizeLabel += si
	}

	attachLabel := "Attchs"
	if si := sortIndicator(query.SortByAttachmentSize); si != "" {
		attachLabel += si
	}

	headerRow := fmt.Sprintf("   %-*s %10s %12s %12s",
		keyWidth, viewLabel,
		countLabel,
		sizeLabel,
		attachLabel,
	)
	sb.WriteString(tableHeaderStyle.Render(padRight(headerRow, m.width)))
	sb.WriteString("\n")
	sb.WriteString(separatorStyle.Render(strings.Repeat("─", m.width)))
	sb.WriteString("\n")

	// Data rows - show at most pageSize-1 to leave room for info line
	endRow := m.scrollOffset + m.pageSize - 1
	if endRow > len(m.rows) {
		endRow = len(m.rows)
	}

	for i := m.scrollOffset; i < endRow; i++ {
		row := m.rows[i]
		isCursor := i == m.cursor
		isChecked := m.selection.aggregateKeys[row.Key]

		// Selection indicator with cursor pointer
		var selIndicator string
		if isCursor {
			if isChecked {
				selIndicator = selectedIndicatorStyle.Render("▶✓ ")
			} else {
				selIndicator = cursorRowStyle.Render("▶  ")
			}
		} else if isChecked {
			selIndicator = selectedIndicatorStyle.Render(" ✓ ")
		} else {
			selIndicator = "   "
		}

		// Pad key to fixed width first, then highlight — so ANSI codes
		// don't affect column alignment.
		key := truncateRunes(row.Key, keyWidth)
		key = fmt.Sprintf("%-*s", keyWidth, key)
		key = highlightTerms(key, m.searchQuery)

		line := fmt.Sprintf("%s %10s %12s %12s",
			key,
			formatCount(row.Count),
			formatBytes(row.TotalSize),
			formatBytes(row.AttachmentSize),
		)

		var style lipgloss.Style
		if isCursor {
			style = cursorRowStyle
		} else if isChecked {
			style = selectedRowStyle
		} else if i%2 == 0 {
			style = normalRowStyle
		} else {
			style = altRowStyle
		}

		sb.WriteString(selIndicator)
		sb.WriteString(style.Render(padRight(line, m.width-3)))
		sb.WriteString("\n")
	}

	// Show "No results" indicator when search returned zero matches
	if len(m.rows) == 0 && !m.loading {
		sb.WriteString(normalRowStyle.Render(padRight("   No results found", m.width)))
		sb.WriteString("\n")
	}

	// Fill remaining space (minus 1 for notification line).
	// Account for the "No results found" line when rows is empty.
	dataRows := endRow - m.scrollOffset
	if len(m.rows) == 0 && !m.loading {
		dataRows = 1 // the "No results found" row
	}
	for i := dataRows; i < m.pageSize-1; i++ {
		sb.WriteString(normalRowStyle.Render(strings.Repeat(" ", m.width)))
		sb.WriteString("\n")
	}

	// Info line - show inline search bar when active, search filter when searching, otherwise blank
	var infoContent string
	isLoading := m.loading || m.inlineSearchLoading || m.searchLoadingMore
	if m.inlineSearchActive {
		infoContent = "/" + m.searchInput.View()
	} else if m.searchQuery != "" {
		infoContent = fmt.Sprintf(" Search: %q", m.searchQuery)
	}
	sb.WriteString(m.renderInfoLine(infoContent, isLoading))

	// Overlay modal if active
	if m.modal != modalNone {
		return m.overlayModal(sb.String())
	}

	return sb.String()
}

// messageListView renders the message list.
func (m Model) messageListView() string {
	// Non-search empty state: show simple "No messages" without header/info line.
	// When a search is active (or search bar is open), fall through to full
	// rendering so the search bar stays visible and the user can edit their query.
	if len(m.messages) == 0 && !m.loading && !m.inlineSearchActive && m.searchQuery == "" && m.err == nil {
		return m.fillScreen(normalRowStyle.Render(padRight("No messages", m.width)), 1)
	}

	var sb strings.Builder

	// Calculate column widths (reserve 3 for selection indicator)
	dateWidth := 16
	fromWidth := 25
	sizeWidth := 8
	subjectWidth := m.width - dateWidth - fromWidth - sizeWidth - 9
	if subjectWidth < 20 {
		subjectWidth = 20
	}

	// Header row with sort indicators
	msgSortIndicator := func(field query.MessageSortField) string {
		if m.msgSortField == field {
			if m.msgSortDirection == query.SortDesc {
				return "↓"
			}
			return "↑"
		}
		return ""
	}

	dateLabel := "Date"
	if si := msgSortIndicator(query.MessageSortByDate); si != "" {
		dateLabel += si
	}

	sizeLabel := "Size"
	if si := msgSortIndicator(query.MessageSortBySize); si != "" {
		sizeLabel += si
	}

	subjectLabel := "Subject"
	if si := msgSortIndicator(query.MessageSortBySubject); si != "" {
		subjectLabel += si
	}

	headerRow := fmt.Sprintf("   %-*s  %-*s  %-*s  %*s",
		dateWidth, dateLabel,
		fromWidth, "From",
		subjectWidth, subjectLabel,
		sizeWidth, sizeLabel,
	)
	sb.WriteString(tableHeaderStyle.Render(padRight(headerRow, m.width)))
	sb.WriteString("\n")
	sb.WriteString(separatorStyle.Render(strings.Repeat("─", m.width)))
	sb.WriteString("\n")

	// Data rows - show at most pageSize-1 to leave room for info line
	endRow := m.scrollOffset + m.pageSize - 1
	if endRow > len(m.messages) {
		endRow = len(m.messages)
	}

	// Show "No results" indicator when search returned zero matches
	if len(m.messages) == 0 && !m.loading {
		sb.WriteString(normalRowStyle.Render(padRight("   No results found", m.width)))
		sb.WriteString("\n")
	}

	for i := m.scrollOffset; i < endRow; i++ {
		msg := m.messages[i]
		isCursor := i == m.cursor
		isChecked := m.selection.messageIDs[msg.ID]

		// Selection indicator with cursor pointer
		var selIndicator string
		if isCursor {
			if isChecked {
				selIndicator = selectedIndicatorStyle.Render("▶✓ ")
			} else {
				selIndicator = cursorRowStyle.Render("▶  ")
			}
		} else if isChecked {
			selIndicator = selectedIndicatorStyle.Render(" ✓ ")
		} else {
			selIndicator = "   "
		}

		// Format date
		date := msg.SentAt.Format("2006-01-02 15:04")

		// Format from (rune-aware for international names)
		// Sanitize untrusted metadata to prevent terminal control-sequence injection.
		from := textutil.SanitizeTerminal(msg.FromEmail)
		if msg.FromName != "" {
			from = textutil.SanitizeTerminal(msg.FromName)
		}
		// For chat messages: fall back to phone number, then group title
		if from == "" && msg.FromPhone != "" {
			from = textutil.SanitizeTerminal(msg.FromPhone)
		}
		if from == "" && msg.ConversationTitle != "" {
			from = textutil.SanitizeTerminal(msg.ConversationTitle)
		}
		from = truncateRunes(from, fromWidth)
		from = fmt.Sprintf("%-*s", fromWidth, from)
		from = highlightTerms(from, m.searchQuery)

		// Format subject with indicators (rune-aware)
		// For chat messages without a subject, show snippet or group title
		subject := textutil.SanitizeTerminal(msg.Subject)
		if subject == "" && msg.MessageType == "whatsapp" {
			title := textutil.SanitizeTerminal(msg.ConversationTitle)
			snippet := textutil.SanitizeTerminal(msg.Snippet)
			if title != "" {
				subject = title + ": " + snippet
			} else {
				subject = snippet
			}
		}
		if msg.DeletedAt != nil {
			subject = "🗑 " + subject // Deleted from server indicator
		}
		if msg.HasAttachments {
			subject = "📎 " + subject
		}
		subject = truncateRunes(subject, subjectWidth)
		subject = fmt.Sprintf("%-*s", subjectWidth, subject)
		subject = highlightTerms(subject, m.searchQuery)

		// Format size
		size := formatBytes(msg.SizeEstimate)

		line := fmt.Sprintf("%-*s  %s  %s  %*s",
			dateWidth, date,
			from,
			subject,
			sizeWidth, size,
		)

		var style lipgloss.Style
		if isCursor {
			style = cursorRowStyle
		} else if isChecked {
			style = selectedRowStyle
		} else if i%2 == 0 {
			style = normalRowStyle
		} else {
			style = altRowStyle
		}

		sb.WriteString(selIndicator)
		sb.WriteString(style.Render(padRight(line, m.width-3)))
		sb.WriteString("\n")
	}

	// Fill remaining space (minus 1 for info line).
	// Account for the "No results found" line when messages is empty.
	dataRows := endRow - m.scrollOffset
	if len(m.messages) == 0 && !m.loading {
		dataRows = 1 // the "No results found" row
	}
	for i := dataRows; i < m.pageSize-1; i++ {
		sb.WriteString(normalRowStyle.Render(strings.Repeat(" ", m.width)))
		sb.WriteString("\n")
	}

	// Info line - show inline search bar when active, search info when searching, otherwise blank
	var infoContent string
	isLoading := m.loading || m.inlineSearchLoading || m.searchLoadingMore
	if m.inlineSearchActive {
		modeTag := "[Fast]"
		if m.searchMode == searchModeDeep {
			modeTag = "[Deep]"
		}
		infoContent = modeTag + "/" + m.searchInput.View()
	} else if m.searchQuery != "" {
		infoContent = fmt.Sprintf(" Search: %q", m.searchQuery)
		if m.searchTotalCount > 0 {
			infoContent += fmt.Sprintf(" (%d results)", m.searchTotalCount)
		} else if m.searchTotalCount == 0 {
			infoContent += " (0 results)"
		} else if m.searchTotalCount == -1 {
			infoContent += fmt.Sprintf(" (%d+ results, PgDn for more)", len(m.messages))
		}
		if m.searchMode == searchModeDeep {
			infoContent += " [Deep]"
		}
	}
	sb.WriteString(m.renderInfoLine(infoContent, isLoading))

	// Overlay modal if active
	if m.modal != modalNone {
		return m.overlayModal(sb.String())
	}

	return sb.String()
}

// buildDetailLines constructs the lines for message detail view.
func (m Model) buildDetailLines() []string {
	if m.messageDetail == nil {
		return nil
	}

	msg := m.messageDetail
	var lines []string

	// Subject
	lines = append(lines, fmt.Sprintf("Subject: %s", msg.Subject))
	lines = append(lines, "")

	// Date
	lines = append(lines, fmt.Sprintf("Date: %s", msg.SentAt.Format("Mon, 02 Jan 2006 15:04:05 MST")))

	// From
	if len(msg.From) > 0 {
		from := formatAddresses(msg.From)
		lines = append(lines, fmt.Sprintf("From: %s", from))
	}

	// To
	if len(msg.To) > 0 {
		to := formatAddresses(msg.To)
		lines = append(lines, fmt.Sprintf("To: %s", to))
	}

	// Cc
	if len(msg.Cc) > 0 {
		cc := formatAddresses(msg.Cc)
		lines = append(lines, fmt.Sprintf("Cc: %s", cc))
	}

	// Bcc
	if len(msg.Bcc) > 0 {
		bcc := formatAddresses(msg.Bcc)
		lines = append(lines, fmt.Sprintf("Bcc: %s", bcc))
	}

	// Labels
	if len(msg.Labels) > 0 {
		lines = append(lines, fmt.Sprintf("Labels: %s", strings.Join(msg.Labels, ", ")))
	}

	// Attachments
	if len(msg.Attachments) > 0 {
		lines = append(lines, "")
		lines = append(lines, fmt.Sprintf("Attachments (%d):", len(msg.Attachments)))
		for _, att := range msg.Attachments {
			lines = append(lines, fmt.Sprintf("  📎 %s (%s)", att.Filename, formatBytes(att.Size)))
		}
	}

	// Separator
	lines = append(lines, "")
	sepWidth := min(m.width-2, 80)
	if sepWidth < 1 {
		sepWidth = 40 // Reasonable default
	}
	lines = append(lines, strings.Repeat("─", sepWidth))
	lines = append(lines, "")

	// Body - wrap lines to fit width
	body := msg.BodyText
	if body == "" {
		body = "(No text content)"
	}
	// Strip carriage returns (CRLF -> LF) to prevent display issues
	body = strings.ReplaceAll(body, "\r\n", "\n")
	body = strings.ReplaceAll(body, "\r", "")
	bodyLines := wrapText(body, m.width-2)
	lines = append(lines, bodyLines...)

	return lines
}

// fillScreenWithPageSize fills the remaining screen space with blank lines up to the given page size.
// Used for loading/error/empty states in all views.
func (m Model) fillScreenWithPageSize(content string, usedLines, pageSize int) string {
	// Guard against zero/negative width (can happen before first resize)
	if m.width <= 0 {
		return content + "\n"
	}

	var sb strings.Builder
	sb.WriteString(content)
	sb.WriteString("\n")
	// Fill remaining space (minus 1 for notification line)
	for i := usedLines; i < pageSize-1; i++ {
		sb.WriteString(normalRowStyle.Render(strings.Repeat(" ", m.width)))
		sb.WriteString("\n")
	}
	// Notification line (blank for now)
	sb.WriteString(normalRowStyle.Render(strings.Repeat(" ", m.width)))
	return sb.String()
}

// fillScreen fills the remaining screen space with blank lines for table views.
func (m Model) fillScreen(content string, usedLines int) string {
	return m.fillScreenWithPageSize(content, usedLines, m.pageSize)
}

// fillScreenDetail fills remaining space for detail view (uses detailPageSize).
func (m Model) fillScreenDetail(content string, usedLines int) string {
	return m.fillScreenWithPageSize(content, usedLines, m.detailPageSize())
}

// messageDetailView renders the full message.
func (m Model) messageDetailView() string {
	if m.messageDetail == nil {
		if m.loading {
			return m.fillScreenDetail(loadingStyle.Render(padRight(m.spinnerIndicator()+" Loading message...", m.width)), 1)
		}
		content := m.fillScreenDetail(normalRowStyle.Render(strings.Repeat(" ", m.width)), 1)
		if m.modal != modalNone {
			return m.overlayModal(content)
		}
		return m.fillScreenDetail(errorStyle.Render(padRight("Message not found (nil detail)", m.width)), 1)
	}

	lines := m.buildDetailLines()

	// Apply scrolling with bounds check
	// Detail view has 2 extra lines vs table views (no table header or separator)
	detailPageSize := m.detailPageSize()
	startLine := m.detailScroll
	if startLine >= len(lines) {
		startLine = len(lines) - 1
	}
	if startLine < 0 {
		startLine = 0
	}

	endLine := startLine + detailPageSize
	if endLine > len(lines) {
		endLine = len(lines)
	}

	visibleLines := lines[startLine:endLine]

	// Determine active highlight query: detail search overrides global search
	detailHighlightQuery := m.detailSearchQuery
	if detailHighlightQuery == "" {
		detailHighlightQuery = m.searchQuery
	}

	var sb strings.Builder
	for lineIdx, line := range visibleLines {
		if detailHighlightQuery != "" {
			line = highlightTerms(line, detailHighlightQuery)
		}
		// Highlight current detail search match line
		absLine := startLine + lineIdx
		if m.detailSearchQuery != "" && len(m.detailSearchMatches) > 0 &&
			m.detailSearchMatchIndex < len(m.detailSearchMatches) &&
			absLine == m.detailSearchMatches[m.detailSearchMatchIndex] {
			// Current match line gets a subtle indicator
			line = "▶ " + line
		}
		sb.WriteString(normalRowStyle.Render(padRight(line, m.width)))
		sb.WriteString("\n")
	}

	// Fill remaining space (minus 1 for notification line)
	for i := len(visibleLines); i < detailPageSize-1; i++ {
		sb.WriteString(normalRowStyle.Render(strings.Repeat(" ", m.width)))
		sb.WriteString("\n")
	}

	// Notification line - show detail search bar, flash, loading, or blank
	if m.detailSearchActive {
		infoContent := "/" + m.detailSearchInput.View()
		sb.WriteString(m.renderInfoLine(infoContent, false))
	} else if m.detailSearchQuery != "" {
		matchInfo := fmt.Sprintf(" /%s", m.detailSearchQuery)
		if len(m.detailSearchMatches) > 0 {
			matchInfo += fmt.Sprintf(" [%d/%d]", m.detailSearchMatchIndex+1, len(m.detailSearchMatches))
		} else {
			matchInfo += " [no matches]"
		}
		sb.WriteString(m.renderInfoLine(matchInfo, false))
	} else {
		sb.WriteString(m.renderNotificationLine())
	}

	// Overlay modal if active
	if m.modal != modalNone {
		return m.overlayModal(sb.String())
	}

	return sb.String()
}

// threadView renders the thread/conversation view.
func (m Model) threadView() string {
	if m.loading && len(m.threadMessages) == 0 {
		return m.fillScreen(loadingStyle.Render(padRight(m.spinnerIndicator()+" Loading thread...", m.width)), 1)
	}

	if !m.loading && len(m.threadMessages) == 0 {
		content := m.fillScreen(normalRowStyle.Render(padRight("No messages in thread", m.width)), 1)
		if m.modal != modalNone {
			return m.overlayModal(content)
		}
		return content
	}

	var sb strings.Builder

	// Calculate column widths (reserve 3 for selection indicator + 6 for spacing)
	dateWidth := 16
	sizeWidth := 8
	fromSubjectWidth := m.width - dateWidth - sizeWidth - 9
	if fromSubjectWidth < 1 {
		fromSubjectWidth = 1
	}

	// Header row
	headerRow := fmt.Sprintf("   %-*s  %-*s  %*s",
		dateWidth, "Date",
		fromSubjectWidth, "From / Subject",
		sizeWidth, "Size",
	)
	sb.WriteString(tableHeaderStyle.Render(padRight(headerRow, m.width)))
	sb.WriteString("\n")

	// Separator
	sb.WriteString(separatorStyle.Render(strings.Repeat("─", m.width)))
	sb.WriteString("\n")

	// Calculate visible rows (account for header + separator + notification line)
	visibleRows := m.height - 5 // header, breadcrumb, table header, separator, footer
	if visibleRows < 1 {
		visibleRows = 1
	}

	// Determine visible range
	endRow := m.threadScrollOffset + visibleRows
	if endRow > len(m.threadMessages) {
		endRow = len(m.threadMessages)
	}

	// Render visible messages
	for i := m.threadScrollOffset; i < endRow; i++ {
		msg := m.threadMessages[i]
		isCursor := i == m.threadCursor

		// Selection indicator (styled to match row background)
		var selIndicator string
		if isCursor {
			selIndicator = cursorRowStyle.Render("▶  ")
		} else if i%2 == 0 {
			selIndicator = normalRowStyle.Render("   ")
		} else {
			selIndicator = altRowStyle.Render("   ")
		}

		// Format date
		dateStr := msg.SentAt.Format("2006-01-02 15:04")

		// Format from/subject with deleted indicator
		// Sanitize untrusted metadata to prevent terminal control-sequence injection.
		fromSubject := textutil.SanitizeTerminal(msg.FromEmail)
		if msg.FromName != "" {
			fromSubject = textutil.SanitizeTerminal(msg.FromName)
		}
		// For chat messages: fall back to phone number
		if fromSubject == "" && msg.FromPhone != "" {
			fromSubject = textutil.SanitizeTerminal(msg.FromPhone)
		}
		if msg.Subject != "" {
			fromSubject = truncateRunes(fromSubject, 18) + ": " + textutil.SanitizeTerminal(msg.Subject)
		} else if msg.MessageType == "whatsapp" && msg.Snippet != "" {
			fromSubject = truncateRunes(fromSubject, 18) + ": " + textutil.SanitizeTerminal(msg.Snippet)
		}
		if msg.DeletedAt != nil {
			fromSubject = "🗑 " + fromSubject // Deleted from server indicator
		}
		fromSubject = truncateRunes(fromSubject, fromSubjectWidth)
		fromSubject = fmt.Sprintf("%-*s", fromSubjectWidth, fromSubject)
		fromSubject = highlightTerms(fromSubject, m.searchQuery)

		// Format size
		sizeStr := formatBytes(msg.SizeEstimate)

		// Build row
		line := fmt.Sprintf("%-*s  %s  %*s",
			dateWidth, dateStr,
			fromSubject,
			sizeWidth, sizeStr,
		)

		// Apply style
		var style lipgloss.Style
		if isCursor {
			style = cursorRowStyle
		} else if i%2 == 0 {
			style = normalRowStyle
		} else {
			style = altRowStyle
		}

		sb.WriteString(selIndicator)
		sb.WriteString(style.Render(padRight(line, m.width-3)))
		sb.WriteString("\n")
	}

	// Fill remaining space (minus 1 for notification line)
	for i := endRow - m.threadScrollOffset; i < visibleRows-1; i++ {
		sb.WriteString(normalRowStyle.Render(strings.Repeat(" ", m.width)))
		sb.WriteString("\n")
	}

	// Notification line - show flash, loading indicator, or blank
	sb.WriteString(m.renderNotificationLine())

	// Overlay modal if active
	if m.modal != modalNone {
		return m.overlayModal(sb.String())
	}

	return sb.String()
}

// footerView renders the footer with keybindings.
func (m Model) footerView() string {
	var keys []string
	var posStr string
	var selStr string

	// Selection count
	selCount := m.selectionCount()
	if selCount > 0 {
		selStr = fmt.Sprintf(" [%d selected] ", selCount)
	}

	switch m.level {
	case levelAggregates:
		keys = []string{
			"↑/k",
			"↓/j",
			"Enter",
			"g group",
			"s sort",
			"A acct",
			"a msgs",
			"d del",
		}
		keys = append(keys, "? help")
		if len(m.rows) > 0 {
			// Use TotalUnique from aggregate rows for true total count
			totalUnique := m.rows[0].TotalUnique
			if totalUnique > 0 && totalUnique > int64(len(m.rows)) {
				// More rows exist than loaded - show "N of M"
				posStr = fmt.Sprintf(" %d of %d ", m.cursor+1, totalUnique)
			} else {
				posStr = fmt.Sprintf(" %d/%d ", m.cursor+1, len(m.rows))
			}
		}

	case levelDrillDown:
		keys = []string{
			"↑/k",
			"↓/j",
			"Enter",
			"Esc",
			"g group",
			"s sort",
			"A acct",
			"a msgs",
			"d del",
		}
		keys = append(keys, "? help")
		if len(m.rows) > 0 {
			// Use TotalUnique from aggregate rows for true total count
			totalUnique := m.rows[0].TotalUnique
			if totalUnique > 0 && totalUnique > int64(len(m.rows)) {
				// More rows exist than loaded - show "N of M"
				posStr = fmt.Sprintf(" %d of %d ", m.cursor+1, totalUnique)
			} else {
				posStr = fmt.Sprintf(" %d/%d ", m.cursor+1, len(m.rows))
			}
		}

	case levelMessageList:
		keys = []string{
			"↑/k",
			"↓/j",
			"Enter",
			"Esc",
			"Space",
			"s sort",
			"d del",
		}
		keys = append(keys, "/ search", "? help")
		if len(m.messages) > 0 {
			// Show position / total - use contextStats for actual total when drilled down,
			// or global stats for All Messages view
			total := int64(len(m.messages))
			if m.contextStats != nil && m.contextStats.MessageCount > total {
				total = m.contextStats.MessageCount
			} else if m.allMessages && m.stats != nil && m.stats.MessageCount > total {
				// All Messages view - use global stats for total count
				total = m.stats.MessageCount
			}
			if total > int64(len(m.messages)) {
				// More messages exist than loaded - show "N of M"
				posStr = fmt.Sprintf(" %d of %d ", m.cursor+1, total)
			} else {
				posStr = fmt.Sprintf(" %d/%d ", m.cursor+1, len(m.messages))
			}
		}

	case levelMessageDetail:
		keys = []string{
			"←/→ prev/next",
			"↑/↓ scroll",
			"/ find",
		}
		if m.detailSearchQuery != "" {
			keys = append(keys, "n/N next/prev")
		}
		// Show export option if message has attachments
		if m.messageDetail != nil && len(m.messageDetail.Attachments) > 0 {
			keys = append(keys, "e export")
		}
		keys = append(keys, "Esc back", "q quit")
		// Show message position (N/M) in the list - reuse total from parent view
		if len(m.messages) > 0 {
			total := int64(len(m.messages))
			if m.contextStats != nil && m.contextStats.MessageCount > total {
				total = m.contextStats.MessageCount
			} else if m.allMessages && m.stats != nil && m.stats.MessageCount > total {
				total = m.stats.MessageCount
			}
			posStr = fmt.Sprintf(" msg %d/%d ", m.detailMessageIndex+1, total)
		} else {
			posStr = ""
		}

	case levelThreadView:
		keys = []string{
			"↑/↓ navigate",
			"Enter view",
			"Esc back",
			"q quit",
		}
		if len(m.threadMessages) > 0 {
			posStr = fmt.Sprintf(" %d/%d ", m.threadCursor+1, len(m.threadMessages))
		}
	}

	keysStr := strings.Join(keys, " │ ")

	// Use lipgloss.Width for ANSI-aware width calculation (handles Unicode arrows ↑↓ correctly)
	gap := m.width - lipgloss.Width(keysStr) - lipgloss.Width(posStr) - lipgloss.Width(selStr) - 2
	if gap < 0 {
		gap = 0
	}

	return footerStyle.Render(keysStr + strings.Repeat(" ", gap) + selStr + posStr)
}

// spinnerIndicator returns the current spinner frame string.
func (m Model) spinnerIndicator() string {
	if m.spinnerFrame < len(spinnerFrames) {
		return spinnerFrames[m.spinnerFrame]
	}
	return spinnerFrames[0]
}

// renderInfoLine renders the info/notification line with optional right-aligned loading spinner.
// Used on the second-to-last line of table views (before footer).
func (m Model) renderInfoLine(content string, loading bool) string {
	// statsStyle has Padding(0, 1) which adds 2 characters, so content should be m.width-2
	contentWidth := m.width - 2
	if contentWidth < 1 {
		contentWidth = 1
	}

	if content == "" && !loading {
		return statsStyle.Render(strings.Repeat(" ", contentWidth))
	}
	if loading {
		indicator := m.spinnerIndicator()
		currentWidth := lipgloss.Width(content)
		indicatorWidth := lipgloss.Width(indicator)
		gap := contentWidth - currentWidth - indicatorWidth
		if gap < 1 {
			gap = 1
		}
		// Render spinner with bold style so it's visible (statsStyle is faint)
		styledIndicator := spinnerStyle.Render(indicator)
		content += strings.Repeat(" ", gap) + styledIndicator
	}
	return statsStyle.Render(padRight(content, contentWidth))
}

// renderNotificationLine renders the notification line for detail/thread views.
// Shows flash message, right-aligned loading spinner, or blank.
func (m Model) renderNotificationLine() string {
	if m.flashMessage != "" {
		if m.loading {
			// Flash + loading spinner
			indicator := m.spinnerIndicator()
			flash := " " + m.flashMessage
			flashWidth := lipgloss.Width(flash)
			indicatorWidth := lipgloss.Width(indicator)
			gap := m.width - flashWidth - indicatorWidth
			if gap < 1 {
				gap = 1
			}
			return flashStyle.Render(padRight(flash+strings.Repeat(" ", gap)+indicator, m.width))
		}
		return flashStyle.Render(padRight(" "+m.flashMessage, m.width))
	}
	if m.loading {
		return m.renderInfoLine("", true)
	}
	return normalRowStyle.Render(strings.Repeat(" ", m.width))
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// overlayModal renders a modal dialog over the content.
// rawHelpLines contains the help modal content. The first line is the title
// (rendered with modalTitleStyle at display time). This is a package-level
// variable so len() can be used without rebuilding the slice on every call.
var rawHelpLines = []string{
	"Keyboard Shortcuts", // rendered with modalTitleStyle in overlayModal
	"",
	"Navigation",
	"  ↑/k, ↓/j    Move cursor up/down",
	"  ←/h, →/l    Prev/next message (in detail view)",
	"  PgUp/PgDn   Page up/down",
	"  Home/End    Go to first/last",
	"  Enter       Drill down",
	"  Esc         Go back",
	"",
	"Views & Sorting",
	"  g/Tab       Cycle view types",
	"  t           Jump to Time view (cycle granularity when in Time)",
	"  s           Cycle sort field",
	"  v/r         Reverse sort order",
	"",
	"Selection & Actions",
	"  Space       Toggle selection",
	"  S           Select all visible",
	"  x           Clear selection",
	"  d/D         Stage for deletion",
	"  a           View all messages",
	"",
	"Other",
	"  /           Search",
	"  A           Select account",
	"  f           Filter (attachments, deleted)",
	"  e           Export attachments (in message view)",
	"  m           Toggle Email/Texts mode",
	"  q           Quit",
	"",
	"[↑/↓] Scroll  [Any other key] Close",
}

// helpMaxVisible returns the max visible lines for the help modal given terminal height.
func (m Model) helpMaxVisible() int {
	v := m.height - 6
	if v < 1 {
		v = 1
	}
	if v > len(rawHelpLines) {
		v = len(rawHelpLines)
	}
	return v
}

// renderDeleteConfirmModal renders the deletion confirmation modal content.
func (m Model) renderDeleteConfirmModal() string {
	if m.pendingManifest == nil {
		return ""
	}
	var sb strings.Builder
	sb.WriteString(modalTitleStyle.Render("Confirm Deletion"))
	sb.WriteString("\n\n")
	_, _ = fmt.Fprintf(&sb, "Stage %d messages for deletion?\n\n", len(m.pendingManifest.GmailIDs))
	sb.WriteString("This creates a deletion batch. Messages will NOT be\n")
	sb.WriteString("deleted until you run 'msgvault delete-staged'\n")
	sb.WriteString("with MSGVAULT_ENABLE_REMOTE_DELETE=1 set.\n\n")
	if m.pendingManifest.Filters.Account == "" {
		sb.WriteString("! Account not set. Use --account when executing.\n\n")
	}
	sb.WriteString("[Y] Yes, stage for deletion    [N] Cancel")
	return sb.String()
}

// renderDeleteResultModal renders the deletion result modal content.
func (m Model) renderDeleteResultModal() string {
	return modalTitleStyle.Render("Result") + "\n\n" +
		m.modalResult + "\n\n" +
		"Press any key to continue"
}

// renderQuitConfirmModal renders the quit confirmation modal content.
func (m Model) renderQuitConfirmModal() string {
	return modalTitleStyle.Render("Quit?") + "\n\n" +
		"Are you sure you want to quit?\n\n" +
		"[Y] Yes    [N] No"
}

// renderAccountSelectorModal renders the account selector modal content.
func (m Model) renderAccountSelectorModal() string {
	var sb strings.Builder
	sb.WriteString(modalTitleStyle.Render("Select Account"))
	sb.WriteString("\n\n")
	// All Accounts option
	indicator := "○"
	if m.modalCursor == 0 {
		indicator = "●"
	}
	_, _ = fmt.Fprintf(&sb, " %s All Accounts\n", indicator)
	// Individual accounts
	for i, acc := range m.accounts {
		indicator = "○"
		if m.modalCursor == i+1 {
			indicator = "●"
		}
		_, _ = fmt.Fprintf(&sb, " %s %s\n", indicator, acc.Identifier)
	}
	sb.WriteString("\n[↑/↓] Navigate  [Enter] Select  [Esc] Cancel")
	return sb.String()
}

// renderFilterModal renders the filter toggle modal content with checkboxes.
func (m Model) renderFilterModal() string {
	var sb strings.Builder
	sb.WriteString(modalTitleStyle.Render("Filter Messages"))
	sb.WriteString("\n\n")

	type filterOption struct {
		label   string
		checked bool
	}
	options := []filterOption{
		{"Only with attachments", m.filters.attachmentsOnly},
		{"Hide deleted from source", m.filters.hideDeletedFromSource},
	}

	for i, opt := range options {
		cursor := "  "
		if m.modalCursor == i {
			cursor = "▶ "
		}
		checkbox := "[ ]"
		if opt.checked {
			checkbox = "[x]"
		}
		_, _ = fmt.Fprintf(&sb, "%s%s %s\n", cursor, checkbox, opt.label)
	}

	sb.WriteString("\n[↑/↓] Navigate  [Space/x] Toggle  [Enter/Esc] Apply")
	return sb.String()
}

// renderHelpModal renders the help modal content with scrolling support.
func (m Model) renderHelpModal() string {
	maxVisible := m.helpMaxVisible()

	// Clamp scroll offset
	maxScroll := len(rawHelpLines) - maxVisible
	if maxScroll < 0 {
		maxScroll = 0
	}
	if m.helpScroll > maxScroll {
		m.helpScroll = maxScroll
	}

	// Build visible slice, rendering the title line with style
	visible := rawHelpLines[m.helpScroll : m.helpScroll+maxVisible]
	rendered := make([]string, len(visible))
	for i, line := range visible {
		if m.helpScroll+i == 0 {
			rendered[i] = modalTitleStyle.Render(line)
		} else {
			rendered[i] = line
		}
	}
	return strings.Join(rendered, "\n")
}

// renderExportAttachmentsModal renders the export attachments modal content.
func (m Model) renderExportAttachmentsModal() string {
	if m.messageDetail == nil || len(m.messageDetail.Attachments) == 0 {
		return modalTitleStyle.Render("Export Attachments") + "\n\n" +
			"No attachments to export.\n\n" +
			"[Esc] Close"
	}
	var sb strings.Builder
	sb.WriteString(modalTitleStyle.Render("Export Attachments"))
	sb.WriteString("\n\n")
	sb.WriteString("Select attachments to export:\n\n")
	for i, att := range m.messageDetail.Attachments {
		cursor := " "
		if i == m.exportCursor {
			cursor = "▶"
		}
		checkbox := "☐"
		if m.exportSelection[i] {
			checkbox = "☑"
		}
		_, _ = fmt.Fprintf(&sb, "%s %s %s (%s)\n", cursor, checkbox, att.Filename, formatBytes(att.Size))
	}
	// Count selected
	selectedCount := 0
	for _, selected := range m.exportSelection {
		if selected {
			selectedCount++
		}
	}
	_, _ = fmt.Fprintf(&sb, "\n%d of %d selected\n", selectedCount, len(m.messageDetail.Attachments))
	sb.WriteString("\n[↑/↓] Navigate  [Space] Toggle  [a] All  [n] None\n")
	sb.WriteString("[Enter] Export  [Esc] Cancel")
	return sb.String()
}

// renderErrorModal renders the error modal content.
func (m Model) renderErrorModal() string {
	return modalTitleStyle.Render("Error") + "\n\n" +
		m.modalResult + "\n\n" +
		"Press any key to dismiss"
}

// renderExportResultModal renders the export result modal content.
func (m Model) renderExportResultModal() string {
	return modalTitleStyle.Render("Export Complete") + "\n\n" +
		m.modalResult + "\n\n" +
		"Press any key to close"
}

func (m Model) overlayModal(background string) string {
	var modalContent string

	switch m.modal {
	case modalDeleteConfirm:
		modalContent = m.renderDeleteConfirmModal()
	case modalDeleteResult:
		modalContent = m.renderDeleteResultModal()
	case modalQuitConfirm:
		modalContent = m.renderQuitConfirmModal()
	case modalAccountSelector:
		modalContent = m.renderAccountSelectorModal()
	case modalFilterToggle:
		modalContent = m.renderFilterModal()
	case modalHelp:
		modalContent = m.renderHelpModal()
	case modalExportAttachments:
		modalContent = m.renderExportAttachmentsModal()
	case modalExportResult:
		modalContent = m.renderExportResultModal()
	case modalError:
		modalContent = m.renderErrorModal()
	}

	if modalContent == "" {
		return background
	}

	// Render modal box
	modal := modalStyle.Render(modalContent)

	// Split background and modal into lines
	bgLines := strings.Split(background, "\n")
	modalLines := strings.Split(modal, "\n")

	// Calculate vertical centering
	modalHeight := len(modalLines)
	startLine := (len(bgLines) - modalHeight) / 2
	if startLine < 0 {
		startLine = 0
	}

	// Calculate horizontal centering
	modalWidth := lipgloss.Width(modal)
	leftPadding := (m.width - modalWidth) / 2
	if leftPadding < 0 {
		leftPadding = 0
	}

	// Overlay modal onto background, preserving background where modal doesn't cover
	for i, modalLine := range modalLines {
		lineIdx := startLine + i
		if lineIdx >= len(bgLines) {
			break
		}
		// Get background line and its visual width
		bgLine := bgLines[lineIdx]
		bgWidth := lipgloss.Width(bgLine)

		// Build composite line: left background + modal + right background
		var composite strings.Builder

		// Left portion of background (before modal)
		if leftPadding > 0 {
			leftBg := truncateToWidth(bgLine, leftPadding)
			composite.WriteString(leftBg)
			// Pad if background is shorter than left padding
			if lipgloss.Width(leftBg) < leftPadding {
				composite.WriteString(strings.Repeat(" ", leftPadding-lipgloss.Width(leftBg)))
			}
		}

		// Modal content
		composite.WriteString(modalLine)

		// Right portion of background (after modal)
		rightStart := leftPadding + modalWidth
		if rightStart < bgWidth {
			rightBg := skipToWidth(bgLine, rightStart)
			composite.WriteString(rightBg)
		}

		bgLines[lineIdx] = composite.String()
	}

	return strings.Join(bgLines, "\n")
}
