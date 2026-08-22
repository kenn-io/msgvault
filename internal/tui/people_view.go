package tui

import (
	"fmt"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/textutil"
)

var peopleTabLabels = [...]string{
	"Overview", "Attributes", "Inboxes", "Meetings", "Files", "Activity",
}

func (m Model) renderPeopleView() string {
	body := m.peopleDirectoryView()
	if m.peopleState.level != peopleLevelDirectory {
		body = m.peopleContactView()
	}
	content := fmt.Sprintf("%s\n%s\n%s", m.peopleHeaderView(), body, m.peopleFooterView())
	if m.modal != modalNone {
		content = m.overlayModal(content)
	}
	return m.renderPeopleFormOverlay(content)
}

func (m Model) peopleHeaderView() string {
	titleText := "msgvault"
	if m.version != "" && m.version != "dev" && m.version != "unknown" {
		titleText = fmt.Sprintf("msgvault [%s]", m.version)
	}
	if m.peopleState.level == peopleLevelDirectory {
		line1 := m.styles.titleBar.Render(padRight(
			fmt.Sprintf("%s [People]", titleText), max(m.width-2, 0),
		))
		position := fmt.Sprintf("%d loaded", len(m.peopleState.rows))
		if m.peopleState.totalCount > 0 {
			position = fmt.Sprintf("%d of %d", len(m.peopleState.rows), m.peopleState.totalCount)
		}
		line2 := m.styles.stats.Render(padRight(
			truncateToWidth(" People Directory | "+position+" ", max(m.width-2, 0)),
			max(m.width-2, 0),
		))
		return line1 + "\n" + line2
	}

	label := "Loading…"
	status := "observed contact"
	contact := m.peopleState.contact
	if contact != nil {
		label = textutil.SanitizeTerminal(contact.DisplayLabel)
		if contact.Profile != nil {
			status = "curated profile"
		}
	}
	line1 := m.styles.titleBar.Render(padRight(
		truncateToWidth(fmt.Sprintf("%s [People] - %s", titleText, label), max(m.width-2, 0)),
		max(m.width-2, 0),
	))
	detail := " " + status + " "
	if contact != nil {
		identifiers := textutil.SanitizeTerminal(compactPeopleIdentifiers(contact.Identifiers))
		if m.width < 90 {
			counts := fmt.Sprintf("A:%d M:%d F:%d", contact.ActivityCount,
				contact.MeetingCount, contact.FileCount)
			identifierWidth := max(m.width-2-lipgloss.Width(counts)-4, 1)
			detail = fmt.Sprintf(" %s | %s ",
				truncateToWidth(identifiers, identifierWidth), counts)
		} else {
			detail = fmt.Sprintf(
				" %s | %s | Activity: %d | Meetings: %d | Files: %d | First %s | Latest %s ",
				status, identifiers, contact.ActivityCount, contact.MeetingCount,
				contact.FileCount, formatPeopleTime(contact.FirstAt), formatPeopleTime(contact.LastAt),
			)
		}
	}
	line2 := m.styles.stats.Render(padRight(
		truncateToWidth(detail, max(m.width-2, 0)), max(m.width-2, 0),
	))
	return line1 + "\n" + line2
}

type peopleDirectoryColumns struct {
	labelWidth      int
	identifierWidth int
	lastWidth       int
	showIdentifiers bool
	showLast        bool
	showCounts      bool
}

func peopleColumns(width int) peopleDirectoryColumns {
	columns := peopleDirectoryColumns{labelWidth: max(width-3, 1)}
	switch {
	case width >= 80:
		columns.showIdentifiers = true
		columns.showLast = true
		columns.showCounts = true
		remaining := max(width-40, 2)
		columns.labelWidth = min(30, max(16, remaining/2))
		columns.identifierWidth = max(remaining-columns.labelWidth, 1)
		columns.lastWidth = 16
	case width >= 60:
		columns.showIdentifiers = true
		columns.showLast = true
		remaining := max(width-23, 2)
		columns.labelWidth = min(24, max(12, remaining/2))
		columns.identifierWidth = max(remaining-columns.labelWidth, 1)
		columns.lastWidth = 16
	case width >= 45:
		columns.showIdentifiers = true
		remaining := max(width-5, 2)
		columns.labelWidth = min(22, max(12, remaining/2))
		columns.identifierWidth = max(remaining-columns.labelWidth, 1)
	}
	return columns
}

