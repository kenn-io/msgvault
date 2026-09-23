package tui

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSettingsCodexConfigConflictRequiresFreshCheck(t *testing.T) {
	assert := assert.New(t)
	model := loadedSettingsModelWithBackend(t, &fakeSettingsBackend{loads: []SettingsSnapshot{settingsFixture()}})
	model.settings.codex = codexSettingsState{
		active: true, requestID: 42, stage: "checked", profile: "codex-profile",
		disclosure: PeopleInferenceDisclosure{Profile: "codex-profile", Fingerprint: "fp-old", Text: "Old disclosure"},
	}
	model = sendSettingsMsg(t, model, codexConsentedMsg{
		requestID: 42,
		err:       &SettingsConflictError{Scope: SettingsConflictConfig, Err: errors.New("HTTP 412")},
	})
	assert.Equal("profile", model.settings.codex.stage)
	assert.Empty(model.settings.codex.disclosure.Fingerprint)
	assert.False(model.settings.codex.consented)
	assert.Contains(stripANSI(model.renderView()), "Run synthetic check")
}

func TestSettingsCodexCollectsPolicyBeforeStartingDeviceLogin(t *testing.T) {
	require := require.New(t)

	assert := assert.New(t)
	backend := &fakePeopleInferenceBackend{loads: []SettingsSnapshot{settingsFixture()}}
	model := loadedSettingsModelWithBackend(t, &backend.fakeSettingsBackend)
	model.settingsBackend = backend
	assert.Contains(stripANSI(model.renderView()), "[p] People inference")
	model, start := sendKey(t, model, key('p'))
	assert.Nil(start)
	assert.Contains(stripANSI(model.renderView()), "Codex profile setup")
	model, start = sendKey(t, model, keyEnter())
	assert.Nil(start)
	assert.Contains(stripANSI(model.renderView()), "complete the profile policy")

	model = setCodexSetupField(t, model, '1', "codex-custom")
	model, _ = sendKey(t, model, key('c'))
	model = setCodexSetupField(t, model, '2', "2025-01-01")
	model = setCodexSetupField(t, model, '3', "2025-12-31")
	model, _ = sendKey(t, model, key('n'))
	model = setCodexSetupField(t, model, '4', "operator assertion: no retention")
	model = setCodexSetupField(t, model, '5', "operator assertion: no training")
	model, start = sendKey(t, model, keyEnter())
	require.NotNil(start)
	assert.Contains(stripANSI(model.renderView()), "Starting Codex sign-in")
	model = sendSettingsMsg(t, model, start())
	assert.Equal("codex-custom", backend.startedProfile)
	assert.Contains(stripANSI(model.renderView()), "Waiting for sign-in")
}

func setCodexSetupField(t *testing.T, model Model, shortcut rune, value string) Model {
	t.Helper()
	model, _ = sendKey(t, model, key(shortcut))
	for _, r := range value {
		model, _ = sendKey(t, model, key(r))
	}
	model, _ = sendKey(t, model, keyEnter())
	return model
}

func startConfiguredCodex(t *testing.T, model Model) (Model, tea.Cmd) {
	t.Helper()
	require := require.New(t)
	model, opened := sendKey(t, model, key('p'))
	require.Nil(opened)
	model = setCodexSetupField(t, model, '1', "codex-profile")
	model, _ = sendKey(t, model, key('c'))
	model = setCodexSetupField(t, model, '2', "2025-01-01")
	model, _ = sendKey(t, model, key('n'))
	model = setCodexSetupField(t, model, '4', "operator assertion: no retention")
	model = setCodexSetupField(t, model, '5', "operator assertion: no training")
	return sendKey(t, model, keyEnter())
}

