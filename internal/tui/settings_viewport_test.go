package tui

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSettingsViewportShortWideScreenKeepsCategoryVisible(t *testing.T) {
	snapshot := SettingsSnapshot{}
	for _, group := range []string{"Archive", "Daemon", "Search"} {
		id := strings.ToLower(group)
		snapshot.Groups = append(snapshot.Groups, SettingsGroup{ID: id, Label: group})
		for i := range 3 {
			snapshot.Fields = append(snapshot.Fields, SettingField{Key: fmt.Sprintf("%s.field%d", id, i), Group: id, Label: fmt.Sprintf("%s field %d", group, i), Kind: SettingKindString, Value: stringSettingValue("value")})
		}
	}
	for _, height := range []int{5, 6} {
		t.Run(strconv.Itoa(height), func(t *testing.T) {
			backend := &fakePeopleInferenceControlBackend{loads: []SettingsSnapshot{snapshot}}
			model := resizeModel(t, loadedSettingsModelWithBackend(t, &backend.fakeSettingsBackend), 100, height)
			model.settingsBackend = backend
			for _, step := range []struct {
				key             rune
				category, field string
			}{{'l', "Daemon", "Daemon field 0"}, {'l', "Search", "Search field 0"}, {'j', "Search", "Search field 1"}, {'j', "Search", "Search field 2"}} {
				model, _ = sendKey(t, model, key(step.key))
				screen := uv.NewScreenBuffer(100, height)
				uv.NewStyledString(model.View().Content).Draw(screen, screen.Bounds())
				assert.Contains(t, screen.String(), "▶ "+step.category)
				assert.Contains(t, screen.String(), "▶   "+step.field)
			}
		})
	}
}

func TestSettingsViewportEndpointDetail(t *testing.T) {
	for _, tc := range []struct {
		pending bool
		width   int
	}{{false, 80}, {true, 80}, {true, 100}} {
		t.Run(fmt.Sprintf("pending_%t_width_%d", tc.pending, tc.width), func(t *testing.T) {
			snapshot := SettingsSnapshot{PendingRestart: tc.pending, Groups: []SettingsGroup{{ID: "search", Label: "Search"}}}
			for i := range 6 {
				snapshot.Fields = append(snapshot.Fields, SettingField{Key: fmt.Sprintf("search.field%d", i), Group: "search", Label: "Other", Kind: SettingKindString, Value: stringSettingValue("auto")})
			}
			snapshot.Fields = append(snapshot.Fields, SettingField{Key: "vector.embeddings.endpoint", Group: "search", Label: "Embedding endpoint", Description: "Endpoint example ▶ value.", RestartRequired: true, Kind: SettingKindString, Value: stringSettingValue(""), Validation: SettingValidation{Hint: "HTTP or HTTPS URL without credentials, query, or fragment."}})
			model := resizeModel(t, loadedSettingsModel(t, snapshot), tc.width, 12)
			if tc.width < settingsWideWidth {
				model, _ = sendKey(t, model, keyEnter())
			}
			for range 6 {
				model, _ = sendKey(t, model, key('j'))
			}
			screen := uv.NewScreenBuffer(tc.width, 12)
			uv.NewStyledString(model.View().Content).Draw(screen, screen.Bounds())
			assert.Contains(t, screen.String(), "▶   Embedding endpoint")
			assert.Contains(t, screen.String(), "HTTP or HTTPS URL without credentials")
		})
	}
}

