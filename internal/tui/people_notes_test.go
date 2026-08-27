package tui

import (
	"errors"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/peoplebrowser"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/store"
)

func peopleNotesDefinition() store.AttributeDefinition {
	return store.AttributeDefinition{
		ID: 9, UniversalID: store.AttributeUniversalIDNotes,
		ObjectType: store.AttributeObjectPerson, Slug: store.AttributeSlugNotes,
		Label: "Notes", ValueType: store.AttributeValueText,
		FieldType: store.AttributeFieldTextarea, Cardinality: store.AttributeCardinalitySingle,
		Ownership: store.AttributeOwnershipSystem, UIEditable: true, APIMutable: true,
		IsSensitive: true, IsActive: true,
	}
}

func peopleOverviewNotesModel(backend peoplebrowser.Backend, profile bool) Model {
	contact := testPerson(7, "Notes Person")
	if profile {
		contact.Profile = &query.PersonProfile{ID: 51, Revision: 2}
	}
	model := peopleModel(backend)
	model.mode = modePeople
	model.presentationGeneration = 8
	model.peopleState.level = peopleLevelContact
	model.peopleState.tab = peopleTabOverview
	model.peopleState.participantID = contact.ID
	model.peopleState.contact = &contact
	return model
}

func TestPeopleOverviewNotesLoadsAndRendersMultilineValue(t *testing.T) {
	assert := assert.New(t)
	definition := peopleNotesDefinition()
	backend := &fakePeopleAttributesBackend{attributePages: []*peoplebrowser.Attributes{{
		PersonID: 51,
		Groups: []peoplebrowser.AttributeGroup{{
			Definition: definition,
			Current: []store.PersonAttributeValue{
				peopleValue(42, 0, definition, textPeopleValue("line one\nline two")),
			},
		}},
	}}}
	model := peopleOverviewNotesModel(backend, false)
	contact := *model.peopleState.contact
	contact.Profile = &query.PersonProfile{ID: 51, Revision: 2}
	model.peopleState.contact = nil
	model.peopleState.contactLoading = true

	updated, load := model.Update(peopleContactLoadedMsg{
		contact: &contact, requestID: model.peopleState.requestID,
		participantID: contact.ID, presentationGeneration: model.presentationGeneration,
	})
	model = asModel(t, updated)
	require.NotNil(t, load)
	loaded := runPeopleCommandMessage[peopleAttributesLoadedMsg](t, load)
	assert.Equal([]int64{51}, backend.attributeRequests)
	model = sendMsg(t, model, loaded)

	view := stripANSI(model.renderView())
	assert.Contains(view, "Notes")
	assert.Contains(view, "line one")
	assert.Contains(view, "line two")
	assert.NotContains(view, "General notes about this person")

	model.width = 70
	narrow := stripANSI(model.renderView())
	assert.Contains(narrow, "[Overview]")
	assert.Contains(narrow, "Notes")
	assert.Contains(narrow, "line one")
	assert.Contains(narrow, "line two")
	for line := range strings.SplitSeq(narrow, "\n") {
		assert.LessOrEqual(ansi.StringWidth(line), 70, "line must fit narrow terminal: %q", line)
	}
}

func TestPeopleOverviewNotesShowsEmptyAndObservedStates(t *testing.T) {
	t.Run("promoted without value", func(t *testing.T) {
		definition := peopleNotesDefinition()
		model := peopleOverviewNotesModel(&fakePeopleAttributesBackend{}, true)
		model.peopleState.attributes = &peoplebrowser.Attributes{
			PersonID: 51,
			Groups:   []peoplebrowser.AttributeGroup{{Definition: definition}},
		}
		model.peopleState.attributesLoaded = true

		view := stripANSI(model.renderView())
		assert.Contains(t, view, "Notes")
		assert.Contains(t, view, "No notes yet")
	})

	t.Run("observed", func(t *testing.T) {
		backend := &fakePeopleAttributesBackend{}
		model := peopleOverviewNotesModel(backend, false)

		view := stripANSI(model.renderView())
		assert.Contains(t, view, "Promote with p to add notes")
		assert.Equal(t, peopleOverlayNone, model.peopleState.form.overlay)
		assert.Empty(t, backend.attributeRequests)
	})
}