func peopleCell(value string, width int) string {
	return padRight(truncateRunes(textutil.SanitizeTerminal(value), width), width)
}

func peopleDirectoryHeader(columns peopleDirectoryColumns) string {
	row := "   " + peopleCell("Label", columns.labelWidth)
	if columns.showIdentifiers {
		row += "  " + peopleCell("Identifiers", columns.identifierWidth)
	}
	if columns.showLast {
		row += "  " + peopleCell("Last interaction", columns.lastWidth)
	}
	if columns.showCounts {
		row += "  " + peopleCell("Activity", 8) + "  " + peopleCell("Files", 5)
	}
	return row
}

func peopleDirectoryRow(person query.PersonSummary, selected bool, columns peopleDirectoryColumns) string {
	indicator := "   "
	if selected {
		indicator = "▶  "
	}
	row := indicator + peopleCell(person.DisplayLabel, columns.labelWidth)
	if columns.showIdentifiers {
		row += "  " + peopleCell(compactPeopleIdentifiers(person.Identifiers), columns.identifierWidth)
	}
	if columns.showLast {
		row += "  " + peopleCell(formatPeopleTime(person.LastAt), columns.lastWidth)
	}
	if columns.showCounts {
		row += "  " + peopleCell(fmt.Sprintf("%d", person.ActivityCount), 8)
		row += "  " + peopleCell(fmt.Sprintf("%d", person.FileCount), 5)
	}
	return row
}

func compactPeopleIdentifiers(identifiers []query.PersonIdentifier) string {
	values := make([]string, 0, len(identifiers))
	seen := make(map[string]struct{}, len(identifiers))
	for _, identifier := range identifiers {
		value := strings.TrimSpace(identifier.DisplayValue)
		if value == "" {
			value = strings.TrimSpace(identifier.Value)
		}
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		values = append(values, value)
	}
	if len(values) == 0 {
		return "—"
	}
	return strings.Join(values, ", ")
}

func formatPeopleTime(value time.Time) string {
	if value.IsZero() {
		return "—"
	}
	return value.Format("2006-01-02 15:04")
}

func (m Model) peopleDirectoryView() string {
	columns := peopleColumns(m.width)
	var sb strings.Builder
	sb.WriteString(m.styles.tableHeader.Render(padRight(
		peopleDirectoryHeader(columns), max(m.width, 0),
	)))
	sb.WriteString("\n")
	sb.WriteString(m.styles.separator.Render(strings.Repeat("─", max(m.width, 0))))
	sb.WriteString("\n")

	visible := m.visibleRows()
	completionLines := m.peopleCompletionPanelLines(min(6, max(visible-1, 0)))
	directoryVisible := visible - len(completionLines)
	end := min(m.peopleState.scrollOffset+directoryVisible, len(m.peopleState.rows))
	used := 0
	if len(m.peopleState.rows) == 0 {
		var message string
		style := m.styles.normalRow
		switch {
		case m.peopleState.err != nil:
			message = m.peopleState.err.Error() + " [r retry]"
			style = m.styles.err
		case m.peopleState.directoryLoading:
			message = m.spinnerIndicator() + " Loading people..."
		case m.peopleState.initialized:
			message = "No people found"
		}
		if message != "" {
			sb.WriteString(style.Render(padRight("   "+message, max(m.width, 0))))
			sb.WriteString("\n")
			used = 1
		}
	}
	for i := m.peopleState.scrollOffset; i < end; i++ {
		style := m.styles.normalRow
		if i == m.peopleState.cursor {
			style = m.styles.cursorRow
		}
		row := peopleDirectoryRow(m.peopleState.rows[i], i == m.peopleState.cursor, columns)
		sb.WriteString(style.Render(padRight(row, max(m.width, 0))))
		sb.WriteString("\n")
		used++
	}
	for ; used < directoryVisible; used++ {
		sb.WriteString(m.styles.normalRow.Render(strings.Repeat(" ", max(m.width, 0))))
		sb.WriteString("\n")
	}
	for _, line := range completionLines {
		style := m.styles.normalRow
		if line.selected {
			style = m.styles.cursorRow
		} else if line.isError {
			style = m.styles.err
		}
		sb.WriteString(style.Render(padRight(
			truncateToWidth(line.text, max(m.width, 0)), max(m.width, 0),
		)))
		sb.WriteString("\n")
	}

	info := ""
	if m.peopleState.searchActive {
		info = "Search people: " + m.peopleState.searchInput.View()
	} else if m.peopleState.directoryNotice != "" {
		info = m.peopleState.directoryNotice
	} else if m.peopleState.err != nil && len(m.peopleState.rows) > 0 {
		info = m.peopleState.err.Error() + " [r retry]"
	} else if m.peopleState.searchQuery != "" {
		info = fmt.Sprintf("Search: %q", m.peopleState.searchQuery)
	} else if m.peopleState.loadingMore {
		info = "Loading more people..."
	}
	sb.WriteString(m.renderInfoLine(info, m.peopleState.directoryLoading))
	return sb.String()
}

