package tui

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSettingsViewportShortScreenPeopleDisable(t *testing.T) {
	backend := &fakePeopleInferenceControlBackend{loads: []SettingsSnapshot{settingsFixture()}, status: PeopleInferenceStatus{Configured: "example-profile", Running: "example-profile", ConfiguredFingerprint: strings.Repeat("a", 64), ConfiguredEnabled: true, RunningEnabled: true, ProfileCount: 1}}
	model := resizeModel(t, loadedSettingsModelWithBackend(t, &backend.fakeSettingsBackend), 40, 12)
	model.settingsBackend = backend
	model, load := sendKey(t, model, key('i'))
	model = sendSettingsMsg(t, model, load())
	model, _ = sendKey(t, model, key('d'))
	model, disable := sendKey(t, model, key('y'))
	model = sendSettingsMsg(t, model, disable())
	screen := uv.NewScreenBuffer(40, 12)
	uv.NewStyledString(model.View().Content).Draw(screen, screen.Bounds())
	assert.Contains(t, screen.String(), "[Esc]")
	assert.Contains(t, screen.String(), "People inference disabled.")
	assert.Contains(t, screen.String(), "--home")
	assert.Contains(t, screen.String(), "service.")
	assert.Regexp(t, `Fingerprint:\s*a{8}`, screen.String())
	model, _ = sendKey(t, model, key('v'))
	screen = uv.NewScreenBuffer(40, 12)
	uv.NewStyledString(model.View().Content).Draw(screen, screen.Bounds())
	assert.Contains(t, screen.String(), "Revoke consent for example-profile?")
	assert.Contains(t, screen.String(), "[y]")
	assert.Equal(t, 1, strings.Count(screen.String(), "Fingerprint:"))
	assert.Contains(t, strings.Join(strings.Fields(screen.String()), ""), strings.Repeat("a", 64))
	model, _ = sendKey(t, model, key('n'))
	model, _ = sendKey(t, model, tea.KeyPressMsg{Code: tea.KeyPgDown})
	screen = uv.NewScreenBuffer(40, 12)
	uv.NewStyledString(model.View().Content).Draw(screen, screen.Bounds())
	assert.Contains(t, strings.Join(strings.Fields(screen.String()), ""), strings.Repeat("a", 64))
	assert.Contains(t, screen.String(), "[Esc]")
	page := screen.String()
	model, _ = sendKey(t, model, tea.KeyPressMsg{Code: tea.KeyPgDown})
	screen = uv.NewScreenBuffer(40, 12)
	uv.NewStyledString(model.View().Content).Draw(screen, screen.Bounds())
	assert.Equal(t, page, screen.String())
	model, _ = sendKey(t, model, key('z'))
	screen = uv.NewScreenBuffer(40, 12)
	uv.NewStyledString(model.View().Content).Draw(screen, screen.Bounds())
	assert.Equal(t, page, screen.String())
	model = resizeModel(t, model, 40, 10)
	model, _ = sendKey(t, model, tea.KeyPressMsg{Code: tea.KeyPgDown})
	model = resizeModel(t, model, 120, 40)
	screen = uv.NewScreenBuffer(120, 40)
	uv.NewStyledString(model.View().Content).Draw(screen, screen.Bounds())
	assert.Contains(t, screen.String(), strings.Repeat("a", 64))
	model = resizeModel(t, model, 40, 14)
	model, _ = sendKey(t, model, tea.KeyPressMsg{Code: tea.KeyPgUp})
	screen = uv.NewScreenBuffer(40, 14)
	uv.NewStyledString(model.View().Content).Draw(screen, screen.Bounds())
	assert.Contains(t, screen.String(), "People inference disabled.")
	model, _ = sendKey(t, model, keyEsc())
	assert.False(t, model.settings.peopleControls.active)
}

func TestSettingsViewportShortScreenSaveFeedback(t *testing.T) {
	snapshot := SettingsSnapshot{Groups: []SettingsGroup{{ID: "archive", Label: "Archive"}}, Fields: []SettingField{{Key: "analytics.auto_build_cache", Group: "archive", Label: "Build cache", Kind: SettingKindBoolean, Value: boolSettingValue(true), RestartRequired: true}}}
	saved := snapshot
	saved.PendingRestart = true
	backend := &fakeSettingsBackend{loads: []SettingsSnapshot{snapshot}, save: saved}
	model := resizeModel(t, loadedSettingsModelWithBackend(t, backend), 40, 12)
	model, _ = sendKey(t, model, keyEnter())
	model, _ = sendKey(t, model, key(' '))
	model, save := sendKey(t, model, tea.KeyPressMsg{Code: 's', Mod: tea.ModCtrl})
	require.NotNil(t, save)
	model = sendSettingsMsg(t, model, save())
	screen := uv.NewScreenBuffer(40, 12)
	uv.NewStyledString(model.View().Content).Draw(screen, screen.Bounds())
	assert.Contains(t, screen.String(), "Settings saved.")
	assert.Contains(t, screen.String(), "[j/k]")
	assert.Contains(t, screen.String(), "[Esc]")
}