func TestSettingsCategoryBoundarySelectionAndPage(t *testing.T) {
	snapshot := SettingsSnapshot{Groups: []SettingsGroup{{ID: "search", Label: "Search"}}}
	for i := range 2 {
		snapshot.Fields = append(snapshot.Fields, SettingField{Key: fmt.Sprintf("search.field%d", i), Group: "search", Label: fmt.Sprintf("Field %d", i), Kind: SettingKindString, Value: stringSettingValue("value")})
	}
	snapshot.Fields[0].Validation.Hint = strings.Repeat("example ", 100)
	model := resizeModel(t, loadedSettingsModel(t, snapshot), 100, 12)
	for _, direction := range []rune{'h', 'l'} {
		model, _ = sendKey(t, model, key('j'))
		model, _ = sendKey(t, model, key(direction))
		screen := uv.NewScreenBuffer(100, 12)
		uv.NewStyledString(model.View().Content).Draw(screen, screen.Bounds())
		assert.Contains(t, screen.String(), "▶   Field 0")
	}
	model.settings.pendingRestart = true
	model = resizeModel(t, model, 100, 8)
	screen := uv.NewScreenBuffer(100, 8)
	uv.NewStyledString(model.View().Content).Draw(screen, screen.Bounds())
	first := screen.String()
	model, _ = sendKey(t, model, tea.KeyPressMsg{Code: tea.KeyPgDown})
	model, _ = sendKey(t, model, tea.KeyPressMsg{Code: tea.KeyPgDown})
	uv.NewStyledString(model.View().Content).Draw(screen, screen.Bounds())
	before := screen.String()
	require.NotEqual(t, first, before)
	for _, direction := range []rune{'h', 'l'} {
		model, _ = sendKey(t, model, key(direction))
		uv.NewStyledString(model.View().Content).Draw(screen, screen.Bounds())
		assert.Equal(t, before, screen.String())
	}
}

func TestSettingsCategoryBoundaryClearsStatusAndShowsSelection(t *testing.T) {
	snapshot := SettingsSnapshot{Groups: []SettingsGroup{{ID: "server", Label: "Server"}}, Fields: []SettingField{
		{Key: "server.bind_addr", Group: "server", Label: "Bind address", Kind: SettingKindString, Value: stringSettingValue("127.0.0.1"), ReadOnly: true},
		{Key: "server.other", Group: "server", Label: "Other setting", Kind: SettingKindString, Value: stringSettingValue("value")},
	}}
	snapshot.Fields[0].Validation.Hint = strings.Repeat("example ", 100)
	for _, direction := range []rune{'h', 'l'} {
		t.Run(string(direction), func(t *testing.T) {
			model := resizeModel(t, loadedSettingsModel(t, snapshot), 100, 10)
			for _, msg := range []tea.KeyPressMsg{keyEnter(), key('j'), key('k'), key(direction)} {
				model, _ = sendKey(t, model, msg)
			}
			screen := uv.NewScreenBuffer(100, 10)
			uv.NewStyledString(model.View().Content).Draw(screen, screen.Bounds())
			assert.Contains(t, screen.String(), "▶   Bind address")
			assert.NotContains(t, screen.String(), "This setting is read-only")
		})
	}
}

func TestSettingsViewportExitEditorKeepsSelectionVisible(t *testing.T) {
	for _, tc := range []struct {
		name   string
		exit   tea.KeyPressMsg
		resize bool
	}{{name: "escape", exit: keyEsc()}, {name: "enter", exit: keyEnter()}, {name: "resize", resize: true}} {
		t.Run(tc.name, func(t *testing.T) {
			snapshot := SettingsSnapshot{PendingRestart: true, Groups: []SettingsGroup{{ID: "search", Label: "Search"}}}
			for i := range 7 {
				snapshot.Fields = append(snapshot.Fields, SettingField{Key: fmt.Sprintf("search.field%d", i), Group: "search", Label: fmt.Sprintf("Field %d", i), Kind: SettingKindString, Value: stringSettingValue("value")})
			}
			height := 12
			if !tc.resize {
				height = 10
				snapshot.Fields[6].Validation.Hint = strings.Repeat("example ", 40)
			}
			model := resizeModel(t, loadedSettingsModel(t, snapshot), 40, height)
			model, _ = sendKey(t, model, keyEnter())
			for range 6 {
				model, _ = sendKey(t, model, key('j'))
			}
			if tc.resize {
				model, _ = sendKey(t, model, tea.KeyPressMsg{Code: tea.KeyPgDown})
				screen := uv.NewScreenBuffer(40, height)
				uv.NewStyledString(model.View().Content).Draw(screen, screen.Bounds())
				require.Contains(t, screen.String(), "▶   Field 6")
				height = 8
				model = resizeModel(t, model, 40, height)
			} else {
				model, _ = sendKey(t, model, keyEnter())
				for range 5 {
					model, _ = sendKey(t, model, tea.KeyPressMsg{Code: tea.KeyPgDown})
				}
				model, _ = sendKey(t, model, tc.exit)
			}
			screen := uv.NewScreenBuffer(40, height)
			uv.NewStyledString(model.View().Content).Draw(screen, screen.Bounds())
			assert.Contains(t, screen.String(), "▶   Field 6")
		})
	}
}

