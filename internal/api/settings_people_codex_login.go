package api

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"time"

	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/peoplesweep"
	"go.kenn.io/msgvault/internal/personenrollment"
)

func (s *Server) codexLoginManager() (*peopleCodexLogins, error) {
	s.peopleCodexLoginOnce.Do(func() {
		if s.peopleCodexLogins != nil {
			return
		}
		if s.cfg == nil {
			s.peopleCodexLoginInitErr = errors.New("people provider configuration is unavailable")
			return
		}
		dataDir := s.cfg.Data.DataDir
		tokensDir := s.cfg.TokensDir()
		for index, directory := range []string{dataDir, tokensDir, filepath.Join(tokensDir, "people-codex")} {
			if err := os.Mkdir(directory, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
				s.peopleCodexLoginInitErr = err
				return
			}
			info, err := os.Lstat(directory)
			if err != nil || !info.IsDir() || (index > 0 && info.Mode().Perm()&0o077 != 0) {
				s.peopleCodexLoginInitErr = errors.New("codex auth directory must be private")
				return
			}
		}
		client, err := peoplesweep.NewCodexEnrollmentClient("codex", filepath.Join(tokensDir, "people-codex"),
			peoplesweep.EnrollmentDraftLifetime)
		if err != nil {
			s.peopleCodexLoginInitErr = err
			return
		}
		s.peopleCodexLogins = newPeopleCodexLogins(client, time.Now)
	})
	if s.peopleCodexLoginInitErr != nil {
		return nil, s.peopleCodexLoginInitErr
	}
	return s.peopleCodexLogins, nil
}

