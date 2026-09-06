package tui

import (
	"context"
	"errors"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSettingsShortcutPreservesAndRestoresContentNavigation(t *testing.T) {
	assertions := assert.New(t)
	backend := &fakeSettingsBackend{loads: []SettingsSnapshot{settingsFixture()}}
	model := NewBuilder().Build()
	model.settingsBackend = backend
	model.mode = modePeople
	model.peopleState.level = peopleLevelDirectory
	model.peopleState.cursor = 4

	model, load := sendKey(t, model, key(','))
	require.NotNil(t, load)
	assertions.True(model.settings.active)
	assertions.Equal(modePeople, model.mode)
	assertions.Equal(peopleLevelDirectory, model.peopleState.level)
	assertions.Equal(4, model.peopleState.cursor)

	model = sendSettingsMsg(t, model, load())
	model, _ = sendKey(t, model, keyEsc())

	assertions.False(model.settings.active)
	assertions.Equal(modePeople, model.mode)
	assertions.Equal(peopleLevelDirectory, model.peopleState.level)
	assertions.Equal(4, model.peopleState.cursor)
}

func TestSettingsLoadGenerationSurvivesCloseAndReopen(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	model := NewBuilder().Build()
	model.settingsBackend = &fakeSettingsBackend{}

	model, firstLoad := sendKey(t, model, key(','))
	requirements.NotNil(firstLoad)
	firstRequestID := model.settings.requestID
	model, _ = sendKey(t, model, keyEsc())
	requirements.False(model.settings.active)

	model, secondLoad := sendKey(t, model, key(','))
	requirements.NotNil(secondLoad)
	secondRequestID := model.settings.requestID
	requirements.Greater(secondRequestID, firstRequestID)

	stale := settingsFixture()
	stale.Fields[0].Label = "Stale setting"
	model = sendSettingsMsg(t, model, settingsLoadedMsg{
		snapshot: stale, requestID: firstRequestID,
	})
	assertions.True(model.settings.loading)
	assertions.Empty(model.settings.fields)

	fresh := settingsFixture()
	fresh.Fields[0].Label = "Fresh setting"
	model = sendSettingsMsg(t, model, settingsLoadedMsg{
		snapshot: fresh, requestID: secondRequestID,
	})
	assertions.False(model.settings.loading)
	requirements.Len(model.settings.fields, 1)
	assertions.Equal("Fresh setting", model.settings.fields[0].Label)

	value := "sql"
	model.settings.drafts["analytics.engine"] = SettingUpdate{
		Key: "analytics.engine", Value: &SettingValue{String: &value},
	}
	model, save := sendKey(t, model, tea.KeyPressMsg{Code: 's', Mod: tea.ModCtrl})
	requirements.NotNil(save)
	saveRequestID := model.settings.requestID
	requirements.Greater(saveRequestID, secondRequestID)
	model = sendSettingsMsg(t, model, settingsSavedMsg{
		snapshot: fresh, requestID: saveRequestID,
	})
	model, _ = sendKey(t, model, keyEsc())
	model, thirdLoad := sendKey(t, model, key(','))
	requirements.NotNil(thirdLoad)
	assertions.Greater(model.settings.requestID, saveRequestID)
}

func TestSettingsShortcutAppearsInEveryModeHelp(t *testing.T) {
	for _, mode := range []tuiMode{modeEmail, modeTexts, modeMeetings, modePeople} {
		model := NewBuilder().WithSize(100, 80).Build()
		model.mode = mode
		assert.Contains(t, stripANSI(model.renderHelpModal()), ",           Open Settings")
	}
}

func TestSettingsShortcutDoesNotStealCommaFromActiveTextInputs(t *testing.T) {
	t.Run("email search", func(t *testing.T) {
		model := NewBuilder().Build()
		model, _ = sendKey(t, model, key('/'))
		require.True(t, model.inlineSearchActive)

		model, _ = sendKey(t, model, key(','))

		assert.False(t, model.settings.active)
		assert.Equal(t, ",", model.searchInput.Value())
	})

	t.Run("meeting search", func(t *testing.T) {
		model := NewBuilder().Build()
		model.mode = modeMeetings
		model, _ = sendKey(t, model, key('/'))
		require.True(t, model.meetingState.searchActive)

		model, _ = sendKey(t, model, key(','))

		assert.False(t, model.settings.active)
		assert.Equal(t, ",", model.meetingState.searchInput.Value())
	})

	t.Run("people search", func(t *testing.T) {
		model := NewBuilder().Build()
		model.mode = modePeople
		model, _ = sendKey(t, model, key('/'))
		require.True(t, model.peopleState.searchActive)

		model, _ = sendKey(t, model, key(','))

		assert.False(t, model.settings.active)
		assert.Equal(t, ",", model.peopleState.searchInput.Value())
	})

	t.Run("message detail search", func(t *testing.T) {
		model := NewBuilder().Build()
		model.level = levelMessageDetail
		model, _ = sendKey(t, model, key('/'))
		require.True(t, model.detailSearchActive)

		model, _ = sendKey(t, model, key(','))

		assert.False(t, model.settings.active)
		assert.Equal(t, ",", model.detailSearchInput.Value())
	})

	t.Run("attribute editor", func(t *testing.T) {
		model := NewBuilder().Build()
		model.mode = modePeople
		model.peopleState.form = newPeopleFieldForm()

		model, _ = sendKey(t, model, key(','))

		assert.False(t, model.settings.active)
		assert.Equal(t, ",", model.peopleState.form.nameInput.Value())
	})
}

func TestSettingsRendersAboveFrozenContentTransitionAndRestoresIt(t *testing.T) {
	backend := &fakeSettingsBackend{loads: []SettingsSnapshot{settingsFixture()}}
	model := NewBuilder().WithSize(100, 24).Build()
	model.settingsBackend = backend
	model.transitionBuffer = "exact frozen content frame"

	model, load := sendKey(t, model, key(','))
	model = sendSettingsMsg(t, model, load())
	settingsView := model.View().Content
	assert.Contains(t, settingsView, "Settings")
	assert.NotContains(t, settingsView, "exact frozen content frame")

	model, _ = sendKey(t, model, keyEsc())
	assert.Equal(t, "exact frozen content frame", model.View().Content)
}

func TestSettingsOmitsBrowserOnlyFields(t *testing.T) {
	assertions := assert.New(t)
	snapshot := SettingsSnapshot{
		ETag: "etag-1",
		Groups: []SettingsGroup{
			{ID: "browser", Label: "Browser"},
			{ID: "server", Label: "Server"},
		},
		Fields: []SettingField{
			{
				Key: "web.theme", Group: "browser", Label: "Theme",
				Kind: SettingKindString, Value: stringSettingValue("dark"),
			},
			{
				Key: "server.bind_addr", Group: "server", Label: "Bind address",
				Kind: SettingKindString, Value: stringSettingValue("127.0.0.1"),
			},
		},
	}
	model := loadedSettingsModel(t, snapshot)

	view := model.renderView()
	assertions.Contains(view, "Server")
	assertions.Contains(view, "Bind address")
	assertions.NotContains(view, "web.theme")
	assertions.NotContains(view, "Theme")
	assertions.NotContains(view, "Browser")
}

func TestSettingsWideAndNarrowNavigationUseTheSameDraft(t *testing.T) {
	assertions := assert.New(t)
	snapshot := SettingsSnapshot{
		ETag: "etag-1",
		Groups: []SettingsGroup{
			{ID: "archive", Label: "Archive"},
			{ID: "search", Label: "Search"},
		},
		Fields: []SettingField{
			{
				Key: "analytics.engine", Group: "archive", Label: "Analytics engine",
				Kind: SettingKindString, Value: stringSettingValue("auto"), Options: []string{"auto", "sql"},
			},
			{
				Key: "vector.enabled", Group: "search", Label: "Semantic search",
				Kind: SettingKindBoolean, Value: boolSettingValue(false),
			},
		},
	}
	model := loadedSettingsModel(t, snapshot)
	assertions.Contains(model.renderView(), "Archive")
	assertions.Contains(model.renderView(), "Analytics engine")

	model, _ = sendKey(t, model, key('l'))
	assertions.Equal("search", model.settings.currentGroup())
	model, _ = sendKey(t, model, key(' '))
	require.True(t, model.settings.dirty())

	model = resizeModel(t, model, 60, 24)
	model.settings.narrowFields = false
	categories := model.renderView()
	assertions.Contains(categories, "Categories")
	assertions.NotContains(categories, "Semantic search")
	model, _ = sendKey(t, model, keyEnter())
	assertions.Contains(model.renderView(), "Semantic search")
	assertions.Contains(model.renderView(), "enabled")
}

func TestSettingsRendersPendingRestartAndFieldMetadata(t *testing.T) {
	assertions := assert.New(t)
	snapshot := SettingsSnapshot{
		ETag:           "etag-1",
		PendingRestart: true,
		Groups:         []SettingsGroup{{ID: "archive", Label: "Archive"}},
		Fields: []SettingField{{
			Key: "analytics.builder_threads", Group: "archive", Label: "Builder threads",
			Description: "Threads used for cache builds.", Kind: SettingKindInteger,
			Value: intSettingValue(2), ReadOnly: true, RestartRequired: true,
			Validation: SettingValidation{Hint: "Between 1 and 16."},
		}},
	}
	model := loadedSettingsModel(t, snapshot)

	view := model.renderView()
	assertions.Contains(view, "Pending restart")
	assertions.Contains(view, "Threads used for cache builds")
	assertions.Contains(view, "read-only")
	assertions.Contains(view, "restart required")
	assertions.Contains(view, "Validation: Between 1 and 16")
	model, _ = sendKey(t, model, keyEnter())
	assertions.False(model.settings.editing)
	assertions.False(model.settings.dirty())
}

func TestSettingsSelectEditorAndSavePublishTypedDraft(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	initial := SettingsSnapshot{
		ETag:   "etag-old",
		Groups: []SettingsGroup{{ID: "archive", Label: "Archive"}},
		Fields: []SettingField{{
			Key: "analytics.engine", Group: "archive", Label: "Analytics engine",
			Kind: SettingKindString, Value: stringSettingValue("auto"),
			Options: []string{"auto", "sql", "duckdb"}, RestartRequired: true,
		}},
	}
	saved := initial
	saved.ETag = "etag-new"
	saved.PendingRestart = true
	saved.Fields = append([]SettingField(nil), initial.Fields...)
	saved.Fields[0].Value = stringSettingValue("sql")
	backend := &fakeSettingsBackend{loads: []SettingsSnapshot{initial}, save: saved}
	model := loadedSettingsModelWithBackend(t, backend)

	model, _ = sendKey(t, model, keyEnter())
	assertions.True(model.settings.editing)
	model, _ = sendKey(t, model, key('l'))
	model, _ = sendKey(t, model, keyEnter())
	assertions.True(model.settings.dirty())
	assertions.Contains(model.renderView(), "sql")

	model, save := sendKey(t, model, tea.KeyPressMsg{Code: 's', Mod: tea.ModCtrl})
	requirements.NotNil(save)
	model = sendSettingsMsg(t, model, save())

	assertions.False(model.settings.dirty())
	assertions.Equal("etag-new", model.settings.etag)
	assertions.True(model.settings.pendingRestart)
	assertions.Contains(model.renderView(), "Settings saved")
	requirements.Len(backend.saves, 1)
	requirements.Len(backend.saves[0].Updates, 1)
	assertions.Equal("analytics.engine", backend.saves[0].Updates[0].Key)
	requirements.NotNil(backend.saves[0].Updates[0].Value.String)
	assertions.Equal("sql", *backend.saves[0].Updates[0].Value.String)
	assertions.Empty(backend.saves[0].Credentials)
}

func TestSettingsInvalidNumberKeepsEditorAndDraftsUnchanged(t *testing.T) {
	minimum := 1.0
	snapshot := SettingsSnapshot{
		ETag:   "etag-1",
		Groups: []SettingsGroup{{ID: "archive", Label: "Archive"}},
		Fields: []SettingField{{
			Key: "analytics.builder_threads", Group: "archive", Label: "Builder threads",
			Kind: SettingKindInteger, Validation: SettingValidation{
				Hint: "At least one thread.", Minimum: &minimum,
			},
		}},
	}
	model := loadedSettingsModel(t, snapshot)
	model, _ = sendKey(t, model, keyEnter())
	model, _ = sendKey(t, model, key('0'))
	model, _ = sendKey(t, model, keyEnter())

	assert.True(t, model.settings.editing)
	assert.False(t, model.settings.dirty())
	assert.Contains(t, model.renderView(), "Invalid value: value must be at least 1")
}

func TestSettingsSecretInputAndDraftNeverRenderSecretBytes(t *testing.T) {
	assertions := assert.New(t)
	const secret = "credential-must-never-render"
	snapshot := SettingsSnapshot{
		ETag:   "etag-1",
		Groups: []SettingsGroup{{ID: "search", Label: "Search"}},
		Fields: []SettingField{{
			Key: "vector.embeddings.api_key", Group: "search", Label: "Embedding API key",
			Kind: SettingKindSecret, CredentialID: "vector.embeddings",
			Secret: &SecretSettingState{Configured: true, Source: "environment"},
		}},
	}
	model := loadedSettingsModel(t, snapshot)
	assertions.Contains(model.settingsFooter(), "[x] Clear")

	model, _ = sendKey(t, model, keyEnter())
	for _, r := range secret {
		model, _ = sendKey(t, model, key(r))
		assertions.NotContains(model.renderView(), secret)
	}
	editingView := model.renderView()
	assertions.NotContains(editingView, secret)
	assertions.Contains(editingView, "*")

	model, _ = sendKey(t, model, keyEnter())
	committedView := model.renderView()
	assertions.NotContains(committedView, secret)
	assertions.Contains(committedView, "configured")
	assertions.Contains(committedView, "environment")
}

func TestSettingsProviderCredentialClearExplainsEnvironmentFallback(t *testing.T) {
	model := loadedSettingsModel(t, SettingsSnapshot{
		ETag:           "config-current",
		CredentialETag: "credentials-current",
		Groups:         []SettingsGroup{{ID: "search", Label: "Search"}},
		Fields: []SettingField{{
			Key: "vector.embeddings.api_key", Group: "search", Label: "Embedding API key",
			Kind: SettingKindSecret, CredentialID: "vector.embeddings",
			Secret: &SecretSettingState{Configured: true, Source: "environment"},
		}},
	})

	model, _ = sendKey(t, model, key('x'))
	field, ok := model.settings.selectedField()
	require.True(t, ok)
	display := model.secretSettingDisplay(field)
	assert.Contains(t, display, "environment may remain")
	assert.NotContains(t, display, "not configured after save")
}

func TestSettingsLegacyConfigSecretUsesConfigLaneAndCtrlSSavesActiveEditor(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	const secret = "config-secret-must-never-render"
	initial := SettingsSnapshot{
		ETag:           "config-old",
		CredentialETag: "credentials-current",
		Groups:         []SettingsGroup{{ID: "integrations", Label: "Integrations"}},
		Fields: []SettingField{{
			Key: "integrations.tasks.api_key", Group: "integrations", Label: "Tasks API key",
			Kind: SettingKindSecret, Secret: &SecretSettingState{Configured: false, Source: "none"},
		}},
	}
	saved := initial
	saved.ETag = "config-new"
	saved.Fields = append([]SettingField(nil), initial.Fields...)
	saved.Fields[0].Secret = &SecretSettingState{Configured: true, Source: "stored"}
	backend := &fakeSettingsBackend{loads: []SettingsSnapshot{initial}, save: saved}
	model := loadedSettingsModelWithBackend(t, backend)

	model, _ = sendKey(t, model, keyEnter())
	for _, r := range secret {
		model, _ = sendKey(t, model, key(r))
	}
	model, save := sendKey(t, model, tea.KeyPressMsg{Code: 's', Mod: tea.ModCtrl})
	requirements.NotNil(save)
	model = sendSettingsMsg(t, model, save())

	assertions.False(model.settings.editing)
	assertions.False(model.settings.dirty())
	assertions.NotContains(model.renderView(), secret)
	requirements.Len(backend.saves, 1)
	assertions.Equal("config-old", backend.saves[0].ConfigETag)
	assertions.Empty(backend.saves[0].Updates)
	assertions.Empty(backend.saves[0].Credentials)
	requirements.Len(backend.saves[0].ConfigSecrets, 1)
	assertions.Equal(ConfigSecretUpdate{
		Key: "integrations.tasks.api_key", Action: "set", Value: secret,
	}, backend.saves[0].ConfigSecrets[0])
}

func TestSettingsErrorRedactionHandlesOverlappingSecretDrafts(t *testing.T) {
	const (
		configSecret   = "shared-secret-prefix"
		providerSecret = "shared-secret-prefix-provider-tail"
	)
	model := loadedSettingsModel(t, SettingsSnapshot{
		Groups: []SettingsGroup{{ID: "integrations", Label: "Integrations"}},
		Fields: []SettingField{
			{
				Key: "integrations.tasks.api_key", Group: "integrations", Label: "Tasks API key",
				Kind: SettingKindSecret, Secret: &SecretSettingState{},
			},
			{
				Key: "vector.embeddings.api_key", Group: "integrations", Label: "Embedding API key",
				Kind: SettingKindSecret, CredentialID: "vector.embeddings", Secret: &SecretSettingState{},
			},
		},
	})
	model.settings.configSecretDrafts["integrations.tasks.api_key"] = ConfigSecretUpdate{
		Key: "integrations.tasks.api_key", Action: "set", Value: configSecret,
	}
	model.settings.credentialDrafts["vector.embeddings.api_key"] = CredentialUpdate{
		Key: "vector.embeddings.api_key", CredentialID: "vector.embeddings",
		Action: "set", Value: providerSecret,
	}

	redacted := model.redactSettingsSecrets("save failed: " + providerSecret + " and " + configSecret)

	assert.Equal(t, "save failed: [redacted] and [redacted]", redacted)
}

func TestSettingsEscapeRequiresDiscardConfirmationWhenDirty(t *testing.T) {
	assertions := assert.New(t)
	snapshot := SettingsSnapshot{
		ETag:   "etag-1",
		Groups: []SettingsGroup{{ID: "archive", Label: "Archive"}},
		Fields: []SettingField{{
			Key: "analytics.auto_build_cache", Group: "archive", Label: "Build cache automatically",
			Kind: SettingKindBoolean, Value: boolSettingValue(true),
		}},
	}
	model := loadedSettingsModel(t, snapshot)
	model.mode = modeMeetings
	model.meetingState.cursor = 3

	model, _ = sendKey(t, model, key(' '))
	assertions.True(model.settings.dirty())
	assertions.Contains(model.renderView(), "Settings *")

	model, _ = sendKey(t, model, keyEsc())
	assertions.True(model.settings.active)
	assertions.True(model.settings.confirmDiscard)
	assertions.Contains(model.renderView(), "Discard unsaved changes")

	model, _ = sendKey(t, model, keyEnter())
	assertions.True(model.settings.active)
	assertions.True(model.settings.confirmDiscard)

	model, _ = sendKey(t, model, key('y'))
	assertions.False(model.settings.active)
	assertions.Equal(modeMeetings, model.mode)
	assertions.Equal(3, model.meetingState.cursor)
}

func TestSettingsEscapeCannotCloseWhileSaveIsInProgress(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	model := loadedSettingsModel(t, SettingsSnapshot{
		ETag:   "etag-1",
		Groups: []SettingsGroup{{ID: "archive", Label: "Archive"}},
		Fields: []SettingField{{
			Key: "analytics.auto_build_cache", Group: "archive", Label: "Build cache automatically",
			Kind: SettingKindBoolean, Value: boolSettingValue(true),
		}},
	})
	model, _ = sendKey(t, model, key(' '))
	requirements.True(model.settings.dirty())
	model, save := sendKey(t, model, tea.KeyPressMsg{Code: 's', Mod: tea.ModCtrl})
	requirements.NotNil(save)
	requirements.True(model.settings.saving)

	model, _ = sendKey(t, model, keyEsc())

	assertions.True(model.settings.active)
	assertions.True(model.settings.saving)
	assertions.False(model.settings.confirmDiscard)
	assertions.Contains(model.renderView(), "Save in progress")
}

func TestSettingsCategoriesScrollToKeepCursorVisible(t *testing.T) {
	model := NewBuilder().WithSize(60, 10).Build()
	model.settings.groups = []SettingsGroup{
		{ID: "one", Label: "Category one"},
		{ID: "two", Label: "Category two"},
		{ID: "three", Label: "Category three"},
		{ID: "four", Label: "Category four"},
		{ID: "five", Label: "Category five"},
		{ID: "six", Label: "Category six"},
	}
	model.settings.groupCursor = 5

	view := stripANSI(model.renderSettingsCategories(30, 4))

	assert.Contains(t, view, "Category six")
	assert.NotContains(t, view, "Category one")
}

func TestSettingsConflictReloadsLatestETagAndRetainsDrafts(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	initial := SettingsSnapshot{
		ETag:   "etag-old",
		Groups: []SettingsGroup{{ID: "archive", Label: "Archive"}},
		Fields: []SettingField{{
			Key: "analytics.auto_build_cache", Group: "archive", Label: "Build cache automatically",
			Kind: SettingKindBoolean, Value: boolSettingValue(true),
		}},
	}
	latest := initial
	latest.ETag = "etag-latest"
	backend := &fakeSettingsBackend{
		loads:   []SettingsSnapshot{initial, latest},
		saveErr: &SettingsConflictError{Scope: SettingsConflictConfig, Err: errors.New("stale settings")},
	}
	model := loadedSettingsModelWithBackend(t, backend)
	model, _ = sendKey(t, model, key(' '))
	requirements.True(model.settings.dirty())

	model, save := sendKey(t, model, tea.KeyPressMsg{Code: 's', Mod: tea.ModCtrl})
	requirements.NotNil(save)
	model = sendSettingsMsg(t, model, save())

	assertions.Equal("etag-latest", model.settings.etag)
	assertions.True(model.settings.dirty(), "the user's local draft must survive conflict recovery")
	assertions.Contains(model.renderView(), "Drafts kept")
	requirements.Len(backend.saves, 1)
	assertions.Equal("etag-old", backend.saves[0].ConfigETag)
	assertions.Len(backend.saves[0].Updates, 1)
	assertions.Empty(backend.saves[0].Credentials)
}

func TestSettingsCredentialConflictUsesCredentialETagAndRetainsSecretDraft(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	const secret = "stale-credential-draft-must-not-render"
	initial := SettingsSnapshot{
		ETag:           "config-etag",
		CredentialETag: "credential-old",
		Groups:         []SettingsGroup{{ID: "search", Label: "Search"}},
		Fields: []SettingField{{
			Key: "vector.embeddings.api_key", Group: "search", Label: "Embedding API key",
			Kind: SettingKindSecret, CredentialID: "vector.embeddings",
			Secret: &SecretSettingState{Configured: false},
		}},
	}
	latest := initial
	latest.CredentialETag = "credential-latest"
	backend := &fakeSettingsBackend{
		loads: []SettingsSnapshot{initial, latest},
		saveErr: &SettingsConflictError{
			Scope: SettingsConflictCredentials,
			Err:   errors.New("credential store changed"),
		},
	}
	model := loadedSettingsModelWithBackend(t, backend)
	model, _ = sendKey(t, model, keyEnter())
	for _, r := range secret {
		model, _ = sendKey(t, model, key(r))
	}
	model, _ = sendKey(t, model, keyEnter())

	model, save := sendKey(t, model, tea.KeyPressMsg{Code: 's', Mod: tea.ModCtrl})
	requirements.NotNil(save)
	model = sendSettingsMsg(t, model, save())

	assertions.Equal("credential-latest", model.settings.credentialETag)
	assertions.True(model.settings.dirty())
	assertions.NotContains(model.renderView(), secret)
	assertions.Contains(model.renderView(), "Drafts kept")
	requirements.Len(backend.saves, 1)
	assertions.Equal("config-etag", backend.saves[0].ConfigETag)
	assertions.Equal("credential-old", backend.saves[0].CredentialETag)
	assertions.Empty(backend.saves[0].Updates, "secret writes must not use PATCH settings updates")
	requirements.Len(backend.saves[0].Credentials, 1)
	assertions.Equal("vector.embeddings.api_key", backend.saves[0].Credentials[0].Key)
	assertions.Equal("vector.embeddings", backend.saves[0].Credentials[0].CredentialID)
}

func TestSettingsPartialSaveDropsSavedDraftsAndKeepsFailedCredentialDraft(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	const secret = "partial-save-secret-must-not-render"
	initial := SettingsSnapshot{
		ETag:           "config-old",
		CredentialETag: "credential-old",
		Groups: []SettingsGroup{
			{ID: "archive", Label: "Archive"},
			{ID: "search", Label: "Search"},
		},
		Fields: []SettingField{
			{
				Key: "analytics.auto_build_cache", Group: "archive", Label: "Build cache automatically",
				Kind: SettingKindBoolean, Value: boolSettingValue(true),
			},
			{
				Key: "vector.embeddings.api_key", Group: "search", Label: "Embedding API key",
				Kind: SettingKindSecret, CredentialID: "vector.embeddings",
				Secret: &SecretSettingState{Configured: false},
			},
		},
	}
	latest := initial
	latest.ETag = "config-latest"
	latest.Fields = append([]SettingField(nil), initial.Fields...)
	latest.Fields[0].Value = boolSettingValue(false)
	backend := &fakeSettingsBackend{
		loads: []SettingsSnapshot{initial, latest},
		saveErr: &SettingsPartialSaveError{
			SavedKeys: []string{"analytics.auto_build_cache"},
			Err:       errors.New("credential store unavailable"),
		},
	}
	model := loadedSettingsModelWithBackend(t, backend)
	model, _ = sendKey(t, model, key(' '))
	model, _ = sendKey(t, model, key('l'))
	model, _ = sendKey(t, model, keyEnter())
	for _, r := range secret {
		model, _ = sendKey(t, model, key(r))
	}
	model, _ = sendKey(t, model, keyEnter())
	requirements.True(model.settings.dirty())

	model, save := sendKey(t, model, tea.KeyPressMsg{Code: 's', Mod: tea.ModCtrl})
	requirements.NotNil(save)
	model = sendSettingsMsg(t, model, save())

	assertions.NotContains(model.renderView(), secret)
	assertions.Contains(model.renderView(), "Some settings were saved")
	assertions.Contains(model.renderView(), "Remaining drafts kept")
	assertions.NotContains(model.settings.drafts, "analytics.auto_build_cache")
	assertions.Contains(model.settings.credentialDrafts, "vector.embeddings.api_key")
	requirements.Len(backend.saves, 1)
	assertions.Len(backend.saves[0].Updates, 1)
	assertions.Len(backend.saves[0].Credentials, 1)
}

type fakeSettingsBackend struct {
	loads     []SettingsSnapshot
	loadIndex int
	loadErr   error
	saveErr   error
	save      SettingsSnapshot
	saves     []fakeSettingsSave
}

type fakeSettingsSave struct {
	SettingsSaveRequest
}

func (b *fakeSettingsBackend) LoadSettings(context.Context) (SettingsSnapshot, error) {
	if b.loadErr != nil {
		return SettingsSnapshot{}, b.loadErr
	}
	if len(b.loads) == 0 {
		return SettingsSnapshot{}, nil
	}
	index := min(b.loadIndex, len(b.loads)-1)
	b.loadIndex++
	return b.loads[index], nil
}

func (b *fakeSettingsBackend) SaveSettings(
	_ context.Context,
	request SettingsSaveRequest,
) (SettingsSnapshot, error) {
	b.saves = append(b.saves, fakeSettingsSave{SettingsSaveRequest: request})
	if b.saveErr != nil {
		return SettingsSnapshot{}, b.saveErr
	}
	return b.save, nil
}

func settingsFixture() SettingsSnapshot {
	return SettingsSnapshot{
		ETag:   "etag-1",
		Groups: []SettingsGroup{{ID: "archive", Label: "Archive"}},
		Fields: []SettingField{{
			Key: "analytics.engine", Group: "archive", Label: "Analytics engine",
			Kind: SettingKindString, Value: stringSettingValue("auto"),
		}},
	}
}

func loadedSettingsModel(t *testing.T, snapshot SettingsSnapshot) Model {
	t.Helper()
	return loadedSettingsModelWithBackend(t, &fakeSettingsBackend{loads: []SettingsSnapshot{snapshot}})
}

func loadedSettingsModelWithBackend(t *testing.T, backend *fakeSettingsBackend) Model {
	t.Helper()
	model := NewBuilder().WithSize(120, 30).Build()
	model.settingsBackend = backend
	model, load := sendKey(t, model, key(','))
	require.NotNil(t, load)
	return sendSettingsMsg(t, model, load())
}

func sendSettingsMsg(t *testing.T, model Model, msg tea.Msg) Model {
	t.Helper()
	updated, _ := model.Update(msg)
	return asModel(t, updated)
}

func stringSettingValue(value string) *SettingValue {
	return &SettingValue{String: &value}
}

func boolSettingValue(value bool) *SettingValue {
	return &SettingValue{Boolean: &value}
}

func intSettingValue(value int) *SettingValue {
	return &SettingValue{Integer: &value}
}