type peopleCompletionPanelLine struct {
	text     string
	selected bool
	isError  bool
}

func (m Model) peopleCompletionPanelLines(limit int) []peopleCompletionPanelLine {
	if !m.peopleState.searchActive || limit <= 0 {
		return nil
	}
	lines := make([]peopleCompletionPanelLine, 0, limit)
	if len(m.peopleState.completions) > 0 {
		lines = append(lines, peopleCompletionPanelLine{text: " Suggestions"})
		rowCapacity := max(limit-1, 0)
		start := min(
			max(m.peopleState.completionCursor-rowCapacity+1, 0),
			max(len(m.peopleState.completions)-rowCapacity, 0),
		)
		end := min(start+rowCapacity, len(m.peopleState.completions))
		for i := start; i < end; i++ {
			row := m.peopleState.completions[i]
			indicator := "   "
			if i == m.peopleState.completionCursor {
				indicator = "▶  "
			}
			lines = append(lines, peopleCompletionPanelLine{
				selected: i == m.peopleState.completionCursor,
				text: fmt.Sprintf("%s%s · %s · %s · %s", indicator,
					textutil.SanitizeTerminal(row.DisplayLabel), row.Kind,
					textutil.SanitizeTerminal(row.Value), textutil.SanitizeTerminal(row.Source)),
			})
		}
	}
	if len(lines) < limit && m.peopleState.completionErr != nil {
		lines = append(lines, peopleCompletionPanelLine{
			text: "   " + textutil.SanitizeTerminal(m.peopleState.completionErr.Error()), isError: true,
		})
	} else if len(lines) == 0 && m.peopleState.completionLoading {
		lines = append(lines, peopleCompletionPanelLine{text: "   Completing people search..."})
	}
	return lines
}

func (m Model) peopleContactView() string {
	if m.peopleState.level == peopleLevelMeetingDetail &&
		(m.peopleState.meetingsErr == nil || m.meetingState.detail != nil) {
		return m.meetingDetailView()
	}
	if m.peopleState.level == peopleLevelActivityMessage &&
		(m.peopleState.activityErr == nil || m.messageDetail != nil) {
		return m.messageDetailView()
	}
	if m.peopleState.level == peopleLevelMessage &&
		(m.peopleState.inboxErr == nil || m.messageDetail != nil) {
		return m.messageDetailView()
	}
	if m.peopleState.level == peopleLevelConversation &&
		(m.peopleState.inboxErr == nil || len(m.textState.messages) > 0) {
		return m.textTimelineView()
	}
	var sb strings.Builder
	tabs := m.peopleTabBar()
	sb.WriteString(m.styles.tableHeader.Render(padRight(tabs, max(m.width, 0))))
	sb.WriteString("\n")
	sb.WriteString(m.styles.separator.Render(strings.Repeat("─", max(m.width, 0))))
	sb.WriteString("\n")

	lines := m.peopleContactTabLines()
	visible := m.visibleRows()
	for i := 0; i < visible; i++ {
		line := ""
		if i < len(lines) {
			line = lines[i]
		}
		sb.WriteString(m.styles.normalRow.Render(padRight(
			truncateToWidth(line, max(m.width, 0)), max(m.width, 0),
		)))
		sb.WriteString("\n")
	}
	info, loading := m.peopleContactStatus()
	sb.WriteString(m.renderInfoLine(info, loading))
	return sb.String()
}