func TestPeopleOverviewNotesLoadFailureOffersRetry(t *testing.T) {
	backend := &fakePeopleAttributesBackend{attributeErrs: []error{errors.New("notes unavailable")}}
	model := peopleOverviewNotesModel(backend, true)
	model.peopleState.requestID++
	model.peopleState.attributesLoading = true
	load := model.loadPeopleAttributes(51, peopleTabOverview)
	model = sendMsg(t, model, runPeopleCommandMessage[peopleAttributesLoadedMsg](t, load))

	view := stripANSI(model.renderView())
	assert.Contains(t, view, "notes unavailable")
	assert.Contains(t, view, "r retry")
}

func TestPeopleNotesShortcutEditsExistingValueWithTextareaCAS(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	definition := peopleNotesDefinition()
	current := peopleValue(42, 0, definition, textPeopleValue("line one\nline two"))
	backend := &fakePeopleAttributesBackend{}
	model := peopleOverviewNotesModel(backend, true)
	model.peopleState.attributes = &peoplebrowser.Attributes{
		PersonID: 51,
		Groups: []peoplebrowser.AttributeGroup{{
			Definition: definition, Current: []store.PersonAttributeValue{current},
		}},
	}
	model.peopleState.attributesLoaded = true

	model, cmd := sendKey(t, model, key('n'))
	assert.Nil(cmd)
	require.Equal(peopleOverlayAttributeValue, model.peopleState.form.overlay)
	assert.True(model.peopleState.form.longText)
	assert.Equal("line one\nline two", model.peopleState.form.valueTextarea.Value())
	require.NotNil(model.peopleState.form.definition)
	assert.Equal(store.AttributeSlugNotes, model.peopleState.form.definition.Slug)

	model, _ = sendKey(t, model, keyEnter())
	assert.Empty(backend.setRequests, "Enter inserts a newline in Notes")
	assert.Equal("line one\nline two\n", model.peopleState.form.draft)
	_, save := sendKey(t, model, tea.KeyPressMsg{Code: 's', Mod: tea.ModCtrl})
	_ = runPeopleCommandMessage[peopleAttributeSetMsg](t, save)
	require.Len(backend.setRequests, 1)
	request := backend.setRequests[0]
	assert.Equal(int64(51), request.PersonID)
	assert.Equal(store.AttributeSlugNotes, request.Slug)
	require.NotNil(request.ExpectedValueID)
	assert.Equal(int64(42), *request.ExpectedValueID)
	assert.Equal("line one\nline two\n", *request.Value.Text)
}

func TestPeopleNotesShortcutRoundTripsUnboundedUnicodeValue(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	definition := peopleNotesDefinition()
	longNote := strings.Repeat("界🙂", 2_100)
	require.Greater(len([]rune(longNote)), 4_096)
	current := peopleValue(42, 0, definition, textPeopleValue(longNote))
	backend := &fakePeopleAttributesBackend{}
	model := peopleOverviewNotesModel(backend, true)
	model.peopleState.attributes = &peoplebrowser.Attributes{
		PersonID: 51,
		Groups: []peoplebrowser.AttributeGroup{{
			Definition: definition, Current: []store.PersonAttributeValue{current},
		}},
	}
	model.peopleState.attributesLoaded = true

	model, cmd := sendKey(t, model, key('n'))
	assert.Nil(cmd)
	require.Equal(peopleOverlayAttributeValue, model.peopleState.form.overlay)
	assert.Zero(model.peopleState.form.valueTextarea.CharLimit)
	assert.Equal(longNote, model.peopleState.form.valueTextarea.Value())

	_, save := sendKey(t, model, tea.KeyPressMsg{Code: 's', Mod: tea.ModCtrl})
	_ = runPeopleCommandMessage[peopleAttributeSetMsg](t, save)
	require.Len(backend.setRequests, 1)
	request := backend.setRequests[0]
	require.NotNil(request.ExpectedValueID)
	assert.Equal(int64(42), *request.ExpectedValueID)
	require.NotNil(request.Value.Text)
	assert.Equal(longNote, *request.Value.Text)
	assert.Len(*request.Value.Text, len(longNote))
	assert.Len([]rune(*request.Value.Text), len([]rune(longNote)))
}

