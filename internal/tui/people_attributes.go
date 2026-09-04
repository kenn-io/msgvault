package tui

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"charm.land/bubbles/v2/textarea"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"go.kenn.io/msgvault/internal/peoplebrowser"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/textutil"
)

const peoplePromotionInstruction = "Press p to promote this contact before editing attributes."

type peopleOverlay uint8

const (
	peopleOverlayNone peopleOverlay = iota
	peopleOverlayNewField
	peopleOverlayAttributeValue
)

type peopleFieldFocus uint8

const (
	peopleFieldFocusName peopleFieldFocus = iota
	peopleFieldFocusType
	peopleFieldFocusCardinality
	peopleFieldFocusSave
	peopleFieldFocusCount
)

type peopleFormState struct {
	overlay peopleOverlay

	fieldFocus       peopleFieldFocus
	nameInput        textinput.Model
	fieldKindIndex   int
	cardinalityIndex int

	valueInput         textinput.Model
	valueTextarea      textarea.Model
	longText           bool
	definition         *store.AttributeDefinition
	draft              string
	ordinal            int64
	expectedValueID    int64
	editing            bool
	serverValue        string
	serverValuePresent bool
	notice             string
	submitting         bool
	staleConflict      bool
	staleReloadPending bool
}

func (f *peopleFormState) close() {
	f.nameInput.Blur()
	f.valueInput.Blur()
	if f.longText {
		f.valueTextarea.Blur()
	}
	*f = peopleFormState{}
}

func (f *peopleFormState) fieldKind() peoplebrowser.FieldKind {
	if f.fieldKindIndex < 0 || f.fieldKindIndex >= len(peoplebrowser.EditableFieldKinds) {
		return ""
	}
	return peoplebrowser.EditableFieldKinds[f.fieldKindIndex]
}

func (f *peopleFormState) cardinality() store.AttributeCardinality {
	if f.cardinalityIndex == 1 {
		return store.AttributeCardinalityMulti
	}
	return store.AttributeCardinalitySingle
}

func newPeopleFieldForm() peopleFormState {
	input := textinput.New()
	input.Placeholder = "display name"
	input.CharLimit = 200
	input.SetWidth(44)
	input.Focus()
	return peopleFormState{
		overlay: peopleOverlayNewField, fieldFocus: peopleFieldFocusName,
		nameInput: input,
	}
}

func newPeopleValueForm(
	definition store.AttributeDefinition, value *store.PersonAttributeValue,
	ordinal int64,
) peopleFormState {
	charLimit := peopleAttributeCharLimit(definition)
	input := textinput.New()
	input.Placeholder = peopleAttributePlaceholder(definition)
	input.CharLimit = charLimit
	input.SetWidth(54)
	input.Focus()
	form := peopleFormState{
		overlay: peopleOverlayAttributeValue, valueInput: input,
		definition: &definition, ordinal: ordinal,
	}
	if definition.FieldType == store.AttributeFieldTextarea {
		area := textarea.New()
		area.Placeholder = peopleAttributePlaceholder(definition)
		area.CharLimit = charLimit
		area.ShowLineNumbers = false
		area.SetWidth(54)
		area.SetHeight(5)
		area.Focus()
		form.longText = true
		form.valueTextarea = area
		form.valueInput.Blur()
	}
	if value != nil {
		form.editing = true
		form.expectedValueID = value.ID
		form.draft = peopleAttributeValueString(value.Value)
		form.serverValue = form.draft
		form.serverValuePresent = true
		form.valueInput.SetValue(form.draft)
		if form.longText {
			form.valueTextarea.SetValue(form.draft)
		}
	}
	return form
}

func peopleAttributeCharLimit(definition store.AttributeDefinition) int {
	if definition.Options == nil || definition.Options.MaxLength <= 0 {
		return 0
	}
	return definition.Options.MaxLength
}

func peopleAttributePlaceholder(definition store.AttributeDefinition) string {
	switch definition.ValueType {
	case store.AttributeValueReal:
		return "number"
	case store.AttributeValueBoolean:
		return "true or false"
	case store.AttributeValueDate:
		return "YYYY-MM-DD"
	case store.AttributeValueTimestamp:
		return "RFC3339 date and time"
	case store.AttributeValueJSON:
		return "valid JSON"
	default:
		return "value"
	}
}

func (m Model) handlePeopleFormKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch m.peopleState.form.overlay {
	case peopleOverlayNewField:
		return m.handlePeopleNewFieldKey(msg)
	case peopleOverlayAttributeValue:
		return m.handlePeopleAttributeValueKey(msg)
	default:
		return m, nil
	}
}

func (m Model) handlePeopleNewFieldKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	form := &m.peopleState.form
	if form.fieldFocus == peopleFieldFocusName && !form.submitting {
		switch msg.String() {
		case keyNameEsc, keyNameCtrlC, keyNameTab, "shift+tab", keyNameEnter:
		default:
			var cmd tea.Cmd
			form.nameInput, cmd = form.nameInput.Update(msg)
			form.notice = ""
			return m, cmd
		}
	}
	switch msg.String() {
	case keyNameEsc:
		if form.submitting {
			m.peopleState.requestID++
		}
		form.close()
		return m, nil
	case keyNameCtrlC:
		m.quitting = true
		return m, tea.Quit
	case keyNameTab:
		form.fieldFocus = (form.fieldFocus + 1) % peopleFieldFocusCount
		m.focusPeopleNewFieldInput()
		return m, nil
	case "shift+tab":
		form.fieldFocus = (form.fieldFocus + peopleFieldFocusCount - 1) % peopleFieldFocusCount
		m.focusPeopleNewFieldInput()
		return m, nil
	case "left", "h":
		m.changePeopleFieldChoice(-1)
		return m, nil
	case keyNameRight, "l", "j", "down":
		m.changePeopleFieldChoice(1)
		return m, nil
	case "k", "up":
		m.changePeopleFieldChoice(-1)
		return m, nil
	case keyNameEnter:
		if form.fieldFocus != peopleFieldFocusSave {
			form.fieldFocus = (form.fieldFocus + 1) % peopleFieldFocusCount
			m.focusPeopleNewFieldInput()
			return m, nil
		}
		return m.submitPeopleNewField()
	default:
		return m, nil
	}
}

func (m *Model) focusPeopleNewFieldInput() {
	if m.peopleState.form.fieldFocus == peopleFieldFocusName {
		m.peopleState.form.nameInput.Focus()
	} else {
		m.peopleState.form.nameInput.Blur()
	}
}

func (m *Model) changePeopleFieldChoice(delta int) {
	form := &m.peopleState.form
	switch form.fieldFocus {
	case peopleFieldFocusType:
		count := len(peoplebrowser.EditableFieldKinds)
		form.fieldKindIndex = (form.fieldKindIndex + delta + count) % count
	case peopleFieldFocusCardinality:
		form.cardinalityIndex = (form.cardinalityIndex + delta + 2) % 2
	case peopleFieldFocusName, peopleFieldFocusSave, peopleFieldFocusCount:
		return
	}
}

func (m Model) submitPeopleNewField() (tea.Model, tea.Cmd) {
	form := &m.peopleState.form
	if form.submitting {
		return m, nil
	}
	label := strings.TrimSpace(form.nameInput.Value())
	if label == "" {
		form.notice = "Name is required."
		return m, nil
	}
	field := peoplebrowser.NewField{
		Label: label, Kind: form.fieldKind(), Cardinality: form.cardinality(),
	}
	if _, err := field.DefinitionInput(); err != nil {
		form.notice = err.Error()
		return m, nil
	}
	form.notice = ""
	form.submitting = true
	m.peopleState.requestID++
	return m, m.createPeopleField(field)
}

func (m Model) handlePeopleAttributeValueKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	form := &m.peopleState.form
	switch msg.String() {
	case keyNameEsc:
		if form.submitting {
			m.peopleState.requestID++
		}
		form.close()
		return m, nil
	case keyNameCtrlC:
		m.quitting = true
		return m, tea.Quit
	case "ctrl+s":
		if form.longText {
			return m.submitPeopleAttribute()
		}
		return m, nil
	case keyNameEnter:
		if !form.longText {
			return m.submitPeopleAttribute()
		}
	}
	if form.submitting {
		return m, nil
	}
	var cmd tea.Cmd
	if form.longText {
		form.valueTextarea, cmd = form.valueTextarea.Update(msg)
		form.draft = form.valueTextarea.Value()
	} else {
		form.valueInput, cmd = form.valueInput.Update(msg)
		form.draft = form.valueInput.Value()
	}
	if !form.staleConflict {
		form.notice = ""
	}
	return m, cmd
}

