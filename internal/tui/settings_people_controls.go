package tui

import (
	"context"
	"errors"
	"strings"

	tea "charm.land/bubbletea/v2"
	"go.kenn.io/msgvault/internal/textutil"
)

// PeopleInferenceControlBackend provides status and reversible safety controls
// without exposing the device-login onboarding journey.
type PeopleInferenceControlBackend interface {
	LoadPeopleInferenceStatus(ctx context.Context) (PeopleInferenceStatus, error)
	RevokePeopleInferenceConsent(ctx context.Context, profile, fingerprint string) (PeopleInferenceStatus, error)
	DisablePeopleInference(ctx context.Context, fingerprint string) (PeopleInferenceStatus, error)
	RemovePeopleInferenceProfile(ctx context.Context, profile, fingerprint string) (PeopleInferenceStatus, error)
}

type peopleInferenceControlState struct {
	active             bool
	loading            bool
	pending            bool
	resolvingMutation  bool
	exitBlocked        bool
	requestID          uint64
	status             *PeopleInferenceStatus
	confirm            string
	confirmProfile     string
	confirmFingerprint string
	message            string
	ctx                context.Context
	cancel             context.CancelFunc
}

type peopleControlsLoadedMsg struct {
	status    PeopleInferenceStatus
	err       error
	requestID uint64
}

type peopleControlsActionMsg struct {
	status    PeopleInferenceStatus
	operation string
	err       error
	requestID uint64
}

func (m Model) peopleInferenceControlBackend() PeopleInferenceControlBackend {
	backend, _ := m.settingsBackend.(PeopleInferenceControlBackend)
	return backend
}

func (m Model) openPeopleInferenceControls() (tea.Model, tea.Cmd) {
	if m.settings.dirty() {
		m.settings.status = "Save or discard settings drafts before changing people inference."
		return m, nil
	}
	backend := m.peopleInferenceControlBackend()
	if backend == nil {
		return m, nil
	}
	m.settingsRequestID++
	ctx, cancel := context.WithCancel(context.Background())
	m.settings.peopleControls = peopleInferenceControlState{
		active: true, loading: true, requestID: m.settingsRequestID, ctx: ctx, cancel: cancel,
	}
	return m, m.loadPeopleInferenceControls(backend)
}

func (m Model) loadPeopleInferenceControls(backend PeopleInferenceControlBackend) tea.Cmd {
	ctx, id := m.settings.peopleControls.ctx, m.settings.peopleControls.requestID
	return func() tea.Msg {
		status, err := backend.LoadPeopleInferenceStatus(ctx)
		return peopleControlsLoadedMsg{status: status, err: err, requestID: id}
	}
}

func (m Model) closePeopleInferenceControls() Model {
	if m.settings.peopleControls.cancel != nil {
		m.settings.peopleControls.cancel()
	}
	m.settings.peopleControls = peopleInferenceControlState{}
	return m
}

func (m Model) handlePeopleInferenceControlKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	s := &m.settings.peopleControls
	if s.pending || (s.resolvingMutation && s.loading) {
		if msg.String() == keyNameEsc || msg.String() == keyNameCtrlC {
			s.exitBlocked = true
		}
		return m, nil
	}
	switch msg.String() {
	case keyNameCtrlC:
		m = m.closePeopleInferenceControls()
		m.quitting = true
		return m, tea.Quit
	case keyNameEsc:
		if s.confirm != "" {
			s.confirm = ""
			return m, nil
		}
		return m.closePeopleInferenceControls(), nil
	}
	backend := m.peopleInferenceControlBackend()
	if s.confirm != "" {
		switch msg.String() {
		case "n", "N":
			s.confirm = ""
			return m, nil
		case "y", "Y":
			operation, profile, fingerprint := s.confirm, s.confirmProfile, s.confirmFingerprint
			s.confirm = ""
			s.pending = true
			ctx, id := s.ctx, s.requestID
			return m, func() tea.Msg {
				var status PeopleInferenceStatus
				var err error
				switch operation {
				case "disable":
					status, err = backend.DisablePeopleInference(ctx, fingerprint)
				case "revoke":
					status, err = backend.RevokePeopleInferenceConsent(ctx, profile, fingerprint)
				case "remove":
					status, err = backend.RemovePeopleInferenceProfile(ctx, profile, fingerprint)
				}
				return peopleControlsActionMsg{status: status, operation: operation, err: err, requestID: id}
			}
		}
		return m, nil
	}
	if msg.String() == "r" {
		s.loading = true
		return m, m.loadPeopleInferenceControls(backend)
	}
	if s.loading || s.status == nil {
		return m, nil
	}
	switch msg.String() {
	case "d":
		if s.status.ConfiguredEnabled && s.status.ConfiguredFingerprint != "" {
			s.confirm = "disable"
			s.confirmFingerprint = s.status.ConfiguredFingerprint
			s.confirmProfile = s.status.Configured
		}
	case "v":
		if s.status.Configured != "" && s.status.ConfiguredFingerprint != "" {
			s.confirm = "revoke"
			s.confirmFingerprint = s.status.ConfiguredFingerprint
			s.confirmProfile = s.status.Configured
		}
	case "x":
		if !s.status.ConfiguredEnabled && s.status.Configured != "" && s.status.ConfiguredFingerprint != "" {
			s.confirm = "remove"
			s.confirmFingerprint = s.status.ConfiguredFingerprint
			s.confirmProfile = s.status.Configured
		}
	}
	return m, nil
}