// These tests catch a login command blocking the event loop, a stale login
// response reviving a cancelled draft, and selection before check and consent.
func TestSettingsCodexJourneyRequiresCheckAndConsent(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	backend := &fakePeopleInferenceBackend{
		loads: []SettingsSnapshot{settingsFixture()},
		login: CodexDeviceLogin{DraftID: "draft-1", SessionID: "session-1", URL: "https://example.test/device", Code: "ABCD-EFGH", Deadline: time.Date(2026, 9, 23, 12, 5, 0, 0, time.UTC)},
		models: []CodexModelChoice{
			{ID: "codex-model-a", DefaultReasoningEffort: "medium", ReasoningEfforts: []string{"low", "medium"}},
			{ID: "codex-model-b", DefaultReasoningEffort: "medium", ReasoningEfforts: []string{"medium", "high"}},
		},
		check:  PeopleInferenceDisclosure{Profile: "codex-profile", Fingerprint: "fp-1", Text: "Conversation text since 2025-01-01 may be sent to Codex. Sensitive content: no."},
		status: PeopleInferenceStatus{Configured: "codex-profile", Running: "previous-profile", PendingRestart: true},
	}
	model := loadedSettingsModelWithBackend(t, &backend.fakeSettingsBackend)
	model.settingsBackend = backend

	model, start := startConfiguredCodex(t, model)
	require.NotNil(start)
	assert.Contains(model.renderView(), "Starting Codex sign-in")
	model = sendSettingsMsg(t, model, start())
	view := stripANSI(model.renderView())
	assert.Contains(view, "https://example.test/device")
	assert.Contains(view, "ABCD-EFGH")
	assert.Contains(view, "12:05 UTC")

	model, poll := sendKey(t, model, key('r'))
	require.NotNil(poll)
	backend.poll = CodexLoginPoll{Complete: true}
	updated, models := model.Update(poll())
	model = asModel(t, updated)
	require.NotNil(models)
	model = sendSettingsMsg(t, model, models())
	assert.Contains(stripANSI(model.renderView()), "codex-model-a")
	model, _ = sendKey(t, model, key('j'))
	model, _ = sendKey(t, model, key('e'))
	assert.Contains(stripANSI(model.renderView()), "high")
	model, save := sendKey(t, model, keyEnter())
	require.NotNil(save)
	model = sendSettingsMsg(t, model, save())
	assert.Equal("draft-1", backend.savedDraft)
	assert.Equal(CodexProfileRequest{
		Name: "codex-profile", Model: "codex-model-b", ReasoningEffort: "high",
		AllowedSources: []string{"conversation_text"}, SourceSince: "2025-01-01", AllowSensitive: false,
		RetentionPosture: "operator assertion: no retention", TrainingPosture: "operator assertion: no training",
	}, backend.savedRequest)

	model, selectBeforeCheck := sendKey(t, model, key('s'))
	assert.Nil(selectBeforeCheck)
	assert.Contains(stripANSI(model.renderView()), "synthetic check")
	model, check := sendKey(t, model, key('c'))
	require.NotNil(check)
	model = sendSettingsMsg(t, model, check())
	assert.Equal("codex-profile", backend.checkedProfile)
	assert.Contains(stripANSI(model.renderView()), backend.check.Text)
	model, selectBeforeConsent := sendKey(t, model, key('s'))
	assert.Nil(selectBeforeConsent)
	assert.Contains(stripANSI(model.renderView()), "consent")
	model, consent := sendKey(t, model, key('a'))
	require.NotNil(consent)
	model = sendSettingsMsg(t, model, consent())
	assert.Equal("codex-profile", backend.consentedProfile)
	assert.Equal("fp-1", backend.consentedFingerprint)
	model, selectProfile := sendKey(t, model, key('s'))
	require.NotNil(selectProfile)
	model = sendSettingsMsg(t, model, selectProfile())
	assert.Equal("codex-profile", backend.selectedProfile)
	assert.Contains(stripANSI(model.renderView()), "previous-profile")
	assert.Contains(stripANSI(model.renderView()), "Restart")
	model, refresh := sendKey(t, model, key('r'))
	require.NotNil(refresh)
	model = sendSettingsMsg(t, model, refresh())
	view = stripANSI(model.renderView())
	assert.Contains(view, "codex-profile")
	assert.Contains(view, "previous-profile")
	assert.Contains(view, "Restart")
	assert.NotContains(view, "Grant consent before selecting")
	model, cancel := sendKey(t, model, keyEsc())
	if cancel != nil {
		_ = cancel()
	}
	view = stripANSI(model.renderView())
	assert.Contains(view, "People inference: codex-profile")
	assert.Contains(view, "Running: previous-profile")
	assert.Contains(view, "Restart")
}

func TestSettingsCodexLoginWaitDoesNotBlockEscape(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	backend := &fakePeopleInferenceBackend{
		loads:       []SettingsSnapshot{settingsFixture()},
		started:     make(chan struct{}),
		startExited: make(chan struct{}),
	}
	model := loadedSettingsModelWithBackend(t, &backend.fakeSettingsBackend)
	model.settingsBackend = backend
	model, start := startConfiguredCodex(t, model)
	require.NotNil(start)
	go func() { _ = start() }()
	select {
	case <-backend.started:
	case <-time.After(3 * time.Second):
		t.Fatal("login command did not start")
	}
	model, cancel := sendKey(t, model, keyEsc())
	assert.Nil(cancel)
	assert.False(model.settings.codex.active)
	select {
	case <-backend.startExited:
	case <-time.After(3 * time.Second):
		t.Fatal("login command did not stop after Escape")
	}
}

