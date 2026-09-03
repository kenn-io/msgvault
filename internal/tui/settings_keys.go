package tui

import (
	"strings"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
)

const settingsWideWidth = 86

func (m Model) settingsIsNarrow() bool {
	return m.width > 0 && m.width < settingsWideWidth
}

func (m Model) handleSettingsKeyPress(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if msg.String() == keyNameCtrlC {
		m.quitting = true
		return m, tea.Quit
	}
	if m.settings.saving {
		if msg.String() == keyNameEsc {
			m.settings.confirmDiscard = false
			m.settings.status = "Save in progress; wait for it to finish."
			m.settings.statusIsError = false
		}
		return m, nil
	}
	if m.settings.confirmDiscard {
		switch msg.String() {
		case "y", "Y":
			m.settings = newSettingsState()
		case "n", "N", keyNameEsc:
			m.settings.confirmDiscard = false
		}
		return m, nil
	}

	if m.settings.editing {
		return m.handleSettingsEditorKey(msg)
	}

	switch msg.String() {
	case "ctrl+s":
		return m.saveSettings()
	case keyNameEsc:
		if m.settings.dirty() {
			m.settings.confirmDiscard = true
			m.settings.status = ""
			return m, nil
		}
		m.settings = newSettingsState()
		return m, nil
	}

	if m.settings.loading {
		return m, nil
	}
	if m.settingsIsNarrow() && !m.settings.narrowFields {
		return m.handleNarrowSettingsCategoryKey(msg)
	}

	switch msg.String() {
	case "up", "k", keyNameCtrlP:
		if m.settings.rowCursor > 0 {
			m.settings.rowCursor--
		}
	case keyNameDown, "j", keyNameCtrlN:
		if m.settings.rowCursor+1 < len(m.settings.currentFields()) {
			m.settings.rowCursor++
		}
	case "left", "h":
		if m.settingsIsNarrow() {
			m.settings.narrowFields = false
			return m, nil
		}
		m.changeSettingsGroup(-1)
	case keyNameRight, "l":
		if !m.settingsIsNarrow() {
			m.changeSettingsGroup(1)
		}
	case keyNameEnter:
		return m.beginSettingsEdit()
	case " ", "space":
		m.toggleSelectedBooleanSetting()
	case "x":
		m.clearSelectedSecretSetting()
	}
	return m, nil
}

func (m Model) handleNarrowSettingsCategoryKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "up", "k", keyNameCtrlP:
		if m.settings.groupCursor > 0 {
			m.settings.groupCursor--
		}
	case keyNameDown, "j", keyNameCtrlN:
		if m.settings.groupCursor+1 < len(m.settings.groups) {
			m.settings.groupCursor++
		}
	case keyNameEnter, keyNameRight, "l":
		if len(m.settings.groups) > 0 {
			m.settings.narrowFields = true
			m.settings.rowCursor = 0
		}
	}
	return m, nil
}

func (m *Model) changeSettingsGroup(delta int) {
	if len(m.settings.groups) == 0 {
		return
	}
	m.settings.groupCursor = min(max(m.settings.groupCursor+delta, 0), len(m.settings.groups)-1)
	m.settings.rowCursor = 0
	m.settings.status = ""
}

func (m Model) beginSettingsEdit() (tea.Model, tea.Cmd) {
	field, ok := m.settings.selectedField()
	if !ok {
		return m, nil
	}
	if field.ReadOnly {
		m.settings.status = "This setting is read-only; edit it on the daemon host."
		m.settings.statusIsError = false
		return m, nil
	}
	if field.Kind == SettingKindBoolean {
		m.toggleSelectedBooleanSetting()
		return m, nil
	}

	m.settings.editing = true
	m.settings.editKey = field.Key
	m.settings.status = ""
	m.settings.statusIsError = false
	if len(field.Options) > 0 {
		m.settings.editOptions = append([]string(nil), field.Options...)
		current := m.currentSettingText(field)
		m.settings.editOption = 0
		for i, option := range field.Options {
			if option == current {
				m.settings.editOption = i
				break
			}
		}
		return m, nil
	}

	input := textinput.New()
	input.CharLimit = 2048
	input.SetWidth(max(min(m.width-12, 64), 12))
	input.Placeholder = "enter value"
	if field.Kind == SettingKindSecret {
		input.EchoMode = textinput.EchoPassword
		input.EchoCharacter = '*'
		input.Placeholder = "new secret (write-only)"
	} else {
		input.SetValue(m.currentSettingText(field))
	}
	m.settings.editor = input
	return m, m.settings.editor.Focus()
}

