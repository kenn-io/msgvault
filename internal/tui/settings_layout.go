package tui

import (
	"strings"

	"charm.land/bubbles/v2/viewport"
	"github.com/charmbracelet/x/ansi"
)

// settingsViewport reserves space for the body and reachable actions.
func (m Model) settingsViewport(title, body, actions string) (viewport.Model, string) {
	width, titleRows, wrapped, footer := m.settingsViewportContent(title, body, actions)
	v := viewport.New(viewport.WithWidth(width), viewport.WithHeight(max(m.height-len(footer)-titleRows, 1)))
	v.SetContent(wrapped)
	if v.TotalLineCount() > v.Height() {
		footer = append(footer, "[PgUp/PgDn] Scroll")
		v.SetHeight(max(m.height-len(footer)-titleRows, 1))
	}
	v.SetYOffset(m.settings.scrollOffset)
	return v, strings.Join(footer, "\n")
}

func (m Model) settingsViewportContent(title, body, actions string) (int, int, string, []string) {
	width := max(m.width, 20)
	footer := wrapActions(actions, width)
	titleRows := strings.Count(ansi.Wrap(title, width, ""), "\n") + 1
	wrapped := ansi.Wrap(body, width, "")
	if !m.settings.confirmDiscard && len(footer) > m.height-titleRows-2 {
		bodyActions := strings.TrimSuffix(actions, "  [Esc] Back")
		if i := strings.LastIndex(bodyActions, "  [Esc] Cancel"); i >= 0 {
			bodyActions = bodyActions[:i]
		}
		wrapped = ansi.Wrap(body+"\n\n"+strings.ReplaceAll(bodyActions, "  [", "\n["), width, "")
		footer = nil
		if strings.Contains(actions, "Esc] Cancel") {
			footer = wrapActions("[Esc] Cancel", width)
		} else if strings.Contains(actions, "Esc]") {
			footer = wrapActions("[Esc] Back", width)
		}
	}
	return width, titleRows, wrapped, footer
}

// wrapActions keeps each key beside the first word of its label when wrapping.
func wrapActions(actions string, width int) []string {
	lines := wrapText(strings.ReplaceAll(actions, "] ", "]\u00a0"), width)
	for i := range lines {
		lines[i] = strings.ReplaceAll(lines[i], "\u00a0", " ")
	}
	return lines
}

func settingsWrappedRows(lines []string, width int) int {
	if len(lines) == 0 {
		return 0
	}
	return strings.Count(ansi.Wrap(strings.Join(lines, "\n"), width, ""), "\n") + 1
}

func (m *Model) revealSettingsFocus() {
	title, body, actions, focus := m.settingsScreen()
	v, _ := m.settingsViewport(title, body, actions)
	v.SetYOffset(focus)
	m.settings.scrollOffset = v.YOffset()
}

func (m Model) settingsFocusVisible() bool {
	if !m.settings.active || m.settings.scrollOffset == 0 || m.settings.confirmDiscard || m.settings.peopleControls.active || m.settings.codex.active {
		return false
	}
	title, body, actions, focus := m.settingsScreen()
	v, _ := m.settingsViewport(title, body, actions)
	return focus >= v.YOffset() && focus < v.YOffset()+v.Height()
}

func (m Model) settingsScreen() (string, string, string, int) {
	if m.settings.confirmDiscard {
		return m.styles.titleBar.Render("Settings *"), m.styles.modalTitle.Render("Discard unsaved changes?") + "\n\nYour settings drafts have not been saved.", "[Y] Yes [N/Esc] No", 0
	}
	if m.settings.peopleControls.active {
		return m.peopleControlsParts()
	}
	if m.settings.codex.active {
		return m.codexSettingsParts()
	}
	return m.mainSettingsParts()
}
