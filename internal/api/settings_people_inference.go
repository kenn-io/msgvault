package api

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/peoplesweep"
	"go.kenn.io/msgvault/internal/personenrollment"
	"go.kenn.io/msgvault/internal/store"
)

// PeopleInferenceProfileSetting exposes policy fields used during enrollment.
// Credential values and Codex authentication state are intentionally absent.
type PeopleInferenceProfileSetting struct {
	Name                 string   `json:"name"`
	Selected             bool     `json:"selected"`
	PresetID             string   `json:"preset_id,omitempty"`
	Protocol             string   `json:"protocol"`
	Endpoint             string   `json:"endpoint,omitempty"`
	Model                string   `json:"model"`
	CredentialSource     string   `json:"credential_source"`
	CredentialEnv        string   `json:"credential_env,omitempty"`
	CredentialConfigured bool     `json:"credential_configured"`
	CredentialRevision   string   `json:"credential_revision,omitempty"`
	Checked              bool     `json:"checked"`
	ConsentActive        bool     `json:"consent_active"`
	OutputMode           string   `json:"output_mode"`
	RetentionPosture     string   `json:"retention_posture"`
	TrainingPosture      string   `json:"training_posture"`
	AllowedSources       []string `json:"allowed_sources"`
	SourceSince          string   `json:"source_since"`
	SourceUntil          string   `json:"source_until,omitempty"`
	AllowSensitive       bool     `json:"allow_sensitive"`
	Fingerprint          string   `json:"fingerprint,omitempty"`
}

// PeopleInferenceSettingsResponse distinguishes disk configuration from the
// policy the daemon loaded at startup. A saved change takes effect on restart.
type PeopleInferenceSettingsResponse struct {
	Profiles              []PeopleInferenceProfileSetting `json:"profiles"`
	ConfiguredName        string                          `json:"configured_name,omitempty"`
	ConfiguredEnabled     bool                            `json:"configured_enabled"`
	ConfiguredFingerprint string                          `json:"configured_fingerprint,omitempty"`
	RunningName           string                          `json:"running_name,omitempty"`
	RunningEnabled        bool                            `json:"running_enabled"`
	RunningFingerprint    string                          `json:"running_fingerprint,omitempty"`
	PendingRestart        bool                            `json:"pending_restart"`
}

type PeopleInferenceSelectionRequest struct {
	Name string `json:"name" minLength:"1"`
}

// PeopleInferencePresetCreateRequest intentionally has no endpoint or key
// field. The daemon binds the selected vendor destination before a separate
// credential write can occur.
type PeopleInferencePresetCreateRequest struct {
	PresetID         string   `json:"preset_id" enum:"openai,openrouter,venice"`
	Model            string   `json:"model" minLength:"1"`
	CredentialEnv    string   `json:"credential_env,omitempty"`
	RetentionPosture string   `json:"retention_posture" minLength:"1"`
	TrainingPosture  string   `json:"training_posture" minLength:"1"`
	AllowedSources   []string `json:"allowed_sources" minItems:"1"`
	SourceSince      string   `json:"source_since"`
	SourceUntil      string   `json:"source_until,omitempty"`
	AllowSensitive   bool     `json:"allow_sensitive"`
}

type PeopleInferenceKeyWriteRequest struct {
	Value string `json:"value" minLength:"1"`
}

type PeopleInferenceCheckResponse struct {
	OK          bool                   `json:"ok"`
	Fingerprint string                 `json:"fingerprint"`
	Model       string                 `json:"model"`
	Usage       peoplesweep.TokenUsage `json:"usage"`
}

type PeopleInferenceConsentRequest struct {
	Fingerprint string `json:"fingerprint"`
	Confirmed   bool   `json:"confirmed"`
}

type PeopleCodexLoginRequest struct {
	Name string `json:"name" minLength:"1"`
}

type PeopleCodexLoginResponse struct {
	SessionID       string    `json:"session_id"`
	VerificationURL string    `json:"verification_url"`
	UserCode        string    `json:"user_code"`
	LocalDeadline   time.Time `json:"local_deadline"`
}

type PeopleCodexLoginStatusResponse struct {
	State string `json:"state"`
}

type PeopleCodexModelsResponse struct {
	Models []peoplesweep.CodexModel `json:"models"`
}

type PeopleCodexProfileRequest struct {
	Model            string   `json:"model" minLength:"1"`
	ReasoningEffort  string   `json:"reasoning_effort" minLength:"1"`
	RetentionPosture string   `json:"retention_posture" minLength:"1"`
	TrainingPosture  string   `json:"training_posture" minLength:"1"`
	AllowedSources   []string `json:"allowed_sources" minItems:"1"`
	SourceSince      string   `json:"source_since"`
	SourceUntil      string   `json:"source_until,omitempty"`
	AllowSensitive   bool     `json:"allow_sensitive"`
}

type peopleInferenceCheckStore interface {
	peoplesweep.ProviderAuthority
	EnsurePersonInferenceProfile(ctx context.Context, profile peoplesweep.ProviderProfile) (bool, error)
	RecordPersonInferenceCheck(ctx context.Context, check store.PersonInferenceCheck) error
}

type peopleInferenceConsentStore interface {
	peoplesweep.ProviderAuthority
	EnsurePersonInferenceProfile(ctx context.Context, profile peoplesweep.ProviderProfile) (bool, error)
	GrantPersonInferenceConsent(ctx context.Context, fingerprint, actor string) (*store.PersonInferenceConsent, bool, error)
}

type peopleInferenceCredentialAuthorityStore interface {
	RevokePersonInferenceConsent(ctx context.Context, fingerprint, actor string) (bool, error)
	InvalidatePersonInferenceCheck(ctx context.Context, fingerprint string) (bool, error)
}

