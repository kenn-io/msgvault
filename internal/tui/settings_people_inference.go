package tui

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"go.kenn.io/msgvault/internal/textutil"
)

// PeopleInferenceBackend performs enrollment on the daemon. The TUI never
// stores credentials or reproduces the daemon's check and consent rules.
type PeopleInferenceBackend interface {
	StartCodexLogin(ctx context.Context, profile string) (CodexDeviceLogin, error)
	PollCodexLogin(ctx context.Context, sessionID string) (CodexLoginPoll, error)
	CancelCodexLogin(ctx context.Context, sessionID string) error
	ListCodexModels(ctx context.Context, sessionID string) ([]CodexModelChoice, error)
	SaveCodexProfile(ctx context.Context, sessionID string, profile CodexProfileRequest) (string, error)
	CheckCodexProfile(ctx context.Context, profile string) (PeopleInferenceDisclosure, error)
	ConsentCodexProfile(ctx context.Context, profile, fingerprint string) error
	SelectCodexProfile(ctx context.Context, profile string) error
	LoadPeopleInferenceStatus(ctx context.Context) (PeopleInferenceStatus, error)
}

type CodexDeviceLogin struct {
	DraftID   string
	SessionID string
	URL       string
	Code      string
	Deadline  time.Time
}

type CodexLoginPoll struct{ Complete bool }

type CodexModelChoice struct {
	ID                     string
	DefaultReasoningEffort string
	ReasoningEfforts       []string
}

type CodexProfileRequest struct {
	Name             string
	Model            string
	ReasoningEffort  string
	RetentionPosture string
	TrainingPosture  string
	AllowedSources   []string
	SourceSince      string
	SourceUntil      string
	AllowSensitive   bool
}

type PeopleInferenceDisclosure struct {
	Profile     string
	Fingerprint string
	Text        string
}

type PeopleInferenceStatus struct {
	Configured            string
	ConfiguredFingerprint string
	ConfiguredEnabled     bool
	Running               string
	RunningFingerprint    string
	RunningEnabled        bool
	PendingRestart        bool
}

// PeopleInferencePresetRequest contains policy fields for a vendor-bound
// profile. A key and endpoint are deliberately absent: the daemon binds the
// destination before a separate credential write is allowed.
type PeopleInferencePresetRequest struct {
	PresetID         string
	Model            string
	RetentionPosture string
	TrainingPosture  string
	AllowedSources   []string
	SourceSince      string
	SourceUntil      string
	AllowSensitive   bool
}

type codexSettingsState struct {
	active       bool
	requestID    uint64
	stage        string
	policy       CodexProfileRequest
	sensitiveSet bool
	editing      string
	editor       textinput.Model
	login        CodexDeviceLogin
	models       []CodexModelChoice
	modelCursor  int
	effortCursor int
	profile      string
	disclosure   PeopleInferenceDisclosure
	consented    bool
	status       PeopleInferenceStatus
	message      string
	ctx          context.Context
	cancel       context.CancelFunc
}

type codexLoginStartedMsg struct {
	login     CodexDeviceLogin
	err       error
	requestID uint64
}
type codexLoginPolledMsg struct {
	poll      CodexLoginPoll
	err       error
	requestID uint64
}
type codexModelsLoadedMsg struct {
	models    []CodexModelChoice
	err       error
	requestID uint64
}
type codexProfileSavedMsg struct {
	profile   string
	err       error
	requestID uint64
}
type codexCheckedMsg struct {
	disclosure PeopleInferenceDisclosure
	err        error
	requestID  uint64
}
type codexConsentedMsg struct {
	err       error
	requestID uint64
}
type codexSelectedMsg struct {
	status    PeopleInferenceStatus
	err       error
	requestID uint64
}
type codexStatusLoadedMsg struct {
	status    PeopleInferenceStatus
	err       error
	requestID uint64
}
type codexPollTickMsg struct{ requestID uint64 }

func (m Model) peopleInferenceBackend() PeopleInferenceBackend {
	backend, _ := m.settingsBackend.(PeopleInferenceBackend)
	return backend
}

