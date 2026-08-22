package tui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/peoplebrowser"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/store"
)

type fakePeopleAttributesBackend struct {
	peoplebrowser.Backend

	promoteRequests   []int64
	promoted          *store.Person
	promoteErr        error
	attributeRequests []int64
	attributePages    []*peoplebrowser.Attributes
	attributeErrs     []error
	createdFields     []peoplebrowser.NewField
	createdDefinition *store.AttributeDefinition
	createFieldErr    error
	setRequests       []peoplebrowser.SetAttributeRequest
	setWrites         []*store.PersonAttributeWrite
	setAttributeErrs  []error
}

func (b *fakePeopleAttributesBackend) Promote(
	_ context.Context, participantID int64,
) (*store.Person, error) {
	b.promoteRequests = append(b.promoteRequests, participantID)
	if b.promoteErr != nil {
		return nil, b.promoteErr
	}
	if b.promoted == nil {
		return nil, errors.New("missing promoted person fixture")
	}
	person := *b.promoted
	return &person, nil
}

func (b *fakePeopleAttributesBackend) ListAttributes(
	_ context.Context, personID int64,
) (*peoplebrowser.Attributes, error) {
	b.attributeRequests = append(b.attributeRequests, personID)
	if len(b.attributeErrs) > 0 {
		err := b.attributeErrs[0]
		b.attributeErrs = b.attributeErrs[1:]
		if err != nil {
			return nil, err
		}
	}
	if len(b.attributePages) == 0 {
		return &peoplebrowser.Attributes{PersonID: personID}, nil
	}
	page := b.attributePages[0]
	b.attributePages = b.attributePages[1:]
	return page, nil
}

func (b *fakePeopleAttributesBackend) CreateField(
	_ context.Context, field peoplebrowser.NewField,
) (*store.AttributeDefinition, error) {
	b.createdFields = append(b.createdFields, field)
	if b.createFieldErr != nil {
		return nil, b.createFieldErr
	}
	if b.createdDefinition == nil {
		return nil, errors.New("missing created definition fixture")
	}
	definition := *b.createdDefinition
	return &definition, nil
}

func (b *fakePeopleAttributesBackend) SetAttribute(
	_ context.Context, request peoplebrowser.SetAttributeRequest,
) (*store.PersonAttributeWrite, error) {
	b.setRequests = append(b.setRequests, clonePeopleSetRequest(request))
	if len(b.setAttributeErrs) > 0 {
		err := b.setAttributeErrs[0]
		b.setAttributeErrs = b.setAttributeErrs[1:]
		if err != nil {
			return nil, err
		}
	}
	if len(b.setWrites) == 0 {
		return &store.PersonAttributeWrite{}, nil
	}
	write := b.setWrites[0]
	b.setWrites = b.setWrites[1:]
	return write, nil
}

func clonePeopleSetRequest(request peoplebrowser.SetAttributeRequest) peoplebrowser.SetAttributeRequest {
	if request.Ordinal != nil {
		ordinal := *request.Ordinal
		request.Ordinal = &ordinal
	}
	if request.ExpectedValueID != nil {
		expected := *request.ExpectedValueID
		request.ExpectedValueID = &expected
	}
	return request
}

func peopleAttributesModel(
	backend peoplebrowser.Backend, groups ...peoplebrowser.AttributeGroup,
) Model {
	contact := testPerson(7, "Attribute Person")
	contact.Profile = &query.PersonProfile{ID: 51, Revision: 2}
	model := peopleModel(backend)
	model.mode = modePeople
	model.presentationGeneration = 8
	model.peopleState.level = peopleLevelContact
	model.peopleState.tab = peopleTabAttributes
	model.peopleState.participantID = contact.ID
	model.peopleState.contact = &contact
	model.peopleState.attributes = &peoplebrowser.Attributes{PersonID: 51, Groups: groups}
	model.peopleState.attributesLoaded = true
	return model
}

func editablePeopleDefinition(
	slug, label string, valueType store.AttributeValueType,
	fieldType store.AttributeFieldType, cardinality store.AttributeCardinality,
) store.AttributeDefinition {
	return store.AttributeDefinition{
		ID: 9, ObjectType: store.AttributeObjectPerson, Slug: slug, Label: label,
		ValueType: valueType, FieldType: fieldType, Cardinality: cardinality,
		Ownership: store.AttributeOwnershipUser, UIEditable: true, APIMutable: true,
		IsActive: true,
	}
}

func peopleValue(
	id, ordinal int64, definition store.AttributeDefinition, value store.AttributeValue,
) store.PersonAttributeValue {
	return store.PersonAttributeValue{
		ID: id, PersonID: 51, DefinitionID: definition.ID,
		DefinitionSlug: definition.Slug, Ordinal: ordinal, Value: value,
	}
}

func textPeopleValue(value string) store.AttributeValue {
	return store.AttributeValue{Type: store.AttributeValueText, Text: &value}
}

func runPeopleCommandMessage[T any](t *testing.T, cmd tea.Cmd) T {
	t.Helper()
	for _, msg := range runBatchCommand(t, cmd) {
		if typed, ok := msg.(T); ok {
			return typed
		}
	}
	require.FailNow(t, "command did not return expected People message")
	var zero T
	return zero
}

var expectedPeopleFieldChoices = []struct {
	kind  peoplebrowser.FieldKind
	label string
}{
	{kind: peoplebrowser.FieldKindText, label: "Text"},
	{kind: peoplebrowser.FieldKindLongText, label: "Long text"},
	{kind: peoplebrowser.FieldKindNumber, label: "Number"},
	{kind: peoplebrowser.FieldKindCheckbox, label: "Checkbox"},
	{kind: peoplebrowser.FieldKindDate, label: "Date"},
	{kind: peoplebrowser.FieldKindDateTime, label: "Datetime"},
	{kind: peoplebrowser.FieldKindURL, label: "URL"},
	{kind: peoplebrowser.FieldKindEmail, label: "Email"},
	{kind: peoplebrowser.FieldKindPhone, label: "Phone"},
	{kind: peoplebrowser.FieldKindJSON, label: "JSON"},
}