func (m Model) peopleContactStatus() (string, bool) {
	if m.peopleState.err != nil {
		return m.peopleState.err.Error() + " [r retry]", false
	}
	if m.peopleState.contactLoading {
		return "Loading contact...", true
	}
	if m.peopleState.promoting {
		return m.peopleState.attributesNotice, true
	}
	switch m.peopleState.tab {
	case peopleTabOverview:
		if m.peopleState.attributesLoadErr != nil &&
			m.peopleState.attributesLoadErrTab == peopleTabOverview {
			return m.peopleState.attributesLoadErr.Error() + " [r retry]", false
		}
		if m.peopleState.attributesNotice != "" {
			return m.peopleState.attributesNotice, m.peopleState.attributesLoading
		}
		if m.peopleState.attributesLoading {
			return "Loading notes...", true
		}
	case peopleTabAttributes:
		if m.peopleState.attributesLoadErr != nil &&
			m.peopleState.attributesLoadErrTab == peopleTabAttributes {
			return m.peopleState.attributesLoadErr.Error() + " [r retry]", false
		}
		if m.peopleState.attributesNotice != "" {
			return m.peopleState.attributesNotice, m.peopleState.attributesLoading
		}
		if m.peopleState.attributesLoading {
			return "Loading attributes...", true
		}
	case peopleTabInboxes:
		if m.peopleState.inboxErr != nil {
			return m.peopleState.inboxErr.Error() + " [r retry]", false
		}
		switch {
		case m.peopleState.inboxesLoading:
			return "Loading contact inboxes...", true
		case m.peopleState.conversationsLoading:
			return "Loading source conversations...", true
		case m.peopleState.conversationLoading:
			return "Loading conversation messages...", true
		case m.peopleState.messageLoading:
			return "Loading message detail...", true
		}
	case peopleTabMeetings:
		if m.peopleState.meetingsErr != nil {
			return m.peopleState.meetingsErr.Error() + " [r retry]", false
		}
		if m.peopleState.meetingsLoading || m.meetingState.detailLoading {
			return "Loading contact meetings...", true
		}
	case peopleTabFiles:
		if m.peopleState.filesErr != nil {
			return m.peopleState.filesErr.Error() + " [r retry]", false
		}
		if m.peopleState.filesLoading || m.peopleState.fileOpening {
			return "Loading received files...", true
		}
	case peopleTabActivity:
		if m.peopleState.activityErr != nil {
			return m.peopleState.activityErr.Error() + " [r retry]", false
		}
		if m.peopleState.activityLoading || m.peopleState.messageLoading {
			return "Loading contact activity...", true
		}
	}
	return "", false
}

func (m Model) peopleTabBar() string {
	active := int(m.peopleState.tab)
	if active < 0 || active >= len(peopleTabLabels) {
		active = 0
	}
	if m.width < 50 {
		return " [" + peopleTabLabels[active] + "]"
	}
	labels := make([]string, len(peopleTabLabels))
	for i, label := range peopleTabLabels {
		if i == active {
			labels[i] = "[" + label + "]"
		} else {
			labels[i] = label
		}
	}
	return truncateToWidth(" "+strings.Join(labels, "  "), max(m.width, 0))
}