func (s *Server) registerPeopleInferenceSettingsRoute(api huma.API) {
	operation := rawAPIV1Operation("getSettingsPeopleInference", http.MethodGet,
		"/settings/people-inference", "Get people inference provider status")
	operation.Responses = jsonResponsesFor[PeopleInferenceSettingsResponse](api)
	addSettingsETagHeader(operation.Responses[httpStatusKey(http.StatusOK)])
	registerRawHumaRoute(api, operation, s.handleGetPeopleInferenceSettings)

	selectOperation := rawAPIV1Operation("selectSettingsPeopleInference", http.MethodPost,
		"/settings/people-inference/select", "Select a checked and consented people inference provider")
	selectOperation.Parameters = append(selectOperation.Parameters,
		&huma.Param{Name: ifMatchHeaderName, In: headerParamLocation, Required: true,
			Description: "Strong config ETag returned by the latest settings read", Schema: &huma.Schema{Type: huma.TypeString}},
	)
	selectOperation.RequestBody = jsonRequestBodyFor[PeopleInferenceSelectionRequest](api)
	selectOperation.Responses = jsonResponsesFor[PeopleInferenceSettingsResponse](api)
	for _, status := range []int{http.StatusBadRequest, http.StatusConflict, http.StatusPreconditionFailed,
		http.StatusPreconditionRequired, http.StatusServiceUnavailable, http.StatusUnprocessableEntity,
		http.StatusInternalServerError} {
		selectOperation.Responses[httpStatusKey(status)] = errorResponseFor(api)
	}
	addSettingsETagHeader(selectOperation.Responses[httpStatusKey(http.StatusOK)])
	registerRawHumaRoute(api, selectOperation, s.handleSelectPeopleInferenceSettings)

	createOperation := rawAPIV1Operation("putSettingsPeopleInferencePreset", http.MethodPut,
		"/settings/people-inference/providers/{name}", "Create a vendor-bound people inference provider")
	createOperation.Parameters = append(createOperation.Parameters,
		&huma.Param{Name: nameKey, In: pathKey, Required: true, Schema: &huma.Schema{Type: huma.TypeString}},
		&huma.Param{Name: ifMatchHeaderName, In: headerParamLocation, Required: true,
			Description: "Strong config ETag returned by the latest settings read", Schema: &huma.Schema{Type: huma.TypeString}},
	)
	createOperation.RequestBody = jsonRequestBodyFor[PeopleInferencePresetCreateRequest](api)
	createOperation.Responses = jsonResponsesFor[PeopleInferenceSettingsResponse](api)
	for _, status := range []int{http.StatusBadRequest, http.StatusConflict, http.StatusPreconditionFailed,
		http.StatusPreconditionRequired, http.StatusUnprocessableEntity, http.StatusInternalServerError} {
		createOperation.Responses[httpStatusKey(status)] = errorResponseFor(api)
	}
	addSettingsETagHeader(createOperation.Responses[httpStatusKey(http.StatusOK)])
	registerRawHumaRoute(api, createOperation, s.handleCreatePeopleInferencePreset)

	keyOperation := rawAPIV1Operation("putSettingsPeopleInferenceKey", http.MethodPut,
		"/settings/people-inference/providers/{name}/key", "Set a write-only people inference API key")
	keyOperation.Parameters = append(keyOperation.Parameters,
		&huma.Param{Name: nameKey, In: pathKey, Required: true, Schema: &huma.Schema{Type: huma.TypeString}},
		&huma.Param{Name: ifMatchHeaderName, In: headerParamLocation, Required: true,
			Description: "Opaque revision for this people provider credential", Schema: &huma.Schema{Type: huma.TypeString}},
	)
	keyOperation.RequestBody = jsonRequestBodyFor[PeopleInferenceKeyWriteRequest](api)
	keyOperation.Responses = jsonResponsesFor[PeopleInferenceSettingsResponse](api)
	for _, status := range []int{http.StatusBadRequest, http.StatusConflict, http.StatusNotFound,
		http.StatusPreconditionFailed, http.StatusPreconditionRequired, http.StatusServiceUnavailable,
		http.StatusInternalServerError} {
		keyOperation.Responses[httpStatusKey(status)] = errorResponseFor(api)
	}
	addSettingsETagHeader(keyOperation.Responses[httpStatusKey(http.StatusOK)])
	registerRawHumaRoute(api, keyOperation, s.handlePutPeopleInferenceKey)

	deleteKeyOperation := rawAPIV1Operation("deleteSettingsPeopleInferenceKey", http.MethodDelete,
		"/settings/people-inference/providers/{name}/key", "Clear a stored people inference API key")
	deleteKeyOperation.Parameters = append(deleteKeyOperation.Parameters,
		&huma.Param{Name: nameKey, In: pathKey, Required: true, Schema: &huma.Schema{Type: huma.TypeString}},
		&huma.Param{Name: ifMatchHeaderName, In: headerParamLocation, Required: true,
			Description: "Opaque revision for this people provider credential", Schema: &huma.Schema{Type: huma.TypeString}},
	)
	deleteKeyOperation.Responses = jsonResponsesFor[PeopleInferenceSettingsResponse](api)
	for _, status := range []int{http.StatusBadRequest, http.StatusConflict, http.StatusNotFound,
		http.StatusPreconditionFailed, http.StatusPreconditionRequired, http.StatusServiceUnavailable,
		http.StatusInternalServerError} {
		deleteKeyOperation.Responses[httpStatusKey(status)] = errorResponseFor(api)
	}
	addSettingsETagHeader(deleteKeyOperation.Responses[httpStatusKey(http.StatusOK)])
	registerRawHumaRoute(api, deleteKeyOperation, s.handleDeletePeopleInferenceKey)

	checkOperation := rawAPIV1Operation("checkSettingsPeopleInferenceProvider", http.MethodPost,
		"/settings/people-inference/providers/{name}/check", "Run a synthetic people inference provider check")
	checkOperation.Parameters = append(checkOperation.Parameters,
		&huma.Param{Name: nameKey, In: pathKey, Required: true, Schema: &huma.Schema{Type: huma.TypeString}},
		&huma.Param{Name: ifMatchHeaderName, In: headerParamLocation, Required: true,
			Description: "Strong config ETag returned by the latest settings read", Schema: &huma.Schema{Type: huma.TypeString}},
	)
	checkOperation.Responses = jsonResponsesFor[PeopleInferenceCheckResponse](api)
	for _, status := range []int{http.StatusBadRequest, http.StatusConflict, http.StatusPreconditionFailed,
		http.StatusPreconditionRequired, http.StatusBadGateway, http.StatusServiceUnavailable,
		http.StatusInternalServerError} {
		checkOperation.Responses[httpStatusKey(status)] = errorResponseFor(api)
	}
	registerRawHumaRoute(api, checkOperation, s.handleCheckPeopleInferenceProvider)

	consentOperation := rawAPIV1Operation("consentSettingsPeopleInferenceProvider", http.MethodPost,
		"/settings/people-inference/providers/{name}/consent", "Grant exact people inference consent")
	consentOperation.Parameters = append(consentOperation.Parameters,
		&huma.Param{Name: nameKey, In: pathKey, Required: true, Schema: &huma.Schema{Type: huma.TypeString}},
		&huma.Param{Name: ifMatchHeaderName, In: headerParamLocation, Required: true,
			Description: "Strong config ETag returned by the latest settings read", Schema: &huma.Schema{Type: huma.TypeString}},
	)
	consentOperation.RequestBody = jsonRequestBodyFor[PeopleInferenceConsentRequest](api)
	consentOperation.Responses = jsonResponsesFor[PeopleInferenceSettingsResponse](api)
	for _, status := range []int{http.StatusBadRequest, http.StatusConflict, http.StatusPreconditionFailed,
		http.StatusPreconditionRequired, http.StatusServiceUnavailable, http.StatusInternalServerError} {
		consentOperation.Responses[httpStatusKey(status)] = errorResponseFor(api)
	}
	addSettingsETagHeader(consentOperation.Responses[httpStatusKey(http.StatusOK)])
	registerRawHumaRoute(api, consentOperation, s.handleConsentPeopleInferenceProvider)

	revokeOperation := rawAPIV1Operation("revokeSettingsPeopleInferenceProvider", http.MethodPost,
		"/settings/people-inference/providers/{name}/revoke", "Revoke exact people inference consent")
	revokeOperation.Parameters = append(revokeOperation.Parameters,
		&huma.Param{Name: nameKey, In: pathKey, Required: true, Schema: &huma.Schema{Type: huma.TypeString}},
		&huma.Param{Name: ifMatchHeaderName, In: headerParamLocation, Required: true,
			Description: "Strong config ETag returned by the latest settings read", Schema: &huma.Schema{Type: huma.TypeString}},
	)
	revokeOperation.Responses = jsonResponsesFor[PeopleInferenceSettingsResponse](api)
	for _, status := range []int{http.StatusBadRequest, http.StatusConflict, http.StatusPreconditionFailed,
		http.StatusPreconditionRequired, http.StatusServiceUnavailable, http.StatusInternalServerError} {
		revokeOperation.Responses[httpStatusKey(status)] = errorResponseFor(api)
	}
	addSettingsETagHeader(revokeOperation.Responses[httpStatusKey(http.StatusOK)])
	registerRawHumaRoute(api, revokeOperation, s.handleRevokePeopleInferenceProvider)

	disableOperation := rawAPIV1Operation("disableSettingsPeopleInference", http.MethodPost,
		"/settings/people-inference/disable", "Disable people inference and revoke active consent")
	disableOperation.Parameters = append(disableOperation.Parameters,
		&huma.Param{Name: ifMatchHeaderName, In: headerParamLocation, Required: true,
			Description: "Strong config ETag returned by the latest settings read", Schema: &huma.Schema{Type: huma.TypeString}},
	)
	disableOperation.Responses = jsonResponsesFor[PeopleInferenceSettingsResponse](api)
	for _, status := range []int{http.StatusBadRequest, http.StatusConflict, http.StatusPreconditionFailed,
		http.StatusPreconditionRequired, http.StatusServiceUnavailable, http.StatusInternalServerError} {
		disableOperation.Responses[httpStatusKey(status)] = errorResponseFor(api)
	}
	addSettingsETagHeader(disableOperation.Responses[httpStatusKey(http.StatusOK)])
	registerRawHumaRoute(api, disableOperation, s.handleDisablePeopleInference)

	removeOperation := rawAPIV1Operation("deleteSettingsPeopleInferenceProvider", http.MethodDelete,
		"/settings/people-inference/providers/{name}", "Remove a people inference provider profile")
	removeOperation.Parameters = append(removeOperation.Parameters,
		&huma.Param{Name: nameKey, In: pathKey, Required: true, Schema: &huma.Schema{Type: huma.TypeString}},
		&huma.Param{Name: ifMatchHeaderName, In: headerParamLocation, Required: true,
			Description: "Strong config ETag returned by the latest settings read", Schema: &huma.Schema{Type: huma.TypeString}},
	)
	removeOperation.Responses = jsonResponsesFor[PeopleInferenceSettingsResponse](api)
	for _, status := range []int{http.StatusBadRequest, http.StatusConflict, http.StatusNotFound,
		http.StatusPreconditionFailed, http.StatusPreconditionRequired, http.StatusServiceUnavailable,
		http.StatusInternalServerError} {
		removeOperation.Responses[httpStatusKey(status)] = errorResponseFor(api)
	}
	addSettingsETagHeader(removeOperation.Responses[httpStatusKey(http.StatusOK)])
	registerRawHumaRoute(api, removeOperation, s.handleRemovePeopleInferenceProvider)

	loginOperation := rawAPIV1Operation("startSettingsPeopleCodexLogin", http.MethodPost,
		"/settings/people-inference/codex/login", "Start a private Codex device login")
	loginOperation.RequestBody = jsonRequestBodyFor[PeopleCodexLoginRequest](api)
	loginOperation.Responses = jsonResponsesFor[PeopleCodexLoginResponse](api)
	for _, status := range []int{http.StatusBadRequest, http.StatusConflict, http.StatusForbidden,
		http.StatusServiceUnavailable, http.StatusBadGateway, http.StatusGatewayTimeout} {
		loginOperation.Responses[httpStatusKey(status)] = errorResponseFor(api)
	}
	registerRawHumaRoute(api, loginOperation, s.handleStartPeopleCodexLogin)

	for _, route := range []struct {
		method, id, path, description string
		handler                       http.HandlerFunc
	}{
		{http.MethodGet, "getSettingsPeopleCodexLogin", "/settings/people-inference/codex/login/{id}", "Get Codex device login status", s.handleGetPeopleCodexLogin},
		{http.MethodDelete, "cancelSettingsPeopleCodexLogin", "/settings/people-inference/codex/login/{id}", "Cancel Codex device login", s.handleCancelPeopleCodexLogin},
		{http.MethodGet, "getSettingsPeopleCodexModels", "/settings/people-inference/codex/login/{id}/models", "List models for completed Codex login", s.handleGetPeopleCodexModels},
	} {
		operation := rawAPIV1Operation(route.id, route.method, route.path, route.description)
		operation.Parameters = append(operation.Parameters,
			&huma.Param{Name: "id", In: pathKey, Required: true, Schema: &huma.Schema{Type: huma.TypeString}},
		)
		if route.method == http.MethodDelete {
			operation.Responses = jsonResponsesFor[PeopleCodexLoginStatusResponse](api)
		} else if route.id == "getSettingsPeopleCodexModels" {
			operation.Responses = jsonResponsesFor[PeopleCodexModelsResponse](api)
		} else {
			operation.Responses = jsonResponsesFor[PeopleCodexLoginStatusResponse](api)
		}
		for _, status := range []int{http.StatusConflict, http.StatusForbidden, http.StatusNotFound,
			http.StatusServiceUnavailable, http.StatusBadGateway} {
			operation.Responses[httpStatusKey(status)] = errorResponseFor(api)
		}
		registerRawHumaRoute(api, operation, route.handler)
	}

	codexProfileOperation := rawAPIV1Operation("putSettingsPeopleCodexProfile", http.MethodPut,
		"/settings/people-inference/codex/login/{id}/profile", "Create a Codex profile from completed device login")
	codexProfileOperation.Parameters = append(codexProfileOperation.Parameters,
		&huma.Param{Name: "id", In: pathKey, Required: true, Schema: &huma.Schema{Type: huma.TypeString}},
		&huma.Param{Name: ifMatchHeaderName, In: headerParamLocation, Required: true,
			Description: "Strong config ETag returned by the latest settings read", Schema: &huma.Schema{Type: huma.TypeString}},
	)
	codexProfileOperation.RequestBody = jsonRequestBodyFor[PeopleCodexProfileRequest](api)
	codexProfileOperation.Responses = jsonResponsesFor[PeopleInferenceSettingsResponse](api)
	for _, status := range []int{http.StatusBadRequest, http.StatusConflict, http.StatusForbidden,
		http.StatusNotFound, http.StatusPreconditionFailed, http.StatusPreconditionRequired,
		http.StatusServiceUnavailable, http.StatusBadGateway, http.StatusUnprocessableEntity,
		http.StatusInternalServerError} {
		codexProfileOperation.Responses[httpStatusKey(status)] = errorResponseFor(api)
	}
	addSettingsETagHeader(codexProfileOperation.Responses[httpStatusKey(http.StatusOK)])
	registerRawHumaRoute(api, codexProfileOperation, s.handlePutPeopleCodexProfile)
}