func TestPeoplePromotionIsExplicitAndKeepsContactTab(t *testing.T) {
	backend := &fakePeopleAttributesBackend{
		promoted:       &store.Person{ID: 51, Revision: 1, ParticipantIDs: []int64{7}},
		attributePages: []*peoplebrowser.Attributes{{PersonID: 51}},
	}
	contact := testPerson(7, "Observed Person")
	model := peopleModel(backend)
	model.mode = modePeople
	model.peopleState.level = peopleLevelContact
	model.peopleState.tab = peopleTabAttributes
	model.peopleState.participantID = contact.ID
	model.peopleState.contact = &contact

	model, cmd := sendKey(t, model, key('n'))
	assert.Nil(t, cmd)
	assert.Empty(t, backend.promoteRequests)
	assert.Equal(t, peopleOverlayNone, model.peopleState.form.overlay)
	assert.Contains(t, model.peopleState.attributesNotice, "p")
	assert.Contains(t, stripANSI(model.renderView()), "Press p")

	model, cmd = sendKey(t, model, keyEnter())
	assert.Nil(t, cmd)
	assert.Empty(t, backend.promoteRequests)
	assert.Contains(t, model.peopleState.attributesNotice, "p")

	originalTab := model.peopleState.tab
	model, cmd = sendKey(t, model, key('p'))
	loaded := runPeopleCommandMessage[peoplePromotedMsg](t, cmd)
	assert.Equal(t, []int64{7}, backend.promoteRequests)
	assert.Equal(t, originalTab, model.peopleState.tab)

	updated, reload := model.Update(loaded)
	model = asModel(t, updated)
	assert.Equal(t, originalTab, model.peopleState.tab)
	require.NotNil(t, model.peopleState.contact.Profile)
	assert.Equal(t, int64(51), model.peopleState.contact.Profile.ID)
	attributesLoaded := runPeopleCommandMessage[peopleAttributesLoadedMsg](t, reload)
	assert.Equal(t, []int64{51}, backend.attributeRequests)
	model = sendMsg(t, model, attributesLoaded)
	assert.True(t, model.peopleState.attributesLoaded)
	assert.Equal(t, originalTab, model.peopleState.tab)
}

func TestPeoplePromotionBlocksTabChangesAndDuplicateRequest(t *testing.T) {
	backend := &fakePeopleAttributesBackend{
		promoted:       &store.Person{ID: 51, Revision: 1, ParticipantIDs: []int64{7}},
		attributePages: []*peoplebrowser.Attributes{{PersonID: 51}},
	}
	contact := testPerson(7, "Observed Person")
	model := peopleModel(backend)
	model.mode = modePeople
	model.peopleState.level = peopleLevelContact
	model.peopleState.tab = peopleTabAttributes
	model.peopleState.participantID = contact.ID
	model.peopleState.contact = &contact

	model, promote := sendKey(t, model, key('p'))
	requestID := model.peopleState.requestID
	loaded := runPeopleCommandMessage[peoplePromotedMsg](t, promote)
	require.Equal(t, []int64{7}, backend.promoteRequests)

	model, cmd := sendKey(t, model, keyTab())
	assert.Nil(t, cmd)
	assert.Equal(t, peopleTabAttributes, model.peopleState.tab)
	assert.Equal(t, requestID, model.peopleState.requestID)
	assert.True(t, model.peopleState.promoting)

	model, cmd = sendKey(t, model, keyShiftTab())
	assert.Nil(t, cmd)
	assert.Equal(t, peopleTabAttributes, model.peopleState.tab)
	assert.Equal(t, requestID, model.peopleState.requestID)
	assert.True(t, model.peopleState.promoting)

	model, duplicate := sendKey(t, model, key('p'))
	assert.Nil(t, duplicate)
	assert.Equal(t, []int64{7}, backend.promoteRequests)

	updated, reload := model.Update(loaded)
	model = asModel(t, updated)
	require.NotNil(t, model.peopleState.contact.Profile)
	assert.Equal(t, int64(51), model.peopleState.contact.Profile.ID)
	assert.Equal(t, peopleTabAttributes, model.peopleState.tab)
	attributesLoaded := runPeopleCommandMessage[peopleAttributesLoadedMsg](t, reload)
	model = sendMsg(t, model, attributesLoaded)
	assert.Equal(t, []int64{51}, backend.attributeRequests)
	assert.Equal(t, []int64{7}, backend.promoteRequests)
}

func TestPeoplePromotionResponseRejectsStaleContext(t *testing.T) {
	newModel := func() Model {
		contact := testPerson(7, "Observed Person")
		model := peopleModel(&fakePeopleAttributesBackend{})
		model.mode = modePeople
		model.presentationGeneration = 10
		model.peopleState.level = peopleLevelContact
		model.peopleState.tab = peopleTabAttributes
		model.peopleState.participantID = contact.ID
		model.peopleState.contact = &contact
		model.peopleState.requestID = 4
		return model
	}
	base := peoplePromotedMsg{
		person: &store.Person{
			ID: 51, Revision: 1, ParticipantIDs: []int64{7},
		},
		participantID: 7, tab: peopleTabAttributes,
		requestID: 4, presentationGeneration: 10,
	}
	tests := []struct {
		name   string
		mutate func(*peoplePromotedMsg)
	}{
		{name: "presentation", mutate: func(msg *peoplePromotedMsg) { msg.presentationGeneration-- }},
		{name: "request", mutate: func(msg *peoplePromotedMsg) { msg.requestID-- }},
		{name: "participant", mutate: func(msg *peoplePromotedMsg) { msg.participantID++ }},
		{name: "person", mutate: func(msg *peoplePromotedMsg) {
			msg.person = &store.Person{ID: 52, ParticipantIDs: []int64{99}}
		}},
		{name: "tab", mutate: func(msg *peoplePromotedMsg) { msg.tab = peopleTabOverview }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			model := newModel()
			msg := base
			tt.mutate(&msg)

			updated, cmd := model.Update(msg)
			model = asModel(t, updated)

			assert.Nil(t, cmd)
			assert.Nil(t, model.peopleState.contact.Profile)
		})
	}

	t.Run("valid", func(t *testing.T) {
		model := newModel()

		updated, reload := model.Update(base)
		model = asModel(t, updated)

		require.NotNil(t, reload)
		require.NotNil(t, model.peopleState.contact.Profile)
		assert.Equal(t, int64(51), model.peopleState.contact.Profile.ID)
		assert.Equal(t, peopleTabAttributes, model.peopleState.tab)
	})
}