func (m Model) peopleContactTabLines() []string {
	contact := m.peopleState.contact
	if contact == nil {
		if m.peopleState.contactLoading {
			return []string{m.spinnerIndicator() + " Loading contact..."}
		}
		return []string{"Contact unavailable"}
	}
	if m.peopleState.tab == peopleTabAttributes {
		return m.peopleAttributesLines()
	}
	if m.peopleState.tab == peopleTabInboxes {
		return m.peopleInboxLines()
	}
	if m.peopleState.tab == peopleTabMeetings {
		return m.peopleMeetingLines()
	}
	if m.peopleState.tab == peopleTabFiles {
		return m.peopleFileLines()
	}
	if m.peopleState.tab == peopleTabActivity {
		return m.peopleActivityLines()
	}
	if m.peopleState.tab != peopleTabOverview {
		return []string{fmt.Sprintf("%s will load when selected.", peopleTabLabels[m.peopleState.tab])}
	}
	profile := "Observed only; no durable profile"
	if contact.Profile != nil {
		profile = "Curated profile"
	}
	sources := make([]string, 0, len(contact.SourceCounts))
	for _, source := range contact.SourceCounts {
		if source.SourceType != "" {
			sources = append(sources, textutil.SanitizeTerminal(source.SourceType))
		}
	}
	if len(sources) == 0 {
		sources = append(sources, "—")
	}
	observedName := contact.DisplayName
	if observedName == "" {
		observedName = "—"
	}
	lines := []string{
		" Contact overview",
		"",
		" Display label: " + textutil.SanitizeTerminal(contact.DisplayLabel),
		" Observed name: " + textutil.SanitizeTerminal(observedName),
		" Identifiers: " + textutil.SanitizeTerminal(compactPeopleIdentifiers(contact.Identifiers)),
		" Sources: " + strings.Join(sources, ", "),
		" First interaction: " + formatPeopleTime(contact.FirstAt),
		" Latest interaction: " + formatPeopleTime(contact.LastAt),
		fmt.Sprintf(" Activity: %d", contact.ActivityCount),
		fmt.Sprintf(" Meetings: %d", contact.MeetingCount),
		fmt.Sprintf(" Files: %d", contact.FileCount),
		" Profile: " + profile,
	}
	return append(lines, m.peopleOverviewNotesLines()...)
}

func (m Model) peopleOverviewNotesLines() []string {
	contact := m.peopleState.contact
	if contact == nil || contact.Profile == nil {
		return []string{"", " Notes", " Promote with p to add notes"}
	}
	if m.peopleState.attributesLoading && !m.peopleState.attributesLoaded {
		return []string{"", " Notes", " " + m.spinnerIndicator() + " Loading notes..."}
	}
	if !m.peopleState.attributesLoaded || m.peopleState.attributes == nil {
		return []string{"", " Notes", " Notes have not loaded. Press r to retry."}
	}
	if m.peopleState.attributesLoadErr != nil &&
		m.peopleState.attributesLoadErrTab == peopleTabOverview {
		return []string{"", " Notes", " Notes are unavailable. Press r to retry."}
	}
	_, current, ok := m.peopleNotesAttribute()
	if !ok {
		return []string{"", " Notes", " Notes are unavailable. Press r to retry."}
	}
	if current == nil {
		return []string{"", " Notes", " No notes yet"}
	}
	value := textutil.SanitizeTerminalMultiline(peopleAttributeValueString(current.Value))
	noteLines := strings.Split(value, "\n")
	lines := []string{"", " Notes"}
	for _, line := range noteLines {
		lines = append(lines, " "+line)
	}
	return lines
}

func (m Model) peopleMeetingLines() []string {
	if len(m.peopleState.meetings) == 0 && !m.peopleState.meetingsLoading {
		return []string{" No meetings found for this contact"}
	}
	lines := []string{" Meetings", ""}
	start, end := m.peopleContentWindow(len(m.peopleState.meetings))
	for i := start; i < end; i++ {
		meeting := m.peopleState.meetings[i]
		indicator := "   "
		if i == m.peopleState.cursor {
			indicator = " ▶ "
		}
		lines = append(lines, fmt.Sprintf(
			"%s%s  %s  %s", indicator, meeting.SentAt.Format("2006-01-02 15:04"),
			textutil.SanitizeTerminal(meeting.Subject), m.meetingSourceLabel(meeting.SourceID),
		))
	}
	return lines
}

func (m Model) peopleFileLines() []string {
	if len(m.peopleState.files) == 0 && !m.peopleState.filesLoading {
		return []string{" No received files found for this contact"}
	}
	lines := []string{" Received files", ""}
	start, end := m.peopleContentWindow(len(m.peopleState.files))
	for i := start; i < end; i++ {
		file := m.peopleState.files[i]
		indicator := "   "
		if i == m.peopleState.cursor {
			indicator = " ▶ "
		}
		lines = append(lines, fmt.Sprintf(
			"%s%s  %s  %s  %s  %s", indicator,
			file.OccurredAt.Format("2006-01-02 15:04"),
			textutil.SanitizeTerminal(file.Filename),
			textutil.SanitizeTerminal(file.MimeType), formatBytes(file.Size),
			peopleContentSource(file.SourceType, file.SourceIdentifier),
		))
	}
	return lines
}

