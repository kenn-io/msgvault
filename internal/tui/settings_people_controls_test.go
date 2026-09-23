package tui

import (
	"context"
	"errors"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSettingsPeopleControlsDisableRequiresConfirmation(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	backend := &fakePeopleInferenceControlBackend{
		loads: []SettingsSnapshot{settingsFixture()},
		status: PeopleInferenceStatus{Configured: "codex-profile", ConfiguredFingerprint: "fp-1", ConfiguredEnabled: true,
			Running: "codex-profile", RunningFingerprint: "fp-1", RunningEnabled: true},
	}
	model := loadedSettingsModelWithBackend(t, &backend.fakeSettingsBackend)
	model.settingsBackend = backend
	assert.Contains(stripANSI(model.renderView()), "[i] People inference controls")
	assert.NotContains(stripANSI(model.renderView()), "[p] People inference")
	model, noOnboarding := sendKey(t, model, key('p'))
	assert.Nil(noOnboarding)

	model, load := sendKey(t, model, key('i'))
	require.NotNil(load)
	model = sendSettingsMsg(t, model, load())
	assert.Contains(stripANSI(model.renderView()), "fp-1")
	model, confirm := sendKey(t, model, key('d'))
	assert.Nil(confirm)
	assert.Contains(stripANSI(model.renderView()), "Disable people inference")
	assert.Contains(stripANSI(model.renderView()), "fp-1")
	assert.Empty(backend.disabledFingerprint)
	model, cancel := sendKey(t, model, key('n'))
	assert.Nil(cancel)
	assert.Empty(backend.disabledFingerprint)
	model, _ = sendKey(t, model, key('d'))
	model, disable := sendKey(t, model, key('y'))
	require.NotNil(disable)
	model = sendSettingsMsg(t, model, disable())
	assert.Equal("fp-1", backend.disabledFingerprint)
	assert.False(model.settings.peopleControls.status.ConfiguredEnabled)
	assert.Contains(stripANSI(model.renderView()), "disabled")
	assert.Contains(stripANSI(model.renderView()), "Restart")
	model, back := sendKey(t, model, keyEsc())
	assert.Nil(back)
	assert.Contains(stripANSI(model.renderView()), "People inference: codex-profile")
}

func TestSettingsPeopleControlsRevokeUsesReviewedFingerprint(t *testing.T) {
	require := require.New(t)

	assert := assert.New(t)
	backend := &fakePeopleInferenceControlBackend{
		loads:  []SettingsSnapshot{settingsFixture()},
		status: PeopleInferenceStatus{Configured: "router", ConfiguredFingerprint: "fp-router", ConfiguredEnabled: true},
	}
	model := loadedSettingsModelWithBackend(t, &backend.fakeSettingsBackend)
	model.settingsBackend = backend
	model, load := sendKey(t, model, key('i'))
	model = sendSettingsMsg(t, model, load())
	model, _ = sendKey(t, model, key('v'))
	assert.Contains(stripANSI(model.renderView()), "Revoke consent")
	assert.Contains(stripANSI(model.renderView()), "fp-router")
	model, revoke := sendKey(t, model, key('y'))
	require.NotNil(revoke)
	model = sendSettingsMsg(t, model, revoke())
	assert.Equal("router", backend.revokedProfile)
	assert.Equal("fp-router", backend.revokedFingerprint)
	assert.Contains(stripANSI(model.renderView()), "Consent revoked")
}

func TestSettingsPeopleControlsRemoveRequiresDisabledProfileAndConfirmation(t *testing.T) {
	require := require.New(t)

	assert := assert.New(t)
	backend := &fakePeopleInferenceControlBackend{
		loads:  []SettingsSnapshot{settingsFixture()},
		status: PeopleInferenceStatus{Configured: "router", ConfiguredFingerprint: "fp-router", ConfiguredEnabled: true},
	}
	model := loadedSettingsModelWithBackend(t, &backend.fakeSettingsBackend)
	model.settingsBackend = backend
	model, load := sendKey(t, model, key('i'))
	model = sendSettingsMsg(t, model, load())
	model, blocked := sendKey(t, model, key('x'))
	assert.Nil(blocked)
	assert.Empty(model.settings.peopleControls.confirm)
	assert.Empty(backend.removedProfile)

	backend.status.ConfiguredEnabled = false
	model, refresh := sendKey(t, model, key('r'))
	model = sendSettingsMsg(t, model, refresh())
	model, confirm := sendKey(t, model, key('x'))
	assert.Nil(confirm)
	view := stripANSI(model.renderView())
	assert.Contains(view, "Remove profile router")
	assert.Contains(view, "fp-router")
	assert.Contains(view, "stored credential")
	model, _ = sendKey(t, model, key('n'))
	assert.Empty(backend.removedProfile)
	model, _ = sendKey(t, model, key('x'))
	model, remove := sendKey(t, model, key('y'))
	require.NotNil(remove)
	model = sendSettingsMsg(t, model, remove())
	assert.Equal("router", backend.removedProfile)
	assert.Equal("fp-router", backend.removedFingerprint)
	assert.Contains(stripANSI(model.renderView()), "Profile removed")
	model, _ = sendKey(t, model, keyEsc())
	assert.Contains(stripANSI(model.renderView()), "People inference: backup")
}

func TestSettingsPeopleControlsReloadAfterConfigConflict(t *testing.T) {
	require := require.New(t)

	assert := assert.New(t)
	backend := &fakePeopleInferenceControlBackend{
		loads:      []SettingsSnapshot{settingsFixture()},
		status:     PeopleInferenceStatus{Configured: "router", ConfiguredFingerprint: "fp-old", ConfiguredEnabled: true},
		disableErr: &SettingsConflictError{Scope: SettingsConflictConfig, Err: errors.New("HTTP 412")},
	}
	model := loadedSettingsModelWithBackend(t, &backend.fakeSettingsBackend)
	model.settingsBackend = backend
	model, load := sendKey(t, model, key('i'))
	model = sendSettingsMsg(t, model, load())
	model, _ = sendKey(t, model, key('d'))
	model, disable := sendKey(t, model, key('y'))
	backend.status.ConfiguredFingerprint = "fp-new"
	updated, reload := model.Update(disable())
	model = asModel(t, updated)
	require.NotNil(reload)
	assert.Nil(model.settings.peopleInferenceStatus)
	model, blocked := sendKey(t, model, keyEsc())
	assert.Nil(blocked)
	assert.True(model.settings.peopleControls.active)
	model, blocked = sendKey(t, model, tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	assert.Nil(blocked)
	assert.False(model.quitting)
	model = sendSettingsMsg(t, model, reload())
	assert.Equal("fp-old", backend.disabledFingerprint)
	assert.Contains(stripANSI(model.renderView()), "fp-new")
	assert.Contains(stripANSI(model.renderView()), "changed")
	assert.Empty(model.settings.peopleControls.confirm)
	model, back := sendKey(t, model, keyEsc())
	assert.Nil(back)
	assert.Contains(stripANSI(model.renderView()), "fp-new")
}

func TestSettingsPeopleControlsWaitsForPendingMutationBeforeExit(t *testing.T) {
	for _, operation := range []string{"disable", "revoke", "remove"} {
		t.Run(operation, func(t *testing.T) {
			require := require.New(t)

			assert := assert.New(t)
			backend := &fakePeopleInferenceControlBackend{
				loads:  []SettingsSnapshot{settingsFixture()},
				status: PeopleInferenceStatus{Configured: "router", ConfiguredFingerprint: "fp-1", ConfiguredEnabled: true},
			}
			if operation == "remove" {
				backend.status.ConfiguredEnabled = false
			}
			model := loadedSettingsModelWithBackend(t, &backend.fakeSettingsBackend)
			model.settingsBackend = backend
			model, load := sendKey(t, model, key('i'))
			model = sendSettingsMsg(t, model, load())
			keyCode := 'd'
			switch operation {
			case "revoke":
				keyCode = 'v'
			case "remove":
				keyCode = 'x'
			}
			model, _ = sendKey(t, model, key(keyCode))
			model, mutate := sendKey(t, model, key('y'))
			require.NotNil(mutate)
			model, blocked := sendKey(t, model, keyEsc())
			assert.Nil(blocked)
			assert.True(model.settings.peopleControls.active)
			assert.Contains(stripANSI(model.renderView()), "wait for the result")
			model, blocked = sendKey(t, model, tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
			assert.Nil(blocked)
			assert.False(model.quitting)
			model = sendSettingsMsg(t, model, mutate())
			assert.NotNil(model.settings.peopleInferenceStatus)
			model, back := sendKey(t, model, keyEsc())
			assert.Nil(back)
			assert.False(model.settings.peopleControls.active)
		})
	}
}

func TestSettingsPeopleControlsClearsCacheWhenConflictRefreshFails(t *testing.T) {
	require := require.New(t)

	assert := assert.New(t)
	backend := &fakePeopleInferenceControlBackend{
		loads:      []SettingsSnapshot{settingsFixture()},
		status:     PeopleInferenceStatus{Configured: "router", ConfiguredFingerprint: "fp-old", ConfiguredEnabled: true},
		disableErr: &SettingsConflictError{Scope: SettingsConflictConfig, Err: errors.New("HTTP 412")},
		loadErrors: []error{nil, errors.New("daemon unavailable")},
	}
	model := loadedSettingsModelWithBackend(t, &backend.fakeSettingsBackend)
	model.settingsBackend = backend
	model, load := sendKey(t, model, key('i'))
	model = sendSettingsMsg(t, model, load())
	model, _ = sendKey(t, model, key('d'))
	model, disable := sendKey(t, model, key('y'))
	updated, reload := model.Update(disable())
	model = asModel(t, updated)
	require.NotNil(reload)
	model = sendSettingsMsg(t, model, reload())
	assert.Nil(model.settings.peopleInferenceStatus)
	model, back := sendKey(t, model, keyEsc())
	assert.Nil(back)
	assert.NotContains(stripANSI(model.renderView()), "People inference: router")
	assert.Contains(stripANSI(model.renderView()), "status could not be refreshed")
}

type fakePeopleInferenceControlBackend struct {
	fakeSettingsBackend

	status              PeopleInferenceStatus
	disabledFingerprint string
	revokedProfile      string
	revokedFingerprint  string
	removedProfile      string
	removedFingerprint  string
	disableErr          error
	loadErrors          []error
	loadCalls           int
}

func (b *fakePeopleInferenceControlBackend) LoadPeopleInferenceStatus(context.Context) (PeopleInferenceStatus, error) {
	index := b.loadCalls
	b.loadCalls++
	if index < len(b.loadErrors) && b.loadErrors[index] != nil {
		return PeopleInferenceStatus{}, b.loadErrors[index]
	}
	return b.status, nil
}

func (b *fakePeopleInferenceControlBackend) DisablePeopleInference(_ context.Context, fingerprint string) (PeopleInferenceStatus, error) {
	b.disabledFingerprint = fingerprint
	if b.disableErr != nil {
		return PeopleInferenceStatus{}, b.disableErr
	}
	b.status.ConfiguredEnabled = false
	b.status.PendingRestart = true
	return b.status, nil
}

func (b *fakePeopleInferenceControlBackend) RevokePeopleInferenceConsent(_ context.Context, profile, fingerprint string) (PeopleInferenceStatus, error) {
	b.revokedProfile, b.revokedFingerprint = profile, fingerprint
	return b.status, nil
}

func (b *fakePeopleInferenceControlBackend) RemovePeopleInferenceProfile(_ context.Context, profile, fingerprint string) (PeopleInferenceStatus, error) {
	b.removedProfile, b.removedFingerprint = profile, fingerprint
	b.status.Configured = "backup"
	b.status.ConfiguredFingerprint = "fp-backup"
	b.status.PendingRestart = true
	return b.status, nil
}