func (m Model) submitPeopleAttribute() (tea.Model, tea.Cmd) {
	form := &m.peopleState.form
	if form.submitting || form.definition == nil || m.peopleState.contact == nil ||
		m.peopleState.contact.Profile == nil {
		return m, nil
	}
	if form.staleReloadPending {
		if m.peopleState.attributesLoading {
			return m, nil
		}
		form.notice = "Reloading the current server value before resubmission..."
		m.peopleState.requestID++
		m.peopleState.attributesLoading = true
		m.loading = true
		return m, m.loadPeopleAttributes(
			m.peopleState.contact.Profile.ID, m.peopleState.tab,
		)
	}
	form.draft = form.valueDraft()
	value, err := parsePeopleAttributeDraft(*form.definition, form.draft)
	if err != nil {
		form.notice = err.Error()
		return m, nil
	}
	request := peoplebrowser.SetAttributeRequest{
		PersonID: m.peopleState.contact.Profile.ID,
		Slug:     form.definition.Slug,
		Value:    value,
	}
	if form.editing {
		ordinal := form.ordinal
		request.Ordinal = &ordinal
	}
	if form.editing && form.expectedValueID > 0 {
		expected := form.expectedValueID
		request.ExpectedValueID = &expected
	}
	form.notice = ""
	form.submitting = true
	m.peopleState.requestID++
	return m, m.setPeopleAttribute(request)
}

func (f *peopleFormState) valueDraft() string {
	if f.longText {
		return f.valueTextarea.Value()
	}
	return f.valueInput.Value()
}

func parsePeopleAttributeDraft(
	definition store.AttributeDefinition, draft string,
) (store.AttributeValue, error) {
	invalid := func(format string, args ...any) (store.AttributeValue, error) {
		return store.AttributeValue{}, fmt.Errorf("invalid value: %s", fmt.Sprintf(format, args...))
	}
	if !peopleAttributeDefinitionSupported(definition) {
		return invalid("type %s with field %s is not editable here",
			definition.ValueType, definition.FieldType)
	}
	switch definition.ValueType {
	case store.AttributeValueText:
		if strings.TrimSpace(draft) == "" {
			return invalid("value must not be blank")
		}
		return store.AttributeValue{Type: store.AttributeValueText, Text: &draft}, nil
	case store.AttributeValueReal:
		value, err := strconv.ParseFloat(draft, 64)
		if err != nil {
			return invalid("enter a number")
		}
		return store.AttributeValue{Type: store.AttributeValueReal, Real: &value}, nil
	case store.AttributeValueBoolean:
		value, err := strconv.ParseBool(draft)
		if err != nil {
			return invalid("enter true or false")
		}
		return store.AttributeValue{Type: store.AttributeValueBoolean, Boolean: &value}, nil
	case store.AttributeValueDate:
		value, err := time.Parse("2006-01-02", draft)
		if err != nil || value.Format("2006-01-02") != draft {
			return invalid("enter an exact YYYY-MM-DD date")
		}
		return store.AttributeValue{Type: store.AttributeValueDate, Date: &draft}, nil
	case store.AttributeValueTimestamp:
		value, err := time.Parse(time.RFC3339Nano, draft)
		if err != nil {
			return invalid("enter an RFC3339 date and time")
		}
		return store.AttributeValue{Type: store.AttributeValueTimestamp, Timestamp: &value}, nil
	case store.AttributeValueJSON:
		raw := json.RawMessage(draft)
		if !json.Valid(raw) {
			return invalid("enter valid JSON")
		}
		return store.AttributeValue{Type: store.AttributeValueJSON, JSON: raw}, nil
	default:
		return invalid("type %s is not editable here", definition.ValueType)
	}
}

type peopleAttributeSelection struct {
	groupIndex int
	valueIndex int
}

func peopleAttributeSelections(attributes *peoplebrowser.Attributes) []peopleAttributeSelection {
	if attributes == nil {
		return nil
	}
	rows := make([]peopleAttributeSelection, 0)
	for groupIndex, group := range attributes.Groups {
		rows = append(rows, peopleAttributeSelection{groupIndex: groupIndex, valueIndex: -1})
		for valueIndex := range group.Current {
			rows = append(rows, peopleAttributeSelection{
				groupIndex: groupIndex, valueIndex: valueIndex,
			})
		}
	}
	return rows
}