func peopleContentSource(sourceType, identifier string) string {
	format := func(value string) string {
		value = strings.TrimSpace(value)
		if value == "" {
			return ""
		}
		return strings.ToUpper(value[:1]) + value[1:]
	}
	sourceType = format(sourceType)
	identifier = displayPeopleInboxIdentifier(identifier)
	switch {
	case sourceType == "":
		return identifier
	case identifier == "Unknown source":
		return sourceType
	default:
		return sourceType + "/" + identifier
	}
}

func (m Model) peopleActivityLines() []string {
	if len(m.peopleState.activity) == 0 && !m.peopleState.activityLoading {
		return []string{" No activity found for this contact"}
	}
	location := m.peopleState.location
	if location == nil {
		location = time.Local
	}
	start, end := m.peopleActivityWindow(len(m.peopleState.activity))
	lines := []string{" Activity", ""}
	lastDay := ""
	for i := start; i < end; i++ {
		entry := m.peopleState.activity[i]
		localTime := entry.OccurredAt.In(location)
		day := localTime.Format("2006-01-02")
		if day != lastDay {
			lines = append(lines, " "+day)
			lastDay = day
		}
		title := strings.TrimSpace(entry.Title)
		if title == "" {
			title = strings.TrimSpace(entry.Preview)
		}
		if title == "" {
			title = "(untitled activity)"
		}
		marker := "  "
		if i == m.peopleState.cursor {
			marker = "▶ "
		}
		if m.width < 60 {
			lines = append(lines, fmt.Sprintf(
				" %s● %s  %s", marker, localTime.Format("15:04"),
				textutil.SanitizeTerminal(title),
			))
		} else {
			lines = append(lines, fmt.Sprintf(
				" %s● %s  %s  %s  %s  %s", marker, localTime.Format("15:04"),
				peopleActivitySource(entry), peopleActivityDirection(entry),
				peopleActivityKind(entry), textutil.SanitizeTerminal(title),
			))
		}
		if i+1 < end && m.peopleState.activity[i+1].OccurredAt.In(location).Format("2006-01-02") == day {
			lines = append(lines, "   │")
		}
	}
	return lines
}

func (m Model) peopleActivityDataRows() int {
	return max((m.visibleRows()-2)/2, 1)
}

func (m Model) peopleActivityWindow(count int) (int, int) {
	start := min(max(m.peopleState.scrollOffset, 0), max(count-1, 0))
	return start, min(start+m.peopleActivityDataRows(), count)
}

func peopleActivitySource(entry query.EntryRow) string {
	if entry.Kind == query.EntryMeeting || entry.MessageType == meetingMessageType {
		return "Meeting"
	}
	if entry.MessageType == "email" || strings.Contains(strings.ToLower(entry.SourceType), "mail") {
		return "Email"
	}
	if strings.TrimSpace(entry.SourceIdentifier) != "" {
		return displayPeopleInboxIdentifier(entry.SourceIdentifier)
	}
	return peopleContentSource(entry.SourceType, "")
}

func peopleActivityDirection(entry query.EntryRow) string {
	if len(entry.MatchedSenderIdentities) > 0 {
		return "received"
	}
	if len(entry.MatchedRecipientIdentities) > 0 {
		return "sent"
	}
	return "—"
}

func peopleActivityKind(entry query.EntryRow) string {
	if entry.Kind == query.EntryMeeting || entry.MessageType == meetingMessageType {
		return "meeting"
	}
	if entry.MessageType != "" {
		return textutil.SanitizeTerminal(entry.MessageType)
	}
	return textutil.SanitizeTerminal(string(entry.Kind))
}

func (m Model) peopleContentWindow(count int) (int, int) {
	start := min(max(m.peopleState.scrollOffset, 0), max(count-1, 0))
	return start, min(start+max(m.visibleRows()-2, 1), count)
}

