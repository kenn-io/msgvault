package tui

import (
	"fmt"
	"strings"

	"charm.land/lipgloss/v2"
	"go.kenn.io/msgvault/internal/textutil"
)

func (m Model) renderSettingsView() string {
	title := "Settings"
	if m.settings.dirty() {
		title += " *"
	}
	header := m.styles.titleBar.Render(title)
	if m.settings.confirmDiscard {
		return strings.Join([]string{
			header,
			"",
			m.styles.modalTitle.Render("Discard unsaved changes?"),
			"",
			"Your settings drafts have not been saved.",
			"",
			"[Y] Discard and return    [N/Esc] Keep editing",
		}, "\n")
	}

	lines := []string{header}
	if m.settings.pendingRestart {
		lines = append(lines, m.styles.flash.Render("Pending restart — saved changes take effect after the daemon restarts."))
	}
	if m.settings.loading {
		return strings.Join(append(lines, "", m.styles.loading.Render("Loading settings…"), "", "[Esc] Back"), "\n")
	}

	availableHeight := max(m.height-len(lines)-3, 4)
	if m.settingsIsNarrow() {
		if m.settings.narrowFields {
			lines = append(lines, m.renderSettingsFields(max(m.width, 20), availableHeight))
		} else {
			lines = append(lines, m.renderSettingsCategories(max(m.width, 20), availableHeight))
		}
	} else {
		categoryWidth := min(max(m.width/4, 20), 30)
		fieldWidth := max(m.width-categoryWidth-3, 30)
		categories := m.renderSettingsCategories(categoryWidth, availableHeight)
		fields := m.renderSettingsFields(fieldWidth, availableHeight)
		lines = append(lines, lipgloss.JoinHorizontal(lipgloss.Top, categories, " │ ", fields))
	}

	if m.settings.status != "" {
		status := textutil.SanitizeTerminalMultiline(m.settings.status)
		if m.settings.statusIsError {
			status = m.styles.err.Render(status)
		} else {
			status = m.styles.flash.Render(status)
		}
		lines = append(lines, status)
	}
	lines = append(lines, m.settingsFooter())
	return strings.Join(lines, "\n")
}

func (m Model) renderSettingsCategories(width, height int) string {
	var sb strings.Builder
	sb.WriteString(m.styles.tableHeader.Render("Categories"))
	sb.WriteString("\n")
	if len(m.settings.groups) == 0 {
		sb.WriteString("No terminal settings available")
		return sb.String()
	}
	maxRows := max(height-1, 1)
	start := 0
	if m.settings.groupCursor >= maxRows {
		start = m.settings.groupCursor - maxRows + 1
	}
	end := min(start+maxRows, len(m.settings.groups))
	for i := start; i < end; i++ {
		group := m.settings.groups[i]
		cursor := "  "
		if i == m.settings.groupCursor {
			cursor = "▶ "
		}
		label := textutil.SanitizeTerminal(group.Label)
		if label == "" {
			label = settingsGroupLabel(group.ID)
		}
		line := truncateRunes(cursor+label, max(width-1, 1))
		if i == m.settings.groupCursor {
			line = m.styles.cursorRow.Render(padRight(line, max(width-1, 1)))
		}
		sb.WriteString(line)
		if i+1 < end {
			sb.WriteString("\n")
		}
	}
	if m.settingsIsNarrow() {
		sb.WriteString("\n\n[↑/↓] Category  [Enter/l] Open  [Esc] Back")
	}
	return sb.String()
}