func (s *Server) handleGetPeopleInferenceSettings(w http.ResponseWriter, r *http.Request) {
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

func (s *Server) handleSelectPeopleInferenceSettings(w http.ResponseWriter, r *http.Request) {
	ifMatch, ok := requiredSingleIfMatch(w, r)
	if !ok {
		return
	}
	var request PeopleInferenceSelectionRequest
	if !decodeStrictSettingsJSON(w, r, &request) {
		return
	}
	if err := peoplesweep.ValidateProviderProfileName(request.Name); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_provider", "People inference provider name is invalid")
		return
	}
	st, ok := s.store.(personenrollment.CheckConsentStore)
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "people_inference_unavailable", "People inference store is unavailable")
		return
	}
	service := personenrollment.NewService(s.cfg.ConfigFilePath(), st)
	if _, err := service.SelectProfile(r.Context(), ifMatch, request.Name); err != nil {
		switch {
		case errors.Is(err, config.ErrConfigConflict):
			writeError(w, http.StatusPreconditionFailed, "settings_conflict", "The config file changed; reload settings and retry")
		case errors.Is(err, personenrollment.ErrCheckRequired):
			writeError(w, http.StatusConflict, "check_required", "Run an exact synthetic check before selecting this provider")
		case errors.Is(err, personenrollment.ErrConsentRequired):
			writeError(w, http.StatusConflict, "consent_required", "Grant exact people inference consent before selecting this provider")
		case errors.Is(err, personenrollment.ErrProfileMissing):
			writeError(w, http.StatusBadRequest, "provider_not_found", "People inference provider was not found")
		default:
			s.writeSettingsConfigError(w, err)
		}
		return
	}
	snapshot, configured, err := s.readPersistedSettings()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "settings_read_failed", "Could not read settings")
		return
	}
	s.settingsPendingRestart.Store(true)
	w.Header().Set(etagHeaderName, snapshot.ETag)
	w.Header().Set("Cache-Control", "no-store")
	response, err := s.buildPeopleInferenceSettingsResponse(r.Context(), configured)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "settings_read_failed", "Could not read people provider status")
		return
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) handleCreatePeopleInferencePreset(w http.ResponseWriter, r *http.Request) {
	ifMatch, ok := requiredSingleIfMatch(w, r)
	if !ok {
		return
	}
	var request PeopleInferencePresetCreateRequest
	if !decodeStrictSettingsJSON(w, r, &request) {
		return
	}
	if err := peoplesweep.ValidateProviderProfileName(r.PathValue("name")); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_provider_name", "People inference provider name is invalid")
		return
	}
	provider, err := peoplesweep.PresetProviderConfig(request.PresetID, request.Model)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "invalid_provider", "People inference provider preset is invalid")
		return
	}
	provider.RetentionPosture = request.RetentionPosture
	if request.CredentialEnv != "" {
		provider.Credential = peoplesweep.CredentialEnv
		provider.CredentialEnv = request.CredentialEnv
	}
	provider.TrainingPosture = request.TrainingPosture
	provider.SourceSince = request.SourceSince
	provider.SourceUntil = request.SourceUntil
	provider.AllowSensitive = request.AllowSensitive
	for _, source := range request.AllowedSources {
		provider.AllowedSources = append(provider.AllowedSources, peoplesweep.SourceClass(source))
	}
	service := personenrollment.NewService(s.cfg.ConfigFilePath(), nil)
	if _, err := service.CreateProfile(ifMatch, r.PathValue("name"), provider); err != nil {
		switch {
		case errors.Is(err, config.ErrConfigConflict):
			writeError(w, http.StatusPreconditionFailed, "settings_conflict", "The config file changed; reload settings and retry")
		case errors.Is(err, personenrollment.ErrProfileExists):
			writeError(w, http.StatusConflict, "provider_exists", "People inference provider already exists")
		case errors.Is(err, personenrollment.ErrInvalidProfile):
			writeError(w, http.StatusUnprocessableEntity, "invalid_provider", "People inference provider settings are invalid")
		default:
			s.writeSettingsConfigError(w, err)
		}
		return
	}
	snapshot, configured, err := s.readPersistedSettings()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "settings_read_failed", "Could not read settings")
		return
	}
	s.settingsPendingRestart.Store(true)
	w.Header().Set(etagHeaderName, snapshot.ETag)
	w.Header().Set("Cache-Control", "no-store")
	response, err := s.buildPeopleInferenceSettingsResponse(r.Context(), configured)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "settings_read_failed", "Could not read people provider status")
		return
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) handlePutPeopleInferenceKey(w http.ResponseWriter, r *http.Request) {
	ifMatch, ok := requiredSingleIfMatch(w, r)
	if !ok {
		return
	}
	target, ok := s.peopleInferenceKeyTarget(w, r, ifMatch, false)
	if !ok {
		return
	}
	var request PeopleInferenceKeyWriteRequest
	if !decodeStrictSettingsJSON(w, r, &request) {
		return
	}
	if request.Value == "" {
		writeError(w, http.StatusBadRequest, "invalid_credential", "An API key is required")
		return
	}
	if _, err := target.store.RevokePersonInferenceConsent(r.Context(), target.fingerprint, "web"); err != nil {
		writeError(w, http.StatusInternalServerError, "consent_revoke_failed", "Could not revoke prior provider consent")
		return
	}
	if _, err := target.store.InvalidatePersonInferenceCheck(r.Context(), target.fingerprint); err != nil {
		writeError(w, http.StatusInternalServerError, "check_invalidation_failed", "Could not invalidate prior provider check")
		return
	}
	if _, err := target.credentials.SaveIfRevision(target.name, peoplesweep.NewCredential(peoplesweep.AuthBearer, request.Value), ifMatch); err != nil {
		if errors.Is(err, peoplesweep.ErrCredentialRevisionConflict) {
			writeError(w, http.StatusPreconditionFailed, "credential_conflict", "People provider credential changed; reload settings")
		} else {
			writeError(w, http.StatusInternalServerError, "credential_store_unavailable", "Could not save people provider credential")
		}
		return
	}
	snapshot, latest, err := s.readPersistedSettings()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "settings_read_failed", "Could not read settings")
		return
	}
	response, err := s.buildPeopleInferenceSettingsResponse(r.Context(), latest)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "settings_read_failed", "Could not read people provider status")
		return
	}
	w.Header().Set(etagHeaderName, snapshot.ETag)
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) handleDeletePeopleInferenceKey(w http.ResponseWriter, r *http.Request) {
	ifMatch, ok := requiredSingleIfMatch(w, r)
	if !ok {
		return
	}
	target, ok := s.peopleInferenceKeyTarget(w, r, ifMatch, true)
	if !ok {
		return
	}
	if _, err := target.store.RevokePersonInferenceConsent(r.Context(), target.fingerprint, "web"); err != nil {
		writeError(w, http.StatusInternalServerError, "consent_revoke_failed", "Could not revoke prior provider consent")
		return
	}
	if _, err := target.store.InvalidatePersonInferenceCheck(r.Context(), target.fingerprint); err != nil {
		writeError(w, http.StatusInternalServerError, "check_invalidation_failed", "Could not invalidate prior provider check")
		return
	}
	if _, err := target.credentials.DeleteIfRevision(target.name, ifMatch); err != nil {
		switch {
		case errors.Is(err, peoplesweep.ErrCredentialRevisionConflict):
			writeError(w, http.StatusPreconditionFailed, "credential_conflict", "People provider credential changed; reload settings")
		case errors.Is(err, peoplesweep.ErrCredentialNotFound):
			writeError(w, http.StatusNotFound, "credential_not_found", "No stored people provider credential was found")
		default:
			writeError(w, http.StatusInternalServerError, "credential_store_unavailable", "Could not clear people provider credential")
		}
		return
	}
	snapshot, latest, err := s.readPersistedSettings()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "settings_read_failed", "Could not read settings")
		return
	}
	response, err := s.buildPeopleInferenceSettingsResponse(r.Context(), latest)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "settings_read_failed", "Could not read people provider status")
		return
	}
	w.Header().Set(etagHeaderName, snapshot.ETag)
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, response)
}