// A new device login can replace the account behind an unchanged profile
// fingerprint. Remove prior authority before the auth file can change.
func (s *Server) revokePriorCodexEnrollmentAuthority(ctx context.Context) error {
	_, configured, err := s.readPersistedSettings()
	if err != nil {
		return err
	}
	fingerprints := make(map[string]struct{})
	for name, provider := range configured.People.Sweep.Providers {
		if provider.Protocol != peoplesweep.ProtocolCodexAppServer {
			continue
		}
		candidate := configured.People.Sweep
		candidate.Enabled = true
		candidate.Provider = peoplesweep.ProviderSelection{Name: name}
		profile, err := candidate.Profile()
		if err != nil {
			return err
		}
		fingerprints[profile.Fingerprint] = struct{}{}
	}
	if s.cfg.People.Sweep.Enabled {
		name := s.cfg.People.Sweep.Provider.Name
		if s.cfg.People.Sweep.Providers[name].Protocol == peoplesweep.ProtocolCodexAppServer {
			profile, err := s.cfg.People.Sweep.Profile()
			if err != nil {
				return err
			}
			fingerprints[profile.Fingerprint] = struct{}{}
		}
	}
	if len(fingerprints) == 0 {
		return nil
	}
	st, ok := s.store.(peopleInferenceCredentialAuthorityStore)
	if !ok {
		return errors.New("people inference authority store is unavailable")
	}
	for fingerprint := range fingerprints {
		if _, err := st.RevokePersonInferenceConsent(ctx, fingerprint, "web"); err != nil {
			return err
		}
		if _, err := st.InvalidatePersonInferenceCheck(ctx, fingerprint); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) peopleCodexLoginOwner(w http.ResponseWriter, r *http.Request) (string, bool) {
	auth := s.requestAuthentication(r)
	switch auth.Mode {
	case AuthModeSession:
		if auth.SessionID != "" {
			return "session:" + auth.SessionID, true
		}
	case AuthModeAPIKey:
		return "owner-api-key", true
	case AuthModeLoopback:
		return "loopback", true
	case AuthModeRequired, AuthModeDelegated:
	}
	writeError(w, http.StatusForbidden, "codex_login_forbidden", "Codex sign-in requires an owner session")
	return "", false
}

func (s *Server) handleStartPeopleCodexLogin(w http.ResponseWriter, r *http.Request) {
	owner, ok := s.peopleCodexLoginOwner(w, r)
	if !ok {
		return
	}
	logins, err := s.codexLoginManager()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "codex_unavailable", "Codex device login is unavailable on this daemon")
		return
	}
	var request PeopleCodexLoginRequest
	if !decodeStrictSettingsJSON(w, r, &request) {
		return
	}
	draft, err := logins.Start(owner, request.Name, func() error {
		return s.revokePriorCodexEnrollmentAuthority(r.Context())
	})
	if err != nil {
		switch {
		case errors.Is(err, errPeopleCodexLoginBusy), errors.Is(err, peoplesweep.ErrEnrollmentDraftActive):
			writeError(w, http.StatusConflict, "codex_login_active", "A Codex device login is already active")
		case errors.Is(err, peoplesweep.ErrEnrollmentDraftInvalid):
			writeError(w, http.StatusBadRequest, "invalid_codex_login", "Codex login request is invalid")
		case errors.Is(err, errPeopleCodexLoginPreparation):
			writeError(w, http.StatusServiceUnavailable, "codex_authority_unavailable", "Could not revoke prior Codex authority before sign-in")
		default:
			writeError(w, http.StatusBadRequest, "invalid_provider_name", "Codex profile name is invalid")
		}
		return
	}
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	wait := time.NewTimer(15 * time.Second)
	defer wait.Stop()
	for {
		session, err := logins.Get(owner, draft.ID)
		if err != nil {
			writeError(w, http.StatusBadGateway, "codex_login_failed", "Codex device login could not start")
			return
		}
		if session.login.UserCode != "" {
			w.Header().Set("Cache-Control", "no-store")
			writeJSON(w, http.StatusOK, PeopleCodexLoginResponse{
				SessionID: draft.ID, VerificationURL: session.login.VerificationURL,
				UserCode: session.login.UserCode, LocalDeadline: draft.ExpiresAt,
			})
			return
		}
		if session.state == "failed" {
			_ = logins.Cancel(owner, draft.ID)
			writeError(w, http.StatusBadGateway, "codex_login_failed", "Codex device login could not start")
			return
		}
		select {
		case <-ticker.C:
		case <-wait.C:
			_ = logins.Cancel(owner, draft.ID)
			writeError(w, http.StatusGatewayTimeout, "codex_login_timeout", "Codex device code was not available in time")
			return
		case <-r.Context().Done():
			_ = logins.Cancel(owner, draft.ID)
			return
		}
	}
}

func (s *Server) peopleCodexLoginForRequest(w http.ResponseWriter, r *http.Request) (string, peopleCodexLoginSession, bool) {
	owner, ok := s.peopleCodexLoginOwner(w, r)
	if !ok {
		return "", peopleCodexLoginSession{}, false
	}
	logins, err := s.codexLoginManager()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "codex_unavailable", "Codex device login is unavailable on this daemon")
		return "", peopleCodexLoginSession{}, false
	}
	session, err := logins.Get(owner, r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusNotFound, "codex_login_not_found", "Codex device login was not found")
		return "", peopleCodexLoginSession{}, false
	}
	return owner, session, true
}

func (s *Server) handleGetPeopleCodexLogin(w http.ResponseWriter, r *http.Request) {
	_, session, ok := s.peopleCodexLoginForRequest(w, r)
	if !ok {
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, PeopleCodexLoginStatusResponse{State: session.state})
}

func (s *Server) handleCancelPeopleCodexLogin(w http.ResponseWriter, r *http.Request) {
	owner, _, ok := s.peopleCodexLoginForRequest(w, r)
	if !ok {
		return
	}
	if err := s.peopleCodexLogins.Cancel(owner, r.PathValue("id")); err != nil {
		if errors.Is(err, errPeopleCodexLoginCompleted) {
			writeError(w, http.StatusConflict, "codex_login_completed", "Codex sign-in completed before cancellation")
		} else {
			writeError(w, http.StatusNotFound, "codex_login_not_found", "Codex device login was not found")
		}
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, PeopleCodexLoginStatusResponse{State: "cancelled"})
}

