package tui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type viewportFullBackend struct {
	*fakePeopleInferenceBackend
	PeopleInferenceControlBackend
}

func (b *viewportFullBackend) LoadPeopleInferenceStatus(ctx context.Context) (PeopleInferenceStatus, error) {
	return b.fakePeopleInferenceBackend.LoadPeopleInferenceStatus(ctx)
}

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
	model, _ = sendKey(t, model, key('v'))
	screen = uv.NewScreenBuffer(40, 12)
	uv.NewStyledString(model.View().Content).Draw(screen, screen.Bounds())
	assert.Contains(t, strings.Join(strings.Fields(screen.String()), ""), strings.Repeat("a", 64))
	model, _ = sendKey(t, model, key('n'))
	model, _ = sendKey(t, model, tea.KeyPressMsg{Code: tea.KeyPgDown})
	screen = uv.NewScreenBuffer(40, 12)
	uv.NewStyledString(model.View().Content).Draw(screen, screen.Bounds())
	assert.Contains(t, screen.String(), "[Esc]")
	page := screen.String()
	model, _ = sendKey(t, model, tea.KeyPressMsg{Code: tea.KeyPgDown})
	screen = uv.NewScreenBuffer(40, 12)
	uv.NewStyledString(model.View().Content).Draw(screen, screen.Bounds())
	assert.Equal(t, page, screen.String())
	assert.NotContains(t, screen.String(), "People inference disabled.")
	model, _ = sendKey(t, model, tea.KeyPressMsg{Code: tea.KeyPgUp})
	screen = uv.NewScreenBuffer(40, 12)
	uv.NewStyledString(model.View().Content).Draw(screen, screen.Bounds())
	assert.Contains(t, screen.String(), "People inference disabled.")
}

func TestSettingsViewportSaveShowsNoticeAndSelectedField(t *testing.T) {
	snapshot := SettingsSnapshot{Groups: []SettingsGroup{{ID: "server", Label: "Daemon"}}}
	for i := range 6 {
		snapshot.Fields = append(snapshot.Fields, SettingField{Key: fmt.Sprintf("server.field%d", i), Group: "server", Label: "Other setting", Kind: SettingKindBoolean, Value: boolSettingValue(true)})
	}
	snapshot.Fields = append(snapshot.Fields, SettingField{Key: "server.daemon_auto_start", Group: "server", Label: "Automatic start", Kind: SettingKindBoolean, Value: boolSettingValue(true), RestartRequired: true})
	saved := snapshot
	saved.PendingRestart = true
	backend := &viewportFullBackend{fakePeopleInferenceBackend: &fakePeopleInferenceBackend{loads: []SettingsSnapshot{snapshot}, save: saved}, PeopleInferenceControlBackend: &fakePeopleInferenceControlBackend{}}
	model := resizeModel(t, loadedSettingsModelWithBackend(t, &backend.fakeSettingsBackend), 40, 12)
	model.settingsBackend = backend
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
	assert.Contains(t, screen.String(), "[j/k]")
	assert.Contains(t, screen.String(), "[Esc]")
	assert.Regexp(t, `\[Ctrl\+S\] +Save`, screen.String())
	assert.Regexp(t, `(?m)^msgvault daemon restart\s*$`, screen.String())
	model, _ = sendKey(t, model, tea.KeyPressMsg{Code: tea.KeyPgDown})
	uv.NewStyledString(model.View().Content).Draw(screen, screen.Bounds())
	assert.Contains(t, screen.String(), "▶   Automatic start")
	assert.Contains(t, screen.String(), "Key: server.daemon_auto_start")
}

func TestSettingsViewportShortScreenSaveErrorAfterDisable(t *testing.T) {
	snapshot := SettingsSnapshot{Groups: []SettingsGroup{{ID: "archive", Label: "Archive"}}, Fields: []SettingField{{Key: "analytics.auto_build_cache", Group: "archive", Label: "Build cache", Kind: SettingKindBoolean, Value: boolSettingValue(true)}}}
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
}

func TestSettingsViewportStalePeopleLoadKeepsPage(t *testing.T) {
	snapshot := settingsFixture()
	snapshot.PendingRestart = true
	backend := &fakePeopleInferenceControlBackend{loads: []SettingsSnapshot{snapshot, snapshot}}
	model := resizeModel(t, loadedSettingsModelWithBackend(t, &backend.fakeSettingsBackend), 40, 8)
	model.settingsBackend = backend
	oldRequestID := model.settings.requestID
	model, _ = sendKey(t, model, keyEsc())
	model, reload := sendKey(t, model, key(','))
	require.NotNil(t, reload)
	model = sendSettingsMsg(t, model, reload())
	model, load := sendKey(t, model, key('i'))
	require.NotNil(t, load)
	model, _ = sendKey(t, model, keyEsc())
	screen := uv.NewScreenBuffer(40, 8)
	uv.NewStyledString(model.View().Content).Draw(screen, screen.Bounds())
	first := screen.String()
	model, _ = sendKey(t, model, tea.KeyPressMsg{Code: tea.KeyPgDown})
	uv.NewStyledString(model.View().Content).Draw(screen, screen.Bounds())
	before := screen.String()
	require.NotEqual(t, first, before)
	model = sendSettingsMsg(t, model, load())
	uv.NewStyledString(model.View().Content).Draw(screen, screen.Bounds())
	assert.Equal(t, before, screen.String())
	for _, stale := range []tea.Msg{
		settingsLoadedMsg{requestID: oldRequestID},
		settingsSavedMsg{requestID: oldRequestID},
	} {
		model = sendSettingsMsg(t, model, stale)
		uv.NewStyledString(model.View().Content).Draw(screen, screen.Bounds())
		assert.Equal(t, before, screen.String())
	}
}

