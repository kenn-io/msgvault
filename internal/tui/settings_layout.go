package tui

import (
	"strings"

	"charm.land/bubbles/v2/viewport"
	"github.com/charmbracelet/x/ansi"
)

const settingsScrollBottom = -1

// settingsViewport keeps the actions outside the scrollable settings content.
func (m Model) settingsViewport(body, actions string) (viewport.Model, string) {
	width := max(m.width, 20)
	footer := wrapText(actions, width)
	v := viewport.New(viewport.WithWidth(width), viewport.WithHeight(max(m.height-len(footer)-1, 1)))
	v.SoftWrap = true
	wrapped := ansi.Wrap(body, width, "")
	v.SetContent(wrapped)
	if v.TotalLineCount() > v.Height() {
		footer = append(footer, "[PgUp/PgDn] Scroll")
		v.SetHeight(max(m.height-len(footer)-1, 1))
	}
	v.SetYOffset(m.settingsYOffset(v, wrapped))
	return v, strings.Join(footer, "\n")
}

// settingsYOffset resolves the stored scroll request against the wrapped body.
// A negative request keeps the selected row visible; results and notices that
// follow an operation stay at the top, and paging reaches the selection.
func (m Model) settingsYOffset(v viewport.Model, wrapped string) int {
	request := m.settings.scrollOffset
	if request >= 0 {
		return request
	}
	bottom, cursor := max(v.TotalLineCount()-v.Height(), 0), settingsCursorLine(wrapped)
	if cursor < 0 {
		return bottom
	}
	return min(bottom, cursor)
}

// settingsCursorLine finds the selected row: the marker furthest right, which
// is the field marker when categories are shown beside the fields.
func settingsCursorLine(wrapped string) int {
	line, best := -1, -1
	for i, text := range strings.Split(ansi.Strip(wrapped), "\n") {
		if at := strings.LastIndex(text, "▶ "); at > best {
			line, best = i, at
		}
	}
	return line
}

// settingsListHeight keeps the notices, the selected row, and the footer on one
// screen: the list gets the lines left once they and the scroll hint are placed.
func (m Model) settingsListHeight(notices []string) int {
	width := max(m.width, 20)
	used := 2 + len(wrapText(m.settingsFooter(), width))
	for _, notice := range notices {
		used += strings.Count(ansi.Wrap(notice, width, ""), "\n") + 1
	}
	return min(max(m.height-4-len(notices), 4), max(m.height-used, 2))
}

func (m Model) settingsScreen() (string, string, string) {
	if m.settings.peopleControls.active {
		return m.peopleControlsParts()
	}
	if m.settings.codex.active {
		return m.codexSettingsParts()
	}
	return m.mainSettingsParts()
}

func (m Model) settingsScreenView() string {
	title, body, actions := m.settingsScreen()
	v, footer := m.settingsViewport(body, actions)
	return strings.Join([]string{title, v.View(), footer}, "\n")
}