func TestPeopleAttributeEditorEnforcesDefinitionMaxLength(t *testing.T) {
	assert := assert.New(t)
	definition := peopleNotesDefinition()
	definition.Options = &store.AttributeOptions{MaxLength: 5}
	current := peopleValue(42, 0, definition, textPeopleValue("界🙂abc"))
	model := peopleOverviewNotesModel(&fakePeopleAttributesBackend{}, true)
	model.peopleState.attributes = &peoplebrowser.Attributes{
		PersonID: 51,
		Groups: []peoplebrowser.AttributeGroup{{
			Definition: definition, Current: []store.PersonAttributeValue{current},
		}},
	}
	model.peopleState.attributesLoaded = true

	model, cmd := sendKey(t, model, key('n'))
	assert.Nil(cmd)
	assert.Equal(5, model.peopleState.form.valueTextarea.CharLimit)
	assert.Equal("界🙂abc", model.peopleState.form.valueTextarea.Value())

	model.peopleState.form.valueTextarea.SetValue("界🙂abcd")
	assert.Equal("界🙂abc", model.peopleState.form.valueTextarea.Value())
}

func TestPeopleNotesShortcutCreatesEmptyStandardValue(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	definition := peopleNotesDefinition()
	definition.Slug = "notes_system"
	backend := &fakePeopleAttributesBackend{}
	model := peopleOverviewNotesModel(backend, true)
	model.peopleState.attributes = &peoplebrowser.Attributes{
		PersonID: 51, Groups: []peoplebrowser.AttributeGroup{{Definition: definition}},
	}
	model.peopleState.attributesLoaded = true

	model, cmd := sendKey(t, model, key('n'))
	assert.Nil(cmd)
	require.Equal(peopleOverlayAttributeValue, model.peopleState.form.overlay)
	assert.False(model.peopleState.form.editing)
	model, _ = sendKey(t, model, key('x'))
	_, save := sendKey(t, model, tea.KeyPressMsg{Code: 's', Mod: tea.ModCtrl})
	_ = runPeopleCommandMessage[peopleAttributeSetMsg](t, save)

	require.Len(backend.setRequests, 1)
	assert.Equal("notes_system", backend.setRequests[0].Slug)
	assert.Nil(backend.setRequests[0].ExpectedValueID)
	assert.Equal("x", *backend.setRequests[0].Value.Text)
}

func TestPeopleNotesShortcutRequiresExplicitPromotionForObservedContact(t *testing.T) {
	assert := assert.New(t)
	backend := &fakePeopleAttributesBackend{}
	model := peopleOverviewNotesModel(backend, false)

	model, cmd := sendKey(t, model, key('n'))

	assert.Nil(cmd)
	assert.Equal(peopleOverlayNone, model.peopleState.form.overlay)
	assert.Empty(backend.setRequests)
	assert.Empty(backend.promoteRequests)
	assert.Contains(stripANSI(model.renderView()), "Promote with p to add notes")
}

func TestPeopleNotesShortcutPreservesDraftAcrossStaleCASReview(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	definition := peopleNotesDefinition()
	oldValue := peopleValue(40, 0, definition, textPeopleValue("old note"))
	currentValue := peopleValue(43, 0, definition, textPeopleValue("server note"))
	backend := &fakePeopleAttributesBackend{
		setAttributeErrs: []error{peoplebrowser.StaleValueError{CurrentValueID: 42}},
		attributePages: []*peoplebrowser.Attributes{{
			PersonID: 51, Groups: []peoplebrowser.AttributeGroup{{
				Definition: definition, Current: []store.PersonAttributeValue{currentValue},
			}},
		}},
	}
	model := peopleOverviewNotesModel(backend, true)
	model.peopleState.attributes = &peoplebrowser.Attributes{
		PersonID: 51, Groups: []peoplebrowser.AttributeGroup{{
			Definition: definition, Current: []store.PersonAttributeValue{oldValue},
		}},
	}
	model.peopleState.attributesLoaded = true
	model, _ = sendKey(t, model, key('n'))
	model, _ = sendKey(t, model, keyEnter())
	model, _ = sendKey(t, model, key('x'))

	model, save := sendKey(t, model, tea.KeyPressMsg{Code: 's', Mod: tea.ModCtrl})
	updated, reload := model.Update(runPeopleCommandMessage[peopleAttributeSetMsg](t, save))
	model = asModel(t, updated)
	assert.Equal("old note\nx", model.peopleState.form.draft)
	assert.True(model.peopleState.form.staleReloadPending)
	loaded := runPeopleCommandMessage[peopleAttributesLoadedMsg](t, reload)
	assert.Equal(peopleTabOverview, loaded.tab)
	model = sendMsg(t, model, loaded)

	assert.Equal("old note\nx", model.peopleState.form.valueTextarea.Value())
	assert.Equal("server note", model.peopleState.form.serverValue)
	assert.False(model.peopleState.form.staleReloadPending)
	assert.Contains(model.peopleState.form.notice, "Review")
	_, resubmit := sendKey(t, model, tea.KeyPressMsg{Code: 's', Mod: tea.ModCtrl})
	_ = runPeopleCommandMessage[peopleAttributeSetMsg](t, resubmit)
	require.Len(backend.setRequests, 2)
	require.NotNil(backend.setRequests[1].ExpectedValueID)
	assert.Equal(int64(43), *backend.setRequests[1].ExpectedValueID)
	assert.Equal("old note\nx", *backend.setRequests[1].Value.Text)
}

