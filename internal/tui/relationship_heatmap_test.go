package tui

import (
	"errors"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/identityindex"
	"go.kenn.io/msgvault/internal/query"
)

func relationshipCalendarFixture() *query.RelationshipCalendarResponse {
	return &query.RelationshipCalendarResponse{
		CanonicalID: 7,
		Year:        2026,
		Timezone:    "UTC",
		Days: []query.RelationshipCalendarDay{
			{Date: "2026-01-01", Level: identityindex.HeatNone},
			{Date: "2026-01-02", Total: 1, Level: identityindex.HeatFirstQuartile},
			{Date: "2026-01-03", Total: 2, Level: identityindex.HeatSecondQuartile},
			{Date: "2026-01-04", Total: 3, Level: identityindex.HeatThirdQuartile},
			{Date: "2026-01-05", Total: 4, Level: identityindex.HeatFourthQuartile},
		},
		Current:          identityindex.TemperatureSummary{Temperature: 62},
		PeakTemperature:  87,
		PeakYear:         2018,
		CacheRevision:    "cache-1",
		IdentityRevision: 4,
	}
}

func TestRelationshipHeatmapCellsHaveFixedWidthAcrossAllLevelsAndFuture(t *testing.T) {
	styles := newStyles(true)
	levels := []identityindex.HeatLevel{
		identityindex.HeatNone,
		identityindex.HeatFirstQuartile,
		identityindex.HeatSecondQuartile,
		identityindex.HeatThirdQuartile,
		identityindex.HeatFourthQuartile,
	}
	for _, noColor := range []bool{false, true} {
		for _, level := range levels {
			cell := relationshipHeatCell(styles, level, false, noColor)
			assert.Equal(t, relationshipHeatCellWidth, lipgloss.Width(cell),
				"level %s noColor=%v", level, noColor)
		}
		assert.Equal(t, relationshipHeatCellWidth,
			lipgloss.Width(relationshipHeatCell(styles, identityindex.HeatNone, true, noColor)))
	}
}

func TestRelationshipHeatmapWideUsesSundayRowsMonthsAndAlignedSummary(t *testing.T) {
	assert := assert.New(t)
	calendar := relationshipCalendarFixture()
	calendar.Days = relationshipElapsedDays(calendar.Year, time.Date(2026, 8, 22, 0, 0, 0, 0, time.UTC))
	calendar.Days[1].Level = identityindex.HeatFirstQuartile
	calendar.Days[2].Level = identityindex.HeatSecondQuartile
	calendar.Days[3].Level = identityindex.HeatThirdQuartile
	calendar.Days[4].Level = identityindex.HeatFourthQuartile

	lines := relationshipHeatmapLines(calendar, 120, newStyles(true), true)
	view := strings.Join(lines, "\n")
	require.NotEmpty(t, lines)
	assert.Contains(lines[0], "● Relationship")
	assert.True(strings.HasSuffix(lines[0], "2026"), "year is unbracketed and right aligned")
	for _, month := range []string{"Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec"} {
		assert.Contains(view, month)
	}
	for _, weekday := range []string{"Sun", "Tue", "Thu", "Sat"} {
		assert.Contains(view, weekday)
	}
	assert.Contains(view, "Less")
	assert.Contains(view, "More")
	summary := findLineContaining(lines, "Current 62/100")
	assert.Contains(summary, "Peak 87/100 - 2018")
	assert.True(strings.HasSuffix(summary, "Peak 87/100 - 2018"))
	navigation := findLineContaining(lines, "[ previous year")
	assert.True(strings.HasPrefix(navigation, "[ previous year"))
	assert.True(strings.HasSuffix(navigation, "next year ]"))
}

func TestRelationshipHeatmapNarrowStacksEqualSizedHalfYearsAndLeavesFutureBlank(t *testing.T) {
	assert := assert.New(t)
	calendar := relationshipCalendarFixture()
	lines := relationshipHeatmapLines(calendar, 70, newStyles(true), true)
	view := strings.Join(lines, "\n")
	assert.Contains(view, "Jan")
	assert.Contains(view, "Jun")
	assert.Contains(view, "Jul")
	assert.Contains(view, "Dec")
	assert.GreaterOrEqual(strings.Count(view, "Sun"), 2, "each half has Sunday-first labels")
	firstHalfDays := strings.Join(lines[2:9], "")
	assert.Equal(5, strings.Count(firstHalfDays, "·")+strings.Count(firstHalfDays, "░")+
		strings.Count(firstHalfDays, "▒")+strings.Count(firstHalfDays, "▓")+
		strings.Count(firstHalfDays, "█"), "future dates remain blank")
	for _, line := range lines {
		assert.LessOrEqual(lipgloss.Width(line), 70)
	}
	assert.NotContains(view, "No interactions in 2026.")

	empty := relationshipCalendarFixture()
	empty.Days = relationshipElapsedDays(2026, time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC))
	assert.Contains(strings.Join(
		relationshipHeatmapLines(empty, 70, newStyles(true), true), "\n",
	), "No interactions in 2026.")
}