func (m Model) peopleInboxLines() []string {
	switch m.peopleState.level {
	case peopleLevelInboxTypes:
		if len(m.peopleState.inboxTypes) == 0 && !m.peopleState.inboxesLoading {
			return []string{" No inboxes found for this contact"}
		}
		lines := []string{" Messaging source types", ""}
		start, end := m.peopleInboxWindow(len(m.peopleState.inboxTypes))
		for i := start; i < end; i++ {
			group := m.peopleState.inboxTypes[i]
			indicator := "   "
			if i == m.peopleState.cursor {
				indicator = " ▶ "
			}
			lines = append(lines, fmt.Sprintf(
				"%s%s  %d source%s", indicator, group.label, len(group.sources),
				pluralSuffix(len(group.sources)),
			))
		}
		return lines
	case peopleLevelInboxSources:
		group, ok := m.selectedPeopleInboxType()
		if !ok {
			return []string{" Source type unavailable"}
		}
		lines := []string{" " + group.label + " sources", ""}
		start, end := m.peopleInboxWindow(len(group.sources))
		for i := start; i < end; i++ {
			source := group.sources[i]
			indicator := "   "
			if i == m.peopleState.cursor {
				indicator = " ▶ "
			}
			latestReceived := "—"
			if source.LatestReceivedAt != nil {
				latestReceived = formatPeopleTime(*source.LatestReceivedAt)
			}
			lines = append(lines, fmt.Sprintf(
				"%s%s  latest received %s  sent %d  received %d  conversations %d",
				indicator, displayPeopleInboxIdentifier(source.SourceIdentifier), latestReceived,
				source.SentCount, source.ReceivedCount, source.ConversationCount,
			))
		}
		return lines
	case peopleLevelConversations:
		if len(m.peopleState.conversations) == 0 && !m.peopleState.conversationsLoading {
			return []string{" No conversations found for this contact on this source"}
		}
		lines := []string{" Conversations", ""}
		start, end := m.peopleInboxWindow(len(m.peopleState.conversations))
		for i := start; i < end; i++ {
			conversation := m.peopleState.conversations[i]
			indicator := "   "
			if i == m.peopleState.cursor {
				indicator = " ▶ "
			}
			title := conversation.Title
			if strings.TrimSpace(title) == "" {
				title = fmt.Sprintf("Conversation %d", conversation.ConversationID)
			}
			lines = append(lines, fmt.Sprintf(
				"%s%s  %d messages  %s", indicator,
				textutil.SanitizeTerminal(title), conversation.MessageCount,
				formatPeopleTime(conversation.LastMessageAt),
			))
		}
		return lines
	case peopleLevelConversation:
		if m.peopleState.inboxErr != nil {
			return []string{" Conversation messages unavailable"}
		}
		return []string{" No messages found in this conversation"}
	case peopleLevelMessage:
		return []string{" Message detail unavailable"}
	}
	return []string{" Inboxes unavailable"}
}

func (m Model) peopleInboxDataRows() int {
	return max(m.visibleRows()-2, 1)
}

func (m Model) peopleInboxWindow(count int) (int, int) {
	start := min(max(m.peopleState.scrollOffset, 0), max(count-1, 0))
	return start, min(start+m.peopleInboxDataRows(), count)
}

func displayPeopleInboxIdentifier(identifier string) string {
	identifier = strings.TrimSpace(identifier)
	if identifier == "" {
		return "Unknown source"
	}
	switch strings.ToLower(identifier) {
	case "whatsapp":
		return "WhatsApp"
	case "imessage":
		return "iMessage"
	case "sms":
		return "SMS"
	case "rcs":
		return "RCS"
	}
	words := strings.FieldsFunc(identifier, func(r rune) bool {
		return r == '_' || r == '-'
	})
	for i, word := range words {
		if word == "" {
			continue
		}
		runes := []rune(word)
		words[i] = strings.ToUpper(string(runes[0])) + string(runes[1:])
	}
	return textutil.SanitizeTerminal(strings.Join(words, " "))
}

func pluralSuffix(count int) string {
	if count == 1 {
		return ""
	}
	return "s"
}

