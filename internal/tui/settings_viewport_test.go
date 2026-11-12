package tui

import (
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/stretchr/testify/assert"
)

func TestSettingsViewportEndpointDetail(t *testing.T) {
	snapshot := SettingsSnapshot{Groups: []SettingsGroup{{ID: "search", Label: "Search"}}}
	for range 6 {
		snapshot.Fields = append(snapshot.Fields, SettingField{Key: "search.field", Group: "search", Label: "Other", Kind: SettingKindString, Value: stringSettingValue("auto")})
	}
	snapshot.Fields = append(snapshot.Fields, SettingField{Key: "vector.embeddings.endpoint", Group: "search", Label: "Embedding endpoint", Description: "Endpoint for the embedding service.", RestartRequired: true, Kind: SettingKindString, Value: stringSettingValue(""), Validation: SettingValidation{Hint: "HTTP or HTTPS URL without credentials, query, or fragment."}})
	model := resizeModel(t, loadedSettingsModel(t, snapshot), 80, 12)
	model, _ = sendKey(t, model, keyEnter())
	for range 6 {
		model, _ = sendKey(t, model, key('j'))
	}
	screen := uv.NewScreenBuffer(80, 12)
	uv.NewStyledString(model.View().Content).Draw(screen, screen.Bounds())
	assert.Contains(t, screen.String(), "HTTP or HTTPS URL without credentials")
	model, _ = sendKey(t, model, tea.KeyPressMsg{Code: tea.KeyPgUp})
	model, _ = sendKey(t, model, tea.KeyPressMsg{Code: tea.KeyPgDown})
	screen = uv.NewScreenBuffer(80, 12)
	uv.NewStyledString(model.View().Content).Draw(screen, screen.Bounds())
	assert.Contains(t, screen.String(), "HTTP or HTTPS URL without credentials")
	assert.Contains(t, screen.String(), "[Esc]")
	model, _ = sendKey(t, model, key('k'))
	screen = uv.NewScreenBuffer(80, 12)
	uv.NewStyledString(model.View().Content).Draw(screen, screen.Bounds())
	assert.Contains(t, screen.String(), "Key: search.field")
}

func TestSettingsViewportPendingSelectionKeepsDetail(t *testing.T) {
	for _, width := range []int{80, 100} {
		t.Run(fmt.Sprintf("width_%d", width), func(t *testing.T) {
			snapshot := SettingsSnapshot{PendingRestart: true, Groups: []SettingsGroup{{ID: "search", Label: "Search"}}}
			for i := range 6 {
				snapshot.Fields = append(snapshot.Fields, SettingField{Key: fmt.Sprintf("search.field%d", i), Group: "search", Label: "Other", Kind: SettingKindString, Value: stringSettingValue("auto")})
			}
			snapshot.Fields = append(snapshot.Fields, SettingField{Key: "vector.embeddings.endpoint", Group: "search", Label: "Embedding endpoint", Description: "Endpoint for the embedding service.", RestartRequired: true, Kind: SettingKindString, Value: stringSettingValue(""), Validation: SettingValidation{Hint: "HTTP or HTTPS URL without credentials, query, or fragment."}})
			model := resizeModel(t, loadedSettingsModel(t, snapshot), width, 12)
			if width < settingsWideWidth {
				model, _ = sendKey(t, model, keyEnter())
			}
			for range 6 {
				model, _ = sendKey(t, model, key('j'))
			}
			screen := uv.NewScreenBuffer(width, 12)
			uv.NewStyledString(model.View().Content).Draw(screen, screen.Bounds())
			assert.Contains(t, screen.String(), "HTTP or HTTPS URL without credentials")
		})
	}
}

func TestSettingsViewportFingerprint(t *testing.T) {
	backend := &fakePeopleInferenceControlBackend{loads: []SettingsSnapshot{settingsFixture()}, status: PeopleInferenceStatus{Configured: "example-profile", Running: "example-profile", ConfiguredFingerprint: strings.Repeat("a", 64), PendingRestart: true}}
	model := resizeModel(t, loadedSettingsModelWithBackend(t, &backend.fakeSettingsBackend), 80, 24)
	model.settingsBackend = backend
	model, load := sendKey(t, model, key('i'))
	model = sendSettingsMsg(t, model, load())
	model, _ = sendKey(t, model, keyEsc())
	screen := uv.NewScreenBuffer(80, 24)
	uv.NewStyledString(model.View().Content).Draw(screen, screen.Bounds())
	assert.Contains(t, strings.Join(strings.Fields(screen.String()), ""), strings.Repeat("a", 64))
}