func TestSettingsViewportResizeKeepsUnpagedSelection(t *testing.T) {
	for _, tc := range []struct{ width, newWidth, newHeight int }{{80, 40, 8}, {100, 100, 6}} {
		t.Run(strconv.Itoa(tc.width), func(t *testing.T) {
			snapshot := SettingsSnapshot{PendingRestart: true, Groups: []SettingsGroup{{ID: "search", Label: "Search"}}, Fields: []SettingField{{Key: "search.field", Group: "search", Label: "Search field", Kind: SettingKindString, Value: stringSettingValue("value")}}}
			model := resizeModel(t, loadedSettingsModel(t, snapshot), tc.width, 24)
			screen := uv.NewScreenBuffer(tc.width, 24)
			uv.NewStyledString(model.View().Content).Draw(screen, screen.Bounds())
			require.Contains(t, screen.String(), "▶ Search")
			model = resizeModel(t, model, tc.newWidth, tc.newHeight)
			screen = uv.NewScreenBuffer(tc.newWidth, tc.newHeight)
			uv.NewStyledString(model.View().Content).Draw(screen, screen.Bounds())
			assert.Contains(t, screen.String(), "▶ Search")
		})
	}
	snapshot := settingsFixture()
	snapshot.PendingRestart = true
	backend := &fakeSettingsBackend{loads: []SettingsSnapshot{snapshot}}
	model := NewBuilder().WithSize(40, 8).Build()
	model.settingsBackend = backend
	model, load := sendKey(t, model, key(','))
	model = sendSettingsMsg(t, model, load())
	model = resizeModel(t, model, 40, 7)
	screen := uv.NewScreenBuffer(40, 7)
	uv.NewStyledString(model.View().Content).Draw(screen, screen.Bounds())
	assert.Contains(t, screen.String(), "Restart pending.")
}

func TestSettingsViewportLoadKeepsCodexEditorVisible(t *testing.T) {
	backend := &fakePeopleInferenceBackend{loads: []SettingsSnapshot{settingsFixture()}}
	model := NewBuilder().WithSize(40, 8).Build()
	model.settingsBackend = backend
	model, load := sendKey(t, model, key(','))
	require.NotNil(t, load)
	model, _ = sendKey(t, model, key('p'))
	model, _ = sendKey(t, model, key('1'))
	for _, r := range "example-profile" {
		model, _ = sendKey(t, model, key(r))
	}
	screen := uv.NewScreenBuffer(40, 8)
	uv.NewStyledString(model.View().Content).Draw(screen, screen.Bounds())
	require.Contains(t, screen.String(), "Edit field 1:")
	model = sendSettingsMsg(t, model, load())
	uv.NewStyledString(model.View().Content).Draw(screen, screen.Bounds())
	assert.Contains(t, screen.String(), "Edit field 1:")
	assert.Contains(t, screen.String(), "example-profile")
}