func (m Model) openCodexSettings() (tea.Model, tea.Cmd) {
	backend := m.peopleInferenceBackend()
	if backend == nil {
		return m, nil
	}
	m.settingsRequestID++
	id := m.settingsRequestID
	ctx, cancel := context.WithCancel(context.Background())
	m.settings.codex = codexSettingsState{active: true, stage: "setup", requestID: id, ctx: ctx, cancel: cancel}
	return m, nil
}

func (m Model) closeCodexSettings() (Model, tea.Cmd) {
	s := m.settings.codex
	if s.cancel != nil {
		s.cancel()
	}
	if s.stage == "selected" {
		status := s.status
		m.settings.peopleInferenceStatus = &status
	}
	m.settings.codex = codexSettingsState{}
	if s.login.SessionID == "" {
		return m, nil
	}
	backend := m.peopleInferenceBackend()
	return m, func() tea.Msg {
		_ = backend.CancelCodexLogin(context.Background(), s.login.SessionID)
		return nil
	}
}

func (m Model) handleCodexSettingsKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	s := &m.settings.codex
	if s.stage == "setup" && s.editing != "" {
		return m.handleCodexPolicyEdit(msg)
	}
	if s.stage == "saving" && (msg.String() == keyNameEsc || msg.String() == keyNameCtrlC) {
		s.message = "Profile save in progress; wait for the result."
		return m, nil
	}
	switch msg.String() {
	case keyNameEsc:
		return m.closeCodexSettings()
	case keyNameCtrlC:
		updated, cancel := m.closeCodexSettings()
		m = updated
		m.quitting = true
		if cancel == nil {
			return m, tea.Quit
		}
		return m, func() tea.Msg { _ = cancel(); return tea.Quit() }
	}
	backend := m.peopleInferenceBackend()
	id := s.requestID
	session := s.login.SessionID
	ctx := s.ctx
	if s.stage == "setup" {
		switch msg.String() {
		case "1", "2", "3", "4", "5":
			return m.beginCodexPolicyEdit(msg.String())
		case "c", "m", "d":
			source := map[string]string{"c": "conversation_text", "m": "meeting_text", "d": "document_text"}[msg.String()]
			if slices.Contains(s.policy.AllowedSources, source) {
				s.policy.AllowedSources = slices.DeleteFunc(s.policy.AllowedSources, func(v string) bool { return v == source })
			} else {
				s.policy.AllowedSources = append(s.policy.AllowedSources, source)
			}
			return m, nil
		case "y", "n":
			s.sensitiveSet = true
			s.policy.AllowSensitive = msg.String() == "y"
			return m, nil
		case keyNameEnter:
			if err := s.validatePolicy(); err != nil {
				s.message = err.Error()
				return m, nil
			}
			s.stage, s.message = "starting", ""
			name := s.policy.Name
			return m, func() tea.Msg {
				login, err := backend.StartCodexLogin(ctx, name)
				return codexLoginStartedMsg{login: login, err: err, requestID: id}
			}
		}
		return m, nil
	}
	switch msg.String() {
	case "r":
		if s.stage == "waiting" && session != "" {
			return m, m.pollCodexLogin(id, session)
		}
		return m, func() tea.Msg {
			status, err := backend.LoadPeopleInferenceStatus(ctx)
			return codexStatusLoadedMsg{status: status, err: err, requestID: id}
		}
	case "m":
		if s.stage == "authenticated" || (s.stage == "models" && len(s.models) == 0) {
			return m, func() tea.Msg {
				models, err := backend.ListCodexModels(ctx, s.login.DraftID)
				return codexModelsLoadedMsg{models: models, err: err, requestID: id}
			}
		}
	case "up", "k":
		if s.stage == "models" && s.modelCursor > 0 {
			s.modelCursor--
			s.selectDefaultEffort()
		}
	case "down", "j":
		if s.stage == "models" && s.modelCursor+1 < len(s.models) {
			s.modelCursor++
			s.selectDefaultEffort()
		}
	case "e":
		if s.stage == "models" && len(s.models) > 0 && len(s.models[s.modelCursor].ReasoningEfforts) > 0 {
			s.effortCursor = (s.effortCursor + 1) % len(s.models[s.modelCursor].ReasoningEfforts)
		}
	case keyNameEnter:
		if s.stage == "models" && len(s.models) > 0 {
			model := s.models[s.modelCursor]
			if len(model.ReasoningEfforts) == 0 {
				s.message = "Selected model has no supported reasoning effort."
				return m, nil
			}
			request := s.policy
			request.Model = model.ID
			request.ReasoningEffort = model.ReasoningEfforts[s.effortCursor]
			s.stage, s.message = "saving", ""
			return m, func() tea.Msg {
				profile, err := backend.SaveCodexProfile(ctx, s.login.DraftID, request)
				return codexProfileSavedMsg{profile: profile, err: err, requestID: id}
			}
		}
	case "c":
		if s.stage == "profile" && s.profile != "" {
			return m, func() tea.Msg {
				disclosure, err := backend.CheckCodexProfile(ctx, s.profile)
				return codexCheckedMsg{disclosure: disclosure, err: err, requestID: id}
			}
		}
	case "a":
		if s.stage == "checked" && s.disclosure.Fingerprint != "" {
			profile, fingerprint := s.disclosure.Profile, s.disclosure.Fingerprint
			return m, func() tea.Msg {
				err := backend.ConsentCodexProfile(ctx, profile, fingerprint)
				return codexConsentedMsg{err: err, requestID: id}
			}
		}
	case "s":
		if !s.consented {
			if s.disclosure.Fingerprint == "" {
				s.message = "Run a synthetic check before selecting."
			} else {
				s.message = "Grant consent before selecting."
			}
			return m, nil
		}
		profile := s.disclosure.Profile
		return m, func() tea.Msg {
			if err := backend.SelectCodexProfile(ctx, profile); err != nil {
				return codexSelectedMsg{err: err, requestID: id}
			}
			status, err := backend.LoadPeopleInferenceStatus(ctx)
			return codexSelectedMsg{status: status, err: err, requestID: id}
		}
	}
	return m, nil
}