func (s *Server) handleGetPeopleCodexModels(w http.ResponseWriter, r *http.Request) {
	_, session, ok := s.peopleCodexLoginForRequest(w, r)
	if !ok {
		return
	}
	if session.state != "complete" {
		writeError(w, http.StatusConflict, "codex_login_incomplete", "Complete Codex sign-in before listing models")
		return
	}
	models, err := s.peopleCodexLogins.client.ListModels(r.Context())
	if err != nil {
		writeError(w, http.StatusBadGateway, "codex_models_failed", "Could not list models for the signed-in Codex account")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, PeopleCodexModelsResponse{Models: models})
}

func (s *Server) handlePutPeopleCodexProfile(w http.ResponseWriter, r *http.Request) {
	ifMatch, ok := requiredSingleIfMatch(w, r)
	if !ok {
		return
	}
	owner, session, ok := s.peopleCodexLoginForRequest(w, r)
	if !ok {
		return
	}
	if session.state != "complete" {
		writeError(w, http.StatusConflict, "codex_login_incomplete", "Complete Codex sign-in before creating a profile")
		return
	}
	var request PeopleCodexProfileRequest
	if !decodeStrictSettingsJSON(w, r, &request) {
		return
	}
	models, err := s.peopleCodexLogins.client.ListModels(r.Context())
	if err != nil {
		writeError(w, http.StatusBadGateway, "codex_models_failed", "Could not verify models for the signed-in Codex account")
		return
	}
	validModel := false
	for _, model := range models {
		if model.ID == request.Model && slices.Contains(model.SupportedEfforts, request.ReasoningEffort) {
			validModel = true
			break
		}
	}
	if !validModel {
		writeError(w, http.StatusUnprocessableEntity, "codex_model_unavailable", "Model or reasoning effort is unavailable for the signed-in Codex account")
		return
	}
	provider := peoplesweep.ProviderConfig{
		Protocol: peoplesweep.ProtocolCodexAppServer, Model: request.Model,
		ReasoningEffort: request.ReasoningEffort, Executable: "codex",
		ExecutionBoundary: peoplesweep.CodexExecutionBoundaryV1,
		Auth:              peoplesweep.AuthNone, Credential: peoplesweep.CredentialNone,
		OutputMode:       peoplesweep.OutputModeNativeJSONSchema,
		RetentionPosture: request.RetentionPosture, TrainingPosture: request.TrainingPosture,
		SourceSince: request.SourceSince, SourceUntil: request.SourceUntil,
		AllowSensitive: request.AllowSensitive,
	}
	for _, source := range request.AllowedSources {
		provider.AllowedSources = append(provider.AllowedSources, peoplesweep.SourceClass(source))
	}
	service := personenrollment.NewService(s.cfg.ConfigFilePath(), nil)
	if _, err := service.CreateProfile(ifMatch, session.name, provider); err != nil {
		switch {
		case errors.Is(err, config.ErrConfigConflict):
			writeError(w, http.StatusPreconditionFailed, "settings_conflict", "The config file changed; reload settings and retry")
		case errors.Is(err, personenrollment.ErrProfileExists):
			writeError(w, http.StatusConflict, "provider_exists", "People inference provider already exists")
		case errors.Is(err, personenrollment.ErrInvalidProfile):
			writeError(w, http.StatusUnprocessableEntity, "invalid_provider", "Codex provider settings are invalid")
		default:
			s.writeSettingsConfigError(w, err)
		}
		return
	}
	_ = s.peopleCodexLogins.Cancel(owner, session.draft.ID)
	s.settingsPendingRestart.Store(true)
	snapshot, configured, err := s.readPersistedSettings()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "settings_read_failed", "Could not read settings")
		return
	}
	response, err := s.buildPeopleInferenceSettingsResponse(r.Context(), configured)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "settings_read_failed", "Could not read people provider status")
		return
	}
	w.Header().Set(etagHeaderName, snapshot.ETag)
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, response)
}