type peopleInferenceKeyTarget struct {
	name        string
	fingerprint string
	store       peopleInferenceCredentialAuthorityStore
	credentials *peoplesweep.FileCredentialStore
}

// peopleInferenceKeyTarget validates the trusted destination and revision
// before a write handler reads any secret-bearing request body.
func (s *Server) peopleInferenceKeyTarget(
	w http.ResponseWriter, r *http.Request, ifMatch string, requireExisting bool,
) (peopleInferenceKeyTarget, bool) {
	name := r.PathValue("name")
	if err := peoplesweep.ValidateProviderProfileName(name); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_provider_name", "People inference provider name is invalid")
		return peopleInferenceKeyTarget{}, false
	}
	_, configured, err := s.readPersistedSettings()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "settings_read_failed", "Could not read settings")
		return peopleInferenceKeyTarget{}, false
	}
	provider, exists := configured.People.Sweep.Providers[name]
	if !exists || provider.Credential != peoplesweep.CredentialStored || provider.PresetID == "" {
		writeError(w, http.StatusNotFound, "provider_not_found", "A stored-key preset provider was not found")
		return peopleInferenceKeyTarget{}, false
	}
	bound, err := peoplesweep.PresetProviderConfig(provider.PresetID, provider.Model)
	if err != nil || provider.Endpoint != bound.Endpoint || provider.Protocol != bound.Protocol || provider.Auth != bound.Auth {
		writeError(w, http.StatusConflict, "provider_binding_changed", "Provider destination changed; reload settings")
		return peopleInferenceKeyTarget{}, false
	}
	profileConfig := configured.People.Sweep
	profileConfig.Enabled = true
	profileConfig.Provider = peoplesweep.ProviderSelection{Name: name}
	profile, err := profileConfig.Profile()
	if err != nil {
		writeError(w, http.StatusConflict, "provider_invalid", "Provider policy is invalid")
		return peopleInferenceKeyTarget{}, false
	}
	st, ok := s.store.(peopleInferenceCredentialAuthorityStore)
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "people_inference_unavailable", "People inference store is unavailable")
		return peopleInferenceKeyTarget{}, false
	}
	credentials := peoplesweep.NewFileCredentialStore(configured.TokensDir())
	current, present, err := credentials.Revision(name)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "credential_store_unavailable", "People provider credential store is unavailable")
		return peopleInferenceKeyTarget{}, false
	}
	if current != ifMatch {
		writeError(w, http.StatusPreconditionFailed, "credential_conflict", "People provider credential changed; reload settings")
		return peopleInferenceKeyTarget{}, false
	}
	if requireExisting && !present {
		writeError(w, http.StatusNotFound, "credential_not_found", "No stored people provider credential was found")
		return peopleInferenceKeyTarget{}, false
	}
	return peopleInferenceKeyTarget{name: name, fingerprint: profile.Fingerprint, store: st, credentials: credentials}, true
}