func TestSettingsViewportSaveKeepsSelectedFieldVisible(t *testing.T) {
	snapshot := SettingsSnapshot{Groups: []SettingsGroup{{ID: "server", Label: "Daemon"}}}
	for i := range 6 {
		snapshot.Fields = append(snapshot.Fields, SettingField{Key: fmt.Sprintf("server.field%d", i), Group: "server", Label: "Other setting", Kind: SettingKindBoolean, Value: boolSettingValue(true)})
	}
	snapshot.Fields = append(snapshot.Fields, SettingField{Key: "server.daemon_auto_start", Group: "server", Label: "Automatic start", Kind: SettingKindBoolean, Value: boolSettingValue(true), RestartRequired: true})
	saved := snapshot
	saved.PendingRestart = true
	backend := &fakeSettingsBackend{loads: []SettingsSnapshot{snapshot}, save: saved}
	model := resizeModel(t, loadedSettingsModelWithBackend(t, backend), 40, 12)
	model, _ = sendKey(t, model, keyEnter())
	for range 6 {
		model, _ = sendKey(t, model, key('j'))
	}
	model, _ = sendKey(t, model, key(' '))
	model, save := sendKey(t, model, tea.KeyPressMsg{Code: 's', Mod: tea.ModCtrl})
	require.NotNil(t, save)
	model = sendSettingsMsg(t, model, save())
	screen := uv.NewScreenBuffer(40, 12)
	uv.NewStyledString(model.View().Content).Draw(screen, screen.Bounds())
	assert.Contains(t, screen.String(), "Settings saved.")
	assert.Regexp(t, `(?s)daemon host.*msgvault daemon restart.*--home.*--config.*service`, screen.String())
	model, _ = sendKey(t, model, tea.KeyPressMsg{Code: tea.KeyPgDown})
	uv.NewStyledString(model.View().Content).Draw(screen, screen.Bounds())
	assert.Contains(t, screen.String(), "▶   Automatic start")
}

func TestSettingsViewportShortScreenSaveErrorAfterDisable(t *testing.T) {
	snapshot := SettingsSnapshot{Groups: []SettingsGroup{{ID: "archive", Label: "Archive"}}, Fields: []SettingField{{Key: "analytics.auto_build_cache", Group: "archive", Label: "Build cache", Kind: SettingKindBoolean, Value: boolSettingValue(true), RestartRequired: true}}}
	backend := &fakePeopleInferenceControlBackend{loads: []SettingsSnapshot{snapshot}, status: PeopleInferenceStatus{Configured: "example-profile", Running: "example-profile", ConfiguredFingerprint: strings.Repeat("a", 64), ConfiguredEnabled: true}}
	backend.saveErr = errors.New(`API error (500): {"title":"Internal Server Error","status":500,"detail":"disk full"}`)
	model := resizeModel(t, loadedSettingsModelWithBackend(t, &backend.fakeSettingsBackend), 40, 14)
	model.settingsBackend = backend
	model, load := sendKey(t, model, key('i'))
	model = sendSettingsMsg(t, model, load())
	model, _ = sendKey(t, model, key('d'))
	model, disable := sendKey(t, model, key('y'))
	model = sendSettingsMsg(t, model, disable())
	model, _ = sendKey(t, model, keyEsc())
	model, _ = sendKey(t, model, keyEnter())
	model, _ = sendKey(t, model, key(' '))
	model, save := sendKey(t, model, tea.KeyPressMsg{Code: 's', Mod: tea.ModCtrl})
	require.NotNil(t, save)
	model = sendSettingsMsg(t, model, save())
	screen := uv.NewScreenBuffer(40, 14)
	uv.NewStyledString(model.View().Content).Draw(screen, screen.Bounds())
	assert.Contains(t, strings.Join(strings.Fields(screen.String()), " "), "disk full")
	assert.Contains(t, screen.String(), "[Esc]")
	assert.Contains(t, screen.String(), "--home")
}