func TestSettingsViewportCategorySelectionAfterPeopleControls(t *testing.T) {
	snapshot := SettingsSnapshot{Groups: []SettingsGroup{{ID: "archive", Label: "Archive"}, {ID: "daemon", Label: "Daemon"}}, Fields: []SettingField{{Key: "archive.enabled", Group: "archive", Label: "Archive", Kind: SettingKindBoolean, Value: boolSettingValue(true)}, {Key: "daemon.enabled", Group: "daemon", Label: "Daemon", Kind: SettingKindBoolean, Value: boolSettingValue(true)}}}
	backend := &fakePeopleInferenceControlBackend{loads: []SettingsSnapshot{snapshot}, status: PeopleInferenceStatus{Configured: "example-profile", Running: "example-profile", ConfiguredFingerprint: strings.Repeat("a", 64), PendingRestart: true}}
	model := resizeModel(t, loadedSettingsModelWithBackend(t, &backend.fakeSettingsBackend), 40, 8)
	model.settingsBackend = backend
	model, load := sendKey(t, model, key('i'))
	model = sendSettingsMsg(t, model, load())
	model, _ = sendKey(t, model, tea.KeyPressMsg{Code: tea.KeyPgDown})
	model, _ = sendKey(t, model, keyEsc())
	screen := uv.NewScreenBuffer(40, 8)
	uv.NewStyledString(model.View().Content).Draw(screen, screen.Bounds())
	require.Contains(t, screen.String(), "Restart pending.")
	require.NotContains(t, screen.String(), "▶ Daemon")
	model, _ = sendKey(t, model, key('j'))
	uv.NewStyledString(model.View().Content).Draw(screen, screen.Bounds())
	assert.Contains(t, screen.String(), "▶ Daemon")
	for range 3 {
		model, _ = sendKey(t, model, tea.KeyPressMsg{Code: tea.KeyPgUp})
	}
	uv.NewStyledString(model.View().Content).Draw(screen, screen.Bounds())
	require.NotContains(t, screen.String(), "▶ Daemon")
	model, _ = sendKey(t, model, keyEnter())
	uv.NewStyledString(model.View().Content).Draw(screen, screen.Bounds())
	assert.Contains(t, screen.String(), "▶   Daemon")
	for range 3 {
		model, _ = sendKey(t, model, tea.KeyPressMsg{Code: tea.KeyPgUp})
	}
	uv.NewStyledString(model.View().Content).Draw(screen, screen.Bounds())
	require.NotContains(t, screen.String(), "▶   Daemon")
	model, _ = sendKey(t, model, key('h'))
	uv.NewStyledString(model.View().Content).Draw(screen, screen.Bounds())
	assert.Contains(t, screen.String(), "▶ Daemon")
	assert.Contains(t, screen.String(), "[Esc]")
}

func TestSettingsViewportSettingsRestartNotice(t *testing.T) {
	for _, tc := range []struct {
		name             string
		settings, people bool
	}{
		{name: "settings", settings: true}, {name: "people", people: true}, {name: "both", settings: true, people: true}, {name: "neither"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			snapshot := settingsFixture()
			snapshot.PendingRestart = tc.settings
			backend := &fakePeopleInferenceControlBackend{loads: []SettingsSnapshot{snapshot}, status: PeopleInferenceStatus{PendingRestart: tc.people}}
			model := resizeModel(t, loadedSettingsModelWithBackend(t, &backend.fakeSettingsBackend), 80, 24)
			model.settingsBackend = backend
			model, load := sendKey(t, model, key('i'))
			model = sendSettingsMsg(t, model, load())
			model, _ = sendKey(t, model, keyEsc())
			screen := uv.NewScreenBuffer(80, 24)
			uv.NewStyledString(model.View().Content).Draw(screen, screen.Bounds())
			view := strings.Join(strings.Fields(screen.String()), " ")
			if !tc.settings && !tc.people {
				assert.NotContains(t, view, "msgvault daemon restart")
				return
			}
			assert.Equal(t, 1, strings.Count(view, "msgvault daemon restart"))
			assert.Contains(t, view, "On the daemon host, restart its service, or stop and rerun your foreground msgvault serve with the same flags")
			assert.Contains(t, view, "Otherwise, run: msgvault daemon restart with the same --home and --config")
		})
	}
}