func TestPeopleNotesShortcutAbandonsStaleLoadAndSaveOwners(t *testing.T) {
	definition := peopleNotesDefinition()
	page := &peoplebrowser.Attributes{
		PersonID: 51, Groups: []peoplebrowser.AttributeGroup{{Definition: definition}},
	}

	t.Run("tab abandons load", func(t *testing.T) {
		backend := &fakePeopleAttributesBackend{attributePages: []*peoplebrowser.Attributes{page}}
		model := peopleOverviewNotesModel(backend, true)
		model.peopleState.requestID++
		model.peopleState.attributesLoading = true
		pending := model.loadPeopleAttributes(51, peopleTabOverview)
		stale := runPeopleCommandMessage[peopleAttributesLoadedMsg](t, pending)

		model, _ = sendKey(t, model, keyTab())
		assert.Equal(t, peopleTabAttributes, model.peopleState.tab)
		assert.True(t, model.peopleState.attributesLoading,
			"Attributes takes ownership with a newly tagged shared-cache load")
		model = sendMsg(t, model, stale)
		assert.False(t, model.peopleState.attributesLoaded)
	})

	t.Run("contact abandons load", func(t *testing.T) {
		backend := &fakePeopleAttributesBackend{attributePages: []*peoplebrowser.Attributes{page}}
		model := peopleOverviewNotesModel(backend, true)
		model.peopleState.breadcrumbs = []peopleNavSnapshot{{level: peopleLevelDirectory}}
		model.peopleState.requestID++
		model.peopleState.attributesLoading = true
		pending := model.loadPeopleAttributes(51, peopleTabOverview)
		stale := runPeopleCommandMessage[peopleAttributesLoadedMsg](t, pending)

		model, _ = sendKey(t, model, keyEsc())
		assert.Equal(t, peopleLevelDirectory, model.peopleState.level)
		assert.False(t, model.peopleState.attributesLoading)
		model = sendMsg(t, model, stale)
		assert.False(t, model.peopleState.attributesLoaded)
	})

	t.Run("mode abandons load", func(t *testing.T) {
		backend := &fakePeopleAttributesBackend{attributePages: []*peoplebrowser.Attributes{page}}
		model := peopleOverviewNotesModel(backend, true)
		model.peopleState.requestID++
		model.peopleState.attributesLoading = true
		pending := model.loadPeopleAttributes(51, peopleTabOverview)
		stale := runPeopleCommandMessage[peopleAttributesLoadedMsg](t, pending)

		model, _ = sendKey(t, model, key('m'))
		assert.NotEqual(t, modePeople, model.mode)
		assert.False(t, model.peopleState.attributesLoading)
		model = sendMsg(t, model, stale)
		assert.False(t, model.peopleState.attributesLoaded)
	})

	t.Run("tab abandons save after editor closes", func(t *testing.T) {
		backend := &fakePeopleAttributesBackend{}
		model := peopleOverviewNotesModel(backend, true)
		model.peopleState.attributes = page
		model.peopleState.attributesLoaded = true
		model, _ = sendKey(t, model, key('n'))
		model, _ = sendKey(t, model, key('x'))
		model, save := sendKey(t, model, tea.KeyPressMsg{Code: 's', Mod: tea.ModCtrl})
		stale := runPeopleCommandMessage[peopleAttributeSetMsg](t, save)

		model, _ = sendKey(t, model, keyEsc())
		model, _ = sendKey(t, model, keyTab())
		assert.Equal(t, peopleTabAttributes, model.peopleState.tab)
		assert.Equal(t, peopleOverlayNone, model.peopleState.form.overlay)
		model = sendMsg(t, model, stale)
		assert.Equal(t, peopleOverlayNone, model.peopleState.form.overlay)
	})
}