func (m *Model) navigatePeopleAttributes(key string) bool {
	rows := peopleAttributeSelections(m.peopleState.attributes)
	count := len(rows)
	switch key {
	case "up", "k":
		m.peopleState.attributeCursor = max(m.peopleState.attributeCursor-1, 0)
	case keyNameDown, "j":
		m.peopleState.attributeCursor = min(m.peopleState.attributeCursor+1, max(count-1, 0))
	case keyNameHome:
		m.peopleState.attributeCursor = 0
	case keyNameEnd, "G":
		m.peopleState.attributeCursor = max(count-1, 0)
	default:
		return false
	}
	m.peopleState.attributeScrollOffset = calculateScrollOffset(
		m.peopleState.attributeCursor, m.peopleState.attributeScrollOffset,
		m.peopleAttributesDataRows(),
	)
	return true
}

func (m *Model) peopleAttributesDataRows() int {
	return max(m.visibleRows(), 1)
}

func (m Model) selectedPeopleAttribute() (
	peoplebrowser.AttributeGroup, *store.PersonAttributeValue, bool,
) {
	rows := peopleAttributeSelections(m.peopleState.attributes)
	if m.peopleState.attributeCursor < 0 || m.peopleState.attributeCursor >= len(rows) {
		return peoplebrowser.AttributeGroup{}, nil, false
	}
	selection := rows[m.peopleState.attributeCursor]
	group := m.peopleState.attributes.Groups[selection.groupIndex]
	if selection.valueIndex < 0 {
		return group, nil, true
	}
	value := group.Current[selection.valueIndex]
	return group, &value, true
}

func (m Model) peopleNotesAttribute() (
	peoplebrowser.AttributeGroup, *store.PersonAttributeValue, bool,
) {
	if !m.peopleState.attributesLoaded || m.peopleState.attributes == nil {
		return peoplebrowser.AttributeGroup{}, nil, false
	}
	for _, group := range m.peopleState.attributes.Groups {
		if group.Definition.UniversalID != store.AttributeUniversalIDNotes {
			continue
		}
		if len(group.Current) == 0 {
			return group, nil, true
		}
		value := group.Current[0]
		return group, &value, true
	}
	return peoplebrowser.AttributeGroup{}, nil, false
}

func (m Model) openPeopleAttributeAdd(group peoplebrowser.AttributeGroup) Model {
	if !peopleAttributeWritable(group.Definition) {
		m.peopleState.attributesNotice = "This field is read-only."
		return m
	}
	if group.Definition.Cardinality == store.AttributeCardinalitySingle && len(group.Current) > 0 {
		m.peopleState.attributesNotice = "Press e on the current value to edit it."
		return m
	}
	m.peopleState.attributesNotice = ""
	m.peopleState.form = newPeopleValueForm(group.Definition, nil, 0)
	return m
}

func (m Model) openPeopleAttributeEdit(
	group peoplebrowser.AttributeGroup, value *store.PersonAttributeValue,
) Model {
	if value == nil {
		m.peopleState.attributesNotice = "Select a current value to edit with e."
		return m
	}
	if !peopleAttributeWritable(group.Definition) {
		m.peopleState.attributesNotice = "This field is read-only."
		return m
	}
	m.peopleState.attributesNotice = ""
	m.peopleState.form = newPeopleValueForm(group.Definition, value, value.Ordinal)
	return m
}

func peopleAttributeWritable(definition store.AttributeDefinition) bool {
	return definition.IsActive && definition.UIEditable && definition.APIMutable &&
		peopleAttributeDefinitionSupported(definition)
}

func peopleAttributeDefinitionSupported(definition store.AttributeDefinition) bool {
	switch definition.ValueType {
	case store.AttributeValueText:
		switch definition.FieldType {
		case store.AttributeFieldText, store.AttributeFieldTextarea, store.AttributeFieldURL,
			store.AttributeFieldEmail, store.AttributeFieldPhone:
			return true
		case store.AttributeFieldSelect, store.AttributeFieldMultiselect,
			store.AttributeFieldCheckbox, store.AttributeFieldDate,
			store.AttributeFieldTimestamp, store.AttributeFieldDuration,
			store.AttributeFieldPerson, store.AttributeFieldOrganization,
			store.AttributeFieldJSON:
			return false
		}
	case store.AttributeValueInteger, store.AttributeValueRecordReference:
		return false
	case store.AttributeValueReal:
		return definition.FieldType == store.AttributeFieldText
	case store.AttributeValueBoolean:
		return definition.FieldType == store.AttributeFieldCheckbox
	case store.AttributeValueDate:
		return definition.FieldType == store.AttributeFieldDate
	case store.AttributeValueTimestamp:
		return definition.FieldType == store.AttributeFieldTimestamp
	case store.AttributeValueJSON:
		return definition.FieldType == store.AttributeFieldJSON
	}
	return false
}