func (s *codexSettingsState) selectDefaultEffort() {
	s.effortCursor = 0
	if s.modelCursor >= len(s.models) {
		return
	}
	model := s.models[s.modelCursor]
	for i, effort := range model.ReasoningEfforts {
		if effort == model.DefaultReasoningEffort {
			s.effortCursor = i
			return
		}
	}
}

func (m Model) beginCodexPolicyEdit(field string) (tea.Model, tea.Cmd) {
	s := &m.settings.codex
	input := textinput.New()
	input.CharLimit = 512
	input.SetWidth(max(min(m.width-12, 64), 12))
	switch field {
	case "1":
		input.SetValue(s.policy.Name)
	case "2":
		input.SetValue(s.policy.SourceSince)
	case "3":
		input.SetValue(s.policy.SourceUntil)
	case "4":
		input.SetValue(s.policy.RetentionPosture)
	case "5":
		input.SetValue(s.policy.TrainingPosture)
	}
	s.editing, s.editor, s.message = field, input, ""
	return m, s.editor.Focus()
}

func (m Model) handleCodexPolicyEdit(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	s := &m.settings.codex
	switch msg.String() {
	case keyNameCtrlC:
		updated, cmd := m.closeCodexSettings()
		m = updated
		m.quitting = true
		if cmd != nil {
			return m, func() tea.Msg { _ = cmd(); return tea.Quit() }
		}
		return m, tea.Quit
	case keyNameEsc:
		s.editor.Blur()
		s.editing = ""
		return m, nil
	case keyNameEnter:
		value := strings.TrimSpace(s.editor.Value())
		switch s.editing {
		case "1":
			s.policy.Name = value
		case "2":
			s.policy.SourceSince = value
		case "3":
			s.policy.SourceUntil = value
		case "4":
			s.policy.RetentionPosture = value
		case "5":
			s.policy.TrainingPosture = value
		}
		s.editor.Blur()
		s.editing = ""
		return m, nil
	}
	var cmd tea.Cmd
	s.editor, cmd = s.editor.Update(msg)
	return m, cmd
}

func (s *codexSettingsState) validatePolicy() error {
	if s.policy.Name == "" || len(s.policy.AllowedSources) == 0 || !s.sensitiveSet ||
		s.policy.RetentionPosture == "" || s.policy.TrainingPosture == "" {
		return errors.New("complete the profile policy before starting Codex sign-in")
	}
	since, err := time.Parse("2006-01-02", s.policy.SourceSince)
	if err != nil {
		return errors.New("enter a valid source since date (YYYY-MM-DD)")
	}
	if s.policy.SourceUntil != "" {
		until, err := time.Parse("2006-01-02", s.policy.SourceUntil)
		if err != nil || until.Before(since) {
			return errors.New("enter a valid source until date on or after source since")
		}
	}
	return nil
}