func TestSettingsViewportProfileWithoutRestartKeepsNavigation(t *testing.T) {
	backend := &fakePeopleInferenceControlBackend{loads: []SettingsSnapshot{settingsFixture()}, status: PeopleInferenceStatus{Configured: "example-profile", Running: "example-profile", ConfiguredFingerprint: strings.Repeat("a", 64)}}
	model := resizeModel(t, loadedSettingsModelWithBackend(t, &backend.fakeSettingsBackend), 40, 12)
	model.settingsBackend = backend
	model, load := sendKey(t, model, key('i'))
	model = sendSettingsMsg(t, model, load())
	model, _ = sendKey(t, model, keyEsc())
	model, _ = sendKey(t, model, keyEnter())
	screen := uv.NewScreenBuffer(40, 12)
	uv.NewStyledString(model.View().Content).Draw(screen, screen.Bounds())
	assert.Contains(t, screen.String(), "[j/k]")
	assert.Contains(t, screen.String(), "[Esc]")
	assert.Contains(t, strings.Join(strings.Fields(screen.String()), ""), strings.Repeat("a", 64))
}

func TestSettingsViewportSettingsRestartNotice(t *testing.T) {
	for _, tc := range []struct {
		name             string
		settings, people bool
	}{
		{name: "settings", settings: true}, {name: "people", people: true}, {name: "both", settings: true, people: true}, {name: "neither"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			model := resizeModel(t, loadedSettingsModel(t, settingsFixture()), 80, 24)
			model.settings.pendingRestart = tc.settings
			model.settings.peopleInferenceStatus = &PeopleInferenceStatus{PendingRestart: tc.people}
			screen := uv.NewScreenBuffer(80, 24)
			uv.NewStyledString(model.View().Content).Draw(screen, screen.Bounds())
			view := strings.Join(strings.Fields(screen.String()), " ")
			if !tc.settings && !tc.people {
				assert.NotContains(t, view, "msgvault daemon restart")
				return
			}
			assert.Equal(t, 1, strings.Count(view, "msgvault daemon restart"))
			assert.Contains(t, view, "on the daemon host, run msgvault daemon restart with its --home and --config, or restart its service")
		})
	}
}

func TestSettingsViewportCodexRestartNotice(t *testing.T) {
	model := resizeModel(t, loadedSettingsModel(t, settingsFixture()), 80, 24)
	model.settings.codex.active = true
	model.settings.codex.stage = "selected"
	model.settings.codex.status = PeopleInferenceStatus{Configured: "example", Running: "example", PendingRestart: true}
	screen := uv.NewScreenBuffer(80, 24)
	uv.NewStyledString(model.View().Content).Draw(screen, screen.Bounds())
	view := strings.Join(strings.Fields(screen.String()), " ")
	assert.Contains(t, view, "on the daemon host, run msgvault daemon restart with its --home and --config, or restart its service")
	assert.Contains(t, view, "[Esc] Cancel and return")
}

func TestSettingsViewportCodexModelSelectionRemainsVisible(t *testing.T) {
	model := resizeModel(t, loadedSettingsModel(t, settingsFixture()), 40, 12)
	model.settings.codex.active = true
	model.settings.codex.stage = "models"
	for i := range 12 {
		model.settings.codex.models = append(model.settings.codex.models, CodexModelChoice{ID: fmt.Sprintf("model-%02d", i), ReasoningEfforts: []string{"medium"}})
	}
	for range 10 {
		model, _ = sendKey(t, model, key('j'))
	}
	screen := uv.NewScreenBuffer(40, 12)
	uv.NewStyledString(model.View().Content).Draw(screen, screen.Bounds())
	assert.Contains(t, screen.String(), "▶ model-10")
	assert.Contains(t, screen.String(), "[Esc]")
	model, _ = sendKey(t, model, tea.KeyPressMsg{Code: tea.KeyPgUp})
	model, _ = sendKey(t, model, key('j'))
	uv.NewStyledString(model.View().Content).Draw(screen, screen.Bounds())
	assert.Contains(t, screen.String(), "▶ model-11")
}

func TestSettingsViewportCodexPolicyEditorRemainsVisible(t *testing.T) {
	backend := &fakePeopleInferenceBackend{loads: []SettingsSnapshot{settingsFixture()}}
	model := resizeModel(t, loadedSettingsModelWithBackend(t, &backend.fakeSettingsBackend), 40, 13)
	model.settingsBackend = backend
	model, _ = sendKey(t, model, key('p'))
	model, _ = sendKey(t, model, key('1'))
	for _, r := range "example-profile" {
		model, _ = sendKey(t, model, key(r))
	}
	screen := uv.NewScreenBuffer(40, 13)
	uv.NewStyledString(model.View().Content).Draw(screen, screen.Bounds())
	assert.Contains(t, screen.String(), "Edit field 1:")
	assert.Contains(t, screen.String(), "example-profile")
}