func TestPeopleNewFieldFormContainsOnlyUserFacingFields(t *testing.T) {
	model := peopleAttributesModel(&fakePeopleAttributesBackend{})

	model, _ = sendKey(t, model, key('n'))
	assert.Equal(t, peopleOverlayNewField, model.peopleState.form.overlay)
	assert.Equal(t, peopleFieldFocusName, model.peopleState.form.fieldFocus)
	assert.True(t, model.peopleState.form.nameInput.Focused())

	view := stripANSI(model.renderView())
	assert.Contains(t, view, "Name")
	assert.Contains(t, view, "Type")
	assert.Contains(t, view, "Cardinality")
	assert.Contains(t, view, "Save")
	assert.NotContains(t, view, "Slug")
	assert.NotContains(t, view, "slug")

	for _, focus := range []peopleFieldFocus{
		peopleFieldFocusType, peopleFieldFocusCardinality,
		peopleFieldFocusSave, peopleFieldFocusName,
	} {
		model, _ = sendKey(t, model, keyTab())
		assert.Equal(t, focus, model.peopleState.form.fieldFocus)
	}

	model, _ = sendKey(t, model, keyEsc())
	assert.Equal(t, peopleOverlayNone, model.peopleState.form.overlay)
}

func TestPeopleNewFieldNameAcceptsVimLettersAndCursorEditing(t *testing.T) {
	model := peopleAttributesModel(&fakePeopleAttributesBackend{})
	model, _ = sendKey(t, model, key('n'))

	for _, character := range "hl" {
		model, _ = sendKey(t, model, key(character))
	}
	model, _ = sendKey(t, model, keyLeft())
	for _, character := range "jk" {
		model, _ = sendKey(t, model, key(character))
	}
	model, _ = sendKey(t, model, keyRight())
	for _, character := range " Relationship" {
		model, _ = sendKey(t, model, key(character))
	}

	assert.Equal(t, "hjkl Relationship", model.peopleState.form.nameInput.Value())
}

func TestPeopleNewFieldSelectsEverySupportedTypeAndCardinality(t *testing.T) {
	backend := &fakePeopleAttributesBackend{}
	model := peopleAttributesModel(backend)
	model, _ = sendKey(t, model, key('n'))
	model, _ = sendKey(t, model, keyTab())

	for index, want := range expectedPeopleFieldChoices {
		if index > 0 {
			model, _ = sendKey(t, model, keyRight())
		}
		assert.Equal(t, want.kind, model.peopleState.form.fieldKind())
		assert.Contains(t, stripANSI(model.renderView()), "Type: ‹ "+want.label+" ›")
	}
	model, _ = sendKey(t, model, keyRight())
	assert.Equal(t, peoplebrowser.FieldKindText, model.peopleState.form.fieldKind(),
		"the approved ten choices must wrap directly back to Text")

	model, _ = sendKey(t, model, keyTab())
	assert.Equal(t, store.AttributeCardinalitySingle, model.peopleState.form.cardinality())
	model, _ = sendKey(t, model, keyRight())
	assert.Equal(t, store.AttributeCardinalityMulti, model.peopleState.form.cardinality())
	model, _ = sendKey(t, model, keyLeft())
	assert.Equal(t, store.AttributeCardinalitySingle, model.peopleState.form.cardinality())
}

func TestPeopleNewFieldValidationKeepsDraft(t *testing.T) {
	backend := &fakePeopleAttributesBackend{}
	model := peopleAttributesModel(backend)
	model, _ = sendKey(t, model, key('n'))
	model.peopleState.form.fieldFocus = peopleFieldFocusSave

	model, cmd := sendKey(t, model, keyEnter())

	assert.Nil(t, cmd)
	assert.Empty(t, backend.createdFields)
	assert.Equal(t, peopleOverlayNewField, model.peopleState.form.overlay)
	assert.Contains(t, model.peopleState.form.notice, "Name")
}

func TestPeopleNewFieldCreationOpensFirstValueForm(t *testing.T) {
	for index, choice := range expectedPeopleFieldChoices {
		t.Run(string(choice.kind), func(t *testing.T) {
			definitionInput, err := (peoplebrowser.NewField{
				Label: "Custom field", Kind: choice.kind,
				Cardinality: store.AttributeCardinalityMulti,
			}).DefinitionInput()
			require.NoError(t, err)
			definition := editablePeopleDefinition(
				"custom_field", "Custom field", definitionInput.ValueType,
				definitionInput.FieldType, store.AttributeCardinalityMulti,
			)
			backend := &fakePeopleAttributesBackend{createdDefinition: &definition}
			model := peopleAttributesModel(backend)
			model, _ = sendKey(t, model, key('n'))
			model.peopleState.form.nameInput.SetValue("Custom field")
			model.peopleState.form.fieldKindIndex = index
			model.peopleState.form.cardinalityIndex = 1
			model.peopleState.form.fieldFocus = peopleFieldFocusSave

			model, cmd := sendKey(t, model, keyEnter())
			created := runPeopleCommandMessage[peopleFieldCreatedMsg](t, cmd)
			require.Len(t, backend.createdFields, 1)
			assert.Equal(t, peoplebrowser.NewField{
				Label: "Custom field", Kind: choice.kind,
				Cardinality: store.AttributeCardinalityMulti,
			}, backend.createdFields[0])

			updated, _ := model.Update(created)
			model = asModel(t, updated)
			assert.Equal(t, peopleOverlayAttributeValue, model.peopleState.form.overlay)
			require.NotNil(t, model.peopleState.form.definition)
			assert.Equal(t, "custom_field", model.peopleState.form.definition.Slug)
			assert.Equal(t, int64(0), model.peopleState.form.ordinal)
			assert.False(t, model.peopleState.form.editing)
		})
	}
}