func (s *Server) peopleInferenceProfileForRequest(
	w http.ResponseWriter, r *http.Request, ifMatch string,
) (config.ConfigFile, *config.Config, peoplesweep.Config, peoplesweep.ProviderProfile, bool) {
	name := r.PathValue("name")
	if err := peoplesweep.ValidateProviderProfileName(name); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_provider_name", "People inference provider name is invalid")
		return config.ConfigFile{}, nil, peoplesweep.Config{}, peoplesweep.ProviderProfile{}, false
	}
	snapshot, configured, err := s.readPersistedSettings()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "settings_read_failed", "Could not read settings")
		return config.ConfigFile{}, nil, peoplesweep.Config{}, peoplesweep.ProviderProfile{}, false
	}
	if snapshot.ETag != ifMatch {
		writeError(w, http.StatusPreconditionFailed, "settings_conflict", "The config file changed; reload settings and retry")
		return config.ConfigFile{}, nil, peoplesweep.Config{}, peoplesweep.ProviderProfile{}, false
	}
	if _, exists := configured.People.Sweep.Providers[name]; !exists {
		writeError(w, http.StatusBadRequest, "provider_not_found", "People inference provider was not found")
		return config.ConfigFile{}, nil, peoplesweep.Config{}, peoplesweep.ProviderProfile{}, false
	}
	selected := configured.People.Sweep
	selected.Enabled = true
	selected.Provider = peoplesweep.ProviderSelection{Name: name}
	profile, err := selected.Profile()
	if err != nil {
		writeError(w, http.StatusConflict, "provider_invalid", "People inference provider policy is invalid")
		return config.ConfigFile{}, nil, peoplesweep.Config{}, peoplesweep.ProviderProfile{}, false
	}
	return snapshot, configured, selected, profile, true
}