func peopleAttributeValueString(value store.AttributeValue) string {
	switch value.Type {
	case store.AttributeValueText:
		if value.Text != nil {
			return *value.Text
		}
	case store.AttributeValueInteger:
		if value.Integer != nil {
			return strconv.FormatInt(*value.Integer, 10)
		}
	case store.AttributeValueReal:
		if value.Real != nil {
			return strconv.FormatFloat(*value.Real, 'g', -1, 64)
		}
	case store.AttributeValueBoolean:
		if value.Boolean != nil {
			return strconv.FormatBool(*value.Boolean)
		}
	case store.AttributeValueDate:
		if value.Date != nil {
			return *value.Date
		}
	case store.AttributeValueTimestamp:
		if value.Timestamp != nil {
			return value.Timestamp.Format(time.RFC3339Nano)
		}
	case store.AttributeValueJSON:
		return string(value.JSON)
	case store.AttributeValueRecordReference:
		return "—"
	}
	return "—"
}

func (m *Model) refreshStalePeopleForm(attributes *peoplebrowser.Attributes) {
	form := &m.peopleState.form
	if form.overlay != peopleOverlayAttributeValue || form.definition == nil ||
		!form.editing || !form.staleConflict || !form.staleReloadPending {
		return
	}
	for _, group := range attributes.Groups {
		if group.Definition.Slug != form.definition.Slug {
			continue
		}
		for _, value := range group.Current {
			if value.Ordinal != form.ordinal {
				continue
			}
			form.expectedValueID = value.ID
			form.serverValue = peopleAttributeValueString(value.Value)
			form.serverValuePresent = true
			form.staleReloadPending = false
			form.notice = "Server value reloaded. Review it against your draft, then press Enter to resubmit."
			return
		}
	}
	form.serverValue = ""
	form.serverValuePresent = false
	form.expectedValueID = 0
	form.staleReloadPending = false
	form.notice = "The server value is absent. Press Enter to recreate it from this draft, or Esc to cancel."
}

func (m Model) peopleAttributesLines() []string {
	contact := m.peopleState.contact
	if contact == nil || contact.Profile == nil {
		return []string{
			" Attributes are read-only for observed contacts.",
			" Press p to promote this contact before creating or editing attributes.",
		}
	}
	if m.peopleState.attributesLoading && !m.peopleState.attributesLoaded {
		return []string{m.spinnerIndicator() + " Loading attributes..."}
	}
	if !m.peopleState.attributesLoaded || m.peopleState.attributes == nil {
		return []string{" Attributes have not loaded. Press r to retry."}
	}
	rows := peopleAttributeSelections(m.peopleState.attributes)
	if len(rows) == 0 {
		return []string{" No attributes. Press n to create a custom field."}
	}
	start := min(max(m.peopleState.attributeScrollOffset, 0), max(len(rows)-1, 0))
	end := min(start+m.peopleAttributesDataRows(), len(rows))
	lines := make([]string, 0, end-start)
	for index := start; index < end; index++ {
		selection := rows[index]
		group := m.peopleState.attributes.Groups[selection.groupIndex]
		marker := listIndicatorBlank
		if index == m.peopleState.attributeCursor {
			marker = "▶  "
		}
		if selection.valueIndex < 0 {
			cardinality := "single"
			if group.Definition.Cardinality == store.AttributeCardinalityMulti {
				cardinality = "multiple"
			}
			writable := ""
			if !peopleAttributeWritable(group.Definition) {
				writable = " · read-only"
			}
			lines = append(lines, fmt.Sprintf("%s%s (%s%s)", marker,
				textutil.SanitizeTerminal(group.Definition.Label), cardinality, writable))
			continue
		}
		value := group.Current[selection.valueIndex]
		lines = append(lines, fmt.Sprintf("%s  %s", marker,
			textutil.SanitizeTerminal(peopleAttributeValueString(value.Value))))
	}
	return lines
}