func TestSettingsCodexEmptyModelListCanRetry(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	backend := &fakePeopleInferenceBackend{
		loads: []SettingsSnapshot{settingsFixture()},
		login: CodexDeviceLogin{DraftID: "draft-empty", SessionID: "session-empty"},
		poll:  CodexLoginPoll{Complete: true},
	}
	model := loadedSettingsModelWithBackend(t, &backend.fakeSettingsBackend)
	model.settingsBackend = backend
	model, start := startConfiguredCodex(t, model)
	model = sendSettingsMsg(t, model, start())
	model, poll := sendKey(t, model, key('r'))
	updated, models := model.Update(poll())
	model = sendSettingsMsg(t, asModel(t, updated), models())
	assert.Contains(stripANSI(model.renderView()), "No Codex models available")
	model, retry := sendKey(t, model, key('m'))
	require.NotNil(retry)
	backend.models = []CodexModelChoice{{ID: "codex-model-a", DefaultReasoningEffort: "medium", ReasoningEfforts: []string{"medium"}}}
	model = sendSettingsMsg(t, model, retry())
	assert.Contains(stripANSI(model.renderView()), "codex-model-a")
}

func TestSettingsCodexPollFailureCanRetryPendingLogin(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	backend := &fakePeopleInferenceBackend{
		loads: []SettingsSnapshot{settingsFixture()},
		login: CodexDeviceLogin{DraftID: "draft-retry", SessionID: "session-retry",
			URL: "https://example.test/device", Code: "RETRY-CODE"},
		models: []CodexModelChoice{{ID: "codex-model-a", DefaultReasoningEffort: "medium", ReasoningEfforts: []string{"medium"}}},
	}
	model := loadedSettingsModelWithBackend(t, &backend.fakeSettingsBackend)
	model.settingsBackend = backend
	model, start := startConfiguredCodex(t, model)
	require.NotNil(start)
	model = sendSettingsMsg(t, model, start())

	backend.pollErr = errors.New("temporary gateway timeout")
	model, poll := sendKey(t, model, key('r'))
	require.NotNil(poll)
	model = sendSettingsMsg(t, model, poll())
	view := stripANSI(model.renderView())
	require.Contains(view, "https://example.test/device")
	assert.Contains(view, "RETRY-CODE")
	assert.Contains(view, "temporary gateway timeout")
	assert.Contains(view, "[r] Check now")

	backend.pollErr = nil
	model, retry := sendKey(t, model, key('r'))
	require.NotNil(retry)
	updated, nextPoll := model.Update(retry())
	model = asModel(t, updated)
	assert.Equal("session-retry", backend.polledSession)
	require.NotNil(nextPoll)
	view = stripANSI(model.renderView())
	assert.Contains(view, "RETRY-CODE")
	assert.NotContains(view, "temporary gateway timeout")

	backend.poll = CodexLoginPoll{Complete: true}
	model, retry = sendKey(t, model, key('r'))
	require.NotNil(retry)
	updated, models := model.Update(retry())
	model = asModel(t, updated)
	require.NotNil(models)
	model = sendSettingsMsg(t, model, models())
	assert.Contains(stripANSI(model.renderView()), "codex-model-a")
}

func TestSettingsCodexEscapeCancelsInFlightPoll(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	backend := &fakePeopleInferenceBackend{
		loads:       []SettingsSnapshot{settingsFixture()},
		login:       CodexDeviceLogin{DraftID: "draft-poll", SessionID: "session-poll"},
		pollStarted: make(chan struct{}),
		pollExited:  make(chan struct{}),
	}
	model := loadedSettingsModelWithBackend(t, &backend.fakeSettingsBackend)
	model.settingsBackend = backend
	model, start := startConfiguredCodex(t, model)
	model = sendSettingsMsg(t, model, start())
	model, poll := sendKey(t, model, key('r'))
	require.NotNil(poll)
	go func() { _ = poll() }()
	select {
	case <-backend.pollStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("poll command did not start")
	}
	model, cancel := sendKey(t, model, keyEsc())
	require.NotNil(cancel)
	_ = cancel()
	select {
	case <-backend.pollExited:
	case <-time.After(3 * time.Second):
		t.Fatal("poll command did not stop after Escape")
	}
	assert.False(model.settings.codex.active)
}

func TestSettingsCodexEscapeCancelsAndIgnoresLateLogin(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	backend := &fakePeopleInferenceBackend{loads: []SettingsSnapshot{settingsFixture()}, login: CodexDeviceLogin{DraftID: "draft-2", SessionID: "session-2"}}
	model := loadedSettingsModelWithBackend(t, &backend.fakeSettingsBackend)
	model.settingsBackend = backend
	model, start := startConfiguredCodex(t, model)
	model = sendSettingsMsg(t, model, start())
	model, cancel := sendKey(t, model, keyEsc())
	require.NotNil(cancel)
	assert.NotContains(stripANSI(model.renderView()), "session-2")
	_ = cancel()
	assert.Equal("session-2", backend.cancelled)
	model = sendSettingsMsg(t, model, codexLoginStartedMsg{login: backend.login, requestID: 1})
	assert.NotContains(stripANSI(model.renderView()), "Starting Codex sign-in")
	updated, lateCancel := model.Update(codexLoginStartedMsg{login: CodexDeviceLogin{SessionID: "late-session"}, requestID: 1})
	asModel(t, updated)
	require.NotNil(lateCancel)
	_ = lateCancel()
	assert.Equal("late-session", backend.cancelled)
}