func (s *Server) handleCheckPeopleInferenceProvider(w http.ResponseWriter, r *http.Request) {
	ifMatch, ok := requiredSingleIfMatch(w, r)
	if !ok {
		return
	}
	_, configured, selected, profile, ok := s.peopleInferenceProfileForRequest(w, r, ifMatch)
	if !ok {
		return
	}
	st, ok := s.store.(peopleInferenceCheckStore)
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "people_inference_unavailable", "People inference store is unavailable")
		return
	}
	client := s.peopleInferenceHTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	registry, err := peoplesweep.NewDriverRegistryWithCodexAuthHome(client,
		peoplesweep.NewCodexCommandStarter(), peoplesweep.NewReleasedCodexIsolationGate(),
		filepath.Join(configured.TokensDir(), "people-codex"))
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "provider_unavailable", "People inference provider is unavailable")
		return
	}
	resolver := peoplesweep.NewCredentialResolver(
		peoplesweep.NewFileCredentialStore(configured.TokensDir()), os.LookupEnv)
	runner, err := peoplesweep.NewRunner(selected, st, registry, resolver)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "provider_unavailable", "People inference provider is unavailable")
		return
	}
	response, err := runner.Check(r.Context())
	if err != nil {
		writeError(w, http.StatusBadGateway, "provider_check_failed", "Synthetic provider check failed")
		return
	}
	if !peoplesweep.DriverVersionMatches(profile.DriverVersion, response.ProviderVersion) {
		writeError(w, http.StatusBadGateway, "provider_check_failed", "Synthetic provider check returned the wrong driver")
		return
	}
	if _, err := st.EnsurePersonInferenceProfile(r.Context(), profile); err != nil {
		writeError(w, http.StatusInternalServerError, "provider_check_store_failed", "Could not record provider check")
		return
	}
	if err := st.RecordPersonInferenceCheck(r.Context(), store.PersonInferenceCheck{
		ProfileFingerprint: profile.Fingerprint, CheckedAt: time.Now().UTC(),
		DriverVersion: profile.DriverVersion, OutputMode: profile.OutputMode,
		ProviderRequestID: response.ProviderRequestID, ModelVersion: response.ModelVersion,
	}); err != nil {
		writeError(w, http.StatusInternalServerError, "provider_check_store_failed", "Could not record provider check")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, PeopleInferenceCheckResponse{
		OK: true, Fingerprint: profile.Fingerprint, Model: profile.Model, Usage: response.Usage,
	})
}

