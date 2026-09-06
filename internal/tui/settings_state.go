package tui

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
)

// SettingsBackend is the daemon settings boundary used by the TUI. The TUI
// intentionally owns these provider-neutral types instead of depending on a
// generated HTTP client contract.
type SettingsBackend interface {
	LoadSettings(ctx context.Context) (SettingsSnapshot, error)
	SaveSettings(ctx context.Context, request SettingsSaveRequest) (SettingsSnapshot, error)
}

// SettingsConflictScope identifies which optimistic-concurrency token became
// stale. Config and provider credentials are separate persistence domains.
type SettingsConflictScope string

const (
	SettingsConflictConfig      SettingsConflictScope = "config"
	SettingsConflictCredentials SettingsConflictScope = "credentials"
)

// SettingsConflictError reports an optimistic-concurrency conflict. The TUI
// reloads the latest snapshot and reapplies the user's unsaved drafts.
type SettingsConflictError struct {
	Scope SettingsConflictScope
	Err   error
}

func (e *SettingsConflictError) Error() string {
	if e == nil || e.Err == nil {
		return "settings changed concurrently"
	}
	return e.Err.Error()
}

func (e *SettingsConflictError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// SettingsPartialSaveError reports that the backend committed only a subset
// of the requested keys. SavedKeys lets the TUI clear those drafts while
// retaining every mutation that still needs attention.
type SettingsPartialSaveError struct {
	SavedKeys []string
	Err       error
}

func (e *SettingsPartialSaveError) Error() string {
	if e == nil || e.Err == nil {
		return "settings were only partially saved"
	}
	return e.Err.Error()
}

func (e *SettingsPartialSaveError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// SettingKind identifies the editor and renderer for one setting.
type SettingKind string

const (
	SettingKindString      SettingKind = "string"
	SettingKindInteger     SettingKind = "integer"
	SettingKindNumber      SettingKind = "number"
	SettingKindBoolean     SettingKind = "boolean"
	SettingKindStringArray SettingKind = "string_array"
	SettingKindSecret      SettingKind = "secret"
)

// SettingsGroup is a daemon-described settings category.
type SettingsGroup struct {
	ID    string
	Label string
}

// SettingValidation describes safe, non-secret input constraints.
type SettingValidation struct {
	Hint     string
	Required bool
	Minimum  *float64
	Maximum  *float64
}

// SecretSettingState is the only secret state that may be loaded or rendered.
// Secret values themselves are write-only.
type SecretSettingState struct {
	Configured bool
	Source     string
}

// SettingValue mirrors the daemon's explicit scalar union without importing a
// generated API package into the TUI.
type SettingValue struct {
	String  *string
	Integer *int
	Number  *float64
	Boolean *bool
	Strings *[]string
}

// SettingField describes one daemon-managed setting.
type SettingField struct {
	Key             string
	CredentialID    string
	Group           string
	Label           string
	Description     string
	Kind            SettingKind
	Value           *SettingValue
	Secret          *SecretSettingState
	Options         []string
	ReadOnly        bool
	RestartRequired bool
	Validation      SettingValidation
}

// SettingsSnapshot is one ETag-addressed settings read.
type SettingsSnapshot struct {
	ETag           string
	CredentialETag string
	Groups         []SettingsGroup
	Fields         []SettingField
	PendingRestart bool
}

// CredentialUpdate is a write-only provider-credential mutation. It is kept
// separate from SettingUpdate so callers cannot accidentally send secrets to
// PATCH /settings.
type CredentialUpdate struct {
	Key          string
	CredentialID string
	Action       string
	Value        string
}

// ConfigSecretUpdate is a write-only legacy config-file secret mutation. It
// uses PATCH /settings and the config ETag, unlike provider credentials.
type ConfigSecretUpdate struct {
	Key    string
	Action string
	Value  string
}

// SettingUpdate is one non-secret PATCH /settings mutation.
type SettingUpdate struct {
	Key   string
	Value *SettingValue
}

// SettingsSaveRequest carries both concurrency tokens while keeping config
// and credential mutations on distinct lanes for the daemon adapter.
type SettingsSaveRequest struct {
	ConfigETag     string
	CredentialETag string
	Updates        []SettingUpdate
	ConfigSecrets  []ConfigSecretUpdate
	Credentials    []CredentialUpdate
}

type settingsState struct {
	active         bool
	loading        bool
	saving         bool
	confirmDiscard bool
	narrowFields   bool

	groups             []SettingsGroup
	fields             []SettingField
	etag               string
	credentialETag     string
	pendingRestart     bool
	groupCursor        int
	rowCursor          int
	drafts             map[string]SettingUpdate
	configSecretDrafts map[string]ConfigSecretUpdate
	credentialDrafts   map[string]CredentialUpdate
	status             string
	statusIsError      bool
	requestID          uint64

	editing     bool
	editKey     string
	editOptions []string
	editOption  int
	editor      textinput.Model
}

func newSettingsState() settingsState {
	input := textinput.New()
	input.CharLimit = 2048
	input.SetWidth(48)
	return settingsState{
		drafts:             make(map[string]SettingUpdate),
		configSecretDrafts: make(map[string]ConfigSecretUpdate),
		credentialDrafts:   make(map[string]CredentialUpdate),
		editor:             input,
	}
}

func (s settingsState) dirty() bool {
	return len(s.drafts) > 0 || len(s.configSecretDrafts) > 0 || len(s.credentialDrafts) > 0
}

// settingsShortcutAvailable reports whether comma is a shell command in the
// current interaction state. Active text inputs own printable characters.
func (m Model) settingsShortcutAvailable() bool {
	if m.modal != modalNone {
		return false
	}
	switch m.mode {
	case modePeople:
		if m.peopleState.form.overlay != peopleOverlayNone || m.peopleState.searchActive {
			return false
		}
		if (m.peopleState.level == peopleLevelMessage ||
			m.peopleState.level == peopleLevelActivityMessage) && m.detailSearchActive {
			return false
		}
		return m.peopleState.level != peopleLevelMeetingDetail || !m.meetingState.detailSearchActive
	case modeMeetings:
		return !m.meetingState.searchActive &&
			(m.meetingState.level != meetingLevelDetail || !m.meetingState.detailSearchActive)
	case modeTexts:
		return !m.inlineSearchActive &&
			(m.textState.level != textLevelDetail || !m.detailSearchActive)
	default:
		return !m.inlineSearchActive &&
			(m.level != levelMessageDetail || !m.detailSearchActive)
	}
}

func (s settingsState) currentGroup() string {
	if s.groupCursor < 0 || s.groupCursor >= len(s.groups) {
		return ""
	}
	return s.groups[s.groupCursor].ID
}

func (s settingsState) currentFields() []SettingField {
	group := s.currentGroup()
	result := make([]SettingField, 0)
	for _, field := range s.fields {
		if field.Group == group {
			result = append(result, field)
		}
	}
	return result
}

func (s settingsState) selectedField() (SettingField, bool) {
	fields := s.currentFields()
	if s.rowCursor < 0 || s.rowCursor >= len(fields) {
		return SettingField{}, false
	}
	return fields[s.rowCursor], true
}

type settingsLoadedMsg struct {
	snapshot  SettingsSnapshot
	err       error
	requestID uint64
}

type settingsSavedMsg struct {
	snapshot  SettingsSnapshot
	err       error
	requestID uint64
	conflict  bool
	partial   bool
	savedKeys []string
	reloadErr error
}

func (m Model) openSettings() (tea.Model, tea.Cmd) {
	m.settings = newSettingsState()
	m.settings.active = true
	m.settings.loading = true
	m.settingsRequestID++
	m.settings.requestID = m.settingsRequestID
	requestID := m.settingsRequestID
	backend := m.settingsBackend
	if backend == nil {
		return m, func() tea.Msg {
			return settingsLoadedMsg{err: errors.New("settings are unavailable"), requestID: requestID}
		}
	}
	return m, func() tea.Msg {
		snapshot, err := backend.LoadSettings(context.Background())
		return settingsLoadedMsg{snapshot: snapshot, err: err, requestID: requestID}
	}
}

func (m Model) handleSettingsLoaded(msg settingsLoadedMsg) (tea.Model, tea.Cmd) {
	if !m.settings.active || msg.requestID != m.settings.requestID {
		return m, nil
	}
	m.settings.loading = false
	if msg.err != nil {
		m.settings.status = "Could not load settings: " + msg.err.Error()
		m.settings.statusIsError = true
		return m, nil
	}
	m.applySettingsSnapshot(msg.snapshot, false)
	return m, nil
}

func (m *Model) applySettingsSnapshot(snapshot SettingsSnapshot, preserveDrafts bool) {
	previousDrafts := m.settings.drafts
	previousConfigSecretDrafts := m.settings.configSecretDrafts
	previousCredentialDrafts := m.settings.credentialDrafts
	fields := make([]SettingField, 0, len(snapshot.Fields))
	availableGroups := make(map[string]bool)
	for _, field := range snapshot.Fields {
		if strings.HasPrefix(field.Key, "web.") {
			continue
		}
		fields = append(fields, field)
		availableGroups[field.Group] = true
	}

	groups := make([]SettingsGroup, 0, len(snapshot.Groups))
	seenGroups := make(map[string]bool)
	for _, group := range snapshot.Groups {
		if !availableGroups[group.ID] || seenGroups[group.ID] {
			continue
		}
		groups = append(groups, group)
		seenGroups[group.ID] = true
	}
	for _, field := range fields {
		if seenGroups[field.Group] {
			continue
		}
		groups = append(groups, SettingsGroup{ID: field.Group, Label: settingsGroupLabel(field.Group)})
		seenGroups[field.Group] = true
	}

	m.settings.groups = groups
	m.settings.fields = fields
	m.settings.etag = snapshot.ETag
	m.settings.credentialETag = snapshot.CredentialETag
	m.settings.pendingRestart = snapshot.PendingRestart
	m.settings.loading = false
	m.settings.saving = false
	m.settings.confirmDiscard = false
	m.settings.editing = false
	if m.settings.groupCursor >= len(groups) {
		m.settings.groupCursor = max(len(groups)-1, 0)
	}
	m.clampSettingsRow()

	if !preserveDrafts {
		m.settings.drafts = make(map[string]SettingUpdate)
		m.settings.configSecretDrafts = make(map[string]ConfigSecretUpdate)
		m.settings.credentialDrafts = make(map[string]CredentialUpdate)
		m.settings.status = ""
		m.settings.statusIsError = false
		return
	}

	editable := make(map[string]SettingField, len(fields))
	for _, field := range fields {
		if !field.ReadOnly {
			editable[field.Key] = field
		}
	}
	m.settings.drafts = make(map[string]SettingUpdate, len(previousDrafts))
	m.settings.configSecretDrafts = make(map[string]ConfigSecretUpdate, len(previousConfigSecretDrafts))
	m.settings.credentialDrafts = make(map[string]CredentialUpdate, len(previousCredentialDrafts))
	for key, draft := range previousDrafts {
		if _, ok := editable[key]; ok {
			m.settings.drafts[key] = draft
		}
	}
	for key, draft := range previousConfigSecretDrafts {
		field, ok := editable[key]
		if ok && field.Kind == SettingKindSecret && settingCredentialID(field) == "" {
			m.settings.configSecretDrafts[key] = draft
		}
	}
	for key, draft := range previousCredentialDrafts {
		field, ok := editable[key]
		if ok && field.Kind == SettingKindSecret && settingCredentialID(field) == draft.CredentialID {
			m.settings.credentialDrafts[key] = draft
		}
	}
	m.settings.status = "Settings changed elsewhere. Drafts kept; review and save again."
	m.settings.statusIsError = true
}

func settingsGroupLabel(group string) string {
	if group == "" {
		return "Other"
	}
	words := strings.Fields(strings.NewReplacer("_", " ", "-", " ", ".", " ").Replace(group))
	for i := range words {
		words[i] = strings.ToUpper(words[i][:1]) + words[i][1:]
	}
	return strings.Join(words, " ")
}

func (m *Model) clampSettingsRow() {
	fields := m.settings.currentFields()
	if len(fields) == 0 {
		m.settings.rowCursor = 0
		return
	}
	m.settings.rowCursor = min(max(m.settings.rowCursor, 0), len(fields)-1)
}

func (m Model) saveSettings() (tea.Model, tea.Cmd) {
	if m.settings.saving || !m.settings.dirty() {
		if !m.settings.dirty() {
			m.settings.status = "No unsaved changes."
			m.settings.statusIsError = false
		}
		return m, nil
	}
	updates := m.orderedSettingsUpdates()
	configSecrets := m.orderedConfigSecretUpdates()
	credentials := m.orderedCredentialUpdates()
	backend := m.settingsBackend
	if backend == nil {
		m.settings.status = "Settings are unavailable."
		m.settings.statusIsError = true
		return m, nil
	}
	m.settings.saving = true
	m.settings.confirmDiscard = false
	m.settingsRequestID++
	m.settings.requestID = m.settingsRequestID
	requestID := m.settingsRequestID
	request := SettingsSaveRequest{
		ConfigETag:     m.settings.etag,
		CredentialETag: m.settings.credentialETag,
		Updates:        updates,
		ConfigSecrets:  configSecrets,
		Credentials:    credentials,
	}
	return m, func() tea.Msg {
		snapshot, err := backend.SaveSettings(context.Background(), request)
		var conflict *SettingsConflictError
		var partial *SettingsPartialSaveError
		if !errors.As(err, &conflict) && !errors.As(err, &partial) {
			return settingsSavedMsg{snapshot: snapshot, err: err, requestID: requestID}
		}
		latest, reloadErr := backend.LoadSettings(context.Background())
		msg := settingsSavedMsg{
			err: err, requestID: requestID, conflict: true,
			snapshot: latest, reloadErr: reloadErr,
		}
		if partial != nil {
			msg.conflict = false
			msg.partial = true
			msg.savedKeys = append([]string(nil), partial.SavedKeys...)
		}
		return msg
	}
}

func (m Model) orderedSettingsUpdates() []SettingUpdate {
	updates := make([]SettingUpdate, 0, len(m.settings.drafts))
	seen := make(map[string]bool, len(m.settings.drafts))
	for _, field := range m.settings.fields {
		if update, ok := m.settings.drafts[field.Key]; ok {
			updates = append(updates, update)
			seen[field.Key] = true
		}
	}
	keys := make([]string, 0)
	for key := range m.settings.drafts {
		if !seen[key] {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	for _, key := range keys {
		updates = append(updates, m.settings.drafts[key])
	}
	return updates
}

func (m Model) orderedCredentialUpdates() []CredentialUpdate {
	updates := make([]CredentialUpdate, 0, len(m.settings.credentialDrafts))
	seen := make(map[string]bool, len(m.settings.credentialDrafts))
	for _, field := range m.settings.fields {
		if update, ok := m.settings.credentialDrafts[field.Key]; ok {
			updates = append(updates, update)
			seen[field.Key] = true
		}
	}
	keys := make([]string, 0)
	for key := range m.settings.credentialDrafts {
		if !seen[key] {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	for _, key := range keys {
		updates = append(updates, m.settings.credentialDrafts[key])
	}
	return updates
}

func (m Model) orderedConfigSecretUpdates() []ConfigSecretUpdate {
	updates := make([]ConfigSecretUpdate, 0, len(m.settings.configSecretDrafts))
	seen := make(map[string]bool, len(m.settings.configSecretDrafts))
	for _, field := range m.settings.fields {
		if update, ok := m.settings.configSecretDrafts[field.Key]; ok {
			updates = append(updates, update)
			seen[field.Key] = true
		}
	}
	keys := make([]string, 0)
	for key := range m.settings.configSecretDrafts {
		if !seen[key] {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	for _, key := range keys {
		updates = append(updates, m.settings.configSecretDrafts[key])
	}
	return updates
}

func (m Model) handleSettingsSaved(msg settingsSavedMsg) (tea.Model, tea.Cmd) {
	if !m.settings.active || msg.requestID != m.settings.requestID {
		return m, nil
	}
	m.settings.saving = false
	if msg.partial {
		m.dropSavedSettingsDrafts(msg.savedKeys)
		if msg.reloadErr == nil {
			m.applySettingsSnapshot(msg.snapshot, true)
			m.dropSavedSettingsDrafts(msg.savedKeys)
		}
		m.settings.status = "Some settings were saved. Remaining drafts kept; review and save again."
		if msg.reloadErr != nil {
			m.settings.status += " The latest settings could not be reloaded."
		}
		m.settings.statusIsError = true
		return m, nil
	}
	if msg.conflict {
		if msg.reloadErr != nil {
			m.settings.status = "Settings changed elsewhere; drafts kept, but the latest settings could not be loaded."
			m.settings.statusIsError = true
			return m, nil
		}
		m.applySettingsSnapshot(msg.snapshot, true)
		return m, nil
	}
	if msg.err != nil {
		m.settings.status = "Could not save settings: " + m.redactSettingsSecrets(msg.err.Error())
		m.settings.statusIsError = true
		return m, nil
	}
	m.applySettingsSnapshot(msg.snapshot, false)
	m.settings.status = "Settings saved."
	m.settings.statusIsError = false
	return m, nil
}

func (m Model) redactSettingsSecrets(message string) string {
	secrets := make([]string, 0, len(m.settings.configSecretDrafts)+len(m.settings.credentialDrafts))
	for key, draft := range m.settings.configSecretDrafts {
		field, ok := m.settingFieldByKey(key)
		if !ok || field.Kind != SettingKindSecret || draft.Value == "" {
			continue
		}
		secrets = append(secrets, draft.Value)
	}
	for key, draft := range m.settings.credentialDrafts {
		field, ok := m.settingFieldByKey(key)
		if !ok || field.Kind != SettingKindSecret || draft.Value == "" {
			continue
		}
		secrets = append(secrets, draft.Value)
	}
	sort.SliceStable(secrets, func(i, j int) bool {
		return len(secrets[i]) > len(secrets[j])
	})
	for _, secret := range secrets {
		message = strings.ReplaceAll(message, secret, "[redacted]")
	}
	return message
}

func (m Model) settingFieldByKey(key string) (SettingField, bool) {
	for _, field := range m.settings.fields {
		if field.Key == key {
			return field, true
		}
	}
	return SettingField{}, false
}

func (m *Model) setSettingsDraft(field SettingField, update SettingUpdate) {
	if m.settings.drafts == nil {
		m.settings.drafts = make(map[string]SettingUpdate)
	}
	if reflect.DeepEqual(field.Value, update.Value) {
		delete(m.settings.drafts, field.Key)
	} else {
		m.settings.drafts[field.Key] = update
	}
}

func (m *Model) setCredentialDraft(field SettingField, update CredentialUpdate) {
	if m.settings.credentialDrafts == nil {
		m.settings.credentialDrafts = make(map[string]CredentialUpdate)
	}
	m.settings.credentialDrafts[field.Key] = update
}

func (m *Model) setConfigSecretDraft(field SettingField, update ConfigSecretUpdate) {
	if m.settings.configSecretDrafts == nil {
		m.settings.configSecretDrafts = make(map[string]ConfigSecretUpdate)
	}
	m.settings.configSecretDrafts[field.Key] = update
}

func settingCredentialID(field SettingField) string {
	return strings.TrimSpace(field.CredentialID)
}

func (m *Model) dropSavedSettingsDrafts(keys []string) {
	for _, key := range keys {
		delete(m.settings.drafts, key)
		delete(m.settings.configSecretDrafts, key)
		delete(m.settings.credentialDrafts, key)
	}
}

func settingValueText(value *SettingValue) string {
	if value == nil {
		return ""
	}
	switch {
	case value.String != nil:
		return *value.String
	case value.Integer != nil:
		return strconv.Itoa(*value.Integer)
	case value.Number != nil:
		return strconv.FormatFloat(*value.Number, 'g', -1, 64)
	case value.Boolean != nil:
		return strconv.FormatBool(*value.Boolean)
	case value.Strings != nil:
		return strings.Join(*value.Strings, ", ")
	default:
		return ""
	}
}

func settingUpdateText(update SettingUpdate) string {
	return settingValueText(update.Value)
}

func parseSettingInput(field SettingField, raw string) (*SettingValue, error) {
	trimmed := strings.TrimSpace(raw)
	if field.Validation.Required && trimmed == "" {
		return nil, errors.New("a value is required")
	}
	var value SettingValue
	switch field.Kind {
	case SettingKindString:
		value.String = &raw
	case SettingKindInteger:
		parsed, err := strconv.Atoi(trimmed)
		if err != nil {
			return nil, errors.New("enter a whole number")
		}
		value.Integer = &parsed
		if err := validateSettingNumber(field.Validation, float64(parsed)); err != nil {
			return nil, err
		}
	case SettingKindNumber:
		parsed, err := strconv.ParseFloat(trimmed, 64)
		if err != nil {
			return nil, errors.New("enter a number")
		}
		value.Number = &parsed
		if err := validateSettingNumber(field.Validation, parsed); err != nil {
			return nil, err
		}
	case SettingKindStringArray:
		items := make([]string, 0)
		for item := range strings.SplitSeq(raw, ",") {
			if item = strings.TrimSpace(item); item != "" {
				items = append(items, item)
			}
		}
		value.Strings = &items
	default:
		return nil, fmt.Errorf("unsupported setting kind %q", field.Kind)
	}
	return &value, nil
}

func validateSettingNumber(validation SettingValidation, value float64) error {
	if validation.Minimum != nil && value < *validation.Minimum {
		return fmt.Errorf("value must be at least %s", strconv.FormatFloat(*validation.Minimum, 'g', -1, 64))
	}
	if validation.Maximum != nil && value > *validation.Maximum {
		return fmt.Errorf("value must be at most %s", strconv.FormatFloat(*validation.Maximum, 'g', -1, 64))
	}
	return nil
}