func (m Model) handleSettingsEditorKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	field, ok := m.settingFieldByKey(m.settings.editKey)
	if !ok {
		m.cancelSettingsEdit()
		return m, nil
	}
	if msg.String() == "ctrl+s" {
		if !m.commitSettingsEdit(field) {
			return m, nil
		}
		return m.saveSettings()
	}
	if len(m.settings.editOptions) > 0 {
		switch msg.String() {
		case "left", "h", "up", "k":
			if m.settings.editOption > 0 {
				m.settings.editOption--
			}
		case keyNameRight, "l", keyNameDown, "j":
			if m.settings.editOption+1 < len(m.settings.editOptions) {
				m.settings.editOption++
			}
		case keyNameEnter:
			m.commitSettingsEdit(field)
		case keyNameEsc:
			m.cancelSettingsEdit()
		}
		return m, nil
	}

	switch msg.String() {
	case keyNameEnter:
		m.commitSettingsEdit(field)
		return m, nil
	case keyNameEsc:
		m.cancelSettingsEdit()
		return m, nil
	case keyNameCtrlC:
		m.quitting = true
		return m, tea.Quit
	}

	var cmd tea.Cmd
	m.settings.editor, cmd = m.settings.editor.Update(msg)
	return m, cmd
}

func (m *Model) commitSettingsEdit(field SettingField) bool {
	if len(m.settings.editOptions) > 0 {
		if m.settings.editOption < 0 || m.settings.editOption >= len(m.settings.editOptions) {
			return false
		}
		value := m.settings.editOptions[m.settings.editOption]
		m.setSettingsDraft(field, SettingUpdate{Key: field.Key, Value: &SettingValue{String: &value}})
		m.cancelSettingsEdit()
		return true
	}

	raw := m.settings.editor.Value()
	if field.Kind == SettingKindSecret {
		if raw != "" {
			m.setSecretDraft(field, "set", raw)
		}
		m.cancelSettingsEdit()
		return true
	}
	value, err := parseSettingInput(field, raw)
	if err != nil {
		m.settings.status = "Invalid value: " + err.Error()
		m.settings.statusIsError = true
		return false
	}
	m.setSettingsDraft(field, SettingUpdate{Key: field.Key, Value: value})
	m.cancelSettingsEdit()
	return true
}

func (m *Model) cancelSettingsEdit() {
	m.settings.editor.Blur()
	m.settings.editor.Reset()
	m.settings.editing = false
	m.settings.editKey = ""
	m.settings.editOptions = nil
	m.settings.editOption = 0
}

func (m *Model) toggleSelectedBooleanSetting() {
	field, ok := m.settings.selectedField()
	if !ok || field.ReadOnly || field.Kind != SettingKindBoolean {
		return
	}
	current := false
	if draft, ok := m.settings.drafts[field.Key]; ok && draft.Value != nil && draft.Value.Boolean != nil {
		current = *draft.Value.Boolean
	} else if field.Value != nil && field.Value.Boolean != nil {
		current = *field.Value.Boolean
	}
	value := !current
	m.setSettingsDraft(field, SettingUpdate{Key: field.Key, Value: &SettingValue{Boolean: &value}})
}

func (m *Model) clearSelectedSecretSetting() {
	field, ok := m.settings.selectedField()
	if !ok || field.ReadOnly || field.Kind != SettingKindSecret {
		return
	}
	configured := field.Secret != nil && field.Secret.Configured
	if !configured {
		delete(m.settings.configSecretDrafts, field.Key)
		delete(m.settings.credentialDrafts, field.Key)
		return
	}
	m.setSecretDraft(field, "clear", "")
}

func (m *Model) setSecretDraft(field SettingField, action, value string) {
	credentialID := settingCredentialID(field)
	if credentialID != "" {
		delete(m.settings.configSecretDrafts, field.Key)
		m.setCredentialDraft(field, CredentialUpdate{
			Key: field.Key, CredentialID: credentialID, Action: action, Value: value,
		})
		return
	}
	delete(m.settings.credentialDrafts, field.Key)
	m.setConfigSecretDraft(field, ConfigSecretUpdate{
		Key: field.Key, Action: action, Value: value,
	})
}

func (m Model) currentSettingText(field SettingField) string {
	if draft, ok := m.settings.drafts[field.Key]; ok && draft.Value != nil {
		return settingUpdateText(draft)
	}
	return settingValueText(field.Value)
}

func (m Model) secretSettingDisplay(field SettingField) string {
	if draft, ok := m.settings.configSecretDrafts[field.Key]; ok {
		if strings.EqualFold(draft.Action, "clear") {
			return "not configured after save"
		}
		return "configured after save"
	}
	if draft, ok := m.settings.credentialDrafts[field.Key]; ok {
		if strings.EqualFold(draft.Action, "clear") {
			return "clear stored override; environment may remain"
		}
		return "configured after save"
	}
	if field.Secret == nil || !field.Secret.Configured {
		return "not configured"
	}
	if source := strings.TrimSpace(field.Secret.Source); source != "" {
		return "configured (" + source + ")"
	}
	return "configured"
}