func TestPeopleNotesShortcutDoesNotStealAttributesNewFieldBinding(t *testing.T) {
	model := peopleAttributesModel(&fakePeopleAttributesBackend{})

	model, _ = sendKey(t, model, key('n'))

	assert.Equal(t, peopleOverlayNewField, model.peopleState.form.overlay)
}

func TestPeopleNotesReviewFailedPostSaveRefreshSurvivesTabRoundTrip(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	definition := peopleNotesDefinition()
	oldValue := peopleValue(42, 0, definition, textPeopleValue("old note"))
	newValue := peopleValue(43, 0, definition, textPeopleValue("new note"))
	backend := &fakePeopleAttributesBackend{
		attributeErrs: []error{errors.New("refresh unavailable")},
		attributePages: []*peoplebrowser.Attributes{{
			PersonID: 51, Groups: []peoplebrowser.AttributeGroup{{
				Definition: definition, Current: []store.PersonAttributeValue{newValue},
			}},
		}},
	}
	model := peopleOverviewNotesModel(backend, true)
	model.peopleState.attributes = &peoplebrowser.Attributes{
		PersonID: 51, Groups: []peoplebrowser.AttributeGroup{{
			Definition: definition, Current: []store.PersonAttributeValue{oldValue},
		}},
	}
	model.peopleState.attributesLoaded = true
	model, _ = sendKey(t, model, key('n'))
	model.peopleState.form.valueTextarea.SetValue("new note")

	model, save := sendKey(t, model, tea.KeyPressMsg{Code: 's', Mod: tea.ModCtrl})
	updated, reload := model.Update(runPeopleCommandMessage[peopleAttributeSetMsg](t, save))
	model = asModel(t, updated)
	model = sendMsg(t, model, runPeopleCommandMessage[peopleAttributesLoadedMsg](t, reload))

	assert.True(model.peopleState.attributesLoaded, "failed refresh retains the prior cache")
	failedView := stripANSI(model.renderView())
	assert.Contains(failedView, "refresh unavailable")
	assert.Contains(failedView, "r retry")
	assert.NotContains(failedView, "old note",
		"the pre-save cache must not be presented as the current note")

	model, cmd := sendKey(t, model, keyTab())
	assert.Nil(cmd)
	assert.Equal(peopleTabAttributes, model.peopleState.tab)
	assert.NotContains(stripANSI(model.renderView()), "refresh unavailable",
		"the failed Overview refresh must not mask Attributes")
	model, cmd = sendKey(t, model, keyShiftTab())
	require.NotNil(cmd, "Overview starts its independent relationship calendar load")
	assert.Equal(peopleTabOverview, model.peopleState.tab)
	roundTripView := stripANSI(model.renderView())
	assert.Contains(roundTripView, "refresh unavailable")
	assert.Contains(roundTripView, "r retry")
	assert.NotContains(roundTripView, "old note")

	model, retry := sendKey(t, model, key('r'))
	require.NotNil(retry)
	model = sendMsg(t, model, runPeopleCommandMessage[peopleAttributesLoadedMsg](t, retry))
	assert.Equal([]int64{51, 51}, backend.attributeRequests)
	assert.Equal(peopleTabOverview, model.peopleState.tab)
	refreshedView := stripANSI(model.renderView())
	assert.Contains(refreshedView, "new note")
	assert.NotContains(refreshedView, "refresh unavailable")
}

func TestPeopleNotesReviewMissingDefinitionIsUnavailableToViewAndAction(t *testing.T) {
	assert := assert.New(t)
	backend := &fakePeopleAttributesBackend{}
	model := peopleOverviewNotesModel(backend, true)
	model.peopleState.attributes = &peoplebrowser.Attributes{PersonID: 51}
	model.peopleState.attributesLoaded = true

	view := stripANSI(model.renderView())
	assert.Contains(view, "Notes are unavailable")
	assert.NotContains(view, "No notes yet")

	model, cmd := sendKey(t, model, key('n'))
	assert.Nil(cmd)
	assert.Equal(peopleOverlayNone, model.peopleState.form.overlay)
	assert.Contains(stripANSI(model.renderView()), "Notes are unavailable")

	_, retry := sendKey(t, model, key('r'))
	require.NotNil(t, retry)
	_ = runPeopleCommandMessage[peopleAttributesLoadedMsg](t, retry)
	assert.Equal([]int64{51}, backend.attributeRequests)
}