func (s *Server) handleConsentPeopleInferenceProvider(w http.ResponseWriter, r *http.Request) {
	ifMatch, ok := requiredSingleIfMatch(w, r)
	if !ok {
		return
	}
	snapshot, configured, _, profile, ok := s.peopleInferenceProfileForRequest(w, r, ifMatch)
	if !ok {
		return
	}
	var request PeopleInferenceConsentRequest
	if !decodeStrictSettingsJSON(w, r, &request) {
		return
	}
	if !request.Confirmed || request.Fingerprint != profile.Fingerprint {
		writeError(w, http.StatusConflict, "consent_disclosure_changed", "Review the exact provider disclosure before consenting")
		return
	}
	st, ok := s.store.(peopleInferenceConsentStore)
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "people_inference_unavailable", "People inference store is unavailable")
		return
	}
	checked, err := st.HasSuccessfulPersonInferenceCheck(r.Context(), profile.Fingerprint)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "provider_check_store_failed", "Could not read provider check")
		return
	}
	if !checked {
		writeError(w, http.StatusConflict, "check_required", "Run an exact synthetic check before consenting")
		return
	}
	if _, err := st.EnsurePersonInferenceProfile(r.Context(), profile); err != nil {
		writeError(w, http.StatusInternalServerError, "consent_store_failed", "Could not record provider consent")
		return
	}
	if _, _, err := st.GrantPersonInferenceConsent(r.Context(), profile.Fingerprint, "web"); err != nil {
		writeError(w, http.StatusInternalServerError, "consent_store_failed", "Could not record provider consent")
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

func (s *Server) handleRevokePeopleInferenceProvider(w http.ResponseWriter, r *http.Request) {
	ifMatch, ok := requiredSingleIfMatch(w, r)
	if !ok {
		return
	}
	snapshot, configured, _, profile, ok := s.peopleInferenceProfileForRequest(w, r, ifMatch)
	if !ok {
		return
	}
	st, ok := s.store.(personenrollment.RevocationStore)
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "people_inference_unavailable", "People inference store is unavailable")
		return
	}
	if _, err := st.RevokePersonInferenceConsent(r.Context(), profile.Fingerprint, "web"); err != nil {
		writeError(w, http.StatusInternalServerError, "consent_revoke_failed", "Could not revoke provider consent")
		return
	}
	if s.cfg.People.Sweep.Enabled && s.cfg.People.Sweep.Provider.Name == r.PathValue("name") {
		running, err := s.cfg.People.Sweep.Profile()
		if err != nil {
			writeError(w, http.StatusInternalServerError, "running_provider_invalid", "Running people provider policy is invalid")
			return
		}
		if running.Fingerprint != profile.Fingerprint {
			if _, err := st.RevokePersonInferenceConsent(r.Context(), running.Fingerprint, "web"); err != nil {
				writeError(w, http.StatusInternalServerError, "consent_revoke_failed", "Could not revoke running provider consent")
				return
			}
		}
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

func (s *Server) handleDisablePeopleInference(w http.ResponseWriter, r *http.Request) {
	ifMatch, ok := requiredSingleIfMatch(w, r)
	if !ok {
		return
	}
	st, ok := s.store.(personenrollment.RevocationStore)
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "people_inference_unavailable", "People inference store is unavailable")
		return
	}
	runningFingerprint := ""
	if s.cfg.People.Sweep.Enabled {
		running, err := s.cfg.People.Sweep.Profile()
		if err != nil {
			writeError(w, http.StatusInternalServerError, "running_provider_invalid", "Running people provider policy is invalid")
			return
		}
		runningFingerprint = running.Fingerprint
	}
	service := personenrollment.NewService(s.cfg.ConfigFilePath(), st)
	if _, err := service.Disable(r.Context(), ifMatch, runningFingerprint, "web"); err != nil {
		s.writeSettingsConfigError(w, err)
		return
	}
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