func (m Model) handlePeopleInferenceControlMsg(msg tea.Msg) (tea.Model, tea.Cmd) {
	s := &m.settings.peopleControls
	switch v := msg.(type) {
	case peopleControlsLoadedMsg:
		if !s.active || v.requestID != s.requestID {
			return m, nil
		}
		s.loading = false
		s.resolvingMutation = false
		s.exitBlocked = false
		if v.err != nil {
			s.message = "Could not load people inference status: " + v.err.Error()
			s.status = nil
			m.settings.peopleInferenceStatus = nil
			m.settings.status = "People inference status could not be refreshed; reopen controls to refresh."
			m.settings.statusIsError = true
			return m, nil
		}
		s.status = &v.status
		m.settings.peopleInferenceStatus = &v.status
		m.settings.status = ""
		m.settings.statusIsError = false
	case peopleControlsActionMsg:
		if !s.active || v.requestID != s.requestID {
			return m, nil
		}
		s.pending = false
		s.exitBlocked = false
		if v.err != nil {
			var conflict *SettingsConflictError
			if errors.As(v.err, &conflict) && conflict.Scope == SettingsConflictConfig {
				s.message = "People inference settings changed; review the refreshed status before retrying."
			} else {
				s.message = "Could not confirm people inference change: " + v.err.Error()
			}
			s.status = nil
			m.settings.peopleInferenceStatus = nil
			s.loading = true
			s.resolvingMutation = true
			return m, m.loadPeopleInferenceControls(m.peopleInferenceControlBackend())
		}
		s.status = &v.status
		m.settings.peopleInferenceStatus = &v.status
		switch v.operation {
		case "disable":
			s.message = "People inference disabled."
		case "remove":
			s.message = "Profile removed."
		default:
			s.message = "Consent revoked."
		}
	}
	return m, nil
}

func (m Model) renderPeopleInferenceControls() string {
	s := m.settings.peopleControls
	lines := []string{m.styles.titleBar.Render("People inference controls"), ""}
	if s.loading {
		lines = append(lines, "Loading people inference status…")
	}
	if status := s.status; status != nil {
		configured := textutil.SanitizeTerminal(status.Configured)
		if configured == "" {
			configured = "none"
		}
		lines = append(lines, "Configured: "+configured, "Fingerprint: "+textutil.SanitizeTerminal(status.ConfiguredFingerprint))
		if status.ConfiguredEnabled {
			lines = append(lines, "People inference: enabled")
		} else {
			lines = append(lines, "People inference: disabled")
		}
		lines = append(lines, "Running: "+textutil.SanitizeTerminal(status.Running))
		if status.PendingRestart {
			lines = append(lines, "Restart the daemon to apply the change.")
		}
	}
	if s.confirm != "" {
		switch s.confirm {
		case "disable":
			lines = append(lines, "Disable people inference and revoke active consent?")
		case "remove":
			lines = append(lines, "Remove profile "+textutil.SanitizeTerminal(s.confirmProfile)+" and its stored credential if any?")
		default:
			lines = append(lines, "Revoke consent for "+textutil.SanitizeTerminal(s.confirmProfile)+"?")
		}
		lines = append(lines, "Fingerprint: "+textutil.SanitizeTerminal(s.confirmFingerprint), "[y] Confirm  [n/Esc] Cancel")
	} else if s.pending {
		lines = append(lines, "Applying people inference change…")
	} else {
		actions := "[d] Disable  [v] Revoke consent"
		if s.status != nil && !s.status.ConfiguredEnabled && s.status.ConfiguredFingerprint != "" {
			actions += "  [x] Remove profile"
		}
		lines = append(lines, actions+"  [r] Refresh  [Esc] Back")
	}
	if s.message != "" {
		lines = append(lines, textutil.SanitizeTerminalMultiline(s.message))
	}
	if s.exitBlocked {
		lines = append(lines, "Change in progress; wait for the result before leaving.")
	}
	width := max(m.width, 20)
	wrapped := []string{lines[0]}
	for _, line := range lines[1:] {
		wrapped = append(wrapped, wrapText(line, width)...)
	}
	return strings.Join(wrapped, "\n")
}