func TestPeopleAttributeEnterAddsEmptyOrAdditionalValueAndEditUsesSelection(t *testing.T) {
	single := editablePeopleDefinition(
		"nickname", "Nickname", store.AttributeValueText,
		store.AttributeFieldText, store.AttributeCardinalitySingle,
	)
	multi := editablePeopleDefinition(
		"score", "Score", store.AttributeValueReal,
		store.AttributeFieldText, store.AttributeCardinalityMulti,
	)
	multi.ID = 10
	multiValues := []store.PersonAttributeValue{
		peopleValue(20, 0, multi, store.AttributeValue{Type: store.AttributeValueReal, Real: new(1.5)}),
		peopleValue(22, 2, multi, store.AttributeValue{Type: store.AttributeValueReal, Real: new(3.5)}),
	}
	backend := &fakePeopleAttributesBackend{}

	t.Run("empty field", func(t *testing.T) {
		model := peopleAttributesModel(backend, peoplebrowser.AttributeGroup{Definition: single})
		model, _ = sendKey(t, model, keyEnter())
		assert.Equal(t, peopleOverlayAttributeValue, model.peopleState.form.overlay)
		assert.False(t, model.peopleState.form.editing)
		assert.Zero(t, model.peopleState.form.ordinal)
	})

	t.Run("multiple field chooses first unused ordinal", func(t *testing.T) {
		model := peopleAttributesModel(backend, peoplebrowser.AttributeGroup{
			Definition: multi, Current: multiValues,
		})
		model, _ = sendKey(t, model, keyEnter())
		assert.Equal(t, peopleOverlayAttributeValue, model.peopleState.form.overlay)
		assert.False(t, model.peopleState.form.editing)
		assert.Equal(t, int64(1), model.peopleState.form.ordinal)
	})

	t.Run("selected value edits with compare and swap", func(t *testing.T) {
		model := peopleAttributesModel(backend, peoplebrowser.AttributeGroup{
			Definition: multi, Current: multiValues,
		})
		model.peopleState.attributeCursor = 2 // field row, ordinal 0, ordinal 2
		model, _ = sendKey(t, model, key('e'))
		assert.Equal(t, peopleOverlayAttributeValue, model.peopleState.form.overlay)
		assert.True(t, model.peopleState.form.editing)
		assert.Equal(t, int64(22), model.peopleState.form.expectedValueID)
		assert.Equal(t, int64(2), model.peopleState.form.ordinal)
		assert.Equal(t, "3.5", model.peopleState.form.draft)
	})
}

func TestPeopleAttributeSingleExistingValueRequiresExplicitEdit(t *testing.T) {
	definition := editablePeopleDefinition(
		"nickname", "Nickname", store.AttributeValueText,
		store.AttributeFieldText, store.AttributeCardinalitySingle,
	)
	model := peopleAttributesModel(&fakePeopleAttributesBackend{}, peoplebrowser.AttributeGroup{
		Definition: definition,
		Current: []store.PersonAttributeValue{
			peopleValue(30, 0, definition, textPeopleValue("old")),
		},
	})

	model, cmd := sendKey(t, model, keyEnter())

	assert.Nil(t, cmd)
	assert.Equal(t, peopleOverlayNone, model.peopleState.form.overlay)
	assert.Contains(t, model.peopleState.attributesNotice, "e")
}