func (s *Server) handleRemovePeopleInferenceProvider(w http.ResponseWriter, r *http.Request) {
	ifMatch, ok := requiredSingleIfMatch(w, r)
	if !ok {
		return
	}
	st, ok := s.store.(personenrollment.RevocationStore)
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "people_inference_unavailable", "People inference store is unavailable")
		return
	}
	name := r.PathValue("name")
	if err := peoplesweep.ValidateProviderProfileName(name); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_provider_name", "People inference provider name is invalid")
		return
	}
	service := personenrollment.NewService(s.cfg.ConfigFilePath(), st)
	credentials := peoplesweep.NewFileCredentialStore(s.cfg.TokensDir())
	if _, err := service.RemoveProfile(r.Context(), ifMatch, name, "web", credentials); err != nil {
		switch {
		case errors.Is(err, config.ErrConfigConflict):
			writeError(w, http.StatusPreconditionFailed, "settings_conflict", "The config file changed; reload settings and retry")
		case errors.Is(err, personenrollment.ErrProfileMissing):
			writeError(w, http.StatusNotFound, "provider_not_found", "People inference provider was not found")
		case errors.Is(err, personenrollment.ErrProfileActive), errors.Is(err, personenrollment.ErrOnlyProfile):
			writeError(w, http.StatusConflict, "provider_in_use", err.Error())
		default:
			s.writeSettingsConfigError(w, err)
		}
		return
	}
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

func (s *Server) buildPeopleInferenceSettingsResponse(ctx context.Context, configured *config.Config) (PeopleInferenceSettingsResponse, error) {
	response := peopleInferenceSettingsResponse(configured.People.Sweep, s.cfg.People.Sweep)
	credentials := peoplesweep.NewFileCredentialStore(configured.TokensDir())
	for index := range response.Profiles {
		profile := &response.Profiles[index]
		if profile.Protocol == string(peoplesweep.ProtocolCodexAppServer) {
			profile.CredentialConfigured = codexAuthFileConfigured(configured.TokensDir())
			continue
		}
		if profile.CredentialSource == string(peoplesweep.CredentialEnv) {
			value, exists := os.LookupEnv(profile.CredentialEnv)
			profile.CredentialConfigured = exists && value != ""
			continue
		}
		if profile.CredentialSource != string(peoplesweep.CredentialStored) {
			continue
		}
		if !peoplesweep.StoredCredentialsSupported() {
			continue // The platform cannot hold profile secrets, so nothing is stored.
		}
		revision, exists, err := credentials.Revision(profile.Name)
		if err != nil {
			return PeopleInferenceSettingsResponse{}, err
		}
		profile.CredentialRevision = revision
		profile.CredentialConfigured = exists
	}
	if authority, ok := s.store.(peoplesweep.ProviderAuthority); ok {
		for index := range response.Profiles {
			profile := &response.Profiles[index]
			if profile.Fingerprint == "" {
				continue
			}
			checked, err := authority.HasSuccessfulPersonInferenceCheck(ctx, profile.Fingerprint)
			if err != nil {
				return PeopleInferenceSettingsResponse{}, err
			}
			consented, err := authority.HasActivePersonInferenceConsent(ctx, profile.Fingerprint)
			if err != nil {
				return PeopleInferenceSettingsResponse{}, err
			}
			profile.Checked = checked
			profile.ConsentActive = consented
		}
	}
	response.PendingRestart = response.PendingRestart || s.settingsPendingRestart.Load()
	return response, nil
}

func codexAuthFileConfigured(tokensDir string) bool {
	home := filepath.Join(tokensDir, "people-codex")
	directory, err := os.Lstat(home) //nolint:gosec // Only checks metadata under the configured daemon tokens directory.
	if err != nil || !directory.IsDir() || directory.Mode().Perm()&0o077 != 0 {
		return false
	}
	auth, err := os.Lstat(filepath.Join(home, "auth.json")) //nolint:gosec // Fixed child name; metadata only.
	return err == nil && auth.Mode().IsRegular() && auth.Mode().Perm()&0o077 == 0 &&
		auth.Size() > 0 && auth.Size() <= 1<<20
}

func peopleInferenceSettingsResponse(configured, running peoplesweep.Config) PeopleInferenceSettingsResponse {
	result := PeopleInferenceSettingsResponse{
		Profiles:          make([]PeopleInferenceProfileSetting, 0, len(configured.Providers)),
		ConfiguredName:    configured.Provider.Name,
		ConfiguredEnabled: configured.Enabled,
		RunningName:       running.Provider.Name,
		RunningEnabled:    running.Enabled,
	}
	names := make([]string, 0, len(configured.Providers))
	for name := range configured.Providers {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		provider := configured.Providers[name]
		entry := PeopleInferenceProfileSetting{
			Name: name, Selected: name == configured.Provider.Name,
			PresetID: provider.PresetID,
			Protocol: string(provider.Protocol),
			Model:    provider.Model, CredentialSource: string(provider.Credential),
			CredentialEnv: provider.CredentialEnv,
			OutputMode:    string(provider.OutputMode), RetentionPosture: provider.RetentionPosture,
			TrainingPosture: provider.TrainingPosture, SourceSince: provider.SourceSince,
			SourceUntil: provider.SourceUntil, AllowSensitive: provider.AllowSensitive,
			AllowedSources: make([]string, 0, len(provider.AllowedSources)),
		}
		for _, source := range provider.AllowedSources {
			entry.AllowedSources = append(entry.AllowedSources, string(source))
		}
		profileConfig := configured
		profileConfig.Enabled = true
		profileConfig.Provider = peoplesweep.ProviderSelection{Name: name}
		if profile, err := profileConfig.Profile(); err == nil {
			entry.Fingerprint = profile.Fingerprint
			entry.Endpoint = profile.Endpoint
			if entry.Selected {
				result.ConfiguredFingerprint = profile.Fingerprint
			}
		}
		result.Profiles = append(result.Profiles, entry)
	}
	if running.Provider.Name != "" {
		runningProfileConfig := running
		runningProfileConfig.Enabled = true
		if profile, err := runningProfileConfig.Profile(); err == nil {
			result.RunningFingerprint = profile.Fingerprint
		}
	}
	result.PendingRestart = !reflect.DeepEqual(configured, running)
	return result
}