func TestSettingsCodexNarrowViewAndCtrlCCancel(t *testing.T) {
	require := require.New(t)

	assert := assert.New(t)
	longURL := "https://example.test/device/" + strings.Repeat("a", 60)
	backend := &fakePeopleInferenceBackend{loads: []SettingsSnapshot{settingsFixture()}, login: CodexDeviceLogin{DraftID: "draft-3", SessionID: "session-3", URL: longURL, Code: "ABCD-EFGH"}}
	model := loadedSettingsModelWithBackend(t, &backend.fakeSettingsBackend)
	model.settingsBackend = backend
	model = resizeModel(t, model, 42, 20)
	assert.Contains(stripANSI(model.renderView()), "People inference")
	model, start := startConfiguredCodex(t, model)
	model = sendSettingsMsg(t, model, start())
	view := stripANSI(model.renderView())
	assert.Contains(strings.ReplaceAll(view, "\n", ""), longURL)
	assert.Contains(view, "ABCD-EFGH")
	_, cancel := sendKey(t, model, tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	require.NotNil(cancel)
	_ = cancel()
	assert.Equal("session-3", backend.cancelled)
	assert.False(backend.selected)
}

func TestSettingsHidesPeopleInferenceUntilBackendIsWired(t *testing.T) {
	assert := assert.New(t)

	model := loadedSettingsModel(t, settingsFixture())
	assert.NotContains(stripANSI(model.renderView()), "People inference")
	model, command := sendKey(t, model, key('p'))
	assert.Nil(command)
	assert.NotContains(stripANSI(model.renderView()), "People inference")
}

type fakePeopleInferenceBackend struct {
	fakeSettingsBackend

	login                CodexDeviceLogin
	poll                 CodexLoginPoll
	pollErr              error
	polledSession        string
	models               []CodexModelChoice
	check                PeopleInferenceDisclosure
	status               PeopleInferenceStatus
	cancelled            string
	selected             bool
	savedDraft           string
	savedRequest         CodexProfileRequest
	checkedProfile       string
	consentedProfile     string
	consentedFingerprint string
	selectedProfile      string
	startedProfile       string
	started              chan struct{}
	startExited          chan struct{}
	pollStarted          chan struct{}
	pollExited           chan struct{}
}

func (b *fakePeopleInferenceBackend) StartCodexLogin(ctx context.Context, name string) (CodexDeviceLogin, error) {
	b.startedProfile = name
	if b.started != nil {
		close(b.started)
		<-ctx.Done()
		close(b.startExited)
		return CodexDeviceLogin{}, ctx.Err()
	}
	return b.login, nil
}
func (b *fakePeopleInferenceBackend) PollCodexLogin(ctx context.Context, session string) (CodexLoginPoll, error) {
	b.polledSession = session
	if b.pollStarted != nil {
		close(b.pollStarted)
		<-ctx.Done()
		close(b.pollExited)
		return CodexLoginPoll{}, ctx.Err()
	}
	return b.poll, b.pollErr
}
func (b *fakePeopleInferenceBackend) CancelCodexLogin(_ context.Context, session string) error {
	b.cancelled = session
	return nil
}
func (b *fakePeopleInferenceBackend) ListCodexModels(context.Context, string) ([]CodexModelChoice, error) {
	return b.models, nil
}
func (b *fakePeopleInferenceBackend) SaveCodexProfile(_ context.Context, draft string, request CodexProfileRequest) (string, error) {
	b.savedDraft, b.savedRequest = draft, request
	return "codex-profile", nil
}
func (b *fakePeopleInferenceBackend) CheckCodexProfile(_ context.Context, profile string) (PeopleInferenceDisclosure, error) {
	b.checkedProfile = profile
	return b.check, nil
}
func (b *fakePeopleInferenceBackend) ConsentCodexProfile(_ context.Context, profile, fingerprint string) error {
	b.consentedProfile, b.consentedFingerprint = profile, fingerprint
	return nil
}
func (b *fakePeopleInferenceBackend) SelectCodexProfile(_ context.Context, profile string) error {
	b.selected = true
	b.selectedProfile = profile
	return nil
}
func (b *fakePeopleInferenceBackend) LoadPeopleInferenceStatus(context.Context) (PeopleInferenceStatus, error) {
	return b.status, nil
}
