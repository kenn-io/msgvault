package tui

import (
	"strings"

	"charm.land/bubbles/v2/viewport"
	"github.com/charmbracelet/x/ansi"
)

// settingsViewport keeps the actions outside the scrollable settings content.
func (m Model) settingsViewport(body, actions string) (viewport.Model, string) {
	width := max(m.width, 20)
	if m.settingsIsNarrow() {
		actions = strings.ReplaceAll(actions, "] ", "]\u00a0")
	}
	footer := wrapText(actions, width)
	v := viewport.New(viewport.WithWidth(width), viewport.WithHeight(max(m.height-len(footer)-1, 1)))
	v.SoftWrap = true
	v.SetContent(ansi.Wrap(body, width, ""))
	if v.TotalLineCount() > v.Height() {
		footer = append(footer, "[PgUp/PgDn] Scroll")
		v.SetHeight(max(m.height-len(footer)-1, 1))
	}
	if m.settings.scrollOffset < 0 {
		v.GotoBottom()
	} else {
		v.SetYOffset(m.settings.scrollOffset)
	}
	return v, strings.Join(footer, "\n")
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