func TestSettingsViewportTinyScreenKeepsPagingAndBack(t *testing.T) {
	snapshot := settingsFixture()
	snapshot.PendingRestart = true
	backend := &viewportFullBackend{fakePeopleInferenceBackend: &fakePeopleInferenceBackend{loads: []SettingsSnapshot{snapshot}}, PeopleInferenceControlBackend: &fakePeopleInferenceControlBackend{}}
	model := NewBuilder().WithSize(20, 6).Build()
	model.settingsBackend = backend
	model, load := sendKey(t, model, key(','))
	require.NotNil(t, load)
	model = sendSettingsMsg(t, model, load())
	screen := uv.NewScreenBuffer(20, 6)
	uv.NewStyledString(model.View().Content).Draw(screen, screen.Bounds())
	assert.Contains(t, screen.String(), "[Esc]")
	assert.Contains(t, screen.String(), "[PgUp/PgDn]")
	var pages strings.Builder
	for range 30 {
		page := strings.NewReplacer("Settings", "", "[Esc] Back", "", "[PgUp/PgDn] Scroll", "").Replace(screen.String())
		pages.WriteString(page)
		before := screen.String()
		model, _ = sendKey(t, model, tea.KeyPressMsg{Code: tea.KeyPgDown})
		uv.NewStyledString(model.View().Content).Draw(screen, screen.Bounds())
		if screen.String() == before {
			break
		}
	}
	assert.Contains(t, strings.Join(strings.Fields(pages.String()), ""), strings.Join(strings.Fields(settingsRestartNotice), ""))
	for _, text := range []string{"[p]", "[i]"} {
		assert.Contains(t, pages.String(), text)
	}
}

func TestSettingsViewportTinyScreenFooters(t *testing.T) {
	for _, tc := range []struct {
		name   string
		height int
	}{{"loading", 3}, {"fields", 6}, {"editing", 6}, {"codex", 6}, {"discard", 4}, {"discard", 6}} {
		t.Run(fmt.Sprintf("%s_%d", tc.name, tc.height), func(t *testing.T) {
			snapshot := settingsFixture()
			snapshot.Fields[0].Options = []string{"auto", "manual"}
			backend := &fakePeopleInferenceBackend{loads: []SettingsSnapshot{snapshot}}
			model := NewBuilder().WithSize(20, tc.height).Build()
			model.settingsBackend = backend
			model, load := sendKey(t, model, key(','))
			if tc.name != "loading" {
				model = sendSettingsMsg(t, model, load())
			}
			switch tc.name {
			case "fields", "editing", "discard":
				model, _ = sendKey(t, model, keyEnter())
			}
			if tc.name == "editing" || tc.name == "discard" {
				model, _ = sendKey(t, model, keyEnter())
			}
			if tc.name == "discard" {
				model, _ = sendKey(t, model, key('l'))
				model, _ = sendKey(t, model, keyEnter())
				model, _ = sendKey(t, model, keyEsc())
			}
			if tc.name == "codex" {
				model, _ = sendKey(t, model, key('p'))
			}
			screen := uv.NewScreenBuffer(20, tc.height)
			var pages strings.Builder
			for range 30 {
				uv.NewStyledString(model.View().Content).Draw(screen, screen.Bounds())
				view := screen.String()
				pages.WriteString(view)
				switch tc.name {
				case "loading", "fields":
					assert.Equal(t, 1, strings.Count(view, "[Esc] Back"))
				case "editing", "codex":
					assert.Equal(t, 1, strings.Count(view, "[Esc] Cancel"))
					assert.NotContains(t, view, "[Esc] Back")
				case "discard":
					assert.Contains(t, view, "[Y] Yes")
					assert.Contains(t, view, "[N/Esc] No")
					assert.Contains(t, view, "[PgUp/PgDn] Scroll")
				}
				model, _ = sendKey(t, model, tea.KeyPressMsg{Code: tea.KeyPgDown})
				uv.NewStyledString(model.View().Content).Draw(screen, screen.Bounds())
				if screen.String() == view {
					break
				}
			}
			if tc.name == "fields" {
				assert.Regexp(t, `\[Ctrl\+S\] +Save`, pages.String())
			}
			if tc.name == "discard" {
				assert.Contains(t, pages.String(), "not been saved")
			}
		})
	}
}
