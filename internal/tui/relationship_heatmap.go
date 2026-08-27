package tui

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	"go.kenn.io/msgvault/internal/identityindex"
	"go.kenn.io/msgvault/internal/query"
)

const relationshipHeatCellWidth = 2

type relationshipHeatPanel struct {
	start time.Time
	end   time.Time
}

func relationshipHeatCell(
	styles tuiStyles, level identityindex.HeatLevel, future, noColor bool,
) string {
	if future {
		return strings.Repeat(" ", relationshipHeatCellWidth)
	}
	index := relationshipHeatLevelIndex(level)
	if noColor {
		glyphs := [...]string{"· ", "░ ", "▒ ", "▓ ", "█ "}
		return glyphs[index]
	}
	return styles.relationshipHeat[index].Render("■ ")
}

func relationshipHeatLevelIndex(level identityindex.HeatLevel) int {
	switch level {
	case identityindex.HeatFirstQuartile:
		return 1
	case identityindex.HeatSecondQuartile:
		return 2
	case identityindex.HeatThirdQuartile:
		return 3
	case identityindex.HeatFourthQuartile:
		return 4
	default:
		return 0
	}
}

func relationshipHeatmapLines(
	calendar *query.RelationshipCalendarResponse,
	width int,
	styles tuiStyles,
	noColor bool,
) []string {
	if calendar == nil {
		return nil
	}
	width = max(width, 1)
	yearStart := time.Date(calendar.Year, time.January, 1, 0, 0, 0, 0, time.UTC)
	yearEnd := time.Date(calendar.Year, time.December, 31, 0, 0, 0, 0, time.UTC)
	fullPanel := relationshipHeatPanel{start: yearStart, end: yearEnd}
	panels := []relationshipHeatPanel{fullPanel}
	if relationshipHeatPanelWidth(fullPanel) > width {
		panels = []relationshipHeatPanel{
			{start: yearStart, end: time.Date(calendar.Year, time.June, 30, 0, 0, 0, 0, time.UTC)},
			{start: time.Date(calendar.Year, time.July, 1, 0, 0, 0, 0, time.UTC), end: yearEnd},
		}
	}

	lines := []string{relationshipAlignedLine("● Relationship", strconv.Itoa(calendar.Year), width)}
	for _, panel := range panels {
		lines = append(lines, relationshipHeatPanelLines(calendar, panel, styles, noColor)...)
	}
	if relationshipCalendarHasActivity(calendar) {
		lines = append(lines, "")
	} else {
		lines = append(lines, fmt.Sprintf("No interactions in %d.", calendar.Year))
	}
	legend := "Less "
	levels := [...]identityindex.HeatLevel{
		identityindex.HeatNone,
		identityindex.HeatFirstQuartile,
		identityindex.HeatSecondQuartile,
		identityindex.HeatThirdQuartile,
		identityindex.HeatFourthQuartile,
	}
	var legendBuilder strings.Builder
	for index, level := range levels {
		legendBuilder.WriteString(relationshipHeatCell(styles, level, false, noColor))
		if index < len(levels)-1 {
			legendBuilder.WriteString(" ")
		}
	}
	legend += legendBuilder.String()
	lines = append(lines, legend+"More", relationshipAlignedLine(
		fmt.Sprintf("Current %d/100", calendar.Current.Temperature),
		fmt.Sprintf("Peak %d/100 - %d", calendar.PeakTemperature, calendar.PeakYear),
		width,
	))
	previous := "[ previous year"
	next := "next year ]"
	lines = append(lines, relationshipAlignedLine(previous, next, width))
	return lines
}

func relationshipHeatPanelWidth(panel relationshipHeatPanel) int {
	start := previousSunday(panel.start)
	end := nextSaturday(panel.end)
	weeks := int(end.Sub(start).Hours()/24)/7 + 1
	return 4 + weeks*relationshipHeatCellWidth
}

func relationshipHeatPanelLines(
	calendar *query.RelationshipCalendarResponse,
	panel relationshipHeatPanel,
	styles tuiStyles,
	noColor bool,
) []string {
	gridStart := previousSunday(panel.start)
	gridEnd := nextSaturday(panel.end)
	weeks := int(gridEnd.Sub(gridStart).Hours()/24)/7 + 1
	lineWidth := 4 + weeks*relationshipHeatCellWidth
	monthLine := []rune(strings.Repeat(" ", lineWidth))
	for month := panel.start.Month(); month <= panel.end.Month(); month++ {
		first := time.Date(calendar.Year, month, 1, 0, 0, 0, 0, time.UTC)
		if first.Before(panel.start) {
			first = panel.start
		}
		week := int(first.Sub(gridStart).Hours()/24) / 7
		writeRelationshipRunes(monthLine, 4+week*relationshipHeatCellWidth, month.String()[:3])
	}

	days := make(map[string]query.RelationshipCalendarDay, len(calendar.Days))
	var elapsedEnd time.Time
	for _, day := range calendar.Days {
		days[day.Date] = day
		parsed, err := time.Parse(time.DateOnly, day.Date)
		if err == nil && parsed.After(elapsedEnd) {
			elapsedEnd = parsed
		}
	}
	lines := []string{strings.TrimRight(string(monthLine), " ")}
	labels := [...]string{"Sun", "", "Tue", "", "Thu", "", "Sat"}
	for weekday := range 7 {
		var row strings.Builder
		_, _ = fmt.Fprintf(&row, "%-4s", labels[weekday])
		for week := range weeks {
			date := gridStart.AddDate(0, 0, week*7+weekday)
			outside := date.Before(panel.start) || date.After(panel.end)
			future := outside || elapsedEnd.IsZero() || date.After(elapsedEnd)
			day := days[date.Format(time.DateOnly)]
			row.WriteString(relationshipHeatCell(styles, day.Level, future, noColor))
		}
		lines = append(lines, strings.TrimRight(row.String(), " "))
	}
	return lines
}

func relationshipAlignedLine(left, right string, width int) string {
	leftWidth := lipgloss.Width(left)
	rightWidth := lipgloss.Width(right)
	gap := width - leftWidth - rightWidth
	if gap < 1 {
		if right == "" {
			return truncateToWidth(left, width)
		}
		return truncateToWidth(left+" "+right, width)
	}
	return left + strings.Repeat(" ", gap) + right
}

func previousSunday(date time.Time) time.Time {
	return date.AddDate(0, 0, -int(date.Weekday()))
}

func nextSaturday(date time.Time) time.Time {
	return date.AddDate(0, 0, 6-int(date.Weekday()))
}

func writeRelationshipRunes(target []rune, offset int, value string) {
	for _, character := range value {
		if offset >= len(target) {
			return
		}
		target[offset] = character
		offset++
	}
}

func relationshipCalendarHasActivity(calendar *query.RelationshipCalendarResponse) bool {
	for _, day := range calendar.Days {
		if day.Total > 0 {
			return true
		}
	}
	return false
}