func TestPeopleAttributeDraftParsing(t *testing.T) {
	timestamp := time.Date(2026, 8, 21, 15, 4, 5, 123456789, time.UTC)
	tests := []struct {
		name       string
		definition store.AttributeDefinition
		draft      string
		want       store.AttributeValue
	}{
		{name: "text", definition: editablePeopleDefinition("text", "Text", store.AttributeValueText, store.AttributeFieldText, store.AttributeCardinalitySingle), draft: "  raw text  ", want: textPeopleValue("  raw text  ")},
		{name: "long text", definition: editablePeopleDefinition("long", "Long", store.AttributeValueText, store.AttributeFieldTextarea, store.AttributeCardinalitySingle), draft: "line one\nline two", want: textPeopleValue("line one\nline two")},
		{name: "number", definition: editablePeopleDefinition("number", "Number", store.AttributeValueReal, store.AttributeFieldText, store.AttributeCardinalitySingle), draft: "3.25", want: store.AttributeValue{Type: store.AttributeValueReal, Real: new(3.25)}},
		{name: "checkbox", definition: editablePeopleDefinition("checkbox", "Checkbox", store.AttributeValueBoolean, store.AttributeFieldCheckbox, store.AttributeCardinalitySingle), draft: "TRUE", want: store.AttributeValue{Type: store.AttributeValueBoolean, Boolean: new(true)}},
		{name: "date", definition: editablePeopleDefinition("date", "Date", store.AttributeValueDate, store.AttributeFieldDate, store.AttributeCardinalitySingle), draft: "2024-02-29", want: store.AttributeValue{Type: store.AttributeValueDate, Date: new("2024-02-29")}},
		{name: "datetime", definition: editablePeopleDefinition("datetime", "Datetime", store.AttributeValueTimestamp, store.AttributeFieldTimestamp, store.AttributeCardinalitySingle), draft: timestamp.Format(time.RFC3339Nano), want: store.AttributeValue{Type: store.AttributeValueTimestamp, Timestamp: &timestamp}},
		{name: "URL", definition: editablePeopleDefinition("url", "URL", store.AttributeValueText, store.AttributeFieldURL, store.AttributeCardinalitySingle), draft: "https://example.test/a", want: textPeopleValue("https://example.test/a")},
		{name: "email", definition: editablePeopleDefinition("email", "Email", store.AttributeValueText, store.AttributeFieldEmail, store.AttributeCardinalitySingle), draft: "alice@example.test", want: textPeopleValue("alice@example.test")},
		{name: "phone", definition: editablePeopleDefinition("phone", "Phone", store.AttributeValueText, store.AttributeFieldPhone, store.AttributeCardinalitySingle), draft: "+1 555 0100", want: textPeopleValue("+1 555 0100")},
		{name: "JSON", definition: editablePeopleDefinition("json", "JSON", store.AttributeValueJSON, store.AttributeFieldJSON, store.AttributeCardinalitySingle), draft: ` {"enabled":true} `, want: store.AttributeValue{Type: store.AttributeValueJSON, JSON: json.RawMessage(` {"enabled":true} `)}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parsePeopleAttributeDraft(tt.definition, tt.draft)
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestPeopleAttributeDraftParsingRejectsMalformedValues(t *testing.T) {
	tests := []struct {
		name       string
		definition store.AttributeDefinition
		draft      string
	}{
		{name: "blank text", definition: editablePeopleDefinition("text", "Text", store.AttributeValueText, store.AttributeFieldText, store.AttributeCardinalitySingle), draft: "   "},
		{name: "number", definition: editablePeopleDefinition("number", "Number", store.AttributeValueReal, store.AttributeFieldText, store.AttributeCardinalitySingle), draft: "3.2x"},
		{name: "number wrong widget", definition: editablePeopleDefinition("number", "Number", store.AttributeValueReal, store.AttributeFieldTextarea, store.AttributeCardinalitySingle), draft: "3.2"},
		{name: "checkbox", definition: editablePeopleDefinition("checkbox", "Checkbox", store.AttributeValueBoolean, store.AttributeFieldCheckbox, store.AttributeCardinalitySingle), draft: "yes"},
		{name: "date shape", definition: editablePeopleDefinition("date", "Date", store.AttributeValueDate, store.AttributeFieldDate, store.AttributeCardinalitySingle), draft: "2026-2-03"},
		{name: "date calendar", definition: editablePeopleDefinition("date", "Date", store.AttributeValueDate, store.AttributeFieldDate, store.AttributeCardinalitySingle), draft: "2026-02-30"},
		{name: "datetime", definition: editablePeopleDefinition("datetime", "Datetime", store.AttributeValueTimestamp, store.AttributeFieldTimestamp, store.AttributeCardinalitySingle), draft: "2026-08-21 12:00"},
		{name: "JSON", definition: editablePeopleDefinition("json", "JSON", store.AttributeValueJSON, store.AttributeFieldJSON, store.AttributeCardinalitySingle), draft: `{"missing":}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parsePeopleAttributeDraft(tt.definition, tt.draft)
			require.Error(t, err)
		})
	}
}

func TestPeopleAttributeUnsupportedDefinitionStaysReadOnly(t *testing.T) {
	definition := editablePeopleDefinition(
		"contact_frequency", "Contact frequency", store.AttributeValueInteger,
		store.AttributeFieldDuration, store.AttributeCardinalitySingle,
	)
	model := peopleAttributesModel(&fakePeopleAttributesBackend{}, peoplebrowser.AttributeGroup{
		Definition: definition,
	})

	model, cmd := sendKey(t, model, keyEnter())

	assert.Nil(t, cmd)
	assert.Equal(t, peopleOverlayNone, model.peopleState.form.overlay)
	assert.Contains(t, model.peopleState.attributesNotice, "read-only")
	assert.Contains(t, stripANSI(model.renderView()), "read-only")
}

func TestPeopleAttributeEditUsesCASAndReloadsWithoutDirectMutation(t *testing.T) {
	definition := editablePeopleDefinition(
		"nickname", "Nickname", store.AttributeValueText,
		store.AttributeFieldText, store.AttributeCardinalitySingle,
	)
	oldValue := peopleValue(40, 0, definition, textPeopleValue("old server value"))
	newValue := peopleValue(41, 0, definition, textPeopleValue("new server value"))
	backend := &fakePeopleAttributesBackend{
		setWrites: []*store.PersonAttributeWrite{{Value: &newValue, Superseded: &oldValue}},
		attributePages: []*peoplebrowser.Attributes{{
			PersonID: 51, Groups: []peoplebrowser.AttributeGroup{{
				Definition: definition, Current: []store.PersonAttributeValue{newValue},
			}},
		}},
	}
	model := peopleAttributesModel(backend, peoplebrowser.AttributeGroup{
		Definition: definition, Current: []store.PersonAttributeValue{oldValue},
	})
	model.peopleState.attributeCursor = 1
	model, _ = sendKey(t, model, key('e'))
	model.peopleState.form.draft = "my draft"
	model.peopleState.form.valueInput.SetValue("my draft")

	model, cmd := sendKey(t, model, keyEnter())
	set := runPeopleCommandMessage[peopleAttributeSetMsg](t, cmd)
	require.Len(t, backend.setRequests, 1)
	request := backend.setRequests[0]
	assert.Equal(t, int64(51), request.PersonID)
	assert.Equal(t, "nickname", request.Slug)
	require.NotNil(t, request.Ordinal)
	assert.Equal(t, int64(0), *request.Ordinal)
	require.NotNil(t, request.ExpectedValueID)
	assert.Equal(t, int64(40), *request.ExpectedValueID)
	assert.Equal(t, textPeopleValue("my draft"), request.Value)

	updated, reload := model.Update(set)
	model = asModel(t, updated)
	assert.Equal(t, peopleOverlayNone, model.peopleState.form.overlay)
	require.Len(t, model.peopleState.attributes.Groups[0].Current, 1)
	assert.Equal(t, int64(40), model.peopleState.attributes.Groups[0].Current[0].ID)
	assert.Equal(t, "old server value", *model.peopleState.attributes.Groups[0].Current[0].Value.Text)

	loaded := runPeopleCommandMessage[peopleAttributesLoadedMsg](t, reload)
	model = sendMsg(t, model, loaded)
	require.Len(t, model.peopleState.attributes.Groups[0].Current, 1)
	assert.Equal(t, int64(41), model.peopleState.attributes.Groups[0].Current[0].ID)
	assert.Equal(t, "new server value", *model.peopleState.attributes.Groups[0].Current[0].Value.Text)
}

func TestPeopleAttributeStaleConflictKeepsDraftAndRequiresResubmit(t *testing.T) {
	definition := editablePeopleDefinition(
		"nickname", "Nickname", store.AttributeValueText,
		store.AttributeFieldText, store.AttributeCardinalitySingle,
	)
	oldValue := peopleValue(40, 0, definition, textPeopleValue("old server value"))
	conflictValue := peopleValue(42, 0, definition, textPeopleValue("conflict payload value"))
	currentValue := peopleValue(43, 0, definition, textPeopleValue("current server value"))
	backend := &fakePeopleAttributesBackend{
		setAttributeErrs: []error{peoplebrowser.StaleValueError{
			CurrentValueID: 42, CurrentValue: &conflictValue,
		}},
		attributePages: []*peoplebrowser.Attributes{{
			PersonID: 51, Groups: []peoplebrowser.AttributeGroup{{
				Definition: definition, Current: []store.PersonAttributeValue{currentValue},
			}},
		}},
	}
	model := peopleAttributesModel(backend, peoplebrowser.AttributeGroup{
		Definition: definition, Current: []store.PersonAttributeValue{oldValue},
	})
	model.peopleState.attributeCursor = 1
	model, _ = sendKey(t, model, key('e'))
	model.peopleState.form.draft = "new draft"
	model.peopleState.form.valueInput.SetValue("new draft")

	model, cmd := sendKey(t, model, keyEnter())
	stale := runPeopleCommandMessage[peopleAttributeSetMsg](t, cmd)
	updated, reload := model.Update(stale)
	model = asModel(t, updated)

	assert.Equal(t, peopleOverlayAttributeValue, model.peopleState.form.overlay)
	assert.Equal(t, "new draft", model.peopleState.form.draft)
	assert.Equal(t, "new draft", model.peopleState.form.valueInput.Value())
	assert.Equal(t, int64(42), model.peopleState.form.expectedValueID)
	assert.True(t, model.peopleState.form.staleReloadPending)
	assert.Empty(t, model.peopleState.form.serverValue)
	assert.Contains(t, model.peopleState.form.notice, "changed")
	assert.Contains(t, stripANSI(model.renderView()), "new draft")
	assert.NotContains(t, stripANSI(model.renderView()), "old server value")
	assert.NotContains(t, stripANSI(model.renderView()), "Server:")
	assert.Len(t, backend.setRequests, 1)
	requestID := model.peopleState.requestID

	model, blockedSubmit := sendKey(t, model, keyEnter())
	assert.Nil(t, blockedSubmit)
	assert.Equal(t, requestID, model.peopleState.requestID)
	assert.Len(t, backend.setRequests, 1, "Enter cannot write before stale reload completes")

	loaded := runPeopleCommandMessage[peopleAttributesLoadedMsg](t, reload)
	model = sendMsg(t, model, loaded)
	assert.Equal(t, "new draft", model.peopleState.form.draft)
	assert.Equal(t, "current server value", model.peopleState.form.serverValue)
	assert.Equal(t, int64(43), model.peopleState.form.expectedValueID)
	assert.False(t, model.peopleState.form.staleReloadPending)
	assert.Contains(t, stripANSI(model.renderView()), "Server: current server value")
	assert.Contains(t, stripANSI(model.renderView()), "Draft:  new draft")
	assert.Len(t, backend.setRequests, 1, "reload must not resubmit the preserved draft")

	model, secondSubmit := sendKey(t, model, keyEnter())
	_ = runPeopleCommandMessage[peopleAttributeSetMsg](t, secondSubmit)
	require.Len(t, backend.setRequests, 2)
	require.NotNil(t, backend.setRequests[1].ExpectedValueID)
	assert.Equal(t, int64(43), *backend.setRequests[1].ExpectedValueID)
	assert.Equal(t, "new draft", *backend.setRequests[1].Value.Text)
}

func TestPeopleAttributeStaleConflictRendersEmptyServerValue(t *testing.T) {
	definition := editablePeopleDefinition(
		"nickname", "Nickname", store.AttributeValueText,
		store.AttributeFieldText, store.AttributeCardinalitySingle,
	)
	oldValue := peopleValue(40, 0, definition, textPeopleValue("old server value"))
	currentValue := peopleValue(43, 0, definition, textPeopleValue(""))
	backend := &fakePeopleAttributesBackend{
		setAttributeErrs: []error{peoplebrowser.StaleValueError{CurrentValueID: 42}},
		attributePages: []*peoplebrowser.Attributes{{
			PersonID: 51, Groups: []peoplebrowser.AttributeGroup{{
				Definition: definition, Current: []store.PersonAttributeValue{currentValue},
			}},
		}},
	}
	model := peopleAttributesModel(backend, peoplebrowser.AttributeGroup{
		Definition: definition, Current: []store.PersonAttributeValue{oldValue},
	})
	model.peopleState.attributeCursor = 1
	model, _ = sendKey(t, model, key('e'))
	model.peopleState.form.valueInput.SetValue("preserved draft")

	model, submit := sendKey(t, model, keyEnter())
	stale := runPeopleCommandMessage[peopleAttributeSetMsg](t, submit)
	updated, reload := model.Update(stale)
	model = asModel(t, updated)
	loaded := runPeopleCommandMessage[peopleAttributesLoadedMsg](t, reload)
	model = sendMsg(t, model, loaded)

	view := stripANSI(model.peopleAttributeValueFormView())
	assert.Contains(t, view, "Server: \nDraft:  preserved draft")
	assert.Equal(t, "preserved draft", model.peopleState.form.draft)
	assert.False(t, model.peopleState.form.staleReloadPending)
}

func TestPeopleAttributeStaleReloadFailureKeepsWritesDisabledAndRetryable(t *testing.T) {
	definition := editablePeopleDefinition(
		"nickname", "Nickname", store.AttributeValueText,
		store.AttributeFieldText, store.AttributeCardinalitySingle,
	)
	oldValue := peopleValue(40, 0, definition, textPeopleValue("old server value"))
	backend := &fakePeopleAttributesBackend{
		setAttributeErrs: []error{peoplebrowser.StaleValueError{CurrentValueID: 42}},
		attributeErrs: []error{
			errors.New("reload unavailable"), errors.New("reload still unavailable"),
		},
	}
	model := peopleAttributesModel(backend, peoplebrowser.AttributeGroup{
		Definition: definition, Current: []store.PersonAttributeValue{oldValue},
	})
	model.peopleState.attributeCursor = 1
	model, _ = sendKey(t, model, key('e'))
	model.peopleState.form.valueInput.SetValue("preserved draft")

	model, submit := sendKey(t, model, keyEnter())
	stale := runPeopleCommandMessage[peopleAttributeSetMsg](t, submit)
	updated, firstReload := model.Update(stale)
	model = asModel(t, updated)

	model, blocked := sendKey(t, model, keyEnter())
	assert.Nil(t, blocked)
	assert.Len(t, backend.setRequests, 1)

	firstFailure := runPeopleCommandMessage[peopleAttributesLoadedMsg](t, firstReload)
	model = sendMsg(t, model, firstFailure)
	assert.True(t, model.peopleState.form.staleReloadPending)
	assert.Equal(t, "preserved draft", model.peopleState.form.draft)
	assert.Contains(t, model.peopleState.form.notice, "reload failed")
	assert.Len(t, backend.setRequests, 1)

	model, retryReload := sendKey(t, model, keyEnter())
	secondFailure := runPeopleCommandMessage[peopleAttributesLoadedMsg](t, retryReload)
	assert.Equal(t, []int64{51, 51}, backend.attributeRequests)
	assert.Len(t, backend.setRequests, 1, "retry performs a read, never a write")
	model = sendMsg(t, model, secondFailure)
	assert.True(t, model.peopleState.form.staleReloadPending)
	assert.Equal(t, "preserved draft", model.peopleState.form.draft)
	assert.Len(t, backend.setRequests, 1)
}

func TestPeopleAttributeStaleConflictDeletionCanBeExplicitlyRecreated(t *testing.T) {
	definition := editablePeopleDefinition(
		"nickname", "Nickname", store.AttributeValueText,
		store.AttributeFieldText, store.AttributeCardinalitySingle,
	)
	oldValue := peopleValue(40, 2, definition, textPeopleValue("old server value"))
	backend := &fakePeopleAttributesBackend{
		setAttributeErrs: []error{peoplebrowser.StaleValueError{CurrentValueID: 42}},
		attributePages:   []*peoplebrowser.Attributes{{PersonID: 51, Groups: []peoplebrowser.AttributeGroup{{Definition: definition}}}},
	}
	model := peopleAttributesModel(backend, peoplebrowser.AttributeGroup{
		Definition: definition, Current: []store.PersonAttributeValue{oldValue},
	})
	model.peopleState.attributeCursor = 1
	model, _ = sendKey(t, model, key('e'))
	model.peopleState.form.valueInput.SetValue("recreated draft")

	model, submit := sendKey(t, model, keyEnter())
	updated, reload := model.Update(runPeopleCommandMessage[peopleAttributeSetMsg](t, submit))
	model = asModel(t, updated)
	model = sendMsg(t, model, runPeopleCommandMessage[peopleAttributesLoadedMsg](t, reload))

	assert.False(t, model.peopleState.form.staleReloadPending)
	assert.False(t, model.peopleState.form.serverValuePresent)
	assert.Zero(t, model.peopleState.form.expectedValueID)
	assert.Equal(t, "recreated draft", model.peopleState.form.draft)
	assert.Contains(t, model.peopleState.form.notice, "recreate")

	model, recreate := sendKey(t, model, keyEnter())
	_ = runPeopleCommandMessage[peopleAttributeSetMsg](t, recreate)
	require.Len(t, backend.setRequests, 2)
	assert.Nil(t, backend.setRequests[1].ExpectedValueID)
	require.NotNil(t, backend.setRequests[1].Ordinal)
	assert.Equal(t, int64(2), *backend.setRequests[1].Ordinal)
	assert.Equal(t, "recreated draft", *backend.setRequests[1].Value.Text)
}

func TestPeopleLongTextEditorAcceptsNewlinesAndUsesControlSToSave(t *testing.T) {
	definition := editablePeopleDefinition(
		"notes", "Notes", store.AttributeValueText,
		store.AttributeFieldTextarea, store.AttributeCardinalitySingle,
	)
	backend := &fakePeopleAttributesBackend{}
	model := peopleAttributesModel(backend, peoplebrowser.AttributeGroup{Definition: definition})
	model, _ = sendKey(t, model, keyEnter())

	for _, input := range []tea.KeyPressMsg{key('l'), key('i'), key('n'), key('e'), key('1')} {
		model, _ = sendKey(t, model, input)
	}
	model, cmd := sendKey(t, model, keyEnter())
	_ = cmd
	assert.Empty(t, backend.setRequests, "Enter inserts a newline instead of saving a textarea")
	assert.Equal(t, "line1\n", model.peopleState.form.draft)
	for _, input := range []tea.KeyPressMsg{key('l'), key('i'), key('n'), key('e'), key('2')} {
		model, _ = sendKey(t, model, input)
	}

	model, save := sendKey(t, model, tea.KeyPressMsg{Code: 's', Mod: tea.ModCtrl})
	require.NotNil(t, save)
	_ = runPeopleCommandMessage[peopleAttributeSetMsg](t, save)
	require.Len(t, backend.setRequests, 1)
	assert.Equal(t, "line1\nline2", *backend.setRequests[0].Value.Text)
	assert.Contains(t, stripANSI(model.peopleAttributeValueFormView()), "Ctrl+S saves")
}

func TestPeopleLongTextEditPreservesMultilineDraftAcrossStaleReload(t *testing.T) {
	definition := editablePeopleDefinition(
		"notes", "Notes", store.AttributeValueText,
		store.AttributeFieldTextarea, store.AttributeCardinalitySingle,
	)
	oldValue := peopleValue(40, 0, definition, textPeopleValue("old"))
	currentValue := peopleValue(43, 0, definition, textPeopleValue("server current"))
	backend := &fakePeopleAttributesBackend{
		setAttributeErrs: []error{peoplebrowser.StaleValueError{CurrentValueID: 42}},
		attributePages: []*peoplebrowser.Attributes{{
			PersonID: 51, Groups: []peoplebrowser.AttributeGroup{{
				Definition: definition, Current: []store.PersonAttributeValue{currentValue},
			}},
		}},
	}
	model := peopleAttributesModel(backend, peoplebrowser.AttributeGroup{
		Definition: definition, Current: []store.PersonAttributeValue{oldValue},
	})
	model.peopleState.attributeCursor = 1
	model, _ = sendKey(t, model, key('e'))
	model, _ = sendKey(t, model, keyEnter())
	model, _ = sendKey(t, model, key('x'))
	assert.Equal(t, "old\nx", model.peopleState.form.draft)

	model, save := sendKey(t, model, tea.KeyPressMsg{Code: 's', Mod: tea.ModCtrl})
	updated, reload := model.Update(runPeopleCommandMessage[peopleAttributeSetMsg](t, save))
	model = asModel(t, updated)
	model = sendMsg(t, model, runPeopleCommandMessage[peopleAttributesLoadedMsg](t, reload))
	assert.Equal(t, "old\nx", model.peopleState.form.draft)
	assert.Equal(t, "old\nx", model.peopleState.form.valueTextarea.Value())

	model, resubmit := sendKey(t, model, tea.KeyPressMsg{Code: 's', Mod: tea.ModCtrl})
	_ = runPeopleCommandMessage[peopleAttributeSetMsg](t, resubmit)
	require.Len(t, backend.setRequests, 2)
	assert.Equal(t, "old\nx", *backend.setRequests[1].Value.Text)
	require.NotNil(t, backend.setRequests[1].ExpectedValueID)
	assert.Equal(t, int64(43), *backend.setRequests[1].ExpectedValueID)
}

func TestPeopleAttributesViewportKeepsSelectionVisible(t *testing.T) {
	groups := make([]peoplebrowser.AttributeGroup, 12)
	for i := range groups {
		groups[i] = peoplebrowser.AttributeGroup{Definition: editablePeopleDefinition(
			fmt.Sprintf("field-%d", i), fmt.Sprintf("Field %d", i),
			store.AttributeValueText, store.AttributeFieldText, store.AttributeCardinalitySingle,
		)}
	}
	model := peopleAttributesModel(&fakePeopleAttributesBackend{}, groups...)
	model.height = 10
	model.pageSize = 4

	model, _ = sendKey(t, model, key('G'))
	lines := model.peopleAttributesLines()
	assert.Positive(t, model.peopleState.attributeScrollOffset)
	assert.LessOrEqual(t, len(lines), model.peopleAttributesDataRows())
	assert.Contains(t, strings.Join(lines, "\n"), "▶  Field 11")
	assert.NotContains(t, strings.Join(lines, "\n"), "Field 0")

	model, _ = sendKey(t, model, keyHome())
	assert.Zero(t, model.peopleState.attributeScrollOffset)
	assert.Contains(t, strings.Join(model.peopleAttributesLines(), "\n"), "▶  Field 0")
}

func TestPeopleAttributeEscapeCancelsBeforeBackendMutation(t *testing.T) {
	definition := editablePeopleDefinition(
		"nickname", "Nickname", store.AttributeValueText,
		store.AttributeFieldText, store.AttributeCardinalitySingle,
	)
	backend := &fakePeopleAttributesBackend{}
	model := peopleAttributesModel(backend, peoplebrowser.AttributeGroup{Definition: definition})
	model, _ = sendKey(t, model, keyEnter())
	model.peopleState.form.valueInput.SetValue("unsaved")
	model.peopleState.form.draft = "unsaved"
	model.modal = modalQuitConfirm

	model, cmd := sendKey(t, model, keyEsc())

	assert.Nil(t, cmd)
	assert.Empty(t, backend.setRequests)
	assert.Equal(t, peopleOverlayNone, model.peopleState.form.overlay)
	assert.Equal(t, modalQuitConfirm, model.modal, "People form must consume Esc before root modal")
}

func TestPeopleAttributeResponsesRejectStalePresentationRequestParticipantAndTab(t *testing.T) {
	definition := editablePeopleDefinition(
		"nickname", "Nickname", store.AttributeValueText,
		store.AttributeFieldText, store.AttributeCardinalitySingle,
	)
	original := &peoplebrowser.Attributes{PersonID: 51}
	replacement := &peoplebrowser.Attributes{PersonID: 51, Groups: []peoplebrowser.AttributeGroup{{
		Definition: definition,
	}}}
	model := peopleAttributesModel(&fakePeopleAttributesBackend{})
	model.peopleState.attributes = original
	model.peopleState.requestID = 5
	model.presentationGeneration = 9
	base := peopleAttributesLoadedMsg{
		attributes: replacement, participantID: 7, personID: 51,
		tab: peopleTabAttributes, requestID: 5, presentationGeneration: 9,
	}

	for _, mutate := range []func(*peopleAttributesLoadedMsg){
		func(msg *peopleAttributesLoadedMsg) { msg.presentationGeneration-- },
		func(msg *peopleAttributesLoadedMsg) { msg.requestID-- },
		func(msg *peopleAttributesLoadedMsg) { msg.participantID++ },
		func(msg *peopleAttributesLoadedMsg) { msg.tab = peopleTabOverview },
	} {
		msg := base
		mutate(&msg)
		model = sendMsg(t, model, msg)
		assert.Same(t, original, model.peopleState.attributes)
	}

	model.peopleState.tab = peopleTabOverview
	model = sendMsg(t, model, base)
	assert.Same(t, original, model.peopleState.attributes)
	assert.Equal(t, peopleOverlayNone, model.peopleState.form.overlay)
}

func TestPeopleContactTabWaitsForContactLoad(t *testing.T) {
	model := peopleModel(&fakePeopleAttributesBackend{})
	model.mode = modePeople
	model.peopleState.level = peopleLevelContact
	model.peopleState.tab = peopleTabOverview
	model.peopleState.participantID = 7
	model.peopleState.contactLoading = true
	model.peopleState.requestID = 5

	model, cmd := sendKey(t, model, keyTab())

	assert.Nil(t, cmd)
	assert.Equal(t, peopleTabOverview, model.peopleState.tab)
	assert.Equal(t, uint64(5), model.peopleState.requestID)
}