func TestRelationshipHeatmapSupportsFiftyThreeAndFiftyFourWeekYears(t *testing.T) {
	assert.Equal(t, 4+53*relationshipHeatCellWidth, relationshipHeatPanelWidth(relationshipHeatPanel{
		start: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		end:   time.Date(2026, 12, 31, 0, 0, 0, 0, time.UTC),
	}))
	assert.Equal(t, 4+54*relationshipHeatCellWidth, relationshipHeatPanelWidth(relationshipHeatPanel{
		start: time.Date(2028, 1, 1, 0, 0, 0, 0, time.UTC),
		end:   time.Date(2028, 12, 31, 0, 0, 0, 0, time.UTC),
	}))
}

func TestPeopleOverviewRelationshipStatesAndPositionBeforeNotes(t *testing.T) {
	assert := assert.New(t)
	contact := testPerson(7, "Heatmap Person")
	contact.Profile = nil
	model := peopleContentModel(&fakePeopleBackend{}, &contact)
	model.width = 120
	model.peopleState.relationshipYear = 2026
	model.peopleState.relationshipFirstYear = 2018

	model.peopleState.relationshipLoading = true
	loading := strings.Join(model.peopleContactTabLines(), "\n")
	assert.Contains(loading, "Relationship")
	assert.Contains(loading, "Loading relationship activity")
	assert.Less(strings.Index(loading, "Relationship"), strings.Index(loading, "Notes"))

	model.peopleState.relationshipLoading = false
	model.peopleState.relationshipErr = errors.New("assert.AnError general error for testing")
	failed := strings.Join(model.peopleContactTabLines(), "\n")
	assert.Contains(failed, "assert.AnError general error for testing")
	assert.Contains(failed, "r retry")

	model.peopleState.relationshipErr = nil
	model.peopleState.relationshipCalendar = relationshipCalendarFixture()
	loaded := strings.Join(model.peopleContactTabLines(), "\n")
	assert.Contains(loaded, "Current 62/100")
	assert.Contains(loaded, "Peak 87/100 - 2018")
}

func TestPeopleOverviewHelpIncludesRelationshipYearNavigation(t *testing.T) {
	contact := testPerson(7, "Help Person")
	model := peopleContentModel(&fakePeopleBackend{}, &contact)
	model.height = 80
	help := stripANSI(model.renderHelpModal())
	footer := stripANSI(model.peopleFooterView())
	assert.Contains(t, help, "Previous relationship year")
	assert.Contains(t, help, "Next relationship year")
	assert.Contains(t, footer, "[ / ] year")
}

func TestPeopleOverviewHeatmapScrollKeepsNotesReachable(t *testing.T) {
	contact := testPerson(7, "Scrollable Person")
	contact.Profile = nil
	model := peopleContentModel(&fakePeopleBackend{}, &contact)
	model.width = 70
	model.height = 24
	model.peopleState.relationshipYear = 2026
	model.peopleState.relationshipFirstYear = 2018
	model.peopleState.relationshipCalendar = relationshipCalendarFixture()

	assert.NotContains(t, stripANSI(model.renderPeopleView()), "Promote with p to add notes")
	model, _ = sendKey(t, model, tea.KeyPressMsg{Code: tea.KeyEnd})
	assert.Positive(t, model.peopleState.scrollOffset)
	assert.Contains(t, stripANSI(model.renderPeopleView()), "Promote with p to add notes")
}

func relationshipElapsedDays(year int, end time.Time) []query.RelationshipCalendarDay {
	days := make([]query.RelationshipCalendarDay, 0, end.YearDay())
	for day := time.Date(year, 1, 1, 0, 0, 0, 0, time.UTC); !day.After(end); day = day.AddDate(0, 0, 1) {
		days = append(days, query.RelationshipCalendarDay{Date: day.Format(time.DateOnly)})
	}
	return days
}

func findLineContaining(lines []string, value string) string {
	for _, line := range lines {
		if strings.Contains(line, value) {
			return line
		}
	}
	return ""
}
