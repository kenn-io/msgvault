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
		ID: 9, ObjectType: store.AttributeObjectPerson, Slug: store.AttributeSlugNotes,
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
	assert.Equal(t, []int64{51}, backend.attributeRequests)
	model = sendMsg(t, model, loaded)

	view := stripANSI(model.renderView())
	assert.Contains(t, view, "Notes")
	assert.Contains(t, view, "line one")
	assert.Contains(t, view, "line two")
	assert.NotContains(t, view, "General notes about this person")

	model.width = 70
	narrow := stripANSI(model.renderView())
	assert.Contains(t, narrow, "[Overview]")
	assert.Contains(t, narrow, "Notes")
	assert.Contains(t, narrow, "line one")
	assert.Contains(t, narrow, "line two")
	for _, line := range strings.Split(narrow, "\n") {
		assert.LessOrEqual(t, ansi.StringWidth(line), 70, "line must fit narrow terminal: %q", line)
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
	assert.Nil(t, cmd)
	require.Equal(t, peopleOverlayAttributeValue, model.peopleState.form.overlay)
	assert.True(t, model.peopleState.form.longText)
	assert.Equal(t, "line one\nline two", model.peopleState.form.valueTextarea.Value())
	require.NotNil(t, model.peopleState.form.definition)
	assert.Equal(t, store.AttributeSlugNotes, model.peopleState.form.definition.Slug)

	model, cmd = sendKey(t, model, keyEnter())
	assert.Empty(t, backend.setRequests, "Enter inserts a newline in Notes")
	assert.Equal(t, "line one\nline two\n", model.peopleState.form.draft)
	model, save := sendKey(t, model, tea.KeyPressMsg{Code: 's', Mod: tea.ModCtrl})
	_ = runPeopleCommandMessage[peopleAttributeSetMsg](t, save)
	require.Len(t, backend.setRequests, 1)
	request := backend.setRequests[0]
	assert.Equal(t, int64(51), request.PersonID)
	assert.Equal(t, store.AttributeSlugNotes, request.Slug)
	require.NotNil(t, request.ExpectedValueID)
	assert.Equal(t, int64(42), *request.ExpectedValueID)
	assert.Equal(t, "line one\nline two\n", *request.Value.Text)
}

func TestPeopleNotesShortcutRoundTripsUnboundedUnicodeValue(t *testing.T) {
	definition := peopleNotesDefinition()
	longNote := strings.Repeat("界🙂", 2_100)
	require.Greater(t, len([]rune(longNote)), 4_096)
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
	assert.Nil(t, cmd)
	require.Equal(t, peopleOverlayAttributeValue, model.peopleState.form.overlay)
	assert.Zero(t, model.peopleState.form.valueTextarea.CharLimit)
	assert.Equal(t, longNote, model.peopleState.form.valueTextarea.Value())

	model, save := sendKey(t, model, tea.KeyPressMsg{Code: 's', Mod: tea.ModCtrl})
	_ = runPeopleCommandMessage[peopleAttributeSetMsg](t, save)
	require.Len(t, backend.setRequests, 1)
	request := backend.setRequests[0]
	require.NotNil(t, request.ExpectedValueID)
	assert.Equal(t, int64(42), *request.ExpectedValueID)
	require.NotNil(t, request.Value.Text)
	assert.Equal(t, longNote, *request.Value.Text)
	assert.Equal(t, len(longNote), len(*request.Value.Text))
	assert.Equal(t, len([]rune(longNote)), len([]rune(*request.Value.Text)))
}

func TestPeopleAttributeEditorEnforcesDefinitionMaxLength(t *testing.T) {
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
	assert.Nil(t, cmd)
	assert.Equal(t, 5, model.peopleState.form.valueTextarea.CharLimit)
	assert.Equal(t, "界🙂abc", model.peopleState.form.valueTextarea.Value())

	model.peopleState.form.valueTextarea.SetValue("界🙂abcd")
	assert.Equal(t, "界🙂abc", model.peopleState.form.valueTextarea.Value())
}

func TestPeopleNotesShortcutCreatesEmptyStandardValue(t *testing.T) {
	definition := peopleNotesDefinition()
	backend := &fakePeopleAttributesBackend{}
	model := peopleOverviewNotesModel(backend, true)
	model.peopleState.attributes = &peoplebrowser.Attributes{
		PersonID: 51, Groups: []peoplebrowser.AttributeGroup{{Definition: definition}},
	}
	model.peopleState.attributesLoaded = true

	model, cmd := sendKey(t, model, key('n'))
	assert.Nil(t, cmd)
	require.Equal(t, peopleOverlayAttributeValue, model.peopleState.form.overlay)
	assert.False(t, model.peopleState.form.editing)
	model, _ = sendKey(t, model, key('x'))
	model, save := sendKey(t, model, tea.KeyPressMsg{Code: 's', Mod: tea.ModCtrl})
	_ = runPeopleCommandMessage[peopleAttributeSetMsg](t, save)

	require.Len(t, backend.setRequests, 1)
	assert.Equal(t, store.AttributeSlugNotes, backend.setRequests[0].Slug)
	assert.Nil(t, backend.setRequests[0].ExpectedValueID)
	assert.Equal(t, "x", *backend.setRequests[0].Value.Text)
}