func TestSettingsViewportCodexRestartNotice(t *testing.T) {
	backend := &fakePeopleInferenceBackend{loads: []SettingsSnapshot{settingsFixture()}}
	model := resizeModel(t, loadedSettingsModelWithBackend(t, &backend.fakeSettingsBackend), 80, 24)
	model.settingsBackend = backend
	model, _ = sendKey(t, model, key('p'))
	model = sendSettingsMsg(t, model, codexSelectedMsg{requestID: model.settings.codex.requestID, status: PeopleInferenceStatus{Configured: "example", Running: "example", PendingRestart: true}})
	screen := uv.NewScreenBuffer(80, 24)
	uv.NewStyledString(model.View().Content).Draw(screen, screen.Bounds())
	view := strings.Join(strings.Fields(screen.String()), " ")
	assert.Equal(t, 1, strings.Count(view, "msgvault daemon restart"))
	assert.Contains(t, view, "[Esc] Cancel and return")
}

func TestSettingsViewportCodexModelSelectionRemainsVisible(t *testing.T) {
	backend := &fakePeopleInferenceBackend{loads: []SettingsSnapshot{settingsFixture()}}
	model := resizeModel(t, loadedSettingsModelWithBackend(t, &backend.fakeSettingsBackend), 40, 12)
	model.settingsBackend = backend
	model, _ = sendKey(t, model, key('p'))
	var models []CodexModelChoice
	for i := range 12 {
		models = append(models, CodexModelChoice{ID: fmt.Sprintf("model-%02d", i), ReasoningEfforts: []string{"medium"}})
	}
	model = sendSettingsMsg(t, model, codexModelsLoadedMsg{requestID: model.settings.codex.requestID, models: models})
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

func TestSettingsViewportCodexLoginPollPreservesPage(t *testing.T) {
	backend := &fakePeopleInferenceBackend{loads: []SettingsSnapshot{settingsFixture()}, login: CodexDeviceLogin{SessionID: "session-1", URL: "https://example.test/device/authorization/session", Code: "ABCD-EFGH", Deadline: time.Date(2026, 9, 23, 12, 5, 0, 0, time.UTC)}}
	model := resizeModel(t, loadedSettingsModelWithBackend(t, &backend.fakeSettingsBackend), 40, 6)
	model.settingsBackend = backend
	model, start := startConfiguredCodex(t, model)
	require.NotNil(t, start)
	model = sendSettingsMsg(t, model, start())
	screen := uv.NewScreenBuffer(40, 6)
	uv.NewStyledString(model.View().Content).Draw(screen, screen.Bounds())
	require.NotContains(t, screen.String(), "ABCD-EFGH")
	model, _ = sendKey(t, model, tea.KeyPressMsg{Code: tea.KeyPgDown})
	uv.NewStyledString(model.View().Content).Draw(screen, screen.Bounds())
	require.Contains(t, screen.String(), "ABCD-EFGH")
	model = sendSettingsMsg(t, model, codexLoginPolledMsg{requestID: model.settings.codex.requestID})
	uv.NewStyledString(model.View().Content).Draw(screen, screen.Bounds())
	assert.Contains(t, screen.String(), "ABCD-EFGH")
	model = sendSettingsMsg(t, model, codexLoginPolledMsg{poll: CodexLoginPoll{Failed: true}, err: errors.New("sign-in expired"), requestID: model.settings.codex.requestID})
	uv.NewStyledString(model.View().Content).Draw(screen, screen.Bounds())
	assert.Contains(t, screen.String(), "sign-in expired")
}