func (m Model) pollCodexLogin(id uint64, session string) tea.Cmd {
	backend := m.peopleInferenceBackend()
	ctx := m.settings.codex.ctx
	return func() tea.Msg {
		poll, err := backend.PollCodexLogin(ctx, session)
		return codexLoginPolledMsg{poll: poll, err: err, requestID: id}
	}
}

func (m Model) handleCodexSettingsMsg(msg tea.Msg) (tea.Model, tea.Cmd) {
	s := &m.settings.codex
	requestID := uint64(0)
	switch v := msg.(type) {
	case codexLoginStartedMsg:
		requestID = v.requestID
		if !s.active || requestID != s.requestID {
			if v.login.SessionID != "" {
				backend := m.peopleInferenceBackend()
				return m, func() tea.Msg { _ = backend.CancelCodexLogin(context.Background(), v.login.SessionID); return nil }
			}
			return m, nil
		}
		if v.err != nil {
			s.stage, s.message = "setup", v.err.Error()
			return m, nil
		}
		s.login, s.stage = v.login, "waiting"
		return m, codexPollTick(s.requestID)
	case codexPollTickMsg:
		if s.active && s.stage == "waiting" && v.requestID == s.requestID {
			return m, m.pollCodexLogin(s.requestID, s.login.SessionID)
		}
		return m, nil
	case codexLoginPolledMsg:
		requestID = v.requestID
		if !s.active || requestID != s.requestID {
			return m, nil
		}
		if v.err != nil {
			s.message = v.err.Error()
			return m, nil
		}
		s.message = ""
		if v.poll.Complete {
			s.stage = "authenticated"
			backend := m.peopleInferenceBackend()
			draft := s.login.DraftID
			ctx := s.ctx
			return m, func() tea.Msg {
				models, err := backend.ListCodexModels(ctx, draft)
				return codexModelsLoadedMsg{models: models, err: err, requestID: requestID}
			}
		}
		return m, codexPollTick(s.requestID)
	case codexModelsLoadedMsg:
		requestID = v.requestID
		if !s.active || requestID != s.requestID {
			return m, nil
		}
		if v.err != nil {
			s.message = v.err.Error()
			return m, nil
		}
		s.models, s.stage = v.models, "models"
		s.modelCursor = 0
		s.selectDefaultEffort()
		s.message = ""
	case codexProfileSavedMsg:
		requestID = v.requestID
		if !s.active || requestID != s.requestID {
			return m, nil
		}
		if v.err != nil {
			s.stage = "models"
			s.message = v.err.Error()
			return m, nil
		}
		s.profile = v.profile
		s.stage = "profile"
		s.login.SessionID = ""
		s.message = ""
	case codexCheckedMsg:
		requestID = v.requestID
		if !s.active || requestID != s.requestID {
			return m, nil
		}
		if v.err != nil {
			s.recordMutationError(v.err)
			return m, nil
		}
		s.disclosure, s.consented, s.stage = v.disclosure, false, "checked"
		s.message = ""
	case codexConsentedMsg:
		requestID = v.requestID
		if !s.active || requestID != s.requestID {
			return m, nil
		}
		if v.err != nil {
			s.recordMutationError(v.err)
			return m, nil
		}
		s.consented, s.stage = true, "consented"
		s.message = ""
	case codexSelectedMsg:
		requestID = v.requestID
		if !s.active || requestID != s.requestID {
			return m, nil
		}
		if v.err != nil {
			s.recordMutationError(v.err)
			return m, nil
		}
		s.status, s.stage = v.status, "selected"
		s.message = ""
	case codexStatusLoadedMsg:
		requestID = v.requestID
		if !s.active || requestID != s.requestID {
			return m, nil
		}
		if v.err != nil {
			s.message = v.err.Error()
			return m, nil
		}
		s.status = v.status
	}
	return m, nil
}

func (s *codexSettingsState) recordMutationError(err error) {
	var conflict *SettingsConflictError
	if errors.As(err, &conflict) && conflict.Scope == SettingsConflictConfig {
		s.disclosure = PeopleInferenceDisclosure{}
		s.consented = false
		s.stage = "profile"
		s.message = "Configuration changed; reload settings and run synthetic check."
		return
	}
	s.message = err.Error()
}