func (m Model) renderPeopleFormOverlay(background string) string {
	if m.peopleState.form.overlay == peopleOverlayNone {
		return background
	}
	content := m.peopleFormView()
	if content == "" {
		return background
	}
	modal := m.styles.modal.Render(content)
	bgLines := strings.Split(background, "\n")
	modalLines := strings.Split(modal, "\n")
	startLine := max((len(bgLines)-len(modalLines))/2, 0)
	modalWidth := lipgloss.Width(modal)
	leftPadding := max((m.width-modalWidth)/2, 0)
	for i, modalLine := range modalLines {
		lineIndex := startLine + i
		if lineIndex >= len(bgLines) {
			break
		}
		backgroundLine := bgLines[lineIndex]
		var composite strings.Builder
		left := truncateToWidth(backgroundLine, leftPadding)
		composite.WriteString(left)
		if width := lipgloss.Width(left); width < leftPadding {
			composite.WriteString(strings.Repeat(" ", leftPadding-width))
		}
		composite.WriteString(modalLine)
		rightStart := leftPadding + modalWidth
		if rightStart < lipgloss.Width(backgroundLine) {
			composite.WriteString(skipToWidth(backgroundLine, rightStart))
		}
		bgLines[lineIndex] = composite.String()
	}
	return strings.Join(bgLines, "\n")
}

func (m Model) peopleFormView() string {
	form := m.peopleState.form
	switch form.overlay {
	case peopleOverlayNewField:
		return m.peopleNewFieldFormView()
	case peopleOverlayAttributeValue:
		return m.peopleAttributeValueFormView()
	default:
		return ""
	}
}

func (m Model) peopleNewFieldFormView() string {
	form := m.peopleState.form
	line := func(focus peopleFieldFocus, label, value string) string {
		marker := "  "
		if form.fieldFocus == focus {
			marker = "▶ "
		}
		return marker + label + ": " + value
	}
	kind := peopleFieldKindLabel(form.fieldKind())
	cardinality := "Single"
	if form.cardinality() == store.AttributeCardinalityMulti {
		cardinality = "Multiple"
	}
	lines := []string{
		m.styles.modalTitle.Render("New custom field"), "",
		line(peopleFieldFocusName, "Name", form.nameInput.View()),
		line(peopleFieldFocusType, "Type", "‹ "+kind+" ›"),
		line(peopleFieldFocusCardinality, "Cardinality", "‹ "+cardinality+" ›"),
		line(peopleFieldFocusSave, "Save", helpLabelEnter),
	}
	if form.submitting {
		lines = append(lines, "", "Saving field...")
	} else if form.notice != "" {
		lines = append(lines, "", m.styles.err.Render(form.notice))
	}
	lines = append(lines, "", "Tab moves · Esc cancels")
	return strings.Join(lines, "\n")
}

func peopleFieldKindLabel(kind peoplebrowser.FieldKind) string {
	switch kind {
	case peoplebrowser.FieldKindText:
		return "Text"
	case peoplebrowser.FieldKindLongText:
		return "Long text"
	case peoplebrowser.FieldKindNumber:
		return "Number"
	case peoplebrowser.FieldKindCheckbox:
		return "Checkbox"
	case peoplebrowser.FieldKindDate:
		return "Date"
	case peoplebrowser.FieldKindDateTime:
		return "Datetime"
	case peoplebrowser.FieldKindURL:
		return "URL"
	case peoplebrowser.FieldKindEmail:
		return "Email"
	case peoplebrowser.FieldKindPhone:
		return "Phone"
	case peoplebrowser.FieldKindJSON:
		return "JSON"
	default:
		return "Unsupported"
	}
}

func (m Model) peopleAttributeValueFormView() string {
	form := m.peopleState.form
	if form.definition == nil {
		return ""
	}
	title := "Add " + textutil.SanitizeTerminal(form.definition.Label)
	if form.editing {
		title = "Edit " + textutil.SanitizeTerminal(form.definition.Label)
	}
	valueView := form.valueInput.View()
	if form.longText {
		valueView = "\n" + form.valueTextarea.View()
	}
	lines := []string{m.styles.modalTitle.Render(title), "", "Value: " + valueView}
	if form.staleConflict {
		lines = append(lines, "")
		if !form.staleReloadPending && form.serverValuePresent {
			lines = append(lines, "Server: "+textutil.SanitizeTerminal(form.serverValue))
		}
		lines = append(lines, "Draft:  "+textutil.SanitizeTerminal(form.draft))
	}
	if form.submitting {
		lines = append(lines, "", "Saving value...")
	} else if form.notice != "" {
		lines = append(lines, "", m.styles.err.Render(form.notice))
	}
	if form.longText {
		lines = append(lines, "", "Ctrl+S saves · Enter inserts newline · Esc cancels")
	} else {
		lines = append(lines, "", "Enter saves · Esc cancels")
	}
	return strings.Join(lines, "\n")
}

func stalePeopleValue(err error) (peoplebrowser.StaleValueError, bool) {
	var stale peoplebrowser.StaleValueError
	return stale, errors.As(err, &stale)
}