func (m Model) renderSettingsFields(width, height int) string {
	var sb strings.Builder
	groupLabel := settingsGroupLabel(m.settings.currentGroup())
	if m.settings.groupCursor >= 0 && m.settings.groupCursor < len(m.settings.groups) {
		if label := strings.TrimSpace(m.settings.groups[m.settings.groupCursor].Label); label != "" {
			groupLabel = textutil.SanitizeTerminal(label)
		}
	}
	sb.WriteString(m.styles.tableHeader.Render(groupLabel))
	sb.WriteString("\n")

	fields := m.settings.currentFields()
	if len(fields) == 0 {
		sb.WriteString("No settings in this category")
		return sb.String()
	}
	maxRows := max(min(len(fields), height/2), 1)
	start := 0
	if m.settings.rowCursor >= maxRows {
		start = m.settings.rowCursor - maxRows + 1
	}
	end := min(start+maxRows, len(fields))
	for i := start; i < end; i++ {
		field := fields[i]
		cursor := "  "
		if i == m.settings.rowCursor {
			cursor = "▶ "
		}
		dirty := " "
		_, configDirty := m.settings.drafts[field.Key]
		_, configSecretDirty := m.settings.configSecretDrafts[field.Key]
		_, credentialDirty := m.settings.credentialDrafts[field.Key]
		if configDirty || configSecretDirty || credentialDirty {
			dirty = "*"
		}
		label := textutil.SanitizeTerminal(field.Label)
		if label == "" {
			label = field.Key
		}
		value := m.settingsFieldValue(field)
		labelWidth := max(min(width/2, 34), 12)
		line := fmt.Sprintf("%s%s %-*s %s", cursor, dirty, labelWidth, truncateRunes(label, labelWidth), value)
		line = truncateRunes(line, max(width-1, 1))
		if i == m.settings.rowCursor {
			line = m.styles.cursorRow.Render(padRight(line, max(width-1, 1)))
		}
		sb.WriteString(line)
		sb.WriteString("\n")
	}

	selected, ok := m.settings.selectedField()
	if !ok {
		return strings.TrimSuffix(sb.String(), "\n")
	}
	sb.WriteString("\n")
	description := textutil.SanitizeTerminalMultiline(selected.Description)
	if description != "" {
		sb.WriteString(truncateRunes(description, max(width-1, 1)))
		sb.WriteString("\n")
	}
	_, _ = fmt.Fprintf(&sb, "Key: %s\n", textutil.SanitizeTerminal(selected.Key))
	metadata := make([]string, 0, 3)
	if selected.ReadOnly {
		metadata = append(metadata, "read-only")
	}
	if selected.RestartRequired {
		metadata = append(metadata, "restart required")
	}
	if len(selected.Options) > 0 {
		metadata = append(metadata, "options: "+strings.Join(selected.Options, " / "))
	}
	if len(metadata) > 0 {
		_, _ = fmt.Fprintf(&sb, "[%s]\n", strings.Join(metadata, "] ["))
	}
	if hint := strings.TrimSpace(selected.Validation.Hint); hint != "" {
		_, _ = fmt.Fprintf(&sb, "Validation: %s\n", textutil.SanitizeTerminal(hint))
	}
	if selected.Kind == SettingKindSecret && selected.Secret != nil && selected.Secret.Source != "" {
		_, _ = fmt.Fprintf(&sb, "Credential source: %s\n", textutil.SanitizeTerminal(selected.Secret.Source))
	}
	return strings.TrimSuffix(sb.String(), "\n")
}

func (m Model) settingsFieldValue(field SettingField) string {
	if m.settings.editing && m.settings.editKey == field.Key {
		if len(m.settings.editOptions) > 0 {
			if m.settings.editOption >= 0 && m.settings.editOption < len(m.settings.editOptions) {
				return "‹ " + textutil.SanitizeTerminal(m.settings.editOptions[m.settings.editOption]) + " ›"
			}
			return ""
		}
		return m.settings.editor.View()
	}
	if field.Kind == SettingKindSecret {
		return m.secretSettingDisplay(field)
	}
	value := m.currentSettingText(field)
	if field.Kind == SettingKindBoolean {
		if value == "true" {
			return "[x] enabled"
		}
		return "[ ] disabled"
	}
	if value == "" {
		return "—"
	}
	return textutil.SanitizeTerminal(value)
}

func (m Model) settingsFooter() string {
	if m.settings.saving {
		return "Saving settings…"
	}
	if m.settings.editing {
		if len(m.settings.editOptions) > 0 {
			return "[h/l] Choose  [Enter] Use  [Ctrl+S] Use & save  [Esc] Cancel"
		}
		return "[Enter] Use value  [Ctrl+S] Use & save  [Esc] Cancel"
	}
	if m.settingsIsNarrow() && !m.settings.narrowFields {
		return "[j/k] Category  [Enter/l] Open  [Esc] Back"
	}
	if field, ok := m.settings.selectedField(); ok &&
		field.Kind == SettingKindSecret && !field.ReadOnly {
		return "[j/k] Row  [h/l] Category  [Enter] Set secret  [x] Clear  [Ctrl+S] Save  [Esc] Back"
	}
	return "[j/k] Row  [h/l] Category  [Enter] Edit  [Space] Toggle  [Ctrl+S] Save  [Esc] Back"
}