func (m Model) peopleFooterView() string {
	keys := []string{"↑/k", "↓/j", "Enter", "/ search", "m mode", "? help"}
	if m.peopleState.level == peopleLevelDirectory && m.peopleState.searchActive {
		keys = []string{"↑/↓ suggest", "Tab accept", "Enter open", "Esc cancel", "? help"}
	}
	if m.peopleState.level != peopleLevelDirectory {
		keys = []string{"Tab/Shift-Tab", "p promote", "Esc back", "m mode", "? help"}
		if m.peopleState.tab == peopleTabOverview {
			keys = []string{"n notes", "p promote", "Tab/Shift-Tab", "Esc back", "m mode", "? help"}
		}
		if m.peopleState.tab == peopleTabAttributes {
			keys = []string{"↑/↓", "Enter add", "e edit", "n field", "p promote", "Tab", "Esc"}
		}
		if m.peopleState.level == peopleLevelContact &&
			(m.peopleState.tab == peopleTabMeetings || m.peopleState.tab == peopleTabFiles ||
				m.peopleState.tab == peopleTabActivity) {
			keys = []string{
				"↑/↓", "PgUp/PgDn", "Enter",
			}
			if m.peopleContentTabRetryAvailable() {
				keys = append(keys, "r retry")
			}
			keys = append(keys, "Tab/Shift-Tab", "Esc back", "m mode", "? help")
		}
		if m.peopleState.level >= peopleLevelInboxTypes {
			keys = []string{"↑/↓", "Enter", "Esc back", "r retry", "m mode", "? help"}
		}
		if m.peopleState.level == peopleLevelMessage {
			keys = []string{"↑/↓ scroll", "/ find", "Esc back", "r retry", "m mode", "? help"}
		}
		if m.peopleState.level == peopleLevelMeetingDetail {
			keys = []string{"↑/↓ scroll", "/ find", "r retry", "Esc back", "m mode", "? help"}
		}
		if m.peopleState.level == peopleLevelActivityMessage {
			keys = []string{"↑/↓ scroll", "/ find", "r retry", "Esc back", "m mode", "? help"}
		}
	}
	if m.width < 60 {
		if m.peopleState.level == peopleLevelDirectory {
			keys = []string{"↑/↓", "Enter", "/", "m", "?"}
			if m.peopleState.searchActive {
				keys = []string{"↑/↓", "Tab", "Enter", "Esc", "?"}
			}
		} else {
			keys = []string{"Tab", "p", "Esc", "m", "?"}
			if m.peopleState.tab == peopleTabOverview {
				keys = []string{"n", "p", "Tab", "Esc", "m", "?"}
			}
			if m.peopleState.tab == peopleTabAttributes {
				keys = []string{"↑/↓", "Enter", "e", "n", "p", "Tab", "Esc"}
			}
			if m.peopleState.level == peopleLevelContact &&
				(m.peopleState.tab == peopleTabMeetings || m.peopleState.tab == peopleTabFiles ||
					m.peopleState.tab == peopleTabActivity) {
				keys = []string{"↑/↓", "Pg", "Enter"}
				if m.peopleContentTabRetryAvailable() {
					keys = append(keys, "r")
				}
				keys = append(keys, "Tab", "Esc")
			}
			if m.peopleState.level >= peopleLevelInboxTypes {
				keys = []string{"↑/↓", "Enter", "Esc", "r", "m", "?"}
			}
			if m.peopleState.level == peopleLevelMessage {
				keys = []string{"↑/↓", "/", "Esc", "r", "m", "?"}
			}
			if m.peopleState.level == peopleLevelMeetingDetail {
				keys = []string{"↑/↓", "/", "r", "Esc", "m", "?"}
			}
			if m.peopleState.level == peopleLevelActivityMessage {
				keys = []string{"↑/↓", "/", "r", "Esc", "m", "?"}
			}
		}
	}
	footer := " " + strings.Join(keys, " │ ")
	contentWidth := max(m.width-2, 1)
	if m.peopleState.level == peopleLevelDirectory && len(m.peopleState.rows) > 0 {
		position := fmt.Sprintf(" %d/%d ", m.peopleState.cursor+1, len(m.peopleState.rows))
		gap := contentWidth - lipgloss.Width(footer) - lipgloss.Width(position)
		if gap >= 1 {
			footer += strings.Repeat(" ", gap) + position
		}
	}
	return m.styles.footer.Render(padRight(truncateToWidth(footer, contentWidth), contentWidth))
}

func (m Model) peopleContentTabRetryAvailable() bool {
	switch m.peopleState.tab {
	case peopleTabMeetings:
		return m.peopleState.meetingsErr != nil
	case peopleTabFiles:
		return m.peopleState.filesErr != nil
	case peopleTabActivity:
		return m.peopleState.activityErr != nil
	default:
		return false
	}
}