func TestPeopleNotesShortcutRequiresExplicitPromotionForObservedContact(t *testing.T) {
	backend := &fakePeopleAttributesBackend{}
	model := peopleOverviewNotesModel(backend, false)

	model, cmd := sendKey(t, model, key('n'))

	assert.Nil(t, cmd)
	assert.Equal(t, peopleOverlayNone, model.peopleState.form.overlay)
	assert.Empty(t, backend.setRequests)
	assert.Empty(t, backend.promoteRequests)
	assert.Contains(t, stripANSI(model.renderView()), "Promote with p to add notes")
}

func TestPeopleNotesShortcutPreservesDraftAcrossStaleCASReview(t *testing.T) {
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
	assert.Equal(t, "old note\nx", model.peopleState.form.draft)
	assert.True(t, model.peopleState.form.staleReloadPending)
	loaded := runPeopleCommandMessage[peopleAttributesLoadedMsg](t, reload)
	assert.Equal(t, peopleTabOverview, loaded.tab)
	model = sendMsg(t, model, loaded)

	assert.Equal(t, "old note\nx", model.peopleState.form.valueTextarea.Value())
	assert.Equal(t, "server note", model.peopleState.form.serverValue)
	assert.False(t, model.peopleState.form.staleReloadPending)
	assert.Contains(t, model.peopleState.form.notice, "Review")
	model, resubmit := sendKey(t, model, tea.KeyPressMsg{Code: 's', Mod: tea.ModCtrl})
	_ = runPeopleCommandMessage[peopleAttributeSetMsg](t, resubmit)
	require.Len(t, backend.setRequests, 2)
	require.NotNil(t, backend.setRequests[1].ExpectedValueID)
	assert.Equal(t, int64(43), *backend.setRequests[1].ExpectedValueID)
	assert.Equal(t, "old note\nx", *backend.setRequests[1].Value.Text)
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

	assert.True(t, model.peopleState.attributesLoaded, "failed refresh retains the prior cache")
	failedView := stripANSI(model.renderView())
	assert.Contains(t, failedView, "refresh unavailable")
	assert.Contains(t, failedView, "r retry")
	assert.NotContains(t, failedView, "old note",
		"the pre-save cache must not be presented as the current note")

	model, cmd := sendKey(t, model, keyTab())
	assert.Nil(t, cmd)
	assert.Equal(t, peopleTabAttributes, model.peopleState.tab)
	assert.NotContains(t, stripANSI(model.renderView()), "refresh unavailable",
		"the failed Overview refresh must not mask Attributes")
	model, cmd = sendKey(t, model, keyShiftTab())
	assert.Nil(t, cmd)
	assert.Equal(t, peopleTabOverview, model.peopleState.tab)
	roundTripView := stripANSI(model.renderView())
	assert.Contains(t, roundTripView, "refresh unavailable")
	assert.Contains(t, roundTripView, "r retry")
	assert.NotContains(t, roundTripView, "old note")

	model, retry := sendKey(t, model, key('r'))
	require.NotNil(t, retry)
	model = sendMsg(t, model, runPeopleCommandMessage[peopleAttributesLoadedMsg](t, retry))
	assert.Equal(t, []int64{51, 51}, backend.attributeRequests)
	assert.Equal(t, peopleTabOverview, model.peopleState.tab)
	refreshedView := stripANSI(model.renderView())
	assert.Contains(t, refreshedView, "new note")
	assert.NotContains(t, refreshedView, "refresh unavailable")
}

func TestPeopleNotesReviewMissingDefinitionIsUnavailableToViewAndAction(t *testing.T) {
	backend := &fakePeopleAttributesBackend{}
	model := peopleOverviewNotesModel(backend, true)
	model.peopleState.attributes = &peoplebrowser.Attributes{PersonID: 51}
	model.peopleState.attributesLoaded = true

	view := stripANSI(model.renderView())
	assert.Contains(t, view, "Notes are unavailable")
	assert.NotContains(t, view, "No notes yet")

	model, cmd := sendKey(t, model, key('n'))
	assert.Nil(t, cmd)
	assert.Equal(t, peopleOverlayNone, model.peopleState.form.overlay)
	assert.Contains(t, stripANSI(model.renderView()), "Notes are unavailable")

	model, retry := sendKey(t, model, key('r'))
	require.NotNil(t, retry)
	_ = runPeopleCommandMessage[peopleAttributesLoadedMsg](t, retry)
	assert.Equal(t, []int64{51}, backend.attributeRequests)
}