func codexPollTick(id uint64) tea.Cmd {
	return tea.Tick(time.Second, func(time.Time) tea.Msg { return codexPollTickMsg{requestID: id} })
}

func (m Model) renderCodexSettings() string {
	s := m.settings.codex
	lines := []string{m.styles.titleBar.Render("People inference · Codex"), ""}
	switch s.stage {
	case "setup":
		lines = append(lines,
			"Codex profile setup",
			"[1] Profile name: "+textutil.SanitizeTerminal(s.policy.Name),
			"Sources: [c] conversation  [m] meetings  [d] documents",
			"Selected: "+textutil.SanitizeTerminal(strings.Join(s.policy.AllowedSources, ", ")),
			"[2] Since: "+textutil.SanitizeTerminal(s.policy.SourceSince),
			"[3] Until (optional): "+textutil.SanitizeTerminal(s.policy.SourceUntil),
			"Sensitive content: [y] allow  [n] exclude",
			"[4] Retention statement: "+textutil.SanitizeTerminal(s.policy.RetentionPosture),
			"[5] Training statement: "+textutil.SanitizeTerminal(s.policy.TrainingPosture),
		)
		if s.editing != "" {
			lines = append(lines, "Edit field "+s.editing+": "+s.editor.View())
		}
		lines = append(lines, "[Enter] Sign in with Codex")
	case "starting":
		lines = append(lines, "Starting Codex sign-in…")
	case "waiting":
		lines = append(lines, "Open: "+textutil.SanitizeTerminal(s.login.URL), "Code: "+textutil.SanitizeTerminal(s.login.Code))
		if !s.login.Deadline.IsZero() {
			lines = append(lines, "Expires: "+s.login.Deadline.UTC().Format("15:04 UTC"))
		}
		lines = append(lines, "Waiting for sign-in… [r] Check now")
	case "authenticated":
		lines = append(lines, "Signed in. [m] List available models")
	case "models":
		if len(s.models) == 0 {
			lines = append(lines, "No Codex models available. [m] Retry")
			break
		}
		lines = append(lines, "Choose a model:")
		for i, model := range s.models {
			prefix := "  "
			if i == s.modelCursor {
				prefix = "▶ "
			}
			lines = append(lines, prefix+textutil.SanitizeTerminal(model.ID))
			if i == s.modelCursor && len(model.ReasoningEfforts) > 0 {
				lines = append(lines, "Reasoning: "+textutil.SanitizeTerminal(model.ReasoningEfforts[s.effortCursor])+"  [e] Change")
			}
		}
		lines = append(lines, "[j/k] Move  [Enter] Use model and reasoning")
	case "saving":
		lines = append(lines, "Saving Codex profile…")
	case "profile":
		lines = append(lines, "Profile saved for "+textutil.SanitizeTerminal(s.profile), "[c] Run synthetic check")
	case "checked", "consented":
		lines = append(lines, "Synthetic check passed.", textutil.SanitizeTerminalMultiline(s.disclosure.Text))
		if s.consented {
			lines = append(lines, "Consent granted. [s] Select profile")
		} else {
			lines = append(lines, "[a] Grant consent")
		}
	case "selected":
		lines = append(lines, "Selected profile: "+textutil.SanitizeTerminal(s.status.Configured))
	case "error":
		lines = append(lines, "Sign-in failed")
	}
	if s.status.Configured != "" || s.status.Running != "" {
		lines = append(lines, fmt.Sprintf("Configured: %s  Running: %s", textutil.SanitizeTerminal(s.status.Configured), textutil.SanitizeTerminal(s.status.Running)))
		if s.status.PendingRestart {
			lines = append(lines, "Restart the daemon to use the selected profile.")
		}
	}
	if s.message != "" {
		lines = append(lines, textutil.SanitizeTerminalMultiline(s.message))
	}
	lines = append(lines, "[r] Refresh  [Esc] Cancel and return")
	width := max(m.width, 20)
	wrapped := []string{lines[0]}
	for _, line := range lines[1:] {
		wrapped = append(wrapped, wrapText(line, width)...)
	}
	return strings.Join(wrapped, "\n")
}
